package core

import (
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jlaffaye/ftp"
	"github.com/pin/tftp/v3"
)

func tcpAddr(ip string, port int) net.Addr { return &net.TCPAddr{IP: net.ParseIP(ip), Port: port} }

func TestLoginGuardLocksOutAfterRepeatedFailures(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	g := newLoginGuard()
	g.now = func() time.Time { return now }
	var blockedLog []string
	g.onBlock = func(addr string, _ time.Duration) { blockedLog = append(blockedLog, addr) }

	bad, other := tcpAddr("10.0.0.5", 1000), tcpAddr("10.0.0.6", 1000)
	for i := 1; i < maxLoginFailures; i++ {
		// every attempt comes from a new source port: the port must not matter
		if g.Failed(tcpAddr("10.0.0.5", 1000+i)) {
			t.Fatalf("blocked after only %d failures", i)
		}
	}
	if g.Blocked(bad) {
		t.Fatal("blocked before the limit")
	}
	if !g.Failed(bad) || !g.Blocked(bad) {
		t.Fatal("not blocked at the limit")
	}
	if _, err := g.Connect(bad); !errors.Is(err, errLockedOut) {
		t.Errorf("a blocked address could connect: %v", err)
	}
	if g.Blocked(other) {
		t.Error("another address was blocked too")
	}
	if rel, err := g.Connect(other); err != nil {
		t.Errorf("another address was refused: %v", err)
	} else {
		rel()
	}
	if len(blockedLog) != 1 || blockedLog[0] != "10.0.0.5" {
		t.Errorf("block notifications: %v", blockedLog)
	}

	now = now.Add(lockoutTime + time.Second)
	if g.Blocked(bad) {
		t.Error("still blocked after the lockout time")
	}
	// the old failures are gone: one more wrong password does not block again at once
	if g.Failed(bad) {
		t.Error("blocked again by a single failure after the lockout")
	}
}

func TestLoginGuardForgetsOldFailures(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	g := newLoginGuard()
	g.now = func() time.Time { return now }
	a := tcpAddr("10.0.0.9", 1)
	// someone who mistypes now and then is never locked out
	for i := 0; i < maxLoginFailures*3; i++ {
		if g.Failed(a) {
			t.Fatalf("blocked by failures spread over time (failure %d)", i+1)
		}
		now = now.Add(failureWindow / time.Duration(maxLoginFailures-2))
	}
}

func TestLoginGuardLimitsConnections(t *testing.T) {
	g := newLoginGuard()
	a := tcpAddr("10.0.0.7", 1)
	var rels []func()
	for i := 0; i < maxConnsPerAddr; i++ {
		rel, err := g.Connect(a)
		if err != nil {
			t.Fatalf("connection %d refused: %v", i+1, err)
		}
		rels = append(rels, rel)
	}
	if _, err := g.Connect(a); !errors.Is(err, errTooManyConns) {
		t.Fatalf("connection over the per-address limit: %v", err)
	}
	if rel, err := g.Connect(tcpAddr("10.0.0.8", 1)); err != nil {
		t.Errorf("another address refused: %v", err)
	} else {
		rel()
	}
	rels[0]()
	rels[0]() // releasing twice must not free a second slot
	if rel, err := g.Connect(a); err != nil {
		t.Errorf("no slot after a release: %v", err)
	} else {
		rels[0] = rel
	}
	if _, err := g.Connect(a); err == nil {
		t.Error("a double release freed two slots")
	}
	for _, r := range rels {
		r()
	}
	g.mu.Lock()
	total, left := g.total, len(g.conns)
	g.mu.Unlock()
	if total != 0 || left != 0 {
		t.Errorf("after everything closed: total %d, addresses %d", total, left)
	}

	// the overall limit, across addresses
	var all []func()
	for i := 0; i < maxConnsTotal; i++ {
		rel, err := g.Connect(tcpAddr("10.1."+itoaTest(i/250)+"."+itoaTest(i%250+1), 1))
		if err != nil {
			t.Fatalf("connection %d refused: %v", i+1, err)
		}
		all = append(all, rel)
	}
	if _, err := g.Connect(tcpAddr("10.9.9.9", 1)); !errors.Is(err, errTooManyConns) {
		t.Errorf("connection over the total limit: %v", err)
	}
	for _, r := range all {
		r()
	}

	var nilGuard *loginGuard // servers built without a guard allow everything
	if rel, err := nilGuard.Connect(a); err != nil || nilGuard.Blocked(a) || nilGuard.Failed(a) {
		t.Error("a nil guard must allow everything")
	} else {
		rel()
	}
}

