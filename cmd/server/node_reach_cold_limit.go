package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Bounded concurrency for *cold* Reach reports (#341).
//
// /api/nodes/{pk}/reach already collapses a thundering herd on one cache key
// with singleflight, but distinct node/window keys are independent: N cold
// keys arriving together run N windowed observation scans at once. The
// server's read pool is 4 connections (OpenDB), so a handful of concurrent
// cold Reach scans can hold every connection and leave /api/nodes and
// /api/packets queueing behind them.
//
// Upstream (fork PR 2127) shields the pool with a *non-blocking* two-build
// semaphore: the third concurrent build is refused immediately with HTTP 429 +
// Retry-After. Evaluated against our deployment:
//
//   - The cap of 2 fits our pool as is: two cold scans leave at least two of
//     the four read connections for every other endpoint, which is what the
//     issue asks for. We keep it.
//   - Refusing instantly does not. Our Reach page is one request per view and
//     a cold report normally completes in well under a second, so a plain
//     two-tab burst would 429 a user whose database is not under any real
//     pressure. Our own front end also only auto-retries 503 + Retry-After
//     (app.js api()), never 429, so an instant refusal surfaces as a failed
//     page rather than a short wait.
//   - We therefore queue for a bounded reachColdBuildWait first and only
//     answer 429 + Retry-After when the limiter is still saturated at the end
//     of it. Queueing costs an idle HTTP goroutine, not a database
//     connection, so the pool protection is identical while a transient burst
//     is absorbed instead of shed. Load shedding still happens — it just
//     requires sustained saturation rather than an instant of it.
//
// This is a per-endpoint cap, not global database admission control: it bounds
// Reach's own cold work and nothing else. Any broader policy belongs with the
// pool-pressure evaluation in #221.
//
// Vars (not consts) so tests can tighten the cap and the queue wait without
// standing up dozens of slow scans — same pattern as reachScanRowLimit.
var (
	// reachColdBuildLimit is how many cold Reach reports may compute at once.
	reachColdBuildLimit = 2
	// reachColdBuildWait is how long a cold request queues for a slot before
	// it is shed with 429. Sized above a typical cold scan (≈1s on a
	// 400k-observation fixture) so a handful of simultaneous cold pages are
	// served in turn rather than refused, while sustained saturation still
	// sheds promptly. Queue time costs a parked goroutine, not a database
	// connection.
	reachColdBuildWait = 3 * time.Second
)

// reachColdRetryAfterSeconds is the Retry-After advertised on a shed cold
// Reach request. Deliberately short: the limiter frees up as soon as one of
// the in-flight scans finishes.
const reachColdRetryAfterSeconds = 2

// errReachColdBusy is returned by reachColdLimiter.acquire when the limiter
// stayed saturated for the whole queue wait. The handler maps it to 429 and
// never caches it (singleflight does not cache errors either), so the next
// request recomputes normally.
var errReachColdBusy = errors.New("reach cold-scan concurrency limit reached")

// reachColdLimiter is a counting semaphore with a bounded queue wait, plus the
// counters the tests and the perf report read. Usable as a zero value: the
// slot channel is built on first use from reachColdBuildLimit, so every
// Server (including the ones tests assemble by hand) gets the limit without a
// constructor.
type reachColdLimiter struct {
	mu   sync.Mutex
	sem  chan struct{} // buffered to the limit; one slot per in-flight scan
	lim  int           // 0 → reachColdBuildLimit
	wait time.Duration // <=0 → reachColdBuildWait
	hook func()        // test injection, run just after a slot is taken

	inflight atomic.Int64  // slots currently held
	peak     atomic.Int64  // high-water mark of inflight
	acquired atomic.Uint64 // slots handed out
	rejected atomic.Uint64 // requests shed with errReachColdBusy
	canceled atomic.Uint64 // waiters whose context ended while queued
}

// configure replaces the limiter's capacity, queue wait and acquire hook.
// Call it before any request can acquire (test setup): in-flight holders
// release into the channel this installs, so swapping it mid-flight loses
// their slots. limit <= 0 and wait <= 0 fall back to the package defaults;
// hook is nil in production.
func (l *reachColdLimiter) configure(limit int, wait time.Duration, hook func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lim, l.wait, l.hook, l.sem = limit, wait, hook, nil
}

// slots returns the (lazily built) slot channel, the queue wait and the
// acquire hook.
func (l *reachColdLimiter) slots() (chan struct{}, time.Duration, func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sem == nil {
		n := l.lim
		if n <= 0 {
			n = reachColdBuildLimit
		}
		if n <= 0 {
			n = 1
		}
		l.sem = make(chan struct{}, n)
	}
	w := l.wait
	if w <= 0 {
		w = reachColdBuildWait
	}
	return l.sem, w, l.hook
}

// acquire takes a slot for one cold Reach build. It returns nil when a slot
// was taken (the caller MUST release it), ctx.Err() when the request went
// away while queued, or errReachColdBusy when the limiter stayed saturated
// for the whole queue wait.
func (l *reachColdLimiter) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sem, wait, hook := l.slots()
	// Fast path: a free slot is taken without arming a timer.
	select {
	case sem <- struct{}{}:
		l.took(hook)
		return nil
	default:
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case sem <- struct{}{}:
		l.took(hook)
		return nil
	case <-ctx.Done():
		l.canceled.Add(1)
		return ctx.Err()
	case <-t.C:
		l.rejected.Add(1)
		return errReachColdBusy
	}
}

// took records a handed-out slot and runs the test hook (if any) outside the
// limiter's own mutex.
func (l *reachColdLimiter) took(hook func()) {
	l.acquired.Add(1)
	n := l.inflight.Add(1)
	for {
		p := l.peak.Load()
		if n <= p || l.peak.CompareAndSwap(p, n) {
			break
		}
	}
	if hook != nil {
		hook()
	}
}

// release hands a slot back. Must be called exactly once per successful
// acquire — on success, on error and on cancellation alike (the handler
// defers it). Never blocks, so an unbalanced release cannot wedge a request.
func (l *reachColdLimiter) release() {
	l.inflight.Add(-1)
	sem, _, _ := l.slots()
	select {
	case <-sem:
	default:
	}
}

func (l *reachColdLimiter) peakInflight() int64   { return l.peak.Load() }
func (l *reachColdLimiter) acquiredCount() uint64 { return l.acquired.Load() }
func (l *reachColdLimiter) rejectedCount() uint64 { return l.rejected.Load() }
func (l *reachColdLimiter) canceledCount() uint64 { return l.canceled.Load() }

// writeReachColdBusy429 sheds a cold Reach request that could not get a slot.
// 429 (not 503) because the server is healthy and the work is rate-limited;
// Retry-After tells a well-behaved client how long to back off.
func writeReachColdBusy429(w http.ResponseWriter) {
	ra := strconv.Itoa(reachColdRetryAfterSeconds)
	w.Header().Set("Retry-After", ra)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(`{"error":"reach is busy computing other reports","retryAfter":` + ra + `}`))
}
