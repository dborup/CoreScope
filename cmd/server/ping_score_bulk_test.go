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

func (c *countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	bulkTestQueriesMu.Lock()
	bulkTestQueries = append(bulkTestQueries, recordedQuery{sql: query, argCount: len(args)})
	bulkTestQueriesMu.Unlock()
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
