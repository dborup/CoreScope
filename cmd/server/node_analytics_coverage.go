package main

import "time"

// NodeAnalyticsCoverage describes available retained store data, not radio
// uptime. The loader's contiguous time window is clamped to persisted
// observation evidence; an old re-heard transmission's FirstSeen is never
// evidence that the intervening history was loaded.
type NodeAnalyticsCoverage struct {
	From    string  `json:"from"`
	To      string  `json:"to"`
	Seconds float64 `json:"seconds"`
	Partial bool    `json:"partial"`
	Reason  string  `json:"reason"`
	Loading bool    `json:"loading"`
	// LateEvictedObservations counts observations (since startup) of evicted
	// transmissions heard more than lateEvictedObservationThreshold (1 h) after
	// the transmission's FirstSeen; ordinary second-observer hearings within
	// that hour are not counted. They are a known, bounded gap in retained
	// evidence and never move From.
	LateEvictedObservations int `json:"late_evicted_observations"`
}

// All fields are guarded by PacketStore.mu. This is constant-sized and is
// updated only on load/eviction, never by a per-request SQL scan.
type nodeAnalyticsHistory struct {
	from         time.Time
	known        bool
	loading      bool
	failed       bool
	cap          bool
	reason       string
	loadCutoff   time.Time
	evictedFrom  time.Time // newest evicted FirstSeen (+1 ns) since the load began
	observedFrom time.Time
	lateEvicted  int
}

// lateEvictedObservationThreshold is how long after an evicted transmission's
// FirstSeen an observation must be heard to count as "late". Routine
// second-observer hearings arrive within seconds to minutes and are not a gap.
const lateEvictedObservationThreshold = time.Hour

// parseCanonicalUTCSeconds is the hot-path parser for exactly
// "YYYY-MM-DDTHH:MM:SSZ", the form the store writes. It rejects anything it
// is not certain about (including out-of-range fields) so the caller can fall
// back to time.Parse with identical results.
func parseCanonicalUTCSeconds(raw string) (time.Time, bool) {
	if len(raw) != 20 || raw[4] != '-' || raw[7] != '-' || raw[10] != 'T' ||
		raw[13] != ':' || raw[16] != ':' || raw[19] != 'Z' {
		return time.Time{}, false
	}
	num := func(i, n int) (int, bool) {
		v := 0
		for ; n > 0; n, i = n-1, i+1 {
			c := raw[i]
			if c < '0' || c > '9' {
				return 0, false
			}
			v = v*10 + int(c-'0')
		}
		return v, true
	}
	y, ok1 := num(0, 4)
	mo, ok2 := num(5, 2)
	d, ok3 := num(8, 2)
	h, ok4 := num(11, 2)
	mi, ok5 := num(14, 2)
	sec, ok6 := num(17, 2)
	if !(ok1 && ok2 && ok3 && ok4 && ok5 && ok6) || mo < 1 || mo > 12 || d < 1 ||
		h > 23 || mi > 59 || sec > 59 {
		return time.Time{}, false
	}
	t := time.Date(y, time.Month(mo), d, h, mi, sec, 0, time.UTC)
	if t.Day() != d { // day overflowed the month (e.g. Feb 30)
		return time.Time{}, false
	}
	return t, true
}

func parseNodeAnalyticsTime(raw string) (time.Time, bool) {
	if t, ok := parseCanonicalUTCSeconds(raw); ok {
		return t, t.Unix() > 0
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t, err = time.ParseInLocation("2006-01-02 15:04:05", raw, time.UTC)
	}
	return t.UTC(), err == nil && t.Unix() > 0
}

func (s *PacketStore) beginNodeAnalyticsLoadLocked(cutoff string) {
	floor, _ := parseNodeAnalyticsTime(cutoff)
	s.analyticsHistory = nodeAnalyticsHistory{loading: true, loadCutoff: floor}
}

func earlierNodeAnalyticsEvidence(earliest time.Time, raw string) time.Time {
	if at, ok := parseNodeAnalyticsTime(raw); ok && (earliest.IsZero() || at.Before(earliest)) {
		return at
	}
	return earliest
}

