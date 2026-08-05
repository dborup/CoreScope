// Package main: Ping Scores history store -- Phase 4A of the recompute
// redesign (see reviews/CoreScope-code-review-2026-08-04.md and the
// accompanying design discussion).
//
// This is a SEPARATE SQLite file the server exclusively owns, distinct
// from meshcore.db (opened elsewhere via OpenDB with mode=ro -- the server
// can never write there; the ingestor is meshcore.db's sole writer). Since
// this file is never shared with the ingestor or any other process, there
// is no multi-writer contention to worry about: the server is the only
// process that ever opens it.
//
// Once a ping's underlying transmissions/observations rows have been
// pruned (packetDays retention, owned by the ingestor), this store becomes
// the ONLY remaining source for that ping's contribution to the all-time
// records and leaderboards -- it is a history store, not a disposable
// cache, and corruption/version-mismatch handling below is written with
// that in mind: never silently discard rows that cannot be reconstructed
// from anywhere else. Corruption, and a KNOWN older version whose
// migration fails, are therefore FATAL to Open (typed errors, see
// PingScoreHistoryCorruptError / PingScoreHistoryMigrationError) rather
// than something this package silently works around -- the caller decides
// whether to run without a history store (falling back to the pre-4A
// recomputer) or to involve an operator. An UNRECOGNIZED FUTURE schema
// version is not fatal: Open still succeeds and returns a usable store,
// just a physically read-only one (see openExistingPingScoreHistoryStore)
// -- existing rows stay readable, nothing is migrated or written, and no
// caller fallback is needed for that case.
//
// Phase 4A scope: only the storage foundation (connection lifecycle,
// versioned additive schema, upsert/delete/load operations, integrity
// metadata plumbing). It is not yet wired into computeAllPingScores or
// StartPingScoresRecomputer -- that is Phase 4B+.
package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	_ "modernc.org/sqlite"
)

// pingScoreHistorySchemaVersion is the schema version THIS code understands
// and will migrate an older on-disk file up to. A file with a HIGHER
// recorded version was written by newer code (e.g. a rolled-back deploy)
// and is opened read-only instead -- see openAndCheck.
//
// v2 (fix-round-2 review of a1c3022d) adds
// ping_score_history_entries.permanently_unreconstructable (see
// PingScoreHistoryEntry.PermanentlyUnreconstructable) and the new
// ping_score_history_gaps table (see PingScoreHistoryGap) -- both purely
// additive, see applyPingScoreHistoryV2.
const pingScoreHistorySchemaVersion = 2

// PingScoreHistoryStore owns the separate, server-only SQLite connection to
// ping_scores_history.db. Not safe for concurrent use from multiple
// goroutines without external synchronization -- Phase 4B's design commits
// to a single-goroutine-owner contract for the whole ping-scores recompute
// cycle, matching today's StartPingScoresRecomputer.
type PingScoreHistoryStore struct {
	conn     *sql.DB
	path     string
	readOnly bool // true when the on-disk schema_version is newer than pingScoreHistorySchemaVersion
}

// PingScoreHistoryEntry is one durably persisted ping's score facts.
//
// Deliberately does NOT include: display names (DeepestName/FarthestName/
// firstName/leaderboard Name) -- these are resolved fresh from `nodes` at
// snapshot-build time in Phase 4B's design, never cached here, so a later
// node rename is never frozen into stale history. Also deliberately does
// NOT include KmPerSecondAirtime -- it is a derived value
// (FarthestKm / (AirtimeMs/1000)) that must be recomputed whenever
// FarthestKm changes, even after AirtimeMs itself has been permanently
// locked in (see the design report's field-by-field merge table).
type PingScoreHistoryEntry struct {
	TxID             int64
	Hash             string
	Sender           string
	ChannelHash      string
	Timestamp        string
	StationCount     int
	DeepestHops      int
	DeepestPubkey    string
	FarthestKm       *float64
	FarthestPubkey   string
	SpreadSeconds    *float64
	AirtimeMs        *float64
	RelayCount       int
	RelayPubkeysJSON string
	FirstPubkey      string
	Unscorable       bool
	FingerprintCount int64
	FingerprintMaxID int64
	StableSince      string
	Settled          bool
	DataPruned       bool

	// PermanentlyUnreconstructable is true only once a REAL deep-sweep
	// attempt (not an age estimate) has actually proven, this tx_id had
	// never been scorable AND that attempt found nothing AND it was
	// already past the retention window -- see
	// maybeMarkPermanentlyUnreconstructable, the ONLY place this is ever
	// set to true. Age alone (Unscorable && old) is deliberately NOT
	// sufficient on its own (fix-round-2 review of a1c3022d) -- an entry
	// stays deep-sweep-eligible until this evidence actually exists, so it
	// always gets at least one real post-retention attempt first. Distinct
	// from DataPruned (which requires the OPPOSITE: !Unscorable, i.e. "had
	// a valid score once") -- mutually exclusive by construction, same as
	// isPermanentlyUnreconstructable's former Unscorable-gated design.
	// Defensively cleared (never left stale) the moment any later cycle
	// DOES produce a real score for this tx_id -- see
	// mergePingScoreHistoryEntry's score != nil branch.
	PermanentlyUnreconstructable bool

	LastDeepSweptAt string
	ComputedAt      string
}

