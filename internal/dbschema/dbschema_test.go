package dbschema

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// minimalDB bootstraps a SQLite DB with just enough tables for the
// ensure_* helpers to run against, but WITHOUT any of the optional
// columns that dbschema.Apply is responsible for ensuring.
func minimalDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "schema.db")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		// Bare-bones tables, mirroring the legacy/empty fixture shape
		// pre-migration. Intentionally omit columns we expect Apply to add.
		`CREATE TABLE transmissions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			raw_hex TEXT NOT NULL,
			hash TEXT NOT NULL UNIQUE,
			first_seen TEXT NOT NULL,
			route_type INTEGER,
			payload_type INTEGER,
			payload_version INTEGER,
			decoded_json TEXT
		)`,
		`CREATE TABLE observations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			transmission_id INTEGER NOT NULL,
			observer_idx INTEGER,
			direction TEXT,
			snr REAL,
			rssi REAL,
			score INTEGER,
			path_json TEXT,
			timestamp INTEGER NOT NULL
		)`,
		`CREATE TABLE observers (
			id TEXT PRIMARY KEY,
			name TEXT
		)`,
		`CREATE TABLE nodes (
			public_key TEXT PRIMARY KEY,
			name TEXT
		)`,
		`CREATE TABLE inactive_nodes (
			public_key TEXT PRIMARY KEY,
			name TEXT,
			last_seen TEXT
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
	}
	return db
}

// TestApplyAddsOptionalColumns_CanonicalSource is the regression gate
// for issue #1321: dbschema.Apply must be the single source of truth
// for ALL optional columns the server PRAGMA-detects. Previously
// scope_name/default_scope/observations.raw_hex lived ONLY in
// cmd/ingestor/db.go applySchema, so the server (which runs
// detectSchema AFTER dbschema.AssertReady) could race the writer and
// cache stale false values when ingestor hadn't yet finished its
// applySchema migrations.
func TestApplyAddsOptionalColumns_CanonicalSource(t *testing.T) {
	db := minimalDB(t)
	defer db.Close()

	if err := Apply(db, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	cases := []struct {
		table, col string
	}{
		{"transmissions", "scope_name"},
		{"nodes", "default_scope"},
		{"inactive_nodes", "default_scope"},
		{"observations", "raw_hex"},
		{"nodes", "default_scope_confirmed_at"},
		{"inactive_nodes", "default_scope_confirmed_at"},
	}
	for _, c := range cases {
		has, err := TableHasColumn(db, c.table, c.col)
		if err != nil {
			t.Fatalf("probe %s.%s: %v", c.table, c.col, err)
		}
		if !has {
			t.Errorf("after Apply: %s.%s missing — dbschema must be the source of truth for this optional column (#1321)", c.table, c.col)
		}
	}
}

// TestAssertReady_RequiresOptionalColumns enforces that AssertReady
// REFUSES a DB missing the optional columns the server depends on —
// proving dbschema.AssertReady (not server-side PRAGMA detection) is
// the gate.
func TestAssertReady_RequiresOptionalColumns(t *testing.T) {
	db := minimalDB(t)
	defer db.Close()
	// Run pre-existing ensures so we only fail on the new ones.
	noop := Logger(func(string, ...interface{}) {})
	if err := ensureNeighborEdgesTable(db); err != nil {
		t.Fatal(err)
	}
	if err := ensureResolvedPathColumn(db, noop); err != nil {
		t.Fatal(err)
	}
	if err := ensureObserverInactiveColumn(db, noop); err != nil {
		t.Fatal(err)
	}
	if err := ensureLastPacketAtColumn(db, noop); err != nil {
		t.Fatal(err)
	}
	if err := ensureObserverIATAColumn(db, noop); err != nil {
		t.Fatal(err)
	}
	if err := ensureForeignAdvertColumn(db, noop); err != nil {
		t.Fatal(err)
	}
	if err := ensureFromPubkeyColumn(db, noop); err != nil {
		t.Fatal(err)
	}

	// At this point the OLD AssertReady set is satisfied but the NEW
	// columns are NOT — AssertReady must still fail.
	err := AssertReady(db)
	if err == nil {
		t.Fatal("AssertReady should fail when scope_name/default_scope/observations.raw_hex are missing (#1321)")
	}
	for _, must := range []string{"scope_name", "default_scope", "raw_hex"} {
		if !contains(err.Error(), must) {
			t.Errorf("AssertReady error should mention missing %q; got: %v", must, err)
		}
	}

	// After full Apply, AssertReady passes.
	if err := Apply(db, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := AssertReady(db); err != nil {
		t.Fatalf("AssertReady after full Apply: %v", err)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestPartialIdxTxLastSeenZero_BackfillMaxUsesPartialIndex pins the planner
// choice for issue #1740: the backfill MAX lookup
//
//	SELECT MAX(id) FROM transmissions WHERE last_seen = 0
//
// (and the chunked-backfill scan that walks WHERE last_seen=0 in id order)
// MUST use the partial index `idx_tx_last_seen_zero` introduced in #1740.
// The pre-fix full `idx_tx_last_seen` covers ALL rows (including the long
// tail where last_seen != 0 after backfill converges), so it grows with
// every transmission ever ingested. The partial index degenerates to the
// "unprocessed" hot subset and stays out of the page cache in steady state.
//
// Failure mode this test guards against:
//   - regressing to a full-table SCAN (no index)
//   - keeping the legacy full `idx_tx_last_seen` and letting the planner
//     reach for it instead of the partial
func TestPartialIdxTxLastSeenZero_BackfillMaxUsesPartialIndex(t *testing.T) {
	db := minimalDB(t)
	defer db.Close()

	if err := Apply(db, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Insert a handful of rows so the planner has something to weigh —
	// SQLite's planner short-circuits empty tables to "SCAN CONSTANT ROW"
	// which would mask a missing/wrong index.
	for i := 0; i < 16; i++ {
		_, err := db.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, last_seen)
			VALUES (?, ?, '2026-01-01T00:00:00Z', ?)`,
			"00", "h"+string(rune('a'+i)), int64(i%2)) // half last_seen=0, half last_seen=1
		if err != nil {
			t.Fatalf("seed insert: %v", err)
		}
	}
	if _, err := db.Exec(`ANALYZE`); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	// The backfill-MAX lookup — the exact shape the chunked backfill uses
	// to find the next batch of unprocessed ids.
	rows, err := db.Query(`EXPLAIN QUERY PLAN SELECT MAX(id) FROM transmissions WHERE last_seen = 0`)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	plan := ""
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan += detail + "\n"
	}
	t.Logf("backfill MAX plan:\n%s", plan)

	if !strings.Contains(plan, "idx_tx_last_seen_zero") {
		t.Fatalf("backfill MAX lookup must use partial index idx_tx_last_seen_zero (#1740); plan was:\n%s", plan)
	}
	// Defense-in-depth: the legacy full index must NOT be the one used,
	// and ideally must not even exist post-migration (the gated DROP
	// covers that — see TestPartialIdxTxLastSeenZero_FullIndexDropped).
	if strings.Contains(plan, "idx_tx_last_seen ") || strings.HasSuffix(strings.TrimSpace(plan), "idx_tx_last_seen") {
		t.Fatalf("backfill MAX lookup should NOT use legacy full idx_tx_last_seen (#1740); plan was:\n%s", plan)
	}
}

