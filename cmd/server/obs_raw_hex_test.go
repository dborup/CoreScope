package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// Upstream issue #1999, ported from Kpa-clawbot/CoreScope PR #2055: the
// packet-detail API returned the transmission's canonical frame for every
// observation, so the browser's hex view could not show the
// selected observation's actual bytes and could contradict the path_json shown
// beside it.
//
// The store drops observations.raw_hex on purpose (#881, ~98MB on a 1.7M
// observation store) on the assumption that one content hash means one frame.
// The firmware hashes payload and type independently of the relay path, so
// observations of a single transmission legitimately differ. Measured on a
// production DB: of the 3000 most recent transmissions, 1977 had more than one
// observation and 1844 of those held genuinely different frames, up to 51 for
// one packet.
//
// The fix reads them back on the detail path only, one query per request.

// seedDistinctFrames inserts one transmission and three observations whose
// stored frames differ in their path bytes, plus one with no stored frame at
// all so the canonical fallback stays covered. Returns the hash and the
// observation ids in insertion order.
func seedDistinctFrames(t *testing.T, db *DB, hash string) (string, []int) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.conn.Exec(`INSERT INTO transmissions
		(raw_hex, hash, first_seen, route_type, payload_type, decoded_json)
		VALUES ('CANON0000', ?, ?, 1, 5, '{"type":"CHAN"}')`, hash, now); err != nil {
		t.Fatalf("insert transmission: %v", err)
	}
	var txID int
	if err := db.conn.QueryRow("SELECT id FROM transmissions WHERE hash = ?", hash).Scan(&txID); err != nil {
		t.Fatalf("lookup tx id: %v", err)
	}

	frames := []struct {
		pathJSON string
		rawHex   interface{} // nil means SQL NULL: no stored frame
	}{
		{`["AA","BB"]`, "FRAME2HOPS"},
		{`["AA","BB","CC"]`, "FRAME3HOPS"},
		{`["AA"]`, "FRAME1HOP"},
		{`["DD"]`, nil},
	}
	var ids []int
	for i, f := range frames {
		res, err := db.conn.Exec(`INSERT INTO observations
			(transmission_id, observer_idx, snr, rssi, path_json, timestamp, raw_hex)
			VALUES (?, 1, 5.0, -90, ?, ?, ?)`,
			txID, f.pathJSON, time.Now().Unix()-int64(i), f.rawHex)
		if err != nil {
			t.Fatalf("insert observation %d: %v", i, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("last insert id: %v", err)
		}
		ids = append(ids, int(id))
	}
	return hash, ids
}

func TestObservationRawHexForHash(t *testing.T) {
	db := setupTestDB(t)
	// Production sets this in detectSchema (db.go) when the column exists; the
	// test schema has the column but the helper does not run detection.
	db.hasObsRawHexFlag.forceTrue()

	hash, ids := seedDistinctFrames(t, db, "1999aaaabbbbcccc")

	got, err := db.ObservationRawHexForHash(hash)
	if err != nil {
		t.Fatalf("ObservationRawHexForHash: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d frames, want 3 (the fourth observation stores none): %v", len(got), got)
	}
	want := map[int]string{ids[0]: "FRAME2HOPS", ids[1]: "FRAME3HOPS", ids[2]: "FRAME1HOP"}
	for id, wantHex := range want {
		if got[id] != wantHex {
			t.Errorf("observation %d: got %q, want %q", id, got[id], wantHex)
		}
	}
	// The observation with a NULL frame must be absent rather than empty, so
	// the caller falls back to the transmission's canonical bytes.
	if _, present := got[ids[3]]; present {
		t.Errorf("observation %d has no stored frame and must not appear in the map", ids[3])
	}

	// Unknown hash: no rows, no error, no panic.
	if got, err := db.ObservationRawHexForHash("0000000000000000"); err != nil || len(got) != 0 {
		if err != nil {
			t.Errorf("unknown hash returned error: %v", err)
		}
		t.Errorf("unknown hash returned %d frames, want 0", len(got))
	}
}

// TestObservationRawHexForHashRespectsSchemaFlag pins the guard: on a schema
// without observations.raw_hex the query must not be attempted at all, because
// it would be a SQL error rather than an empty result.
func TestObservationRawHexForHashRespectsSchemaFlag(t *testing.T) {
	db := setupTestDB(t)
	db.hasObsRawHexFlag.v.Store(false) // explicit: a schema without the column
	hash, _ := seedDistinctFrames(t, db, "1999ddddeeeeffff")
	if got, err := db.ObservationRawHexForHash(hash); err != nil || got != nil {
		if err != nil {
			t.Errorf("hasObsRawHex() false returned error: %v", err)
		}
		t.Errorf("hasObsRawHex() false must return nil, got %v", got)
	}
}

func TestObservationRawHexForHashReturnsDatabaseErrors(t *testing.T) {
	db := setupTestDB(t)
	db.hasObsRawHexFlag.forceTrue()
	if err := db.conn.Close(); err != nil {
		t.Fatalf("close test database: %v", err)
	}

	got, err := db.ObservationRawHexForHash("1999aaaabbbbcccc")
	if err == nil {
		t.Fatalf("got frames %v and nil error from a closed database, want a visible error", got)
	}
	if !strings.Contains(err.Error(), "lookup transmission for observation frames") {
		t.Fatalf("error %q does not identify the failed operation", err)
	}
}

