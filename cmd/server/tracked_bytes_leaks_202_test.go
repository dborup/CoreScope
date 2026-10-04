package main

import (
	"testing"
	"time"
)

// Issue #202: two accounting leaks left after #198.
//
// Every test asserts against a live "ballast" transmission that is never
// evicted, so a double credit shows up as a number below the ballast's
// charge instead of being hidden by the clamp at zero in the eviction pass.

// leak202Ballast inserts one recent, unrelated transmission with one
// observation. It only has to stay live.
func leak202Ballast(t *testing.T, db *DB) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.conn.Exec(`INSERT INTO transmissions
		(id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json)
		VALUES (900, '1500aabbccdd', 'ballast0000000001', ?, 1, 5, '{"type":"GRP_TXT","text":"ballast"}')`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.Exec(`INSERT INTO observations
		(id, transmission_id, observer_id, observer_name, path_json, timestamp)
		VALUES (900, 900, 'observer', 'Observer', '["aa"]', ?)`, now); err != nil {
		t.Fatal(err)
	}
}

// leak202LiveCharge is the charge of everything still in the store.
func leak202LiveCharge(s *PacketStore) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sum, _ := acct113Sum(s)
	return sum
}

// leak202TxCharge is everything trackedBytes holds for one transmission.
func leak202TxCharge(t *testing.T, s *PacketStore, id int) int64 {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	tx := s.byTxID[id]
	if tx == nil {
		t.Fatalf("tx %d not in the store", id)
	}
	sum := s.txChargedBytes(tx)
	for _, o := range tx.Observations {
		sum += estimateStoreObsBytes(o)
	}
	return sum
}

// A hash migration that merges two duplicate transmissions must credit each
// observation exactly once when the store later evicts them.
func TestHashMigrationMerge_EvictionCreditsEveryObservationOnce_202(t *testing.T) {
	db := setupTestDBv2(t)
	store := NewPacketStore(db, nil)

	rawHex := "0A00D69FD7A5A7475DB07337749AE61FA53A4788E976"
	rows := []struct {
		id        int
		oldHash   string
		firstSeen string
		paths     []string // one observation per entry
	}{
		{1, "old-hash-one", "2026-01-01T00:00:00Z", []string{`["AA"]`, `["AA","BB"]`}},
		{2, "old-hash-two", "2026-01-01T00:00:01Z", []string{`["CC"]`, `["CC","DD","EE"]`, `["FF"]`}},
	}
	obsID := 0
	for _, r := range rows {
		if _, err := db.conn.Exec(`INSERT INTO transmissions
			(id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json)
			VALUES (?, ?, ?, ?, 1, 5, '{}')`, r.id, rawHex, r.oldHash, r.firstSeen); err != nil {
			t.Fatal(err)
		}
		for _, p := range r.paths {
			obsID++
			if _, err := db.conn.Exec(`INSERT INTO observations
				(id, transmission_id, observer_id, observer_name, path_json, timestamp)
				VALUES (?, ?, 'observer', 'Observer', ?, ?)`, obsID, r.id, p, r.firstSeen); err != nil {
				t.Fatal(err)
			}
		}
	}
	leak202Ballast(t, db)

	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	if !store.WaitIndexesReady(30 * time.Second) {
		t.Fatal("background index builds did not finish")
	}
	ballast := leak202TxCharge(t, store, 900)

	migrateContentHashesAsync(store, 1, 0)
	if !store.hashMigrationComplete.Load() {
		t.Fatal("hash migration did not complete")
	}
	var dbTx int
	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM transmissions WHERE raw_hex = ?`, rawHex).Scan(&dbTx); err != nil {
		t.Fatal(err)
	}
	if dbTx != 1 {
		t.Fatalf("setup: %d duplicate rows left in the DB, want the merge to leave 1", dbTx)
	}

	// Whatever the merge did in memory, trackedBytes must still equal what the
	// store holds, before anything is evicted.
	if got, want := store.trackedBytes, leak202LiveCharge(store); got != want {
		t.Fatalf("after the merge trackedBytes = %d, live charges sum to %d (drift %d)", got, want, got-want)
	}

	store.mu.Lock()
	store.retentionHours = 24
	store.mu.Unlock()
	if n := store.RunEviction(); n == 0 {
		t.Fatal("setup: eviction removed nothing")
	}

	store.mu.RLock()
	got, live := store.trackedBytes, len(store.packets)
	store.mu.RUnlock()
	if live != 1 {
		t.Fatalf("setup: %d transmissions live after eviction, want only the ballast", live)
	}
	if got != ballast {
		t.Fatalf("after evicting both duplicates trackedBytes = %d, want exactly the ballast's %d (drift %d)",
			got, ballast, got-ballast)
	}
}
