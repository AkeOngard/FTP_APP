package core

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// ---- human-readable formatting ----

// HumanBytes formats a byte count as "1.5 MB".
func HumanBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v, i := float64(n)/1024, 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if v >= 100 {
		return fmt.Sprintf("%.0f %s", v, units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// HumanDuration formats a duration as "3.4s", "45s", "3m 12s" or "1h 05m". Whole seconds print
// without a decimal ("5s"), which is what time-left estimates need.
func HumanDuration(d time.Duration) string {
	switch {
	case d < 10*time.Second && d%time.Second == 0:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()+0.5))
	case d < time.Hour:
		s := int(d.Seconds() + 0.5)
		return fmt.Sprintf("%dm %02ds", s/60, s%60)
	default:
		m := int(d.Minutes() + 0.5)
		return fmt.Sprintf("%dh %02dm", m/60, m%60)
	}
}

// ---- speed and time-left estimation ----

// Rate turns cumulative byte counts into a smoothed bytes-per-second figure.
type Rate struct {
	lastT time.Time
	lastB int64
	speed float64
}

// minRateSample is the shortest window a speed sample is taken over; faster calls are ignored.
const minRateSample = 250 * time.Millisecond

// NewRate starts measuring at start with zero bytes transferred.
func NewRate(start time.Time) *Rate { return &Rate{lastT: start} }

// Update feeds the total bytes transferred so far and returns the smoothed speed.
// Recent samples weigh more, so the figure follows speed changes without jumping about.
func (r *Rate) Update(total int64, now time.Time) float64 {
	dt := now.Sub(r.lastT)
	if dt < minRateSample {
		return r.speed
	}
	inst := float64(total-r.lastB) / dt.Seconds()
	if r.speed == 0 {
		r.speed = inst
	} else {
		r.speed = r.speed*0.6 + inst*0.4
	}
	r.lastT, r.lastB = now, total
	return r.speed
}

func (r *Rate) Speed() float64 { return r.speed }

// Overall folds the per-file progress reports of a multi-file transfer, several files of which may be
// moving at the same time, into whole-transfer figures: bytes done, files completed and in progress,
// smoothed speed and time left. Not safe for concurrent use; the caller serialises access.
type Overall struct {
	Total int64 // bytes in the whole transfer, -1 when unknown

	rate      *Rate
	base      int64            // bytes of files that have finished
	cur       map[string]int64 // bytes so far of each file in progress
	filesDone int
}

// OverallState is the whole-transfer picture after an update.
type OverallState struct {
	Done      int64
	FilesDone int
	Active    int     // files in progress right now
	Speed     float64 // bytes per second
	EtaSecs   int64   // -1 when unknown
}

func NewOverall(total int64, start time.Time) *Overall {
	return &Overall{Total: total, rate: NewRate(start), cur: map[string]int64{}}
}

// Update records that `file` has `done` bytes transferred.
func (o *Overall) Update(file string, done int64, now time.Time) OverallState {
	if done < o.cur[file] { // reports can arrive slightly out of order from concurrent workers
		done = o.cur[file]
	}
	o.cur[file] = done
	return o.state(now)
}

// FileDone records that `file` finished, moving its bytes from "in progress" to "done".
func (o *Overall) FileDone(file string, now time.Time) OverallState {
	if n, ok := o.cur[file]; ok {
		o.base += n
		delete(o.cur, file)
	}
	o.filesDone++
	return o.state(now)
}

func (o *Overall) state(now time.Time) OverallState {
	total := o.base
	for _, n := range o.cur {
		total += n
	}
	st := OverallState{Done: total, FilesDone: o.filesDone, Active: len(o.cur), Speed: o.rate.Update(total, now), EtaSecs: -1}
	if o.Total >= 0 {
		if secs, ok := ETA(o.Total-total, st.Speed); ok {
			st.EtaSecs = secs
		}
	}
	return st
}

// ETA estimates the seconds left for `remaining` bytes at `speed`; ok is false when unknown.
func ETA(remaining int64, speed float64) (secs int64, ok bool) {
	if remaining < 0 || speed < 1 {
		return 0, false
	}
	return int64(float64(remaining)/speed + 0.5), true
}

// ---- live monitor of transfers handled by our own servers ----

// TransferStatus describes one transfer in progress on one of our servers.
// Dir is from the client's point of view: "upload" means the client sends a file to us.
type TransferStatus struct {
	ID      string    `json:"id"`
	Service string    `json:"service"`
	User    string    `json:"user"`
	Remote  string    `json:"remote"`
	Dir     string    `json:"dir"`
	Name    string    `json:"name"`
	Bytes   int64     `json:"bytes"`
	Total   int64     `json:"total"`   // -1 when the client did not say how big the file is
	Speed   float64   `json:"speed"`   // bytes per second, smoothed
	EtaSecs int64     `json:"etaSecs"` // -1 when unknown
	Started time.Time `json:"started"`
}

