package main

import "time"

// NodeActivityHour is one half-open hour of transmission activity.
type NodeActivityHour struct {
	Start string `json:"start"`
	End   string `json:"end"`
	Count int    `json:"count"`
}

// NodeActivityPrevious24h is the 24 hours immediately before the rolling
// window of NodeActivity24h. It exists only as a like-for-like baseline for
// that window, so it carries no hourly detail. Complete is false unless the
// in-memory index is known to cover the whole EARLIER window: an unknown
// history must never be read as a quiet period, and a Count without Complete
// is a lower bound, not a baseline (#304).
type NodeActivityPrevious24h struct {
	WindowStart string `json:"windowStart"`
	WindowEnd   string `json:"windowEnd"`
	Count       int    `json:"count"`
	Complete    bool   `json:"complete"`
}

// NodeActivity24h describes the last 24 rolling hours. Complete is false
// unless the in-memory index is known to cover the whole window.
type NodeActivity24h struct {
	WindowStart   string                  `json:"windowStart"`
	WindowEnd     string                  `json:"windowEnd"`
	Buckets       [24]NodeActivityHour    `json:"buckets"`
	CoverageStart *string                 `json:"coverageStart"`
	Complete      bool                    `json:"complete"`
	Previous      NodeActivityPrevious24h `json:"previous"`
	start         time.Time
	end           time.Time
	// prevStart is the start of Previous's window (start - 24h).
	prevStart time.Time
	// startSecond and prevStartSecond are start / prevStart truncated to
	// whole seconds in the fixed-width "2006-01-02T15:04:05" form; add()
	// uses prevStartSecond as its string pre-filter.
	startSecond     string
	prevStartSecond string
}

// nodeActivitySecondLayout is fixed width: no fraction, no zone suffix.
const nodeActivitySecondLayout = "2006-01-02T15:04:05"

func newNodeActivity24h(now, coverageStart time.Time) NodeActivity24h {
	end := now.UTC()
	start := end.Add(-24 * time.Hour)
	prevStart := start.Add(-24 * time.Hour)
	a := NodeActivity24h{
		WindowStart: start.Format(time.RFC3339Nano),
		WindowEnd:   end.Format(time.RFC3339Nano),
		Previous: NodeActivityPrevious24h{
			WindowStart: prevStart.Format(time.RFC3339Nano),
			WindowEnd:   start.Format(time.RFC3339Nano),
		},
		start:           start,
		end:             end,
		prevStart:       prevStart,
		startSecond:     start.Format(nodeActivitySecondLayout),
		prevStartSecond: prevStart.Format(nodeActivitySecondLayout),
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
		a.Previous.Complete = !coverageStart.After(prevStart)
	}
	return a
}

func (a *NodeActivity24h) add(tx *StoreTx) {
	if tx == nil {
		return
	}
	// #351 F6/R3: cheap pre-filter before the RFC3339Nano parse, for the
	// common case of a node whose in-memory history spans well past the
	// window. It compares against prevStartSecond, the EARLIEST bound
	// (prevStart) truncated to whole seconds, and never against WindowStart:
	// RFC3339Nano trims trailing zeros, so the fraction widths of a bound and
	// a stamp differ ("…:00Z" vs "…:00.100Z") and a full-string compare can
	// sort an in-window stamp below the bound. The second prefix is fixed
	// width, so a UTC stamp at or after the bound always sorts >=
	// prevStartSecond whatever its fraction. A stamp inside the bound's own
	// second survives and the exact bounds checks below decide it. The upper
	// bound is deliberately NOT string-filtered, for the same reason.
	// #304: the filter admits the previous window too, because one pass fills
	// both. That costs the parse for transmissions 24-48h old, the same order
	// as the per-transmission work GetNodeHealth already does for every row
	// of the index.
	if tx.FirstSeen < a.prevStartSecond {
		return
	}
	when, err := time.Parse(time.RFC3339Nano, tx.FirstSeen)
	if err != nil || when.Before(a.prevStart) || !when.Before(a.end) {
		return
	}
	if when.Before(a.start) {
		a.Previous.Count++
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
