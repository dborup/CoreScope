package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Issue #200: tests that kill the mutants X1, X3 and X4 from the PR #190
// review (round 2b), which survived the whole ingestor suite.

// Mutant X1: a neighbour-edge tick whose build failed must not publish a
// post-build graph. A failed build reports scanned == 0 or a short batch, so
// caughtUp() alone would call it complete.
//
// The upsert into neighbor_edges fails (a trigger stands in for a write
// error), so the warm-up and every tick fail after reading the new
// observation. Reading neighbor_edges still works, so each tick still
// refreshes the graph, and that graph is not empty: without the err == nil
// guard the backfill would be ready and run on it.
func TestNeighborEdgesBuilder_FailedTickDoesNotMarkGraphBuilt_200(t *testing.T) {
	store := backfillFixture188(t, filepath.Join(t.TempDir(), "ingest.db"), true)
	defer store.Close()
	ids := seedNullRows188(t, store, 5)
	// Newer than the seeded edge's last_seen, so every build reads it and
	// tries to upsert observer <-> c3a.
	seedObservations190(t, store, 1, []string{`["c355"]`})
	for _, op := range []string{"INSERT", "UPDATE"} {
		if _, err := store.db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_edge_%s BEFORE %s ON neighbor_edges
			BEGIN SELECT RAISE(ABORT, 'neighbor_edges write fails (#200)'); END`, strings.ToLower(op), op)); err != nil {
			t.Fatal(err)
		}
	}

	stop := store.StartNeighborEdgesBuilder(20 * time.Millisecond)
	defer stop()
	// Every tick publishes a new snapshot. After the second one, the first
	// tick has finished, including any storeBuilt.
	waitForGraphSnapshots200(t, store, 2)
	if built, _ := store.neighborGraph.buildState(); built {
		t.Error("a failed neighbour-edge tick published a post-build graph")
	}
	_, err := store.RunResolvedPathBackfill(context.Background(), defaultResolvedPathBackfillBatchSize, 0)
	if !errors.Is(err, errResolvedPathBackfillNotReady) {
		t.Errorf("pass after failed ticks: err = %v, want not ready", err)
	}
	if w := backfillWatermarkOrZero188(t, store); w != 0 {
		t.Errorf("watermark moved to %d after failed ticks", w)
	}
	if resolvedPathOf188(t, store, ids[0]).Valid {
		t.Error("a row was resolved after failed ticks")
	}
	if t.Failed() {
		return
	}

	// Control: once the write works again, the next tick marks the graph
	// built and the same pass runs.
	for _, op := range []string{"insert", "update"} {
		if _, err := store.db.Exec(`DROP TRIGGER fail_edge_` + op); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for built, _ := store.neighborGraph.buildState(); !built; built, _ = store.neighborGraph.buildState() {
		if time.Now().After(deadline) {
			t.Fatal("no successful tick published a post-build graph")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := store.RunResolvedPathBackfill(context.Background(), defaultResolvedPathBackfillBatchSize, 0); err != nil {
		t.Fatal(err)
	}
	wantAllResolvedTo188(t, store, ids, c3a)
}

// waitForGraphSnapshots200 waits until the neighbour graph snapshot has been
// replaced n times.
func waitForGraphSnapshots200(t *testing.T, store *Store, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	last := store.neighborGraph.load()
	for seen := 0; seen < n; {
		if time.Now().After(deadline) {
			t.Fatalf("saw %d of %d neighbour graph refreshes", seen, n)
		}
		if g := store.neighborGraph.load(); g != last {
			last = g
			seen++
			continue
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Mutant X3: an observation whose transmission has a NULL route_type creates
// no observer <-> last-hop edge. Only flood route types (0, 1) do; the
// builder reads NULL as -1. A flood row in the same build is the control.
func TestNeighborEdgesBuilder_NullRouteTypeAddsNoObserverEdge_200(t *testing.T) {
	store := backfillFixture188(t, filepath.Join(t.TempDir(), "ingest.db"), true)
	defer store.Close()
	clearEdges188(t, store)
	if _, err := store.db.Exec(`INSERT INTO nodes (public_key, name, role) VALUES (?, 'a111', 'repeater')`, a1a); err != nil {
		t.Fatal(err)
	}
	var obsIdx int64
	if err := store.db.QueryRow(`SELECT rowid FROM observers WHERE id = ?`, strings.ToUpper(obs188)).Scan(&obsIdx); err != nil {
		t.Fatal(err)
	}
	for i, o := range []struct {
		route any
		path  string
	}{
		{routeFlood188, `["a111"]`}, // control: observer heard a1a
		{nil, `["c366"]`},           // NULL route type: no edge to c3b
	} {
		res, err := store.db.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json) VALUES ('00', ?, '2026-06-01T00:00:00Z', ?, 5, '{}')`,
			fmt.Sprintf("h200-route-%d", i), o.route)
		if err != nil {
			t.Fatal(err)
		}
		txID, _ := res.LastInsertId()
		if _, err := store.db.Exec(`INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp) VALUES (?, ?, ?, ?)`, txID, obsIdx, o.path, ts190+int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	var nulls int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM transmissions WHERE route_type IS NULL`).Scan(&nulls); err != nil || nulls != 1 {
		t.Fatalf("fixture: %d transmissions with NULL route_type (err %v), want 1", nulls, err)
	}
	if _, err := store.buildAndPersistNeighborEdges(); err != nil {
		t.Fatal(err)
	}
	g, err := loadNeighborGraph(store.db)
	if err != nil {
		t.Fatal(err)
	}
	if !g.IsAdjacent(obs188, a1a) {
		t.Fatal("no observer<->a1a edge from the flood observation")
	}
	if g.IsAdjacent(obs188, c3b) {
		t.Fatal("observer<->c3b edge built from an observation with NULL route_type")
	}
}

// fromAlt200 shares the 1-byte prefix "f0" with the originator from188.
const fromAlt200 = "f0aa0000000000000000000000000000000000000000000000000000000000bb"

// Mutant X4: the backward chain never resolves a hop to the packet's
// originator; a node does not appear in the path of a flood it originated.
//
// Flood ADVERT from from188 with path ["f0", "b2"]. The forward chain leaves
// hop 0 nil: its anchor is the originator, which is excluded, and fromAlt200
// has no known edge. Hop 1 (b2a) is unique. The backward chain anchors hop 0
// on b2a, and the only "f0" candidate adjacent to b2a is the originator, so
// without the exclusion hop 0 would resolve to it.
func TestObserverAnchor_BackwardWalkExcludesOriginator_200(t *testing.T) {
	idx := idx188(obs188, from188, fromAlt200, b2a)
	g := graph188([2]string{obs188, b2a}, [2]string{b2a, from188})
	hops := []string{"f0", "b2"}

	fwd := resolvePathForward(hops, from188, obs188, g, idx)
	wantPath188(t, fwd, "", b2a)
	for i, p := range resolvePathBackward(hops, from188, obs188, g, idx) {
		if p != nil && *p == from188 {
			t.Fatalf("backward chain resolved hop %d to the originator", i)
		}
	}
	got := resolveObservationPath(hops, from188, obs188, routeFlood188, g, idx)
	wantPath188(t, got, "", b2a)
}
