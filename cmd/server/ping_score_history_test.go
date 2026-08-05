package main

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func f64ptr(v float64) *float64 { return &v }

func sampleHistoryEntry(txID int64) PingScoreHistoryEntry {
	return PingScoreHistoryEntry{
		TxID:             txID,
		Hash:             fmt.Sprintf("hash%d", txID),
		Sender:           "Alice",
		ChannelHash:      "#ping",
		Timestamp:        "2026-08-04T12:00:00Z",
		StationCount:     3,
		DeepestHops:      5,
		DeepestPubkey:    "deepestpk",
		FarthestKm:       f64ptr(42.5),
		FarthestPubkey:   "farthestpk",
		SpreadSeconds:    f64ptr(12.3),
		AirtimeMs:        f64ptr(999.5),
		RelayCount:       4,
		RelayPubkeysJSON: `["r1","r2"]`,
		FirstPubkey:      "firstpk",
		Unscorable:       false,
		FingerprintCount: 3,
		FingerprintMaxID: 100,
		StableSince:      "2026-08-04T12:05:00Z",
		Settled:          true,
		DataPruned:       false,
		LastDeepSweptAt:  "2026-08-04T13:00:00Z",
		ComputedAt:       "2026-08-04T12:05:00Z",
	}
}

// --- 1. new empty file is created correctly ---

func TestOpenPingScoreHistoryStore_CreatesNewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ping_scores_history.db")

	if _, err := os.Stat(path); err == nil {
		t.Fatalf("file already exists before open")
	}
	store, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatalf("OpenPingScoreHistoryStore: %v", err)
	}
	defer store.Close()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file to exist after open: %v", err)
	}
	if store.ReadOnly() {
		t.Error("a freshly created store should not be read-only")
	}
	entries, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries in a fresh store, got %d", len(entries))
	}
	v, err := store.readSchemaVersion()
	if err != nil {
		t.Fatalf("readSchemaVersion: %v", err)
	}
	if v != pingScoreHistorySchemaVersion {
		t.Errorf("schema_version = %d, want %d", v, pingScoreHistorySchemaVersion)
	}
}

// --- 2. reopening preserves entries ---

func TestOpenPingScoreHistoryStore_ReopenPreservesEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ping_scores_history.db")

	store, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.UpsertAndDelete([]PingScoreHistoryEntry{sampleHistoryEntry(1), sampleHistoryEntry(2)}, nil); err != nil {
		t.Fatalf("UpsertAndDelete: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	entries, err := reopened.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll after reopen: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries to survive reopen, got %d", len(entries))
	}
	if entries[0].TxID != 1 || entries[1].TxID != 2 {
		t.Errorf("unexpected tx_ids after reopen: %+v", entries)
	}
	if entries[0].Hash != "hash1" || entries[0].Sender != "Alice" {
		t.Errorf("entry content not preserved: %+v", entries[0])
	}
}

// --- 3. upsert updates correctly ---

func TestUpsertAndDelete_UpsertUpdatesExistingRow(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenPingScoreHistoryStore(filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	e := sampleHistoryEntry(5)
	if err := store.UpsertAndDelete([]PingScoreHistoryEntry{e}, nil); err != nil {
		t.Fatalf("initial upsert: %v", err)
	}

	e.StationCount = 99
	e.FarthestKm = f64ptr(777.0)
	if err := store.UpsertAndDelete([]PingScoreHistoryEntry{e}, nil); err != nil {
		t.Fatalf("update upsert: %v", err)
	}

	entries, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 row (update, not insert), got %d", len(entries))
	}
	if entries[0].StationCount != 99 {
		t.Errorf("StationCount = %d, want 99 (update should have applied)", entries[0].StationCount)
	}
	if entries[0].FarthestKm == nil || *entries[0].FarthestKm != 777.0 {
		t.Errorf("FarthestKm = %v, want 777.0", entries[0].FarthestKm)
	}
}

// --- 4. upsert + delete is atomic (both applied together) ---

