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
	evictedFrom  time.Time
	observedFrom time.Time
}

func parseNodeAnalyticsTime(raw string) (time.Time, bool) {
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
	return NodeAnalyticsCoverage{From: from.Format(time.RFC3339Nano), To: now.Format(time.RFC3339Nano), Seconds: now.Sub(from).Seconds(), Partial: partial, Reason: reason, Loading: loading}
}

// invalidateNodeAnalyticsEvictionLocked is deliberately based on removed
// observation evidence (including re-heard old rows), never on the earliest
// surviving FirstSeen. Tied timestamps are excluded by advancing one ns.
func (s *PacketStore) invalidateNodeAnalyticsEvictionLocked(removed []*StoreTx) {
	h := &s.analyticsHistory
	for _, tx := range removed {
		valid := false
		for _, obs := range tx.Observations {
			if at, ok := parseNodeAnalyticsTime(obs.Timestamp); ok {
				valid = true
				if !at.Before(h.from) {
					h.from = at.Add(time.Nanosecond)
				}
			}
		}
		if !valid {
			h.from = time.Now().UTC()
		}
		if h.from.After(h.evictedFrom) {
			h.evictedFrom = h.from
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