// PingScoreHistoryGap's ROW EXISTENCE is a permanent, cumulative
// historical signal: once written, it records that AT LEAST ONE entry has
// EVER been evidence-confirmed PermanentlyUnreconstructable over the
// WHOLE lifetime of this history store -- deliberately SEPARATE from
// PingScoreHistoryIntegrity's "initial-backfill-incomplete" status (which
// is a one-time snapshot at the genuine first bootstrap only, see
// buildInitialBootstrapIntegrity). A permanently-unreconstructable ping
// can be proven at ANY point in the store's life, long after a perfectly
// healthy bootstrap -- this record exists so that loss is never silently
// invisible just because the ORIGINAL bootstrap had nothing to report.
//
// The COUNT fields are NOT cumulative, though -- PermanentlyUnreconstructableCount
// and TotalTriggers (see their own doc comments below) are the CURRENT
// live trigger population, refreshed every cycle, free to rise, fall, and
// reach 0. Only the row's existence and FirstDetectedAt are the permanent,
// cumulative part of this record -- the row is never deleted or reverted
// to an "ok" status just because the current count happens to be 0 (see
// buildPingScoreHistoryGap for the exact update decision this asymmetry
// drives).
//
// Persisted as its own singleton row (id=1) in ping_score_history_gaps,
// added in the v2 migration -- kept as a SEPARATE table rather than
// overloading ping_score_history_integrity's existing columns, since the
// two records answer genuinely different questions (a one-time snapshot
// vs. a permanent signal paired with a live count) and conflating them
// would lose information (see applyPingScoreHistoryV2's doc comment).
//
// nil (from LoadGap) means "no permanently-unreconstructable entry has
// EVER been proven" -- the normal, healthy case, same "nil means healthy"
// convention as PingScoreHistoryIntegrity.
type PingScoreHistoryGap struct {
	// Status is always "history-incomplete" whenever this struct is
	// non-nil -- there is currently no other value this ever takes; the
	// field exists (rather than the mere presence of the row) so a future
	// caller has an explicit, self-describing value to branch on, matching
	// PingScoreHistoryIntegrity's own Status-string convention.
	Status string

	// FirstDetectedAt (RFC3339) is set ONCE, the very first time this
	// record is ever written, and PRESERVED VERBATIM forever after --
	// never revised, no matter how many more cycles add further evidence,
	// how the counts below change, or even if PermanentlyUnreconstructableCount
	// later drops back to 0. This is the "when was the first loss ever
	// observed" timestamp the review specifically asked to never lose.
	FirstDetectedAt string

	// LastDetectedAt (RFC3339) advances ONLY when a cycle observes NEW
	// evidence -- an entry transitioning to PermanentlyUnreconstructable=true
	// for the first time (see newEvidenceThisCycle in Cycle and
	// buildPingScoreHistoryGap's own doc comment). It is deliberately NOT
	// "last cycle this row was written" or "last time it was checked": a
	// cycle that only re-counts an unchanged population, or that counts
	// FEWER permanently-unreconstructable entries because one's trigger
	// was pruned, updates the counts below without touching this field
	// (fix-round-4 review of bea755e5 -- the previous version stamped this
	// with `now` on every single cycle a permanent entry existed, whether
	// or not anything was actually newly detected).
	LastDetectedAt string

	// PermanentlyUnreconstructableCount is the CURRENT live count of
	// entries with PermanentlyUnreconstructable=true -- NOT a cumulative/
	// ever count. It is refreshed every cycle (even when unchanged, the
	// computed value is still current; see buildPingScoreHistoryGap for
	// when that refresh is actually WRITTEN vs skipped) and can legitimately
	// drop all the way to 0 -- when it does, the row is updated to 0
	// rather than left showing a stale earlier count; the row itself
	// still isn't deleted or reverted to "ok" (see Status above).
	PermanentlyUnreconstructableCount int

	// TotalTriggers is the CURRENT live trigger count, describing the
	// exact SAME post-cycle population PermanentlyUnreconstructableCount
	// is scoped to (see Cycle's deletedThisCycle exclusion) -- matches
	// PingScoreHistoryIntegrity.TotalTriggers's own naming and scope.
	TotalTriggers int

	Detail string
}

// PingScoreHistoryCorruptError means the file failed PRAGMA integrity_check
// (or couldn't even run it -- e.g. it isn't a valid SQLite database at
// all). The file, and any -wal/-shm side files, are left completely
// untouched: no rename, no delete, no replacement. Recovering from this is
// an explicit, separate operator action in a later phase -- Open never
// attempts it automatically, because after packetDays pruning this file
// may be the only remaining source for old pings' all-time contribution.
type PingScoreHistoryCorruptError struct {
	Path   string
	Detail string
}

func (e *PingScoreHistoryCorruptError) Error() string {
	return fmt.Sprintf("ping score history store: %s failed integrity check: %s", e.Path, e.Detail)
}

// PingScoreHistoryMigrationError means an additive schema migration failed
// partway. The connection is closed and no further writes are attempted --
// this is a hard failure of Open, not a degraded-but-usable store. Rows
// already committed by any EARLIER migration step remain in place (each
// step is its own transaction; see pingScoreHistoryMigrations), so a retry
// on a later Open call only re-attempts the step that failed.
type PingScoreHistoryMigrationError struct {
	Path        string
	FromVersion int
	ToVersion   int
	Err         error
}

func (e *PingScoreHistoryMigrationError) Error() string {
	return fmt.Sprintf("ping score history store: %s: migration v%d -> v%d failed: %v", e.Path, e.FromVersion, e.ToVersion, e.Err)
}

func (e *PingScoreHistoryMigrationError) Unwrap() error { return e.Err }

// PingScoreHistoryIntegrity records an abnormal state of the history store
// that a consumer (Phase 4B+, and eventually the API) should surface
// rather than silently paper over. Fields not relevant to a given Status
// are left zero-valued.
//
// IMPORTANT: OpenPingScoreHistoryStore itself NEVER sets this -- corruption
// and a failed migration of a known older version are both FATAL to Open
// (typed errors: PingScoreHistoryCorruptError / PingScoreHistoryMigrationError),
// precisely so an operator/caller decides what happens next rather than
// this package silently switching to a reduced-history state on their
// behalf. (An unrecognized FUTURE schema version is a third, non-fatal
// case: Open just returns a usable, physically read-only store -- see the
// package doc comment above -- so there is nothing to record here for it
// either.) StoreIntegrity/LoadIntegrity are plumbing for a LATER, explicit
// recovery function (not built in Phase 4A) to use once it exists.
type PingScoreHistoryIntegrity struct {
	// Status is one of: "ok", "initial-backfill-incomplete",
	// "degraded-unknown-version", "recovered-partial", "recovered-empty".
	Status     string
	DetectedAt string // RFC3339

	// Relevant to "initial-backfill-incomplete" (a later phase populates
	// these once GetPacketPathsBulk exists; Phase 4A only provides the
	// storage for them).
	TotalTriggers          int
	ScoredCount            int
	UnreconstructableCount int

	// Relevant to "recovered-partial"/"recovered-empty", written by a
	// future explicit recovery function -- never by Open itself.
	RowsRecovered int

	Detail string
}