// newStoreBackedRouter serves the packet API from a loaded in-memory store, so
// transmissions seeded before the call take the handler's store path.
func newStoreBackedRouter(t *testing.T, db *DB) (*Server, *mux.Router) {
	t.Helper()
	srv := NewServer(db, &Config{Port: 3000}, NewHub())
	store := NewPacketStore(db, nil)
	if err := store.Load(); err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if !store.WaitIndexesReady(5 * time.Second) {
		t.Fatal("background indexes never became ready")
	}
	srv.store = store
	router := mux.NewRouter()
	srv.RegisterRoutes(router)
	return srv, router
}

// assertGenericFrameFailure pins the client-facing side of a failed
// observation-frame lookup: HTTP 500 with exactly the generic message, and
// nothing from the database error in the body. The deny-list is checked
// case-insensitively; none of its entries occur in the generic message itself
// ("observation frames" is not "observations").
func assertGenericFrameFailure(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	body := w.Body.String()
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500 for failed observation-frame lookup (body: %s)", w.Code, body)
	}
	if got, want := strings.TrimSpace(body), `{"error":"Failed to load observation frames"}`; got != want {
		t.Fatalf("body = %s, want exactly %s", got, want)
	}
	lower := strings.ToLower(body)
	for _, leak := range []string{"no such column", "sql", "sqlite", "raw_hex", "observations", ":memory:", ".db", "/"} {
		if strings.Contains(lower, leak) {
			t.Errorf("response body leaks %q: %s", leak, body)
		}
	}
}

