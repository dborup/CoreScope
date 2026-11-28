package main

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Admission control for the opt-in /api/perf?mem=1 store memory breakdown
// (#371).
//
// GetStoreMemoryBreakdown walks every transmission and observation under the
// packet store's read lock, and /api/perf is public. Without a gate, N
// overlapping or rapidly repeated mem=1 requests run N full scans and keep the
// read lock held long enough to stall ingest and eviction, which need the
// write lock.
//
// The gate admits one scan per process at a time plus a short cooldown after
// it completes; everything else is shed with 429 + Retry-After in the same
// shape as the Reach cold limiter (#341). Unlike Reach it does not queue: the
// breakdown is an operator diagnostic, not a page a user waits on, so a burst
// should be refused outright rather than served in turn. The check runs
// before handlePerf touches the store, so a rejected request never takes the
// store lock. Ordinary /api/perf and mem=1 without a store are not gated.
//
// State is per Server, which is per process in production; nothing here is
// shared across instances.

// perfMemScanCooldown is how long after a completed scan the next one is
// refused. Long enough that a tight polling loop costs one scan per period,
// short enough that an operator re-running it by hand barely notices.
const perfMemScanCooldown = 5 * time.Second

// perfMemLimiter admits at most one store memory scan at a time and refuses
// new ones for a cooldown after each completes. Usable as a zero value.
type perfMemLimiter struct {
	mu       sync.Mutex
	active   bool
	until    time.Time        // end of the cooldown after the last scan
	cooldown time.Duration    // <=0 → perfMemScanCooldown
	now      func() time.Time // nil → time.Now
	hook     func()           // test injection, run just after admission

	acquired atomic.Uint64 // scans admitted
	rejected atomic.Uint64 // requests shed with 429
}

// configure replaces the cooldown, clock and admission hook. Test setup only:
// call it before any request can acquire. Production uses the zero value.
func (l *perfMemLimiter) configure(cooldown time.Duration, now func() time.Time, hook func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cooldown, l.now, l.hook = cooldown, now, hook
}

// settings returns the effective cooldown and clock. Caller holds l.mu.
func (l *perfMemLimiter) settings() (time.Duration, func() time.Time) {
	cd, now := l.cooldown, l.now
	if cd <= 0 {
		cd = perfMemScanCooldown
	}
	if now == nil {
		now = time.Now
	}
	return cd, now
}

// acquire admits one scan. On success it returns a release func that ends the
// scan and starts the cooldown; it is single-use, so calling it again (e.g.
// from a defer after an explicit release) is a no-op. On refusal it returns
// how long the caller should wait: the remaining cooldown, or the full
// cooldown while a scan is still running (its end is unknown, and no new
// scan can start sooner than that after it).
func (l *perfMemLimiter) acquire() (release func(), wait time.Duration, ok bool) {
	l.mu.Lock()
	cd, now := l.settings()
	if l.active {
		l.mu.Unlock()
		l.rejected.Add(1)
		return nil, cd, false
	}
	if left := l.until.Sub(now()); left > 0 {
		l.mu.Unlock()
		l.rejected.Add(1)
		return nil, left, false
	}
	l.active = true
	hook := l.hook
	l.mu.Unlock()
	l.acquired.Add(1)

	var once sync.Once
	release = func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			cd, now := l.settings()
			l.active = false
			l.until = now().Add(cd)
		})
	}
	if hook != nil {
		hook()
	}
	return release, 0, true
}

func (l *perfMemLimiter) acquiredCount() uint64 { return l.acquired.Load() }
func (l *perfMemLimiter) rejectedCount() uint64 { return l.rejected.Load() }

// writePerfMemBusy429 sheds a mem=1 request the limiter refused. Retry-After
// is the wait rounded up to whole seconds (always >= 1: acquire never refuses
// with a non-positive wait).
func writePerfMemBusy429(w http.ResponseWriter, wait time.Duration) {
	setRetryAfter(w, wait)
	ra := w.Header().Get("Retry-After")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(`{"error":"store memory breakdown is busy or cooling down","retryAfter":` + ra + `}`))
}
