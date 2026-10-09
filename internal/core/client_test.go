package core

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

func splitAddr(t *testing.T, addr string) (string, int) {
	t.Helper()
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	return h, portOf(t, addr)
}

// startAllServers starts FTP (with optional TLS), SFTP and TFTP and returns client parameters
// for each protocol, signed in as alice.
func startAllServers(t *testing.T, mutate func(*Config)) (*Manager, map[string]ConnectParams) {
	t.Helper()
	m := newTestManager(t)
	cfg := m.Config()
	cfg.FTP.TLS = true
	if mutate != nil {
		mutate(&cfg)
	}
	if err := m.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	params := map[string]ConnectParams{}
	for proto, svc := range map[string]string{ProtoFTP: ServiceFTP, ProtoFTPS: ServiceFTP, ProtoSFTP: ServiceSFTP, ProtoTFTP: ServiceTFTP} {
		if st := statusOf(m, svc); !st.Running {
			startService(t, m, svc)
		}
		host, port := splitAddr(t, statusOf(m, svc).Addr)
		params[proto] = ConnectParams{
			Protocol: proto, Host: host, Port: port, User: "alice", Password: "secret",
			SkipTLSVerify: true, TimeoutSecs: 5,
		}
	}
	return m, params
}

func statusOf(m *Manager, name string) ServiceStatus {
	for _, st := range m.Status() {
		if st.Name == name {
			return st
		}
	}
	return ServiceStatus{}
}