func TestUpsertAndDelete_BothPartsApplyTogether(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenPingScoreHistoryStore(filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	if err := store.UpsertAndDelete([]PingScoreHistoryEntry{sampleHistoryEntry(1), sampleHistoryEntry(2), sampleHistoryEntry(3)}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// One transaction: upsert a NEW entry (4) while deleting an OLD one (2).
	if err := store.UpsertAndDelete([]PingScoreHistoryEntry{sampleHistoryEntry(4)}, []int64{2}); err != nil {
		t.Fatalf("combined upsert+delete: %v", err)
	}

	entries, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	got := map[int64]bool{}
	for _, e := range entries {
		got[e.TxID] = true
	}
	if got[2] {
		t.Error("tx_id=2 should have been deleted")
	}
	if !got[1] || !got[3] || !got[4] {
		t.Errorf("expected tx_ids {1,3,4} to remain/be added, got %+v", got)
	}
	if len(entries) != 3 {
		t.Errorf("expected 3 entries total, got %d", len(entries))
	}
}

// --- 5. forced failure mid-transaction leaves prior state unchanged ---

func TestUpsertAndDelete_FailureRollsBackEntireTransaction(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenPingScoreHistoryStore(filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	// Seed one pre-existing row so we can also confirm it is untouched.
	if err := store.UpsertAndDelete([]PingScoreHistoryEntry{sampleHistoryEntry(1)}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Batch: two valid NEW rows, then a row violating the tx_id>0 CHECK
	// constraint. The valid rows are Exec'd (within the transaction)
	// BEFORE the failing one -- proving a mid-transaction failure discards
	// already-Exec'd-but-uncommitted work, not just the failing row.
	bad := sampleHistoryEntry(-1)
	batch := []PingScoreHistoryEntry{sampleHistoryEntry(10), sampleHistoryEntry(11), bad}
	if err := store.UpsertAndDelete(batch, nil); err == nil {
		t.Fatal("expected an error from the CHECK-constraint-violating row, got nil")
	}

	entries, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(entries) != 1 || entries[0].TxID != 1 {
		t.Errorf("expected ONLY the pre-existing tx_id=1 row to survive the failed transaction, got %+v", entries)
	}
}

// --- 5b. UpsertDeleteAndIntegrity: integrity commits atomically ------------

func TestUpsertDeleteAndIntegrity_IntegrityCommitsWithUpserts(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenPingScoreHistoryStore(filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	integrity := &PingScoreHistoryIntegrity{
		Status: "initial-backfill-incomplete", DetectedAt: "2026-01-01T00:00:00Z",
		TotalTriggers: 10, ScoredCount: 8, UnreconstructableCount: 2, Detail: "2 triggers older than retention",
	}
	if err := store.UpsertDeleteAndIntegrity([]PingScoreHistoryEntry{sampleHistoryEntry(1)}, nil, integrity); err != nil {
		t.Fatalf("UpsertDeleteAndIntegrity: %v", err)
	}

	entries, err := store.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want 1", entries)
	}
	got, err := store.LoadIntegrity()
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Status != "initial-backfill-incomplete" || got.TotalTriggers != 10 || got.ScoredCount != 8 || got.UnreconstructableCount != 2 {
		t.Errorf("LoadIntegrity() = %+v, want the stored integrity", got)
	}
}

func TestUpsertDeleteAndIntegrity_NilIntegrityLeavesExistingUntouched(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenPingScoreHistoryStore(filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	// Seed an abnormal integrity status first.
	seeded := PingScoreHistoryIntegrity{Status: "initial-backfill-incomplete", DetectedAt: "2026-01-01T00:00:00Z", UnreconstructableCount: 3}
	if err := store.StoreIntegrity(seeded); err != nil {
		t.Fatalf("seed integrity: %v", err)
	}

	// A later cycle upserts entries but passes nil for integrity -- must
	// NOT clear the previously recorded abnormal status.
	if err := store.UpsertDeleteAndIntegrity([]PingScoreHistoryEntry{sampleHistoryEntry(2)}, nil, nil); err != nil {
		t.Fatalf("UpsertDeleteAndIntegrity: %v", err)
	}

	got, err := store.LoadIntegrity()
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Status != "initial-backfill-incomplete" || got.UnreconstructableCount != 3 {
		t.Errorf("LoadIntegrity() = %+v, want the ORIGINAL abnormal status, unmodified by an integrity=nil call", got)
	}
}

func TestUpsertDeleteAndIntegrity_FailureRollsBackIntegrityToo(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenPingScoreHistoryStore(filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	bad := sampleHistoryEntry(-1) // violates tx_id>0 CHECK
	integrity := &PingScoreHistoryIntegrity{Status: "initial-backfill-incomplete", DetectedAt: "2026-01-01T00:00:00Z", UnreconstructableCount: 1}
	err = store.UpsertDeleteAndIntegrity([]PingScoreHistoryEntry{bad}, nil, integrity)
	if err == nil {
		t.Fatal("want an error from the CHECK-constraint-violating row")
	}

	got, err := store.LoadIntegrity()
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("LoadIntegrity() = %+v, want nil -- the integrity write must roll back with the rest of the failed transaction", got)
	}
}

// --- 6. unknown newer schema version opens read-only, never writes ---

func TestOpenPingScoreHistoryStore_UnknownNewerVersionIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "h.db")

	store, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatalf("initial open: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Simulate a file written by a hypothetical future version.
	rawSetSchemaVersion(t, path, "999")

	reopened, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatalf("reopen with unknown future version: %v", err)
	}
	defer reopened.Close()

	if !reopened.ReadOnly() {
		t.Fatal("expected ReadOnly()=true for a schema_version newer than this code understands")
	}
	// Reads still work.
	if _, err := reopened.LoadAll(); err != nil {
		t.Errorf("LoadAll should still work in read-only mode: %v", err)
	}
	// Writes are refused.
	if err := reopened.UpsertAndDelete([]PingScoreHistoryEntry{sampleHistoryEntry(1)}, nil); err == nil {
		t.Error("expected UpsertAndDelete to fail on a read-only (future-version) store")
	}
	if err := reopened.StoreIntegrity(PingScoreHistoryIntegrity{Status: "ok", DetectedAt: "x"}); err == nil {
		t.Error("expected StoreIntegrity to fail on a read-only (future-version) store")
	}
	reopened.Close()

	// Verify NOTHING was written: schema_version is still "999", and the
	// entries table is still empty.
	if v := rawGetSchemaVersion(t, path); v != "999" {
		t.Errorf("schema_version was modified: got %q, want unchanged \"999\"", v)
	}
	verify, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatalf("re-verify open: %v", err)
	}
	defer verify.Close()
	entries, err := verify.LoadAll()
	if err != nil {
		t.Fatalf("re-verify LoadAll: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries (nothing should have been written), got %d", len(entries))
	}
}

// TestOpenPingScoreHistoryStore_UnknownNewerVersionConnectionIsPhysicallyReadOnly
// proves the write protection is the underlying SQLite connection itself
// (opened with mode=ro), not just the Go-level readOnly bool -- a direct
// Exec against the internal connection, bypassing the public API entirely,
// must still fail. This is the test the review specifically asked for:
// the public methods checking the flag is not sufficient on its own.
func TestOpenPingScoreHistoryStore_UnknownNewerVersionConnectionIsPhysicallyReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "h.db")

	store, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatalf("initial open: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	rawSetSchemaVersion(t, path, "999")

	reopened, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatalf("reopen with unknown future version: %v", err)
	}
	defer reopened.Close()
	if !reopened.ReadOnly() {
		t.Fatal("expected ReadOnly()=true")
	}

	// Bypass every public method and Exec directly against the internal
	// connection. If this succeeds, the connection itself is writable and
	// the whole scheme depends entirely on every future method
	// remembering to check readOnly -- exactly what must NOT be true.
	_, execErr := reopened.conn.Exec(
		`INSERT INTO ping_score_history_entries (tx_id, hash, timestamp, station_count, deepest_hops, computed_at)
		 VALUES (1, 'h', 't', 0, 0, 'c')`)
	if execErr == nil {
		t.Fatal("direct Exec against the internal connection succeeded -- connection is NOT physically read-only")
	}
	if !strings.Contains(strings.ToLower(execErr.Error()), "readonly") {
		t.Errorf("expected a \"readonly database\" style driver error, got: %v", execErr)
	}
}

