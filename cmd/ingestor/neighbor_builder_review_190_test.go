package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// PR #190, second review (P2-1): the warm-up build must not stop after a
// full batch of observations just because that batch produced few edges, and
// the backfill must not start until the build has caught up.

const ts190 = int64(1780272000) // 2026-06-01T00:00:00Z

// seedObservations190 writes one transmission + observation per path, at
// timestamps ts190+offset+i, from the obs188 observer. Flood route, GRP_TXT.
func seedObservations190(t *testing.T, store *Store, offset int64, paths []string) []int64 {
	t.Helper()
	var obsIdx int64
	if err := store.db.QueryRow(`SELECT rowid FROM observers WHERE id = ?`, strings.ToUpper(obs188)).Scan(&obsIdx); err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	insTx, err := tx.Prepare(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json) VALUES ('00', ?, '2026-06-01T00:00:00Z', 1, 5, '{}')`)
	if err != nil {
		t.Fatal(err)
	}
	insObs, err := tx.Prepare(`INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp) VALUES (?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 0, len(paths))
	for i, p := range paths {
		res, err := insTx.Exec(fmt.Sprintf("h190-%d-%d", offset, i))
		if err != nil {
			t.Fatal(err)
		}
		txID, _ := res.LastInsertId()
		res, err = insObs.Exec(txID, obsIdx, p, ts190+offset+int64(i))
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		ids = append(ids, id)
	}
	insTx.Close()
	insObs.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func repeatPath190(p string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = p
	}
	return out
}

// A full first batch (neighborBuilderMaxBatch observations) that yields one
// edge must not end the warm-up: the edge observer <-> c3a comes from the
// next observation, in the second batch.
func TestNeighborEdgesBuilder_WarmUpScansPastFullBatchWithFewEdges_190(t *testing.T) {
	store := backfillFixture188(t, filepath.Join(t.TempDir(), "ingest.db"), true)
	defer store.Close()
	clearEdges188(t, store)
	if _, err := store.db.Exec(`INSERT INTO nodes (public_key, name, role) VALUES (?, 'b2a', 'repeater')`, b2a); err != nil {
		t.Fatal(err)
	}
	// Batch 1: 49,999 ambiguous ["c3"] rows (no edge) and, last, ["b233"]
	// (edge observer <-> b2a). Batch 2: ["c355"] (edge observer <-> c3a).
	first := append(repeatPath190(`["c3"]`, neighborBuilderMaxBatch-1), `["b233"]`)
	ids := seedObservations190(t, store, 0, first)
	seedObservations190(t, store, int64(neighborBuilderMaxBatch), []string{`["c355"]`})

	stop := store.StartNeighborEdgesBuilder(time.Hour)
	defer stop()
	if !store.neighborGraph.load().IsAdjacent(obs188, c3a) {
		t.Fatal("the warm-up stopped after a full batch with one edge: no observer<->c3a edge from the second batch")
	}
	res, err := store.RunResolvedPathBackfill(context.Background(), defaultResolvedPathBackfillBatchSize, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Resolved < len(ids)-1 {
		t.Fatalf("pass = %+v, want the %d ambiguous rows resolved", res, len(ids)-1)
	}
	wantAllResolvedTo188(t, store, ids[:3], c3a)
}

// A full batch that yields no edge newer than the watermark leaves the
// builder unable to move past it. The graph is then incomplete, so the
// backfill must not run on it, even though the graph is not empty.
func TestResolvedPathBackfill_WaitsWhileEdgeBuildCannotCatchUp_190(t *testing.T) {
	store := backfillFixture188(t, filepath.Join(t.TempDir(), "ingest.db"), true)
	defer store.Close()
	clearEdges188(t, store)
	// An older edge: the graph is not empty, and it sets the watermark.
	if _, err := store.db.Exec(`INSERT INTO neighbor_edges (node_a, node_b, count, last_seen) VALUES (?, ?, 5, '2026-05-01T00:00:00Z')`, a1a, b2a); err != nil {
		t.Fatal(err)
	}
	ids := seedObservations190(t, store, 0, repeatPath190(`["c3"]`, neighborBuilderMaxBatch))
	seedObservations190(t, store, int64(neighborBuilderMaxBatch), []string{`["c355"]`})

	// Ticks every 100 ms: a tick that has not caught up must not publish a
	// post-build graph either.
	stop := store.StartNeighborEdgesBuilder(100 * time.Millisecond)
	defer stop()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if built, _ := store.neighborGraph.buildState(); built {
			t.Fatal("a post-build graph was published although no build caught up")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := store.RunResolvedPathBackfill(context.Background(), defaultResolvedPathBackfillBatchSize, 0); err == nil {
		t.Fatal("the pass ran although the edge build has not caught up with the observations")
	}
	if w := backfillWatermarkOrZero188(t, store); w != 0 {
		t.Fatalf("watermark moved to %d on an incomplete graph", w)
	}
	if resolvedPathOf188(t, store, ids[0]).Valid {
		t.Fatal("a row was resolved on an incomplete graph")
	}
}
