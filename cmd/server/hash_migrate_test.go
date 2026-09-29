package main

import (
	"reflect"
	"testing"
	"time"
)

func TestMigrateContentHashesAsync(t *testing.T) {
	db := setupTestDBv2(t)
	store := NewPacketStore(db, nil)

	// Insert a packet with a manually wrong hash (simulating old formula).
	rawHex := "0A00D69FD7A5A7475DB07337749AE61FA53A4788E976"
	correctHash := ComputeContentHash(rawHex)
	wrongHash := "deadbeef12345678"

	_, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type)
		VALUES (?, ?, datetime('now'), 0, 2)`, rawHex, wrongHash)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Load(); err != nil {
		t.Fatal(err)
	}

	if store.byHash[wrongHash] == nil {
		t.Fatal("expected packet under wrong hash before migration")
	}

	migrateContentHashesAsync(store, 100, time.Millisecond)

	if !store.hashMigrationComplete.Load() {
		t.Error("expected hashMigrationComplete to be true")
	}
	if store.byHash[wrongHash] != nil {
		t.Error("old hash should be removed from index")
	}
	if store.byHash[correctHash] == nil {
		t.Error("new hash should be in index")
	}

	var dbHash string
	err = db.conn.QueryRow("SELECT hash FROM transmissions WHERE raw_hex = ?", rawHex).Scan(&dbHash)
	if err != nil {
		t.Fatal(err)
	}
	if dbHash != correctHash {
		t.Errorf("DB hash = %s, want %s", dbHash, correctHash)
	}
}

func TestMigrateContentHashesAsync_NoOp(t *testing.T) {
	db := setupTestDBv2(t)
	store := NewPacketStore(db, nil)

	rawHex := "0A00D69FD7A5A7475DB07337749AE61FA53A4788E976"
	correctHash := ComputeContentHash(rawHex)

	_, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type)
		VALUES (?, ?, datetime('now'), 0, 2)`, rawHex, correctHash)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Load(); err != nil {
		t.Fatal(err)
	}

	migrateContentHashesAsync(store, 100, time.Millisecond)

	if !store.hashMigrationComplete.Load() {
		t.Error("expected hashMigrationComplete to be true")
	}
	if store.byHash[correctHash] == nil {
		t.Error("hash should remain in index")
	}
}

func TestMigrateContentHashesAsync_CollisionUnionsObservedPathHashSizesWithoutChangingLegacyIndexes(t *testing.T) {
	db := setupTestDBv2(t)
	store := NewPacketStore(db, nil)

	rawHex := "0A00D69FD7A5A7475DB07337749AE61FA53A4788E976"
	correctHash := ComputeContentHash(rawHex)
	for _, row := range []struct {
		id        int
		wrongHash string
		firstSeen string
		pathJSON  string
	}{
		{1, "old-hash-one", "2026-01-01T00:00:00Z", `["AA"]`},
		{2, "old-hash-two", "2026-01-01T00:00:01Z", `["BEEF"]`},
	} {
		if _, err := db.conn.Exec(`INSERT INTO transmissions
			(id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json)
			VALUES (?, ?, ?, ?, 1, 5, '{}')`, row.id, rawHex, row.wrongHash, row.firstSeen); err != nil {
			t.Fatal(err)
		}
		if _, err := db.conn.Exec(`INSERT INTO observations
			(id, transmission_id, observer_id, observer_name, path_json, timestamp)
			VALUES (?, ?, ?, ?, ?, ?)`, row.id, row.id, "observer", "Observer", row.pathJSON, row.id); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	// One row per batch proves that the survivor's evidence is recovered from
	// byTxID rather than only from same-batch updates.
	migrateContentHashesAsync(store, 1, 0)

	wantSizes := []int{1, 2}
	for _, tx := range store.packets {
		if got := tx.observedPathHashSizes(); !reflect.DeepEqual(got, wantSizes) {
			t.Fatalf("tx %d observed path hash sizes = %v, want %v", tx.ID, got, wantSizes)
		}
	}
	if authoritative := store.byHash[correctHash]; authoritative == nil ||
		!reflect.DeepEqual(authoritative.observedPathHashSizes(), wantSizes) {
		t.Fatalf("authoritative observed path hash sizes = %v, want %v",
			authoritative.observedPathHashSizes(), wantSizes)
	}

	// Characterize, do not silently fix, the pre-existing duplicate-migration
	// index bug. A separate change must remove the deleted row from every
	// PacketStore index and re-parent its observations atomically.
	if len(store.packets) != 2 || len(store.byPayloadType[PayloadGRP_TXT]) != 2 {
		t.Fatalf("legacy in-memory cardinality changed: packets/payload = %d/%d, want 2/2",
			len(store.packets), len(store.byPayloadType[PayloadGRP_TXT]))
	}
	if got := store.QueryPackets(PacketQuery{Limit: 10}).Total; got != 2 {
		t.Fatalf("legacy QueryPackets cardinality changed: got %d, want 2", got)
	}
	if store.byTxID[2] == nil || store.byTxID[2].Observations[0].TransmissionID != 2 {
		t.Fatal("legacy duplicate/observation ownership changed unexpectedly")
	}

	var txCount, canonicalObservationCount int
	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM transmissions WHERE raw_hex = ?`, rawHex).Scan(&txCount); err != nil {
		t.Fatal(err)
	}
	if txCount != 1 {
		t.Fatalf("DB transmission rows = %d, want 1 after duplicate merge", txCount)
	}
	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM observations WHERE transmission_id = 1`).Scan(&canonicalObservationCount); err != nil {
		t.Fatal(err)
	}
	if canonicalObservationCount != 2 {
		t.Fatalf("DB canonical observations = %d, want 2 after duplicate merge", canonicalObservationCount)
	}
}