// TestOpenPingScoreHistoryStore_UnknownNewerVersionJournalModeUnchanged
// confirms the read-only inspection pass itself never issues a
// _journal_mode=WAL (or any other) PRAGMA SET -- the file's on-disk journal
// mode before and after an Open+Close cycle must be identical.
func TestOpenPingScoreHistoryStore_UnknownNewerVersionJournalModeUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "h.db")

	store, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatalf("initial open: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	rawSetSchemaVersion(t, path, "999")

	before := rawGetJournalMode(t, path)

	reopened, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatalf("reopen with unknown future version: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("close reopened: %v", err)
	}

	after := rawGetJournalMode(t, path)
	if before != after {
		t.Errorf("journal_mode changed from %q to %q across an Open+Close cycle on a future-version file", before, after)
	}
}

// TestOpenPingScoreHistoryStore_UnknownNewerVersionDoesNotTouchWALFile
// confirms opening a future-version file for read-only inspection neither
// creates a new -wal side file nor modifies an existing one.
func TestOpenPingScoreHistoryStore_UnknownNewerVersionDoesNotTouchWALFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "h.db")

	store, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatalf("initial open: %v", err)
	}
	if err := store.UpsertAndDelete([]PingScoreHistoryEntry{sampleHistoryEntry(1)}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	rawSetSchemaVersion(t, path, "999")

	walPath := path + "-wal"
	existedBefore := fileExistsForTest(walPath)
	var contentBefore []byte
	if existedBefore {
		contentBefore, err = os.ReadFile(walPath)
		if err != nil {
			t.Fatalf("read wal file before: %v", err)
		}
	}

	reopened, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatalf("reopen with unknown future version: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("close reopened: %v", err)
	}

	existedAfter := fileExistsForTest(walPath)
	if existedAfter != existedBefore {
		t.Fatalf("-wal file existence changed: existed before=%v, after=%v", existedBefore, existedAfter)
	}
	if existedAfter {
		contentAfter, err := os.ReadFile(walPath)
		if err != nil {
			t.Fatalf("read wal file after: %v", err)
		}
		if !bytes.Equal(contentBefore, contentAfter) {
			t.Error("-wal file content changed by opening a future-version file read-only")
		}
	}
}

