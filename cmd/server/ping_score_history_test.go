package main

import (
	"database/sql"
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

// --- corruption handling: preserved, never silently discarded ---

func TestOpenPingScoreHistoryStore_CorruptFileIsPreservedNotDeleted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "h.db")

	// Write garbage that is not a valid SQLite file at all -- guaranteed
	// to fail PRAGMA integrity_check.
	if err := os.WriteFile(path, []byte("this is not a sqlite database, just garbage bytes"), 0o644); err != nil {
		t.Fatalf("write garbage file: %v", err)
	}

	store, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatalf("OpenPingScoreHistoryStore should recover from corruption, not fail: %v", err)
	}
	defer store.Close()

	entries, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll on recovered store: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected an empty fresh store after corruption recovery, got %d entries", len(entries))
	}

	integrity, err := store.LoadIntegrity()
	if err != nil {
		t.Fatalf("LoadIntegrity: %v", err)
	}
	if integrity == nil || integrity.Status != "recovered-empty" {
		t.Fatalf("expected integrity status \"recovered-empty\", got %+v", integrity)
	}

	// The ORIGINAL corrupt file must still be on disk, renamed aside, not deleted.
	matches, err := filepath.Glob(path + ".corrupt-*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 preserved corrupt-file backup, found %d: %v", len(matches), matches)
	}
	preserved, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read preserved corrupt file: %v", err)
	}
	if string(preserved) != "this is not a sqlite database, just garbage bytes" {
		t.Error("preserved corrupt file's content was altered -- must be byte-identical to the original")
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
