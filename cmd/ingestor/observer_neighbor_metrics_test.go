package main

import (
	"testing"
	"time"
)

// #1865 follow-up: dborup spotted that the raw /neighbors payload also
// carries snr/heard_secs_ago per neighbor, previously dropped entirely.
// observer_neighbor_metrics is an APPEND-ONLY history (unlike
// observer_neighbors' current-only snapshot), inspired by the existing
// RF Health tab's observer_metrics pattern.

func floatPtr(f float64) *float64 { return &f }
func intPtr(i int) *int           { return &i }

func countObserverNeighborMetrics(t *testing.T, store *Store, observerID, pubkey string) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM observer_neighbor_metrics WHERE observer_id = ? AND neighbor_pubkey = ?`,
		observerID, pubkey).Scan(&n); err != nil {
		t.Fatalf("count observer_neighbor_metrics: %v", err)
	}
	return n
}

func TestRecordObserverNeighborMetrics_Basic(t *testing.T) {
	store := openNeighborsStore(t)
	seedObserverForNeighbors(t, store, "obs-metrics-1")

	entries := []ObserverNeighborEntry{
		{Pubkey: "aaaa000000000000000000000000000000000000000000000000000000000001", Status: "responded", SNR: floatPtr(10.5), HeardSecsAgo: intPtr(75)},
		{Pubkey: "bbbb000000000000000000000000000000000000000000000000000000000002", Status: "timeout", SNR: floatPtr(-8.75), HeardSecsAgo: intPtr(77)},
	}
	if err := store.RecordObserverNeighborMetrics("obs-metrics-1", entries, "2026-07-26T12:00:00Z"); err != nil {
		t.Fatal(err)
	}

	var snr float64
	var heardSecsAgo int
	if err := store.db.QueryRow(`SELECT snr, heard_secs_ago FROM observer_neighbor_metrics WHERE observer_id = ? AND neighbor_pubkey = ? AND timestamp = ?`,
		"obs-metrics-1", "aaaa000000000000000000000000000000000000000000000000000000000001", "2026-07-26T12:00:00Z").Scan(&snr, &heardSecsAgo); err != nil {
		t.Fatalf("select: %v", err)
	}
	if snr != 10.5 || heardSecsAgo != 75 {
		t.Errorf("snr=%v heardSecsAgo=%v, want 10.5/75", snr, heardSecsAgo)
	}
	// Timeout entries still carry snr -- must be recorded too, not just
	// scope-query "responded" entries.
	if n := countObserverNeighborMetrics(t, store, "obs-metrics-1", "bbbb000000000000000000000000000000000000000000000000000000000002"); n != 1 {
		t.Errorf("expected 1 row for the timeout neighbor's SNR reading, got %d", n)
	}
}

func TestRecordObserverNeighborMetrics_AccumulatesAcrossReports(t *testing.T) {
	store := openNeighborsStore(t)
	seedObserverForNeighbors(t, store, "obs-metrics-2")
	pk := "cccc000000000000000000000000000000000000000000000000000000000003"

	if err := store.RecordObserverNeighborMetrics("obs-metrics-2", []ObserverNeighborEntry{
		{Pubkey: pk, Status: "responded", SNR: floatPtr(5)},
	}, "2026-07-26T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordObserverNeighborMetrics("obs-metrics-2", []ObserverNeighborEntry{
		{Pubkey: pk, Status: "responded", SNR: floatPtr(6)},
	}, "2026-07-26T13:00:00Z"); err != nil {
		t.Fatal(err)
	}
	// Unlike ReplaceObserverNeighbors, this is a time-series -- both rows
	// must survive, not just the latest.
	if n := countObserverNeighborMetrics(t, store, "obs-metrics-2", pk); n != 2 {
		t.Fatalf("expected 2 accumulated history rows, got %d", n)
	}
}

func TestRecordObserverNeighborMetrics_OutOfOrderStillRecorded(t *testing.T) {
	store := openNeighborsStore(t)
	seedObserverForNeighbors(t, store, "obs-metrics-3")
	pk := "dddd000000000000000000000000000000000000000000000000000000000004"

	// Newer report first, then an older out-of-order one -- unlike
	// ReplaceObserverNeighbors' snapshot guard, BOTH are valid history at
	// their own timestamp and must both be recorded.
	if err := store.RecordObserverNeighborMetrics("obs-metrics-3", []ObserverNeighborEntry{
		{Pubkey: pk, Status: "responded", SNR: floatPtr(9)},
	}, "2026-07-26T14:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordObserverNeighborMetrics("obs-metrics-3", []ObserverNeighborEntry{
		{Pubkey: pk, Status: "responded", SNR: floatPtr(3)},
	}, "2026-07-26T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if n := countObserverNeighborMetrics(t, store, "obs-metrics-3", pk); n != 2 {
		t.Errorf("expected both the newer and the out-of-order older reading recorded, got %d rows", n)
	}
}

func TestRecordObserverNeighborMetrics_SkipsEntriesWithoutSNR(t *testing.T) {
	store := openNeighborsStore(t)
	seedObserverForNeighbors(t, store, "obs-metrics-4")
	pk := "eeee000000000000000000000000000000000000000000000000000000000005"

	if err := store.RecordObserverNeighborMetrics("obs-metrics-4", []ObserverNeighborEntry{
		{Pubkey: pk, Status: "timeout", SNR: nil},
	}, "2026-07-26T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if n := countObserverNeighborMetrics(t, store, "obs-metrics-4", pk); n != 0 {
		t.Errorf("expected no row when SNR is nil, got %d", n)
	}
}

// TestPruneOldNeighborMetrics pins the retention boundary of
// PruneOldNeighborMetrics(30) against a fixed reference instant (via the
// pruneNeighborMetricsNow hook) instead of a hardcoded calendar date, so the
// assertions hold regardless of what today's real date is (the original
// fixture -- "recent" pinned to 2026-07-26 -- aged past the 30-day window
// and started failing once run after 2026-08-25).
//
// The production query is `WHERE timestamp < cutoff` (db.go), a strict
// less-than, so a row exactly at the cutoff (age == retentionDays) is
// RETAINED, not pruned. That exact-boundary row can only be asserted
// deterministically if the test's "now" and PruneOldNeighborMetrics' own
// cutoff computation read the identical instant -- two independent
// time.Now() calls a few instructions apart would occasionally straddle a
// second (the granularity RFC3339 storage truncates to) and flip the
// boundary row's fate under real timing. Pinning both to the same fixed
// instant via pruneNeighborMetricsNow removes that race entirely.
//
// 30 is passed directly (not imported from a named constant) because
// PruneOldNeighborMetrics takes retentionDays as a plain parameter -- there
// is no production-side named retention constant to duplicate; 30 here is
// simply the boundary value this test chooses to exercise (it happens to
// match Config.MetricsRetentionDays' own fallback default, config.go, but
// that default is not itself a value this test needs to reach through).
func TestPruneOldNeighborMetrics(t *testing.T) {
	store := openNeighborsStore(t)
	seedObserverForNeighbors(t, store, "obs-metrics-5")
	const retentionDays = 30

	// Fixed reference instant, independent of the real wall clock and of
	// the test process's local timezone.
	fixedNow := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	origNow := pruneNeighborMetricsNow
	t.Cleanup(func() { pruneNeighborMetricsNow = origNow })
	pruneNeighborMetricsNow = func() time.Time { return fixedNow }

	cutoff := fixedNow.AddDate(0, 0, -retentionDays)
	pkOld := "ffff000000000000000000000000000000000000000000000000000000000006"
	pkBoundary := "ffff000000000000000000000000000000000000000000000000000000000007"
	pkRecent := "ffff000000000000000000000000000000000000000000000000000000000008"
	pkRecentOffset := "ffff000000000000000000000000000000000000000000000000000000000009"

	seed := func(pk, ts string) {
		t.Helper()
		if err := store.RecordObserverNeighborMetrics("obs-metrics-5", []ObserverNeighborEntry{{Pubkey: pk, SNR: floatPtr(1)}}, ts); err != nil {
			t.Fatalf("seed %s at %s: %v", pk, ts, err)
		}
	}
	// Clearly older than the window: age = retentionDays+1 days -> pruned.
	seed(pkOld, cutoff.AddDate(0, 0, -1).Format(time.RFC3339))
	// Exactly at the window edge: age == retentionDays -> retained (`<`, not `<=`).
	seed(pkBoundary, cutoff.Format(time.RFC3339))
	// Clearly newer than the window: age = retentionDays-1 days -> retained.
	seed(pkRecent, cutoff.AddDate(0, 0, 1).Format(time.RFC3339))
	// Same instant as pkRecent, expressed in a non-UTC offset. normalizeReportTS
	// (db.go) parses the offset and re-stores it as canonical UTC, so this must
	// prune/retain identically to pkRecent -- the fixture's timezone must not
	// change the outcome.
	recentInCopenhagenOffset := cutoff.AddDate(0, 0, 1).In(time.FixedZone("CEST", 2*60*60)).Format(time.RFC3339)
	seed(pkRecentOffset, recentInCopenhagenOffset)

	n, err := store.PruneOldNeighborMetrics(retentionDays)
	if err != nil {
		t.Fatal(err)
	}
	// Only pkOld crosses the boundary; the other three rows must not
	// influence each other's fate.
	if n != 1 {
		t.Fatalf("expected 1 row pruned, got %d", n)
	}
	if got := countObserverNeighborMetrics(t, store, "obs-metrics-5", pkOld); got != 0 {
		t.Errorf("pkOld (age %dd+1): expected pruned, %d row(s) remain", retentionDays, got)
	}
	if got := countObserverNeighborMetrics(t, store, "obs-metrics-5", pkBoundary); got != 1 {
		t.Errorf("pkBoundary (age exactly %dd): expected retained (timestamp < cutoff is strict), got %d row(s)", retentionDays, got)
	}
	if got := countObserverNeighborMetrics(t, store, "obs-metrics-5", pkRecent); got != 1 {
		t.Errorf("pkRecent (age %dd-1): expected retained, got %d row(s)", retentionDays, got)
	}
	if got := countObserverNeighborMetrics(t, store, "obs-metrics-5", pkRecentOffset); got != 1 {
		t.Errorf("pkRecentOffset (same instant as pkRecent, +02:00 offset): expected retained like pkRecent, got %d row(s)", got)
	}
}

func TestHandleNeighborsReport_RecordsSnrHistory(t *testing.T) {
	store := openNeighborsStore(t)
	seedObserverForNeighbors(t, store, "obs-metrics-6")

	report := map[string]interface{}{
		"timestamp": "2026-07-26T16:45:06.000000+00:00",
		"neighbors": []interface{}{
			map[string]interface{}{"pubkey": "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20", "snr": 10.0, "heard_secs_ago": 75.0, "scopes": "", "status": "timeout"},
			map[string]interface{}{"pubkey": "2102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20", "snr": 14.0, "heard_secs_ago": 83.0, "scopes": "dk", "status": "responded"},
		},
	}
	handleNeighborsReport(store, "test", "obs-metrics-6", report)

	var snr float64
	var heardSecsAgo int
	if err := store.db.QueryRow(`SELECT snr, heard_secs_ago FROM observer_neighbor_metrics WHERE observer_id = ? AND neighbor_pubkey = ?`,
		"obs-metrics-6", "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20").Scan(&snr, &heardSecsAgo); err != nil {
		t.Fatalf("select: %v", err)
	}
	if snr != 10.0 || heardSecsAgo != 75 {
		t.Errorf("snr=%v heardSecsAgo=%v, want 10.0/75 (timeout entry must still record SNR)", snr, heardSecsAgo)
	}
}