func TestMigrateContentHashesAsync_CollisionIncludesAlreadyCurrentSurvivorEvidence(t *testing.T) {
	db := setupTestDBv2(t)
	store := NewPacketStore(db, nil)

	rawHex := "0A00D69FD7A5A7475DB07337749AE61FA53A4788E976"
	correctHash := ComputeContentHash(rawHex)
	for _, row := range []struct {
		id       int
		hash     string
		pathJSON string
	}{
		{1, correctHash, `["010203"]`},
		{2, "old-hash", `["BEEF"]`},
	} {
		if _, err := db.conn.Exec(`INSERT INTO transmissions
			(id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json)
			VALUES (?, ?, ?, ?, 1, 5, '{}')`, row.id, rawHex, row.hash,
			time.Date(2026, 1, 1, 0, 0, row.id, 0, time.UTC).Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.conn.Exec(`INSERT INTO observations
			(id, transmission_id, observer_id, observer_name, path_json, timestamp)
			VALUES (?, ?, ?, ?, ?, ?)`, row.id, row.id, "observer", "Observer", row.pathJSON, row.id); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	migrateContentHashesAsync(store, 100, 0)

	want := []int{2, 3}
	for _, tx := range store.packets {
		if got := tx.observedPathHashSizes(); !reflect.DeepEqual(got, want) {
			t.Fatalf("tx %d observed path hash sizes = %v, want %v", tx.ID, got, want)
		}
	}
	if got := store.byHash[correctHash].observedPathHashSizes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("authoritative observed path hash sizes = %v, want %v", got, want)
	}
	if len(store.packets) != 2 || store.QueryPackets(PacketQuery{Limit: 10}).Total != 2 {
		t.Fatal("legacy in-memory duplicate cardinality changed unexpectedly")
	}
}

func TestMigrateContentHashesAsync_CollisionCarriesEvidenceAcrossBatches(t *testing.T) {
	db := setupTestDBv2(t)
	store := NewPacketStore(db, nil)

	rawHex := "0A00D69FD7A5A7475DB07337749AE61FA53A4788E976"
	correctHash := ComputeContentHash(rawHex)
	for _, row := range []struct {
		id        int
		wrongHash string
		pathJSON  string
	}{
		{1, "old-hash-one", `["AA"]`},
		{2, "old-hash-two", `["BEEF"]`},
		{3, "old-hash-three", `["010203"]`},
	} {
		if _, err := db.conn.Exec(`INSERT INTO transmissions
			(id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json)
			VALUES (?, ?, ?, ?, 1, 5, '{}')`, row.id, rawHex, row.wrongHash,
			time.Date(2026, 1, 1, 0, 0, row.id, 0, time.UTC).Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.conn.Exec(`INSERT INTO observations
			(id, transmission_id, observer_id, observer_name, path_json, timestamp)
			VALUES (?, ?, ?, ?, ?, ?)`, row.id, row.id, "observer", "Observer", row.pathJSON, row.id); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	// Force each collision into a different transaction/batch. Evidence learned
	// by the third collision must flow back into the ghost row retained by the
	// second collision's legacy in-memory behaviour.
	migrateContentHashesAsync(store, 1, 0)

	want := []int{1, 2, 3}
	for _, tx := range store.packets {
		if got := tx.observedPathHashSizes(); !reflect.DeepEqual(got, want) {
			t.Fatalf("tx %d observed path hash sizes = %v, want %v", tx.ID, got, want)
		}
	}
	if got := store.byHash[correctHash].observedPathHashSizes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("authoritative observed path hash sizes = %v, want %v", got, want)
	}

	// Keep characterizing the separate legacy index bug rather than hiding it
	// inside this evidence-only feature change.
	if len(store.packets) != 3 || len(store.byPayloadType[PayloadGRP_TXT]) != 3 {
		t.Fatalf("legacy in-memory cardinality changed: packets/payload = %d/%d, want 3/3",
			len(store.packets), len(store.byPayloadType[PayloadGRP_TXT]))
	}
	if got := store.QueryPackets(PacketQuery{Limit: 10}).Total; got != 3 {
		t.Fatalf("legacy QueryPackets cardinality changed: got %d, want 3", got)
	}
	for _, duplicateID := range []int{2, 3} {
		duplicate := store.byTxID[duplicateID]
		if duplicate == nil || len(duplicate.Observations) != 1 ||
			duplicate.Observations[0].TransmissionID != duplicateID {
			t.Fatalf("legacy duplicate %d observation ownership changed unexpectedly", duplicateID)
		}
	}

	var txCount, canonicalObservationCount int
	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM transmissions WHERE raw_hex = ?`, rawHex).Scan(&txCount); err != nil {
		t.Fatal(err)
	}
	if txCount != 1 {
		t.Fatalf("DB transmission rows = %d, want 1 after duplicate merges", txCount)
	}
	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM observations WHERE transmission_id = 1`).Scan(&canonicalObservationCount); err != nil {
		t.Fatal(err)
	}
	if canonicalObservationCount != 3 {
		t.Fatalf("DB canonical observations = %d, want 3 after duplicate merges", canonicalObservationCount)
	}
}
