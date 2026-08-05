package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	_ "modernc.org/sqlite"
)

// --- query-count instrumentation (test-only) -------------------------------
//
// Proves GetPacketPathsBulk/nearestPositionedNeighborsBulk genuinely batch
// (query count scales with chunk count, not with hash/pubkey count) and
// proves the VALUES-CTE parameter budget is exactly N per chunk of N
// targets, not 2N -- by wrapping modernc.org/sqlite's real driver.Conn and
// recording every QueryContext call's SQL text and bound-argument count.
// Confirmed empirically (scratch probe, not committed) that the driver
// implements driver.QueryerContext, so database/sql always calls through
// this wrapper for Query/QueryRow/QueryContext -- it never silently falls
// back to a Prepare+Stmt path this instrumentation wouldn't see.

type recordedQuery struct {
	sql      string
	argCount int
}

var (
	bulkTestQueriesMu sync.Mutex
	bulkTestQueries   []recordedQuery
)

func resetBulkTestQueryLog() {
	bulkTestQueriesMu.Lock()
	bulkTestQueries = nil
	bulkTestQueriesMu.Unlock()
	atomic.StoreInt64(&bulkTestQueryContextCalls, 0)
}

func bulkTestQueryLog() []recordedQuery {
	bulkTestQueriesMu.Lock()
	defer bulkTestQueriesMu.Unlock()
	out := make([]recordedQuery, len(bulkTestQueries))
	copy(out, bulkTestQueries)
	return out
}

type countingDriver struct{ inner driver.Driver }

func (d *countingDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: c}, nil
}

type countingConn struct{ driver.Conn }

// bulkTestFailAfterQueries, when >= 0, makes the (bulkTestFailAfterQueries+1)th
// QueryContext call onward fail with a synthetic error instead of reaching
// the real driver -- used to deterministically prove that a chunk failing
// partway through a multi-chunk operation discards any earlier chunks'
// results rather than returning them as a silent partial success. -1
// (the zero-adjusted default, set via clearBulkTestFailAfterQueries) disables
// fault injection entirely.
var bulkTestFailAfterQueries int64 = -1

func setBulkTestFailAfterQueries(n int64) {
	atomic.StoreInt64(&bulkTestFailAfterQueries, n)
}

func clearBulkTestFailAfterQueries() {
	atomic.StoreInt64(&bulkTestFailAfterQueries, -1)
}

var bulkTestQueryContextCalls int64

func (c *countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	n := atomic.AddInt64(&bulkTestQueryContextCalls, 1)
	bulkTestQueriesMu.Lock()
	bulkTestQueries = append(bulkTestQueries, recordedQuery{sql: query, argCount: len(args)})
	bulkTestQueriesMu.Unlock()
	if failAfter := atomic.LoadInt64(&bulkTestFailAfterQueries); failAfter >= 0 && n > failAfter {
		return nil, fmt.Errorf("injected test failure: query #%d (fail-after=%d)", n, failAfter)
	}
	qc, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return qc.QueryContext(ctx, query, args)
}

var registerCountingDriverOnce sync.Once

func registerCountingDriver() {
	registerCountingDriverOnce.Do(func() {
		tmp, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			panic(err)
		}
		realDriver := tmp.Driver()
		tmp.Close()
		sql.Register("sqlite-counting-ping-score-bulk", &countingDriver{inner: realDriver})
	})
}

var countingDBNameCounter int64