// Guessing passwords against the real servers: after the limit, even the right password is refused,
// on both protocols, because they share the users.
func TestServersLockOutPasswordGuessing(t *testing.T) {
	m := newTestManager(t)
	ftpAddr := startService(t, m, ServiceFTP)
	sftpAddr := startService(t, m, ServiceSFTP)

	if c, err := sshDial(t, sftpAddr, "alice", "secret"); err != nil {
		t.Fatalf("baseline login: %v", err)
	} else {
		c.Close()
	}

	var wg sync.WaitGroup
	for i := 0; i < maxLoginFailures; i++ { // half over SFTP, half over FTP
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				if c, err := sshDial(t, sftpAddr, "alice", "guess"+itoaTest(i)); err == nil {
					c.Close()
					t.Error("a wrong password was accepted")
				}
				return
			}
			c, err := ftp.Dial(ftpAddr, ftp.DialWithTimeout(5*time.Second))
			if err != nil {
				return
			}
			defer c.Quit()
			if c.Login("alice", "guess"+itoaTest(i)) == nil {
				t.Error("a wrong password was accepted")
			}
		}()
	}
	wg.Wait()

	if c, err := sshDial(t, sftpAddr, "alice", "secret"); err == nil {
		c.Close()
		t.Error("SFTP: the right password still works from a blocked address")
	}
	if c, err := ftp.Dial(ftpAddr, ftp.DialWithTimeout(5*time.Second)); err == nil {
		if c.Login("alice", "secret") == nil {
			t.Error("FTP: the right password still works from a blocked address")
		}
		c.Quit()
	}
	if !strings.Contains(joined(logLines(m.Log(), "app")), "too many failed logins") {
		t.Errorf("the lockout is not in the log: %s", joined(m.Log().Entries()))
	}

	// the lockout ends by itself
	m.guard.mu.Lock()
	m.guard.now = func() time.Time { return time.Now().Add(lockoutTime + time.Minute) }
	m.guard.mu.Unlock()
	c, err := sshDial(t, sftpAddr, "alice", "secret")
	if err != nil {
		t.Fatalf("still locked out after the lockout time: %v", err)
	}
	c.Close()
}

func TestServerWillNotStartWhenItsShareExposesTheSettings(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig(dir)
	cfg.FTP.BindAddr, cfg.FTP.Port, cfg.FTP.PassiveStart, cfg.FTP.PassiveEnd = "127.0.0.1", 0, 0, 0
	cfg.SFTP.BindAddr, cfg.SFTP.Port = "127.0.0.1", 0
	cfg.FTP.Root = filepath.Dir(dir) // written by an older version, or by hand
	if err := SaveConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(dir)
	if err != nil {
		t.Fatalf("the app must still open: %v", err)
	}
	t.Cleanup(m.StopAll)
	if err := m.Start(ServiceFTP); err == nil || !strings.Contains(err.Error(), "settings folder") {
		t.Errorf("FTP started on a share that contains config.json and the host key: %v", err)
	}
	if statusOf(m, ServiceFTP).Running {
		t.Error("FTP is running")
	}
	if !strings.Contains(joined(logLines(m.Log(), ServiceFTP)), "settings folder") {
		t.Error("the reason is not in the log")
	}
	if err := m.Start(ServiceSFTP); err != nil { // the other servers, with a safe share, are unaffected
		t.Errorf("SFTP with its own safe share: %v", err)
	}
}

func TestAShareThatReachesTheSettingsThroughALinkIsRefused(t *testing.T) {
	m := newTestManager(t)
	link := filepath.Join(m.Dir(), "share-link")
	if err := os.Symlink(m.Dir(), link); err != nil {
		if out, jerr := exec.Command("cmd", "/c", "mklink", "/J", link, m.Dir()).CombinedOutput(); jerr != nil {
			t.Skipf("cannot create a symlink or junction here: %v / %v %s", err, jerr, out)
		}
	}
	cfg := m.Config()
	cfg.SFTP.Root = "share-link" // looks like a subfolder, is the settings folder itself
	if err := m.SetConfig(cfg); err == nil || !strings.Contains(err.Error(), "settings folder") {
		t.Errorf("a link to the settings folder was accepted as a share: %v", err)
	}
}

