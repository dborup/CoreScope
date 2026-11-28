package main

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// Issue #289 M2: repair observations.resolved_path rows the pre-#308
// resolved-path backfill wrote with the transmission's first route type
// (t.route_type) instead of each observation's own.
//
// The fixture is the #188 one (backfillFixture188): two relays sharing the
// 1-byte prefix "c3", one observer edge obs188 <-> c3a. So a flood ["c3"]
// resolves to c3a through the observer anchor and a DIRECT ["c3"] resolves to
// nothing. ["c355"] names c3a uniquely and resolves under either route type.

// rp289 is the stored form of a resolved path over the given pubkeys.
func rp289(pks ...string) string {
	out := make([]*string, len(pks))
	for i := range pks {
		pk := pks[i]
		if pk != "" {
			out[i] = &pk
		}
	}
	return marshalResolvedPath(out)
}

// setResolvedPath289 writes a row's resolved_path directly ("" = NULL): the
// value the pre-#308 backfill, or live ingest, had left there.
func setResolvedPath289(t *testing.T, store *Store, id int64, rp string) {
	t.Helper()
	if _, err := store.db.Exec(`UPDATE observations SET resolved_path = ? WHERE id = ?`, nilIfEmpty(rp), id); err != nil {
		t.Fatal(err)
	}
}

// smallRepairBatches289 makes the pass run many small batches so the
// multi-batch loop, the watermark and a resume are exercised on a few rows.
func smallRepairBatches289(t *testing.T, window int64, batch int) {
	t.Helper()
	oldWindow, oldBatch, oldPause := resolvedPathRouteRepairIDWindow, resolvedPathRouteRepairBatchSize, resolvedPathRouteRepairPause
	resolvedPathRouteRepairIDWindow, resolvedPathRouteRepairBatchSize, resolvedPathRouteRepairPause = window, batch, 0
	t.Cleanup(func() {
		resolvedPathRouteRepairIDWindow, resolvedPathRouteRepairBatchSize, resolvedPathRouteRepairPause = oldWindow, oldBatch, oldPause
	})
}

func repairState289(t *testing.T, store *Store) *ResolvedPathRouteRepairState {
	t.Helper()
	st, err := store.ResolvedPathRouteRepairState()
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("no resolved_path_route_repair_state row")
	}
	return st
}