// setupPacketPathCountingDB builds an isolated, query-counting v3-schema DB
// covering only what GetPacketPath/GetPacketPathsBulk/
// nearestPositionedNeighbor/observationFingerprintsBulk touch. Deliberately
// smaller than setupTestDB's schema and on its own driver registration,
// since query-count instrumentation needs to intercept the actual
// database/sql driver, not just reuse an already-open :memory: connection.
func setupPacketPathCountingDB(t *testing.T) *DB {
	t.Helper()
	registerCountingDriver()
	clearBulkTestFailAfterQueries()
	t.Cleanup(clearBulkTestFailAfterQueries)
	name := fmt.Sprintf("bulktest%d", atomic.AddInt64(&countingDBNameCounter, 1))
	conn, err := sql.Open("sqlite-counting-ping-score-bulk", "file:"+name+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	schema := `
		CREATE TABLE nodes (
			public_key TEXT PRIMARY KEY, name TEXT, role TEXT, lat REAL, lon REAL
		);
		CREATE TABLE observers (
			id TEXT PRIMARY KEY, name TEXT, iata TEXT
		);
		CREATE TABLE transmissions (
			id INTEGER PRIMARY KEY AUTOINCREMENT, raw_hex TEXT NOT NULL, hash TEXT NOT NULL UNIQUE,
			first_seen TEXT NOT NULL, route_type INTEGER, payload_type INTEGER, decoded_json TEXT,
			channel_hash TEXT DEFAULT NULL
		);
		CREATE TABLE observations (
			id INTEGER PRIMARY KEY AUTOINCREMENT, transmission_id INTEGER NOT NULL REFERENCES transmissions(id),
			observer_idx INTEGER, snr REAL, rssi REAL, path_json TEXT, timestamp INTEGER NOT NULL, resolved_path TEXT
		);
		CREATE TABLE neighbor_edges (
			node_a TEXT NOT NULL, node_b TEXT NOT NULL, count INTEGER DEFAULT 1, last_seen TEXT,
			PRIMARY KEY (node_a, node_b)
		);
	`
	if _, err := conn.Exec(schema); err != nil {
		t.Fatal(err)
	}
	db := &DB{conn: conn}
	db.isV3Flag.forceTrue()
	db.hasResolvedPathFlag.forceTrue()
	return db
}

// --- GetPacketPathsBulk: golden equivalence against GetPacketPath ----------

// TestGetPacketPathsBulk_MatchesGetPacketPath_RichFixture is the core
// golden test: one DB seeded with five hashes covering First-vs-deepest-
// branch divergence, weighted-neighbor-centroid fallback for both a hop
// point and an observer, (0,0) null-island exclusion, ambiguous
// name-match skipping, and observer-position source ordering (own GPS >
// name match > IATA) -- plus one hash never inserted at all. All five are
// requested from GetPacketPathsBulk in a single call (forcing the shared
// nodeByPK/nodeByName/neighbor-estimate resolution to run jointly across
// all of them, exactly the risky part of this refactor) and each is
// compared field-for-field against an independent GetPacketPath call for
// that same hash. Every fixture keeps branch hop-counts distinct within a
// hash -- resp.Branches is built from iterating a Go map, so if two
// branches tied on Hops their relative order would be nondeterministic
// between the single and bulk paths' independently-populated maps; that's
// pre-existing GetPacketPath behavior, not something this phase changes,
// so the fixtures simply avoid exercising it.
func TestGetPacketPathsBulk_MatchesGetPacketPath_RichFixture(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	db.conn.Exec(`CREATE TABLE IF NOT EXISTS neighbor_edges (node_a TEXT NOT NULL, node_b TEXT NOT NULL, count INTEGER DEFAULT 1, last_seen TEXT, PRIMARY KEY (node_a, node_b))`)

	// Hash "first": First (earliest, shallow) diverges from Branches[0]
	// (deepest, later) -- mirrors TestGetPacketPath_First.
	db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES ('obsEarly', 'Observer Early', 'SJC')`)
	db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES ('obsDeep', 'Observer Deep', 'SFO')`)
	db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		VALUES ('AA', 'bulkfirst0000001', '2026-01-15T10:00:00Z', 1, 5,
		'{"type":"CHAN","channel":"#ping","text":"ping","sender":"Eve"}', '#ping')`)
	db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp)
		VALUES (1, 1, 9.0, -88, '[]', 100)`)
	db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp)
		VALUES (1, 2, 4.0, -95, '["aa","bb","cc"]', 200)`)

	// Hash "approx": hop point AND observer both fall back to the
	// weighted-neighbor-centroid estimate -- mirrors
	// TestGetPacketPath_FallsBackToWeightedNeighborCentroid.
	db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES ('obsghost2', 'Ghost Observer Two', NULL)`)
	db.conn.Exec(`INSERT INTO nodes (public_key, name, role) VALUES ('pkghost2', 'GhostRepeaterTwo', 'repeater')`)
	db.conn.Exec(`INSERT INTO nodes (public_key, name, role) VALUES ('obsghost2', 'Ghost Observer Two', 'repeater')`)
	db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES ('pkanchor2', 'AnchorRepeaterTwo', 'repeater', 55.5, 9.5)`)
	db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES ('pkanchor3', 'AnchorRepeaterThree', 'repeater', 55.6, 9.6)`)
	db.conn.Exec(`INSERT INTO neighbor_edges (node_a, node_b, count) VALUES ('pkanchor2', 'pkghost2', 10)`)
	db.conn.Exec(`INSERT INTO neighbor_edges (node_a, node_b, count) VALUES ('pkanchor3', 'pkghost2', 6)`)
	db.conn.Exec(`INSERT INTO neighbor_edges (node_a, node_b, count) VALUES ('obsghost2', 'pkanchor2', 10)`)
	db.conn.Exec(`INSERT INTO neighbor_edges (node_a, node_b, count) VALUES ('obsghost2', 'pkanchor3', 6)`)
	db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		VALUES ('AA', 'bulkapprox000001', '2026-01-15T10:00:00Z', 1, 5,
		'{"type":"CHAN","channel":"#ping","text":"ping","sender":"Eve"}', '#ping')`)
	db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, resolved_path, timestamp)
		VALUES (2, 3, 4.0, -95, '["aa"]', '["pkghost2"]', 1736935200)`)

	// Hash "nullisland": (0,0) sentinel exclusion -- mirrors
	// TestGetPacketPath_ExcludesNullIsland.
	db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES ('obsZero2', 'Zero Observer Two', NULL)`)
	db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES ('pkZero2', 'ZeroRepeaterTwo', 'repeater', 0, 0)`)
	db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES ('obsZero2', 'Zero Observer Two', 'repeater', 0, 0)`)
	db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		VALUES ('AA', 'bulknull00000001', '2026-01-15T10:00:00Z', 1, 5,
		'{"type":"CHAN","channel":"#ping","text":"ping","sender":"Eve"}', '#ping')`)
	db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, resolved_path, timestamp)
		VALUES (3, 4, 9.0, -88, '["aa"]', '["pkZero2"]', 1736935200)`)

	// Hash "ambiguous": two positioned nodes share the observer's display
	// name -- name-fallback must skip it (ambiguous), same as
	// TestGetPacketPath_ObserverPositionSkipsAmbiguousNameMatch.
	db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES ('obsAmbig2', 'Ambiguous Two', NULL)`)
	db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES ('pkAmbigA2', 'Ambiguous Two', 'repeater', 10.0, 10.0)`)
	db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES ('pkAmbigB2', 'Ambiguous Two', 'repeater', 20.0, 20.0)`)
	db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		VALUES ('AA', 'bulkambig0000001', '2026-01-15T10:00:00Z', 1, 5,
		'{"type":"CHAN","channel":"#ping","text":"ping","sender":"Eve"}', '#ping')`)
	db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp)
		VALUES (4, 5, 9.0, -88, '[]', 1736935200)`)

	// Hash "ownpos": observer has its own GPS -- must be preferred over
	// name-match / IATA, mirrors TestGetPacketPath_ObserverPositionPrefersOwnGPS.
	// Reuses obsEarly's own node row for a real self-advertised GPS fix.
	db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES ('obsEarly', 'Observer Early', 'repeater', 61.0, 11.0)`)
	db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		VALUES ('AA', 'bulkownpos000001', '2026-01-15T10:00:00Z', 1, 5,
		'{"type":"CHAN","channel":"#ping","text":"ping","sender":"Eve"}', '#ping')`)
	db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp)
		VALUES (5, 1, 9.0, -88, '["dd","ee"]', 1736935200)`)

	hashes := []string{
		"bulkfirst0000001", "bulkapprox000001", "bulknull00000001",
		"bulkambig0000001", "bulkownpos000001", "bulkunknown0000x",
	}
	bulk, err := db.GetPacketPathsBulk(hashes, 500)
	if err != nil {
		t.Fatalf("GetPacketPathsBulk: %v", err)
	}

	if _, ok := bulk["bulkunknown0000x"]; ok {
		t.Errorf("bulk map contains bulkunknown0000x, want it absent (never observed)")
	}

	for _, hash := range hashes[:5] {
		single, err := db.GetPacketPath(hash, 500)
		if err != nil {
			t.Fatalf("GetPacketPath(%s): %v", hash, err)
		}
		gotBulk, ok := bulk[hash]
		if !ok {
			t.Fatalf("bulk map missing %s", hash)
		}
		if !reflect.DeepEqual(gotBulk, single) {
			t.Errorf("GetPacketPathsBulk(%s) != GetPacketPath(%s):\n bulk:   %+v\n single: %+v", hash, hash, dumpPacketPathResponse(gotBulk), dumpPacketPathResponse(single))
		}
	}
}

// dumpPacketPathResponse renders enough of a PacketPathResponse for a
// useful test-failure diff without hand-rolling a deep formatter --
// pointer fields print as addresses via %+v, so this dereferences the
// ones that matter for debugging a mismatch.
func dumpPacketPathResponse(r *PacketPathResponse) string {
	if r == nil {
		return "<nil>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Hash=%s TxID=%d First=%s Branches=[", r.Hash, r.TxID, dumpBranch(r.First))
	for i, br := range r.Branches {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(dumpBranch(&br))
	}
	b.WriteString("]")
	return b.String()
}

func dumpBranch(b *PacketPathBranch) string {
	if b == nil {
		return "<nil>"
	}
	obs := "<nil>"
	if b.Observer != nil {
		obs = fmt.Sprintf("{Name=%s PK=%s Lat=%v Lon=%v Approx=%v}", b.Observer.Name, b.Observer.PublicKey, derefF(b.Observer.Lat), derefF(b.Observer.Lon), b.Observer.Approx)
	}
	return fmt.Sprintf("{Hops=%d Points=%d Observer=%s Dist=%v}", b.Hops, len(b.Points), obs, derefF(b.DistanceFromFirstKm))
}

func derefF(f *float64) interface{} {
	if f == nil {
		return nil
	}
	return *f
}

func TestGetPacketPathsBulk_EmptyInput(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	result, err := db.GetPacketPathsBulk(nil, 0)
	if err != nil {
		t.Fatalf("GetPacketPathsBulk(nil): %v", err)
	}
	if len(result) != 0 {
		t.Errorf("result = %+v, want empty map", result)
	}
}

func TestGetPacketPathsBulk_NoResolvedPath(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	db.hasResolvedPathFlag.v.Store(false)

	_, err := db.GetPacketPathsBulk([]string{"whatever"}, 0)
	if err == nil {
		t.Fatal("GetPacketPathsBulk with hasResolvedPath=false: want error, got nil")
	}
}

func TestGetPacketPathsBulk_DuplicateHashesCollapse(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES ('obsDup', 'Observer Dup', 'SJC')`)
	db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		VALUES ('AA', 'bulkdup00000001', '2026-01-15T10:00:00Z', 1, 5,
		'{"type":"CHAN","channel":"#ping","text":"ping","sender":"Eve"}', '#ping')`)
	db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp)
		VALUES (1, 1, 9.0, -88, '[]', 100)`)

	result, err := db.GetPacketPathsBulk([]string{"bulkdup00000001", "BULKDUP00000001", "bulkdup00000001"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("result = %+v, want exactly 1 entry (case-insensitive dedup)", result)
	}
}