// TestPartialIdxTxLastSeenZero_FullIndexDropped asserts the gated DROP
// migration ran after the partial index was created (#1740 step b).
func TestPartialIdxTxLastSeenZero_FullIndexDropped(t *testing.T) {
	db := minimalDB(t)
	defer db.Close()
	if err := Apply(db, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Confirm the partial exists (precondition for the DROP gate).
	var partialName string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_tx_last_seen_zero'`).Scan(&partialName)
	if err != nil {
		t.Fatalf("idx_tx_last_seen_zero must exist after Apply (#1740): %v", err)
	}

	// Legacy full index must be gone after Apply (gated DROP).
	var legacyName string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_tx_last_seen'`).Scan(&legacyName)
	if err == nil {
		t.Fatalf("legacy idx_tx_last_seen must be dropped after partial index is in place (#1740); still present as %q", legacyName)
	}
}

// Shared channel proposals: Apply owns the table, names are unique with
// byte-exact (BINARY) semantics, and the server's AssertReady does NOT
// require the table, so a new server keeps starting against a database an
// older ingestor has not migrated yet (rolling upgrade).
func TestChannelProposalsTable(t *testing.T) {
	db := minimalDB(t)
	defer db.Close()
	for i := 0; i < 2; i++ { // idempotent
		if err := Apply(db, nil); err != nil {
			t.Fatalf("Apply #%d: %v", i+1, err)
		}
	}
	ins := func(id, name string) error {
		_, err := db.Exec(`INSERT INTO channel_proposals (id, name, status, created_at) VALUES (?, ?, 'pending', 1)`, id, name)
		return err
	}
	if err := ins("a", "#Test"); err != nil {
		t.Fatal(err)
	}
	if err := ins("b", "#test"); err != nil {
		t.Fatalf("#test must be distinct from #Test: %v", err)
	}
	if err := ins("c", "#Test"); err == nil {
		t.Fatal("duplicate #Test must violate UNIQUE")
	}
	if _, err := db.Exec(`INSERT INTO channel_proposals (id, name, status, created_at) VALUES ('d', '#x', 'bogus', 1)`); err == nil {
		t.Fatal("status CHECK not enforced")
	}
	if err := AssertReady(db); err != nil {
		t.Fatalf("AssertReady after Apply: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE channel_proposals`); err != nil {
		t.Fatal(err)
	}
	if err := AssertReady(db); err != nil {
		t.Fatalf("AssertReady must not require channel_proposals: %v", err)
	}
}

// A database that ran commit 4868622e (the first shared-channel-proposals
// commit, deployed to test instances) has channel_proposals with a CHECK that
// does not allow 'revoked'. CREATE TABLE IF NOT EXISTS leaves that table
// alone, so Apply must rebuild it — keeping every row — or every revoke fails
// with "CHECK constraint failed".
func TestChannelProposalsTable_UpgradesCheckWithoutRevoked(t *testing.T) {
	db := minimalDB(t)
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE channel_proposals (
		id TEXT PRIMARY KEY,
		name TEXT COLLATE BINARY NOT NULL UNIQUE,
		status TEXT NOT NULL CHECK(status IN ('pending','approved','rejected')),
		created_at INTEGER NOT NULL,
		reviewed_at INTEGER NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE INDEX idx_channel_proposals_status ON channel_proposals(status, created_at)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO channel_proposals (id, name, status, created_at, reviewed_at) VALUES
		('a', '#Alpha', 'approved', 10, 20),
		('b', '#beta', 'pending', 11, NULL),
		('c', '#Gamma', 'rejected', 12, 22)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE channel_proposals SET status = 'revoked' WHERE id = 'a'`); err == nil {
		t.Fatal("precondition: the old CHECK must reject 'revoked'")
	}

	var logs []string
	logf := func(format string, args ...interface{}) { logs = append(logs, fmt.Sprintf(format, args...)) }
	if err := Apply(db, logf); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rebuilt := 0
	for _, l := range logs {
		if strings.Contains(l, "channel_proposals") && strings.Contains(l, "revoked") {
			rebuilt++
		}
	}
	if rebuilt != 1 {
		t.Fatalf("want exactly one log line about rebuilding channel_proposals for 'revoked', got %q", logs)
	}

	rows, err := db.Query(`SELECT id, name, status, created_at, COALESCE(reviewed_at, -1) FROM channel_proposals ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var id, name, status string
		var created, reviewed int64
		if err := rows.Scan(&id, &name, &status, &created, &reviewed); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s|%s|%s|%d|%d", id, name, status, created, reviewed))
	}
	rows.Close()
	want := []string{"a|#Alpha|approved|10|20", "b|#beta|pending|11|-1", "c|#Gamma|rejected|12|22"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rows after rebuild = %v, want %v", got, want)
	}
	if _, err := db.Exec(`UPDATE channel_proposals SET status = 'revoked' WHERE id = 'a'`); err != nil {
		t.Fatalf("revoke after rebuild: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO channel_proposals (id, name, status, created_at) VALUES ('d', '#alpha', 'pending', 1)`); err != nil {
		t.Fatalf("#alpha must still be distinct from #Alpha (BINARY): %v", err)
	}
	if _, err := db.Exec(`INSERT INTO channel_proposals (id, name, status, created_at) VALUES ('e', '#Alpha', 'pending', 1)`); err == nil {
		t.Fatal("UNIQUE(name) lost in rebuild")
	}
	if _, err := db.Exec(`INSERT INTO channel_proposals (id, name, status, created_at) VALUES ('f', '#x', 'bogus', 1)`); err == nil {
		t.Fatal("status CHECK lost in rebuild")
	}
	var idx int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_channel_proposals_status'`).Scan(&idx); err != nil || idx != 1 {
		t.Fatalf("status index after rebuild: n=%d err=%v", idx, err)
	}
	var leftovers int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'channel_proposals_%'`).Scan(&leftovers); err != nil || leftovers != 0 {
		t.Fatalf("temporary table left behind: n=%d err=%v", leftovers, err)
	}

	// A second run is a no-op: nothing is rebuilt or logged, rows unchanged.
	logs = nil
	if err := Apply(db, logf); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	for _, l := range logs {
		if strings.Contains(l, "channel_proposals") {
			t.Fatalf("second Apply must not touch channel_proposals, logged %q", l)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM channel_proposals`).Scan(&n); err != nil || n != 4 {
		t.Fatalf("row count after second Apply = %d (%v), want 4", n, err)
	}
}
