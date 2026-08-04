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
// from anywhere else.
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
	"time"

	_ "modernc.org/sqlite"
)

// pingScoreHistorySchemaVersion is the schema version THIS code understands
// and will migrate an older on-disk file up to. A file with a HIGHER
// recorded version was written by newer code (e.g. a rolled-back deploy)
// and is opened read-only instead -- see openAndCheck.
const pingScoreHistorySchemaVersion = 1

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
	LastDeepSweptAt  string
	ComputedAt       string
}

// PingScoreHistoryIntegrity records an abnormal state of the history store
// that a consumer (Phase 4B+, and eventually the API) should surface
// rather than silently paper over. Fields not relevant to a given Status
// are left zero-valued.
type PingScoreHistoryIntegrity struct {
	// Status is one of: "ok", "initial-backfill-incomplete",
	// "degraded-unknown-version", "recovered-partial", "recovered-empty".
	Status     string
	DetectedAt string // RFC3339

	// Relevant to "initial-backfill-incomplete" (Phase 4B populates these;
	// Phase 4A only provides the storage for them).
	TotalTriggers          int
	ScoredCount            int
	UnreconstructableCount int

	// Relevant to "recovered-partial"/"recovered-empty".
	RowsRecovered int

	Detail string
}

// DefaultPingScoreHistoryPath derives the production location of the
// history store: a sibling file next to the main database, so it lives in
// the same persistent data directory/volume by construction (no separate
// config knob to get wrong). Not yet called from main.go in Phase 4A --
// wiring this in is Phase 4B's job.
func DefaultPingScoreHistoryPath(mainDBPath string) string {
	return filepath.Join(filepath.Dir(mainDBPath), "ping_scores_history.db")
}

// OpenPingScoreHistoryStore opens (creating if absent) the history store at
// path. It NEVER touches meshcore.db or any connection opened via OpenDB --
// this is a completely independent *sql.DB against a different file.
//
// Corruption handling: PRAGMA integrity_check runs on open. If it fails,
// the ORIGINAL file is renamed aside (never deleted) and a fresh, empty
// store is created in its place, with a "recovered-empty" integrity record
// explaining what happened and where the original file was preserved for
// manual recovery. Automatic row-wise partial recovery from a corrupt
// SQLite file is NOT implemented in this phase -- see the Phase 4A report's
// "known limitations" section for why, rather than faking a "best effort"
// that isn't actually reliable with this driver.
//
// Version handling: an on-disk schema_version newer than
// pingScoreHistorySchemaVersion means a newer server version wrote this
// file (e.g. a rolled-back deploy). The store opens successfully in
// READ-ONLY mode in that case -- LoadAll/LoadIntegrity still work, but
// UpsertAndDelete/StoreIntegrity return an error rather than risking
// writing a layout this code doesn't understand.
func OpenPingScoreHistoryStore(path string) (*PingScoreHistoryStore, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if _, err := os.Stat(dir); err != nil {
			return nil, fmt.Errorf("ping score history store: parent directory %q: %w", dir, err)
		}
	}

	store, corrupt, corruptDetail, err := openAndCheck(path)
	if err != nil {
		return nil, err
	}
	if !corrupt {
		return store, nil
	}

	// Corrupt: never delete, never silently keep using it. Preserve the
	// original file for manual recovery and start a fresh, empty store.
	store.conn.Close()
	backupPath := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
	if err := os.Rename(path, backupPath); err != nil {
		return nil, fmt.Errorf("ping score history store: rename corrupt file %s aside: %w", path, err)
	}
	fresh, stillCorrupt, _, err := openAndCheck(path)
	if err != nil {
		return nil, fmt.Errorf("ping score history store: create fresh store after corruption: %w", err)
	}
	if stillCorrupt {
		// A brand-new, just-created file failing its own integrity check
		// would indicate something wrong with the environment itself
		// (disk, filesystem) rather than the old file's content.
		fresh.conn.Close()
		return nil, fmt.Errorf("ping score history store: freshly created file at %s failed integrity_check", path)
	}
	integrity := PingScoreHistoryIntegrity{
		Status:     "recovered-empty",
		DetectedAt: time.Now().UTC().Format(time.RFC3339),
		Detail: fmt.Sprintf(
			"original file failed PRAGMA integrity_check (%s) and was preserved at %s for manual recovery; "+
				"automatic row-wise partial recovery is not implemented in this version",
			corruptDetail, backupPath),
	}
	if err := fresh.StoreIntegrity(integrity); err != nil {
		fresh.Close()
		return nil, fmt.Errorf("ping score history store: record corruption integrity state: %w", err)
	}
	return fresh, nil
}

