package main

import (
	"fmt"
	"testing"
	"time"
)

func filledWSHistory() (*wsClientHistory, time.Time) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	h := newWSClientHistory(wsHistoryCapacity)
	for i := 0; i < wsHistoryCapacity; i++ {
		h.sample(i%50, now.Add(time.Duration(i-wsHistoryCapacity+1)*time.Minute))
	}
	return h, now
}

func TestWSClientHistory_FullMonthResponseIsAggregated(t *testing.T) {
	h, now := filledWSHistory()
	for _, tc := range []struct {
		name string
		max  int
	}{{"24h", 1441}, {"7d", 2017}, {"30d", 2881}} {
		r, _ := wsHistoryRange(tc.name)
		got := h.snapshotRange(now, r)
		if len(got) == 0 || len(got) > tc.max {
			t.Fatalf("%s: %d points, want 1..%d", tc.name, len(got), tc.max)
		}
		if got[len(got)-1].Clients != (wsHistoryCapacity-1)%50 {
			t.Fatalf("%s: latest count changed", tc.name)
		}
	}
}

func BenchmarkWSClientHistoryFullMonth(b *testing.B) {
	h, now := filledWSHistory()
	for _, name := range []string{"24h", "7d", "30d"} {
		r, _ := wsHistoryRange(name)
		points := len(h.snapshotRange(now, r))
		b.Run(fmt.Sprintf("%s_%dpoints", name, points), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = h.snapshotRange(now, r)
			}
		})
	}
}