// failAfter yields n bytes and then breaks, like a device that loses power mid-upload.
type failAfter struct{ n int }

func (f *failAfter) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errors.New("link went down")
	}
	if len(p) > f.n {
		p = p[:f.n]
	}
	for i := range p {
		p[i] = 'N'
	}
	f.n -= len(p)
	return len(p), nil
}

func TestTFTPBrokenUploadKeepsThePreviousFile(t *testing.T) {
	m := newTestManager(t)
	addr := startService(t, m, ServiceTFTP)
	root := m.ResolveRoot(m.Config().TFTP.Root)
	target := filepath.Join(root, "running-config")
	if err := os.WriteFile(target, []byte("the good old config"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := tftp.NewClient(addr)
	if err != nil {
		t.Fatal(err)
	}
	c.SetTimeout(2 * time.Second)

	rf, err := c.Send("running-config", "octet")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rf.ReadFrom(&failAfter{n: 5000}); err == nil {
		t.Fatal("the broken upload reported success")
	}
	// the server notices the abort a moment later
	waitFor(t, "the partial upload to be cleaned up", func() bool {
		_, err := os.Stat(target + tftpPartSuffix)
		return errors.Is(err, os.ErrNotExist)
	})
	if got, _ := os.ReadFile(target); string(got) != "the good old config" {
		t.Errorf("a broken upload damaged the existing file: %q", got)
	}

	// a complete upload does replace it
	rf, err = c.Send("running-config", "octet")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rf.ReadFrom(strings.NewReader("the new config")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the new file", func() bool {
		got, _ := os.ReadFile(target)
		return string(got) == "the new config"
	})
	if _, err := os.Stat(target + tftpPartSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Error("the side file is still there after a complete upload")
	}

	// uploading over a folder is refused up front
	if err := os.Mkdir(filepath.Join(root, "afolder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if rf, err := c.Send("afolder", "octet"); err == nil {
		if _, err := rf.ReadFrom(strings.NewReader("x")); err == nil {
			t.Error("uploaded over a folder")
		}
	}
	var _ io.Reader = (*failAfter)(nil)
}

func TestLogTextIsCleaned(t *testing.T) {
	l := NewLogger()
	l.Infof("ftp", "%s logged in", "eve\r\n2026-01-01 [sftp] \"admin\" logged in\x1b[2J‮")
	msg := l.Entries()[0].Message
	if strings.ContainsAny(msg, "\r\n\x1b‮") {
		t.Errorf("control characters reached the log: %q", msg)
	}
	if !strings.HasPrefix(msg, "eve ") || !strings.HasSuffix(strings.TrimSpace(msg), "logged in") {
		t.Errorf("the text itself was damaged: %q", msg)
	}
	l.Errorf("ftp", "%s", strings.Repeat("ก", 5000)) // multi-byte text must be cut on a character boundary
	long := l.Entries()[1].Message
	if len(long) > maxLogMessage+8 || !strings.HasSuffix(long, "…") || strings.ContainsRune(long, '�') {
		t.Errorf("long message: %d bytes, ends %q", len(long), long[len(long)-8:])
	}
}

func TestPasswordsSetFromTheUIHaveAMinimumLength(t *testing.T) {
	m := newTestManager(t)
	if err := m.SetUser("carol", "short", false); err == nil {
		t.Error("a 5-character password was accepted")
	}
	if userDBHas(m.Config(), "carol", "short") {
		t.Error("the user was created anyway")
	}
	if err := m.SetUser("carol", "long-enough", false); err != nil {
		t.Errorf("a good password was refused: %v", err)
	}
	if err := m.SetUser("carol", "", true); err != nil { // empty keeps the password, e.g. to change read-only
		t.Errorf("changing only the read-only flag: %v", err)
	}
	if !userDBHas(m.Config(), "carol", "long-enough") {
		t.Error("the password was lost")
	}
}