func mustConnect(t *testing.T, m *Manager, p ConnectParams) RemoteClient {
	t.Helper()
	c, err := Connect(context.Background(), p, m.KnownHosts())
	if err != nil {
		t.Fatalf("connect %s: %v", p.Protocol, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// lastProgress records the most recent progress callback; safe for concurrent callers.
type lastProgress struct{ done, total atomic.Int64 }

func (l *lastProgress) fn(done, total int64) { l.done.Store(done); l.total.Store(total) }

func TestClientBrowsableProtocols(t *testing.T) {
	m, params := startAllServers(t, nil)
	ctx := context.Background()

	for _, proto := range []string{ProtoFTP, ProtoFTPS, ProtoSFTP} {
		t.Run(proto, func(t *testing.T) {
			c := mustConnect(t, m, params[proto])
			if !c.Caps().Browse || !c.Caps().Modify || c.Protocol() != proto {
				t.Fatalf("caps/protocol: %+v %s", c.Caps(), c.Protocol())
			}
			wd, err := c.Getwd()
			if err != nil || wd != "/" {
				t.Fatalf("Getwd = %q, %v", wd, err)
			}

			dir := "work-" + proto
			if err := c.Mkdir(dir); err != nil {
				t.Fatal(err)
			}
			data := randomBytes(t, 300_000)
			var up lastProgress
			if err := c.Put(ctx, dir+"/a.bin", bytes.NewReader(data), int64(len(data)), up.fn); err != nil {
				t.Fatalf("Put: %v", err)
			}
			if up.done.Load() != int64(len(data)) || up.total.Load() != int64(len(data)) {
				t.Errorf("upload progress ended at %d/%d, want %d", up.done.Load(), up.total.Load(), len(data))
			}
			if got, err := os.ReadFile(filepath.Join(share(m), dir, "a.bin")); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("uploaded file differs on disk: %v", err)
			}

			entries, err := c.List(dir)
			if err != nil || len(entries) != 1 || entries[0].Name != "a.bin" || entries[0].Size != int64(len(data)) || entries[0].IsDir {
				t.Fatalf("List = %+v, %v", entries, err)
			}
			roots, err := c.List("/")
			if err != nil {
				t.Fatal(err)
			}
			if !hasEntry(roots, dir, true) {
				t.Errorf("root listing misses directory %s: %+v", dir, roots)
			}

			var buf bytes.Buffer
			var down lastProgress
			if err := c.Get(ctx, dir+"/a.bin", &buf, down.fn); err != nil || !bytes.Equal(buf.Bytes(), data) {
				t.Fatalf("Get differs: %v", err)
			}
			if down.done.Load() != int64(len(data)) || down.total.Load() != int64(len(data)) {
				t.Errorf("download progress ended at %d/%d, want %d", down.done.Load(), down.total.Load(), len(data))
			}

			if err := c.Rename(dir+"/a.bin", dir+"/b.bin"); err != nil {
				t.Fatal(err)
			}
			if err := c.Remove(dir + "/b.bin"); err != nil {
				t.Fatal(err)
			}
			if err := c.RemoveDir(dir); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, dir+"/gone.bin", io.Discard, nil); err == nil {
				t.Error("Get of a missing file succeeded")
			}
		})
	}
}

func hasEntry(es []RemoteEntry, name string, dir bool) bool {
	for _, e := range es {
		if e.Name == name && e.IsDir == dir {
			return true
		}
	}
	return false
}

func TestClientTFTP(t *testing.T) {
	m, params := startAllServers(t, nil)
	c := mustConnect(t, m, params[ProtoTFTP])
	ctx := context.Background()

	if c.Caps().Browse || c.Caps().Modify {
		t.Errorf("TFTP must not claim browse/modify: %+v", c.Caps())
	}
	if _, err := c.List("/"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("List: got %v, want ErrUnsupported", err)
	}
	if err := c.Mkdir("x"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Mkdir: got %v, want ErrUnsupported", err)
	}

	data := randomBytes(t, 700_000)
	var up lastProgress
	if err := c.Put(ctx, "t.bin", bytes.NewReader(data), int64(len(data)), up.fn); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if up.done.Load() != int64(len(data)) {
		t.Errorf("upload progress ended at %d, want %d", up.done.Load(), len(data))
	}
	var buf bytes.Buffer
	var down lastProgress
	if err := c.Get(ctx, "t.bin", &buf, down.fn); err != nil || !bytes.Equal(buf.Bytes(), data) {
		t.Fatalf("Get differs: %v", err)
	}
	if down.total.Load() != int64(len(data)) {
		t.Errorf("download total = %d, want %d (tsize)", down.total.Load(), len(data))
	}
}

func TestClientBadCredentials(t *testing.T) {
	m, params := startAllServers(t, nil)
	for _, proto := range []string{ProtoFTP, ProtoFTPS, ProtoSFTP} {
		p := params[proto]
		p.Password = "wrong"
		if c, err := Connect(context.Background(), p, m.KnownHosts()); err == nil {
			c.Close()
			t.Errorf("%s: connect with a wrong password succeeded", proto)
		}
	}
}

func TestClientParamValidation(t *testing.T) {
	for name, p := range map[string]ConnectParams{
		"unknown protocol": {Protocol: "gopher", Host: "h"},
		"empty host":       {Protocol: ProtoFTP, Host: "  "},
		"bad port":         {Protocol: ProtoFTP, Host: "h", Port: 70000},
	} {
		if c, err := Connect(context.Background(), p, nil); err == nil {
			c.Close()
			t.Errorf("%s: expected an error", name)
		}
	}
	if c, err := Connect(context.Background(), ConnectParams{Protocol: ProtoSFTP, Host: "h"}, nil); err == nil {
		c.Close()
		t.Error("SFTP without credentials should fail")
	}
}

func TestFTPSCertificateVerification(t *testing.T) {
	m, params := startAllServers(t, nil)
	p := params[ProtoFTPS]
	p.SkipTLSVerify = false
	if c, err := Connect(context.Background(), p, m.KnownHosts()); err == nil {
		c.Close()
		t.Error("self-signed certificate was accepted without SkipTLSVerify")
	}
}

func TestFTPRequireTLS(t *testing.T) {
	m, params := startAllServers(t, func(c *Config) { c.FTP.RequireTLS = true })
	if c, err := Connect(context.Background(), params[ProtoFTP], m.KnownHosts()); err == nil {
		c.Close()
		t.Error("plain FTP login succeeded although TLS is required")
	}
	c := mustConnect(t, m, params[ProtoFTPS])
	if _, err := c.List("/"); err != nil {
		t.Errorf("FTPS list on a TLS-only server: %v", err)
	}
}

// cancelAfterWriter cancels ctx once n bytes have been written.
type cancelAfterWriter struct {
	n       int
	cancel  context.CancelFunc
	written int
}

func (w *cancelAfterWriter) Write(p []byte) (int, error) {
	w.written += len(p)
	if w.written >= w.n {
		w.cancel()
	}
	return len(p), nil
}

type cancelAfterReader struct {
	r      io.Reader
	n      int
	cancel context.CancelFunc
	read   int
}

func (r *cancelAfterReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.read += n
	if r.read >= r.n {
		r.cancel()
	}
	return n, err
}

func TestClientCancelKeepsConnectionUsable(t *testing.T) {
	m, params := startAllServers(t, nil)
	big := randomBytes(t, 12_000_000)
	if err := os.MkdirAll(share(m), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(share(m), "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}

	for _, proto := range []string{ProtoFTP, ProtoSFTP} {
		t.Run(proto, func(t *testing.T) {
			c := mustConnect(t, m, params[proto])

			ctx, cancel := context.WithCancel(context.Background())
			err := c.Get(ctx, "big.bin", &cancelAfterWriter{n: 500_000, cancel: cancel}, nil)
			cancel()
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled Get returned %v, want context.Canceled", err)
			}
			if _, err := c.List("/"); err != nil {
				t.Fatalf("connection unusable after cancelled download: %v", err)
			}

			ctx, cancel = context.WithCancel(context.Background())
			src := &cancelAfterReader{r: bytes.NewReader(big), n: 500_000, cancel: cancel}
			err = c.Put(ctx, "partial-"+proto+".bin", src, int64(len(big)), nil)
			cancel()
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled Put returned %v, want context.Canceled", err)
			}
			if _, err := c.List("/"); err != nil {
				t.Fatalf("connection unusable after cancelled upload: %v", err)
			}

			// And a normal transfer still works afterwards.
			var buf bytes.Buffer
			if err := c.Get(context.Background(), "big.bin", &buf, nil); err != nil || !bytes.Equal(buf.Bytes(), big) {
				t.Fatalf("download after cancel differs: %v", err)
			}
		})
	}
}

func TestClientContextAlreadyCancelled(t *testing.T) {
	m, params := startAllServers(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, proto := range []string{ProtoFTP, ProtoSFTP, ProtoTFTP} {
		if c, err := Connect(ctx, params[proto], m.KnownHosts()); err == nil {
			// TFTP has no connection phase, so it connects but must refuse to transfer.
			if err := c.Get(ctx, "x", io.Discard, nil); err == nil {
				t.Errorf("%s: transfer with a cancelled context succeeded", proto)
			}
			c.Close()
		}
	}
}

// writeTree creates a small nested tree with an empty directory and returns its files.
func writeTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{
		"top.txt":           []byte("top"),
		"docs/readme.md":    []byte("# hello"),
		"docs/deep/big.bin": randomBytes(t, 150_000),
		"docs/deep/z.txt":   []byte("z"),
	}
	for rel, data := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	return files
}

func assertTree(t *testing.T, root string, files map[string][]byte) {
	t.Helper()
	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s differs or is missing: %v", rel, err)
		}
	}
	if st, err := os.Stat(filepath.Join(root, "empty")); err != nil || !st.IsDir() {
		t.Errorf("empty directory was not recreated: %v", err)
	}
	// No leftover partial files.
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".part") {
			t.Errorf("leftover partial file %s", p)
		}
		return nil
	})
}

