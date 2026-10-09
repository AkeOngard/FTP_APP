package core

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// poolConn is a connection that only counts how it is used.
type poolConn struct {
	RemoteClient // unused methods panic on the nil interface: the pool must never call them
	id           int
	closed       *atomic.Int32
}

func (c *poolConn) Close() error { c.closed.Add(1); return nil }

// countingDialer returns a Dialer plus counters for dials and closes.
func countingDialer(failAfter int) (Dialer, *atomic.Int32, *atomic.Int32) {
	var dials, closed atomic.Int32
	return func(ctx context.Context) (RemoteClient, error) {
		n := dials.Add(1)
		if failAfter > 0 && int(n) > failAfter {
			dials.Add(-1) // a refused connection was never made
			return nil, errors.New("421 too many connections")
		}
		return &poolConn{id: int(n), closed: &closed}, nil
	}, &dials, &closed
}

func TestPoolRespectsMaxAndReusesConnections(t *testing.T) {
	dial, dials, _ := countingDialer(0)
	p := NewConnPool(dial, 3)
	defer p.Close()

	var inUse, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := p.Get(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			n := inUse.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			inUse.Add(-1)
			p.Put(c, false)
		}()
	}
	wg.Wait()
	if peak.Load() > 3 {
		t.Errorf("%d connections were in use at once, the pool allows 3", peak.Load())
	}
	if dials.Load() > 3 {
		t.Errorf("dialled %d connections for 30 uses, want at most 3 (they must be reused)", dials.Load())
	}
}

func TestPoolReplacesBrokenConnections(t *testing.T) {
	dial, dials, closed := countingDialer(0)
	p := NewConnPool(dial, 1)
	defer p.Close()

	c, _ := p.Get(context.Background())
	p.Put(c, true)
	if closed.Load() != 1 {
		t.Errorf("a broken connection must be closed, closed = %d", closed.Load())
	}
	c2, err := p.Get(context.Background())
	if err != nil || c2 == c || dials.Load() != 2 {
		t.Errorf("a fresh connection should replace the broken one: err=%v same=%v dials=%d", err, c2 == c, dials.Load())
	}
}

func TestPoolServesWaitersInOrder(t *testing.T) {
	dial, _, _ := countingDialer(0)
	p := NewConnPool(dial, 1)
	defer p.Close()

	held, _ := p.Get(context.Background())
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	for i := 1; i <= 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := p.Get(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			p.Put(c, false)
		}()
		time.Sleep(30 * time.Millisecond) // make sure waiter i queues before waiter i+1
	}
	p.Put(held, false)
	wg.Wait()
	for i, v := range order {
		if v != i+1 {
			t.Fatalf("waiters were served in order %v, want 1 2 3 4", order)
		}
	}
}

func TestPoolGetHonoursCancellation(t *testing.T) {
	dial, _, _ := countingDialer(0)
	p := NewConnPool(dial, 1)
	defer p.Close()
	held, _ := p.Get(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := p.Get(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Get on a busy pool with a deadline returned %v", err)
	}
	// The cancelled waiter must not take the connection with it.
	p.Put(held, false)
	c, err := p.Get(context.Background())
	if err != nil {
		t.Fatalf("pool unusable after a cancelled Get: %v", err)
	}
	p.Put(c, false)
}

func TestPoolNeverLeaksSlotsUnderCancellationStress(t *testing.T) {
	dial, _, closed := countingDialer(0)
	p := NewConnPool(dial, 3)

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i%7)*time.Millisecond)
			defer cancel()
			c, err := p.Get(ctx)
			if err != nil {
				return
			}
			time.Sleep(time.Duration(i%3) * time.Millisecond)
			p.Put(c, i%11 == 0)
		}()
	}
	wg.Wait()

	p.mu.Lock()
	open, idle, waiters := p.open, len(p.idle), len(p.waiters)
	p.mu.Unlock()
	if open != idle || waiters != 0 || open > 3 {
		t.Errorf("after everything was returned: open=%d idle=%d waiters=%d; want open == idle <= 3 and no waiters", open, idle, waiters)
	}
	p.Close()
	if closed.Load() == 0 {
		t.Error("Close must close the idle connections")
	}
}

