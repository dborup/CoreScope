package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Issue #380, server side: the ingestor's decode backfill rewrites
// channel_hash, decoded_json and scope_name under a running server. The
// server never writes; it polls one revision counter and drops the caches
// that could otherwise keep serving a pre-rewrite channel list.

// dbfStateDDL is the ingestor's state table (cmd/ingestor/decode_backfill.go),
// created here exactly as the ingestor creates it.
const dbfStateDDL = `CREATE TABLE decode_backfill_state (
	id              INTEGER PRIMARY KEY CHECK (id = 1),
	kinds           TEXT    NOT NULL,
	watermark       INTEGER NOT NULL,
	ceiling         INTEGER NOT NULL,
	status          TEXT    NOT NULL,
	revision        INTEGER NOT NULL DEFAULT 0,
	scanned         INTEGER NOT NULL DEFAULT 0,
	channel_updated INTEGER NOT NULL DEFAULT 0,
	scope_updated   INTEGER NOT NULL DEFAULT 0,
	ambiguous       INTEGER NOT NULL DEFAULT 0,
	decode_errors   INTEGER NOT NULL DEFAULT 0,
	started_at      TEXT    NOT NULL,
	updated_at      TEXT    NOT NULL
)`

// dbfDB is a server test DB holding one undecryptable GRP_TXT row and the
// ingestor's decode-backfill state table.
func dbfDB(t *testing.T) *DB {
	t.Helper()
	db := setupTestDB(t)
	dbfExec(t, db, dbfStateDDL)
	dbfExec(t, db, `INSERT INTO decode_backfill_state
		(id, kinds, watermark, ceiling, status, revision, scanned, channel_updated, scope_updated, ambiguous, decode_errors, started_at, updated_at)
		VALUES (1, 'channel', 0, 10, 'running', 0, 0, 0, 0, 0, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	dbfExec(t, db, `INSERT INTO observers (id, name, iata) VALUES ('obs-a', 'obs-a', 'TST')`)
	ts := time.Now().UTC().Add(-time.Hour)
	dbfExec(t, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		VALUES (1, '1501ab3aaabb', 'dbf-1', ?, 1, 5, '{"type":"GRP_TXT","channelHashHex":"3A","decryptionStatus":"no_key"}', 'enc_3A')`, ts.Format(time.RFC3339))
	dbfExec(t, db, `INSERT INTO observations (id, transmission_id, observer_idx, path_json, timestamp) VALUES (1, 1, 1, '[]', ?)`, ts.Unix())
	return db
}

func dbfExec(t *testing.T, db *DB, q string, args ...interface{}) {
	t.Helper()
	if _, err := db.conn.Exec(q, args...); err != nil {
		t.Fatalf("exec %.60s: %v", q, err)
	}
}

