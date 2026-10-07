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
}

func newNodeActivity24h(now, coverageStart time.Time) NodeActivity24h {
	end := now.UTC()
	start := end.Add(-24 * time.Hour)
	a := NodeActivity24h{
		WindowStart: start.Format(time.RFC3339Nano),
		WindowEnd:   end.Format(time.RFC3339Nano),
		start:       start,
		end:         end,
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
func (s *PacketStore) nodeActivityCoverageStartLocked(now time.Time) time.Time {
	if !s.loaded || !s.backgroundLoadDone.Load() || s.backgroundLoadFailed.Load() ||
		(s.retentionHours > 0 && s.retentionHours < 24) || len(s.packets) == 0 {
		return time.Time{}
	}
	// The startup "done" gate accepts 90% of DB rows. That is not enough
	// to call every empty hour a confirmed zero; require the exact count.
	s.bgErrMu.RLock()
	loadCoverage := s.loadCoverageRatio
	s.bgErrMu.RUnlock()
	if loadCoverage < 1 {
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