// DefaultPingScoreHistoryPath derives the production location of the
// history store: a sibling file next to the main database, so it lives in
// the same persistent data directory/volume by construction (no separate
// config knob to get wrong). Called from main.go (fase 5G) to derive the
// path passed to StartPingScoreHistoryEngine.
//
// The derived path is NOT automatically guaranteed to differ from
// mainDBPath itself -- if mainDBPath's own basename happens to literally
// be "ping_scores_history.db" (a misconfigured dbPath), this function
// would return mainDBPath unchanged. Callers must not treat the result
// as safe to open read-write without independently verifying it is
// distinct from mainDBPath first -- see pingScoreHistoryPathsCollide,
// which validatePingScoreHistoryStartConfig calls synchronously before
// StartPingScoreHistoryEngine ever opens anything.
func DefaultPingScoreHistoryPath(mainDBPath string) string {
	return filepath.Join(filepath.Dir(mainDBPath), "ping_scores_history.db")
}

// OpenPingScoreHistoryStore opens (creating if absent) the history store at
// path. It NEVER touches meshcore.db or any connection opened via OpenDB --
// this is a completely independent *sql.DB against a different file.
//
// Corruption handling: if the file already exists, it is FIRST inspected
// through a physically read-only connection (SQLite URI mode=ro, no
// _journal_mode=WAL parameter -- so opening for inspection cannot itself
// change the journal mode or create/modify -wal/-shm side files). If
// PRAGMA integrity_check fails on that connection, Open returns a
// *PingScoreHistoryCorruptError and touches NOTHING on disk: no rename, no
// delete, no replacement file. Recovering from this is a separate,
// explicit operator action in a later phase, not something this function
// does automatically -- see the type's doc comment for why.
//
// Version handling: an on-disk schema_version newer than
// pingScoreHistorySchemaVersion means a newer server version wrote this
// file (e.g. a rolled-back deploy). In that case the store is returned
// using the SAME physically read-only connection from the inspection pass
// -- it is never reopened read-write, so there is no way for this code to
// write a layout it doesn't understand, regardless of what any future
// method might forget to check. ReadOnly() reports this for API
// visibility, but is not the mechanism enforcing it.
//
// Migration handling: only reached when schema_version <= supported. The
// inspection connection is closed and a genuinely read-write connection is
// opened (this is the ONLY path that ever produces a write-capable
// connection). The safety checks (integrity, version) are repeated against
// that new connection before migrating, since decisions made against the
// now-closed inspection connection aren't safe to assume still hold. On
// migration failure, Open returns a *PingScoreHistoryMigrationError and
// closes the connection -- no partial, silently-degraded store is ever
// returned.
func OpenPingScoreHistoryStore(path string) (*PingScoreHistoryStore, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if _, err := os.Stat(dir); err != nil {
			return nil, fmt.Errorf("ping score history store: parent directory %q: %w", dir, err)
		}
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return createFreshPingScoreHistoryStore(path)
	} else if err != nil {
		return nil, fmt.Errorf("ping score history store: stat %s: %w", path, err)
	}
	return openExistingPingScoreHistoryStore(path)
}

// createFreshPingScoreHistoryStore handles the "file does not exist yet"
// case: opened directly read-write (nothing to inspect, there is no prior
// content to protect), then migrated from version 0.
func createFreshPingScoreHistoryStore(path string) (*PingScoreHistoryStore, error) {
	conn, err := openPingScoreHistoryConn(path, true)
	if err != nil {
		return nil, fmt.Errorf("ping score history store: create %s: %w", path, err)
	}
	s := &PingScoreHistoryStore{conn: conn, path: path}
	if err := s.migrateFrom(0); err != nil {
		conn.Close()
		return nil, &PingScoreHistoryMigrationError{Path: path, FromVersion: 0, ToVersion: pingScoreHistorySchemaVersion, Err: err}
	}
	return s, nil
}

// openExistingPingScoreHistoryStore handles the "file already exists"
// case, per the read/inspect-before-write flow documented on
// OpenPingScoreHistoryStore.
func openExistingPingScoreHistoryStore(path string) (*PingScoreHistoryStore, error) {
	roConn, err := openPingScoreHistoryConn(path, false)
	if err != nil {
		return nil, fmt.Errorf("ping score history store: open %s read-only for inspection: %w", path, err)
	}

	if corruptErr := checkPingScoreHistoryIntegrity(roConn, path); corruptErr != nil {
		roConn.Close()
		return nil, corruptErr
	}

	version, err := readSchemaVersionFrom(roConn)
	if err != nil {
		roConn.Close()
		return nil, fmt.Errorf("ping score history store: read schema version from %s: %w", path, err)
	}

	if version > pingScoreHistorySchemaVersion {
		// Newer than this code understands: KEEP this physically read-only
		// connection as-is. It never becomes write-capable.
		return &PingScoreHistoryStore{conn: roConn, path: path, readOnly: true}, nil
	}

	// version <= supported: a write-capable connection is needed (at
	// minimum to check/apply migrations). This is the ONLY branch that
	// ever opens a read-write connection. The inspection connection's job
	// is done.
	roConn.Close()

	rwConn, err := openPingScoreHistoryConn(path, true)
	if err != nil {
		return nil, fmt.Errorf("ping score history store: reopen %s read-write: %w", path, err)
	}

	// Re-verify against the NEW connection rather than trusting the
	// now-closed inspection connection's findings -- cheap, and avoids
	// acting on a decision made against a connection that no longer exists.
	if corruptErr := checkPingScoreHistoryIntegrity(rwConn, path); corruptErr != nil {
		rwConn.Close()
		return nil, corruptErr
	}
	version2, err := readSchemaVersionFrom(rwConn)
	if err != nil {
		rwConn.Close()
		return nil, fmt.Errorf("ping score history store: re-read schema version from %s: %w", path, err)
	}
	if version2 > pingScoreHistorySchemaVersion {
		// Changed between the two checks. This store is server-owned and
		// single-writer by design, so this "shouldn't" happen -- handled
		// safely anyway rather than assumed impossible: close the
		// write-capable connection and return a fresh, physically
		// read-only one instead of ever using the one that's open now.
		rwConn.Close()
		roConn2, err := openPingScoreHistoryConn(path, false)
		if err != nil {
			return nil, fmt.Errorf("ping score history store: reopen %s read-only after version changed mid-open: %w", path, err)
		}
		return &PingScoreHistoryStore{conn: roConn2, path: path, readOnly: true}, nil
	}

	s := &PingScoreHistoryStore{conn: rwConn, path: path}
	if version2 < pingScoreHistorySchemaVersion {
		if err := s.migrateFrom(version2); err != nil {
			rwConn.Close()
			return nil, &PingScoreHistoryMigrationError{Path: path, FromVersion: version2, ToVersion: pingScoreHistorySchemaVersion, Err: err}
		}
	}
	return s, nil
}

