package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// slowServer is a pretend remote: every transfer takes `delay`, which makes parallelism measurable
// without depending on how fast the machine is.
type slowServer struct {
	delay time.Duration
	tree  map[string][]RemoteEntry // directory -> entries, for List

	mu       sync.Mutex
	cur      int
	peak     int
	dirs     map[string]bool
	received map[string]int64 // uploaded path -> bytes
	fail     map[string]bool  // paths whose transfer fails
	dials    int
	closed   int
	badPut   []string // uploads into a directory that did not exist yet
}

func newSlowServer(delay time.Duration) *slowServer {
	return &slowServer{delay: delay, tree: map[string][]RemoteEntry{}, dirs: map[string]bool{"/": true}, received: map[string]int64{}, fail: map[string]bool{}}
}

func (s *slowServer) dial(context.Context) (RemoteClient, error) {
	s.mu.Lock()
	s.dials++
	s.mu.Unlock()
	return &slowConn{s: s}, nil
}

// work holds a "transfer slot" for the delay and records the peak concurrency.
func (s *slowServer) work(ctx context.Context) error {
	s.mu.Lock()
	s.cur++
	if s.cur > s.peak {
		s.peak = s.cur
	}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.cur--; s.mu.Unlock() }()
	select {
	case <-time.After(s.delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type slowConn struct {
	RemoteClient
	s *slowServer
}

func (c *slowConn) Close() error { c.s.mu.Lock(); c.s.closed++; c.s.mu.Unlock(); return nil }

func (c *slowConn) List(dir string) ([]RemoteEntry, error) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	if es, ok := c.s.tree[dir]; ok {
		return es, nil
	}
	return nil, errors.New("no such directory " + dir)
}

func (c *slowConn) Mkdir(p string) error {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	c.s.dirs[p] = true
	return nil
}

func (c *slowConn) Get(ctx context.Context, remote string, w io.Writer, progress BytesFunc) error {
	if err := c.s.work(ctx); err != nil {
		return err
	}
	c.s.mu.Lock()
	bad := c.s.fail[remote]
	c.s.mu.Unlock()
	if bad {
		return errors.New("550 cannot read " + remote)
	}
	data := []byte("content of " + remote)
	if _, err := w.Write(data); err != nil {
		return err
	}
	if progress != nil {
		progress(int64(len(data)), int64(len(data)))
	}
	return nil
}

func (c *slowConn) Put(ctx context.Context, remote string, r io.Reader, size int64, progress BytesFunc) error {
	c.s.mu.Lock()
	if parent := path.Dir(remote); !c.s.dirs[parent] {
		c.s.badPut = append(c.s.badPut, remote)
	}
	bad := c.s.fail[remote]
	c.s.mu.Unlock()
	if err := c.s.work(ctx); err != nil {
		return err
	}
	if bad {
		return errors.New("553 cannot write " + remote)
	}
	n, err := io.Copy(io.Discard, r)
	c.s.mu.Lock()
	c.s.received[remote] = n
	c.s.mu.Unlock()
	if progress != nil {
		progress(n, size)
	}
	return err
}

func (s *slowServer) snapshot() (peak, dials, closed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak, s.dials, s.closed
}

// bigTree lays out /top0..top1 plus three directories of four files: 14 files.
func (s *slowServer) bigTree() (files []string) {
	add := func(dir, name string) {
		s.tree[dir] = append(s.tree[dir], RemoteEntry{Name: name, Size: int64(len("content of " + path.Join(dir, name)))})
		files = append(files, path.Join(dir, name))
	}
	add("/", "top0")
	add("/", "top1")
	for _, d := range []string{"a", "b", "c"} {
		s.tree["/"] = append(s.tree["/"], RemoteEntry{Name: d, IsDir: true})
		for i := 0; i < 4; i++ {
			add("/"+d, fmt.Sprintf("f%d", i))
		}
	}
	return files
}

func TestDownloadTreeRunsFilesInParallel(t *testing.T) {
	s := newSlowServer(60 * time.Millisecond)
	files := s.bigTree()
	pool := NewConnPool(s.dial, 4)
	defer pool.Close()

	dst := filepath.Join(t.TempDir(), "out")
	var done, progressed atomic.Int32
	start := time.Now()
	n, err := DownloadTree(context.Background(), pool, "/", dst, TreeOptions{
		Progress: func(string, int64, int64) { progressed.Add(1) },
		FileDone: func(string) { done.Add(1) },
	})
	elapsed := time.Since(start)

	if err != nil || n != len(files) {
		t.Fatalf("DownloadTree = %d, %v; want %d files", n, err, len(files))
	}
	for _, f := range files {
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(f)))
		if err != nil || string(got) != "content of "+f {
			t.Errorf("%s: %q, %v", f, got, err)
		}
	}
	peak, dials, _ := s.snapshot()
	if peak != 4 {
		t.Errorf("peak concurrency = %d, want exactly 4 (the pool size)", peak)
	}
	if dials > 4 {
		t.Errorf("dialled %d connections, want at most 4", dials)
	}
	if int(done.Load()) != len(files) || int(progressed.Load()) != len(files) {
		t.Errorf("FileDone called %d times and Progress %d times, want %d each", done.Load(), progressed.Load(), len(files))
	}
	// 14 files at 60 ms: 840 ms one at a time, about 240 ms four at a time.
	if elapsed > 600*time.Millisecond {
		t.Errorf("took %v: the files were not transferred in parallel", elapsed)
	}
	assertNoPartFiles(t, dst)
}

