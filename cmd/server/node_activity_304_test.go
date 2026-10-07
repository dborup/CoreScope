package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNodeActivity24hRollingBoundariesAndUniqueTransmissions(t *testing.T) {
	now := time.Date(2026, 10, 7, 13, 17, 0, 0, time.UTC)
	start := now.Add(-24 * time.Hour)
	packets := make([]*StoreTx, 0, 30)
	for i := 0; i < 25; i++ {
		packets = append(packets, &StoreTx{ID: i + 1, FirstSeen: start.Add(time.Duration(i) * time.Hour).Format(time.RFC3339), ObservationCount: 3})
	}
	packets = append(packets,
		&StoreTx{ID: 26, FirstSeen: start.Add(-time.Second).Format(time.RFC3339)},
		&StoreTx{ID: 27, FirstSeen: now.Format(time.RFC3339)},
		&StoreTx{ID: 28, FirstSeen: "bad timestamp"},
	)
	got := newNodeActivity24h(now, start.Add(-time.Hour))
	for _, tx := range packets {
		got.add(tx)
	}
	if len(got.Buckets) != 24 {
		t.Fatalf("buckets=%d, want 24", len(got.Buckets))
	}
	if got.WindowStart != start.Format(time.RFC3339) || got.WindowEnd != now.Format(time.RFC3339) {
		t.Fatalf("window=%s..%s", got.WindowStart, got.WindowEnd)
	}
	for i, b := range got.Buckets {
		if b.Count != 1 {
			t.Errorf("bucket %d count=%d, want one transmission despite three observations", i, b.Count)
		}
	}
}

func TestNodeActivity24hIncompleteWhenCoverageUnknown(t *testing.T) {
	now := time.Date(2026, 10, 7, 13, 17, 0, 0, time.UTC)
	got := newNodeActivity24h(now, time.Time{})
	if got.Complete || got.CoverageStart != nil {
		t.Fatalf("empty history must not claim complete coverage: %+v", got)
	}
	for _, b := range got.Buckets {
		if b.Count != 0 {
			t.Fatalf("empty bucket count=%d", b.Count)
		}
	}
}

func TestNodeHealthActivity24hUsesFullByNodeIndex(t *testing.T) {
	srv, _ := setupTestServer(t)
	const pubkey = "aabbccdd11223344"
	now := time.Date(2026, 10, 7, 13, 17, 0, 0, time.UTC)
	start := now.Add(-24 * time.Hour)
	packets := make([]*StoreTx, 0, 27)
	for i := 0; i < 26; i++ {
		packets = append(packets, &StoreTx{ID: i + 1, FirstSeen: start.Add(time.Duration(i%24) * time.Hour).Format(time.RFC3339), ObservationCount: 4})
	}
	packets = append(packets, &StoreTx{ID: 27, FirstSeen: "malformed"})
	srv.store.mu.Lock()
	srv.store.byNode[pubkey] = packets
	srv.store.loaded = true
	srv.store.loadCoverageRatio = 1
	srv.store.retentionHours = 168
	srv.store.oldestLoaded = start.Add(-time.Hour).Format(time.RFC3339)
	srv.store.packets = []*StoreTx{{ID: 100, FirstSeen: srv.store.oldestLoaded}}
	srv.store.backgroundLoadDone.Store(true)
	srv.store.mu.Unlock()
	health, err := srv.store.getNodeHealthAt(pubkey, now)
	if err != nil {
		t.Fatal(err)
	}
	activity, ok := health["activity24h"].(NodeActivity24h)
	if !ok || !activity.Complete || activity.CoverageStart == nil {
		t.Fatalf("activity coverage: %#v", health["activity24h"])
	}
	if activity.Buckets[0].Count != 2 || activity.Buckets[23].Count != 1 {
		t.Fatalf("buckets[0]=%d [23]=%d", activity.Buckets[0].Count, activity.Buckets[23].Count)
	}
	if len(health["recentPackets"].([]map[string]interface{})) != 20 {
		t.Fatal("recentPackets compatibility limit changed")
	}
	encoded, err := json.Marshal(health)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Activity24h struct {
			Buckets  []NodeActivityHour `json:"buckets"`
			Complete bool               `json:"complete"`
		} `json:"activity24h"`
	}
	if err := json.Unmarshal(encoded, &body); err != nil || !body.Activity24h.Complete || len(body.Activity24h.Buckets) != 24 {
		t.Fatalf("JSON contract invalid: %v %s", err, encoded)
	}
}

func TestNodeActivity24hCoverageRequiresLoadedHistory(t *testing.T) {
	now := time.Date(2026, 10, 7, 13, 17, 0, 0, time.UTC)
	start := now.Add(-24 * time.Hour)
	s := &PacketStore{loaded: true, loadCoverageRatio: 1, oldestLoaded: start.Add(-time.Hour).Format(time.RFC3339), retentionHours: 168,
		packets: []*StoreTx{{FirstSeen: start.Add(-time.Hour).Format(time.RFC3339)}}}
	s.backgroundLoadDone.Store(true)
	if got := s.nodeActivityCoverageStartLocked(now); got.After(start) || got.IsZero() {
		t.Fatalf("complete load coverage=%v", got)
	}
	s.loadCoverageRatio = 0.90
	if got := s.nodeActivityCoverageStartLocked(now); !got.IsZero() {
		t.Fatalf("90%% startup gate is not full coverage, got %v", got)
	}
	s.loadCoverageRatio = 1
	s.maxMemoryMB = 100
	s.packets[0].FirstSeen = start.Add(time.Hour).Format(time.RFC3339)
	if got := s.nodeActivityCoverageStartLocked(now); !got.After(start) {
		t.Fatalf("memory cap evicted first hour, coverage=%v", got)
	}
	s.backgroundLoadFailed.Store(true)
	if got := s.nodeActivityCoverageStartLocked(now); !got.IsZero() {
		t.Fatalf("failed background load must be unknown, coverage=%v", got)
	}
	s.backgroundLoadFailed.Store(false)
	s.retentionHours = 12
	if got := s.nodeActivityCoverageStartLocked(now); !got.IsZero() {
		t.Fatalf("12h retention cannot cover 24h, coverage=%v", got)
	}
	s.retentionHours = 168
	s.packets = nil
	if got := s.nodeActivityCoverageStartLocked(now); !got.IsZero() {
		t.Fatalf("empty index cannot prove coverage, coverage=%v", got)
	}
}

func BenchmarkNodeActivity24h30K(b *testing.B) {
	now := time.Date(2026, 10, 7, 13, 17, 0, 0, time.UTC)
	packets := make([]*StoreTx, 30000)
	for i := range packets {
		packets[i] = &StoreTx{ID: i + 1, FirstSeen: now.Add(-time.Duration(i%1440) * time.Minute).Format(time.RFC3339)}
	}
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		a := newNodeActivity24h(now, now.Add(-25*time.Hour))
		for _, tx := range packets {
			a.add(tx)
		}
		if a.Buckets[23].Count == 0 {
			b.Fatal("unexpected empty final hour")
		}
	}
}
