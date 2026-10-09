package core

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Dialer opens a new, independent connection to a server.
type Dialer func(ctx context.Context) (RemoteClient, error)

// ErrPoolClosed is returned by Get after the pool has been closed.
var ErrPoolClosed = errors.New("connection pool is closed")

// defaultIdleTTL is how long an unused connection is kept open before it is closed.
const defaultIdleTTL = 30 * time.Second

// ConnPool hands out up to Max connections to one server and reuses them, so transferring many files
// does not pay for a new TCP connection and login each time. It also caps the number of simultaneous
// transfers: when every connection is in use, Get waits, and waiters are served in the order they asked.
type ConnPool struct {
	dial  Dialer
	max   int
	ttl   time.Duration
	fixed bool // wraps one caller-owned connection: never closed, never replaced

	mu      sync.Mutex
	open    int // connections that exist: idle, handed out, or being dialled
	idle    []idleConn
	waiters []chan poolGrant
	closed  bool
	reaper  *time.Timer
}

type idleConn struct {
	c     RemoteClient
	since time.Time
}

// poolGrant answers a waiter: a ready connection, or permission to dial one (c == nil, err == nil).
type poolGrant struct {
	c   RemoteClient
	err error
}

// NewConnPool returns a pool that dials at most max connections (at least 1).
func NewConnPool(dial Dialer, max int) *ConnPool {
	if max < 1 {
		max = 1
	}
	return &ConnPool{dial: dial, max: max, ttl: defaultIdleTTL}
}

// NewFixedPool wraps one existing connection as a pool of one. Transfers through it run one at a time on
// that connection, which stays open and owned by the caller.
func NewFixedPool(c RemoteClient) *ConnPool {
	return &ConnPool{max: 1, open: 1, fixed: true, idle: []idleConn{{c: c}}}
}

// Max is the largest number of connections, and so of simultaneous transfers. It can shrink: when a
// server refuses an extra connection while others are open, the pool learns that limit and stops asking.
func (p *ConnPool) Max() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.max
}

// Get returns an idle connection, dials a new one while fewer than Max exist, or waits for one to be
// returned. Give it back with Put.
func (p *ConnPool) Get(ctx context.Context) (RemoteClient, error) {
	for {
		c, retry, err := p.get(ctx)
		if !retry {
			return c, err
		}
	}
}

// get makes one attempt; retry is true when a dial failed but other connections exist, in which case the
// limit has been lowered and the caller should wait for one of those instead.
func (p *ConnPool) get(ctx context.Context) (c RemoteClient, retry bool, err error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, false, ErrPoolClosed
	}
	if c := p.popIdleLocked(); c != nil {
		p.mu.Unlock()
		return c, false, nil
	}
	if p.open < p.max && p.dial != nil {
		p.open++
		p.mu.Unlock()
		return p.dialOne(ctx)
	}
	w := make(chan poolGrant, 1)
	p.waiters = append(p.waiters, w)
	p.mu.Unlock()

	select {
	case g := <-w:
		return p.finishGrant(ctx, g)
	case <-ctx.Done():
		p.mu.Lock()
		removed := p.removeWaiterLocked(w)
		p.mu.Unlock()
		if !removed {
			// Granted at the same moment: do not lose the connection, pass it on.
			if g := <-w; g.c != nil {
				p.Put(g.c, false)
			} else if g.err == nil {
				p.releaseSlot()
			}
		}
		return nil, false, ctx.Err()
	}
}

func (p *ConnPool) finishGrant(ctx context.Context, g poolGrant) (RemoteClient, bool, error) {
	if g.err != nil || g.c != nil {
		return g.c, false, g.err
	}
	return p.dialOne(ctx) // the slot was already counted when it was granted
}