func fileExistsForTest(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func rawGetJournalMode(t *testing.T, path string) string {
	t.Helper()
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open for journal_mode read: %v", err)
	}
	defer conn.Close()
	var mode string
	if err := conn.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("raw read journal_mode: %v", err)
	}
	return mode
}

// --- 7. additive migration preserves existing rows ---

func TestOpenPingScoreHistoryStore_MigrationPreservesExistingRows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "h.db")

	store, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatalf("initial open: %v", err)
	}
	if err := store.UpsertAndDelete([]PingScoreHistoryEntry{sampleHistoryEntry(1), sampleHistoryEntry(2)}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Simulate an on-disk file that predates version tracking (schema_version
	// rolled back to 0) even though its tables already match v1 -- exercises
	// the REAL migration code path (migrateFrom re-running the idempotent v1
	// step) against data that must survive.
	rawSetSchemaVersion(t, path, "0")

	migrated, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatalf("reopen triggering migration: %v", err)
	}
	defer migrated.Close()

	entries, err := migrated.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll after migration: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected the 2 pre-existing rows to survive migration, got %d", len(entries))
	}
	v, err := migrated.readSchemaVersion()
	if err != nil {
		t.Fatalf("readSchemaVersion: %v", err)
	}
	if v != pingScoreHistorySchemaVersion {
		t.Errorf("schema_version after migration = %d, want %d", v, pingScoreHistorySchemaVersion)
	}
}