func TestPoolLearnsAServerConnectionLimit(t *testing.T) {
	dial, _, _ := countingDialer(2) // the server only allows two connections
	p := NewConnPool(dial, 4)
	defer p.Close()

	a, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		c, err := p.Get(context.Background()) // the third dial is refused: this must wait, not fail
		if err == nil {
			p.Put(c, false)
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("a refused extra connection must make Get wait for a free one, returned %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	p.Put(a, false)
	if err := <-got; err != nil {
		t.Fatalf("Get failed after a connection was freed: %v", err)
	}
	p.Put(b, false)
	if p.Max() != 2 {
		t.Errorf("Max = %d, want 2 (the limit the server enforced)", p.Max())
	}
}

func TestPoolReportsDialFailureWhenNothingIsOpen(t *testing.T) {
	p := NewConnPool(func(context.Context) (RemoteClient, error) { return nil, errors.New("connection refused") }, 4)
	defer p.Close()
	for i := 0; i < 3; i++ {
		if _, err := p.Get(context.Background()); err == nil || err.Error() != "connection refused" {
			t.Fatalf("Get = %v, want the dial error", err)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.open != 0 || p.max != 4 {
		t.Errorf("failed dials changed the pool: open=%d max=%d", p.open, p.max)
	}
}

func TestPoolClosesIdleConnectionsAfterTheTTL(t *testing.T) {
	dial, _, closed := countingDialer(0)
	p := NewConnPool(dial, 2)
	p.ttl = 40 * time.Millisecond
	defer p.Close()

	c, _ := p.Get(context.Background())
	p.Put(c, false)
	deadline := time.Now().Add(2 * time.Second)
	for closed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if closed.Load() != 1 {
		t.Fatal("an unused connection was never closed")
	}
	// and the pool still works afterwards
	c2, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p.Put(c2, false)
}

func TestPoolClose(t *testing.T) {
	dial, _, closed := countingDialer(0)
	p := NewConnPool(dial, 2)
	out, _ := p.Get(context.Background())
	idle, _ := p.Get(context.Background())
	p.Put(idle, false)

	waiting := make(chan error, 1)
	go func() {
		a, _ := p.Get(context.Background()) // takes the idle one
		_, err := p.Get(context.Background())
		p.Put(a, false)
		waiting <- err
	}()
	time.Sleep(50 * time.Millisecond)
	p.Close()

	if err := <-waiting; !errors.Is(err, ErrPoolClosed) {
		t.Errorf("a waiter must be released with ErrPoolClosed, got %v", err)
	}
	if _, err := p.Get(context.Background()); !errors.Is(err, ErrPoolClosed) {
		t.Errorf("Get after Close = %v, want ErrPoolClosed", err)
	}
	p.Put(out, false) // handed out before Close: closed on return
	if closed.Load() != 2 {
		t.Errorf("both connections should be closed, closed = %d", closed.Load())
	}
	p.Close() // a second Close is harmless
}

func TestFixedPoolLeavesTheCallersConnectionAlone(t *testing.T) {
	var closed atomic.Int32
	c := &poolConn{closed: &closed}
	p := NewFixedPool(c)

	got, err := p.Get(context.Background())
	if err != nil || got != RemoteClient(c) {
		t.Fatalf("Get = %v, %v; want the wrapped connection", got, err)
	}
	p.Put(got, true) // even an error must not close a connection the caller owns
	again, err := p.Get(context.Background())
	if err != nil || again != RemoteClient(c) {
		t.Fatalf("the connection was lost after an error: %v %v", again, err)
	}
	p.Put(again, false)
	p.Close()
	if closed.Load() != 0 {
		t.Error("closing a fixed pool must not close the caller's connection")
	}
	if p.Max() != 1 {
		t.Errorf("Max = %d, want 1", p.Max())
	}
}
