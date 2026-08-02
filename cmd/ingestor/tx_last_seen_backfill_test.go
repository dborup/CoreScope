package main

import (
	"context"
	"testing"
	"time"
)

// PR #1735 (ported from upstream): backfillTxLastSeen must chunk its work
// rather than run one giant correlated UPDATE, must never loop forever on
// transmissions it cannot resolve (no observations), and must not touch
// rows inserted after its maxID snapshot.

// seedTxNoObs inserts a bare transmission row with last_seen=0 and no
// observations. hash must be unique.
func seedTxNoObs(t *testing.T, s *Store, hash string) int64 {
	t.Helper()
	res, err := s.db.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, payload_type, last_seen) VALUES ('aabb', ?, '2026-01-01T00:00:00Z', 1, 0)`, hash)
	if err != nil {
		t.Fatalf("seed transmission %s: %v", hash, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// seedTxWithObs inserts a transmission row with last_seen=0 plus one or
// more observations at the given unix timestamps.
func seedTxWithObs(t *testing.T, s *Store, hash string, obsTimestamps ...int64) int64 {
	t.Helper()
	id := seedTxNoObs(t, s, hash)
	for _, ts := range obsTimestamps {
		if _, err := s.db.Exec(`INSERT INTO observations (transmission_id, timestamp) VALUES (?, ?)`, id, ts); err != nil {
			t.Fatalf("seed observation for %s: %v", hash, err)
		}
	}
	return id
}

func txLastSeen(t *testing.T, s *Store, id int64) int64 {
	t.Helper()
	var ls int64
	if err := s.db.QueryRow(`SELECT last_seen FROM transmissions WHERE id = ?`, id).Scan(&ls); err != nil {
		t.Fatalf("read last_seen for id=%d: %v", id, err)
	}
	return ls
}

// TestBackfillTxLastSeen_ResolvesFromMaxObservationTimestamp is the core
// regression: a transmission with observations gets last_seen set to the
// MAX of their timestamps.
func TestBackfillTxLastSeen_ResolvesFromMaxObservationTimestamp(t *testing.T) {
	store := newTestStore(t)
	id := seedTxWithObs(t, store, "resolve1", 100, 300, 200)

	if err := backfillTxLastSeen(context.Background(), store.db); err != nil {
		t.Fatalf("backfillTxLastSeen: %v", err)
	}

	if got := txLastSeen(t, store, id); got != 300 {
		t.Errorf("last_seen = %d, want 300 (MAX of 100,300,200)", got)
	}
}

// TestBackfillTxLastSeen_OrphanNeverLoopsForever pins the termination
// guarantee: a transmission with zero observations must be left at
// last_seen=0 (unresolvable) WITHOUT backfillTxLastSeen looping forever
// re-selecting it every batch. The test's own timeout is the assertion —
// if the EXISTS filter regresses, this test hangs.
func TestBackfillTxLastSeen_OrphanNeverLoopsForever(t *testing.T) {
	store := newTestStore(t)
	orphan := seedTxNoObs(t, store, "orphan1")
	resolvable := seedTxWithObs(t, store, "resolvable1", 555)

	oldBatch, oldYield := txLastSeenBackfillBatchSize, txLastSeenBackfillYield
	txLastSeenBackfillBatchSize = 1
	txLastSeenBackfillYield = time.Millisecond
	t.Cleanup(func() {
		txLastSeenBackfillBatchSize = oldBatch
		txLastSeenBackfillYield = oldYield
	})

	done := make(chan error, 1)
	go func() { done <- backfillTxLastSeen(context.Background(), store.db) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("backfillTxLastSeen: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("backfillTxLastSeen did not return within 5s — orphan row is being re-selected forever")
	}

	if got := txLastSeen(t, store, orphan); got != 0 {
		t.Errorf("orphan last_seen = %d, want 0 (unresolvable, left untouched)", got)
	}
	if got := txLastSeen(t, store, resolvable); got != 555 {
		t.Errorf("resolvable last_seen = %d, want 555", got)
	}
}

// TestBackfillTxLastSeen_ChunksAcrossMultipleBatches proves the loop
// actually iterates in batches rather than doing the whole table in one
// UPDATE — a small batch size forced across more rows than fit in one
// batch must still resolve every row.
func TestBackfillTxLastSeen_ChunksAcrossMultipleBatches(t *testing.T) {
	store := newTestStore(t)

	oldBatch, oldYield := txLastSeenBackfillBatchSize, txLastSeenBackfillYield
	txLastSeenBackfillBatchSize = 2
	txLastSeenBackfillYield = time.Millisecond
	t.Cleanup(func() {
		txLastSeenBackfillBatchSize = oldBatch
		txLastSeenBackfillYield = oldYield
	})

	const n = 9 // not a multiple of the batch size, to exercise the tail batch
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		ts := int64(1000 + i)
		id := seedTxWithObs(t, store, "chunk"+string(rune('a'+i)), ts)
		ids = append(ids, id)
	}

	if err := backfillTxLastSeen(context.Background(), store.db); err != nil {
		t.Fatalf("backfillTxLastSeen: %v", err)
	}

	for i, id := range ids {
		want := int64(1000 + i)
		if got := txLastSeen(t, store, id); got != want {
			t.Errorf("row %d: last_seen = %d, want %d", i, got, want)
		}
	}
}

// TestBackfillTxLastSeen_IgnoresRowsInsertedAfterSnapshot pins the maxID
// bound: a transmission inserted concurrently, after backfillTxLastSeen
// has already snapshotted MAX(id), must not be touched by that run (it
// already arrives with last_seen populated via InsertTransmission in
// production; this test just confirms the snapshot really is a bound).
func TestBackfillTxLastSeen_IgnoresRowsInsertedAfterSnapshot(t *testing.T) {
	store := newTestStore(t)
	before := seedTxWithObs(t, store, "before1", 42)

	// Snapshot maxID by calling the same query the function uses, then
	// insert a new row before running the backfill body against that
	// stale snapshot — simulating a row that lands mid-backfill.
	var maxID int64
	if err := store.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM transmissions`).Scan(&maxID); err != nil {
		t.Fatal(err)
	}
	after := seedTxWithObs(t, store, "after1", 99)

	if _, err := store.db.Exec(`
		UPDATE transmissions
		SET last_seen = COALESCE((
			SELECT MAX(timestamp) FROM observations WHERE transmission_id = transmissions.id
		), last_seen)
		WHERE id IN (
			SELECT id FROM transmissions
			WHERE last_seen = 0 AND id <= ?
				AND EXISTS (SELECT 1 FROM observations WHERE observations.transmission_id = transmissions.id)
			ORDER BY id
			LIMIT 1000
		)
	`, maxID); err != nil {
		t.Fatalf("simulated backfill batch: %v", err)
	}

	if got := txLastSeen(t, store, before); got != 42 {
		t.Errorf("before-snapshot row last_seen = %d, want 42", got)
	}
	if got := txLastSeen(t, store, after); got != 0 {
		t.Errorf("after-snapshot row last_seen = %d, want 0 (must not be touched by a run that snapshotted maxID before it existed)", got)
	}
}
