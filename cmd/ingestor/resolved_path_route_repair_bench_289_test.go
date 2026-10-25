package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Sizing for the #289 M2 repair pass. The plan on #289 asks for the run time
// at the ~6.5M observations of the #89 sizing comment, so this builds a
// synthetic database of that shape at a fraction of the size and reports the
// per-observation cost, the writer hold percentiles and the extrapolation.
//
// Run it with:
//
//	go test ./cmd/ingestor -run '^$' -bench ResolvedPathRouteRepairPass_289 -benchtime 1x -timeout 60m
//
// The row mix follows the plan's reasoning: a transmission row is shared by
// receptions of the same payload, most of which carry the route type of the
// first reception, and only a small share carry the other family. The share is
// a package var so a sizing run can try a different one.
var (
	repairBenchObservations   = 1_200_000
	repairBenchObsPerTx       = 3
	repairBenchCandidateEvery = 100 // 1 in N observations is a #289 candidate
	repairBenchTargetRows     = 6_500_000
)

// obsBench289 are the observers the synthetic rows are spread over: the
// observations table rejects two rows with the same transmission, observer and
// path (idx_observations_dedup), so several receptions of one transmission
// need different observers. Each gets the same obs <-> c3a edge as obs188, so
// a flood ["c3"] resolves to c3a for all of them.
var obsBench289 = []string{
	obs188,
	"0b5e000000000000000000000000000000000000000000000000000000000002",
	"0b5e000000000000000000000000000000000000000000000000000000000003",
}

// seedRepairBenchDB fills store with repairBenchObservations observations over
// repairBenchObservations/repairBenchObsPerTx transmissions. Every third
// transmission carries a mixed route_mask; inside those, one observation in
// repairBenchCandidateEvery carries a frame of the other route family, which
// makes it a repair candidate. Every other observation carries a frame of the
// transmission's own family, so the SQL prefilter drops it without the pass
// reading the row.
func seedRepairBenchDB(b *testing.B, store *Store) (candidates int) {
	b.Helper()
	if repairBenchObsPerTx > len(obsBench289) {
		b.Fatalf("repairBenchObsPerTx = %d, only %d observers are seeded", repairBenchObsPerTx, len(obsBench289))
	}
	obsIdx := make([]int64, 0, repairBenchObsPerTx)
	for _, id := range obsBench289[:repairBenchObsPerTx] {
		if err := store.UpsertObserver(strings.ToUpper(id), "bench-289", "", nil); err != nil {
			b.Fatal(err)
		}
		if _, err := store.db.Exec(`INSERT OR IGNORE INTO neighbor_edges (node_a, node_b, count, last_seen)
			VALUES (?, ?, 5, '2026-06-01T00:00:00Z')`, id, c3a); err != nil {
			b.Fatal(err)
		}
		var idx int64
		if err := store.db.QueryRow(`SELECT rowid FROM observers WHERE id = ?`, strings.ToUpper(id)).Scan(&idx); err != nil {
			b.Fatal(err)
		}
		obsIdx = append(obsIdx, idx)
	}
	txCount := repairBenchObservations / repairBenchObsPerTx
	start := time.Now()
	rows := 0
	// The insert runs in its own function so a failure rolls the transaction
	// back before b.Fatal unwinds the goroutine: the store holds a single
	// SQLite connection, and an open transaction would deadlock the cleanup.
	err := func() error {
		tx, err := store.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback() // no-op after Commit
		insTx, err := tx.Prepare(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, route_mask)
			VALUES ('00', ?, '2026-06-01T00:00:00Z', ?, 5, '{}', ?)`)
		if err != nil {
			return err
		}
		defer insTx.Close()
		insObs, err := tx.Prepare(`INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp, raw_hex, resolved_path)
			VALUES (?, ?, '["c3"]', ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer insObs.Close()
		for i := 0; i < txCount; i++ {
			mask := sql.NullInt64{Int64: floodMask289, Valid: true}
			mixed := i%3 == 0
			if mixed {
				mask = sql.NullInt64{Int64: mixedMask289, Valid: true}
			}
			res, err := insTx.Exec(fmt.Sprintf("bench289-%d", i), routeFlood188, mask)
			if err != nil {
				return fmt.Errorf("transmission %d: %w", i, err)
			}
			txID, _ := res.LastInsertId()
			for j := 0; j < repairBenchObsPerTx; j++ {
				rows++
				frame := floodFrame289
				if mixed && rows%repairBenchCandidateEvery == 0 {
					// A DIRECT reception the pre-#308 backfill anchored on the
					// observer: the pass must clear the stored relay.
					frame = directFrame289
					candidates++
				}
				if _, err := insObs.Exec(txID, obsIdx[j], ts190+int64(rows), frame, rp289(c3a)); err != nil {
					return fmt.Errorf("observation %d: %w", rows, err)
				}
			}
		}
		return tx.Commit()
	}()
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("seeded %d transmissions / %d observations (%d candidates) in %s",
		txCount, rows, candidates, time.Since(start).Round(time.Second))
	return candidates
}