// --- GetPacketPathsBulk: query-count instrumentation -----------------------

// TestGetPacketPathsBulk_QueryCountIndependentOfHashCount proves the whole
// point of Phase 4B: total query count for a batch that fits in one chunk
// depends on the SHAPE of the queries (a small fixed set), not on how many
// hashes are in the batch. A 3-hash batch and a 30-hash batch (same
// per-hash shape, all within the 499-per-chunk limit) must issue the exact
// same number of queries.
func TestGetPacketPathsBulk_QueryCountIndependentOfHashCount(t *testing.T) {
	db := setupPacketPathCountingDB(t)
	defer db.Close()

	seedPacketPathFixture := func(batch string, n int) []string {
		hashes := make([]string, n)
		for i := 0; i < n; i++ {
			hash := fmt.Sprintf("qc%s%012d", batch, i)
			hashes[i] = hash
			pk := fmt.Sprintf("pk%s%012d", batch, i)
			if _, err := db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES (?, ?, 'repeater', ?, ?)`, pk, "Node"+pk, 55.0+float64(i)*0.001, 9.0); err != nil {
				t.Fatalf("insert node: %v", err)
			}
			obsInsert, err := db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES (?, ?, NULL)`, "obs"+pk, "Observer"+pk)
			if err != nil {
				t.Fatalf("insert observer: %v", err)
			}
			observerIdx, err := obsInsert.LastInsertId()
			if err != nil {
				t.Fatalf("observer LastInsertId: %v", err)
			}
			res, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
				VALUES ('AA', ?, '2026-01-15T10:00:00Z', 1, 5, '{}', '#ping')`, hash)
			if err != nil {
				t.Fatalf("insert transmission: %v", err)
			}
			txID, err := res.LastInsertId()
			if err != nil {
				t.Fatalf("LastInsertId: %v", err)
			}
			if _, err := db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, resolved_path, timestamp)
				VALUES (?, ?, 9.0, -88, '["aa"]', ?, 100)`, txID, observerIdx, `["`+pk+`"]`); err != nil {
				t.Fatalf("insert observation: %v", err)
			}
		}
		return hashes
	}

	smallHashes := seedPacketPathFixture("small", 3)
	resetBulkTestQueryLog()
	if _, err := db.GetPacketPathsBulk(smallHashes, 0); err != nil {
		t.Fatalf("GetPacketPathsBulk(small): %v", err)
	}
	smallCount := len(bulkTestQueryLog())

	largeHashes := seedPacketPathFixture("large", 30)
	resetBulkTestQueryLog()
	if _, err := db.GetPacketPathsBulk(largeHashes, 0); err != nil {
		t.Fatalf("GetPacketPathsBulk(large): %v", err)
	}
	largeCount := len(bulkTestQueryLog())

	if smallCount == 0 {
		t.Fatal("smallCount = 0, instrumentation isn't capturing queries")
	}
	if largeCount != smallCount {
		t.Errorf("query count = %d for %d hashes vs %d for %d hashes, want equal -- both fit in one chunk", largeCount, len(largeHashes), smallCount, len(smallHashes))
	}

	// Contrast with the naive N-separate-calls cost, to make the saving
	// concrete rather than just "not proportional".
	resetBulkTestQueryLog()
	for _, h := range largeHashes {
		if _, err := db.GetPacketPath(h, 0); err != nil {
			t.Fatalf("GetPacketPath(%s): %v", h, err)
		}
	}
	naiveCount := len(bulkTestQueryLog())
	if naiveCount <= largeCount {
		t.Errorf("naive per-hash query count = %d, bulk = %d for %d hashes -- expected bulk to be substantially cheaper", naiveCount, largeCount, len(largeHashes))
	}
}

// --- observationFingerprintsBulk --------------------------------------------

func TestObservationFingerprintsBulk_MatchesManualCounts(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES ('obsFp', 'Observer Fp', NULL)`)
	// tx1: 3 observations, tx2: 1 observation, tx3: 0 observations (never referenced by any observation row).
	db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		VALUES ('AA', 'fptx1', '2026-01-15T10:00:00Z', 1, 5, '{}', '#ping')`)
	db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		VALUES ('AA', 'fptx2', '2026-01-15T10:00:00Z', 1, 5, '{}', '#ping')`)
	db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		VALUES ('AA', 'fptx3', '2026-01-15T10:00:00Z', 1, 5, '{}', '#ping')`)
	for i := 0; i < 3; i++ {
		db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp) VALUES (1, 1, 9.0, -88, '[]', ?)`, 100+i)
	}
	db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp) VALUES (2, 1, 9.0, -88, '[]', 200)`)

	got, err := db.observationFingerprintsBulk([]int64{1, 2, 3, 999})
	if err != nil {
		t.Fatal(err)
	}
	fp1, ok := got[1]
	if !ok || fp1.Count != 3 {
		t.Errorf("got[1] = %+v (ok=%v), want Count=3", fp1, ok)
	}
	fp2, ok := got[2]
	if !ok || fp2.Count != 1 {
		t.Errorf("got[2] = %+v (ok=%v), want Count=1", fp2, ok)
	}
	if _, ok := got[3]; ok {
		t.Errorf("got[3] present, want absent -- tx3 has zero observations")
	}
	if _, ok := got[999]; ok {
		t.Errorf("got[999] present, want absent -- unknown txID")
	}
	// MaxID should be the highest observations.id for that transmission --
	// tx1's rows are inserted first (ids 1,2,3), tx2's is id 4.
	if fp1.MaxID != 3 {
		t.Errorf("got[1].MaxID = %d, want 3", fp1.MaxID)
	}
	if fp2.MaxID != 4 {
		t.Errorf("got[2].MaxID = %d, want 4", fp2.MaxID)
	}
}

func TestObservationFingerprintsBulk_EmptyInput(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	got, err := db.observationFingerprintsBulk(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got = %+v, want empty", got)
	}
}

