package dhm

import (
	"context"
	"sync"
	"time"
)

// Flight coalesces concurrent calls with the same key into one, like
// singleflight, but with cancellation (#683 review N3). The shared call
// runs on its own context, which is cancelled once every caller has
// given up (its ctx is done) or after timeout (0: none). A caller that
// gives up returns its ctx.Err() at once; the others keep waiting.
//
// singleflight cannot do this: its call runs on whichever context the
// first caller captured, so that caller's cancel would fail the others,
// and nothing would stop the call once they had all left.
type Flight[V any] struct {
	mu    sync.Mutex
	calls map[string]*flightCall[V]
}

type flightCall[V any] struct {
	done    chan struct{}
	val     V
	err     error
	waiters int
	cancel  context.CancelFunc
}

// Do runs fn once per key at a time and returns its result to every
// caller; shared is true for callers that joined a running call.
func (g *Flight[V]) Do(ctx context.Context, key string, timeout time.Duration, fn func(context.Context) (V, error)) (v V, shared bool, err error) {
	return g.DoAdmitted(ctx, key, timeout, nil, fn)
}

// DoAdmitted is Do with a bound on new calls (#744): a call that would
// start a worker takes an adm slot first, and when none is free it
// returns ErrAdmissionFull at once, with no worker. The slot is held until
// the worker has returned, so adm bounds live workers, queued or running.
// Joining a running call takes no slot. A nil adm admits everything.
func (g *Flight[V]) DoAdmitted(ctx context.Context, key string, timeout time.Duration, adm *Admission, fn func(context.Context) (V, error)) (v V, shared bool, err error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[string]*flightCall[V])
	}
	c, shared := g.calls[key]
	if shared {
		c.waiters++
	} else {
		if !adm.tryAcquire() {
			g.mu.Unlock()
			var zero V
			return zero, false, ErrAdmissionFull
		}
		work, cancel := context.WithCancel(context.Background())
		if timeout > 0 {
			work, cancel = context.WithTimeout(context.Background(), timeout)
		}
		c = &flightCall[V]{done: make(chan struct{}), waiters: 1, cancel: cancel}
		g.calls[key] = c
		go func() {
			defer adm.release()
			c.val, c.err = fn(work)
			g.forget(key, c)
			cancel()
			close(c.done)
		}()
	}
	g.mu.Unlock()

	select {
	case <-c.done:
		return c.val, shared, c.err
	case <-ctx.Done():
		g.mu.Lock()
		c.waiters--
		if c.waiters == 0 {
			// Nobody wants the result any more: stop the work, and let
			// the next caller start afresh rather than join a dying call.
			c.cancel()
			if g.calls[key] == c {
				delete(g.calls, key)
			}
		}
		g.mu.Unlock()
		var zero V
		return zero, shared, ctx.Err()
	}
}

func (g *Flight[V]) forget(key string, c *flightCall[V]) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.calls[key] == c {
		delete(g.calls, key)
	}
}

// waiters is the number of callers waiting on key's running call.
func (g *Flight[V]) waiters(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.calls[key]; ok {
		return c.waiters
	}
	return 0
}

// Pending is the number of callers waiting on any running call.
func (g *Flight[V]) Pending() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.calls {
		n += c.waiters
	}
	return n
}
