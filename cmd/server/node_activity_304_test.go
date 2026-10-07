package main

import (
	"encoding/json"
	"path/filepath"
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
	health, err := srv.store.GetNodeHealth(pubkey, now)
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
	if got := s.nodeActivityCoverageStartLocked(now, s.loadCoverageRatio); got.After(start) || got.IsZero() {
		t.Fatalf("complete load coverage=%v", got)
	}
	s.loadCoverageRatio = 0.90
	if got := s.nodeActivityCoverageStartLocked(now, s.loadCoverageRatio); !got.IsZero() {
		t.Fatalf("90%% startup gate is not full coverage, got %v", got)
	}
	s.retentionHours = 0
	s.hotStartupHours = 25
	s.loadCoverageRatio = 0
	if got := s.nodeActivityCoverageStartLocked(now, s.loadCoverageRatio); got.After(start) || got.IsZero() {
		t.Fatalf("unlimited retention with a fully loaded 25h hot window must be complete, got %v", got)
	}
	s.hotStartupHours = 12
	if got := s.nodeActivityCoverageStartLocked(now, s.loadCoverageRatio); !got.IsZero() {
		t.Fatalf("a 12h hot window cannot prove 24h coverage, got %v", got)
	}
	s.hotStartupHours = 0
	s.loadCoverageRatio = 1 // RunStartupLoad sets this after full synchronous loading.
	if got := s.nodeActivityCoverageStartLocked(now, s.loadCoverageRatio); got.After(start) || got.IsZero() {
		t.Fatalf("unlimited retention with no hot window loads full DB, got %v", got)
	}
	s.retentionHours = 168
	s.maxMemoryMB = 100
	s.packets[0].FirstSeen = start.Add(time.Hour).Format(time.RFC3339)
	if got := s.nodeActivityCoverageStartLocked(now, s.loadCoverageRatio); !got.After(start) {
		t.Fatalf("memory cap evicted first hour, coverage=%v", got)
	}
	s.backgroundLoadFailed.Store(true)
	if got := s.nodeActivityCoverageStartLocked(now, s.loadCoverageRatio); !got.IsZero() {
		t.Fatalf("failed background load must be unknown, coverage=%v", got)
	}
	s.backgroundLoadFailed.Store(false)
	s.retentionHours = 12
	if got := s.nodeActivityCoverageStartLocked(now, s.loadCoverageRatio); !got.IsZero() {
		t.Fatalf("12h retention cannot cover 24h, coverage=%v", got)
	}
	s.retentionHours = 168
	s.packets = nil
	if got := s.nodeActivityCoverageStartLocked(now, s.loadCoverageRatio); !got.IsZero() {
		t.Fatalf("empty index cannot prove coverage, coverage=%v", got)
	}
}

// TestNodeActivity24hKnownButShorterCoverageIsIncomplete covers #351 F3 /
// #304 ("distinguish known zero activity from missing/incomplete coverage").
// When coverage is KNOWN but reaches back less than the full 24h window, the
// window must report incomplete — a zero bucket inside the uncovered span is
// "unknown", not a confirmed quiet hour.
//
// Kills mutant M3: replacing `a.Complete = !coverageStart.After(start)` with
// an unconditional `a.Complete = true` whenever coverage is known. That
// mutant survived the entire cmd/server suite before this test (review
// finding 3).
func TestNodeActivity24hKnownButShorterCoverageIsIncomplete(t *testing.T) {
	now := time.Date(2026, 10, 7, 13, 17, 0, 0, time.UTC)
	start := now.Add(-24 * time.Hour)

	// Coverage known, but only 8h deep — short of the 24h window.
	short := newNodeActivity24h(now, now.Add(-8*time.Hour))
	if short.CoverageStart == nil {
		t.Fatalf("coverage start must be reported when it is known: %+v", short)
	}
	if short.Complete {
		t.Fatalf("coverage reaching back only 8h must not mark a 24h window complete: %+v", short)
	}

	// Boundary the mutant would also mask: coverage exactly at the window
	// start IS complete, so the assertion above is testing the comparison,
	// not merely that Complete is ever false.
	if full := newNodeActivity24h(now, start); !full.Complete || full.CoverageStart == nil {
		t.Fatalf("coverage back to the window start must be complete: %+v", full)
	}

	// And one nanosecond past the start is still incomplete (half the point
	// of the strict comparison).
	if justShort := newNodeActivity24h(now, start.Add(time.Nanosecond)); justShort.Complete {
		t.Fatalf("coverage starting after the window start must be incomplete: %+v", justShort)
	}
}

