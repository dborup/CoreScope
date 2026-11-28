package anomaly

import "time"

// New-stream volume.
//
// An episode of a key starts at its first observed event, or at the first
// event after QuietPeriod of silence (reactivation). The rule counts events
// causally in [start, start+Window) and fires once, at the event that brings
// the count to MinCount.
//
// Left censoring: a key first seen within CensorWindow of the history start
// may have been active before the data begins, so it is reported with
// Censored=true and low confidence, never as a certain new stream. This is
// what keeps a restart or a replay from calling old streams new.
//
// Limitation: a key evicted by StateTTL (idle longer than StateTTL >=
// QuietPeriod) and seen again later is indistinguishable from a new key; it
// is reported as new, not reactivated. Capacity evictions are flagged on
// candidates via CoverageKeyEvictions.
type newStreamState struct {
	start, lastSeen      int64
	count, exp, mon      uint32
	begun                bool
	censored, reactivate bool
	fired                bool
}

func (s *newStreamState) observe(t int64, cls TrafficClass, history int64, r *NewStreamRule) (*NewStreamEvidence, TrafficLabel, bool) {
	switch {
	case !s.begun:
		*s = newStreamState{begun: true, start: t, censored: t-history < int64(r.CensorWindow)}
	case t-s.lastSeen >= int64(r.QuietPeriod):
		*s = newStreamState{begun: true, start: t, reactivate: true}
	}
	s.lastSeen = t
	if t-s.start >= int64(r.Window) {
		return nil, 0, false
	}
	s.count++
	switch cls {
	case TrafficExpected:
		s.exp++
	case TrafficMonitored:
		s.mon++
	}
	if s.fired || s.count < uint32(r.MinCount) {
		return nil, 0, false
	}
	s.fired = true
	ev := &NewStreamEvidence{
		Window:      r.Window,
		MinCount:    r.MinCount,
		Count:       int(s.count),
		Age:         time.Duration(t - s.start),
		Censored:    s.censored,
		Reactivated: s.reactivate,
	}
	return ev, labelFromCounts(uint64(s.count), uint64(s.exp), uint64(s.mon)), true
}