// resetRepairWatermark289 rewinds the pass so the next run rescans everything
// with the same ceiling: used to show that the second scan writes nothing.
func resetRepairWatermark289(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.db.Exec(`UPDATE resolved_path_route_repair_state SET watermark = 0 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
}

type repairCase289 struct {
	name    string
	txRoute int
	mask    sql.NullInt64
	rawHex  string
	path    string
	stored  string // resolved_path before the pass ("" = NULL)
	want    string // resolved_path after the pass ("" = NULL)
	id      int64
}

// Both bug directions (the plain and the transport route types), the
// route_mask fallback, the rows that must not change, the drifted live-ingest
// rows that must not change either, and the mismatch whose answer does not
// depend on the route type.
func repairCases289() []repairCase289 {
	return []repairCase289{
		// #289 direction 1: a DIRECT reception anchored on the observer stored
		// a relay it never heard the packet from. The repair removes it.
		{name: "DIRECT frame under a FLOOD transmission", txRoute: routeFlood188, mask: mask289(mixedMask289),
			rawHex: directFrame289, path: `["c3"]`, stored: rp289(c3a), want: ""},
		// #289 direction 2: a FLOOD reception treated as DIRECT got no observer
		// anchor and stayed NULL. The repair resolves it.
		{name: "FLOOD frame under a DIRECT transmission", txRoute: routeDirect188, mask: mask289(mixedMask289),
			rawHex: floodFrame289, path: `["c3"]`, stored: "", want: rp289(c3a)},
		// No frame and a mask with both families: #308 reports the route type
		// as unknown, the pre-#308 backfill used t.route_type (FLOOD).
		{name: "no frame, mixed mask, FLOOD transmission", txRoute: routeFlood188, mask: mask289(mixedMask289),
			rawHex: "", path: `["c3"]`, stored: rp289(c3a), want: ""},

		// The same two directions with the transport route types, which differ
		// from the other family's non-transport type in bit 1 only
		// (PR #590 review F4).
		{name: "TRANSPORT_DIRECT frame under a FLOOD transmission", txRoute: routeFlood188, mask: mask289(mixedMask289),
			rawHex: transportDirectFrame289, path: `["c3"]`, stored: rp289(c3a), want: ""},
		{name: "TRANSPORT_FLOOD frame under a DIRECT transmission", txRoute: routeDirect188, mask: mask289(mixedMask289),
			rawHex: transportFloodFrame289, path: `["c3"]`, stored: "", want: rp289(c3a)},

		// Drifted live-ingest rows (PR #590 review F1): the family mismatch is
		// real, but the stored value is not what the pre-#308 rule produces
		// today, so the row was not written by that rule. Live ingest has
		// always used the frame's own route type, so these are correct values
		// resolved against an older graph, and the pass must leave them alone.
		{name: "DIRECT frame under a FLOOD transmission, drifted stored relay", txRoute: routeFlood188,
			mask: mask289(mixedMask289), rawHex: directFrame289, path: `["c3"]`, stored: rp289(c3b), want: rp289(c3b)},
		{name: "FLOOD frame under a DIRECT transmission, drifted stored relay", txRoute: routeDirect188,
			mask: mask289(mixedMask289), rawHex: floodFrame289, path: `["c3"]`, stored: rp289(c3b), want: rp289(c3b)},

		// Not candidates: the frame header and t.route_type agree.
		{name: "FLOOD frame under a FLOOD transmission", txRoute: routeFlood188, mask: mask289(floodMask289),
			rawHex: floodFrame289, path: `["c3"]`, stored: rp289(c3a), want: rp289(c3a)},
		{name: "DIRECT frame under a DIRECT transmission", txRoute: routeDirect188, mask: mask289(0b0100),
			rawHex: directFrame289, path: `["c3"]`, stored: "", want: ""},
		// No frame: #308 and t.route_type agree on flood-ness.
		{name: "no frame, mixed mask, DIRECT transmission", txRoute: routeDirect188, mask: mask289(mixedMask289),
			rawHex: "", path: `["c3"]`, stored: "", want: ""},
		{name: "no frame, flood-only mask", txRoute: routeFlood188, mask: mask289(floodMask289),
			rawHex: "", path: `["c3"]`, stored: rp289(c3a), want: rp289(c3a)},
		{name: "no frame, NULL mask", txRoute: routeFlood188, mask: sql.NullInt64{},
			rawHex: "", path: `["c3"]`, stored: rp289(c3a), want: rp289(c3a)},

		// Candidates whose answer does not depend on the route type: ["c355"]
		// names c3a uniquely either way. One has a stored value that is stale
		// for some other reason, which is not #289; the other already holds
		// the answer. Neither is written — the second one would be a write of
		// the value the row already has.
		{name: "DIRECT frame under a FLOOD transmission, unique 2-byte hop", txRoute: routeFlood188,
			mask: mask289(mixedMask289), rawHex: directFrame289, path: `["c355"]`, stored: rp289(c3b), want: rp289(c3b)},
		{name: "DIRECT frame under a FLOOD transmission, unique 2-byte hop, already correct", txRoute: routeFlood188,
			mask: mask289(mixedMask289), rawHex: directFrame289, path: `["c355"]`, stored: rp289(c3a), want: rp289(c3a)},
	}
}

// seedRepairCases289 seeds every case into one store, so the candidate
// selection runs over a mixed table.
func seedRepairCases289(t *testing.T, store *Store, cases []repairCase289) {
	t.Helper()
	for i := range cases {
		c := &cases[i]
		c.id = seedObservation289(t, store, c.txRoute, c.mask, c.rawHex, c.path, ts190+int64(i))
		setResolvedPath289(t, store, c.id, c.stored)
	}
}

func assertRepairCases289(t *testing.T, store *Store, cases []repairCase289) {
	t.Helper()
	for _, c := range cases {
		got := resolvedPathOf188(t, store, c.id)
		if c.want == "" {
			if got.Valid {
				t.Errorf("%s: resolved_path = %s, want NULL", c.name, got.String)
			}
			continue
		}
		if !got.Valid {
			t.Errorf("%s: resolved_path is NULL, want %s", c.name, c.want)
			continue
		}
		if got.String != c.want {
			t.Errorf("%s: resolved_path = %s, want %s", c.name, got.String, c.want)
		}
	}
}

func runRepair289(t *testing.T, store *Store) {
	t.Helper()
	if err := store.repairResolvedPathRouteTypes(context.Background(), store.db); err != nil {
		t.Fatal(err)
	}
}

// Both bug directions are repaired, and a row whose route type the frame and
// the transmission agree on is not touched.
func TestResolvedPathRouteRepair_RepairsBothDirections_289(t *testing.T) {
	smallRepairBatches289(t, 3, 2)
	store := backfillFixture188(t, filepath.Join(t.TempDir(), "ingest.db"), true)
	defer store.Close()
	cases := repairCases289()
	seedRepairCases289(t, store, cases)
	primeIndexAndGraph188(t, store)

	runRepair289(t, store)
	assertRepairCases289(t, store, cases)

	st := repairState289(t, store)
	if st.Watermark < st.Ceiling {
		t.Fatalf("watermark %d did not reach the ceiling %d", st.Watermark, st.Ceiling)
	}
	if st.Repaired != 2 || st.Cleared != 3 {
		t.Fatalf("state = %d repaired, %d cleared; want 2 repaired (the two NULL flood rows) and 3 cleared (the three wrong relays)", st.Repaired, st.Cleared)
	}
	if st.Candidates != 9 {
		t.Fatalf("candidates = %d, want 9 (five defects, two drifted rows and two route-type-independent rows)", st.Candidates)
	}
}

// A FLOOD reception treated as DIRECT was left NULL by the pre-#308 backfill,
// which then moved its watermark past the row. The corrected backfill never
// revisits it; this pass scans from id 0 and does.
func TestResolvedPathRouteRepair_RepairsNullRowBelowBackfillWatermark_289(t *testing.T) {
	store := backfillFixture188(t, filepath.Join(t.TempDir(), "ingest.db"), true)
	defer store.Close()
	id := seedObservation289(t, store, routeDirect188, mask289(mixedMask289), floodFrame289, `["c3"]`, ts190)
	primeIndexAndGraph188(t, store)

	// The pre-#308 backfill scanned the row, resolved nothing (it used the
	// transmission's DIRECT route type) and persisted a watermark past it.
	if err := ensureResolvedPathBackfillState(store.db); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO resolved_path_backfill_state (id, watermark, updated_at) VALUES (1, ?, '2026-06-01T00:00:00Z')
		ON CONFLICT(id) DO UPDATE SET watermark = excluded.watermark`, id); err != nil {
		t.Fatal(err)
	}
	res, err := store.RunResolvedPathBackfill(context.Background(), 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 0 {
		t.Fatalf("the backfill scanned %d rows below its watermark, want 0", res.Scanned)
	}
	if rp := resolvedPathOf188(t, store, id); rp.Valid {
		t.Fatalf("setup: resolved_path = %s, want the NULL row the pre-#308 backfill left", rp.String)
	}

	runRepair289(t, store)

	rp := resolvedPathOf188(t, store, id)
	if !rp.Valid || rp.String != rp289(c3a) {
		t.Fatalf("resolved_path = %v, want %s", rp, rp289(c3a))
	}
}

// A second pass writes nothing: the watermark has reached the ceiling, and a
// rescan from 0 finds every candidate already carrying the value it wants.
func TestResolvedPathRouteRepair_Idempotent_289(t *testing.T) {
	smallRepairBatches289(t, 3, 2)
	store := backfillFixture188(t, filepath.Join(t.TempDir(), "ingest.db"), true)
	defer store.Close()
	cases := repairCases289()
	seedRepairCases289(t, store, cases)
	primeIndexAndGraph188(t, store)

	runRepair289(t, store)
	first := repairState289(t, store)

	runRepair289(t, store) // watermark already at the ceiling
	if st := repairState289(t, store); st.Repaired != first.Repaired || st.Cleared != first.Cleared || st.Scanned != first.Scanned {
		t.Fatalf("a completed pass ran again: %+v, want %+v", st, first)
	}

	resetRepairWatermark289(t, store) // rescan the same ids
	runRepair289(t, store)
	st := repairState289(t, store)
	if st.Repaired != first.Repaired || st.Cleared != first.Cleared {
		t.Fatalf("the rescan wrote %d repaired / %d cleared more than the first pass (%d/%d)",
			st.Repaired-first.Repaired, st.Cleared-first.Cleared, first.Repaired, first.Cleared)
	}
	if st.Candidates <= first.Candidates {
		t.Fatalf("the rescan found %d candidates in all, want more than the first pass's %d (it must have read the rows again)", st.Candidates, first.Candidates)
	}
	assertRepairCases289(t, store, cases)
}

// A cancelled pass keeps the watermark of its last committed batch and the
// next run finishes the rest.
func TestResolvedPathRouteRepair_ResumesAfterStop_289(t *testing.T) {
	smallRepairBatches289(t, 1, 1) // one id per batch
	store := backfillFixture188(t, filepath.Join(t.TempDir(), "ingest.db"), true)
	defer store.Close()
	cases := repairCases289()
	seedRepairCases289(t, store, cases)
	primeIndexAndGraph188(t, store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batches := 0
	oldHook := resolvedPathRouteRepairPreWriteHook
	resolvedPathRouteRepairPreWriteHook = func() {
		batches++
		if batches == 2 {
			cancel()
		}
	}
	t.Cleanup(func() { resolvedPathRouteRepairPreWriteHook = oldHook })

	err := store.repairResolvedPathRouteTypes(ctx, store.db)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped pass returned %v, want context.Canceled", err)
	}
	stopped := repairState289(t, store)
	if stopped.Watermark == 0 || stopped.Watermark >= stopped.Ceiling {
		t.Fatalf("watermark after the stop = %d, want a committed batch below the ceiling %d", stopped.Watermark, stopped.Ceiling)
	}
	// The first case (id of cases[0]) is in the first window and is repaired;
	// the last case is not reached yet.
	if rp := resolvedPathOf188(t, store, cases[0].id); rp.Valid {
		t.Fatalf("the first batch did not commit its repair: resolved_path = %s, want NULL", rp.String)
	}
	last := cases[len(cases)-2] // the NULL-mask row that must stay as seeded
	if rp := resolvedPathOf188(t, store, last.id); !rp.Valid {
		t.Fatalf("a row above the watermark was already written: %s", last.name)
	}

	resolvedPathRouteRepairPreWriteHook = oldHook
	runRepair289(t, store)
	assertRepairCases289(t, store, cases)
	if st := repairState289(t, store); st.Watermark < st.Ceiling {
		t.Fatalf("watermark %d did not reach the ceiling %d after the resume", st.Watermark, st.Ceiling)
	}
	if st := repairState289(t, store); st.Scanned <= stopped.Scanned {
		t.Fatalf("the resume scanned nothing new (%d, stopped at %d)", st.Scanned, stopped.Scanned)
	}
}

// The pass resolves outside its write transaction (#267) and opens one write
// transaction per batch, so a live insert queues behind a batch and not behind
// the run.
func TestResolvedPathRouteRepair_ResolvesOutsideWriteTransaction_289(t *testing.T) {
	smallRepairBatches289(t, 2, 1)
	store := backfillFixture188(t, filepath.Join(t.TempDir(), "ingest.db"), true)
	defer store.Close()
	cases := repairCases289()
	seedRepairCases289(t, store, cases)
	primeIndexAndGraph188(t, store)

	var resolved, underLock int
	old := resolvedPathRouteRepairResolve
	resolvedPathRouteRepairResolve = func(hops []string, fromPubkey, observerID string, routeType int, graph *NeighborGraph, idx prefixIndex) []*string {
		resolved++
		if writerMuHeld() {
			underLock++
		}
		return old(hops, fromPubkey, observerID, routeType, graph, idx)
	}
	t.Cleanup(func() { resolvedPathRouteRepairResolve = old })

	batches := 0
	oldHook := resolvedPathRouteRepairPreWriteHook
	resolvedPathRouteRepairPreWriteHook = func() { batches++ }
	t.Cleanup(func() { resolvedPathRouteRepairPreWriteHook = oldHook })

	txBefore := writerStatsAgg.get(resolvedPathRouteRepairComponent).snapshot().Count
	runRepair289(t, store)

	if resolved == 0 {
		t.Fatal("the pass resolved nothing")
	}
	if underLock != 0 {
		t.Fatalf("%d of %d resolutions ran while the write transaction held writerMu", underLock, resolved)
	}
	if batches < 2 {
		t.Fatalf("%d batches, want several so the per-batch bound is exercised", batches)
	}
	// One write transaction per batch, plus the one that recorded the state row.
	if n := writerStatsAgg.get(resolvedPathRouteRepairComponent).snapshot().Count - txBefore; n != int64(batches)+1 {
		t.Fatalf("%d write transactions for %d batches, want one per batch plus the state row", n, batches)
	}
}

// Live ingest resolves with the frame's own route type and is authoritative.
// A row it rewrote between the batch's read and its write keeps ingest's
// value: every UPDATE is guarded on the value the batch read.
func TestResolvedPathRouteRepair_DoesNotOverwriteAConcurrentWrite_289(t *testing.T) {
	store := backfillFixture188(t, filepath.Join(t.TempDir(), "ingest.db"), true)
	defer store.Close()
	id := seedObservation289(t, store, routeFlood188, mask289(mixedMask289), directFrame289, `["c3"]`, ts190)
	setResolvedPath289(t, store, id, rp289(c3a)) // the pre-#308 backfill's wrong relay
	primeIndexAndGraph188(t, store)

	old := resolvedPathRouteRepairPreWriteHook
	resolvedPathRouteRepairPreWriteHook = func() { setResolvedPath289(t, store, id, rp289(c3b)) }
	t.Cleanup(func() { resolvedPathRouteRepairPreWriteHook = old })

	runRepair289(t, store)

	rp := resolvedPathOf188(t, store, id)
	if !rp.Valid || rp.String != rp289(c3b) {
		t.Fatalf("resolved_path = %v, want the concurrently written %s", rp, rp289(c3b))
	}
	if st := repairState289(t, store); st.Cleared != 0 || st.Repaired != 0 {
		t.Fatalf("state = %d repaired, %d cleared; want 0/0: the guarded UPDATE matched no row", st.Repaired, st.Cleared)
	}
}

// Resolving against a cold prefix index or neighbour graph would move the
// watermark past rows that cannot resolve yet, so the pass waits for the
// post-build graph instead of writing anything.
func TestResolvedPathRouteRepair_WaitsForIndexAndGraph_289(t *testing.T) {
	store := backfillFixture188(t, filepath.Join(t.TempDir(), "ingest.db"), true)
	defer store.Close()
	id := seedObservation289(t, store, routeDirect188, mask289(mixedMask289), floodFrame289, `["c3"]`, ts190)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := store.repairResolvedPathRouteTypes(ctx, store.db); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pass on a cold index returned %v, want context.DeadlineExceeded", err)
	}
	if st := repairState289(t, store); st.Watermark != 0 {
		t.Fatalf("watermark = %d, want 0: no batch may run before the index and graph are ready", st.Watermark)
	}
	if rp := resolvedPathOf188(t, store, id); rp.Valid {
		t.Fatalf("resolved_path = %s, want the untouched NULL", rp.String)
	}

	primeIndexAndGraph188(t, store)
	runRepair289(t, store)
	if rp := resolvedPathOf188(t, store, id); !rp.Valid || rp.String != rp289(c3a) {
		t.Fatalf("resolved_path = %v, want %s once the graph is ready", rp, rp289(c3a))
	}
}