// TestNodeActivity24hAddPreFilterKeepsBoundaryPackets covers #351 F6: add()'s
// cheap string pre-filter trims ONLY the lower bound, so it must never drop an
// in-window packet. With a sub-second `now`, WindowEnd carries a fraction, so a
// whole-second stamp inside the final second sorts lexicographically AFTER
// WindowEnd — a string upper-bound filter would wrongly drop it; the exact time
// comparison must keep it. A stamp exactly at the (fractional) start is kept too.
func TestNodeActivity24hAddPreFilterKeepsBoundaryPackets(t *testing.T) {
	now := time.Date(2026, 10, 7, 13, 17, 0, 500_000_000, time.UTC) // .5s precision
	start := now.Add(-24 * time.Hour)

	final := newNodeActivity24h(now, start)
	// 100ms before now → final (23rd) bucket, whole-second precision.
	final.add(&StoreTx{FirstSeen: now.Add(-100 * time.Millisecond).Truncate(time.Second).Format(time.RFC3339)})
	if final.Buckets[23].Count != 1 {
		t.Fatalf("final-second whole-second packet must land in bucket 23, got %d", final.Buckets[23].Count)
	}

	atStart := newNodeActivity24h(now, start)
	atStart.add(&StoreTx{FirstSeen: start.Format(time.RFC3339Nano)})
	if atStart.Buckets[0].Count != 1 {
		t.Fatalf("packet at the window start must land in bucket 0, got %d", atStart.Buckets[0].Count)
	}

	// A stamp one second before the window start is older than the window and
	// must be dropped (exercises the pre-filter's skip path).
	before := newNodeActivity24h(now, start)
	before.add(&StoreTx{FirstSeen: start.Add(-time.Second).Format(time.RFC3339Nano)})
	var total int
	for _, b := range before.Buckets {
		total += b.Count
	}
	if total != 0 {
		t.Fatalf("pre-window packet must not be counted, got total %d", total)
	}
}

// Test351F1StartupCoverageHonoursRetentionWindow covers #351 F1: the startup
// coverage ratio must be measured against the rows LoadChunked actually
// targeted (the retained window), not a raw COUNT(*) over the whole table.
// With the shipped default (retentionHours>0, hotStartupHours=0) and a DB
// holding history older than the retention window, the old whole-table
// denominator held loadCoverageRatio structurally below 1 forever, so the
// 24h activity gate could never prove completeness and the chart showed
// "Activity unavailable" permanently. Both startup paths are exercised.
func Test351F1StartupCoverageHonoursRetentionWindow(t *testing.T) {
	now := time.Now().UTC()
	windowStart := now.Add(-24 * time.Hour)

	// 30 daily rows: id 1 = today ... id 30 = 29 days ago. Only the first
	// ~8 fall inside a 168h (7-day) retention window; the whole table is 30,
	// so a whole-table ratio would be ~0.27.
	seed := func(dbPath string) {
		seedTestDBRows(t, dbPath, 30, 1, func(i int) (string, int64) {
			ts := now.Add(-time.Duration(i-1) * 24 * time.Hour)
			return ts.Format(time.RFC3339), ts.Unix()
		})
	}

	assertComplete := func(t *testing.T, store *PacketStore) {
		t.Helper()
		if store.loadCoverageRatio < 1 {
			t.Fatalf("loadCoverageRatio=%.4f < 1: coverage measured against the whole table "+
				"instead of the retained window (finding 1)", store.loadCoverageRatio)
		}
		store.mu.RLock()
		cov := store.nodeActivityCoverageStartLocked(now, store.loadCoverageRatio)
		store.mu.RUnlock()
		if cov.IsZero() || cov.After(windowStart) {
			t.Fatalf("24h coverage gate must prove completeness when 7+ days of retained "+
				"history are in memory; got coverageStart=%v (windowStart=%v)", cov, windowStart)
		}
	}

	t.Run("hotStartupHours=0 synchronous full load", func(t *testing.T) {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "test.db")
		seed(dbPath)
		db, err := OpenDB(dbPath)
		if err != nil {
			t.Fatalf("OpenDB: %v", err)
		}
		defer db.conn.Close()
		store := NewPacketStore(db, &PacketStoreConfig{RetentionHours: 168, HotStartupHours: 0})
		if err := store.RunStartupLoad(500); err != nil {
			t.Fatalf("RunStartupLoad: %v", err)
		}
		assertComplete(t, store)
	})

	t.Run("hotStartupHours=24 hot window plus background fill", func(t *testing.T) {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "test.db")
		seed(dbPath)
		db, err := OpenDB(dbPath)
		if err != nil {
			t.Fatalf("OpenDB: %v", err)
		}
		defer db.conn.Close()
		store := NewPacketStore(db, &PacketStoreConfig{RetentionHours: 168, HotStartupHours: 24})
		if err := store.RunStartupLoad(500); err != nil {
			t.Fatalf("RunStartupLoad: %v", err)
		}
		assertComplete(t, store)
	})
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
