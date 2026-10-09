package core

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHumanFormats(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KB", 1536: "1.5 KB", 5 << 20: "5.0 MB", 250 << 20: "250 MB", 3 << 30: "3.0 GB"} {
		if got := HumanBytes(n); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", n, got, want)
		}
	}
	for d, want := range map[time.Duration]string{
		400 * time.Millisecond: "0.4s", 3400 * time.Millisecond: "3.4s", 5 * time.Second: "5s", 0: "0s", 45 * time.Second: "45s",
		192 * time.Second: "3m 12s", 3900 * time.Second: "1h 05m",
	} {
		if got := HumanDuration(d); got != want {
			t.Errorf("HumanDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestRateAndETA(t *testing.T) {
	t0 := time.Unix(1000, 0)
	r := NewRate(t0)
	if s := r.Update(1<<20, t0.Add(100*time.Millisecond)); s != 0 {
		t.Errorf("a sample shorter than the minimum window must be ignored, got %v", s)
	}
	if s := r.Update(1<<20, t0.Add(time.Second)); s < 0.99*(1<<20) || s > 1.01*(1<<20) {
		t.Errorf("first speed = %v, want about 1 MiB/s", s)
	}
	// the transfer doubles its speed: the estimate moves towards it without jumping all the way
	s := r.Update(3<<20, t0.Add(2*time.Second))
	if s <= 1<<20 || s >= 2<<20 {
		t.Errorf("smoothed speed = %v, want between the old (1 MiB/s) and new (2 MiB/s) speed", s)
	}

	if secs, ok := ETA(10<<20, 2<<20); !ok || secs != 5 {
		t.Errorf("ETA = %d, %v; want 5, true", secs, ok)
	}
	if _, ok := ETA(100, 0); ok {
		t.Error("ETA must be unknown at zero speed")
	}
	if _, ok := ETA(-1, 100); ok {
		t.Error("ETA must be unknown for an unknown remainder")
	}
}

func TestOverallAcrossFiles(t *testing.T) {
	t0 := time.Unix(0, 0)
	o := NewOverall(1000, t0)

	st := o.Update("a", 50, t0.Add(time.Second))
	if st.Done != 50 || st.FilesDone != 0 || st.Active != 1 {
		t.Fatalf("after a@50: %+v", st)
	}
	st = o.Update("a", 100, t0.Add(2*time.Second))
	st = o.FileDone("a", t0.Add(2*time.Second))
	if st.Done != 100 || st.FilesDone != 1 || st.Active != 0 {
		t.Fatalf("after a finished: %+v", st)
	}
	st = o.Update("b", 40, t0.Add(3*time.Second))
	if st.Done != 140 || st.FilesDone != 1 {
		t.Fatalf("after b@40: %+v, want done=140 files=1", st)
	}
	if st.Speed <= 0 || st.EtaSecs <= 0 {
		t.Errorf("speed and ETA should be known mid-transfer: %+v", st)
	}
	st = o.Update("b", 30, t0.Add(4*time.Second)) // an out-of-order report must not go backwards
	if st.Done != 140 {
		t.Errorf("progress went backwards: %+v", st)
	}

	unknown := NewOverall(-1, t0)
	if st := unknown.Update("x", 10, t0.Add(time.Second)); st.EtaSecs != -1 {
		t.Errorf("unknown total must give an unknown ETA, got %d", st.EtaSecs)
	}
}

// logLines returns the messages logged for a service.
func logLines(l *Logger, service string) []LogEntry {
	var out []LogEntry
	for _, e := range l.Entries() {
		if e.Service == service {
			out = append(out, e)
		}
	}
	return out
}

func joined(es []LogEntry) string {
	var b strings.Builder
	for _, e := range es {
		b.WriteString(e.Level + ": " + e.Message + "\n")
	}
	return b.String()
}

func newFastMonitor() (*Monitor, *Logger) {
	l := NewLogger()
	m := NewMonitor(l)
	m.tickEvery, m.startDelay, m.logEvery = 20*time.Millisecond, 60*time.Millisecond, 80*time.Millisecond
	return m, l
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestMonitorLogsLongTransfers(t *testing.T) {
	m, l := newFastMonitor()

	up := m.Begin("ftp", "alice", "10.0.0.2:5000", "upload", "/big.bin", -1)
	down := m.Begin("sftp", "bob", "10.0.0.3:6000", "download", "/movie.mkv", 1000)
	up.Add(500)
	down.Add(250)

	waitFor(t, "progress lines", func() bool {
		return strings.Contains(joined(logLines(l, "ftp")), "alice@10.0.0.2 is uploading /big.bin") &&
			strings.Contains(joined(logLines(l, "sftp")), "bob@10.0.0.3 is downloading /movie.mkv")
	})
	if got := joined(logLines(l, "ftp")); !strings.Contains(got, "500 B received so far") {
		t.Errorf("an upload of unknown size must show bytes received, got:\n%s", got)
	}
	if got := joined(logLines(l, "sftp")); !strings.Contains(got, "250 B of 1000 B (25%)") {
		t.Errorf("a download of known size must show how much of the total is done, got:\n%s", got)
	}

	snap := m.Snapshot()
	if len(snap.Active) != 2 || snap.Active[0].Name != "/big.bin" || snap.Active[0].Total != -1 || snap.Active[0].EtaSecs != -1 {
		t.Fatalf("snapshot = %+v", snap.Active)
	}

	up.End(nil)
	down.End(errors.New("connection reset"))
	waitFor(t, "finish lines", func() bool {
		return strings.Contains(joined(logLines(l, "ftp")), "alice@10.0.0.2 uploaded /big.bin: 500 B in") &&
			strings.Contains(joined(logLines(l, "sftp")), "interrupted after 250 B")
	})
	if !strings.Contains(joined(logLines(l, "sftp")), "error: bob@10.0.0.3: download of /movie.mkv interrupted") {
		t.Errorf("an interrupted transfer must be logged as an error:\n%s", joined(logLines(l, "sftp")))
	}
	snap = m.Snapshot()
	if len(snap.Active) != 0 || len(snap.Recent) != 2 {
		t.Fatalf("after finishing: %d active, %d recent", len(snap.Active), len(snap.Recent))
	}
	if r := snap.Recent[0]; r.Name != "/movie.mkv" || r.Error == "" || r.Bytes != 250 {
		t.Errorf("most recent entry = %+v", r)
	}
}

func TestMonitorShortTransfersOnlyLogTheEnd(t *testing.T) {
	m, l := newFastMonitor()
	h := m.Begin("ftp", "alice", "10.0.0.2:1", "upload", "/tiny.txt", -1)
	h.Add(10)
	h.End(nil)
	h.End(errors.New("a second End must be ignored"))

	time.Sleep(200 * time.Millisecond) // longer than startDelay: nothing may be logged for it any more
	lines := logLines(l, "ftp")
	if len(lines) != 1 || !strings.Contains(lines[0].Message, "alice@10.0.0.2 uploaded /tiny.txt: 10 B") || lines[0].Level != "info" {
		t.Errorf("want exactly one finish line, got:\n%s", joined(lines))
	}
	if n := len(m.Snapshot().Recent); n != 1 {
		t.Errorf("recent = %d entries, want 1", n)
	}
}

func TestMonitorShowsPartialTransfersHonestly(t *testing.T) {
	m, l := newFastMonitor()

	// A client that stops early closes the file normally: the server must not claim a full transfer.
	stopped := m.Begin("sftp", "bob", "10.0.0.3:1", "download", "/movie.mkv", 1000)
	stopped.Add(400)
	stopped.End(nil)
	// An upload whose announced size matches the bytes received is a plain success.
	whole := m.Begin("tftp", "-", "10.0.0.4:1", "upload", "fw.bin", -1)
	whole.SetTotal(300)
	whole.Add(300)
	whole.End(nil)

	if got := joined(logLines(l, "sftp")); !strings.Contains(got, "bob@10.0.0.3 downloaded /movie.mkv: 400 B of 1000 B in") {
		t.Errorf("an incomplete download must show how much of the file moved:\n%s", got)
	}
	if got := joined(logLines(l, "tftp")); !strings.Contains(got, "10.0.0.4 uploaded fw.bin: 300 B in") || strings.Contains(got, " of ") {
		t.Errorf("a complete transfer must show a plain size:\n%s", got)
	}
	r := m.Snapshot().Recent
	if r[1].Total != 1000 || r[1].Bytes != 400 || r[0].Total != 300 {
		t.Errorf("recent entries should carry the known totals: %+v", r)
	}
}

func TestMonitorSaysAlmostDone(t *testing.T) {
	msg := progressMessage(TransferStatus{User: "a", Remote: "1.2.3.4:5", Dir: "download", Name: "f", Bytes: 99, Total: 100, Speed: 50, EtaSecs: 0})
	if !strings.Contains(msg, "almost done") || strings.Contains(msg, "0s left") {
		t.Errorf("an ETA of zero should read 'almost done', got %q", msg)
	}
	msg = progressMessage(TransferStatus{User: "a", Dir: "download", Name: "f", Bytes: 50, Total: 100, Speed: 10, EtaSecs: 5})
	if !strings.Contains(msg, "about 5s left") {
		t.Errorf("whole-second ETA should print without decimals, got %q", msg)
	}
}

func TestWhoNamesTheClient(t *testing.T) {
	for _, c := range []struct{ user, remote, want string }{
		{"alice", "10.0.0.2:5000", "alice@10.0.0.2"},
		{"-", "10.0.0.2:69", "10.0.0.2"},
		{"alice", "", "alice"},
		{"bob", "[::1]:22", "bob@::1"},
	} {
		if got := who(c.user, c.remote); got != c.want {
			t.Errorf("who(%q, %q) = %q, want %q", c.user, c.remote, got, c.want)
		}
	}
}

func TestMonitorRecentIsBounded(t *testing.T) {
	m, _ := newFastMonitor()
	for i := 0; i < recentKept+5; i++ {
		m.Begin("ftp", "u", "x", "upload", "f", -1).End(nil)
	}
	if n := len(m.Snapshot().Recent); n != recentKept {
		t.Errorf("recent = %d, want %d", n, recentKept)
	}
}

func TestMonitorNotifiesSubscribers(t *testing.T) {
	m, _ := newFastMonitor()
	var mu sync.Mutex
	var got []TransferSnapshot
	cancel := m.Subscribe(func(s TransferSnapshot) { mu.Lock(); got = append(got, s); mu.Unlock() })

	h := m.Begin("tftp", "-", "1.2.3.4:5", "download", "fw.bin", 100)
	waitFor(t, "a snapshot with the active transfer", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) > 0 && len(got[len(got)-1].Active) == 1
	})
	h.End(nil)
	waitFor(t, "a snapshot after it finished", func() bool {
		mu.Lock()
		defer mu.Unlock()
		last := got[len(got)-1]
		return len(last.Active) == 0 && len(last.Recent) == 1
	})
	cancel()
	mu.Lock()
	n := len(got)
	mu.Unlock()
	m.Begin("tftp", "-", "x", "download", "y", 1).End(nil)
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != n {
		t.Error("an unsubscribed callback was still called")
	}
}

// Every protocol must report its transfers: sizes in the recent list and a finish line in the log.
func TestServersReportTransfers(t *testing.T) {
	m, params := startAllServers(t, nil)
	ctx := context.Background()
	data := randomBytes(t, 400_000)

	for _, proto := range []string{ProtoFTP, ProtoSFTP, ProtoTFTP} {
		t.Run(proto, func(t *testing.T) {
			c := mustConnect(t, m, params[proto])
			name := "mon-" + proto + ".bin"
			if err := c.Put(ctx, "/"+name, bytes.NewReader(data), int64(len(data)), nil); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, "/"+name, io.Discard, nil); err != nil {
				t.Fatal(err)
			}

			find := func(dir string) *FinishedTransfer {
				for _, r := range m.Monitor().Snapshot().Recent {
					if r.Service == proto && r.Dir == dir && strings.Contains(r.Name, name) {
						return &r
					}
				}
				return nil
			}
			waitFor(t, proto+" upload and download to be reported", func() bool { return find("upload") != nil && find("download") != nil })
			for _, dir := range []string{"upload", "download"} {
				r := find(dir)
				if r.Bytes != int64(len(data)) || r.Error != "" || r.Secs <= 0 || r.Speed <= 0 {
					t.Errorf("%s %s: %+v, want %d bytes, no error, positive time and speed", proto, dir, *r, len(data))
				}
			}
			logs := joined(logLines(m.Log(), proto))
			for _, want := range []string{"uploaded", "downloaded", "391 KB", "127.0.0.1"} { // 400000 bytes
				if !strings.Contains(logs, want) {
					t.Errorf("%s log misses %q:\n%s", proto, want, logs)
				}
			}
		})
	}
	if n := len(m.Monitor().Snapshot().Active); n != 0 {
		t.Errorf("%d transfers still marked active after all finished", n)
	}
}