// The pass is scheduled as an async migration, is recorded done, and a later
// start does not run it again.
func TestResolvedPathRouteRepair_RunsOnceAsAnAsyncMigration_289(t *testing.T) {
	store := backfillFixture188(t, filepath.Join(t.TempDir(), "ingest.db"), true)
	defer store.Close()
	id := seedObservation289(t, store, routeFlood188, mask289(mixedMask289), directFrame289, `["c3"]`, ts190)
	setResolvedPath289(t, store, id, rp289(c3a))
	primeIndexAndGraph188(t, store)

	store.StartResolvedPathRouteRepair(context.Background(), true)
	store.WaitForAsyncMigrations()

	if status, err := store.AsyncMigrationStatus(resolvedPathRouteRepairMigration); err != nil || status != "done" {
		t.Fatalf("migration status = %q, %v; want done", status, err)
	}
	if rp := resolvedPathOf188(t, store, id); rp.Valid {
		t.Fatalf("resolved_path = %s, want NULL", rp.String)
	}

	resetRepairWatermark289(t, store)
	store.StartResolvedPathRouteRepair(context.Background(), true)
	store.WaitForAsyncMigrations()
	if st := repairState289(t, store); st.Watermark != 0 {
		t.Fatalf("watermark = %d, want 0: a migration recorded done must not run again", st.Watermark)
	}
}

