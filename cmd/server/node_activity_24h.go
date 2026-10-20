package main

import "time"

// NodeActivityHour is one half-open hour of transmission activity.
type NodeActivityHour struct {
	Start string `json:"start"`
	End   string `json:"end"`
	Count int    `json:"count"`
}

// NodeActivity24h describes the last 24 rolling hours. Complete is false
// unless the in-memory index is known to cover the whole window.
type NodeActivity24h struct {
	WindowStart   string               `json:"windowStart"`
	WindowEnd     string               `json:"windowEnd"`
	Buckets       [24]NodeActivityHour `json:"buckets"`
	CoverageStart *string              `json:"coverageStart"`
	Complete      bool                 `json:"complete"`
	start         time.Time
	end           time.Time
	// startSecond is start truncated to whole seconds in the fixed-width
	// "2006-01-02T15:04:05" form; add() uses it as a string pre-filter.
	startSecond string
}

// nodeActivitySecondLayout is fixed width: no fraction, no zone suffix.
const nodeActivitySecondLayout = "2006-01-02T15:04:05"

func newNodeActivity24h(now, coverageStart time.Time) NodeActivity24h {
	end := now.UTC()
	start := end.Add(-24 * time.Hour)
	a := NodeActivity24h{
		WindowStart: start.Format(time.RFC3339Nano),
		WindowEnd:   end.Format(time.RFC3339Nano),
		start:       start,
		end:         end,
		startSecond: start.Format(nodeActivitySecondLayout),
	}
	for i := range a.Buckets {
		bucketStart := start.Add(time.Duration(i) * time.Hour)
		a.Buckets[i] = NodeActivityHour{
			Start: bucketStart.Format(time.RFC3339Nano),
			End:   bucketStart.Add(time.Hour).Format(time.RFC3339Nano),
		}
	}
	if !coverageStart.IsZero() {
		coverageStart = coverageStart.UTC()
		iso := coverageStart.Format(time.RFC3339Nano)
		a.CoverageStart = &iso
		a.Complete = !coverageStart.After(start)
	}
	return a
}

func (a *NodeActivity24h) add(tx *StoreTx) {
	if tx == nil {
		return
	}
	// #351 F6/R3: cheap pre-filter before the RFC3339Nano parse, for the
	// common case of a node whose in-memory history spans well past 24h.
	// It compares against startSecond, the window start truncated to whole
	// seconds, NOT WindowStart: RFC3339Nano trims trailing zeros, so the
	// fraction widths of WindowStart and a stamp differ ("…:00Z" vs
	// "…:00.100Z") and a full-string compare can sort an in-window stamp
	// below the bound. The second prefix is fixed width, so a UTC stamp at
	// or after the window start always sorts >= startSecond whatever its
	// fraction. A stamp in the start's own second survives and the exact
	// bounds check below decides it. The upper bound is deliberately NOT
	// string-filtered, for the same fraction-width reason.
	if tx.FirstSeen < a.startSecond {
		return
	}
	when, err := time.Parse(time.RFC3339Nano, tx.FirstSeen)
	if err != nil || when.Before(a.start) || !when.Before(a.end) {
		return
	}
	idx := int(when.Sub(a.start) / time.Hour)
	a.Buckets[idx].Count++
}

// nodeActivityCoverageStartLocked returns zero unless load and retention
// provide positive evidence of continuous in-memory coverage.
func (s *PacketStore) nodeActivityCoverageStartLocked(now time.Time) time.Time {
	// Preserve the existing 24h health contract while other views can expose
	// a shorter known interval as partial history: unlimited retention with
	// a hot window shorter than 24h has no background fill to reach 24h.
	if (s.retentionHours > 0 && s.retentionHours < 24) ||
		(s.retentionHours <= 0 && s.hotStartupHours > 0 && s.hotStartupHours < 24) {
		return time.Time{}
	}
	return s.retainedHistoryCoverageStartLocked(now)
}

// retainedHistoryCoverageStartLocked describes the known in-memory history,
// independent of the requested display window.
func (s *PacketStore) retainedHistoryCoverageStartLocked(now time.Time) time.Time {
	if !s.loaded || !s.backgroundLoadDone.Load() || s.backgroundLoadFailed.Load() ||
		len(s.packets) == 0 {
		return time.Time{}
	}
	// The startup "done" gate accepts 90% of DB rows. That is not enough
	// to call every empty hour a confirmed zero: startup must have proven
	// the history complete (verifyRetainedHistory, #509). That proof also
	// covers a capped load, which can omit newer rows while still retaining
	// a much older first row.
	coverage := s.retainedHistoryFrom
	if coverage.IsZero() {
		return time.Time{}
	}
	// A memory cap can evict the oldest rows after startup, so use the
	// current first row as the stricter lower bound when it is enabled.
	if s.maxMemoryMB > 0 {
		first, err := time.Parse(time.RFC3339Nano, s.packets[0].FirstSeen)
		if err != nil {
			return time.Time{}
		}
		if first.After(coverage) {
			coverage = first
		}
	}
	if s.retentionHours > 0 {
		floor := now.UTC().Add(-time.Duration(s.retentionHours * float64(time.Hour)))
		if floor.After(coverage) {
			coverage = floor
		}
	}
	return coverage
}