func TestPacketDetailReturns500WhenObservationFrameLookupFails(t *testing.T) {
	db := setupTestDB(t)
	db.hasObsRawHexFlag.forceTrue()
	hash, _ := seedDistinctFrames(t, db, "1999deadbeef0011")

	_, router := newStoreBackedRouter(t, db)
	if err := db.conn.Close(); err != nil {
		t.Fatalf("close test database: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/packets/"+hash, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assertGenericFrameFailure(t, w)
}

// TestObservationFrameFailuresAfterTransmissionLookup covers the error branches
// that come after the transmission lookup succeeded: the observation query, the
// row scan, and an error raised while stepping to a later row (rows.Err). Each
// is provoked through a real SQLite schema change made after the store loaded
// and after the column was detected, not through a test seam:
//   - query:   observations.raw_hex is dropped (schema drift under a stale flag);
//   - scan:    observations becomes a view whose id is not an integer;
//   - iterate: observations becomes a view whose raw_hex expression fails at
//     runtime on every stored frame except the first one the query returns.
//     The driver steps the first row inside Query, so the failure surfaces
//     from rows.Next. Rows without a frame are left alone because the scan
//     may visit them before the first match, and the failing expression
//     depends on the row (`'{' || id`) because SQLite hoists constant ones.
func TestObservationFrameFailuresAfterTransmissionLookup(t *testing.T) {
	const viewCols = `transmission_id, observer_idx, direction, snr, rssi, score,
		path_json, timestamp, resolved_path`
	cases := []struct {
		name    string
		wantErr string
		breakDB func(t *testing.T, db *DB, txHash string)
	}{
		{"query", "query observation frames", func(t *testing.T, db *DB, _ string) {
			mustExec(t, db, `ALTER TABLE observations DROP COLUMN raw_hex`)
		}},
		{"scan", "scan observation frame", func(t *testing.T, db *DB, _ string) {
			mustExec(t, db, `ALTER TABLE observations RENAME TO observations_real`)
			mustExec(t, db, `CREATE VIEW observations AS SELECT 'not-an-int' AS id, `+viewCols+`,
				raw_hex FROM observations_real`)
		}},
		{"iterate", "iterate observation frames", func(t *testing.T, db *DB, txHash string) {
			// The row the production query returns first must still succeed,
			// otherwise the error would surface from Query instead. Ask SQLite
			// with the same statement ObservationRawHexForHash runs.
			var txID, firstID int
			if err := db.conn.QueryRow("SELECT id FROM transmissions WHERE hash = ?", txHash).Scan(&txID); err != nil {
				t.Fatalf("find transmission: %v", err)
			}
			if err := db.conn.QueryRow(
				`SELECT id, raw_hex FROM observations
		 WHERE transmission_id = ? AND raw_hex IS NOT NULL AND raw_hex <> ''`, txID).Scan(&firstID, new(string)); err != nil {
				t.Fatalf("find first frame row: %v", err)
			}
			mustExec(t, db, `ALTER TABLE observations RENAME TO observations_real`)
			mustExec(t, db, fmt.Sprintf(`CREATE VIEW observations AS SELECT id, `+viewCols+`,
				CASE WHEN id <> %d AND raw_hex IS NOT NULL AND raw_hex <> ''
					THEN json_extract('{' || id, '$') ELSE raw_hex END AS raw_hex
				FROM observations_real`, firstID))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := setupTestDB(t)
			db.hasObsRawHexFlag.forceTrue()
			hash, _ := seedDistinctFrames(t, db, "1999c0ffee000011")
			_, router := newStoreBackedRouter(t, db)
			tc.breakDB(t, db, hash)

			got, err := db.ObservationRawHexForHash(hash)
			if err == nil {
				t.Fatalf("got frames %v and nil error, want the %s error", got, tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not identify the failed operation %q", err, tc.wantErr)
			}

			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/packets/"+hash, nil))
			assertGenericFrameFailure(t, w)
		})
	}
}

// TestPacketDetailExposesPerObservationFrames is the regression the issue asks
// for: the detail response must give each observation its own bytes, and fall
// back to the transmission's only where none are stored.
func TestPacketDetailExposesPerObservationFrames(t *testing.T) {
	srv, router := setupTestServer(t)
	srv.db.hasObsRawHexFlag.forceTrue()

	// Inserted after store.Load(), so this transmission is DB-only and the
	// handler takes its DB-fallback path. Both paths converge on the same
	// backfill, and the DB path is the one that can be set up deterministically.
	hash, ids := seedDistinctFrames(t, srv.db, "1999112233445566")

	req := httptest.NewRequest("GET", "/api/packets/"+hash, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	obsList, _ := body["observations"].([]interface{})
	if len(obsList) != 4 {
		t.Fatalf("got %d observations, want 4", len(obsList))
	}

	byID := map[int]string{}
	for _, raw := range obsList {
		m, ok := raw.(map[string]interface{})
		if !ok {
			t.Fatalf("observation is not an object: %T", raw)
		}
		idF, ok := m["id"].(float64) // JSON numbers decode as float64
		if !ok {
			t.Fatalf("observation has no numeric id: %v", m["id"])
		}
		hx, _ := m["raw_hex"].(string)
		byID[int(idF)] = hx
	}

	for i, wantHex := range []string{"FRAME2HOPS", "FRAME3HOPS", "FRAME1HOP"} {
		if got := byID[ids[i]]; got != wantHex {
			t.Errorf("observation %d: raw_hex = %q, want its own frame %q", ids[i], got, wantHex)
		}
	}
	// The one without stored bytes gets the transmission's canonical frame. On
	// this path that is new: the DB observation query selects no raw_hex, so
	// before the fix the field was missing from those observations entirely.
	if got := byID[ids[3]]; got != "CANON0000" {
		t.Errorf("observation %d: raw_hex = %q, want the canonical fallback %q", ids[3], got, "CANON0000")
	}

	// The whole point: the frames are not all the same value.
	distinct := map[string]bool{}
	for _, hx := range byID {
		distinct[hx] = true
	}
	if len(distinct) < 3 {
		t.Errorf("only %d distinct frames across 4 observations (%v) — the canonical frame is still being repeated", len(distinct), byID)
	}
}

// TestPacketDetailExposesPerObservationFramesFromStore covers the path almost
// every real request takes: the transmission IS in the in-memory store.
//
// This is the case a DB-fallback-only test misses. enrichObsWithTx already puts
// the transmission's canonical frame into every observation map, so a backfill
// that skipped observations which "already have" raw_hex would skip all of them
// and leave the defect untouched on the main path. A stored frame has to win
// over that placeholder.
func TestPacketDetailExposesPerObservationFramesFromStore(t *testing.T) {
	db := setupTestDB(t)
	seedTestData(t, db)
	db.hasObsRawHexFlag.forceTrue()

	// Seed BEFORE the store loads, so the store holds this transmission and the
	// handler never reaches its DB fallback.
	hash, ids := seedDistinctFrames(t, db, "1999aabbccdd0011")

	srv, router := newStoreBackedRouter(t, db)

	if got := srv.store.GetPacketByHash(hash); got == nil {
		t.Fatal("precondition failed: the store does not hold the seeded transmission")
	}

	req := httptest.NewRequest("GET", "/api/packets/"+hash, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	obsList, _ := body["observations"].([]interface{})
	if len(obsList) != 4 {
		t.Fatalf("got %d observations, want 4", len(obsList))
	}

	byID := map[int]string{}
	for _, raw := range obsList {
		m, _ := raw.(map[string]interface{})
		idF, ok := m["id"].(float64)
		if !ok {
			t.Fatalf("observation has no numeric id: %v", m["id"])
		}
		hx, _ := m["raw_hex"].(string)
		byID[int(idF)] = hx
	}

	for i, wantHex := range []string{"FRAME2HOPS", "FRAME3HOPS", "FRAME1HOP"} {
		if got := byID[ids[i]]; got != wantHex {
			t.Errorf("store path, observation %d: raw_hex = %q, want its own frame %q (a canonical value here means the backfill was skipped)", ids[i], got, wantHex)
		}
	}
	if got := byID[ids[3]]; got != "CANON0000" {
		t.Errorf("store path, observation %d: raw_hex = %q, want the canonical fallback %q", ids[3], got, "CANON0000")
	}
}