// PR #590 review F1. A family mismatch is not by itself a #289 defect: live
// ingest has always resolved with the frame's own route type, so most
// mismatched rows were written correctly by ingest. When the neighbour graph
// or the prefix index has drifted since, today's answer differs from the
// stored one, and rewriting the row would destroy a correct value. The pass
// therefore writes only a row whose stored value is exactly what the pre-#308
// rule produces today.
//
// Both rows below are the reviewer's repro on the #188 fixture: live ingest
// resolved them to c3b when c3b was the only "c3" node, and the edge that now
// picks c3a was added later.
func TestResolvedPathRouteRepair_LeavesDriftedLiveIngestRowsUntouched_289(t *testing.T) {
	store := backfillFixture188(t, filepath.Join(t.TempDir(), "ingest.db"), true)
	defer store.Close()
	// A DIRECT reception under a FLOOD transmission. Today DIRECT resolves to
	// nothing and the pre-#308 FLOOD rule to c3a, so an unguarded pass clears
	// the row.
	drifted1 := seedObservation289(t, store, routeFlood188, mask289(mixedMask289), directFrame289, `["c3"]`, ts190)
	setResolvedPath289(t, store, drifted1, rp289(c3b))
	// A FLOOD reception under a DIRECT transmission. Today the flood rule
	// resolves to c3a, so an unguarded pass overwrites the stored c3b.
	drifted2 := seedObservation289(t, store, routeDirect188, mask289(mixedMask289), floodFrame289, `["c3"]`, ts190+1)
	setResolvedPath289(t, store, drifted2, rp289(c3b))
	primeIndexAndGraph188(t, store)

	runRepair289(t, store)

	for _, c := range []struct {
		name string
		id   int64
	}{
		{"a DIRECT reception whose stored relay is not what the pre-#308 rule gives", drifted1},
		{"a FLOOD reception whose stored relay is not what the pre-#308 rule gives", drifted2},
	} {
		rp := resolvedPathOf188(t, store, c.id)
		if !rp.Valid || rp.String != rp289(c3b) {
			t.Errorf("%s: resolved_path = %v, want the untouched %s", c.name, rp, rp289(c3b))
		}
	}
	st := repairState289(t, store)
	if st.Repaired != 0 || st.Cleared != 0 {
		t.Fatalf("state = %d repaired, %d cleared; want 0/0: neither row carries the value the pre-#308 rule produces", st.Repaired, st.Cleared)
	}
	if st.Candidates != 2 {
		t.Fatalf("candidates = %d, want 2: both rows are read and then left alone", st.Candidates)
	}
	if st.Watermark < st.Ceiling {
		t.Fatalf("watermark %d did not reach the ceiling %d", st.Watermark, st.Ceiling)
	}
}