// openAndCheck opens the connection, runs the integrity check, and -- if
// the file is intact -- applies any pending additive migrations or flips
// into read-only mode for an unrecognized future schema version. It does
// NOT handle the corrupt case beyond reporting it; the caller (which may
// need to close this connection and open a different path) does that.
func openAndCheck(path string) (store *PingScoreHistoryStore, corrupt bool, corruptDetail string, err error) {
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000", path)
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, false, "", fmt.Errorf("ping score history store: open %s: %w", path, err)
	}
	// Single connection: this store is only ever driven by one goroutine
	// (Phase 4B's design commits to this), and a single-connection pool
	// avoids any ambiguity about which connection sees which uncommitted
	// state within the process itself.
	conn.SetMaxOpenConns(1)
	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, false, "", fmt.Errorf("ping score history store: ping %s: %w", path, err)
	}
	s := &PingScoreHistoryStore{conn: conn, path: path}

	// A file that isn't a valid SQLite database at all (garbage bytes,
	// truncated, wrong format) fails the PRAGMA itself at the driver level
	// -- e.g. modernc.org/sqlite's "file is not a database (26)" -- rather
	// than returning a non-"ok" result row. Ping() above can succeed even
	// on such a file (it doesn't necessarily read page content), so this
	// is the first point corruption is actually detectable. Treat BOTH
	// failure shapes (a hard query error, or a non-"ok" result string) as
	// "corrupt": the safe, non-destructive recovery path is identical
	// either way, and refusing to start the server over a corrupt cache
	// file would be a worse outcome than recovering into a fresh one.
	var result string
	queryErr := conn.QueryRow(`PRAGMA integrity_check`).Scan(&result)
	if queryErr != nil {
		return s, true, queryErr.Error(), nil
	}
	if result != "ok" {
		return s, true, result, nil
	}

	version, err := s.readSchemaVersion()
	if err != nil {
		conn.Close()
		return nil, false, "", fmt.Errorf("ping score history store: read schema version: %w", err)
	}
	switch {
	case version < pingScoreHistorySchemaVersion:
		if err := s.migrateFrom(version); err != nil {
			conn.Close()
			return nil, false, "", fmt.Errorf("ping score history store: migrate from v%d: %w", version, err)
		}
	case version > pingScoreHistorySchemaVersion:
		// Newer than this code understands. Never migrate, never write --
		// just serve reads from whatever this code's own SELECTs (which
		// name every column explicitly, never `SELECT *`) can still find.
		s.readOnly = true
	}
	return s, false, "", nil
}

// ReadOnly reports whether this store was opened against a newer-than-
// understood schema version and is therefore refusing all writes.
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

func (s *PingScoreHistoryStore) readSchemaVersion() (int, error) {
	var tableExists int
	if err := s.conn.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='_meta'`,
	).Scan(&tableExists); err != nil {
		return 0, fmt.Errorf("check _meta existence: %w", err)
	}
	if tableExists == 0 {
		return 0, nil // brand-new file, nothing migrated yet
	}
	var raw string
	err := s.conn.QueryRow(`SELECT value FROM _meta WHERE key = 'schema_version'`).Scan(&raw)
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
	last_deep_swept_at, computed_at
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
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
// none of it does -- the deferred Rollback is a no-op after a successful
// Commit, and fires on every error path before this function returns.
func (s *PingScoreHistoryStore) UpsertAndDelete(upserts []PingScoreHistoryEntry, deleteTxIDs []int64) error {
	if s.readOnly {
		return fmt.Errorf("ping score history store: read-only (on-disk schema is newer than this code understands)")
	}
	if len(upserts) == 0 && len(deleteTxIDs) == 0 {
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
				boolToInt(e.Settled), boolToInt(e.DataPruned), nullableString(e.LastDeepSweptAt), e.ComputedAt,
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
			last_deep_swept_at, computed_at
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
		var unscorable, settled, dataPruned int
		if err := rows.Scan(
			&e.TxID, &e.Hash, &sender, &channelHash, &e.Timestamp, &e.StationCount, &e.DeepestHops,
			&deepestPubkey, &farthestKm, &farthestPubkey, &spreadSeconds, &airtimeMs,
			&relayCount, &relayPubkeysJSON, &firstPubkey, &unscorable,
			&e.FingerprintCount, &e.FingerprintMaxID, &stableSince, &settled, &dataPruned,
			&lastDeepSweptAt, &e.ComputedAt,
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