func assertNoPartFiles(t *testing.T, root string) {
	t.Helper()
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".part") {
			t.Errorf("leftover partial file %s", p)
		}
		return nil
	})
}

func TestOneConnectionTransfersOneFileAtATime(t *testing.T) {
	s := newSlowServer(15 * time.Millisecond)
	s.bigTree()
	n, err := DownloadTree(context.Background(), NewConnPool(s.dial, 1), "/", filepath.Join(t.TempDir(), "o"), TreeOptions{})
	if err != nil || n != 14 {
		t.Fatalf("DownloadTree = %d, %v", n, err)
	}
	if peak, _, _ := s.snapshot(); peak != 1 {
		t.Errorf("peak concurrency = %d with a single connection, want 1", peak)
	}
}

func TestUploadTreeCreatesDirectoriesBeforeTheirFiles(t *testing.T) {
	s := newSlowServer(30 * time.Millisecond)
	pool := NewConnPool(s.dial, 4)
	defer pool.Close()

	src := filepath.Join(t.TempDir(), "src")
	files := writeTree(t, src)
	var done atomic.Int32
	n, err := UploadTree(context.Background(), pool, src, "/dest", TreeOptions{FileDone: func(string) { done.Add(1) }})
	if err != nil || n != len(files) {
		t.Fatalf("UploadTree = %d, %v; want %d", n, err, len(files))
	}
	if len(s.badPut) > 0 {
		t.Errorf("files were uploaded before their directory existed: %v", s.badPut)
	}
	for rel, data := range files {
		if got := s.received["/dest/"+rel]; got != int64(len(data)) {
			t.Errorf("%s: server received %d bytes, want %d", rel, got, len(data))
		}
	}
	if !s.dirs["/dest/docs/deep"] || !s.dirs["/dest/empty"] {
		t.Errorf("directories were not created: %v", s.dirs)
	}
	if int(done.Load()) != len(files) {
		t.Errorf("FileDone called %d times, want %d", done.Load(), len(files))
	}
}

func TestTreeFailuresDoNotStopTheOtherFiles(t *testing.T) {
	s := newSlowServer(10 * time.Millisecond)
	files := s.bigTree()
	s.fail["/a/f1"], s.fail["/b/f2"], s.fail["/top0"] = true, true, true
	pool := NewConnPool(s.dial, 3)
	defer pool.Close()

	dst := filepath.Join(t.TempDir(), "out")
	var done atomic.Int32
	n, err := DownloadTree(context.Background(), pool, "/", dst, TreeOptions{FileDone: func(string) { done.Add(1) }})

	if n != len(files)-3 || int(done.Load()) != len(files)-3 {
		t.Errorf("%d files succeeded (FileDone %d), want %d", n, done.Load(), len(files)-3)
	}
	for _, name := range []string{"/a/f1", "/b/f2", "/top0"} {
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("the error must name %s: %v", name, err)
		}
	}
	if _, statErr := os.Stat(filepath.Join(dst, "a", "f1")); statErr == nil {
		t.Error("a failed download left a file behind")
	}
	assertNoPartFiles(t, dst)
	// the three failures each cost a connection, which the pool replaced
	if _, dials, closed := s.snapshot(); closed < 3 || dials < 4 {
		t.Errorf("dials=%d closed=%d: connections that saw errors should be replaced", dials, closed)
	}
}