// openPingScoreHistoryConn opens a *sql.DB against path. writable=false
// uses SQLite's URI mode=ro (physically read-only at the VFS level, no
// _journal_mode parameter -- cannot change the journal mode or create/
// touch -wal/-shm side files). writable=true is the ONLY DSN shape that
// enables WAL and permits writes.
func openPingScoreHistoryConn(path string, writable bool) (*sql.DB, error) {
	var dsn string
	if writable {
		dsn = fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000", path)
	} else {
		dsn = fmt.Sprintf("file:%s?mode=ro&_busy_timeout=5000", path)
	}
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Single connection: this store is only ever driven by one goroutine
	// (Phase 4B's design commits to this), and a single-connection pool
	// avoids any ambiguity about which connection sees which uncommitted
	// state within the process itself.
	conn.SetMaxOpenConns(1)
	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// checkPingScoreHistoryIntegrity runs PRAGMA integrity_check against conn
// and returns a *PingScoreHistoryCorruptError if the file is not a valid,
// intact SQLite database. A file that isn't a valid SQLite database at all
// (garbage bytes, truncated, wrong format) fails the PRAGMA itself at the
// driver level -- e.g. modernc.org/sqlite's "file is not a database (26)"
// -- rather than returning a non-"ok" result row; both failure shapes are
// treated identically here.
func checkPingScoreHistoryIntegrity(conn *sql.DB, path string) *PingScoreHistoryCorruptError {
	var result string
	if err := conn.QueryRow(`PRAGMA integrity_check`).Scan(&result); err != nil {
		return &PingScoreHistoryCorruptError{Path: path, Detail: err.Error()}
	}
	if result != "ok" {
		return &PingScoreHistoryCorruptError{Path: path, Detail: result}
	}
	return nil
}

// ReadOnly reports whether this store was opened against a newer-than-
// understood schema version. This is API-level information for callers --
// the actual write protection is the underlying connection itself being
// opened with SQLite's mode=ro (see openPingScoreHistoryConn), not this
// flag; UpsertAndDelete/StoreIntegrity check it only to fail with a clear
// error message instead of a raw driver "readonly database" error.
func (s *PingScoreHistoryStore) ReadOnly() bool { return s.readOnly }

// Close releases the underlying connection. Safe to call multiple times.
func (s *PingScoreHistoryStore) Close() error {
	if s.conn == nil {
		return nil
	}
	err := s.conn.Close()
	s.conn = nil
	return err
}

// readSchemaVersionFrom is a free function (not a *PingScoreHistoryStore
// method) because it must run against the inspection (read-only) or
// reopened (read-write) connection BEFORE a PingScoreHistoryStore exists
// for either of them, in openExistingPingScoreHistoryStore's flow.
func readSchemaVersionFrom(conn *sql.DB) (int, error) {
	var tableExists int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='_meta'`,
	).Scan(&tableExists); err != nil {
		return 0, fmt.Errorf("check _meta existence: %w", err)
	}
	if tableExists == 0 {
		return 0, nil // brand-new file, nothing migrated yet
	}
	var raw string
	err := conn.QueryRow(`SELECT value FROM _meta WHERE key = 'schema_version'`).Scan(&raw)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read _meta.schema_version: %w", err)
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid schema_version value %q: %w", raw, err)
	}
	return v, nil
}

func (s *PingScoreHistoryStore) readSchemaVersion() (int, error) {
	return readSchemaVersionFrom(s.conn)
}

// pingScoreHistoryMigration is one additive, idempotent step in the
// version ladder. Steps run in order, each in its OWN transaction --
// resumable by construction: if step N fails, steps before it stay
// committed (and recorded), and a retry only re-attempts from N.
type pingScoreHistoryMigration struct {
	toVersion int
	apply     func(tx *sql.Tx) error
}

// pingScoreHistoryMigrations is the full ladder up to
// pingScoreHistorySchemaVersion. Rule (carried over from the design
// report, itself matching this codebase's established schemaFlag
// philosophy -- "ADD-only columns never disappear"): every future step
// appended here MUST be additive (CREATE TABLE IF NOT EXISTS / ADD COLUMN
// with a default / CREATE INDEX IF NOT EXISTS). The table is never
// dropped or recreated by a migration.
var pingScoreHistoryMigrations = []pingScoreHistoryMigration{
	{toVersion: 1, apply: applyPingScoreHistoryV1},
	{toVersion: 2, apply: applyPingScoreHistoryV2},
}

func (s *PingScoreHistoryStore) migrateFrom(fromVersion int) error {
	for _, m := range pingScoreHistoryMigrations {
		if m.toVersion <= fromVersion {
			continue
		}
		tx, err := s.conn.Begin()
		if err != nil {
			return fmt.Errorf("begin migration to v%d: %w", m.toVersion, err)
		}
		if err := m.apply(tx); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration to v%d: %w", m.toVersion, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO _meta (key, value) VALUES ('schema_version', ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			strconv.Itoa(m.toVersion),
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration v%d: %w", m.toVersion, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration to v%d: %w", m.toVersion, err)
		}
	}
	return nil
}

// applyPingScoreHistoryV1 creates the full initial schema. Every statement
// is idempotent (IF NOT EXISTS) so re-running this on an already-v1 file
// (e.g. a retried migration after an unrelated later failure) is always
// safe.
func applyPingScoreHistoryV1(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS _meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		// tx_id > 0 mirrors ping_triggers.tx_id's own INTEGER PRIMARY KEY
		// (SQLite rowid alias) semantics -- always a positive integer in
		// real usage; the constraint exists mainly as a documented
		// invariant (and a way to deliberately provoke a mid-transaction
		// failure in tests, see TestUpsertAndDelete_FailureRollsBackEntireTransaction).
		`CREATE TABLE IF NOT EXISTS ping_score_history_entries (
			tx_id                  INTEGER PRIMARY KEY CHECK (tx_id > 0),
			hash                   TEXT NOT NULL,
			sender                 TEXT,
			channel_hash           TEXT,
			timestamp              TEXT NOT NULL,
			station_count          INTEGER NOT NULL,
			deepest_hops           INTEGER NOT NULL,
			deepest_pubkey         TEXT,
			farthest_km            REAL,
			farthest_pubkey        TEXT,
			spread_seconds         REAL,
			airtime_ms             REAL,
			relay_count            INTEGER,
			relay_pubkeys_json     TEXT,
			first_pubkey           TEXT,
			unscorable             INTEGER NOT NULL DEFAULT 0,
			fingerprint_count      INTEGER NOT NULL DEFAULT 0,
			fingerprint_max_id     INTEGER NOT NULL DEFAULT 0,
			stable_since           TEXT,
			settled                INTEGER NOT NULL DEFAULT 0,
			data_pruned            INTEGER NOT NULL DEFAULT 0,
			last_deep_swept_at     TEXT,
			computed_at            TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_ping_score_history_timestamp ON ping_score_history_entries(timestamp)`,
		`CREATE INDEX IF NOT EXISTS idx_ping_score_history_settled ON ping_score_history_entries(settled)`,
		`CREATE TABLE IF NOT EXISTS ping_score_history_integrity (
			id                       INTEGER PRIMARY KEY CHECK (id = 1),
			status                   TEXT NOT NULL,
			detected_at              TEXT NOT NULL,
			total_triggers           INTEGER NOT NULL DEFAULT 0,
			scored_count             INTEGER NOT NULL DEFAULT 0,
			unreconstructable_count  INTEGER NOT NULL DEFAULT 0,
			rows_recovered           INTEGER NOT NULL DEFAULT 0,
			detail                   TEXT
		)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("exec %q: %w", stmt, err)
		}
	}
	return nil
}