func TestClientTrees(t *testing.T) {
	m, params := startAllServers(t, nil)
	ctx := context.Background()
	for _, proto := range []string{ProtoFTP, ProtoSFTP} {
		t.Run(proto, func(t *testing.T) {
			c := mustConnect(t, m, params[proto])
			src := filepath.Join(t.TempDir(), "src")
			files := writeTree(t, src)

			var lastFile atomic.Value
			n, err := UploadTree(ctx, NewFixedPool(c), src, "/tree-"+proto, TreeOptions{Progress: func(file string, _, _ int64) { lastFile.Store(file) }})
			if err != nil || n != len(files) {
				t.Fatalf("UploadTree = %d, %v; want %d files", n, err, len(files))
			}
			if lastFile.Load() == nil {
				t.Error("progress was never reported with a file name")
			}
			assertTree(t, filepath.Join(share(m), "tree-"+proto), files)

			// Uploading again must work over the existing directories.
			if n, err := UploadTree(ctx, NewFixedPool(c), src, "/tree-"+proto, TreeOptions{}); err != nil || n != len(files) {
				t.Fatalf("second UploadTree = %d, %v", n, err)
			}

			dst := filepath.Join(t.TempDir(), "dst")
			n, err = DownloadTree(ctx, NewFixedPool(c), "/tree-"+proto, dst, TreeOptions{})
			if err != nil || n != len(files) {
				t.Fatalf("DownloadTree = %d, %v; want %d files", n, err, len(files))
			}
			assertTree(t, dst, files)

			if err := RemoveTree(ctx, c, "/tree-"+proto); err != nil {
				t.Fatalf("RemoveTree: %v", err)
			}
			if _, err := os.Stat(filepath.Join(share(m), "tree-"+proto)); err == nil {
				t.Error("remote tree still exists after RemoveTree")
			}
		})
	}
}