func TestObservationFingerprintsBulk_DeduplicatesInput(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		VALUES ('AA', 'fpdup1', '2026-01-15T10:00:00Z', 1, 5, '{}', '#ping')`)
	db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp) VALUES (1, 1, 9.0, -88, '[]', 100)`)

	got, err := db.observationFingerprintsBulk([]int64{1, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[1].Count != 1 {
		t.Errorf("got = %+v, want a single entry {Count:1}", got)
	}
}

// TestObservationFingerprintsBulk_ChunksAcrossBoundary seeds 520 distinct
// transmissions (each with a distinct, verifiable observation count) --
// more than one 499-sized chunk -- and confirms every single one resolves
// correctly, proving the chunk-boundary arithmetic (not just that chunking
// happens at all).
func TestObservationFingerprintsBulk_ChunksAcrossBoundary(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	const n = 520
	txIDs := make([]int64, 0, n)
	wantCount := make(map[int64]int64, n)
	for i := 0; i < n; i++ {
		hash := fmt.Sprintf("chunkfp%09d", i)
		res, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
			VALUES ('AA', ?, '2026-01-15T10:00:00Z', 1, 5, '{}', '#ping')`, hash)
		if err != nil {
			t.Fatal(err)
		}
		txID, _ := res.LastInsertId()
		txIDs = append(txIDs, txID)
		// Vary observation count 1..3 so the test can't pass by coincidence.
		count := int64(1 + i%3)
		wantCount[txID] = count
		for j := int64(0); j < count; j++ {
			db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp) VALUES (?, 1, 9.0, -88, '[]', ?)`, txID, 100+j)
		}
	}

	got, err := db.observationFingerprintsBulk(txIDs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("len(got) = %d, want %d", len(got), n)
	}
	for _, txID := range txIDs {
		if got[txID].Count != wantCount[txID] {
			t.Errorf("got[%d].Count = %d, want %d", txID, got[txID].Count, wantCount[txID])
		}
	}
}

// --- nearestPositionedNeighborsBulk ------------------------------------------

// TestNearestPositionedNeighborsBulk_MatchesSingleItem seeds three targets
// with distinct-count (no-tie) neighbor sets -- one plain, one where
// maxEdgeKm's geo-sanity filter actually drops an outlier, one with no
// edges at all -- and compares each bulk result field-for-field against
// the single-item nearestPositionedNeighbor call for that same pubkey, all
// resolved from one shared nearestPositionedNeighborsBulk call (so the
// per-target VALUES-CTE join has to correctly separate their edge sets).
func TestNearestPositionedNeighborsBulk_MatchesSingleItem(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	db.conn.Exec(`CREATE TABLE IF NOT EXISTS neighbor_edges (node_a TEXT NOT NULL, node_b TEXT NOT NULL, count INTEGER DEFAULT 1, last_seen TEXT, PRIMARY KEY (node_a, node_b))`)

	// Pubkeys lowercase throughout -- nearestPositionedNeighbor lowercases
	// its input and neighbor_edges rows must agree on case for the WHERE
	// node_a = ? OR node_b = ? lookup to match (same convention as
	// TestGetPacketPath_FallsBackToSingleNeighborPosition).

	// Target A: two positioned neighbors, distinct counts.
	db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES ('nba1', 'NbA1', 'repeater', 56.0, 10.0)`)
	db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES ('nba2', 'NbA2', 'repeater', 56.01, 10.01)`)
	db.conn.Exec(`INSERT INTO neighbor_edges (node_a, node_b, count) VALUES ('targeta', 'nba1', 9)`)
	db.conn.Exec(`INSERT INTO neighbor_edges (node_a, node_b, count) VALUES ('nba2', 'targeta', 4)`)

	// Target B: strongest neighbor close by, a second neighbor far enough
	// away that a tight maxEdgeKm excludes it.
	db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES ('nbb1', 'NbB1', 'repeater', 40.0, 10.0)`)
	db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES ('nbb2', 'NbB2', 'repeater', 60.0, 30.0)`)
	db.conn.Exec(`INSERT INTO neighbor_edges (node_a, node_b, count) VALUES ('targetb', 'nbb1', 8)`)
	db.conn.Exec(`INSERT INTO neighbor_edges (node_a, node_b, count) VALUES ('targetb', 'nbb2', 3)`)

	// Target C: no edges at all.
	targets := []string{"targeta", "targetb", "targetc"}
	const maxEdgeKm = 50.0

	bulk, err := db.nearestPositionedNeighborsBulk(targets, maxEdgeKm)
	if err != nil {
		t.Fatal(err)
	}

	for _, pk := range []string{"targeta", "targetb"} {
		wantName, wantLat, wantLon, wantCount, wantSpread, wantOK := db.nearestPositionedNeighbor(pk, maxEdgeKm)
		if !wantOK {
			t.Fatalf("single-item nearestPositionedNeighbor(%s) returned ok=false unexpectedly", pk)
		}
		got, ok := bulk[pk]
		if !ok {
			t.Fatalf("bulk[%s] missing, want present", pk)
		}
		if got.Name != wantName || got.Lat != wantLat || got.Lon != wantLon || got.ContributorCount != wantCount || got.SpreadKm != wantSpread {
			t.Errorf("bulk[%s] = %+v, want {Name:%s Lat:%v Lon:%v Count:%d Spread:%v}", pk, got, wantName, wantLat, wantLon, wantCount, wantSpread)
		}
	}

	if _, ok := bulk["targetc"]; ok {
		t.Errorf("bulk[targetc] present, want absent -- no neighbor_edges rows at all")
	}
	_, _, _, _, _, singleOK := db.nearestPositionedNeighbor("targetc", maxEdgeKm)
	if singleOK {
		t.Fatalf("single-item nearestPositionedNeighbor(targetc) returned ok=true unexpectedly")
	}

	// Confirm the geo-filter actually did something in this fixture (else
	// the test wouldn't be exercising what it claims to).
	if bulk["targetb"].ContributorCount != 1 {
		t.Errorf("bulk[targetb].ContributorCount = %d, want 1 -- nbb2 should be dropped by the 50km geo-filter", bulk["targetb"].ContributorCount)
	}
}

func TestNearestPositionedNeighborsBulk_EmptyInput(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	got, err := db.nearestPositionedNeighborsBulk(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got = %+v, want empty", got)
	}
}

// TestNearestPositionedNeighborsBulk_ParameterBudgetIsExactlyNPerChunk is
// the direct proof (against the real production code, not the earlier
// scratch probe) that a chunk of N targets binds exactly N SQL parameters
// -- not 2N -- despite the underlying adjacency check needing both
// neighbor_edges.node_a and node_b compared against each target. Uses 37
// synthetic pubkeys with zero real edges (result correctness for the
// zero-edges case is already covered elsewhere); what this test checks is
// the argument count the driver actually received for the targets-CTE query.
func TestNearestPositionedNeighborsBulk_ParameterBudgetIsExactlyNPerChunk(t *testing.T) {
	db := setupPacketPathCountingDB(t)
	defer db.Close()

	const n = 37
	targets := make([]string, n)
	for i := range targets {
		targets[i] = fmt.Sprintf("budget-pk-%03d", i)
	}

	resetBulkTestQueryLog()
	if _, err := db.nearestPositionedNeighborsBulk(targets, 0); err != nil {
		t.Fatal(err)
	}

	found := false
	for _, q := range bulkTestQueryLog() {
		if !strings.Contains(q.sql, "WITH targets") {
			continue
		}
		found = true
		if q.argCount != n {
			t.Errorf("targets-CTE query bound %d args for %d targets, want exactly %d (one per target, not doubled for the OR condition)", q.argCount, n, n)
		}
	}
	if !found {
		t.Fatal("no targets-CTE query was observed -- instrumentation or query shape assumption is wrong")
	}
}