func TestTreeCancelStopsPromptly(t *testing.T) {
	s := newSlowServer(2 * time.Second)
	s.bigTree()
	pool := NewConnPool(s.dial, 4)
	defer pool.Close()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	n, err := DownloadTree(ctx, pool, "/", filepath.Join(t.TempDir(), "out"), TreeOptions{})
	if !errors.Is(err, context.Canceled) || n != 0 {
		t.Errorf("DownloadTree = %d, %v; want 0 files and context.Canceled", n, err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("cancelling took %v", time.Since(start))
	}
	// the pool must still be usable for the next transfer
	c, err := pool.Get(context.Background())
	if err != nil {
		t.Fatalf("pool unusable after a cancelled tree: %v", err)
	}
	pool.Put(c, false)
}

func TestTreeStopsWhenThePoolIsClosed(t *testing.T) {
	s := newSlowServer(10 * time.Millisecond)
	s.bigTree()
	pool := NewConnPool(s.dial, 2)
	pool.Close() // e.g. the user disconnected

	n, err := DownloadTree(context.Background(), pool, "/", filepath.Join(t.TempDir(), "out"), TreeOptions{})
	if n != 0 || !errors.Is(err, ErrPoolClosed) {
		t.Errorf("DownloadTree on a closed pool = %d, %v; want 0 and ErrPoolClosed", n, err)
	}
}

// ---- against the real servers ----

func manyFiles(t *testing.T, root string, dirs, perDir int) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for d := 0; d < dirs; d++ {
		for i := 0; i < perDir; i++ {
			rel := fmt.Sprintf("d%02d/sub/f%03d.bin", d, i)
			data := randomBytes(t, 1000+d*97+i*13) // different sizes, so a mix-up shows
			p := filepath.Join(root, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, data, 0o644); err != nil {
				t.Fatal(err)
			}
			files[rel] = data
		}
	}
	return files
}

func TestParallelTreesAgainstRealServers(t *testing.T) {
	m, params := startAllServers(t, nil)
	ctx := context.Background()

	for _, proto := range []string{ProtoFTP, ProtoFTPS, ProtoSFTP} {
		t.Run(proto, func(t *testing.T) {
			p := params[proto]
			p.Parallel = 4
			id, err := m.Connect(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Disconnect(id)
			pool, err := m.Pool(id)
			if err != nil {
				t.Fatal(err)
			}
			if pool.Max() != 4 {
				t.Fatalf("pool size = %d, want 4", pool.Max())
			}

			src := filepath.Join(t.TempDir(), "src")
			files := manyFiles(t, src, 6, 12) // 72 files over 6 directories
			n, err := UploadTree(ctx, pool, src, "/par-"+proto, TreeOptions{})
			if err != nil || n != len(files) {
				t.Fatalf("UploadTree = %d, %v; want %d", n, err, len(files))
			}
			for rel, want := range files {
				got, err := os.ReadFile(filepath.Join(share(m), "par-"+proto, filepath.FromSlash(rel)))
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("%s differs on the server: %v", rel, err)
				}
			}

			dst := filepath.Join(t.TempDir(), "dst")
			n, err = DownloadTree(ctx, pool, "/par-"+proto, dst, TreeOptions{})
			if err != nil || n != len(files) {
				t.Fatalf("DownloadTree = %d, %v; want %d", n, err, len(files))
			}
			for rel, want := range files {
				got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel)))
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("%s differs after download: %v", rel, err)
				}
			}
			assertNoPartFiles(t, dst)

			pool.mu.Lock()
			open := pool.open
			pool.mu.Unlock()
			if open > 4 {
				t.Errorf("the pool opened %d connections, want at most 4", open)
			}
		})
	}
}

func TestManagerPoolFollowsTheSession(t *testing.T) {
	m, params := startAllServers(t, nil)
	p := params[ProtoSFTP]
	p.Parallel = 3
	id, err := m.Connect(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := m.Pool(id)
	if err != nil || pool.Max() != 3 {
		t.Fatalf("Pool = %v, max %d; want a pool of 3", err, pool.Max())
	}
	again, _ := m.Pool(id)
	if again != pool {
		t.Error("Pool must return the same pool every time")
	}
	c, err := pool.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.List("/"); err != nil {
		t.Errorf("a pooled connection must be usable: %v", err)
	}
	pool.Put(c, false)

	if err := m.Disconnect(id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Get(context.Background()); !errors.Is(err, ErrPoolClosed) {
		t.Errorf("after Disconnect the pool must be closed, Get = %v", err)
	}
	if _, err := m.Pool(id); err == nil {
		t.Error("a disconnected session has no pool")
	}
}

func TestParallelDefaults(t *testing.T) {
	for in, want := range map[int]int{0: 4, -1: 4, 1: 1, 8: 8, 16: 16, 100: 16} {
		if got := (ConnectParams{Parallel: in}).parallel(); got != want {
			t.Errorf("parallel(%d) = %d, want %d", in, got, want)
		}
	}
}