func TestFTPUploadSizeFromALLO(t *testing.T) {
	m := NewMonitor(NewLogger())
	fs := &ftpFS{mon: m, alloc: -1}
	if fs.takeAlloc() != -1 {
		t.Fatal("no ALLO means an unknown size")
	}
	if err := fs.AllocateSpace(1234); err != nil {
		t.Fatal(err)
	}
	if got := fs.takeAlloc(); got != 1234 {
		t.Errorf("announced size = %d, want 1234", got)
	}
	if got := fs.takeAlloc(); got != -1 {
		t.Errorf("an ALLO applies to one upload only, got %d afterwards", got)
	}
}

func TestScanTotalsMatchTransfer(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	files := writeTree(t, src)
	var want int64
	for _, d := range files {
		want += int64(len(d))
	}
	// A symlink would be skipped by UploadTree and therefore must not be counted either.
	if err := os.Symlink(filepath.Join(src, "top.txt"), filepath.Join(src, "link")); err != nil {
		t.Logf("symlink not creatable here: %v", err)
	}

	n, b, err := ScanLocal(context.Background(), src)
	if err != nil || n != len(files) || b != want {
		t.Errorf("ScanLocal = %d files, %d bytes, %v; want %d, %d", n, b, err, len(files), want)
	}

	c := &fakeClient{list: func(dir string) ([]RemoteEntry, error) {
		switch dir {
		case "/":
			return []RemoteEntry{
				{Name: "a", Size: 100}, {Name: "../evil", Size: 999}, {Name: "link", IsLink: true, Size: 999},
				{Name: "sub", IsDir: true},
			}, nil
		case "/sub":
			return []RemoteEntry{{Name: "b", Size: 50}, {Name: "c", Size: 25}}, nil
		}
		return nil, errors.New("unexpected " + dir)
	}}
	n, b, err = ScanRemote(context.Background(), c, "/")
	if err != nil || n != 3 || b != 175 {
		t.Errorf("ScanRemote = %d files, %d bytes, %v; want 3 files and 175 bytes (unsafe names and links excluded)", n, b, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := ScanLocal(ctx, src); !errors.Is(err, context.Canceled) {
		t.Errorf("ScanLocal ignored cancellation: %v", err)
	}
}
