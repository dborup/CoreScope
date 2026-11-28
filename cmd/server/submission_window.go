package main

import (
	"sync"
	"time"
)

// submissionWindow is a global sliding one-hour window of accepted public
// submissions, shared by channel proposals and antenna claims (#789 M2a). A
// per-client limit would be unreliable behind a reverse proxy (ws_limits.go).
// Bounded: it never holds more than the limit's timestamps.
type submissionWindow struct {
	rateMu sync.Mutex
	recent []time.Time // accepted submissions in the last hour, oldest first
}

// allow records a submission at now if fewer than limit were accepted in the
// last hour, or reports how long until one may be.
func (w *submissionWindow) allow(now time.Time, limit int) (bool, time.Duration) {
	w.rateMu.Lock()
	defer w.rateMu.Unlock()
	cutoff := now.Add(-time.Hour)
	i := 0
	for i < len(w.recent) && !w.recent[i].After(cutoff) {
		i++
	}
	w.recent = w.recent[i:]
	if len(w.recent) >= limit {
		return false, w.recent[0].Add(time.Hour).Sub(now)
	}
	w.recent = append(w.recent, now)
	return true, 0
}

// refund gives back the budget of a submission that was not queued.
func (w *submissionWindow) refund() {
	w.rateMu.Lock()
	defer w.rateMu.Unlock()
	if n := len(w.recent); n > 0 {
		w.recent = w.recent[:n-1]
	}
}
