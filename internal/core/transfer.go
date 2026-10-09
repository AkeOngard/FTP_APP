package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

// maxTreeDepth stops runaway recursion on pathological or looping remote trees.
const maxTreeDepth = 64

func joinRemote(dir, name string) string { return path.Join(dir, name) }

// safeLocalName reports whether a name received from a server can be used as a single
// local path component. A hostile server could otherwise send "../../x" and write outside
// the chosen download folder.
func safeLocalName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, "/\\\x00") && filepath.IsLocal(name)
}

// DownloadFile saves remote to the local path. The data goes to "<local>.part" first and is
// renamed on success, so a failed or cancelled transfer never leaves a truncated file.
func DownloadFile(ctx context.Context, c RemoteClient, remote, local string, progress ProgressFunc) error {
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return err
	}
	part := local + ".part"
	f, err := os.Create(part)
	if err != nil {
		return err
	}
	err = c.Get(ctx, remote, f, bind(progress, remote))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(part)
		return err
	}
	if err := os.Rename(part, local); err != nil {
		os.Remove(part)
		return err
	}
	return nil
}

// UploadFile sends the local file to remote, replacing it.
func UploadFile(ctx context.Context, c RemoteClient, local, remote string, progress ProgressFunc) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.IsDir() {
		return fmt.Errorf("%s is a directory", local)
	}
	return c.Put(ctx, remote, f, st.Size(), bind(progress, remote))
}

func bind(fn ProgressFunc, file string) BytesFunc {
	if fn == nil {
		return nil
	}
	return func(done, total int64) { fn(file, done, total) }
}

// TreeOptions customises a folder transfer. Both callbacks can be called from several goroutines at
// once, because files are transferred in parallel.
type TreeOptions struct {
	Progress ProgressFunc      // bytes of one file; the file is identified by its remote path
	FileDone func(file string) // a file finished successfully
}

// treeJob is one file to move. key is the remote path, the same name Progress reports it under.
type treeJob struct{ key, src, dst string }

// runTree moves the files that walk emits, as many at a time as the pool has connections. walk runs on
// the calling goroutine and reports problems with single entries through fail; those are collected
// and the rest continues. Cancelling stops everything. It returns the number of files that succeeded.
func runTree(ctx context.Context, pool *ConnPool,
	walk func(emit func(treeJob) bool, fail func(error)),
	do func(context.Context, RemoteClient, treeJob) error, opt TreeOptions) (int, error) {

	var (
		mu    sync.Mutex
		errs  []error
		files int
	)
	fail := func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}

	workers := pool.Max()
	jobs := make(chan treeJob, workers*2)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					continue // after a cancel, drain the queue without starting anything
				}
				c, err := pool.Get(ctx)
				if err != nil {
					if ctx.Err() == nil {
						fail(fmt.Errorf("%s: %w", j.key, err))
					}
					continue
				}
				err = do(ctx, c, j)
				// A failed transfer may have left the connection unusable: let the pool replace it.
				pool.Put(c, err != nil)
				if err != nil {
					fail(err)
					continue
				}
				mu.Lock()
				files++
				mu.Unlock()
				if opt.FileDone != nil {
					opt.FileDone(j.key)
				}
			}
		}()
	}

	emit := func(j treeJob) bool {
		select {
		case jobs <- j:
			return true
		case <-ctx.Done():
			return false
		}
	}
	walk(emit, fail)
	close(jobs)
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return files, err
	}
	return files, errors.Join(errs...)
}

// withConn runs fn on a pooled connection. The connection is held only for the call, so the directory
// walk never keeps a connection away from the transfers.
func withConn(ctx context.Context, pool *ConnPool, fn func(RemoteClient) error) error {
	c, err := pool.Get(ctx)
	if err != nil {
		return err
	}
	err = fn(c)
	pool.Put(c, err != nil)
	return err
}

// DownloadTree copies a remote directory into localDir, transferring several files at once (as many as
// the pool has connections). Problems with single entries (unsafe names, links, failed files) are
// collected and the rest continues; cancelling stops at once. It returns the number of files downloaded.
func DownloadTree(ctx context.Context, pool *ConnPool, remoteDir, localDir string, opt TreeOptions) (int, error) {
	walk := func(emit func(treeJob) bool, fail func(error)) {
		var visit func(rdir, ldir string, depth int)
		visit = func(rdir, ldir string, depth int) {
			if depth > maxTreeDepth {
				fail(fmt.Errorf("%s: too deeply nested", rdir))
				return
			}
			var entries []RemoteEntry
			if err := withConn(ctx, pool, func(c RemoteClient) (err error) { entries, err = c.List(rdir); return }); err != nil {
				if ctx.Err() == nil {
					fail(fmt.Errorf("list %s: %w", rdir, err))
				}
				return
			}
			if err := os.MkdirAll(ldir, 0o755); err != nil {
				fail(err)
				return
			}
			for _, e := range entries {
				if ctx.Err() != nil {
					return
				}
				rp := joinRemote(rdir, e.Name)
				switch {
				case !safeLocalName(e.Name):
					fail(fmt.Errorf("skipped %q in %s: unsafe name", e.Name, rdir))
				case e.IsLink:
					fail(fmt.Errorf("skipped %s: symbolic links are not followed", rp))
				case e.IsDir:
					visit(rp, filepath.Join(ldir, e.Name), depth+1)
				default:
					if !emit(treeJob{key: rp, src: rp, dst: filepath.Join(ldir, e.Name)}) {
						return
					}
				}
			}
		}
		visit(remoteDir, localDir, 0)
	}
	do := func(ctx context.Context, c RemoteClient, j treeJob) error {
		if err := DownloadFile(ctx, c, j.src, j.dst, opt.Progress); err != nil {
			return fmt.Errorf("%s: %w", j.src, err)
		}
		return nil
	}
	return runTree(ctx, pool, walk, do, opt)
}