func TestDownloadFileCleansUpOnFailure(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(local, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	c := &fakeClient{get: func(w io.Writer) error { w.Write([]byte("partial")); return boom }}

	if err := DownloadFile(context.Background(), c, "/x", local, nil); !errors.Is(err, boom) {
		t.Fatalf("got %v, want boom", err)
	}
	if got, _ := os.ReadFile(local); string(got) != "old" {
		t.Errorf("existing file was damaged by a failed download: %q", got)
	}
	if _, err := os.Stat(local + ".part"); err == nil {
		t.Error("partial file left behind")
	}

	c.get = func(w io.Writer) error { _, err := w.Write([]byte("new")); return err }
	if err := DownloadFile(context.Background(), c, "/x", local, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(local); string(got) != "new" {
		t.Errorf("successful download not applied: %q", got)
	}
}

// fakeClient lets tests script a server's answers. Unset methods panic on the nil embedded interface.
type fakeClient struct {
	RemoteClient
	list func(dir string) ([]RemoteEntry, error)
	get  func(w io.Writer) error
}

func (f *fakeClient) List(dir string) ([]RemoteEntry, error) { return f.list(dir) }
func (f *fakeClient) Get(_ context.Context, _ string, w io.Writer, _ BytesFunc) error {
	return f.get(w)
}

func TestDownloadTreeRejectsHostileNames(t *testing.T) {
	base := t.TempDir()
	dst := filepath.Join(base, "inner", "dst")
	c := &fakeClient{
		list: func(dir string) ([]RemoteEntry, error) {
			return []RemoteEntry{
				{Name: "../evil.txt"},
				{Name: "..\\evil2.txt"},
				{Name: "sub/evil3.txt"},
				{Name: ".."},
				{Name: "."},
				{Name: "link", IsLink: true},
				{Name: "ok.txt"},
			}, nil
		},
		get: func(w io.Writer) error { _, err := w.Write([]byte("data")); return err },
	}
	n, err := DownloadTree(context.Background(), NewFixedPool(c), "/", dst, TreeOptions{})
	if n != 1 {
		t.Errorf("downloaded %d files, want only ok.txt", n)
	}
	if err == nil || !strings.Contains(err.Error(), "unsafe name") {
		t.Errorf("expected an 'unsafe name' error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "ok.txt")); err != nil {
		t.Errorf("safe file missing: %v", err)
	}
	filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Base(p) != "ok.txt" {
			t.Errorf("unexpected file written: %s", p)
		}
		return nil
	})
}

func TestDownloadTreeStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	c := &fakeClient{
		list: func(string) ([]RemoteEntry, error) {
			return []RemoteEntry{{Name: "a"}, {Name: "b"}, {Name: "c"}}, nil
		},
		get: func(w io.Writer) error { calls++; cancel(); return nil },
	}
	_, err := DownloadTree(ctx, NewFixedPool(c), "/", filepath.Join(t.TempDir(), "d"), TreeOptions{})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Errorf("err=%v, calls=%d; want Canceled after 1 file", err, calls)
	}
}