func TestNearestPositionedNeighborsBulk_ChunksAcrossBoundary(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	db.conn.Exec(`CREATE TABLE IF NOT EXISTS neighbor_edges (node_a TEXT NOT NULL, node_b TEXT NOT NULL, count INTEGER DEFAULT 1, last_seen TEXT, PRIMARY KEY (node_a, node_b))`)

	const n = 520
	targets := make([]string, n)
	for i := 0; i < n; i++ {
		target := fmt.Sprintf("chunknb%09d", i)
		nb := fmt.Sprintf("chunknbnbr%09d", i)
		targets[i] = target
		db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES (?, ?, 'repeater', ?, ?)`, nb, "N"+nb, 10.0+float64(i)*0.0001, 10.0)
		db.conn.Exec(`INSERT INTO neighbor_edges (node_a, node_b, count) VALUES (?, ?, 5)`, target, nb)
	}

	got, err := db.nearestPositionedNeighborsBulk(targets, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("len(got) = %d, want %d", len(got), n)
	}
	for i, target := range targets {
		wantLat := 10.0 + float64(i)*0.0001
		if got[target].Lat != wantLat || got[target].ContributorCount != 1 {
			t.Errorf("got[%s] = %+v, want Lat=%v Count=1", target, got[target], wantLat)
		}
	}
}

// --- legacy (non-v3) schema branch coverage ---------------------------------
//
// setupTestDB/setupTestDBV2 don't cover this: setupTestDB always forces
// isV3Flag true, and setupTestDBV2's observations table has no
// resolved_path column at all (it predates GetPacketPath entirely), so
// GetPacketPath's own legacy-schema SQL branch has never actually been
// exercised by a Go test in this codebase -- a pre-existing gap, not
// something Phase 4B changes. GetPacketPathsBulk adds a second copy of
// that branching (isV3() ? v3 query : legacy query), so this test builds
// a minimal legacy-shaped schema (observer_id/observer_name columns
// instead of an observers table join, but WITH resolved_path present)
// to prove the legacy query text GetPacketPathsBulk issues is at least
// syntactically correct and produces the same result GetPacketPath would.
func setupPacketPathLegacyTestDB(t *testing.T) *DB {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	schema := `
		CREATE TABLE nodes (
			public_key TEXT PRIMARY KEY, name TEXT, role TEXT, lat REAL, lon REAL
		);
		CREATE TABLE transmissions (
			id INTEGER PRIMARY KEY AUTOINCREMENT, raw_hex TEXT NOT NULL, hash TEXT NOT NULL UNIQUE,
			first_seen TEXT NOT NULL, route_type INTEGER, payload_type INTEGER, decoded_json TEXT,
			channel_hash TEXT DEFAULT NULL
		);
		CREATE TABLE observations (
			id INTEGER PRIMARY KEY AUTOINCREMENT, transmission_id INTEGER NOT NULL REFERENCES transmissions(id),
			observer_id TEXT, observer_name TEXT, snr REAL, rssi REAL, path_json TEXT,
			timestamp INTEGER NOT NULL, resolved_path TEXT
		);
		CREATE TABLE neighbor_edges (
			node_a TEXT NOT NULL, node_b TEXT NOT NULL, count INTEGER DEFAULT 1, last_seen TEXT,
			PRIMARY KEY (node_a, node_b)
		);
	`
	if _, err := conn.Exec(schema); err != nil {
		t.Fatal(err)
	}
	db := &DB{conn: conn}
	db.hasResolvedPathFlag.forceTrue()
	// isV3Flag deliberately left at its zero value (false).
	return db
}

func TestGetPacketPathsBulk_LegacySchemaMatchesGetPacketPath(t *testing.T) {
	db := setupPacketPathLegacyTestDB(t)
	defer db.Close()

	db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES ('pklegacy1', 'LegacyRepeater', 'repeater', 56.2, 10.3)`)
	db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		VALUES ('AA', 'legacyhash000001', '2026-01-15T10:00:00Z', 1, 5, '{}', '#ping')`)
	db.conn.Exec(`INSERT INTO observations (transmission_id, observer_id, observer_name, snr, rssi, path_json, resolved_path, timestamp)
		VALUES (1, 'legacyobs1', 'Legacy Observer One', 9.0, -88, '["aa"]', '["pklegacy1"]', 1736935200)`)
	db.conn.Exec(`INSERT INTO observations (transmission_id, observer_id, observer_name, snr, rssi, path_json, resolved_path, timestamp)
		VALUES (1, 'legacyobs2', 'Legacy Observer Two', 4.0, -95, '["aa","bb"]', '["pklegacy1","pklegacy2"]', 1736935260)`)

	single, err := db.GetPacketPath("legacyhash000001", 0)
	if err != nil {
		t.Fatalf("GetPacketPath: %v", err)
	}
	if len(single.Branches) != 2 {
		t.Fatalf("sanity check failed: single.Branches = %+v, want 2 (fixture problem, not the code under test)", single.Branches)
	}

	bulk, err := db.GetPacketPathsBulk([]string{"legacyhash000001"}, 0)
	if err != nil {
		t.Fatalf("GetPacketPathsBulk: %v", err)
	}
	got, ok := bulk["legacyhash000001"]
	if !ok {
		t.Fatal("bulk map missing legacyhash000001")
	}
	if !reflect.DeepEqual(got, single) {
		t.Errorf("GetPacketPathsBulk != GetPacketPath on the legacy schema branch:\n bulk:   %+v\n single: %+v", dumpPacketPathResponse(got), dumpPacketPathResponse(single))
	}
}

// ============================================================================
// Fix round 2 (review of commit 511438d5): chunking correctness for
// resolveNodesByPubkey/resolveNodesByName/nearestPositionedNeighborsChunk's
// candidate lookup, deterministic tie-breaks, and the GetPacketPathsBulk
// empty-input fast path.
// ============================================================================

// --- resolveNodesByPubkey / resolveNodesByName chunking --------------------

func TestResolveNodesByPubkey_ChunksAt499(t *testing.T) {
	db := setupPacketPathCountingDB(t)
	defer db.Close()

	const n = 650
	pubkeys := make([]string, n)
	for i := 0; i < n; i++ {
		pk := fmt.Sprintf("respk%09d", i)
		pubkeys[i] = pk
		if _, err := db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES (?, ?, 'repeater', ?, 10.0)`, pk, "N"+pk, 55.0+float64(i)*0.0001); err != nil {
			t.Fatalf("insert node %d: %v", i, err)
		}
	}

	resetBulkTestQueryLog()
	got, err := db.resolveNodesByPubkey(pubkeys)
	if err != nil {
		t.Fatalf("resolveNodesByPubkey: %v", err)
	}
	if len(got) != n {
		t.Fatalf("len(got) = %d, want %d", len(got), n)
	}
	for i, pk := range pubkeys {
		wantLat := 55.0 + float64(i)*0.0001
		if got[pk].lat == nil || *got[pk].lat != wantLat {
			t.Errorf("got[%s].lat = %v, want %v", pk, got[pk].lat, wantLat)
		}
	}

	log := bulkTestQueryLog()
	nodeQueries := 0
	for _, q := range log {
		if !strings.Contains(q.sql, "FROM nodes WHERE public_key IN") {
			continue
		}
		nodeQueries++
		if q.argCount > 499 {
			t.Errorf("node lookup query bound %d args, want <= 499", q.argCount)
		}
	}
	if nodeQueries < 2 {
		t.Errorf("nodeQueries = %d, want >= 2 (650 pubkeys must span more than one 499-sized chunk)", nodeQueries)
	}
}