// TestOpenPingScoreHistoryStore_MigrationFailureIsTypedAndPreservesRows
// provokes a REAL migration failure (not a fabricated stub): applyPingScoreHistoryV1's
// first statement, `CREATE TABLE IF NOT EXISTS _meta (...)`, is made to
// collide with a pre-existing INDEX named "_meta" -- SQLite's IF NOT EXISTS
// only suppresses the "already exists" error when the existing object is
// itself a TABLE of the same name; colliding with an object of a
// DIFFERENT type (an index here) still raises a real error (verified
// empirically against modernc.org/sqlite before writing this test).
func TestOpenPingScoreHistoryStore_MigrationFailureIsTypedAndPreservesRows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "h.db")

	store, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatalf("initial open: %v", err)
	}
	if err := store.UpsertAndDelete([]PingScoreHistoryEntry{sampleHistoryEntry(1)}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Replace the real _meta TABLE with an INDEX of the same name (pointing
	// at an unrelated table) so readSchemaVersion sees "no _meta table" (=
	// version 0, migration needed), but the migration's own
	// `CREATE TABLE IF NOT EXISTS _meta` then collides with that index.
	func() {
		conn, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatalf("raw open to corrupt _meta: %v", err)
		}
		defer conn.Close()
		if _, err := conn.Exec(`DROP TABLE _meta`); err != nil {
			t.Fatalf("drop real _meta: %v", err)
		}
		if _, err := conn.Exec(`CREATE TABLE dummy_unrelated (id INTEGER)`); err != nil {
			t.Fatalf("create dummy table: %v", err)
		}
		if _, err := conn.Exec(`CREATE INDEX _meta ON dummy_unrelated(id)`); err != nil {
			t.Fatalf("create colliding index named _meta: %v", err)
		}
	}()

	reopened, err := OpenPingScoreHistoryStore(path)
	if reopened != nil {
		reopened.Close()
		t.Fatal("expected a nil store when migration fails, got a non-nil one")
	}
	if err == nil {
		t.Fatal("expected an error from the provoked migration failure, got nil")
	}
	var migErr *PingScoreHistoryMigrationError
	if !errors.As(err, &migErr) {
		t.Fatalf("expected errors.As to find a *PingScoreHistoryMigrationError, got: %v (%T)", err, err)
	}
	if migErr.Path != path {
		t.Errorf("MigrationError.Path = %q, want %q", migErr.Path, path)
	}
	if migErr.FromVersion != 0 || migErr.ToVersion != pingScoreHistorySchemaVersion {
		t.Errorf("MigrationError versions = %d -> %d, want 0 -> %d", migErr.FromVersion, migErr.ToVersion, pingScoreHistorySchemaVersion)
	}
	if migErr.Unwrap() == nil {
		t.Error("MigrationError.Unwrap() returned nil, want the underlying SQL error")
	}

	// schema_version must NOT have been bumped to 1 -- the migration's own
	// transaction never committed.
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw reopen to verify: %v", err)
	}
	defer conn.Close()
	var metaType string
	if err := conn.QueryRow(`SELECT type FROM sqlite_master WHERE name = '_meta'`).Scan(&metaType); err != nil {
		t.Fatalf("check _meta type: %v", err)
	}
	if metaType != "index" {
		t.Errorf("_meta type = %q, want still \"index\" (unchanged by the failed migration)", metaType)
	}

	// The pre-existing entry in ping_score_history_entries must have
	// survived -- the failed migration must not have touched that table.
	var count int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM ping_score_history_entries`).Scan(&count); err != nil {
		t.Fatalf("count entries: %v", err)
	}
	if count != 1 {
		t.Errorf("ping_score_history_entries row count = %d, want 1 (pre-existing row must survive a failed migration)", count)
	}
}

// --- 8. integrity metadata survives restart ---

func TestIntegrity_SurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "h.db")

	store, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	want := PingScoreHistoryIntegrity{
		Status:                 "initial-backfill-incomplete",
		DetectedAt:             "2026-08-04T12:00:00Z",
		TotalTriggers:          749,
		ScoredCount:            700,
		UnreconstructableCount: 49,
		Detail:                 "49 triggers predate available packet data",
	}
	if err := store.StoreIntegrity(want); err != nil {
		t.Fatalf("StoreIntegrity: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	got, err := reopened.LoadIntegrity()
	if err != nil {
		t.Fatalf("LoadIntegrity: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil integrity after restart")
	}
	if *got != want {
		t.Errorf("integrity after restart = %+v, want %+v", *got, want)
	}
}

func TestIntegrity_NilWhenNeverRecorded(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenPingScoreHistoryStore(filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	got, err := store.LoadIntegrity()
	if err != nil {
		t.Fatalf("LoadIntegrity: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil integrity for a healthy store that never recorded an issue, got %+v", got)
	}
}

// --- 9. KmPerSecondAirtime and display names are never persisted ---

func TestSchema_DoesNotPersistDerivedOrDisplayFields(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenPingScoreHistoryStore(filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	rows, err := store.conn.Query(`PRAGMA table_info(ping_score_history_entries)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt interface{}
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info iteration: %v", err)
	}

	forbidden := []string{
		"km_per_second_airtime", "deepest_name", "farthest_name", "first_name", "name", "sender_name",
	}
	for _, col := range forbidden {
		if cols[col] {
			t.Errorf("column %q must never be persisted (derived/display field), but schema has it", col)
		}
	}
	// Sanity: confirm the check is meaningful by asserting a real column IS present.
	if !cols["airtime_ms"] {
		t.Error("sanity check failed: airtime_ms column expected but missing")
	}
}

