package core

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jlaffaye/ftp"
	"github.com/pin/tftp/v3"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// newTestManager returns a Manager whose servers bind loopback on random ports, with
// users alice (read/write) and bob (read-only, via his user flag).
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.KnownHosts().TrustNew = true // the tests' own servers; asking the user is tested separately
	cfg := m.Config()
	for _, s := range []*ServiceConfig{&cfg.FTP.ServiceConfig, &cfg.SFTP.ServiceConfig, &cfg.TFTP.ServiceConfig} {
		s.BindAddr, s.Port = "127.0.0.1", 0
	}
	cfg.FTP.PassiveStart, cfg.FTP.PassiveEnd = 0, 0
	cfg.TFTP.ReadOnly = false
	cfg.Users = nil
	if err := cfg.SetUser("alice", "secret", false); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetUser("bob", "hunter2", true); err != nil {
		t.Fatal(err)
	}
	if err := m.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.StopAll)
	return m
}

func startService(t *testing.T, m *Manager, name string) string {
	t.Helper()
	if err := m.Start(name); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	for _, st := range m.Status() {
		if st.Name == name && st.Running {
			return st.Addr
		}
	}
	t.Fatalf("%s not running after Start", name)
	return ""
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func share(m *Manager) string { return m.ResolveRoot(m.Config().FTP.Root) }

func portOf(t *testing.T, addr string) int {
	t.Helper()
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg, pw, err := LoadConfig(dir)
	if err != nil || pw == "" || len(cfg.Users) != 1 || cfg.Users[0].Name != "admin" {
		t.Fatalf("first run: cfg=%+v pw=%q err=%v", cfg, pw, err)
	}
	if !userDBHas(cfg, "admin", pw) {
		t.Fatal("generated password does not authenticate")
	}
	cfg2, pw2, err := LoadConfig(dir)
	if err != nil || pw2 != "" || len(cfg2.Users) != 1 {
		t.Fatalf("second run: pw=%q err=%v", pw2, err)
	}
}

func userDBHas(cfg Config, name, pw string) bool {
	_, ok := newUserDB(cfg.Users).Authenticate(name, pw)
	return ok
}

func TestConfigValidate(t *testing.T) {
	good := DefaultConfig(t.TempDir())
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Config){
		"bad port":        func(c *Config) { c.FTP.Port = 70000 },
		"bad bind":        func(c *Config) { c.SFTP.BindAddr = "not-an-ip" },
		"empty root":      func(c *Config) { c.TFTP.Root = " " },
		"bad passive":     func(c *Config) { c.FTP.PassiveStart, c.FTP.PassiveEnd = 6000, 5000 },
		"ipv6 public":     func(c *Config) { c.FTP.PublicHost = "::1" },
		"duplicate users": func(c *Config) { c.Users = []User{{Name: "a"}, {Name: "A"}} },
	}
	for name, mutate := range cases {
		c := DefaultConfig(t.TempDir())
		mutate(&c)
		if c.Validate() == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

func TestSandboxBlocksEscape(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	rootDir := filepath.Join(t.TempDir(), "root")
	sb, err := OpenSandbox(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	fs := sb.View(false)

	// ".." is clamped to the root, so this resolves inside it and does not exist there.
	if _, err := fs.Open("../../" + filepath.Base(outside) + "/secret.txt"); err == nil {
		t.Error("opened a file outside the root via ..")
	}
	link := filepath.Join(rootDir, "link")
	if err := os.Symlink(outside, link); err != nil {
		// Windows needs a privilege for symlinks; a directory junction is the same kind of escape.
		if out, jerr := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); jerr != nil {
			t.Skipf("cannot create a symlink or junction here: %v / %v %s", err, jerr, out)
		}
	}
	if f, err := fs.Open("link/secret.txt"); err == nil {
		f.Close()
		t.Error("opened a file outside the root via a link")
	}
	if _, err := fs.Stat("link/secret.txt"); err == nil {
		t.Error("stat'ed a file outside the root via a link")
	}
	if f, err := fs.OpenFile("link/new.txt", os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		f.Close()
		t.Error("created a file outside the root via a link")
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); err == nil {
		t.Error("a file appeared outside the root")
	}
}

func TestReadOnlyView(t *testing.T) {
	sb, err := OpenSandbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	ro := sb.View(true)
	for name, err := range map[string]error{
		"create": func() error { _, err := ro.OpenFile("a", os.O_CREATE|os.O_WRONLY, 0o644); return err }(),
		"mkdir":  ro.Mkdir("d", 0o755),
		"remove": ro.Remove("a"),
		"rename": ro.Rename("a", "b"),
	} {
		if !errors.Is(err, os.ErrPermission) {
			t.Errorf("%s on read-only view: got %v, want permission error", name, err)
		}
	}
}

func TestFTP(t *testing.T) {
	m := newTestManager(t)
	addr := startService(t, m, ServiceFTP)

	dial := func(user, pass string) (*ftp.ServerConn, error) {
		c, err := ftp.Dial(addr, ftp.DialWithTimeout(5*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Login(user, pass); err != nil {
			c.Quit()
			return nil, err
		}
		return c, nil
	}

	if _, err := dial("alice", "wrong"); err == nil {
		t.Error("login with a wrong password succeeded")
	}
	if _, err := dial("nobody", "x"); err == nil {
		t.Error("login with an unknown user succeeded")
	}

	c, err := dial("alice", "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Quit()

	data := randomBytes(t, 300_000)
	if err := c.Stor("a.bin", bytes.NewReader(data)); err != nil {
		t.Fatalf("STOR: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(share(m), "a.bin")); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("uploaded file on disk differs: %v", err)
	}
	r, err := c.Retr("a.bin")
	if err != nil {
		t.Fatalf("RETR: %v", err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("downloaded data differs: %v", err)
	}

	if err := c.MakeDir("sub"); err != nil {
		t.Fatal(err)
	}
	if err := c.Rename("a.bin", "sub/b.bin"); err != nil {
		t.Fatal(err)
	}
	entries, err := c.List("sub")
	if err != nil || len(entries) != 1 || entries[0].Name != "b.bin" || entries[0].Size != uint64(len(data)) {
		t.Fatalf("LIST sub: %+v err=%v", entries, err)
	}
	if err := c.Delete("sub/b.bin"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveDir("sub"); err != nil {
		t.Fatal(err)
	}

	// A path with ".." must stay inside the share.
	if err := c.Stor("../escape.txt", bytes.NewReader([]byte("x"))); err != nil {
		t.Logf("STOR ../escape.txt rejected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(share(m)), "escape.txt")); err == nil {
		t.Error("upload escaped the share folder")
	}

	ro, err := dial("bob", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Quit()
	if err := ro.Stor("nope.txt", bytes.NewReader([]byte("x"))); err == nil {
		t.Error("read-only user could upload")
	}
	if err := ro.MakeDir("nope"); err == nil {
		t.Error("read-only user could create a directory")
	}

	if err := m.Stop(ServiceFTP); err != nil {
		t.Fatal(err)
	}
	if _, err := ftp.Dial(addr, ftp.DialWithTimeout(time.Second)); err == nil {
		t.Error("server still accepts connections after Stop")
	}
}

func TestFTPAnonymousIsReadOnly(t *testing.T) {
	m := newTestManager(t)
	cfg := m.Config()
	cfg.FTP.AllowAnonymous = true
	if err := m.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(share(m), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(share(m), "pub.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	addr := startService(t, m, ServiceFTP)

	c, err := ftp.Dial(addr, ftp.DialWithTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Quit()
	if err := c.Login("anonymous", "guest@example.com"); err != nil {
		t.Fatal(err)
	}
	r, err := c.Retr("pub.txt")
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	if err := c.Stor("x.txt", bytes.NewReader([]byte("x"))); err == nil {
		t.Error("anonymous user could upload")
	}
}

func sshDial(t *testing.T, addr, user, pass string) (*ssh.Client, error) {
	t.Helper()
	return ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(pass)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
}

func TestSFTP(t *testing.T) {
	m := newTestManager(t)
	addr := startService(t, m, ServiceSFTP)

	if _, err := sshDial(t, addr, "alice", "wrong"); err == nil {
		t.Error("login with a wrong password succeeded")
	}

	conn, err := sshDial(t, addr, "alice", "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c, err := sftp.NewClient(conn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	data := randomBytes(t, 500_000)
	w, err := c.Create("a.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(share(m), "a.bin")); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("uploaded file on disk differs: %v", err)
	}
	r, err := c.Open("a.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("downloaded data differs: %v", err)
	}

	if err := c.Mkdir("sub"); err != nil {
		t.Fatal(err)
	}
	if err := c.Rename("a.bin", "sub/b.bin"); err != nil {
		t.Fatal(err)
	}
	infos, err := c.ReadDir("sub")
	if err != nil || len(infos) != 1 || infos[0].Name() != "b.bin" || infos[0].Size() != int64(len(data)) {
		t.Fatalf("ReadDir sub: %v err=%v", infos, err)
	}
	if _, err := c.Stat("missing"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat missing: got %v, want not-exist", err)
	}
	mt := time.Unix(1_700_000_000, 0)
	if err := c.Chtimes("sub/b.bin", mt, mt); err != nil {
		t.Fatal(err)
	}
	if st, err := c.Stat("sub/b.bin"); err != nil || !st.ModTime().Equal(mt) {
		t.Errorf("mtime after Chtimes: %v err=%v", st, err)
	}
	if err := c.Remove("sub"); err == nil {
		t.Error("Remove deleted a directory")
	}
	if err := c.Remove("sub/b.bin"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveDirectory("sub"); err != nil {
		t.Fatal(err)
	}

	if f, err := c.Create("../escape.txt"); err == nil {
		f.Close()
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(share(m)), "escape.txt")); err == nil {
		t.Error("upload escaped the share folder")
	}

	roConn, err := sshDial(t, addr, "bob", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	defer roConn.Close()
	ro, err := sftp.NewClient(roConn)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if f, err := ro.Create("nope.txt"); err == nil {
		_, werr := f.Write([]byte("x"))
		cerr := f.Close()
		if werr == nil && cerr == nil {
			t.Error("read-only user could upload")
		}
	}
	if err := ro.Mkdir("nope"); err == nil {
		t.Error("read-only user could create a directory")
	}

	if err := m.Stop(ServiceSFTP); err != nil {
		t.Fatal(err)
	}
	if _, err := sshDial(t, addr, "alice", "secret"); err == nil {
		t.Error("server still accepts connections after Stop")
	}
}

func TestSFTPHostKeyIsStable(t *testing.T) {
	m := newTestManager(t)
	keyOf := func() string {
		addr := startService(t, m, ServiceSFTP)
		var seen string
		conn, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
			User: "alice", Auth: []ssh.AuthMethod{ssh.Password("secret")}, Timeout: 5 * time.Second,
			HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
				seen = ssh.FingerprintSHA256(k)
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
		if err := m.Stop(ServiceSFTP); err != nil {
			t.Fatal(err)
		}
		return seen
	}
	first, second := keyOf(), keyOf()
	if first == "" || first != second {
		t.Errorf("host key changed across restarts: %q vs %q", first, second)
	}
}

func TestTFTP(t *testing.T) {
	m := newTestManager(t)
	addr := startService(t, m, ServiceTFTP)

	c, err := tftp.NewClient(addr)
	if err != nil {
		t.Fatal(err)
	}
	c.SetTimeout(5 * time.Second)

	data := randomBytes(t, 1_000_000)
	wt, err := c.Send("up.bin", "octet")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.ReadFrom(bytes.NewReader(data)); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(share(m), "up.bin")); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("uploaded file on disk differs: %v", err)
	}

	rf, err := c.Receive("up.bin", "octet")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := rf.WriteTo(&buf); err != nil || !bytes.Equal(buf.Bytes(), data) {
		t.Fatalf("receive differs: %v", err)
	}

	if rf, err := c.Receive("missing.bin", "octet"); err == nil {
		if _, err := rf.WriteTo(io.Discard); err == nil {
			t.Error("downloading a missing file succeeded")
		}
	}

	// Windows-style separators from embedded devices map into the share.
	if err := os.MkdirAll(filepath.Join(share(m), "fw"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(share(m), "fw", "img.bin"), []byte("fw"), 0o644); err != nil {
		t.Fatal(err)
	}
	rf, err = c.Receive(`fw\img.bin`, "octet")
	if err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if _, err := rf.WriteTo(&buf); err != nil || buf.String() != "fw" {
		t.Fatalf("backslash path: %q err=%v", buf.String(), err)
	}

	// Traversal must not reach outside the share.
	secret := filepath.Join(filepath.Dir(share(m)), "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rf, err := c.Receive("../secret.txt", "octet"); err == nil {
		buf.Reset()
		if _, err := rf.WriteTo(&buf); err == nil {
			t.Errorf("read a file outside the share: %q", buf.String())
		}
	}
}

func TestTFTPReadOnly(t *testing.T) {
	m := newTestManager(t)
	cfg := m.Config()
	cfg.TFTP.ReadOnly = true
	if err := m.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	addr := startService(t, m, ServiceTFTP)

	c, err := tftp.NewClient(addr)
	if err != nil {
		t.Fatal(err)
	}
	c.SetTimeout(2 * time.Second)
	if wt, err := c.Send("nope.bin", "octet"); err == nil {
		if _, err := wt.ReadFrom(bytes.NewReader([]byte("x"))); err == nil {
			t.Error("write succeeded on a read-only TFTP server")
		}
	}
	if _, err := os.Stat(filepath.Join(share(m), "nope.bin")); err == nil {
		t.Error("file was created on a read-only TFTP server")
	}
}

func TestUserChangesApplyToRunningServers(t *testing.T) {
	m := newTestManager(t)
	ftpAddr := startService(t, m, ServiceFTP)
	sftpAddr := startService(t, m, ServiceSFTP)

	loginFTP := func(pass string) bool {
		c, err := ftp.Dial(ftpAddr, ftp.DialWithTimeout(5*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Quit()
		return c.Login("alice", pass) == nil
	}
	loginSFTP := func(pass string) bool {
		c, err := sshDial(t, sftpAddr, "alice", pass)
		if err == nil {
			c.Close()
		}
		return err == nil
	}

	if !loginFTP("secret") || !loginSFTP("secret") {
		t.Fatal("baseline login failed")
	}
	if err := m.SetUser("alice", "changed-pw", false); err != nil {
		t.Fatal(err)
	}
	if loginFTP("secret") || loginSFTP("secret") {
		t.Error("old password still works on a running server")
	}
	if !loginFTP("changed-pw") || !loginSFTP("changed-pw") {
		t.Error("new password rejected by a running server")
	}
	if err := m.DeleteUser("alice"); err != nil {
		t.Fatal(err)
	}
	if loginFTP("changed-pw") || loginSFTP("changed-pw") {
		t.Error("a deleted user can still sign in")
	}
}

func TestManagerLifecycle(t *testing.T) {
	m := newTestManager(t)
	startService(t, m, ServiceFTP)
	if err := m.Start(ServiceFTP); err == nil {
		t.Error("starting a running service should fail")
	}
	if err := m.Restart(ServiceFTP); err != nil {
		t.Fatal(err)
	}
	if err := m.Start("gopher"); err == nil {
		t.Error("unknown service should fail")
	}
	m.StopAll()
	for _, st := range m.Status() {
		if st.Running {
			t.Errorf("%s still running after StopAll", st.Name)
		}
	}

	// A port that is already taken must produce an error, not a half-started service.
	addr := startService(t, m, ServiceSFTP)
	cfg := m.Config()
	cfg.FTP.Port = portOf(t, addr)
	cfg.FTP.BindAddr = "127.0.0.1"
	if err := m.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ServiceFTP); err == nil {
		t.Error("expected an error when the port is already in use")
	}
}