// PR #590 review F3. The pass overwrites data in place and keeps no copy of
// the previous value, so it must not run on the first boot of any build that
// contains it: it is opt-in, and an absent or empty block leaves it off.
func TestResolvedPathRouteRepairEnabled_OptIn_289(t *testing.T) {
	var nilCfg *Config
	for _, c := range []struct {
		name string
		cfg  *Config
		want bool
	}{
		{"nil config", nilCfg, false},
		{"no block", &Config{}, false},
		{"empty block", &Config{ResolvedPathRouteRepair: &ResolvedPathRouteRepairConfig{}}, false},
		{"enabled: false", &Config{ResolvedPathRouteRepair: &ResolvedPathRouteRepairConfig{Enabled: false}}, false},
		{"enabled: true", &Config{ResolvedPathRouteRepair: &ResolvedPathRouteRepairConfig{Enabled: true}}, true},
	} {
		if got := c.cfg.ResolvedPathRouteRepairEnabled(); got != c.want {
			t.Errorf("%s: ResolvedPathRouteRepairEnabled() = %v, want %v", c.name, got, c.want)
		}
	}
}

// The switch reaches the scheduler: with the repair off, nothing is recorded
// in _async_migrations, no state table is written and the defect row keeps the
// value the pre-#308 backfill left. Turning it on repairs the same row.
func TestResolvedPathRouteRepair_NotScheduledWhenDisabled_289(t *testing.T) {
	store := backfillFixture188(t, filepath.Join(t.TempDir(), "ingest.db"), true)
	defer store.Close()
	id := seedObservation289(t, store, routeFlood188, mask289(mixedMask289), directFrame289, `["c3"]`, ts190)
	setResolvedPath289(t, store, id, rp289(c3a))
	primeIndexAndGraph188(t, store)

	store.StartResolvedPathRouteRepair(context.Background(), false)
	store.WaitForAsyncMigrations()

	if status, err := store.AsyncMigrationStatus(resolvedPathRouteRepairMigration); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("migration status = %q, err = %v; want no recorded run", status, err)
	}
	st, err := store.ResolvedPathRouteRepairState()
	if err != nil {
		t.Fatal(err)
	}
	if st != nil {
		t.Fatalf("state row = %+v, want none: a disabled repair must not write", st)
	}
	if rp := resolvedPathOf188(t, store, id); !rp.Valid || rp.String != rp289(c3a) {
		t.Fatalf("resolved_path = %v, want the untouched %s", rp, rp289(c3a))
	}

	store.StartResolvedPathRouteRepair(context.Background(), true)
	store.WaitForAsyncMigrations()
	if status, err := store.AsyncMigrationStatus(resolvedPathRouteRepairMigration); err != nil || status != "done" {
		t.Fatalf("migration status = %q, %v; want done once the operator opts in", status, err)
	}
	if rp := resolvedPathOf188(t, store, id); rp.Valid {
		t.Fatalf("resolved_path = %s, want NULL once the repair is enabled", rp.String)
	}
}