func TestKnownHostsTrustOnFirstUse(t *testing.T) {
	// Fix the SFTP port so a restarted server is the same "host" to the client.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	m, params := startAllServers(t, func(c *Config) { c.SFTP.Port = port })
	p := params[ProtoSFTP]
	hostPort := net.JoinHostPort(p.Host, strconv.Itoa(p.Port))

	mustConnect(t, m, p)
	mustConnect(t, m, p) // same key again: accepted
	if got := m.KnownHosts().List(); len(got) != 1 || got[hostPort] == "" {
		t.Fatalf("known hosts = %v, want an entry for %s", got, hostPort)
	}
	reloaded, err := LoadKnownHosts(m.Dir(), nil)
	if err != nil || reloaded.List()[hostPort] == "" {
		t.Fatalf("trusted key not persisted: %v %v", reloaded.List(), err)
	}

	// Replace the server's host key: the client must refuse.
	if err := m.Stop(ServiceSFTP); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(m.Dir(), hostKeyFileName)); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ServiceSFTP); err != nil {
		t.Fatal(err)
	}
	_, err = Connect(context.Background(), p, m.KnownHosts())
	var changed *HostKeyChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("got %v, want HostKeyChangedError", err)
	}

	// After the user forgets the host, the new key is learned.
	if err := m.KnownHosts().Forget(hostPort); err != nil {
		t.Fatal(err)
	}
	mustConnect(t, m, p)
}

// startKeyOnlySFTP runs a minimal SSH server that accepts one public key and serves an
// in-memory SFTP tree, to exercise key-file authentication.
func startKeyOnlySFTP(t *testing.T, authorized ssh.PublicKey) (host string, port int) {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(k.Marshal(), authorized.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("unauthorized key")
		},
	}
	cfg.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer nc.Close()
				sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
				if err != nil {
					return
				}
				defer sc.Close()
				go ssh.DiscardRequests(reqs)
				for nch := range chans {
					ch, chReqs, err := nch.Accept()
					if err != nil {
						return
					}
					go func() {
						defer ch.Close()
						for r := range chReqs {
							r.Reply(r.Type == "subsystem", nil)
							if r.Type == "subsystem" {
								sftp.NewRequestServer(ch, sftp.InMemHandler()).Serve()
								return
							}
						}
					}()
				}
			}()
		}
	}()
	h, p := splitAddr(t, ln.Addr().String())
	return h, p
}

func writeKey(t *testing.T, passphrase string) (path string, pub ssh.PublicKey) {
	t.Helper()
	pubKey, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(priv, "test")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "test", []byte(passphrase))
	}
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		t.Fatal(err)
	}
	return path, sshPub
}

