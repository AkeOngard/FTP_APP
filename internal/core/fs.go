package core

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"
)

// Sandbox confines file access to a single directory tree. It is built on os.Root,
// so ".." components and symlinks cannot escape the directory, for every protocol.
type Sandbox struct {
	root *os.Root
}

// OpenSandbox opens dir as a sandbox root, creating the directory if needed.
func OpenSandbox(dir string) (*Sandbox, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Sandbox{root: r}, nil
}

func (s *Sandbox) Close() error { return s.root.Close() }

// View returns a per-session handle on the sandbox. Closing the sandbox
// invalidates every view.
func (s *Sandbox) View(readOnly bool) *SandboxFS {
	return &SandboxFS{root: s.root, readOnly: readOnly}
}

// SandboxFS is the file API shared by the FTP, SFTP and TFTP handlers.
// Names are slash-separated and interpreted relative to the sandbox root.
type SandboxFS struct {
	root     *os.Root
	readOnly bool
}

func (f *SandboxFS) IsReadOnly() bool { return f.readOnly }

// rel maps a protocol path ("/a/../b", "c/d") to a clean root-relative OS path.
func rel(name string) string {
	p := path.Clean("/" + name)
	if p == "/" {
		return "."
	}
	return filepath.FromSlash(p[1:])
}

func (f *SandboxFS) denyWrite(op, name string) error {
	if f.readOnly {
		return &fs.PathError{Op: op, Path: name, Err: fs.ErrPermission}
	}
	return nil
}

func (f *SandboxFS) Open(name string) (*os.File, error) {
	return f.root.Open(rel(name))
}

// OpenFile opens name with os.O_* flags; any flag that can modify data is refused
// on a read-only view.
func (f *SandboxFS) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	const writeFlags = os.O_WRONLY | os.O_RDWR | os.O_CREATE | os.O_TRUNC | os.O_APPEND
	if flag&writeFlags != 0 {
		if err := f.denyWrite("open", name); err != nil {
			return nil, err
		}
	}
	return f.root.OpenFile(rel(name), flag, perm)
}

func (f *SandboxFS) Stat(name string) (os.FileInfo, error)  { return f.root.Stat(rel(name)) }
func (f *SandboxFS) Lstat(name string) (os.FileInfo, error) { return f.root.Lstat(rel(name)) }

// ReadDir lists a directory sorted by name.
func (f *SandboxFS) ReadDir(name string) ([]os.FileInfo, error) {
	d, err := f.root.Open(rel(name))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	infos, err := d.Readdir(-1)
	if err != nil {
		return nil, err
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name() < infos[j].Name() })
	return infos, nil
}

func (f *SandboxFS) Mkdir(name string, perm os.FileMode) error {
	if err := f.denyWrite("mkdir", name); err != nil {
		return err
	}
	return f.root.Mkdir(rel(name), perm)
}

func (f *SandboxFS) MkdirAll(name string, perm os.FileMode) error {
	if err := f.denyWrite("mkdir", name); err != nil {
		return err
	}
	return f.root.MkdirAll(rel(name), perm)
}

// Remove deletes a file or an empty directory.
func (f *SandboxFS) Remove(name string) error {
	if err := f.denyWrite("remove", name); err != nil {
		return err
	}
	if rel(name) == "." {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrPermission}
	}
	return f.root.Remove(rel(name))
}

func (f *SandboxFS) RemoveAll(name string) error {
	if err := f.denyWrite("remove", name); err != nil {
		return err
	}
	if rel(name) == "." {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrPermission}
	}
	return f.root.RemoveAll(rel(name))
}

func (f *SandboxFS) Rename(oldName, newName string) error {
	if err := f.denyWrite("rename", oldName); err != nil {
		return err
	}
	if rel(oldName) == "." || rel(newName) == "." {
		return &fs.PathError{Op: "rename", Path: oldName, Err: fs.ErrPermission}
	}
	return f.root.Rename(rel(oldName), rel(newName))
}

func (f *SandboxFS) Chtimes(name string, atime, mtime time.Time) error {
	if err := f.denyWrite("chtimes", name); err != nil {
		return err
	}
	return f.root.Chtimes(rel(name), atime, mtime)
}

// ErrUnsupported is returned for operations a server or client does not offer
// (symlinks on the servers, listing over TFTP, ...).
var ErrUnsupported = errors.New("operation not supported")
