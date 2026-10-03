package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Issue #188 point 4: backfill of NULL observations.resolved_path.

// backfillFixture188 opens a store at path with two relays sharing the
// 1-byte prefix "c3" and an observer whose neighbour edge picks c3a, so a
// non-advert flood observation with path ["c3"] resolves to c3a.
func backfillFixture188(t *testing.T, path string, seed bool) *Store {
	t.Helper()
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	store.backfillWg.Wait()
	if seed {
		for _, pk := range []string{c3a, c3b} {
			if _, err := store.db.Exec(`INSERT INTO nodes (public_key, name, role) VALUES (?, ?, 'repeater')`, pk, pk[:4]); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.UpsertObserver(strings.ToUpper(obs188), "observer-188", "", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`INSERT INTO neighbor_edges (node_a, node_b, count, last_seen) VALUES (?, ?, 5, '2026-06-01T00:00:00Z')`, obs188, c3a); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func primeIndexAndGraph188(t *testing.T, store *Store) {
	t.Helper()
	if err := store.RefreshPrefixIndex(); err != nil {
		t.Fatal(err)
	}
	if err := store.RefreshNeighborGraph(); err != nil {
		t.Fatal(err)
	}
}

// seedNullRows188 writes n transmissions with one observation each, path
// ["c3"], resolved_path NULL: rows stored before #188. Returns their ids.
func seedNullRows188(t testing.TB, store *Store, n int) []int64 {
	t.Helper()
	var obsIdx int64
	if err := store.db.QueryRow(`SELECT rowid FROM observers WHERE id = ?`, strings.ToUpper(obs188)).Scan(&obsIdx); err != nil {
		t.Fatal(err)
	}
	var base int
	store.db.QueryRow(`SELECT COUNT(*) FROM transmissions`).Scan(&base)
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		res, err := tx.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json) VALUES ('00', ?, '2026-06-01T00:00:00Z', 1, 5, '{}')`,
			fmt.Sprintf("h188-%d", base+i))
		if err != nil {
			t.Fatal(err)
		}
		txID, _ := res.LastInsertId()
		res, err = tx.Exec(`INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp) VALUES (?, ?, '["c3"]', 1780272000)`, txID, obsIdx)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		ids = append(ids, id)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func resolvedPathOf188(t *testing.T, store *Store, id int64) sql.NullString {
	t.Helper()
	var rp sql.NullString
	if err := store.db.QueryRow(`SELECT resolved_path FROM observations WHERE id = ?`, id).Scan(&rp); err != nil {
		t.Fatal(err)
	}
	return rp
}

func persistedWatermark188(t *testing.T, store *Store) int64 {
	t.Helper()
	w, err := store.resolvedPathBackfillWatermark()
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// Rows ingested before the prefix index is primed (the start-up window in
// main.go) are stored NULL; the backfill that starts after priming resolves
// them.
func TestResolvedPathBackfill_ResolvesRowsIngestedBeforePriming_188(t *testing.T) {
	store := backfillFixture188(t, filepath.Join(t.TempDir(), "ingest.db"), true)
	defer store.Close()
	if store.prefixIdx.load() != nil {
		t.Fatal("setup: the prefix index is already primed")
	}
	pkt := &PacketData{RawHex: "c0ffee", Timestamp: "2026-06-01T00:00:00Z", ObserverID: strings.ToUpper(obs188),
		Hash: "h-188-window", RouteType: 1, PayloadType: 5, PathJSON: `["c3"]`, DecodedJSON: "{}"}
	if _, err := store.InsertTransmission(pkt); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := store.db.QueryRow(`SELECT id FROM observations ORDER BY id DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if rp := resolvedPathOf188(t, store, id); rp.Valid {
		t.Fatalf("setup: row ingested before priming was resolved: %s", rp.String)
	}

	primeIndexAndGraph188(t, store)
	if _, err := store.RunResolvedPathBackfill(context.Background(), 100, 0); err != nil {
		t.Fatal(err)
	}
	rp := resolvedPathOf188(t, store, id)
	if !rp.Valid {
		t.Fatal("the backfill left the start-up window row NULL")
	}
	wantPath188(t, unmarshalResolvedPathLocal(rp.String), c3a)
}

func TestResolvedPathBackfill_BatchBoundAndWatermark_188(t *testing.T) {
	store := backfillFixture188(t, filepath.Join(t.TempDir(), "ingest.db"), true)
	defer store.Close()
	ids := seedNullRows188(t, store, 25)
	primeIndexAndGraph188(t, store)
	if err := ensureResolvedPathBackfillState(store.db); err != nil {
		t.Fatal(err)
	}
	ceiling := ids[19] // rows above the ceiling arrived after the pass started

	b, err := store.resolvedPathBackfillBatch(context.Background(), 0, ceiling, 10)
	if err != nil {
		t.Fatal(err)
	}
	if b.Scanned != 10 || b.Resolved != 10 || b.Last != ids[9] {
		t.Fatalf("first batch = %+v, want 10 scanned, 10 resolved, last id %d", b, ids[9])
	}
	if w := persistedWatermark188(t, store); w != ids[9] {
		t.Fatalf("persisted watermark = %d, want %d", w, ids[9])
	}
	for i, id := range ids {
		if got, want := resolvedPathOf188(t, store, id).Valid, i < 10; got != want {
			t.Fatalf("row %d resolved = %v after one batch of 10, want %v", i, got, want)
		}
	}

	b, err = store.resolvedPathBackfillBatch(context.Background(), b.Last, ceiling, 10)
	if err != nil {
		t.Fatal(err)
	}
	if b.Scanned != 10 || b.Last != ids[19] {
		t.Fatalf("second batch = %+v, want 10 scanned up to id %d", b, ids[19])
	}
	b, err = store.resolvedPathBackfillBatch(context.Background(), b.Last, ceiling, 10)
	if err != nil || b.Scanned != 0 {
		t.Fatalf("batch past the ceiling = %+v, %v; want nothing", b, err)
	}
	for _, id := range ids[20:] {
		if resolvedPathOf188(t, store, id).Valid {
			t.Fatalf("row %d above the ceiling was touched", id)
		}
	}
}

// Re-running never rewrites a row: only NULL rows are updated, and a row
// that already has a resolved_path is left as it is.
func TestResolvedPathBackfill_Idempotent_188(t *testing.T) {
	store := backfillFixture188(t, filepath.Join(t.TempDir(), "ingest.db"), true)
	defer store.Close()
	ids := seedNullRows188(t, store, 12)
	const kept = `["kept-by-ingest"]`
	if _, err := store.db.Exec(`UPDATE observations SET resolved_path = ? WHERE id = ?`, kept, ids[3]); err != nil {
		t.Fatal(err)
	}
	primeIndexAndGraph188(t, store)

	first, err := store.RunResolvedPathBackfill(context.Background(), 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.Scanned != 12 || first.Resolved != 11 || first.Batches != 3 {
		t.Fatalf("first pass = %+v, want 12 scanned, 11 resolved, 3 batches", first)
	}
	before := map[int64]string{}
	for _, id := range ids {
		before[id] = resolvedPathOf188(t, store, id).String
	}
	if before[ids[3]] != kept {
		t.Fatalf("a row that already had a resolved_path was rewritten to %s", before[ids[3]])
	}

	// Forget the watermark so the same rows are scanned again.
	if _, err := store.db.Exec(`DELETE FROM resolved_path_backfill_state`); err != nil {
		t.Fatal(err)
	}
	second, err := store.RunResolvedPathBackfill(context.Background(), 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if second.Scanned != 12 || second.Resolved != 0 {
		t.Fatalf("second pass = %+v, want 12 scanned, 0 resolved", second)
	}
	for _, id := range ids {
		if got := resolvedPathOf188(t, store, id).String; got != before[id] {
			t.Fatalf("row %d changed on re-run: %s -> %s", id, before[id], got)
		}
	}
	// A pass with nothing new does no work.
	third, err := store.RunResolvedPathBackfill(context.Background(), 5, 0)
	if err != nil || third.Scanned != 0 {
		t.Fatalf("third pass = %+v, %v; want nothing to do", third, err)
	}
}

// After a restart the pass resumes at the persisted watermark: rows below it
// are not scanned again, rows above it are.
func TestResolvedPathBackfill_ResumesAfterRestart_188(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ingest.db")
	store := backfillFixture188(t, path, true)
	ids := seedNullRows188(t, store, 30)
	primeIndexAndGraph188(t, store)
	if err := ensureResolvedPathBackfillState(store.db); err != nil {
		t.Fatal(err)
	}
	// The process stops after its first committed batch.
	if _, err := store.resolvedPathBackfillBatch(context.Background(), 0, ids[29], 10); err != nil {
		t.Fatal(err)
	}
	// Mark a row below the watermark: a rescan would resolve it again.
	if _, err := store.db.Exec(`UPDATE observations SET resolved_path = NULL WHERE id = ?`, ids[0]); err != nil {
		t.Fatal(err)
	}
	store.Close()

	store = backfillFixture188(t, path, false)
	defer store.Close()
	primeIndexAndGraph188(t, store)
	res, err := store.RunResolvedPathBackfill(context.Background(), 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.From != ids[9] || res.Scanned != 20 || res.Resolved != 20 {
		t.Fatalf("resumed pass = %+v, want from %d, 20 scanned, 20 resolved", res, ids[9])
	}
	if resolvedPathOf188(t, store, ids[0]).Valid {
		t.Fatal("a row below the persisted watermark was scanned again")
	}
	for _, id := range ids[10:] {
		if !resolvedPathOf188(t, store, id).Valid {
			t.Fatalf("row %d above the watermark is still NULL", id)
		}
	}
	if w := persistedWatermark188(t, store); w != ids[29] {
		t.Fatalf("final watermark = %d, want %d", w, ids[29])
	}
}

// Stopping the background pass keeps the last committed watermark.
func TestResolvedPathBackfill_StopKeepsWatermark_188(t *testing.T) {
	store := backfillFixture188(t, filepath.Join(t.TempDir(), "ingest.db"), true)
	defer store.Close()
	ids := seedNullRows188(t, store, 50)
	primeIndexAndGraph188(t, store)
	stop := store.StartResolvedPathBackfill(10, time.Hour) // first batch, then a long pause
	deadline := time.Now().Add(5 * time.Second)
	for {
		var w sql.NullInt64
		store.db.QueryRow(`SELECT watermark FROM resolved_path_backfill_state WHERE id = 1`).Scan(&w)
		if w.Valid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the background pass committed no batch")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	if w := persistedWatermark188(t, store); w != ids[9] {
		t.Fatalf("watermark after stop = %d, want %d (one batch)", w, ids[9])
	}
}

// resolvedPathBackfillHoldBudget is the stated per-batch budget for the write
// transaction at the default batch size (500 rows). It bounds how long a live
// InsertTransmission can wait behind one batch.
const resolvedPathBackfillHoldBudget = 250 * time.Millisecond

func TestResolvedPathBackfill_WriteHoldUnderBudget_188(t *testing.T) {
	store := backfillFixture188(t, filepath.Join(t.TempDir(), "ingest.db"), true)
	defer store.Close()
	seedNullRows188(t, store, 5000)
	primeIndexAndGraph188(t, store)
	res, err := store.RunResolvedPathBackfill(context.Background(), defaultResolvedPathBackfillBatchSize, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Resolved != 5000 || res.Batches != 10 {
		t.Fatalf("pass = %+v, want 5000 resolved in 10 batches", res)
	}
	t.Logf("max write hold per %d-row batch: %s (budget %s)", defaultResolvedPathBackfillBatchSize, res.MaxHold, resolvedPathBackfillHoldBudget)
	if res.MaxHold > resolvedPathBackfillHoldBudget {
		t.Fatalf("max write hold %s exceeds the budget %s", res.MaxHold, resolvedPathBackfillHoldBudget)
	}
}

func TestResolvedPathBackfillSettings_Defaults_188(t *testing.T) {
	var c Config
	if on, b, p := c.ResolvedPathBackfillSettings(); !on || b != 500 || p != 250*time.Millisecond {
		t.Fatalf("defaults = %v, %d, %s", on, b, p)
	}
	c.ResolvedPathBackfill = &ResolvedPathBackfillConfig{BatchSize: 50, PauseMs: 1000}
	if on, b, p := c.ResolvedPathBackfillSettings(); !on || b != 50 || p != time.Second {
		t.Fatalf("overrides = %v, %d, %s", on, b, p)
	}
	c.ResolvedPathBackfill = &ResolvedPathBackfillConfig{Disabled: true}
	if on, _, _ := c.ResolvedPathBackfillSettings(); on {
		t.Fatal("disabled config still enabled")
	}
}
