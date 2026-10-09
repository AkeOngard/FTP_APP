package main

import (
	"bytes"
	"fmt"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"ftpapp/internal/core"
)

// startSFTP returns a manager with an SFTP server running on loopback, and the address to connect to.
func startSFTP(t *testing.T) (*core.Manager, core.ConnectParams, string) {
	t.Helper()
	mgr, err := core.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr.KnownHosts().TrustNew = true
	cfg := mgr.Config()
	cfg.SFTP.BindAddr, cfg.SFTP.Port = "127.0.0.1", 0
	if err := cfg.SetUser("u", "pw", false); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Start(core.ServiceSFTP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Shutdown)
	addr := mgr.Status()[1].Addr
	host, port, _ := net.SplitHostPort(addr)
	n, _ := strconv.Atoi(port)
	return mgr, core.ConnectParams{Protocol: core.ProtoSFTP, Host: host, Port: n, User: "u", Password: "pw", TimeoutSecs: 5, Parallel: 4}, mgr.ResolveRoot(cfg.SFTP.Root)
}

func waitTransfer(t *testing.T, a *App, id string) TransferInfo {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, ti := range a.GetTransfers() {
			if ti.ID == id && (ti.State == stateDone || ti.State == stateError || ti.State == stateCancelled) {
				return ti
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("transfer %s did not finish", id)
	return TransferInfo{}
}

func TestFolderTransferReportsWholeFolderFigures(t *testing.T) {
	mgr, params, serverRoot := startSFTP(t)
	a := NewApp(mgr)
	sid, err := mgr.Connect(t.Context(), params)
	if err != nil {
		t.Fatal(err)
	}

	src := filepath.Join(t.TempDir(), "photos")
	var total int64
	rng := rand.New(rand.NewSource(1))
	for d := 0; d < 4; d++ {
		for i := 0; i < 8; i++ {
			data := make([]byte, 20_000+d*1000+i*10)
			rng.Read(data)
			p := filepath.Join(src, fmt.Sprintf("d%d", d), fmt.Sprintf("f%d.bin", i))
			os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, data, 0o644); err != nil {
				t.Fatal(err)
			}
			total += int64(len(data))
		}
	}

	if err := a.Upload(sid, []string{src}, "/"); err != nil {
		t.Fatal(err)
	}
	up := waitTransfer(t, a, a.GetTransfers()[0].ID)
	if up.State != stateDone || up.Error != "" {
		t.Fatalf("upload: %+v", up)
	}
	if up.Files != 32 || up.FilesTotal != 32 || up.OverallTotal != total || up.OverallDone != total || up.ActiveFiles != 0 {
		t.Errorf("upload figures: files %d/%d, bytes %d/%d (want 32/32 and %d/%d), active %d",
			up.Files, up.FilesTotal, up.OverallDone, up.OverallTotal, total, total, up.ActiveFiles)
	}
	got, err := os.ReadFile(filepath.Join(serverRoot, "photos", "d2", "f5.bin"))
	want, _ := os.ReadFile(filepath.Join(src, "d2", "f5.bin"))
	if err != nil || !bytes.Equal(got, want) {
		t.Errorf("a file differs on the server: %v", err)
	}

	dst := t.TempDir()
	if err := a.Download(sid, []RemoteItem{{Name: "photos", Path: "/photos", IsDir: true, Size: -1}}, dst); err != nil {
		t.Fatal(err)
	}
	down := waitTransfer(t, a, a.GetTransfers()[1].ID)
	if down.State != stateDone || down.Files != 32 || down.OverallTotal != total || down.OverallDone != total {
		t.Errorf("download: state %s files %d bytes %d/%d (want 32 files, %d bytes) err=%q",
			down.State, down.Files, down.OverallDone, down.OverallTotal, total, down.Error)
	}
}

func TestSelectedFilesShareThePoolAndAllFinish(t *testing.T) {
	mgr, params, serverRoot := startSFTP(t)
	params.Parallel = 2
	a := NewApp(mgr)
	sid, err := mgr.Connect(t.Context(), params)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	var paths []string
	for i := 0; i < 9; i++ { // more items than connections: the rest wait their turn
		p := filepath.Join(dir, fmt.Sprintf("f%d.txt", i))
		os.WriteFile(p, bytes.Repeat([]byte{byte('a' + i)}, 5000+i), 0o644)
		paths = append(paths, p)
	}
	if err := a.Upload(sid, paths, "/"); err != nil {
		t.Fatal(err)
	}
	for _, ti := range a.GetTransfers() {
		if done := waitTransfer(t, a, ti.ID); done.State != stateDone || done.Files != 1 {
			t.Errorf("%s: %+v", ti.Name, done)
		}
	}
	for i := range paths {
		got, err := os.ReadFile(filepath.Join(serverRoot, fmt.Sprintf("f%d.txt", i)))
		if err != nil || len(got) != 5000+i || got[0] != byte('a'+i) {
			t.Errorf("f%d.txt on the server: %d bytes, %v", i, len(got), err)
		}
	}
	pool, _ := mgr.Pool(sid)
	if pool.Max() != 2 {
		t.Errorf("pool size %d, want the session's setting of 2", pool.Max())
	}
}

func TestCancellingAQueuedTransferDoesNotHangThePool(t *testing.T) {
	mgr, params, _ := startSFTP(t)
	params.Parallel = 1
	a := NewApp(mgr)
	sid, err := mgr.Connect(t.Context(), params)
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := mgr.Pool(sid)
	hold, err := pool.Get(t.Context()) // occupy the only connection so the next transfer has to wait
	if err != nil {
		t.Fatal(err)
	}

	p := filepath.Join(t.TempDir(), "x.bin")
	os.WriteFile(p, []byte("data"), 0o644)
	if err := a.Upload(sid, []string{p}, "/"); err != nil {
		t.Fatal(err)
	}
	id := a.GetTransfers()[0].ID
	time.Sleep(100 * time.Millisecond)
	if st := a.GetTransfers()[0].State; st != stateQueued {
		t.Fatalf("a transfer with no free connection must stay queued, is %s", st)
	}
	a.CancelTransfer(id)
	if got := waitTransfer(t, a, id); got.State != stateCancelled {
		t.Errorf("state = %s, want cancelled", got.State)
	}
	pool.Put(hold, false)
	c, err := pool.Get(t.Context())
	if err != nil {
		t.Fatalf("pool unusable afterwards: %v", err)
	}
	pool.Put(c, false)
}

// What the UI does on first contact with an SFTP server: ask, trust, connect again.
func TestConnectAsksBeforeTrustingANewSFTPServer(t *testing.T) {
	mgr, params, _ := startSFTP(t)
	mgr.KnownHosts().TrustNew = false // as the app runs
	a := NewApp(mgr)

	info, err := a.Connect(params)
	if err != nil {
		t.Fatalf("an unknown host is a question, not an error: %v", err)
	}
	q := info.UnknownHost
	if q == nil || info.ID != "" {
		t.Fatalf("connected to a server nobody accepted: %+v", info)
	}
	if q.Host != net.JoinHostPort(params.Host, strconv.Itoa(params.Port)) || q.KeyType != "ssh-ed25519" || len(q.Fingerprint) != len("SHA256:")+43 {
		t.Errorf("prompt: %+v", q)
	}

	if err := a.TrustHost(q.Host, *q); err != nil {
		t.Fatal(err)
	}
	info, err = a.Connect(params)
	if err != nil || info.UnknownHost != nil || info.ID == "" {
		t.Fatalf("after accepting: %+v, %v", info, err)
	}
	if _, err := a.ListRemote(info.ID, "/"); err != nil {
		t.Errorf("the session does not work: %v", err)
	}
	a.Disconnect(info.ID)
}

func TestTrimMemoryWaitsForTransfers(t *testing.T) {
	mgr, err := core.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := NewApp(mgr)

	if !a.TrimMemory() {
		t.Error("an idle app should trim")
	}

	// a transfer on one of our servers
	h := mgr.Monitor().Begin("sftp", "alice", "10.0.0.2:1", "upload", "f", -1)
	if a.TrimMemory() {
		t.Error("must not trim while a server transfer is running")
	}
	h.End(nil)
	if !a.TrimMemory() {
		t.Error("should trim again once the transfer has finished")
	}

	// a transfer started from the client side
	a.transfers["t1"] = &transfer{info: TransferInfo{ID: "t1", State: stateRunning}, cancel: func() {}}
	if a.TrimMemory() {
		t.Error("must not trim while a client transfer is running")
	}
	for _, s := range []string{stateQueued, stateScanning} {
		a.transfers["t1"].info.State = s
		if a.TrimMemory() {
			t.Errorf("must not trim while a transfer is %s", s)
		}
	}
	a.transfers["t1"].info.State = stateDone
	if !a.TrimMemory() {
		t.Error("a finished transfer must not block trimming")
	}
}