// dbfRewrite is what the ingestor's batch commits: the row is resolved and
// the revision grows, in one transaction.
func dbfRewrite(t *testing.T, db *DB, id int, channel string) {
	t.Helper()
	tx, err := db.conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE transmissions SET channel_hash = ?, decoded_json = ?
		WHERE id = ? AND payload_type = 5 AND channel_hash LIKE 'enc_%'`,
		channel, `{"type":"CHAN","channel":"`+channel+`","channelHashHex":"3A","decryptionStatus":"decrypted","text":"Alice: hi"}`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE decode_backfill_state SET revision = revision + 1, channel_updated = channel_updated + 1, watermark = ? WHERE id = 1`, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// dbfChannelNames lists the names /api/channels would report, through the
// same cached DB path the endpoint uses.
func dbfChannelNames(t *testing.T, db *DB) []string {
	t.Helper()
	chans, err := db.GetChannels("")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range chans {
		if n, ok := c["hash"].(string); ok {
			out = append(out, n)
		}
	}
	return out
}

func dbfContains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// A rewrite committed while the server runs reaches /api/channels on the next
// poller tick, because the tick drops the channel list caches. Without the
// invalidation the 60s-TTL cache keeps serving the pre-rewrite list.
func TestDecodeBackfill_RewriteReachesChannelsAfterOneTick(t *testing.T) {
	db := dbfDB(t)
	s := NewPacketStore(db, nil)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	poll := newRMPoller(db, s)
	poll.tick() // records the revision baseline

	// Warm the cache with the pre-rewrite answer.
	if names := dbfChannelNames(t, db); dbfContains(names, "#dbf") {
		t.Fatalf("precondition: #dbf already listed: %v", names)
	}

	dbfRewrite(t, db, 1, "#dbf")

	// Still cached: the rewrite alone does not reach a warm cache.
	if names := dbfChannelNames(t, db); dbfContains(names, "#dbf") {
		t.Fatalf("cache served the rewrite before any invalidation: %v", names)
	}

	poll.tick()
	names := dbfChannelNames(t, db)
	if !dbfContains(names, "#dbf") {
		t.Errorf("/api/channels does not list the backfilled channel after one tick: %v", names)
	}
}

// The first poll only records the baseline, so a server starting next to a
// running backfill does not drop its caches on tick one; a later growth does.
func TestDecodeBackfill_FirstPollOnlyBaselines(t *testing.T) {
	db := dbfDB(t)
	dbfExec(t, db, `UPDATE decode_backfill_state SET revision = 7 WHERE id = 1`)
	s := NewPacketStore(db, nil)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if s.RefreshDecodeBackfill() {
		t.Error("the first poll invalidated; it must only record the baseline")
	}
	if s.RefreshDecodeBackfill() {
		t.Error("an unchanged revision invalidated")
	}
	dbfExec(t, db, `UPDATE decode_backfill_state SET revision = 8 WHERE id = 1`)
	if !s.RefreshDecodeBackfill() {
		t.Error("a grown revision did not invalidate")
	}
	// A revision that somehow went backwards (a restored database) must not
	// invalidate on every tick afterwards.
	dbfExec(t, db, `UPDATE decode_backfill_state SET revision = 2 WHERE id = 1`)
	if s.RefreshDecodeBackfill() {
		t.Error("a shrinking revision invalidated")
	}
}

// A database no #380 ingestor has touched has no state table: the server
// reports "absent" and stops querying it on every tick.
func TestDecodeBackfill_AbsentTableStopsPolling(t *testing.T) {
	db := setupTestDB(t)
	s := NewPacketStore(db, nil)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if s.RefreshDecodeBackfill() {
		t.Error("invalidated against a database without the state table")
	}
	s.decodeBackfillMu.Lock()
	state := s.decodeBackfillState
	s.decodeBackfillMu.Unlock()
	if state != -1 {
		t.Errorf("decodeBackfillState = %d, want -1 so the poller stops querying a missing table", state)
	}
	if got := db.decodeBackfillStatus(); got.Status != "absent" {
		t.Errorf("status = %q, want absent", got.Status)
	}
}

// The table can still appear while the server runs: on a joint deploy the
// ingestor creates it lazily on its first pass, which can land after this
// server's first tick. The poller must therefore re-probe on a slow cadence
// instead of latching off for good, and must take the revision it finds as a
// baseline rather than treating the rows already rewritten as news.
func TestDecodeBackfill_AbsentTableIsRecheckedAndPickedUp(t *testing.T) {
	db := setupTestDB(t)
	s := NewPacketStore(db, nil)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	s.RefreshDecodeBackfill() // table missing: latch off

	// The ingestor shows up and has already rewritten rows.
	dbfExec(t, db, dbfStateDDL)
	dbfExec(t, db, `INSERT INTO decode_backfill_state
		(id, kinds, watermark, ceiling, status, revision, scanned, channel_updated, scope_updated, ambiguous, decode_errors, started_at, updated_at)
		VALUES (1, 'channel', 9, 9, 'done', 4, 9, 2, 0, 0, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)

	// Ticks before the re-check window must not query it again. The latching
	// tick above did not count towards the window, so the window closes on
	// the decodeBackfillAbsentRecheck-th tick after it.
	for i := 0; i < decodeBackfillAbsentRecheck-1; i++ {
		if s.RefreshDecodeBackfill() {
			t.Fatalf("tick %d invalidated before the re-check window", i)
		}
	}
	s.decodeBackfillMu.Lock()
	early := s.decodeBackfillState
	s.decodeBackfillMu.Unlock()
	if early != -1 {
		t.Errorf("state = %d before the re-check window, want -1", early)
	}

	// The re-check tick finds the table and only records the baseline.
	if s.RefreshDecodeBackfill() {
		t.Error("the re-check tick invalidated; it must only baseline the revision it found")
	}
	s.decodeBackfillMu.Lock()
	state, rev := s.decodeBackfillState, s.decodeBackfillRev
	s.decodeBackfillMu.Unlock()
	if state != 1 || rev != 4 {
		t.Fatalf("state = %d rev = %d, want 1 and 4 after the table appeared", state, rev)
	}

	// From then on it behaves like a normal poll.
	dbfExec(t, db, `UPDATE decode_backfill_state SET revision = 5 WHERE id = 1`)
	if !s.RefreshDecodeBackfill() {
		t.Error("a revision written after the table appeared did not invalidate")
	}
}

// /api/healthz reports the ingestor's progress, abstentions and undecodable
// rows, read-only.
func TestDecodeBackfill_HealthzReportsProgress(t *testing.T) {
	db := dbfDB(t)
	dbfExec(t, db, `UPDATE decode_backfill_state SET status = 'running', kinds = 'channel,scope',
		watermark = 4, ceiling = 10, revision = 3, scanned = 40, channel_updated = 5,
		scope_updated = 2, ambiguous = 1, decode_errors = 7, updated_at = '2026-02-02T00:00:00Z' WHERE id = 1`)
	srv := &Server{db: db, cfg: &Config{}}
	prev := readiness.Swap(1)
	t.Cleanup(func() { readiness.Store(prev) })
	rec := httptest.NewRecorder()
	srv.handleHealthz(rec, httptest.NewRequest(http.MethodGet, "/api/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		DecodeBackfill DecodeBackfillStatus `json:"decode_backfill"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	got := resp.DecodeBackfill
	want := DecodeBackfillStatus{Status: "running", Kinds: "channel,scope", Watermark: 4, Ceiling: 10,
		Revision: 3, Scanned: 40, ChannelUpdated: 5, ScopeUpdated: 2, Ambiguous: 1, DecodeErrors: 7,
		UpdatedAt: "2026-02-02T00:00:00Z"}
	if got != want {
		t.Errorf("decode_backfill = %+v, want %+v", got, want)
	}
}

// Documented boundary: an in-memory StoreTx loaded before the rewrite keeps
// its load-time decoded payload until the next server start, while the
// SQL-backed channel surface is fresh. Writing those strings in place would
// race the analytics snapshots that read them outside s.mu, so the pass does
// not; a restart (a fresh Load) picks the rewrite up.
func TestDecodeBackfill_InMemoryRowRefreshesOnRestartOnly(t *testing.T) {
	db := dbfDB(t)
	s := NewPacketStore(db, nil)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	poll := newRMPoller(db, s)
	poll.tick()
	dbfRewrite(t, db, 1, "#dbf")
	poll.tick()

	if dbfInMemoryDecoded(s, 1) != `{"type":"GRP_TXT","channelHashHex":"3A","decryptionStatus":"no_key"}` {
		t.Errorf("in-memory decoded_json changed under a running server: %s", dbfInMemoryDecoded(s, 1))
	}
	if names := dbfChannelNames(t, db); !dbfContains(names, "#dbf") {
		t.Errorf("the SQL-backed channel list must be fresh: %v", names)
	}

	restarted := NewPacketStore(db, nil)
	if err := restarted.Load(); err != nil {
		t.Fatal(err)
	}
	if got := dbfInMemoryDecoded(restarted, 1); got != `{"type":"CHAN","channel":"#dbf","channelHashHex":"3A","decryptionStatus":"decrypted","text":"Alice: hi"}` {
		t.Errorf("a restarted server did not load the rewritten decoded_json: %s", got)
	}
}

func dbfInMemoryDecoded(s *PacketStore, id int) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if tx := s.byTxID[id]; tx != nil {
		return tx.DecodedJSON
	}
	return ""
}

// The store-level channel cache (the no-database fallback of GET
// /api/channels) is dropped too, not only the DB-level list caches.
func TestDecodeBackfill_DropsStoreChannelCache(t *testing.T) {
	db := dbfDB(t)
	s := NewPacketStore(db, nil)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	poll := newRMPoller(db, s)
	poll.tick()
	s.GetChannels("") // populates channelsCacheRes
	s.channelsCacheMu.Lock()
	warm := s.channelsCacheRes != nil
	s.channelsCacheMu.Unlock()
	if !warm {
		t.Fatal("precondition: the store channel cache did not warm up")
	}

	dbfRewrite(t, db, 1, "#dbf")
	poll.tick()

	s.channelsCacheMu.Lock()
	still := s.channelsCacheRes != nil
	s.channelsCacheMu.Unlock()
	if still {
		t.Error("the store channel cache survived a decode-backfill rewrite")
	}
}