func TestResolveNodesByPubkey_Dedupes(t *testing.T) {
	db := setupPacketPathCountingDB(t)
	defer db.Close()
	if _, err := db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES ('deduppk', 'DedupNode', 'repeater', 12.0, 13.0)`); err != nil {
		t.Fatal(err)
	}
	dup := make([]string, 600)
	for i := range dup {
		dup[i] = "deduppk"
	}
	resetBulkTestQueryLog()
	got, err := db.resolveNodesByPubkey(dup)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (600 duplicate pubkeys must dedupe before chunking)", len(got))
	}
	log := bulkTestQueryLog()
	nodeQueries := 0
	for _, q := range log {
		if strings.Contains(q.sql, "FROM nodes WHERE public_key IN") {
			nodeQueries++
		}
	}
	if nodeQueries != 1 {
		t.Errorf("nodeQueries = %d, want exactly 1 (600 duplicates of the same pubkey dedupe to one chunk)", nodeQueries)
	}
}

func TestResolveNodesByPubkey_ChunkFailureDiscardsPartialResult(t *testing.T) {
	db := setupPacketPathCountingDB(t)
	defer db.Close()

	const n = 700 // spans 2 chunks (499 + 201)
	pubkeys := make([]string, n)
	for i := 0; i < n; i++ {
		pk := fmt.Sprintf("failpk%09d", i)
		pubkeys[i] = pk
		if _, err := db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES (?, ?, 'repeater', 10.0, 10.0)`, pk, "N"+pk); err != nil {
			t.Fatalf("insert node %d: %v", i, err)
		}
	}

	resetBulkTestQueryLog()
	setBulkTestFailAfterQueries(1) // chunk 1 (query #1) succeeds, chunk 2 (query #2) fails
	defer clearBulkTestFailAfterQueries()

	got, err := db.resolveNodesByPubkey(pubkeys)
	if err == nil {
		t.Fatalf("resolveNodesByPubkey: want error from the injected chunk-2 failure, got success with %d entries", len(got))
	}
	if got != nil {
		t.Errorf("resolveNodesByPubkey returned a non-nil map (%d entries) alongside the error -- want nil, never a partial result", len(got))
	}
}

func TestResolveNodesByName_ChunksAt499AndAmbiguityIsGlobal(t *testing.T) {
	db := setupPacketPathCountingDB(t)
	defer db.Close()

	const n = 650
	names := make([]string, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("ResName%09d", i)
		names[i] = name
		if _, err := db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES (?, ?, 'repeater', ?, 10.0)`, "pk-"+name, name, 55.0+float64(i)*0.0001); err != nil {
			t.Fatalf("insert node %d: %v", i, err)
		}
	}
	// One name shared by two positioned nodes -- must still be detected as
	// ambiguous (and dropped) even though it's surrounded by hundreds of
	// unrelated names spread across multiple chunks.
	if _, err := db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES ('ambigA', 'AmbiguousResName', 'repeater', 1.0, 1.0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES ('ambigB', 'AmbiguousResName', 'repeater', 2.0, 2.0)`); err != nil {
		t.Fatal(err)
	}
	names = append(names, "AmbiguousResName")

	resetBulkTestQueryLog()
	got, err := db.resolveNodesByName(names)
	if err != nil {
		t.Fatalf("resolveNodesByName: %v", err)
	}
	if len(got) != n {
		t.Fatalf("len(got) = %d, want %d (the ambiguous name must be dropped, none of the others affected)", len(got), n)
	}
	if _, ok := got["AmbiguousResName"]; ok {
		t.Errorf("got[AmbiguousResName] present, want dropped as ambiguous")
	}
	for i, name := range names[:n] {
		wantLat := 55.0 + float64(i)*0.0001
		if got[name].lat == nil || *got[name].lat != wantLat {
			t.Errorf("got[%s].lat = %v, want %v", name, got[name].lat, wantLat)
		}
	}

	log := bulkTestQueryLog()
	nameQueries := 0
	for _, q := range log {
		if !strings.Contains(q.sql, "FROM nodes WHERE name IN") {
			continue
		}
		nameQueries++
		if q.argCount > 499 {
			t.Errorf("name lookup query bound %d args, want <= 499", q.argCount)
		}
	}
	if nameQueries < 2 {
		t.Errorf("nameQueries = %d, want >= 2 (651 names must span more than one 499-sized chunk)", nameQueries)
	}
}

func TestResolveNodesByName_ChunkFailureDiscardsPartialResult(t *testing.T) {
	db := setupPacketPathCountingDB(t)
	defer db.Close()

	const n = 700
	names := make([]string, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("FailName%09d", i)
		names[i] = name
		if _, err := db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES (?, ?, 'repeater', 10.0, 10.0)`, "pk-"+name, name); err != nil {
			t.Fatalf("insert node %d: %v", i, err)
		}
	}

	resetBulkTestQueryLog()
	setBulkTestFailAfterQueries(1)
	defer clearBulkTestFailAfterQueries()

	got, err := db.resolveNodesByName(names)
	if err == nil {
		t.Fatalf("resolveNodesByName: want error from the injected chunk-2 failure, got success with %d entries", len(got))
	}
	if got != nil {
		t.Errorf("resolveNodesByName returned a non-nil map (%d entries) alongside the error -- want nil, never a partial result", len(got))
	}
}

// --- nearestPositionedNeighborsBulk: candidate-union chunking ---------------

// TestNearestPositionedNeighborsBulk_CandidateUnionChunksNodeLookup builds
// 30 targets, each with 20 UNIQUE (non-overlapping) positioned neighbors --
// 600 distinct candidate pubkeys total, well past the 499-per-query budget,
// while the 30 targets themselves stay far under their own chunk size (so
// only the candidate-lookup chunking is being exercised here, not the
// targets chunking already covered by TestNearestPositionedNeighborsBulk_ChunksAcrossBoundary).
func TestNearestPositionedNeighborsBulk_CandidateUnionChunksNodeLookup(t *testing.T) {
	db := setupPacketPathCountingDB(t)
	defer db.Close()

	const numTargets = 30
	const candidatesPerTarget = 20
	targets := make([]string, numTargets)
	for i := 0; i < numTargets; i++ {
		target := fmt.Sprintf("cutarget%03d", i)
		targets[i] = target
		for j := 0; j < candidatesPerTarget; j++ {
			nb := fmt.Sprintf("cucand%03d_%03d", i, j)
			lat := 10.0 + float64(i)*0.01 + float64(j)*0.0001
			if _, err := db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES (?, ?, 'repeater', ?, 20.0)`, nb, "N"+nb, lat); err != nil {
				t.Fatalf("insert candidate node: %v", err)
			}
			count := candidatesPerTarget - j // distinct counts per target, avoids the tie-break edge case
			if _, err := db.conn.Exec(`INSERT INTO neighbor_edges (node_a, node_b, count) VALUES (?, ?, ?)`, target, nb, count); err != nil {
				t.Fatalf("insert edge: %v", err)
			}
		}
	}

	resetBulkTestQueryLog()
	got, err := db.nearestPositionedNeighborsBulk(targets, 0)
	if err != nil {
		t.Fatalf("nearestPositionedNeighborsBulk: %v", err)
	}
	if len(got) != numTargets {
		t.Fatalf("len(got) = %d, want %d", len(got), numTargets)
	}
	for _, target := range targets {
		if got[target].ContributorCount != candidatesPerTarget {
			t.Errorf("got[%s].ContributorCount = %d, want %d", target, got[target].ContributorCount, candidatesPerTarget)
		}
	}

	log := bulkTestQueryLog()
	candidateNodeQueries := 0
	targetQueries := 0
	for _, q := range log {
		if q.argCount > 499 {
			t.Errorf("query bound %d args, want <= 499: %s", q.argCount, q.sql)
		}
		if strings.Contains(q.sql, "WITH targets") {
			targetQueries++
		} else if strings.Contains(q.sql, "FROM nodes WHERE public_key IN") {
			candidateNodeQueries++
		}
	}
	if targetQueries != 1 {
		t.Errorf("targetQueries = %d, want 1 (30 targets fit in a single targets-chunk)", targetQueries)
	}
	if candidateNodeQueries < 2 {
		t.Errorf("candidateNodeQueries = %d, want >= 2 (600 candidate pubkeys must span more than one 499-sized chunk)", candidateNodeQueries)
	}
}