// finishNodeAnalyticsLoadLocked certifies only the interval actually read.
// Earliest persisted observation is a conservative evidence edge, not proof
// that the recorder was continuously running or that RF reception succeeded.
// A capped/error load cannot certify its historical subset; only subsequent
// live FirstSeen traffic is eligible, from completion onward.
func (s *PacketStore) finishNodeAnalyticsLoadLocked(cutoff time.Time, capped, loading bool) {
	h := &s.analyticsHistory
	earliest := h.observedFrom
	h.loading = loading
	h.cap = h.cap || capped
	if earliest.IsZero() {
		h.known = false
		h.from = time.Now().UTC()
		h.reason = "unknown_history"
		return
	}
	floor := earliest
	if cutoff.After(floor) {
		floor = cutoff
	}
	if h.cap || h.failed {
		floor = time.Now().UTC()
		h.reason = "load_capped"
		if h.failed {
			h.reason = "load_failed"
		}
	} else {
		h.reason = "available_history"
	}
	// Eviction may have removed evidence while background chunks loaded.
	// Never move a previously invalidated edge backward during completion.
	if h.evictedFrom.After(floor) {
		floor = h.evictedFrom
		h.reason = "memory_eviction"
	}
	h.from, h.known = floor.UTC(), true
}

func (s *PacketStore) nodeAnalyticsCoverageLocked(days int, now time.Time) NodeAnalyticsCoverage {
	now = now.UTC()
	requested := now.Add(-time.Duration(days) * 24 * time.Hour)
	h := s.analyticsHistory
	from := requested
	if s.retentionHours > 0 {
		if floor := now.Add(-time.Duration(s.retentionHours * float64(time.Hour))); floor.After(from) {
			from = floor
		}
	}
	reason := h.reason
	if !h.known {
		from = now
		reason = "unknown_history"
	} else if h.from.After(from) {
		from = h.from
	}
	if from.After(now) {
		from = now
	}
	loading := !h.failed && (h.loading || !s.loaded)
	if loading {
		reason = "loading"
	}
	if h.failed {
		reason = "load_failed"
	}
	partial := loading || h.failed || !h.known || from.After(requested)
	if !partial {
		reason = "complete"
	} else if reason == "" || reason == "available_history" {
		reason = "available_history"
	}
	return NodeAnalyticsCoverage{From: from.Format(time.RFC3339Nano), To: now.Format(time.RFC3339Nano), Seconds: now.Sub(from).Seconds(), Partial: partial, Reason: reason, Loading: loading, LateEvictedObservations: h.lateEvicted}
}

// invalidateNodeAnalyticsEvictionLocked advances the coverage edge to the
// eviction boundary: the evicted transmission's FirstSeen (+1 ns to exclude
// ties). Analytics membership is by FirstSeen, so nothing at or before that
// instant can still be complete. Late observations of an evicted, re-heard
// transmission (up to ~2 days after FirstSeen in production) are a known,
// bounded gap that is only counted, never allowed to move the edge: routine
// minute-by-minute eviction would otherwise make coverage jump forward.
// An unparseable FirstSeen cannot bound anything and stays conservative.
func (s *PacketStore) invalidateNodeAnalyticsEvictionLocked(removed []*StoreTx) {
	h := &s.analyticsHistory
	for _, tx := range removed {
		edge, ok := parseNodeAnalyticsTime(tx.FirstSeen)
		if !ok {
			edge = time.Now().UTC()
		} else {
			lateAfter := edge.Add(lateEvictedObservationThreshold)
			edge = edge.Add(time.Nanosecond)
			for _, obs := range tx.Observations {
				if at, ok := parseNodeAnalyticsTime(obs.Timestamp); ok && at.After(lateAfter) {
					h.lateEvicted++
				}
			}
		}
		if edge.After(h.from) {
			h.from = edge
		}
		// #509: only the eviction boundary itself. Folding in h.from, the
		// provisional hot-window floor while the background fill runs, made
		// one retention eviction of an old re-heard row shrink the final
		// coverage to the hot window.
		if edge.After(h.evictedFrom) {
			h.evictedFrom = edge
		}
	}
	if len(removed) > 0 {
		h.reason = "memory_eviction"
	}
}

// An initially empty database gains a conservative edge at its first live
// observation, without inventing any history before the receiver evidence.
func (s *PacketStore) observeNodeAnalyticsLiveLocked(timestamp string) {
	h := &s.analyticsHistory
	// A live row may arrive between an empty hot load and background
	// completion. Keep its evidence even though the interval cannot be
	// certified until the loader finishes. Steady-state known history pays
	// no extra parse/reduction on this hot path.
	if h.loading || !h.known {
		h.observedFrom = earlierNodeAnalyticsEvidence(h.observedFrom, timestamp)
	}
	if h.known || h.loading || !s.loaded {
		return
	}
	if at, ok := parseNodeAnalyticsTime(timestamp); ok {
		if h.from.After(at) {
			at = h.from
		}
		if h.evictedFrom.After(at) {
			at = h.evictedFrom
		}
		h.from, h.known, h.reason = at, true, "available_history"
	}
}
