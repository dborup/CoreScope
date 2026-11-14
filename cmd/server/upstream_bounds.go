package main

// Shared plumbing of the endpoints that query a third-party upstream on a
// visitor's behalf: the terrain profile (#681) and the heritage query
// (#723 M2). Each resolves its own config block with configKnobs and
// bounds its distinct upstream fetches server-wide with an admission.

import (
	"sync"
	"sync/atomic"
	"time"
)

// configKnobs reads optional numeric values of one config block. A nil
// value takes the default; an out-of-range one logs a warning that names
// the key (never the value) and takes the default too.
type configKnobs struct {
	tag   string // log prefix, e.g. "[dhm]"
	block string // config path, e.g. "map.dhm.profile"
	logf  func(string, ...interface{})
}

// num accepts lo..hi, excluding lo when loOpen. NaN is out of range.
func (k configKnobs) num(key string, v *float64, def, lo, hi float64, loOpen bool) float64 {
	if v == nil {
		return def
	}
	ok := *v <= hi && (*v > lo || !loOpen && *v == lo) // NaN fails both
	if !ok {
		k.logf("%s config %s.%s is out of range; using %v", k.tag, k.block, key, def)
		return def
	}
	return *v
}

// integer accepts lo..hi inclusive.
func (k configKnobs) integer(key string, v *int, def, lo, hi int) int {
	if v == nil {
		return def
	}
	if *v < lo || *v > hi {
		k.logf("%s config %s.%s is out of range; using %d", k.tag, k.block, key, def)
		return def
	}
	return *v
}

// admission bounds the distinct upstream fetches running at once,
// server-wide (#696). A fetch takes a slot without waiting or is refused,
// so nothing queues behind a full gate, and refusals are counted for a
// throttled report. The leader of a dhm.Flight takes the slot, so callers
// that join a running fetch are free.
type admission struct {
	admit    chan struct{} // one token per fetch in flight
	refused  atomic.Int64  // refusals since start
	logMu    sync.Mutex    // guards the throttle below
	logAt    time.Time     // last refusal report
	logCount int64         // refusals already reported
}

func newAdmission(slots int) admission {
	return admission{admit: make(chan struct{}, slots)}
}

// tryAdmit takes a slot if one is free; release gives it back.
func (a *admission) tryAdmit() bool {
	select {
	case a.admit <- struct{}{}:
		return true
	default:
		return false
	}
}

func (a *admission) release() { <-a.admit }

// refusal counts one refusal and returns how many to report now: all
// those since the last report, at most once per every (the first at
// once); 0 while throttled.
func (a *admission) refusal(now time.Time, every time.Duration) int64 {
	total := a.refused.Add(1)
	a.logMu.Lock()
	defer a.logMu.Unlock()
	if !a.logAt.IsZero() && now.Sub(a.logAt) < every {
		return 0
	}
	n := total - a.logCount
	a.logAt, a.logCount = now, total
	return n
}