func TestNearestPositionedNeighborsBulk_TargetChunkFailureDiscardsPartialResult(t *testing.T) {
	db := setupPacketPathCountingDB(t)
	defer db.Close()

	const n = 700 // spans 2 target chunks (499 + 201)
	targets := make([]string, n)
	for i := 0; i < n; i++ {
		targets[i] = fmt.Sprintf("tcfail%09d", i)
	}

	resetBulkTestQueryLog()
	setBulkTestFailAfterQueries(1) // chunk 1's targets-CTE query succeeds, chunk 2's fails
	defer clearBulkTestFailAfterQueries()

	got, err := db.nearestPositionedNeighborsBulk(targets, 0)
	if err == nil {
		t.Fatalf("nearestPositionedNeighborsBulk: want error from the injected chunk-2 failure, got success with %d entries", len(got))
	}
	if got != nil {
		t.Errorf("nearestPositionedNeighborsBulk returned a non-nil map (%d entries) alongside the error -- want nil, never a partial result", len(got))
	}
}

// --- neighbor candidate tie-break determinism -------------------------------

// TestNearestPositionedNeighbor_TieBreakDeterministic_SingleVsBulk builds 25
// candidates for one target, all with EQUAL count so every one of them ties
// at the 20-candidate cutoff, and asserts nearestPositionedNeighbor and
// nearestPositionedNeighborsBulk select the identical 20 (same
// ContributorCount, same weighted centroid, same strongest-contributor
// name) -- proving the shared `count DESC, neighbor ASC` secondary order
// keeps both queries in lockstep even when ties span the cutoff. This
// determinizes previously-undefined behavior (see the historical "Known
// limitation" note removed from nearestPositionedNeighborsBulk's doc
// comment); it is not a claim that this order was ever guaranteed before.
func TestNearestPositionedNeighbor_TieBreakDeterministic_SingleVsBulk(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	db.conn.Exec(`CREATE TABLE IF NOT EXISTS neighbor_edges (node_a TEXT NOT NULL, node_b TEXT NOT NULL, count INTEGER DEFAULT 1, last_seen TEXT, PRIMARY KEY (node_a, node_b))`)

	const target = "tietarget"
	const numCandidates = 25 // > 20, all tied on count -- forces the cutoff to depend entirely on the tie-break
	for i := 0; i < numCandidates; i++ {
		nb := fmt.Sprintf("tienb%03d", i)
		db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES (?, ?, 'repeater', ?, 10.0)`, nb, "Name"+nb, 50.0+float64(i)*0.01)
		db.conn.Exec(`INSERT INTO neighbor_edges (node_a, node_b, count) VALUES (?, ?, 5)`, target, nb)
	}

	singleName, singleLat, singleLon, singleCount, singleSpread, singleOK := db.nearestPositionedNeighbor(target, 0)
	if !singleOK {
		t.Fatal("nearestPositionedNeighbor: want ok=true")
	}
	if singleCount != 20 {
		t.Fatalf("single ContributorCount = %d, want 20 (top-20-of-25 cutoff)", singleCount)
	}

	bulk, err := db.nearestPositionedNeighborsBulk([]string{target}, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := bulk[target]
	if !ok {
		t.Fatal("bulk map missing target")
	}
	if got.Name != singleName || got.Lat != singleLat || got.Lon != singleLon || got.ContributorCount != singleCount || got.SpreadKm != singleSpread {
		t.Errorf("bulk = %+v, want {Name:%s Lat:%v Lon:%v Count:%d Spread:%v} (matching the single-item call on a tie-heavy candidate set)", got, singleName, singleLat, singleLon, singleCount, singleSpread)
	}
	// The lowest-named candidates (tienb000..tienb019) should have won the
	// tie-break (count DESC, neighbor ASC) -- confirms the assertion above
	// isn't vacuously true because both sides independently picked an
	// arbitrary-but-matching 20; the winning set is the deterministically
	// PREDICTABLE one.
	wantName := "Nametienb000" // ASC-first candidate is also the returned "strongest" name on a full tie
	if singleName != wantName {
		t.Errorf("singleName = %q, want %q (ASC tie-break should prefer tienb000)", singleName, wantName)
	}
}

// --- observation-processing tie-break determinism ---------------------------

// TestGetPacketPath_TieBreak_SameHopsDifferentPath covers two observations
// from the SAME observer with the SAME hop count but different resolved
// paths -- packetPathReduction.fold must deterministically pick one (lowest
// observations.id) rather than whichever the query happened to scan first.
func TestGetPacketPath_TieBreak_SameHopsDifferentPath(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES ('obsTie', 'Observer Tie', NULL)`)
	db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES ('pkTieA', 'TieRepeaterA', 'repeater', 10.0, 10.0)`)
	db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES ('pkTieB', 'TieRepeaterB', 'repeater', 20.0, 20.0)`)
	db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		VALUES ('AA', 'tiehops0000001', '2026-01-15T10:00:00Z', 1, 5, '{}', '#ping')`)
	// Same observer (observer_idx=1), same hop count (1), different path --
	// inserted in a specific order; obs id 1 is pkTieA, obs id 2 is pkTieB.
	db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, resolved_path, timestamp)
		VALUES (1, 1, 9.0, -88, '["aa"]', '["pkTieA"]', 100)`)
	db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, resolved_path, timestamp)
		VALUES (1, 1, 9.0, -88, '["bb"]', '["pkTieB"]', 100)`)

	for i := 0; i < 5; i++ {
		resp, err := db.GetPacketPath("tiehops0000001", 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Branches) != 1 {
			t.Fatalf("call %d: Branches = %+v, want 1 (one observer)", i, resp.Branches)
		}
		if len(resp.Branches[0].Points) != 1 {
			t.Fatalf("call %d: Points = %+v, want 1 entry", i, resp.Branches[0].Points)
		}
		if resp.Branches[0].Points[0].PublicKey != "pkTieA" {
			t.Errorf("call %d: Points[0].PublicKey = %q, want pkTieA (lowest obsID wins the hops tie) on every call", i, resp.Branches[0].Points[0].PublicKey)
		}
	}
}

// TestGetPacketPath_TieBreak_SameEarliestTimestamp covers two observations
// (from different observers) tied for the earliest timestamp -- First must
// deterministically resolve to one of them (lowest observations.id) across
// repeated calls, not whichever the query happened to scan first.
func TestGetPacketPath_TieBreak_SameEarliestTimestamp(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES ('obsTieX', 'Observer Tie X', 'SJC')`)
	db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES ('obsTieY', 'Observer Tie Y', 'SFO')`)
	db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		VALUES ('AA', 'tiefirst0000001', '2026-01-15T10:00:00Z', 1, 5, '{}', '#ping')`)
	// obsTieX inserted first (obs id 1), obsTieY second (obs id 2); both
	// timestamp=100.
	db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp)
		VALUES (1, 1, 9.0, -88, '[]', 100)`)
	db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp)
		VALUES (1, 2, 4.0, -95, '[]', 100)`)

	for i := 0; i < 5; i++ {
		resp, err := db.GetPacketPath("tiefirst0000001", 0)
		if err != nil {
			t.Fatal(err)
		}
		if resp.First == nil || resp.First.Observer == nil {
			t.Fatalf("call %d: First = %+v, want a resolved observer", i, resp.First)
		}
		if resp.First.Observer.Name != "Observer Tie X" {
			t.Errorf("call %d: First.Observer.Name = %q, want %q (lowest obsID wins the timestamp tie) on every call", i, resp.First.Observer.Name, "Observer Tie X")
		}
	}
}