// BenchmarkResolvedPathRouteRepairPass_289 runs the whole pass once over the
// synthetic database and reports the cost per observation scanned, the writer
// hold percentiles and the extrapolation to repairBenchTargetRows.
func BenchmarkResolvedPathRouteRepairPass_289(b *testing.B) {
	for i := 0; i < b.N; i++ {
		func() {
			dbPath := filepath.Join(b.TempDir(), "ingest.db")
			store := backfillFixture188(b, dbPath, true)
			defer store.Close()
			wantCandidates := seedRepairBenchDB(b, store)
			primeIndexAndGraph188(b, store)
			store.Checkpoint()
			if fi, err := os.Stat(dbPath); err == nil {
				b.Logf("synthetic database: %.0f MB", float64(fi.Size())/(1<<20))
			}

			oldPause := resolvedPathRouteRepairPause
			resolvedPathRouteRepairPause = 0 // the pause is wall-clock, not cost
			defer func() { resolvedPathRouteRepairPause = oldPause }()
			before := writerStatsAgg.get(resolvedPathRouteRepairComponent).snapshot()

			b.ResetTimer()
			start := time.Now()
			if err := store.repairResolvedPathRouteTypes(context.Background(), store.db); err != nil {
				b.Fatal(err)
			}
			elapsed := time.Since(start)
			b.StopTimer()

			st, err := store.ResolvedPathRouteRepairState()
			if err != nil || st == nil {
				b.Fatalf("state: %v", err)
			}
			if int(st.Cleared) != wantCandidates {
				b.Fatalf("cleared %d rows, want the %d seeded candidates", st.Cleared, wantCandidates)
			}
			w := writerStatsAgg.get(resolvedPathRouteRepairComponent).snapshot()
			perRow := float64(elapsed.Nanoseconds()) / float64(st.Ceiling)
			b.Logf("pass over %d observations: %s wall (%.0f ns/observation), %d candidates, %d cleared, %d batches",
				st.Ceiling, elapsed.Round(time.Millisecond), perRow, st.Candidates, st.Cleared, w.Count-before.Count)
			b.Logf("writer hold ms: p50 %.1f p95 %.1f p99 %.1f max %.1f over %d transactions (window %d, batch %d)",
				w.HoldMsP50, w.HoldMsP95, w.HoldMsP99, w.HoldMsMax, w.Count-before.Count,
				resolvedPathRouteRepairIDWindow, resolvedPathRouteRepairBatchSize)
			b.Logf("extrapolated to %d observations: %s of CPU plus the %s pause per batch",
				repairBenchTargetRows,
				time.Duration(perRow*float64(repairBenchTargetRows)).Round(time.Second),
				oldPause)
			b.ReportMetric(perRow, "ns/observation")
		}()
	}
}

// BenchmarkResolvedPathRouteRepairBatch_289 is the per-batch cost on the same
// synthetic database: one window read, resolved and written.
func BenchmarkResolvedPathRouteRepairBatch_289(b *testing.B) {
	store := backfillFixture188(b, filepath.Join(b.TempDir(), "ingest.db"), true)
	defer store.Close()
	seedRepairBenchDB(b, store)
	primeIndexAndGraph188(b, store)
	st, err := store.resolvedPathRouteRepairStart()
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if st.Watermark >= st.Ceiling {
			st.Watermark = 0 // wrap: the rows are already repaired, the scan cost is the same
		}
		if _, err := store.resolvedPathRouteRepairBatch(context.Background(), st); err != nil {
			b.Fatal(err)
		}
	}
}