// applyPingScoreHistoryV2 (fix-round-2 review of a1c3022d) adds:
//   - ping_score_history_entries.permanently_unreconstructable: the
//     evidence-based (never age-based) permanent-unreconstructability
//     flag -- see PingScoreHistoryEntry.PermanentlyUnreconstructable and
//     maybeMarkPermanentlyUnreconstructable in
//     ping_score_history_engine.go. Defaults to 0/false for every
//     pre-existing v1 row: a v1 file has no rows that were ever evaluated
//     against the evidence rule, so "not (yet) flagged" is the only
//     honest default -- a future deep-sweep cycle naturally establishes
//     the flag for any row that genuinely qualifies, exactly as it would
//     for a brand-new entry.
//   - ping_score_history_gaps: a NEW singleton table (see
//     PingScoreHistoryGap's own doc comment for why this must be separate
//     from ping_score_history_integrity rather than overloading its
//     existing columns).
//
// SQLite has no portable ADD COLUMN IF NOT EXISTS across the versions this
// codebase targets, so the ALTER TABLE below is preceded by an explicit
// PRAGMA table_info check and skipped if the column is already present --
// this migration step is otherwise the only non-naturally-idempotent one
// in the ladder (v1's statements are all CREATE ... IF NOT EXISTS), and
// schema_version/schema CAN legitimately desync from what migrateFrom's
// normal one-transaction-per-step design alone would guarantee, e.g. an
// operator manually editing _meta.schema_version, or (as this package's
// own tests do) reopening a file whose recorded version was rolled back
// out-of-band while its tables already reflect a later version.
func applyPingScoreHistoryV2(tx *sql.Tx) error {
	var alreadyHasColumn int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('ping_score_history_entries') WHERE name = 'permanently_unreconstructable'`,
	).Scan(&alreadyHasColumn); err != nil {
		return fmt.Errorf("check permanently_unreconstructable column existence: %w", err)
	}

	var stmts []string
	if alreadyHasColumn == 0 {
		stmts = append(stmts, `ALTER TABLE ping_score_history_entries ADD COLUMN permanently_unreconstructable INTEGER NOT NULL DEFAULT 0`)
	}
	stmts = append(stmts, `CREATE TABLE IF NOT EXISTS ping_score_history_gaps (
		id                                    INTEGER PRIMARY KEY CHECK (id = 1),
		status                                TEXT NOT NULL,
		first_detected_at                     TEXT NOT NULL,
		last_detected_at                      TEXT NOT NULL,
		permanently_unreconstructable_count   INTEGER NOT NULL DEFAULT 0,
		total_triggers                        INTEGER NOT NULL DEFAULT 0,
		detail                                TEXT
	)`)
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("exec %q: %w", stmt, err)
		}
	}
	return nil
}

// pingScoreHistoryUpsertSQL is executed as a PREPARED statement, once per
// row, inside a single transaction in UpsertAndDelete -- deliberately NOT
// a multi-row INSERT. chunkSize=499 (used below for the DELETE IN-list) is
// a safe bound for a single IN-list's parameter count; it says nothing
// about how many rows fit in one multi-row INSERT, which uses
// columns-per-row times rows-per-statement parameters instead. Rather than
// compute rowsPerStatement = maxVariables/columnsPerRow (and have to
// re-derive it if the column count ever changes), a prepared single-row
// upsert sidesteps the whole class of bug: each Exec always binds exactly
// len(columns) parameters, regardless of batch size.
const pingScoreHistoryUpsertSQL = `
INSERT INTO ping_score_history_entries (
	tx_id, hash, sender, channel_hash, timestamp, station_count, deepest_hops,
	deepest_pubkey, farthest_km, farthest_pubkey, spread_seconds, airtime_ms,
	relay_count, relay_pubkeys_json, first_pubkey, unscorable,
	fingerprint_count, fingerprint_max_id, stable_since, settled, data_pruned,
	permanently_unreconstructable, last_deep_swept_at, computed_at
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(tx_id) DO UPDATE SET
	hash=excluded.hash, sender=excluded.sender, channel_hash=excluded.channel_hash,
	timestamp=excluded.timestamp, station_count=excluded.station_count,
	deepest_hops=excluded.deepest_hops, deepest_pubkey=excluded.deepest_pubkey,
	farthest_km=excluded.farthest_km, farthest_pubkey=excluded.farthest_pubkey,
	spread_seconds=excluded.spread_seconds, airtime_ms=excluded.airtime_ms,
	relay_count=excluded.relay_count, relay_pubkeys_json=excluded.relay_pubkeys_json,
	first_pubkey=excluded.first_pubkey, unscorable=excluded.unscorable,
	fingerprint_count=excluded.fingerprint_count, fingerprint_max_id=excluded.fingerprint_max_id,
	stable_since=excluded.stable_since, settled=excluded.settled, data_pruned=excluded.data_pruned,
	permanently_unreconstructable=excluded.permanently_unreconstructable,
	last_deep_swept_at=excluded.last_deep_swept_at, computed_at=excluded.computed_at
`

// pingScoreHistoryDeleteChunkSize bounds a single DELETE...IN(...)
// statement's parameter count -- a completely separate concern from the
// upsert path above (which never builds an IN-list at all). Matches this
// codebase's established convention (namesAndRolesForPubkeys,
// foreignFlagsForPubkeys in db.go both use 499).
const pingScoreHistoryDeleteChunkSize = 499

// UpsertAndDelete applies a batch of upserts and a set of tx_ids to delete
// as ONE SQL transaction: either the whole batch lands, or (on any error)
// none of it does. Thin wrapper around UpsertDeleteAndIntegrity with
// integrity=nil (leave the integrity table untouched) -- see that
// function's doc comment for the shared transactional guarantee.
func (s *PingScoreHistoryStore) UpsertAndDelete(upserts []PingScoreHistoryEntry, deleteTxIDs []int64) error {
	return s.UpsertDeleteAndIntegrity(upserts, deleteTxIDs, nil)
}

// UpsertDeleteAndIntegrity applies a batch of upserts, a set of tx_ids to
// delete, and (when integrity is non-nil) an integrity-metadata write, all
// as ONE SQL transaction. Thin wrapper around UpsertDeleteAndMetadata with
// gap=nil and historyInitializedAt=nil -- unchanged signature and behavior
// from Phase 4D, preserved verbatim for every existing caller. See
// UpsertDeleteAndMetadata's own doc comment for the full transactional
// guarantee this and UpsertAndDelete both share.
func (s *PingScoreHistoryStore) UpsertDeleteAndIntegrity(upserts []PingScoreHistoryEntry, deleteTxIDs []int64, integrity *PingScoreHistoryIntegrity) error {
	return s.UpsertDeleteAndMetadata(upserts, deleteTxIDs, integrity, nil, nil)
}

// UpsertDeleteAndMetadata applies a batch of upserts, a set of tx_ids to
// delete, and up to three optional metadata writes -- an integrity record,
// a gap record, and the one-time history-initialized marker -- all as ONE
// SQL transaction: either everything lands, or (on any error) NONE of it
// does -- the deferred Rollback is a no-op after a successful Commit, and
// fires on every error path before this function returns.
//
// This exists (Phase 4D, extended in the fix-round-2 review of a1c3022d)
// because none of these metadata writes are safe to persist as a SEPARATE
// transaction from the SAME cycle's upserts/deletes: if the process
// crashes between two separate writes, a reader could otherwise see
// freshly-computed entries (e.g. one newly flagged
// PermanentlyUnreconstructable) without the gap record that explains it,
// or vice versa.
//
//   - integrity == nil means "don't touch the integrity table this cycle"
//     (leaving whatever was there, including any PREVIOUSLY recorded
//     abnormal status, untouched -- see PingScoreHistoryIntegrity).
//   - gap == nil means "don't touch the gaps table this cycle" -- same
//     never-silently-clear convention (see PingScoreHistoryGap and
//     buildPingScoreHistoryGap, which is the only place that ever decides
//     what to pass here).
//   - historyInitializedAt == nil means "don't touch the history-
//     initialized marker this cycle" (it is written EXACTLY once, ever,
//     by the genuine first successful cycle -- see HistoryInitializedAt
//     and isGenuineBootstrap in ping_score_history_engine.go). A non-nil
//     value is an RFC3339 timestamp to record under the `_meta` key
//     'history_initialized_at'.
func (s *PingScoreHistoryStore) UpsertDeleteAndMetadata(upserts []PingScoreHistoryEntry, deleteTxIDs []int64, integrity *PingScoreHistoryIntegrity, gap *PingScoreHistoryGap, historyInitializedAt *string) error {
	if s.readOnly {
		return fmt.Errorf("ping score history store: read-only (on-disk schema is newer than this code understands)")
	}
	if len(upserts) == 0 && len(deleteTxIDs) == 0 && integrity == nil && gap == nil && historyInitializedAt == nil {
		return nil
	}

	tx, err := s.conn.Begin()
	if err != nil {
		return fmt.Errorf("ping score history store: begin transaction: %w", err)
	}
	defer tx.Rollback() // no-op once Commit has succeeded

	if len(upserts) > 0 {
		stmt, err := tx.Prepare(pingScoreHistoryUpsertSQL)
		if err != nil {
			return fmt.Errorf("ping score history store: prepare upsert: %w", err)
		}
		defer stmt.Close()
		for _, e := range upserts {
			if _, err := stmt.Exec(
				e.TxID, e.Hash, nullableString(e.Sender), nullableString(e.ChannelHash), e.Timestamp,
				e.StationCount, e.DeepestHops, nullableString(e.DeepestPubkey),
				nullableFloat(e.FarthestKm), nullableString(e.FarthestPubkey), nullableFloat(e.SpreadSeconds),
				nullableFloat(e.AirtimeMs), e.RelayCount, nullableString(e.RelayPubkeysJSON),
				nullableString(e.FirstPubkey), boolToInt(e.Unscorable),
				e.FingerprintCount, e.FingerprintMaxID, nullableString(e.StableSince),
				boolToInt(e.Settled), boolToInt(e.DataPruned), boolToInt(e.PermanentlyUnreconstructable),
				nullableString(e.LastDeepSweptAt), e.ComputedAt,
			); err != nil {
				return fmt.Errorf("ping score history store: upsert tx_id=%d: %w", e.TxID, err)
			}
		}
	}

	for start := 0; start < len(deleteTxIDs); start += pingScoreHistoryDeleteChunkSize {
		end := start + pingScoreHistoryDeleteChunkSize
		if end > len(deleteTxIDs) {
			end = len(deleteTxIDs)
		}
		chunk := deleteTxIDs[start:end]
		placeholders := make([]byte, 0, len(chunk)*2)
		args := make([]interface{}, len(chunk))
		for i, id := range chunk {
			if i > 0 {
				placeholders = append(placeholders, ',')
			}
			placeholders = append(placeholders, '?')
			args[i] = id
		}
		query := "DELETE FROM ping_score_history_entries WHERE tx_id IN (" + string(placeholders) + ")"
		if _, err := tx.Exec(query, args...); err != nil {
			return fmt.Errorf("ping score history store: delete chunk [%d:%d]: %w", start, end, err)
		}
	}

	if integrity != nil {
		if _, err := tx.Exec(`
			INSERT INTO ping_score_history_integrity
				(id, status, detected_at, total_triggers, scored_count, unreconstructable_count, rows_recovered, detail)
			VALUES (1, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				status=excluded.status, detected_at=excluded.detected_at,
				total_triggers=excluded.total_triggers, scored_count=excluded.scored_count,
				unreconstructable_count=excluded.unreconstructable_count, rows_recovered=excluded.rows_recovered,
				detail=excluded.detail`,
			integrity.Status, integrity.DetectedAt, integrity.TotalTriggers, integrity.ScoredCount,
			integrity.UnreconstructableCount, integrity.RowsRecovered, nullableString(integrity.Detail),
		); err != nil {
			return fmt.Errorf("ping score history store: store integrity: %w", err)
		}
	}

	if gap != nil {
		if _, err := tx.Exec(`
			INSERT INTO ping_score_history_gaps
				(id, status, first_detected_at, last_detected_at, permanently_unreconstructable_count, total_triggers, detail)
			VALUES (1, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				status=excluded.status, first_detected_at=excluded.first_detected_at,
				last_detected_at=excluded.last_detected_at,
				permanently_unreconstructable_count=excluded.permanently_unreconstructable_count,
				total_triggers=excluded.total_triggers, detail=excluded.detail`,
			gap.Status, gap.FirstDetectedAt, gap.LastDetectedAt,
			gap.PermanentlyUnreconstructableCount, gap.TotalTriggers, nullableString(gap.Detail),
		); err != nil {
			return fmt.Errorf("ping score history store: store gap: %w", err)
		}
	}

	if historyInitializedAt != nil {
		// ON CONFLICT DO NOTHING (not DO UPDATE): write-once is
		// DATABASE-enforced here, not merely a convention Cycle's own
		// isGenuineBootstrap gating happens to uphold (fix-round-4 review
		// of bea755e5) -- if this is ever somehow called a second time
		// with a different value (a bug elsewhere, concurrent/duplicate
		// initialization), the ORIGINAL value silently wins rather than
		// being overwritten, exactly matching FirstDetectedAt's own
		// never-revised contract on the gap record.
		if _, err := tx.Exec(
			`INSERT INTO _meta (key, value) VALUES ('history_initialized_at', ?)
			 ON CONFLICT(key) DO NOTHING`,
			*historyInitializedAt,
		); err != nil {
			return fmt.Errorf("ping score history store: store history_initialized_at: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ping score history store: commit: %w", err)
	}
	return nil
}

// LoadAll returns every persisted entry, ordered by tx_id. Column list is
// explicit (never `SELECT *`), so a future ADD COLUMN (or an unrecognized
// future column from a newer schema version) never changes what this
// specific code version reads.
func (s *PingScoreHistoryStore) LoadAll() ([]PingScoreHistoryEntry, error) {
	rows, err := s.conn.Query(`
		SELECT tx_id, hash, sender, channel_hash, timestamp, station_count, deepest_hops,
			deepest_pubkey, farthest_km, farthest_pubkey, spread_seconds, airtime_ms,
			relay_count, relay_pubkeys_json, first_pubkey, unscorable,
			fingerprint_count, fingerprint_max_id, stable_since, settled, data_pruned,
			permanently_unreconstructable, last_deep_swept_at, computed_at
		FROM ping_score_history_entries
		ORDER BY tx_id`)
	if err != nil {
		return nil, fmt.Errorf("ping score history store: load all: %w", err)
	}
	defer rows.Close()

	var out []PingScoreHistoryEntry
	for rows.Next() {
		var e PingScoreHistoryEntry
		var sender, channelHash, deepestPubkey, farthestPubkey, relayPubkeysJSON, firstPubkey sql.NullString
		var stableSince, lastDeepSweptAt sql.NullString
		var farthestKm, spreadSeconds, airtimeMs sql.NullFloat64
		var relayCount sql.NullInt64
		var unscorable, settled, dataPruned, permanentlyUnreconstructable int
		if err := rows.Scan(
			&e.TxID, &e.Hash, &sender, &channelHash, &e.Timestamp, &e.StationCount, &e.DeepestHops,
			&deepestPubkey, &farthestKm, &farthestPubkey, &spreadSeconds, &airtimeMs,
			&relayCount, &relayPubkeysJSON, &firstPubkey, &unscorable,
			&e.FingerprintCount, &e.FingerprintMaxID, &stableSince, &settled, &dataPruned,
			&permanentlyUnreconstructable, &lastDeepSweptAt, &e.ComputedAt,
		); err != nil {
			return nil, fmt.Errorf("ping score history store: scan row: %w", err)
		}
		e.Sender = sender.String
		e.ChannelHash = channelHash.String
		e.DeepestPubkey = deepestPubkey.String
		e.FarthestPubkey = farthestPubkey.String
		e.RelayPubkeysJSON = relayPubkeysJSON.String
		e.FirstPubkey = firstPubkey.String
		e.StableSince = stableSince.String
		e.LastDeepSweptAt = lastDeepSweptAt.String
		if farthestKm.Valid {
			v := farthestKm.Float64
			e.FarthestKm = &v
		}
		if spreadSeconds.Valid {
			v := spreadSeconds.Float64
			e.SpreadSeconds = &v
		}
		if airtimeMs.Valid {
			v := airtimeMs.Float64
			e.AirtimeMs = &v
		}
		if relayCount.Valid {
			e.RelayCount = int(relayCount.Int64)
		}
		e.Unscorable = unscorable != 0
		e.Settled = settled != 0
		e.DataPruned = dataPruned != 0
		e.PermanentlyUnreconstructable = permanentlyUnreconstructable != 0
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ping score history store: row iteration: %w", err)
	}
	return out, nil
}

// StoreIntegrity persists (upserting the singleton row) the current
// integrity state of the store.
func (s *PingScoreHistoryStore) StoreIntegrity(integrity PingScoreHistoryIntegrity) error {
	if s.readOnly {
		return fmt.Errorf("ping score history store: read-only (on-disk schema is newer than this code understands)")
	}
	_, err := s.conn.Exec(`
		INSERT INTO ping_score_history_integrity
			(id, status, detected_at, total_triggers, scored_count, unreconstructable_count, rows_recovered, detail)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			status=excluded.status, detected_at=excluded.detected_at,
			total_triggers=excluded.total_triggers, scored_count=excluded.scored_count,
			unreconstructable_count=excluded.unreconstructable_count, rows_recovered=excluded.rows_recovered,
			detail=excluded.detail`,
		integrity.Status, integrity.DetectedAt, integrity.TotalTriggers, integrity.ScoredCount,
		integrity.UnreconstructableCount, integrity.RowsRecovered, nullableString(integrity.Detail))
	if err != nil {
		return fmt.Errorf("ping score history store: store integrity: %w", err)
	}
	return nil
}

// LoadIntegrity returns the persisted integrity state, or nil if none has
// ever been recorded (the normal, healthy case).
func (s *PingScoreHistoryStore) LoadIntegrity() (*PingScoreHistoryIntegrity, error) {
	var integrity PingScoreHistoryIntegrity
	var detail sql.NullString
	err := s.conn.QueryRow(`
		SELECT status, detected_at, total_triggers, scored_count, unreconstructable_count, rows_recovered, detail
		FROM ping_score_history_integrity WHERE id = 1`,
	).Scan(&integrity.Status, &integrity.DetectedAt, &integrity.TotalTriggers, &integrity.ScoredCount,
		&integrity.UnreconstructableCount, &integrity.RowsRecovered, &detail)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ping score history store: load integrity: %w", err)
	}
	integrity.Detail = detail.String
	return &integrity, nil
}

// StoreGap persists (upserting the singleton row) the current gap state --
// see PingScoreHistoryGap's own doc comment. Exists mainly for tests (pre-
// seeding a gap record); Cycle's normal path writes it atomically via
// UpsertDeleteAndMetadata instead, never through this method directly.
func (s *PingScoreHistoryStore) StoreGap(gap PingScoreHistoryGap) error {
	if s.readOnly {
		return fmt.Errorf("ping score history store: read-only (on-disk schema is newer than this code understands)")
	}
	_, err := s.conn.Exec(`
		INSERT INTO ping_score_history_gaps
			(id, status, first_detected_at, last_detected_at, permanently_unreconstructable_count, total_triggers, detail)
		VALUES (1, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			status=excluded.status, first_detected_at=excluded.first_detected_at,
			last_detected_at=excluded.last_detected_at,
			permanently_unreconstructable_count=excluded.permanently_unreconstructable_count,
			total_triggers=excluded.total_triggers, detail=excluded.detail`,
		gap.Status, gap.FirstDetectedAt, gap.LastDetectedAt,
		gap.PermanentlyUnreconstructableCount, gap.TotalTriggers, nullableString(gap.Detail))
	if err != nil {
		return fmt.Errorf("ping score history store: store gap: %w", err)
	}
	return nil
}

// LoadGap returns the persisted gap state, or nil if no
// permanently-unreconstructable entry has ever been proven over this
// store's whole lifetime (the normal, healthy case) -- see
// PingScoreHistoryGap's own doc comment.
func (s *PingScoreHistoryStore) LoadGap() (*PingScoreHistoryGap, error) {
	var gap PingScoreHistoryGap
	var detail sql.NullString
	err := s.conn.QueryRow(`
		SELECT status, first_detected_at, last_detected_at, permanently_unreconstructable_count, total_triggers, detail
		FROM ping_score_history_gaps WHERE id = 1`,
	).Scan(&gap.Status, &gap.FirstDetectedAt, &gap.LastDetectedAt,
		&gap.PermanentlyUnreconstructableCount, &gap.TotalTriggers, &detail)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ping score history store: load gap: %w", err)
	}
	gap.Detail = detail.String
	return &gap, nil
}

// HistoryInitializedAt returns the persisted RFC3339 timestamp of this
// store's genuine first successful engine cycle, and whether one has ever
// been recorded. This is the ONLY authoritative signal for "has bootstrap
// already happened" (see isGenuineBootstrap in
// ping_score_history_engine.go) -- deliberately NOT derived from whether
// the in-memory index is currently empty, which a later full-prune cycle
// (every tracked trigger deleted) could otherwise make indistinguishable
// from a store that has never bootstrapped at all. The underlying value is
// write-once at the DATABASE level (see UpsertDeleteAndMetadata's
// ON CONFLICT DO NOTHING) -- a second write attempt, whatever value it
// carries, can never change what this method returns.
func (s *PingScoreHistoryStore) HistoryInitializedAt() (string, bool, error) {
	var raw string
	err := s.conn.QueryRow(`SELECT value FROM _meta WHERE key = 'history_initialized_at'`).Scan(&raw)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("ping score history store: load history_initialized_at: %w", err)
	}
	return raw, true, nil
}

func nullableString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func nullableFloat(f *float64) interface{} {
	if f == nil {
		return nil
	}
	return *f
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