// FinishedTransfer is a completed or interrupted transfer, kept briefly for the UI.
type FinishedTransfer struct {
	Service string    `json:"service"`
	User    string    `json:"user"`
	Remote  string    `json:"remote"`
	Dir     string    `json:"dir"`
	Name    string    `json:"name"`
	Bytes   int64     `json:"bytes"`
	Total   int64     `json:"total"` // file size when known, else -1; differs from Bytes when a client stopped early or resumed
	Secs    float64   `json:"secs"`
	Speed   float64   `json:"speed"`
	Error   string    `json:"error"`
	Ended   time.Time `json:"ended"`
}

type TransferSnapshot struct {
	Active []TransferStatus   `json:"active"`
	Recent []FinishedTransfer `json:"recent"`
}

const recentKept = 10

type activeTransfer struct {
	id, service, user, remote, dir, name string
	total                                int64
	started                              time.Time
	bytes                                atomic.Int64
	totalHint                            atomic.Int64 // total learned after Begin (e.g. TFTP tsize); -1 = none

	rate    *Rate
	lastLog time.Time
	logged  bool
}

// Monitor tracks transfers on our servers, writes progress to the log and feeds the UI.
// Per-file start lines are deliberately not logged: uploading a thousand small files would
// bury everything else. A transfer shows up in the log once it has run for a while, then
// periodically, and always once more when it finishes.
type Monitor struct {
	log *Logger

	// Timings are fields so tests can shorten them.
	tickEvery  time.Duration
	startDelay time.Duration
	logEvery   time.Duration

	mu            sync.Mutex
	active        map[string]*activeTransfer
	recent        []FinishedTransfer
	nextID        int
	running       bool
	notifyPending bool
	subs          map[int]func(TransferSnapshot)
	nextSub       int
}

func NewMonitor(log *Logger) *Monitor {
	return &Monitor{
		log:        log,
		tickEvery:  time.Second,
		startDelay: 2 * time.Second,
		logEvery:   5 * time.Second,
		active:     map[string]*activeTransfer{},
		subs:       map[int]func(TransferSnapshot){},
	}
}

// Subscribe registers fn for snapshots, sent about 6 times a second at most while anything changes.
func (m *Monitor) Subscribe(fn func(TransferSnapshot)) (cancel func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.nextSub
	m.nextSub++
	m.subs[id] = fn
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.subs, id)
	}
}

func (m *Monitor) Snapshot() TransferSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked(time.Now())
}

func (m *Monitor) snapshotLocked(now time.Time) TransferSnapshot {
	snap := TransferSnapshot{
		Active: make([]TransferStatus, 0, len(m.active)),
		Recent: append([]FinishedTransfer{}, m.recent...),
	}
	for _, t := range m.active {
		snap.Active = append(snap.Active, m.statusLocked(t))
	}
	sortStatuses(snap.Active)
	return snap
}

func (m *Monitor) statusLocked(t *activeTransfer) TransferStatus {
	b := t.bytes.Load()
	total := t.total
	if total < 0 {
		if h := t.totalHint.Load(); h >= 0 {
			total = h
		}
	}
	st := TransferStatus{
		ID: t.id, Service: t.service, User: t.user, Remote: t.remote, Dir: t.dir, Name: t.name,
		Bytes: b, Total: total, Speed: t.rate.Speed(), EtaSecs: -1, Started: t.started,
	}
	if total >= 0 {
		if secs, ok := ETA(total-b, st.Speed); ok {
			st.EtaSecs = secs
		}
	}
	return st
}

func sortStatuses(s []TransferStatus) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Started.Before(s[j-1].Started); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// scheduleNotify coalesces change notifications so a burst of small files does not flood the UI.
func (m *Monitor) scheduleNotify() {
	m.mu.Lock()
	if m.notifyPending {
		m.mu.Unlock()
		return
	}
	m.notifyPending = true
	m.mu.Unlock()
	time.AfterFunc(150*time.Millisecond, func() {
		m.mu.Lock()
		m.notifyPending = false
		snap := m.snapshotLocked(time.Now())
		subs := make([]func(TransferSnapshot), 0, len(m.subs))
		for _, fn := range m.subs {
			subs = append(subs, fn)
		}
		m.mu.Unlock()
		for _, fn := range subs {
			fn(snap)
		}
	})
}

// TransferHandle is how a protocol handler reports the progress of one transfer.
type TransferHandle struct {
	m    *Monitor
	t    *activeTransfer
	once sync.Once
}

// Begin registers a transfer. total is the file size if known, else -1.
func (m *Monitor) Begin(service, user, remote, dir, name string, total int64) *TransferHandle {
	now := time.Now()
	m.mu.Lock()
	m.nextID++
	t := &activeTransfer{
		id: fmt.Sprintf("s%d", m.nextID), service: service, user: user, remote: remote, dir: dir, name: name,
		total: total, started: now, rate: NewRate(now),
	}
	t.totalHint.Store(-1)
	m.active[t.id] = t
	start := !m.running
	m.running = true
	m.mu.Unlock()
	if start {
		go m.run()
	}
	m.scheduleNotify()
	return &TransferHandle{m: m, t: t}
}