func TestSFTPKeyAuth(t *testing.T) {
	known, err := LoadKnownHosts(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	known.TrustNew = true
	keyPath, pub := writeKey(t, "")
	host, port := startKeyOnlySFTP(t, pub)
	p := ConnectParams{Protocol: ProtoSFTP, Host: host, Port: port, User: "u", KeyFile: keyPath, TimeoutSecs: 5}

	c, err := Connect(context.Background(), p, known)
	if err != nil {
		t.Fatalf("key auth failed: %v", err)
	}
	defer c.Close()
	if err := c.Mkdir("/k"); err != nil {
		t.Fatal(err)
	}
	if es, err := c.List("/"); err != nil || !hasEntry(es, "k", true) {
		t.Fatalf("List = %+v, %v", es, err)
	}

	// A key the server does not know is refused.
	otherPath, _ := writeKey(t, "")
	p.KeyFile = otherPath
	if c, err := Connect(context.Background(), p, known); err == nil {
		c.Close()
		t.Error("an unauthorized key was accepted")
	}
}

func TestSFTPEncryptedKey(t *testing.T) {
	known, _ := LoadKnownHosts(t.TempDir(), nil)
	known.TrustNew = true
	keyPath, pub := writeKey(t, "s3cret")
	host, port := startKeyOnlySFTP(t, pub)
	p := ConnectParams{Protocol: ProtoSFTP, Host: host, Port: port, User: "u", KeyFile: keyPath, TimeoutSecs: 5}

	if c, err := Connect(context.Background(), p, known); err == nil {
		c.Close()
		t.Fatal("encrypted key without passphrase was accepted")
	} else if !strings.Contains(err.Error(), "passphrase") {
		t.Errorf("error should mention the passphrase, got: %v", err)
	}
	p.KeyPassphrase = "wrong"
	if c, err := Connect(context.Background(), p, known); err == nil {
		c.Close()
		t.Fatal("wrong passphrase was accepted")
	}
	p.KeyPassphrase = "s3cret"
	c, err := Connect(context.Background(), p, known)
	if err != nil {
		t.Fatalf("correct passphrase failed: %v", err)
	}
	c.Close()
}

func TestManagerSessionsAndSites(t *testing.T) {
	m, params := startAllServers(t, nil)

	id, err := m.Connect(context.Background(), params[ProtoSFTP])
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.Client(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.List("/"); err != nil {
		t.Fatal(err)
	}
	if err := m.Disconnect(id); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Client(id); err == nil {
		t.Error("session still found after Disconnect")
	}
	if _, err := m.Connect(context.Background(), ConnectParams{Protocol: ProtoSFTP, Host: "127.0.0.1", Port: 1, User: "x", Password: "y", TimeoutSecs: 2}); err == nil {
		t.Error("connecting to a closed port succeeded")
	}

	site := Site{Name: "Lab", Protocol: ProtoSFTP, Host: "10.0.0.5", User: "root"}
	if err := m.SaveSite(site); err != nil {
		t.Fatal(err)
	}
	site.Host = "10.0.0.6"
	if err := m.SaveSite(site); err != nil { // same name: replaced, not duplicated
		t.Fatal(err)
	}
	if sites := m.Config().Sites; len(sites) != 1 || sites[0].Host != "10.0.0.6" {
		t.Fatalf("sites = %+v", sites)
	}
	if err := m.SaveSite(Site{Name: "bad", Protocol: "gopher", Host: "h"}); err == nil {
		t.Error("a site with an unknown protocol was accepted")
	}
	if sites := m.Config().Sites; len(sites) != 1 {
		t.Errorf("a rejected site changed the config: %+v", sites)
	}
	cfg, _, err := LoadConfig(m.Dir())
	if err != nil || len(cfg.Sites) != 1 {
		t.Fatalf("sites not persisted: %+v %v", cfg.Sites, err)
	}
	raw, _ := os.ReadFile(filepath.Join(m.Dir(), configFileName))
	if strings.Contains(string(raw), "secret") {
		t.Error("a password leaked into config.json")
	}
	if err := m.DeleteSite("lab"); err != nil || len(m.Config().Sites) != 0 {
		t.Fatalf("DeleteSite: %v %+v", err, m.Config().Sites)
	}
}
