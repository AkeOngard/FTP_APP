package core

import (
	"errors"
	"net"
	"sync"
	"time"
)

// Limits that keep a client on the network from guessing passwords or exhausting the app. They are
// generous for real use (a client with 16 parallel transfers stays far below them).
const (
	maxLoginFailures = 8                // wrong passwords from one address within failureWindow ...
	failureWindow    = 10 * time.Minute // ...
	lockoutTime      = 5 * time.Minute  // ... block that address for this long
	maxConnsPerAddr  = 64               // simultaneous connections from one address
	maxConnsTotal    = 256              // simultaneous connections to the FTP and SFTP servers together
	maxTrackedAddrs  = 4096             // bound on remembered addresses
)

var (
	errLockedOut    = errors.New("too many failed logins from this address; try again later")
	errTooManyConns = errors.New("too many connections")
)

// loginGuard is shared by the FTP and SFTP servers, which share the same users: failures on one count
// on the other. All methods are safe on a nil guard, which allows everything.
type loginGuard struct {
	now func() time.Time

	mu      sync.Mutex
	fails   map[string]*failures
	conns   map[string]int
	total   int
	onBlock func(addr string, d time.Duration) // called once when an address gets blocked
}

type failures struct {
	recent  []time.Time // failures inside the window
	blocked time.Time   // blocked until then; zero when not blocked
}

func newLoginGuard() *loginGuard {
	return &loginGuard{now: time.Now, fails: map[string]*failures{}, conns: map[string]int{}}
}

// addrKey reduces a network address to the host that connections are counted by.
func addrKey(a net.Addr) string {
	if a == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String()
	}
	return host
}

// Blocked reports whether logins from addr are refused right now.
func (g *loginGuard) Blocked(addr net.Addr) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.blockedLocked(addrKey(addr))
}

func (g *loginGuard) blockedLocked(key string) bool {
	f := g.fails[key]
	return f != nil && g.now().Before(f.blocked)
}

// Failed records a wrong password and reports whether the address is now blocked.
func (g *loginGuard) Failed(addr net.Addr) bool {
	if g == nil {
		return false
	}
	key := addrKey(addr)
	g.mu.Lock()
	now := g.now()
	f := g.fails[key]
	if f == nil {
		if len(g.fails) >= maxTrackedAddrs {
			g.pruneLocked(now)
		}
		f = &failures{}
		g.fails[key] = f
	}
	keep := f.recent[:0]
	for _, t := range f.recent {
		if now.Sub(t) < failureWindow {
			keep = append(keep, t)
		}
	}
	f.recent = append(keep, now)
	newlyBlocked := false
	if len(f.recent) >= maxLoginFailures && !now.Before(f.blocked) {
		f.blocked, f.recent, newlyBlocked = now.Add(lockoutTime), nil, true
	}
	blocked, notify := now.Before(f.blocked), g.onBlock
	g.mu.Unlock()
	if newlyBlocked && notify != nil {
		notify(key, lockoutTime)
	}
	return blocked
}

// pruneLocked forgets addresses with nothing left to remember; if every one is still live, it forgets
// the lot rather than grow without bound (an attacker with that many addresses is not slowed down by
// a per-address limit anyway).
func (g *loginGuard) pruneLocked(now time.Time) {
	for k, f := range g.fails {
		live := now.Before(f.blocked)
		for _, t := range f.recent {
			live = live || now.Sub(t) < failureWindow
		}
		if !live {
			delete(g.fails, k)
		}
	}
	if len(g.fails) >= maxTrackedAddrs {
		g.fails = map[string]*failures{}
	}
}

// Connect admits a new connection or refuses it (address blocked, or too many connections). The caller
// must call release exactly once when an admitted connection ends.
func (g *loginGuard) Connect(addr net.Addr) (release func(), err error) {
	if g == nil {
		return func() {}, nil
	}
	key := addrKey(addr)
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case g.blockedLocked(key):
		return nil, errLockedOut
	case g.total >= maxConnsTotal || g.conns[key] >= maxConnsPerAddr:
		return nil, errTooManyConns
	}
	g.total++
	g.conns[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.total--
			if g.conns[key]--; g.conns[key] <= 0 {
				delete(g.conns, key)
			}
			g.mu.Unlock()
		})
	}, nil
}