// Add records n more bytes transferred.
func (h *TransferHandle) Add(n int) {
	if n > 0 {
		h.t.bytes.Add(int64(n))
	}
}

// SetTotal updates the expected size once the client announces it.
func (h *TransferHandle) SetTotal(n int64) {
	if n >= 0 {
		h.t.totalHint.Store(n)
	}
}

// End marks the transfer finished; err is nil on success. Only the first call counts.
func (h *TransferHandle) End(err error) { h.once.Do(func() { h.m.finish(h.t, err) }) }

func (m *Monitor) run() {
	tk := time.NewTicker(m.tickEvery)
	defer tk.Stop()
	for now := range tk.C {
		if !m.tick(now) {
			return
		}
	}
}

// tick samples speeds, logs progress for long transfers and reports whether any transfer is left.
func (m *Monitor) tick(now time.Time) bool {
	type line struct{ svc, msg string }
	var lines []line

	m.mu.Lock()
	for _, t := range m.active {
		t.rate.Update(t.bytes.Load(), now)
		if now.Sub(t.started) >= m.startDelay && (!t.logged || now.Sub(t.lastLog) >= m.logEvery) {
			lines = append(lines, line{t.service, progressMessage(m.statusLocked(t))})
			t.logged, t.lastLog = true, now
		}
	}
	more := len(m.active) > 0
	if !more {
		m.running = false
	}
	m.mu.Unlock()

	for _, l := range lines {
		m.log.Infof(l.svc, "%s", l.msg)
	}
	m.scheduleNotify()
	return more
}

func (m *Monitor) finish(t *activeTransfer, err error) {
	b := t.bytes.Load()
	d := time.Since(t.started)
	if d < time.Millisecond {
		d = time.Millisecond
	}
	total := t.total
	if total < 0 {
		total = t.totalHint.Load()
	}
	rec := FinishedTransfer{
		Service: t.service, User: t.user, Remote: t.remote, Dir: t.dir, Name: t.name,
		Bytes: b, Total: total, Secs: d.Seconds(), Speed: float64(b) / d.Seconds(), Ended: time.Now(),
	}
	if err != nil {
		rec.Error = err.Error()
	}
	m.mu.Lock()
	delete(m.active, t.id)
	m.recent = append([]FinishedTransfer{rec}, m.recent...)
	if len(m.recent) > recentKept {
		m.recent = m.recent[:recentKept]
	}
	m.mu.Unlock()

	if err != nil {
		m.log.Errorf(t.service, "%s: %s of %s interrupted after %s in %s: %v",
			who(t.user, t.remote), t.dir, t.name, HumanBytes(b), HumanDuration(d), err)
	} else {
		verb := map[string]string{"upload": "uploaded", "download": "downloaded"}[t.dir]
		// The server cannot tell a cancelled transfer from a finished one: the client just closes the
		// file. When fewer bytes than the file holds moved, say so rather than imply it completed
		// (a resumed download legitimately moves only the rest).
		size := HumanBytes(b)
		if total >= 0 && b != total {
			size = fmt.Sprintf("%s of %s", HumanBytes(b), HumanBytes(total))
		}
		m.log.Infof(t.service, "%s %s %s: %s in %s (%s/s)", who(t.user, t.remote), verb, t.name, size, HumanDuration(d), HumanBytes(int64(rec.Speed)))
	}
	m.scheduleNotify()
}

// who names the client in log lines: "alice@10.0.0.2", or just the address for TFTP, which has no login.
func who(user, remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	switch {
	case host == "":
		return user
	case user == "" || user == "-":
		return host
	}
	return user + "@" + host
}

// progressMessage is the log line for a transfer that has been running for a while.
func progressMessage(s TransferStatus) string {
	verb := map[string]string{"upload": "uploading", "download": "downloading"}[s.Dir]
	speed := ""
	if s.Speed >= 1 {
		speed = fmt.Sprintf(", %s/s", HumanBytes(int64(s.Speed)))
	}
	if s.Total >= 0 {
		pct := 100
		if s.Total > 0 {
			pct = int(float64(s.Bytes) * 100 / float64(s.Total))
		}
		left := ""
		switch {
		case s.EtaSecs == 0:
			left = ", almost done"
		case s.EtaSecs > 0:
			left = fmt.Sprintf(", about %s left", HumanDuration(time.Duration(s.EtaSecs)*time.Second))
		}
		return fmt.Sprintf("%s is %s %s: %s of %s (%d%%)%s%s", who(s.User, s.Remote), verb, s.Name, HumanBytes(s.Bytes), HumanBytes(s.Total), pct, speed, left)
	}
	moved := map[string]string{"upload": "received", "download": "sent"}[s.Dir]
	return fmt.Sprintf("%s is %s %s: %s %s so far%s", who(s.User, s.Remote), verb, s.Name, HumanBytes(s.Bytes), moved, speed)
}
