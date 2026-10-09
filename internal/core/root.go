package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Shared folders are stored relative to the app's own folder whenever they lie inside it, so the whole
// folder (program, settings and shared files) can be moved or copied to another place or machine and
// keep working. A folder elsewhere is stored as a full path.

// ResolveRoot turns a configured shared folder into the absolute path used at run time. A relative
// path is relative to dir, the folder holding config.json.
func ResolveRoot(dir, root string) string {
	root = strings.TrimSpace(root)
	if filepath.IsAbs(root) {
		return filepath.Clean(root)
	}
	return filepath.Join(dir, filepath.FromSlash(root))
}

// checkRootSyntax rejects forms that are neither a full path nor a plain relative path. On Windows
// "C:data" (relative to the current folder of drive C) and "\data" (relative to the current drive) depend
// on state outside the config, so they would point somewhere else after a move.
func checkRootSyntax(root string) error {
	r := strings.TrimSpace(root)
	if r == "" || filepath.IsAbs(r) {
		return nil
	}
	if filepath.VolumeName(r) != "" || strings.HasPrefix(r, `\`) || strings.HasPrefix(r, "/") {
		return fmt.Errorf("%q is neither a full path (like C:\\data) nor a path relative to the app folder (like share)", root)
	}
	return nil
}

// portableRoot rewrites a full path that lies inside dir as a slash-separated path relative to dir.
// Relative paths are tidied, and paths outside dir (or on another drive) stay as they are.
func portableRoot(dir, root string) string {
	r := strings.TrimSpace(root)
	if r == "" {
		return r // left for validation to reject
	}
	if !filepath.IsAbs(r) {
		return filepath.ToSlash(filepath.Clean(filepath.FromSlash(r)))
	}
	r = filepath.Clean(r)
	rel, err := filepath.Rel(dir, r)
	if err != nil || escapes(rel) {
		return r
	}
	return filepath.ToSlash(rel)
}

// escapes reports whether a relative path from Rel leads out of its base.
func escapes(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// normalizeRoots stores every shared folder in its portable form and reports whether anything changed.
func (c *Config) normalizeRoots(dir string) bool {
	changed := false
	for _, s := range []*ServiceConfig{&c.FTP.ServiceConfig, &c.SFTP.ServiceConfig, &c.TFTP.ServiceConfig} {
		if n := portableRoot(dir, s.Root); n != s.Root {
			s.Root, changed = n, true
		}
	}
	return changed
}

// checkRootsAvoidSettings refuses a shared folder that contains the app's settings folder: config.json
// holds password hashes and the SSH host key is a private key, and clients could download both.
func (c Config) checkRootsAvoidSettings(dir string) error {
	for name, s := range map[string]ServiceConfig{"FTP": c.FTP.ServiceConfig, "SFTP": c.SFTP.ServiceConfig, "TFTP": c.TFTP.ServiceConfig} {
		if err := checkRootAvoidsSettings(dir, ResolveRoot(dir, s.Root)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// checkRootAvoidsSettings is the check for one absolute shared folder. Links are resolved first, so a
// link or junction that leads to a parent of the settings folder is caught as well.
func checkRootAvoidsSettings(dir, root string) error {
	exposed := false
	if rel, err := filepath.Rel(realPath(root), realPath(dir)); err == nil && !escapes(rel) {
		exposed = true
	}
	// Compare by file identity as well: Windows junctions are not resolved by EvalSymlinks, and a path
	// can reach the same folder under different names.
	if rootInfo, err := os.Stat(root); err == nil {
		for p := dir; !exposed; p = filepath.Dir(p) {
			if st, err := os.Stat(p); err == nil && os.SameFile(rootInfo, st) {
				exposed = true
			}
			if filepath.Dir(p) == p {
				break
			}
		}
	}
	if exposed {
		return errors.New("the shared folder must not contain the app's settings folder (" + dir +
			"), which holds passwords and keys. Choose a folder inside it, such as \"share\", or one elsewhere")
	}
	return nil
}

// realPath resolves links in p; a path that does not exist yet is returned as it is.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}