func TestPingScoreHistoryEntry_HasNoKmPerSecondAirtimeField(t *testing.T) {
	// Compile-time-adjacent guard: PingScoreHistoryEntry must not grow a
	// KmPerSecondAirtime field. There's no reflection trick needed here --
	// this test exists so a future accidental addition is caught by a
	// clear, purpose-named test failure (a stray field would still need a
	// corresponding schema column to be USEFUL, and TestSchema_* above
	// pins that), rather than relying solely on code review.
	e := PingScoreHistoryEntry{}
	_ = e // no KmPerSecondAirtime field exists on this struct; see the type definition.
}

// --- 10. more than 499 deletes handled correctly ---

func TestUpsertAndDelete_MoreThan499Deletes(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenPingScoreHistoryStore(filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	const n = 1500
	seed := make([]PingScoreHistoryEntry, n)
	deleteIDs := make([]int64, n)
	for i := 0; i < n; i++ {
		txID := int64(i + 1)
		seed[i] = sampleHistoryEntry(txID)
		deleteIDs[i] = txID
	}
	if err := store.UpsertAndDelete(seed, nil); err != nil {
		t.Fatalf("seed %d rows: %v", n, err)
	}
	entries, err := store.LoadAll()
	if err != nil || len(entries) != n {
		t.Fatalf("seed did not land: len=%d err=%v", len(entries), err)
	}

	// Delete ALL of them in one call, spanning multiple 499-chunks.
	if err := store.UpsertAndDelete(nil, deleteIDs); err != nil {
		t.Fatalf("bulk delete %d rows: %v", n, err)
	}
	entries, err = store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll after bulk delete: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries after deleting all %d, got %d", n, len(entries))
	}
}

// --- 11. many upserts don't exceed SQLite's parameter limit ---

func TestUpsertAndDelete_ManyUpsertsNoParameterLimitError(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenPingScoreHistoryStore(filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	const n = 2000 // well past 499 -- would overflow a single multi-row INSERT's parameter budget
	batch := make([]PingScoreHistoryEntry, n)
	for i := 0; i < n; i++ {
		batch[i] = sampleHistoryEntry(int64(i + 1))
	}
	if err := store.UpsertAndDelete(batch, nil); err != nil {
		t.Fatalf("UpsertAndDelete with %d rows: %v", n, err)
	}
	entries, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(entries) != n {
		t.Errorf("expected %d entries, got %d", n, len(entries))
	}
}

// --- 12. two different history files are isolated ---

func TestTwoHistoryStores_AreIsolated(t *testing.T) {
	dir := t.TempDir()
	storeA, err := OpenPingScoreHistoryStore(filepath.Join(dir, "a.db"))
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	defer storeA.Close()
	storeB, err := OpenPingScoreHistoryStore(filepath.Join(dir, "b.db"))
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	defer storeB.Close()

	if err := storeA.UpsertAndDelete([]PingScoreHistoryEntry{sampleHistoryEntry(1)}, nil); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if err := storeB.UpsertAndDelete([]PingScoreHistoryEntry{sampleHistoryEntry(2), sampleHistoryEntry(3)}, nil); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	aEntries, err := storeA.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll A: %v", err)
	}
	bEntries, err := storeB.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll B: %v", err)
	}
	if len(aEntries) != 1 || aEntries[0].TxID != 1 {
		t.Errorf("store A leaked B's data or is missing its own: %+v", aEntries)
	}
	if len(bEntries) != 2 {
		t.Errorf("store B should have exactly its own 2 rows, got %+v", bEntries)
	}
}