// --- branch-sort tie-break determinism --------------------------------------

// TestGetPacketPath_BranchSortTieBreak_MultipleBranchesSameHops covers three
// observers each contributing a distinct-path 2-hop branch -- Branches must
// come out in a deterministic order on every call, and DeepestPubkey/
// DeepestName (Branches[0], the field ping_scores.go's computePingScore
// reads) must be reproducible rather than picked by arbitrary map
// iteration order.
//
// Uses the legacy (non-v3) schema deliberately: buildPacketPathResponseFromReduction
// sorts by the internal grouping key from parsePacketPathObsRow, which for
// the legacy schema is o.observer_id (a real, human-meaningful identifier)
// -- for v3 it's obs.rowid (SQLite's own numeric row id, rendered as a
// string), which sorts correctly but not in a way a hardcoded "expected
// order" in a test could usefully assert without the assertion itself
// becoming a confusing implementation-detail tautology. Both schemas share
// the identical fold/sort code path, so this still exercises the real fix.
func TestGetPacketPath_BranchSortTieBreak_MultipleBranchesSameHops(t *testing.T) {
	db := setupPacketPathLegacyTestDB(t)
	defer db.Close()
	// Observer IDs deliberately NOT inserted in alphabetical order, so a
	// correct fix can't accidentally pass by coincidentally matching
	// insertion order.
	db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		VALUES ('AA', 'tiebranch000001', '2026-01-15T10:00:00Z', 1, 5, '{}', '#ping')`)
	db.conn.Exec(`INSERT INTO observations (transmission_id, observer_id, observer_name, snr, rssi, path_json, timestamp)
		VALUES (1, 'obsZ', 'Observer Z', 9.0, -88, '["aa","bb"]', 100)`)
	db.conn.Exec(`INSERT INTO observations (transmission_id, observer_id, observer_name, snr, rssi, path_json, timestamp)
		VALUES (1, 'obsA', 'Observer A', 9.0, -88, '["aa","bb"]', 200)`)
	db.conn.Exec(`INSERT INTO observations (transmission_id, observer_id, observer_name, snr, rssi, path_json, timestamp)
		VALUES (1, 'obsM', 'Observer M', 9.0, -88, '["aa","bb"]', 300)`)

	var firstOrder []string
	for i := 0; i < 5; i++ {
		resp, err := db.GetPacketPath("tiebranch000001", 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Branches) != 3 {
			t.Fatalf("call %d: Branches = %+v, want 3", i, resp.Branches)
		}
		order := make([]string, 3)
		for j, b := range resp.Branches {
			if b.Hops != 2 {
				t.Fatalf("call %d: Branches[%d].Hops = %d, want 2 (sanity check -- fixture must tie on Hops)", i, j, b.Hops)
			}
			order[j] = b.Observer.PublicKey
		}
		if i == 0 {
			firstOrder = order
			// Sort key is the raw (cased) observer_id ("obsA" < "obsM" < "obsZ"),
			// but PublicKey itself is lowercased by parsePacketPathObsRow --
			// the expected deterministic order, in displayed form.
			want := []string{"obsa", "obsm", "obsz"}
			for j := range want {
				if order[j] != want[j] {
					t.Errorf("Branches order = %v, want %v (observer-key ascending)", order, want)
					break
				}
			}
		} else if !reflect.DeepEqual(order, firstOrder) {
			t.Errorf("call %d: Branches order = %v, want %v (identical to call 0)", i, order, firstOrder)
		}
	}
}

// TestGetPacketPathsBulk_MatchesGetPacketPath_WithHopsAndTimestampTies is a
// golden-equivalence test specifically for the tie-break paths: three
// branches tied on Hops (observer-key order) plus a First/earliest-timestamp
// tie. GetPacketPathsBulk must resolve to the exact same PacketPathResponse
// GetPacketPath does, byte-for-byte, proving the deterministic tie-breaks
// (obsID-based fold, observer-key-ascending build order, stable Hops sort)
// are genuinely shared between the two paths and not just each
// independently "consistent with itself".
func TestGetPacketPathsBulk_MatchesGetPacketPath_WithHopsAndTimestampTies(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES ('obsTieC', 'Observer Tie C', NULL)`)
	db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES ('obsTieD', 'Observer Tie D', NULL)`)
	db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES ('obsTieE', 'Observer Tie E', NULL)`)
	db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		VALUES ('AA', 'tiebulk00000001', '2026-01-15T10:00:00Z', 1, 5, '{}', '#ping')`)
	// obsTieC and obsTieD tie for earliest timestamp (100); obsTieE arrives
	// later. All three tie on Hops (2).
	db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp)
		VALUES (1, 1, 9.0, -88, '["aa","bb"]', 100)`)
	db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp)
		VALUES (1, 2, 9.0, -88, '["aa","bb"]', 100)`)
	db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp)
		VALUES (1, 3, 9.0, -88, '["aa","bb"]', 200)`)

	single, err := db.GetPacketPath("tiebulk00000001", 0)
	if err != nil {
		t.Fatal(err)
	}
	bulk, err := db.GetPacketPathsBulk([]string{"tiebulk00000001"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := bulk["tiebulk00000001"]
	if !ok {
		t.Fatal("bulk map missing tiebulk00000001")
	}
	if !reflect.DeepEqual(got, single) {
		t.Errorf("GetPacketPathsBulk != GetPacketPath under ties:\n bulk:   %+v\n single: %+v", dumpPacketPathResponse(got), dumpPacketPathResponse(single))
	}
}

// --- GetPacketPathsBulk empty-input fast path -------------------------------

// TestGetPacketPathsBulk_EmptyInputSkipsSchemaCheck proves the fixed
// ordering: an empty hashes slice returns an empty map with no error even
// against a DB that lacks resolved_path entirely, and issues no query at
// all -- both would have been impossible before the fix, since
// hasResolvedPath() was checked (and would have errored) before the
// empty-input short-circuit.
func TestGetPacketPathsBulk_EmptyInputSkipsSchemaCheck(t *testing.T) {
	db := setupPacketPathCountingDB(t)
	defer db.Close()
	db.hasResolvedPathFlag.v.Store(false) // simulates a schema without resolved_path

	resetBulkTestQueryLog()
	result, err := db.GetPacketPathsBulk(nil, 0)
	if err != nil {
		t.Fatalf("GetPacketPathsBulk(nil) on a no-resolved_path schema: want no error, got %v", err)
	}
	if len(result) != 0 {
		t.Errorf("result = %+v, want empty map", result)
	}
	if len(bulkTestQueryLog()) != 0 {
		t.Errorf("query log = %+v, want no queries issued for an empty request", bulkTestQueryLog())
	}

	// Sanity check: a NON-empty request against the same no-resolved_path
	// DB must still error -- the fast path is empty-input-specific, not a
	// blanket skip of the schema check.
	_, err = db.GetPacketPathsBulk([]string{"whatever"}, 0)
	if err == nil {
		t.Fatal("GetPacketPathsBulk([]string{\"whatever\"}) on a no-resolved_path schema: want an error")
	}
}
