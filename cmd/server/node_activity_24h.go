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
// provide positive evidence of continuous in-memory coverage. A memory cap
// can evict the oldest rows after oldestLoaded was set, so use the current
// first row as the stricter lower bound when it is enabled.
//
// #351 F4: loadCoverage is read by the caller (GetNodeHealth) BEFORE it
// takes s.mu and passed in here, so this function never acquires bgErrMu
// while holding s.mu. bgErrMu exists precisely so its readers need not
// synchronise on s.mu (store.go:530); nesting s.mu → bgErrMu would be the
// first such site in the codebase and would quietly undermine that contract.
func (s *PacketStore) nodeActivityCoverageStartLocked(now time.Time, loadCoverage float64) time.Time {
	if !s.loaded || !s.backgroundLoadDone.Load() || s.backgroundLoadFailed.Load() ||
		(s.retentionHours > 0 && s.retentionHours < 24) || len(s.packets) == 0 {
		return time.Time{}
	}
	// The startup "done" gate accepts 90% of DB rows. That is not enough
	// to call every empty hour a confirmed zero; require the exact count.
	// Unlimited retention with a hot-start window has no background fill and
	// leaves loadCoverageRatio at zero. The window itself can still prove 24h
	// coverage when it is at least 24h wide; oldestLoaded is checked below.
	if loadCoverage < 1 && !(s.retentionHours <= 0 && s.hotStartupHours >= 24) {
		return time.Time{}
	}
	coverage, err := time.Parse(time.RFC3339Nano, s.oldestLoaded)
	if err != nil {
		return time.Time{}
	}
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