// PR #590 review F5. The loop's gate can pass and the snapshot stop being
// usable before the batch reads it (a graph rebuild publishes an empty graph
// between the two). That batch writes nothing and reports
// errResolvedPathBackfillNotReady; the loop must retry the same watermark
// rather than fail the migration. The batch's readiness check is a seam so the
// race does not have to be won for real.
func TestResolvedPathRouteRepair_RetriesABatchThatSeesAColdSnapshot_289(t *testing.T) {
	store := backfillFixture188(t, filepath.Join(t.TempDir(), "ingest.db"), true)
	defer store.Close()
	id := seedObservation289(t, store, routeDirect188, mask289(mixedMask289), floodFrame289, `["c3"]`, ts190)
	primeIndexAndGraph188(t, store)

	checks := 0
	old := resolvedPathRouteRepairBatchReady
	resolvedPathRouteRepairBatchReady = func(s *Store, idx prefixIndex, graph *NeighborGraph) bool {
		checks++
		if checks == 1 {
			return false // the snapshot went cold after the loop's gate passed
		}
		return old(s, idx, graph)
	}
	t.Cleanup(func() { resolvedPathRouteRepairBatchReady = old })

	runRepair289(t, store)

	if checks < 2 {
		t.Fatalf("the batch ran %d time(s), want a retry after the not-ready batch", checks)
	}
	if rp := resolvedPathOf188(t, store, id); !rp.Valid || rp.String != rp289(c3a) {
		t.Fatalf("resolved_path = %v, want %s from the retried batch", rp, rp289(c3a))
	}
	st := repairState289(t, store)
	if st.Watermark < st.Ceiling {
		t.Fatalf("watermark %d did not reach the ceiling %d", st.Watermark, st.Ceiling)
	}
	if st.Repaired != 1 {
		t.Fatalf("state = %d repaired, want 1 from the retried batch", st.Repaired)
	}
}
