package main

import (
	"os"
	"testing"
	"time"
)

// TestPruneTransmissionsChannelDaysTiming measures the #296 prune on a
// staging-sized fixture: 100 days of traffic, 5,000 transmissions a day
// (500,000; a staging copy holds ~497k), four observations each, and one in
// three a channel message (the share in test-fixtures/e2e-fixture.db, which
// is more than a typical mesh and so the expensive case for the walk over
// kept channel messages). It logs wall time and the longest writer hold for
// today's PruneOldPackets and for PruneTransmissions, each run three ways: the
// first run, the next day's prune (one more day ages out), and a run with
// nothing to delete. Opt-in, as seeding takes tens of seconds:
//
//	CORESCOPE_PRUNE_PERF=1 go test -run TestPruneTransmissionsChannelDaysTiming -v .
func TestPruneTransmissionsChannelDaysTiming(t *testing.T) {
	if os.Getenv("CORESCOPE_PRUNE_PERF") == "" {
		t.Skip("set CORESCOPE_PRUNE_PERF=1 to run the prune timing on a staging-sized fixture")
	}
	const days, perDay, obsPerTx = 100, 5000, 4

	seed := func(t *testing.T, store *Store) {
		t.Helper()
		// Let the empty DB's async migrations finish first, and give every
		// row a last_seen: a 0 would queue tx_last_seen_backfill_v1 over all
		// 500k rows, writing next to the prune being timed.
		store.WaitForAsyncMigrations()
		start := time.Now()
		now := time.Now().UTC()
		if _, err := store.db.Exec(`
			WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i + 1 < ?),
			     r(i, ts) AS (SELECT i, strftime('%Y-%m-%dT%H:%M:%SZ', ?, '-' || (i * 86400 / ?) || ' seconds') FROM n)
			INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, payload_version, decoded_json, last_seen)
			SELECT 'AA', 'perf-' || i, ts,
			       0, CASE i % 3 WHEN 0 THEN 5 WHEN 1 THEN 4 ELSE 2 END, 1, '{}', CAST(strftime('%s', ts) AS INTEGER)
			FROM r`, days*perDay, now.Format(time.RFC3339), perDay); err != nil {
			t.Fatalf("seed transmissions: %v", err)
		}
		if _, err := store.db.Exec(`
			WITH RECURSIVE k(j) AS (SELECT 0 UNION ALL SELECT j + 1 FROM k WHERE j + 1 < ?)
			INSERT INTO observations (transmission_id, observer_idx, direction, snr, rssi, score, path_json, timestamp)
			SELECT t.id, k.j, 'rx', 1.0, -100, 0, '[]', 0 FROM transmissions t, k`, obsPerTx); err != nil {
			t.Fatalf("seed observations: %v", err)
		}
		t.Logf("seeded %d transmissions, %d observations in %v",
			countRows(t, store, "transmissions"), countRows(t, store, "observations"), time.Since(start).Round(time.Millisecond))
	}

	run := func(t *testing.T, store *Store, label string, prune func() (PruneResult, error)) {
		t.Helper()
		ResetWriterStatsForTest()
		start := time.Now()
		r, err := prune()
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		elapsed := time.Since(start)
		stats := store.WriterStatsSnapshot()
		p, c := stats["prune_packets"], stats["prune_channel_messages"]
		t.Logf("%-28s %8v  packets=%-6d (%3d tx, hold max %6.1fms)  channel=%-6d (%2d tx, hold max %6.1fms)",
			label, elapsed.Round(time.Millisecond), r.Packets, p.Count, p.HoldMsMax, r.ChannelMessages, c.Count, c.HoldMsMax)
	}

	t.Run("today PruneOldPackets(14)", func(t *testing.T) {
		store := openPruneStore(t, "perf-today.db")
		seed(t, store)
		for _, step := range []struct {
			label string
			days  int
		}{{"first run (14)", 14}, {"next day (13)", 13}, {"nothing to delete (13)", 13}} {
			days := step.days
			run(t, store, step.label, func() (PruneResult, error) {
				n, err := store.PruneOldPackets(days)
				return PruneResult{Packets: n}, err
			})
		}
	})
	t.Run("PruneTransmissions(14, 90)", func(t *testing.T) {
		store := openPruneStore(t, "perf-channel.db")
		seed(t, store)
		for _, step := range []struct {
			label string
			days  int
		}{{"first run (14, 90)", 14}, {"next day (13, 90)", 13}, {"nothing to delete (13, 90)", 13}} {
			days := step.days
			run(t, store, step.label, func() (PruneResult, error) { return store.PruneTransmissions(days, 90) })
		}
	})
}
