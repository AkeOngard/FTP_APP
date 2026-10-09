package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveRoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "app")
	abs := filepath.Join(t.TempDir(), "elsewhere")
	for in, want := range map[string]string{
		"share":                          filepath.Join(dir, "share"),
		"data/ftp":                       filepath.Join(dir, "data", "ftp"),
		"./share/":                       filepath.Join(dir, "share"),
		"../shared":                      filepath.Join(filepath.Dir(dir), "shared"),
		" share ":                        filepath.Join(dir, "share"),
		".":                              dir,
		abs:                              abs,
		abs + string(filepath.Separator): abs,
	} {
		if got := ResolveRoot(dir, in); got != want {
			t.Errorf("ResolveRoot(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPortableRoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "app")
	outside := filepath.Join(t.TempDir(), "other")
	cases := map[string]string{
		filepath.Join(dir, "share"):   "share",
		filepath.Join(dir, "a", "b"):  "a/b",
		dir:                           ".",
		filepath.Join(dir, "x") + `\`: "x",
		outside:                       outside, // not inside the app folder: stays a full path
		"share":                       "share",
		"./share/":                    "share",
		`data\ftp`:                    filepath.ToSlash(filepath.Clean(filepath.FromSlash(`data\ftp`))),
		"":                            "",
	}
	if runtime.GOOS != "windows" {
		delete(cases, filepath.Join(dir, "x")+`\`)
		delete(cases, `data\ftp`)
	}
	for in, want := range cases {
		if got := portableRoot(dir, in); got != want {
			t.Errorf("portableRoot(%q) = %q, want %q", in, got, want)
		}
	}
	if runtime.GOOS == "windows" {
		// drive letters compare case-insensitively
		lower := strings.ToLower(filepath.Join(dir, "Share"))
		if got := portableRoot(dir, lower); got != "Share" && got != "share" {
			t.Errorf("portableRoot(%q) = %q, want the folder name relative to the app folder", lower, got)
		}
		if got := portableRoot(dir, `Z:\data`); got != `Z:\data` && !strings.EqualFold(filepath.VolumeName(dir), "Z:") {
			t.Errorf("a path on another drive must stay as it is, got %q", got)
		}
	}
}

func TestRootSyntax(t *testing.T) {
	for _, ok := range []string{"share", "a/b", "../x", "."} {
		if err := checkRootSyntax(ok); err != nil {
			t.Errorf("%q should be accepted: %v", ok, err)
		}
	}
	if runtime.GOOS == "windows" {
		for _, bad := range []string{`C:data`, `\data`, `/data`} {
			if checkRootSyntax(bad) == nil {
				t.Errorf("%q depends on the current drive or folder and must be rejected", bad)
			}
		}
	}
}

func TestDefaultConfigIsPortable(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	for name, r := range map[string]string{"ftp": cfg.FTP.Root, "sftp": cfg.SFTP.Root, "tftp": cfg.TFTP.Root} {
		if filepath.IsAbs(r) || strings.Contains(r, dir) {
			t.Errorf("%s root %q must be relative so the folder can move", name, r)
		}
	}
	first, _, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, configFileName))
	if strings.Contains(string(raw), strings.ReplaceAll(dir, `\`, `\\`)) {
		t.Errorf("config.json contains the absolute path of its own folder:\n%s", raw)
	}
	if first.FTP.Root != "share" {
		t.Errorf("stored root = %q, want \"share\"", first.FTP.Root)
	}
}

// Configs written by earlier versions hold full paths; they must become portable on the next start.
func TestOldConfigWithFullPathsIsMigrated(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	outside := filepath.Join(t.TempDir(), "files")
	cfg.FTP.Root = filepath.Join(dir, "share")      // inside the app folder
	cfg.SFTP.Root = filepath.Join(dir, "data", "s") // inside, nested
	cfg.TFTP.Root = outside                         // elsewhere on purpose
	if err := SaveConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}

	loaded, _, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.FTP.Root != "share" || loaded.SFTP.Root != "data/s" || loaded.TFTP.Root != outside {
		t.Errorf("roots after loading: %q, %q, %q", loaded.FTP.Root, loaded.SFTP.Root, loaded.TFTP.Root)
	}
	var onDisk Config
	raw, _ := os.ReadFile(filepath.Join(dir, configFileName))
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.FTP.Root != "share" || onDisk.SFTP.Root != "data/s" || onDisk.TFTP.Root != outside {
		t.Errorf("the migrated roots were not written back: %q, %q, %q", onDisk.FTP.Root, onDisk.SFTP.Root, onDisk.TFTP.Root)
	}
}

func TestSavingAFolderInsideTheAppFolderStoresItRelative(t *testing.T) {
	m := newTestManager(t)
	cfg := m.Config()
	cfg.FTP.Root = filepath.Join(m.Dir(), "inbox", "ftp") // what Browse… would return
	outside := filepath.Join(t.TempDir(), "elsewhere")
	cfg.SFTP.Root = outside
	if err := m.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	got := m.Config()
	if got.FTP.Root != "inbox/ftp" || got.SFTP.Root != outside {
		t.Errorf("stored roots: %q, %q", got.FTP.Root, got.SFTP.Root)
	}
	if share(m) != filepath.Join(m.Dir(), "inbox", "ftp") {
		t.Errorf("resolved root = %q", share(m))
	}
}

func TestAShareThatContainsTheSettingsFolderIsRefused(t *testing.T) {
	m := newTestManager(t)
	before := m.Config().FTP.Root
	for _, root := range []string{".", m.Dir(), filepath.Dir(m.Dir()), ".."} {
		cfg := m.Config()
		cfg.FTP.Root = root
		err := m.SetConfig(cfg)
		if err == nil || !strings.Contains(err.Error(), "settings folder") {
			t.Errorf("sharing %q would expose passwords and keys, got %v", root, err)
		}
	}
	if m.Config().FTP.Root != before {
		t.Error("a refused change must not be applied")
	}

	// A sibling of the settings folder is fine, and so is a subfolder.
	for _, root := range []string{"share", "a/b", filepath.Join(t.TempDir(), "sibling")} {
		cfg := m.Config()
		cfg.FTP.Root = root
		if err := m.SetConfig(cfg); err != nil {
			t.Errorf("%q should be accepted: %v", root, err)
		}
	}
}

// An existing config that shares a parent folder must still load: refusing it would stop the app
// from starting. Only changes made through the UI are checked.
func TestLoadingAnOldConfigThatSharesTheParentStillWorks(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	cfg.FTP.Root = filepath.Dir(dir)
	if err := SaveConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(dir); err != nil {
		t.Fatalf("the app must still start: %v", err)
	}
}

// The point of it all: copy the whole folder somewhere else and it serves its own files there.
func TestCopiedFolderServesItsOwnFiles(t *testing.T) {
	first := filepath.Join(t.TempDir(), "FTP_App")
	if err := os.MkdirAll(first, 0o755); err != nil {
		t.Fatal(err)
	}
	m1, err := NewManager(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(first, "share"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "share", "hello.txt"), []byte("moved along"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = m1

	second := filepath.Join(t.TempDir(), "elsewhere", "Portable")
	if err := os.CopyFS(second, os.DirFS(first)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(first); err != nil { // the old location is gone
		t.Fatal(err)
	}

	m2, err := NewManager(second)
	if err != nil {
		t.Fatal(err)
	}
	m2.KnownHosts().TrustNew = true
	cfg := m2.Config()
	cfg.SFTP.BindAddr, cfg.SFTP.Port = "127.0.0.1", 0
	if err := cfg.SetUser("alice", "secret", false); err != nil {
		t.Fatal(err)
	}
	if err := m2.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m2.StopAll)
	addr := startService(t, m2, ServiceSFTP)

	host, port := splitAddr(t, addr)
	c, err := Connect(context.Background(), ConnectParams{Protocol: ProtoSFTP, Host: host, Port: port, User: "alice", Password: "secret", TimeoutSecs: 5}, m2.KnownHosts())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	entries, err := c.List("/")
	if err != nil || !hasEntry(entries, "hello.txt", false) {
		t.Fatalf("the copied folder does not serve its own share: %+v, %v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(first, "share")); err == nil {
		t.Error("the app recreated the shared folder at the old location")
	}
	raw, _ := os.ReadFile(filepath.Join(second, configFileName))
	if strings.Contains(string(raw), "FTP_App") && strings.Contains(strings.ToLower(string(raw)), strings.ToLower(strings.ReplaceAll(first, `\`, `\\`))) {
		t.Errorf("the config still names the old location:\n%s", raw)
	}
}