// dialOne dials into a slot the caller has already counted in p.open.
func (p *ConnPool) dialOne(ctx context.Context) (RemoteClient, bool, error) {
	c, err := p.dial(ctx)
	if err == nil {
		return c, false, nil
	}
	p.mu.Lock()
	p.open--
	// Servers often cap the connections per user. If others are already open, treat this failure as that
	// cap and keep working with what we have instead of failing the transfer.
	retry := p.open > 0 && ctx.Err() == nil
	if retry {
		p.max = p.open
	}
	p.serveWaiterLocked()
	p.mu.Unlock()
	if retry {
		return nil, true, nil
	}
	return nil, false, err
}

// Put returns a connection. Pass broken=true after an error that may have left it unusable: it is then
// closed and a later Get dials a replacement.
func (p *ConnPool) Put(c RemoteClient, broken bool) {
	p.mu.Lock()
	if p.fixed {
		p.idle = append(p.idle, idleConn{c: c})
		p.serveWaiterLocked()
		p.mu.Unlock()
		return
	}
	if broken || p.closed {
		p.open--
		p.serveWaiterLocked()
		p.mu.Unlock()
		c.Close()
		return
	}
	p.idle = append(p.idle, idleConn{c: c, since: time.Now()})
	p.serveWaiterLocked()
	p.scheduleReapLocked()
	p.mu.Unlock()
}

// releaseSlot gives back a slot that was counted but never produced a connection.
func (p *ConnPool) releaseSlot() {
	p.mu.Lock()
	p.open--
	p.serveWaiterLocked()
	p.mu.Unlock()
}

func (p *ConnPool) popIdleLocked() RemoteClient {
	n := len(p.idle)
	if n == 0 {
		return nil
	}
	ic := p.idle[n-1] // most recently used: least likely to have timed out on the server
	p.idle = p.idle[:n-1]
	return ic.c
}

// serveWaiterLocked hands the first waiter an idle connection, or a slot to dial in.
func (p *ConnPool) serveWaiterLocked() {
	if len(p.waiters) == 0 {
		return
	}
	switch {
	case p.closed:
		for _, w := range p.waiters {
			w <- poolGrant{err: ErrPoolClosed}
		}
		p.waiters = nil
		return
	case len(p.idle) > 0:
		w := p.waiters[0]
		p.waiters = p.waiters[1:]
		w <- poolGrant{c: p.popIdleLocked()}
	case p.open < p.max && p.dial != nil:
		w := p.waiters[0]
		p.waiters = p.waiters[1:]
		p.open++
		w <- poolGrant{}
	}
}

func (p *ConnPool) removeWaiterLocked(w chan poolGrant) bool {
	for i, x := range p.waiters {
		if x == w {
			p.waiters = append(p.waiters[:i], p.waiters[i+1:]...)
			return true
		}
	}
	return false
}

func (p *ConnPool) scheduleReapLocked() {
	if p.ttl <= 0 || p.reaper != nil {
		return
	}
	p.reaper = time.AfterFunc(p.ttl, p.reap)
}

// reap closes connections that have sat unused for the idle time, so a finished transfer does not
// keep several logins open on the server.
func (p *ConnPool) reap() {
	p.mu.Lock()
	p.reaper = nil
	now := time.Now()
	keep := p.idle[:0:0]
	var stale []RemoteClient
	for _, ic := range p.idle {
		if now.Sub(ic.since) >= p.ttl {
			stale = append(stale, ic.c)
			p.open--
		} else {
			keep = append(keep, ic)
		}
	}
	p.idle = keep
	if len(p.idle) > 0 {
		p.scheduleReapLocked()
	}
	p.serveWaiterLocked()
	p.mu.Unlock()
	for _, c := range stale {
		c.Close()
	}
}

// Close closes the idle connections and makes Get fail. Connections still handed out are closed when
// they are returned. The connection of a fixed pool belongs to the caller and is left open.
func (p *ConnPool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	if p.reaper != nil {
		p.reaper.Stop()
		p.reaper = nil
	}
	var toClose []RemoteClient
	if !p.fixed {
		for _, ic := range p.idle {
			toClose = append(toClose, ic.c)
			p.open--
		}
		p.idle = nil
	}
	p.serveWaiterLocked()
	p.mu.Unlock()
	for _, c := range toClose {
		c.Close()
	}
}