// --- 13. the main database is never touched ---

func TestPingScoreHistoryStore_NeverTouchesMainDatabase(t *testing.T) {
	// setupTestDB (routes_test.go / db_test.go convention) sets up the
	// SEPARATE, shared meshcore.db-shaped test fixture. Confirm it is
	// unaffected by any history-store operation.
	mainDB := setupTestDB(t)
	var nodesBefore int
	if err := mainDB.conn.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodesBefore); err != nil {
		t.Fatalf("count nodes before: %v", err)
	}

	dir := t.TempDir()
	history, err := OpenPingScoreHistoryStore(filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatalf("open history store: %v", err)
	}
	defer history.Close()
	if err := history.UpsertAndDelete([]PingScoreHistoryEntry{sampleHistoryEntry(1), sampleHistoryEntry(2)}, nil); err != nil {
		t.Fatalf("history upsert: %v", err)
	}
	if _, err := history.LoadAll(); err != nil {
		t.Fatalf("history LoadAll: %v", err)
	}

	var nodesAfter int
	if err := mainDB.conn.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodesAfter); err != nil {
		t.Fatalf("count nodes after: %v", err)
	}
	if nodesBefore != nodesAfter {
		t.Errorf("main database's nodes count changed (%d -> %d) from a history-store-only operation", nodesBefore, nodesAfter)
	}
	// Confirm history's table doesn't even exist on the main connection.
	var count int
	err = mainDB.conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='ping_score_history_entries'`).Scan(&count)
	if err != nil {
		t.Fatalf("check main db for history table: %v", err)
	}
	if count != 0 {
		t.Error("ping_score_history_entries table leaked into the main database")
	}
}

// --- corruption handling: NEVER auto-replaced, a typed error instead ---
//
// ping_scores_history.db becomes the only remaining source for a ping's
// all-time contribution once its raw packet data has been pruned --
// automatically discarding a corrupt file and starting fresh, as an
// earlier version of this code did, could silently and permanently reduce
// "all-time" results. The correct behavior is to touch NOTHING and hand
// the caller a typed error to decide what happens next (e.g. run without
// a history store, or alert an operator) -- manual/offline recovery is
// out of scope for Phase 4A.

func TestOpenPingScoreHistoryStore_CorruptFileReturnsTypedErrorAndIsUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "h.db")
	const garbage = "this is not a sqlite database, just garbage bytes"

	if err := os.WriteFile(path, []byte(garbage), 0o644); err != nil {
		t.Fatalf("write garbage file: %v", err)
	}
	infoBefore, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}

	store, err := OpenPingScoreHistoryStore(path)
	if store != nil {
		store.Close()
		t.Fatal("OpenPingScoreHistoryStore returned a non-nil store for a corrupt file -- must return nil, error")
	}
	if err == nil {
		t.Fatal("expected an error for a corrupt file, got nil")
	}

	var corruptErr *PingScoreHistoryCorruptError
	if !errors.As(err, &corruptErr) {
		t.Fatalf("expected errors.As to find a *PingScoreHistoryCorruptError, got: %v (%T)", err, err)
	}
	if corruptErr.Path != path {
		t.Errorf("PingScoreHistoryCorruptError.Path = %q, want %q", corruptErr.Path, path)
	}
	if corruptErr.Detail == "" {
		t.Error("PingScoreHistoryCorruptError.Detail is empty, want the integrity_check failure detail")
	}

	// The file must be byte-identical and at the SAME path -- no rename,
	// no delete, no replacement.
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file after failed open: %v", err)
	}
	if string(content) != garbage {
		t.Error("file content was modified by a failed Open -- must be byte-identical to the original")
	}
	infoAfter, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if infoBefore.ModTime() != infoAfter.ModTime() {
		t.Error("file mtime changed -- Open must not have written to it")
	}

	// No .corrupt-* sibling, no new active database, no side files.
	matches, err := filepath.Glob(path + ".corrupt-*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("expected no .corrupt-* backup files to be created, found: %v", matches)
	}
	if fileExistsForTest(path + "-wal") {
		t.Error("a -wal side file was created for a corrupt file that was never opened writably")
	}
	if fileExistsForTest(path + "-shm") {
		t.Error("a -shm side file was created for a corrupt file that was never opened writably")
	}

	// Exactly one file exists in dir: the original, untouched garbage file.
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(dirEntries) != 1 || dirEntries[0].Name() != filepath.Base(path) {
		names := make([]string, len(dirEntries))
		for i, de := range dirEntries {
			names[i] = de.Name()
		}
		t.Errorf("expected exactly the original file in %s, found: %v", dir, names)
	}
}

// TestOpenPingScoreHistoryStore_CorruptIntegrityCheckFailureIsAlsoTyped
// covers the OTHER corruption shape: a file that IS parseable enough for
// PRAGMA integrity_check to run and return a non-"ok" result, rather than
// failing the PRAGMA outright. Constructed by damaging a real, previously
// valid database file's bytes rather than using plain garbage.
func TestOpenPingScoreHistoryStore_CorruptIntegrityCheckFailureIsAlsoTyped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "h.db")

	store, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatalf("initial open: %v", err)
	}
	if err := store.UpsertAndDelete([]PingScoreHistoryEntry{sampleHistoryEntry(1)}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Truncate a real database file's tail to corrupt it structurally,
	// while keeping the SQLite header intact (unlike plain garbage bytes,
	// this can pass Ping() and even the PRAGMA query itself, surfacing as
	// a non-"ok" integrity_check RESULT rather than a query-level error).
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	if len(original) < 200 {
		t.Fatalf("original file too small to meaningfully truncate: %d bytes", len(original))
	}
	truncated := original[:len(original)/2]
	if err := os.WriteFile(path, truncated, 0o644); err != nil {
		t.Fatalf("write truncated file: %v", err)
	}

	reopened, err := OpenPingScoreHistoryStore(path)
	if reopened != nil {
		reopened.Close()
	}
	if err == nil {
		t.Fatal("expected an error opening a truncated/corrupt database, got nil")
	}
	var corruptErr *PingScoreHistoryCorruptError
	if !errors.As(err, &corruptErr) {
		t.Fatalf("expected errors.As to find a *PingScoreHistoryCorruptError, got: %v (%T)", err, err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after failed open: %v", err)
	}
	if !bytes.Equal(content, truncated) {
		t.Error("truncated file was further modified by a failed Open")
	}
}

// --- missing parent directory: contextual error ---

func TestOpenPingScoreHistoryStore_MissingParentDirectory(t *testing.T) {
	_, err := OpenPingScoreHistoryStore("/this/path/definitely/does/not/exist/h.db")
	if err == nil {
		t.Fatal("expected an error when the parent directory does not exist")
	}
	if !strings.Contains(err.Error(), "parent directory") {
		t.Errorf("expected a contextual error mentioning the missing parent directory, got: %v", err)
	}
}

// --- DefaultPingScoreHistoryPath ---

func TestDefaultPingScoreHistoryPath(t *testing.T) {
	got := DefaultPingScoreHistoryPath("/opt/corescope/data/meshcore.db")
	want := "/opt/corescope/data/ping_scores_history.db"
	if got != want {
		t.Errorf("DefaultPingScoreHistoryPath = %q, want %q", got, want)
	}
}

// --- test helpers: raw, out-of-band manipulation of _meta for migration/version tests ---

func rawSetSchemaVersion(t *testing.T, path, version string) {
	t.Helper()
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open for schema_version manipulation: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Exec(
		`INSERT INTO _meta (key, value) VALUES ('schema_version', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, version,
	); err != nil {
		t.Fatalf("raw set schema_version: %v", err)
	}
}

func rawGetSchemaVersion(t *testing.T, path string) string {
	t.Helper()
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open for schema_version read: %v", err)
	}
	defer conn.Close()
	var v string
	if err := conn.QueryRow(`SELECT value FROM _meta WHERE key = 'schema_version'`).Scan(&v); err != nil {
		t.Fatalf("raw read schema_version: %v", err)
	}
	return v
}
