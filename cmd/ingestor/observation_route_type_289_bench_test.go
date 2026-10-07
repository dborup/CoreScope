package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #289 M1 cost: both batch readers select one more 2-character slice of
// observations.raw_hex and t.route_mask per row. These benchmarks time one
// full batch of each reader on rows that carry a frame, at the batch sizes
// production uses (neighborBuilderMaxBatch, the backfill default).

// frame289 is a 60-byte frame: header, path length, then filler.
func frame289(header string) string {
	return header + "01c3" + strings.Repeat("ab", 57)
}

// seedFramedRows289 writes n transmissions with one framed observation each,
// path ["c3"], every other one a mixed-mask DIRECT frame. Timestamps start
// after the fixture's edge, so the builder scans them.
func seedFramedRows289(b *testing.B, store *Store, n int) []int64 {
	b.Helper()
	var obsIdx int64
	if err := store.db.QueryRow(`SELECT rowid FROM observers WHERE id = ?`, strings.ToUpper(obs188)).Scan(&obsIdx); err != nil {
		b.Fatal(err)
	}
	tx, err := store.db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	insTx, err := tx.Prepare(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, route_mask) VALUES ('00', ?, '2026-06-01T00:00:00Z', 1, 5, '{}', ?)`)
	if err != nil {
		b.Fatal(err)
	}
	insObs, err := tx.Prepare(`INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp, raw_hex) VALUES (?, ?, '["c3"]', ?, ?)`)
	if err != nil {
		b.Fatal(err)
	}
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		mask, header := floodMask289, "15"
		if i%2 == 1 {
			mask, header = mixedMask289, "16"
		}
		res, err := insTx.Exec(fmt.Sprintf("h289-bench-%d", i), mask)
		if err != nil {
			b.Fatal(err)
		}
		txID, _ := res.LastInsertId()
		res, err = insObs.Exec(txID, obsIdx, ts190+1+int64(i), frame289(header))
		if err != nil {
			b.Fatal(err)
		}
		id, _ := res.LastInsertId()
		ids = append(ids, id)
	}
	insTx.Close()
	insObs.Close()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return ids
}

// BenchmarkNeighborEdgesBuildBatch_289 times one full builder batch
// (neighborBuilderMaxBatch rows) from an empty neighbor_edges table.
func BenchmarkNeighborEdgesBuildBatch_289(b *testing.B) {
	store := backfillFixture188(b, filepath.Join(b.TempDir(), "ingest.db"), true)
	defer store.Close()
	seedFramedRows289(b, store, neighborBuilderMaxBatch)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		if _, err := store.db.Exec(`DELETE FROM neighbor_edges`); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		r, err := store.buildNeighborEdges()
		if err != nil {
			b.Fatal(err)
		}
		if r.scanned != neighborBuilderMaxBatch {
			b.Fatalf("scanned %d rows, want %d", r.scanned, neighborBuilderMaxBatch)
		}
	}
}

// BenchmarkResolvedPathBackfillBatch_289 times one backfill batch at the
// default batch size over framed rows; ms/batch is the whole batch, read
// and resolve included.
func BenchmarkResolvedPathBackfillBatch_289(b *testing.B) {
	const batchSize = defaultResolvedPathBackfillBatchSize
	store := backfillFixture188(b, filepath.Join(b.TempDir(), "ingest.db"), true)
	defer store.Close()
	ids := seedFramedRows289(b, store, batchSize)
	primeIndexAndGraph188(b, store)
	if err := ensureResolvedPathBackfillState(store.db); err != nil {
		b.Fatal(err)
	}
	var total time.Duration
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		if _, err := store.db.Exec(`UPDATE observations SET resolved_path = NULL`); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		start := time.Now()
		if _, err := store.resolvedPathBackfillBatch(context.Background(), 0, ids[len(ids)-1], batchSize); err != nil {
			b.Fatal(err)
		}
		total += time.Since(start)
	}
	b.ReportMetric(float64(total.Microseconds())/1000/float64(b.N), "ms/batch")
}
