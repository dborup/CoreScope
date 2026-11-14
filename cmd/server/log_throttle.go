package main

import (
	"sync"
	"time"
)

// logThrottle lets a log line through at most once per interval: the
// first at once, then at most one per interval. The masts proxy and the
// terrain profiler share it (#753 review N8).
type logThrottle struct {
	mu    sync.Mutex
	at    time.Time
	count int64
}

// note takes a running total and, when a line may go through, returns how
// much the total grew since the last line that did.
func (t *logThrottle) note(now time.Time, total int64, every time.Duration) (int64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.at.IsZero() && now.Sub(t.at) < every {
		return 0, false
	}
	n := total - t.count
	t.at, t.count = now, total
	return n, true
}

// allow is note for a line that reports no count.
func (t *logThrottle) allow(now time.Time, every time.Duration) bool {
	_, ok := t.note(now, 0, every)
	return ok
}