// UploadTree copies localDir to remoteDir, creating directories as needed and transferring several files
// at once. Symbolic links are skipped. It returns the number of files uploaded.
func UploadTree(ctx context.Context, pool *ConnPool, localDir, remoteDir string, opt TreeOptions) (int, error) {
	walk := func(emit func(treeJob) bool, fail func(error)) {
		var visit func(ldir, rdir string, depth int)
		visit = func(ldir, rdir string, depth int) {
			if depth > maxTreeDepth {
				fail(fmt.Errorf("%s: too deeply nested", ldir))
				return
			}
			// The directory must exist before any file is queued into it: the uploads run on other connections.
			if err := withConn(ctx, pool, func(c RemoteClient) error { return ensureRemoteDir(c, rdir) }); err != nil {
				if ctx.Err() == nil {
					fail(fmt.Errorf("mkdir %s: %w", rdir, err))
				}
				return
			}
			entries, err := os.ReadDir(ldir)
			if err != nil {
				fail(err)
				return
			}
			for _, e := range entries {
				if ctx.Err() != nil {
					return
				}
				lp, rp := filepath.Join(ldir, e.Name()), joinRemote(rdir, e.Name())
				switch {
				case e.Type()&os.ModeSymlink != 0:
					fail(fmt.Errorf("skipped %s: symbolic links are not followed", lp))
				case e.IsDir():
					visit(lp, rp, depth+1)
				case e.Type().IsRegular():
					if !emit(treeJob{key: rp, src: lp, dst: rp}) {
						return
					}
				}
			}
		}
		visit(localDir, remoteDir, 0)
	}
	do := func(ctx context.Context, c RemoteClient, j treeJob) error {
		if err := UploadFile(ctx, c, j.src, j.dst, opt.Progress); err != nil {
			return fmt.Errorf("%s: %w", j.src, err)
		}
		return nil
	}
	return runTree(ctx, pool, walk, do, opt)
}

// ScanLocal counts the files UploadTree would send from dir and their total size, so a progress
// display can show an overall percentage and time left before the first byte moves.
func ScanLocal(ctx context.Context, dir string) (files int, bytes int64, err error) {
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, werr error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if werr != nil {
			return nil // unreadable entries are reported by the transfer itself
		}
		if d.Type().IsRegular() {
			if info, ierr := d.Info(); ierr == nil {
				files++
				bytes += info.Size()
			}
		}
		return nil
	})
	return files, bytes, err
}

// ScanRemote counts the files DownloadTree would fetch from dir and their total size. It applies
// the same rules (no links, no unsafe names, depth limit), so the totals match what is transferred.
func ScanRemote(ctx context.Context, c RemoteClient, dir string) (files int, bytes int64, err error) {
	var walk func(string, int)
	walk = func(d string, depth int) {
		if depth > maxTreeDepth || ctx.Err() != nil {
			return
		}
		entries, lerr := c.List(d)
		if lerr != nil {
			return
		}
		for _, e := range entries {
			switch {
			case !safeLocalName(e.Name) || e.IsLink:
			case e.IsDir:
				walk(joinRemote(d, e.Name), depth+1)
			default:
				files++
				if e.Size > 0 {
					bytes += e.Size
				}
			}
		}
	}
	walk(dir, 0)
	return files, bytes, ctx.Err()
}

// ensureRemoteDir creates dir unless it already exists (servers disagree on how Mkdir reports that).
func ensureRemoteDir(c RemoteClient, dir string) error {
	err := c.Mkdir(dir)
	if err == nil {
		return nil
	}
	if _, lerr := c.List(dir); lerr == nil {
		return nil
	}
	return err
}

// RemoveTree deletes a remote directory and everything in it. Links are removed, never followed.
func RemoveTree(ctx context.Context, c RemoteClient, dir string) error {
	return removeTree(ctx, c, dir, 0)
}

func removeTree(ctx context.Context, c RemoteClient, dir string, depth int) error {
	if depth > maxTreeDepth {
		return fmt.Errorf("%s: too deeply nested", dir)
	}
	entries, err := c.List(dir)
	if err != nil {
		return fmt.Errorf("list %s: %w", dir, err)
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		p := joinRemote(dir, e.Name)
		var err error
		if e.IsDir && !e.IsLink {
			err = removeTree(ctx, c, p, depth+1)
		} else {
			err = c.Remove(p)
		}
		if err != nil {
			return err
		}
	}
	return c.RemoveDir(dir)
}
