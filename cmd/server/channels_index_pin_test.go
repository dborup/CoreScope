package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Issue #100: GetChannels is pinned to idx_tx_channel_hash and
// CountFloodAdvertsForNode to idx_transmissions_from_pubkey, so both get the
// plan SQLite picks with ANALYZE statistics while neither binary runs ANALYZE.

// pinTestDB builds the production transmissions/observations/observers
// indexes (cmd/ingestor/db.go + internal/dbschema) - unlike setupTestDB,
// which has a non-partial channel_hash index and no payload_type index.
// payloadTypeLast creates idx_transmissions_payload_type after the
// channel_hash and from_pubkey indexes instead of before them (the
// ingestor's order); without statistics SQLite's choice between equally
// rated indexes follows that order. v3 selects observations.observer_idx
// (else the v2 observer_id column).
func pinTestDB(t testing.TB, v3, payloadTypeLast bool) *DB {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { conn.Close() })
	observerCol, observerIdx := "observer_idx INTEGER", "idx_observations_observer_idx ON observations(observer_idx)"
	if !v3 {
		observerCol, observerIdx = "observer_id TEXT", "idx_observations_observer_id ON observations(observer_id)"
	}
	stmts := []string{
		`CREATE TABLE observers (id TEXT PRIMARY KEY, name TEXT, iata TEXT, last_seen TEXT)`,
		`CREATE TABLE transmissions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			raw_hex TEXT NOT NULL,
			hash TEXT NOT NULL UNIQUE,
			first_seen TEXT NOT NULL,
			route_type INTEGER,
			payload_type INTEGER,
			payload_version INTEGER,
			decoded_json TEXT,
			from_pubkey TEXT,
			last_seen INTEGER NOT NULL DEFAULT 0,
			created_at TEXT,
			channel_hash TEXT DEFAULT NULL,
			scope_name TEXT)`,
		`CREATE TABLE observations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			transmission_id INTEGER NOT NULL REFERENCES transmissions(id),
			` + observerCol + `,
			path_json TEXT,
			timestamp INTEGER NOT NULL)`,
		`CREATE INDEX idx_transmissions_hash ON transmissions(hash)`,
		`CREATE INDEX idx_transmissions_first_seen ON transmissions(first_seen)`,
	}
	payloadTypeIdx := `CREATE INDEX idx_transmissions_payload_type ON transmissions(payload_type)`
	if !payloadTypeLast {
		stmts = append(stmts, payloadTypeIdx)
	}
	stmts = append(stmts,
		`CREATE INDEX idx_tx_channel_hash ON transmissions(channel_hash) WHERE payload_type = 5`,
		`CREATE INDEX idx_transmissions_from_pubkey ON transmissions(from_pubkey)`,
		`CREATE INDEX idx_tx_scope_name ON transmissions(scope_name) WHERE scope_name IS NOT NULL`,
		`CREATE INDEX idx_tx_last_seen_zero ON transmissions(id) WHERE last_seen=0`,
		`CREATE INDEX idx_observations_transmission_id ON observations(transmission_id)`,
		`CREATE INDEX `+observerIdx,
		`CREATE INDEX idx_observations_timestamp ON observations(timestamp)`,
		`CREATE INDEX idx_observations_tx_ts ON observations(transmission_id, timestamp)`,
	)
	if payloadTypeLast {
		stmts = append(stmts, payloadTypeIdx)
	}
	for _, s := range stmts {
		if _, err := conn.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	db := &DB{conn: conn}
	if v3 {
		db.isV3Flag.forceTrue()
	}
	return db
}

var pinTestIATAs = []string{"AAR", " cph ", "CPH", "OSL", "", "sto"}

// pinTestSeed fills a pinTestDB with the shape that matters for the plan:
// GRP_TXT is the largest payload type and most of it encrypted, spread over
// few channel hashes; each node sends a few adverts. It also mixes in what
// the result-equivalence test needs: NULL channel_hash and decoded_json,
// undecodable JSON, enc_/ENC_/encX hashes, channel_hash on other payload
// types, first_seen ties within a channel and across channels, several
// first_seen formats, transmissions without observations and observations of
// unknown or NULL observers.
func pinTestSeed(t testing.TB, db *DB, v3 bool, nTx int, seed int64) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	tx, err := db.conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i, iata := range pinTestIATAs {
		var v interface{} = iata
		if i == 4 {
			v = nil
		}
		if _, err := tx.Exec(`INSERT INTO observers (id, name, iata) VALUES (?, ?, ?)`, fmt.Sprintf("obs%d", i), "o", v); err != nil {
			t.Fatal(err)
		}
	}
	insTx, err := tx.Prepare(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey, channel_hash) VALUES ('AA', ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	insObs, err := tx.Prepare(`INSERT INTO observations (transmission_id, ` + map[bool]string{true: "observer_idx", false: "observer_id"}[v3] + `, path_json, timestamp) VALUES (?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	decrypted := []string{"#public", "#test", "#dk", "#ping", "#aarhus", "#mesh", "#x"}
	jsons := []interface{}{
		`{"type":"CHAN","text":"Alice: hi there","sender":"Alice"}`,
		`{"type":"CHAN","text":"no separator","sender":"Bob"}`,
		`{"type":"CHAN","text":"","sender":"Eve"}`,
		`{"type":"CHAN","sender":"NoText"}`,
		`{"type":"CHAN","text":"Carl: yo"}`,
		`not json`,
		nil,
	}
	for i := 0; i < nTx; i++ {
		// Mostly second-resolution RFC3339 over 30 days, so ties happen;
		// a few rows in the ingestor's other formats.
		ts := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(rng.Intn(30*24*3600/7)) * 7 * time.Second)
		firstSeen := ts.Format(time.RFC3339)
		switch rng.Intn(20) {
		case 0:
			firstSeen = ts.Format("2006-01-02 15:04:05")
		case 1:
			firstSeen = ts.Format("2006-01-02T15:04:05.000Z")
		}
		var pt, rt int
		var chHash, fromPK, dj interface{}
		switch r := rng.Intn(100); {
		case r < 55: // GRP_TXT
			pt, rt = 5, 1
			switch c := rng.Intn(100); {
			case c < 70:
				chHash = fmt.Sprintf("enc_%02X", rng.Intn(40))
			case c < 72:
				chHash = []string{"ENC_1A", "encX1", "enc_"}[rng.Intn(3)]
			case c < 75:
				chHash = nil
			default:
				chHash = decrypted[rng.Intn(len(decrypted))]
			}
			dj = jsons[rng.Intn(len(jsons))]
		case r < 80: // ADVERT
			pt, rt = payloadTypeAdvert, rng.Intn(4)
			fromPK = fmt.Sprintf("%064x", rng.Intn(nTx/20+1))
			dj = `{"type":"ADVERT"}`
		default:
			pt, rt = []int{1, 2, 3, 8, 9}[rng.Intn(5)], rng.Intn(4)
			if rng.Intn(10) == 0 {
				chHash = decrypted[rng.Intn(len(decrypted))] // never a channel: payload_type != 5
			}
		}
		res, err := insTx.Exec(fmt.Sprintf("h%d-%d", seed, i), firstSeen, rt, pt, dj, fromPK, chHash)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		for k, n := 0, rng.Intn(6); k < n; k++ { // 0 observations for ~1/6 of the rows
			var obs interface{}
			switch o := rng.Intn(20); {
			case o == 0:
				obs = nil
			case o == 1:
				if v3 {
					obs = 99 // no such observer rowid
				} else {
					obs = "obs-missing"
				}
			default:
				j := rng.Intn(len(pinTestIATAs))
				if v3 {
					obs = j + 1
				} else {
					obs = fmt.Sprintf("obs%d", j)
				}
			}
			if _, err := insObs.Exec(id, obs, fmt.Sprint(k), ts.Unix()+int64(k)); err != nil {
				t.Fatal(err)
			}
		}
	}
	insTx.Close()
	insObs.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func explainPlan(t testing.TB, conn *sql.DB, q string, args ...interface{}) []string {
	t.Helper()
	rows, err := conn.Query("EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v\n%s", err, q)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	return plan
}

// channelsRegionCases are the region shapes GetChannels builds SQL for.
var channelsRegionCases = []struct {
	name        string
	placeholder string
	args        []interface{}
}{
	{"all", "", nil},
	{"one region", "?", []interface{}{"AAR"}},
	{"two regions", "?,?", []interface{}{"AAR", "CPH"}},
}

// assertChannelsPlan checks the plan SQLite chooses with statistics: the
// outer scan and the sample subquery both on idx_tx_channel_hash, the GROUP
// BY served by the index order, no payload_type index anywhere.
func assertChannelsPlan(t *testing.T, label string, plan []string) {
	t.Helper()
	joined := strings.Join(plan, " | ")
	outer, sub := false, false
	for _, line := range plan {
		if strings.HasPrefix(line, "SEARCH t USING INDEX "+channelHashIndex+" ") {
			outer = true
		}
		if line == "SEARCH t2 USING INDEX "+channelHashIndex+" (channel_hash=?)" {
			sub = true
		}
	}
	if !outer || !sub || strings.Contains(joined, "idx_transmissions_payload_type") || strings.Contains(joined, "TEMP B-TREE FOR GROUP BY") {
		t.Errorf("%s: plan = %s\nwant outer and subquery on %s, no payload_type index, no GROUP BY temp B-tree", label, joined, channelHashIndex)
	}
}

// Without sqlite_stat1 - production - the pinned query gets the statistics
// plan in both index creation orders. Unpinned, SQLite takes the payload_type
// index for the outer scan (both orders) and for the subquery too when that
// index is the newer one.
func TestChannelsSQL_PlanWithoutStats(t *testing.T) {
	for _, v3 := range []bool{true, false} {
		for _, last := range []bool{false, true} {
			db := pinTestDB(t, v3, last)
			for _, rc := range channelsRegionCases {
				label := fmt.Sprintf("v3=%v payloadTypeLast=%v %s", v3, last, rc.name)
				assertChannelsPlan(t, label, explainPlan(t, db.conn, channelsSQL(v3, rc.placeholder, true), rc.args...))
			}
		}
	}
}

// With sqlite_stat1 on realistically skewed data the pin changes nothing:
// the pinned plan is the one SQLite picks for the unpinned query, and that
// is the idx_tx_channel_hash plan (as measured on the staging copy, PR #106).
func TestChannelsSQL_PlanWithStats(t *testing.T) {
	for _, v3 := range []bool{true, false} {
		for _, last := range []bool{false, true} {
			db := pinTestDB(t, v3, last)
			pinTestSeed(t, db, v3, 6000, 100)
			if _, err := db.conn.Exec("ANALYZE"); err != nil {
				t.Fatal(err)
			}
			for _, rc := range channelsRegionCases {
				label := fmt.Sprintf("v3=%v payloadTypeLast=%v %s", v3, last, rc.name)
				pinned := explainPlan(t, db.conn, channelsSQL(v3, rc.placeholder, true), rc.args...)
				unpinned := explainPlan(t, db.conn, channelsSQL(v3, rc.placeholder, false), rc.args...)
				if !reflect.DeepEqual(pinned, unpinned) {
					t.Errorf("%s: with statistics the pinned plan differs from SQLite's own choice\npinned:   %s\nunpinned: %s", label, strings.Join(pinned, " | "), strings.Join(unpinned, " | "))
				}
				assertChannelsPlan(t, label, pinned)
			}
		}
	}
}

// CountFloodAdvertsForNode walks idx_transmissions_from_pubkey in id order
// (no sort), with and without statistics and in both index creation orders.
func TestFloodAdvertCountSQL_Plan(t *testing.T) {
	for _, last := range []bool{false, true} {
		for _, stats := range []bool{false, true} {
			db := pinTestDB(t, true, last)
			if stats {
				pinTestSeed(t, db, true, 6000, 200)
				if _, err := db.conn.Exec("ANALYZE"); err != nil {
					t.Fatal(err)
				}
			}
			plan := strings.Join(explainPlan(t, db.conn, floodAdvertCountSQL, fmt.Sprintf("%064x", 1), payloadTypeAdvert, advertRouteTypeFlood, "2026-09-01", floodAdvertRowCap), " | ")
			if plan != "SEARCH transmissions USING INDEX idx_transmissions_from_pubkey (from_pubkey=?)" {
				t.Errorf("payloadTypeLast=%v stats=%v: plan = %s, want an index-ordered from_pubkey search", last, stats, plan)
			}
		}
	}
}

// The pin must not change what CountFloodAdvertsForNode counts: compare it
// with the unpinned predicate on every node of the random data.
func TestFloodAdvertCountSQL_MatchesUnpinned(t *testing.T) {
	db := pinTestDB(t, true, true)
	pinTestSeed(t, db, true, 6000, 300)
	unpinned := strings.Replace(floodAdvertCountSQL, "+payload_type", "payload_type", 1)
	if unpinned == floodAdvertCountSQL {
		t.Fatal("floodAdvertCountSQL has no +payload_type")
	}
	rows, err := db.conn.Query(`SELECT DISTINCT from_pubkey FROM transmissions WHERE from_pubkey IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for rows.Next() {
		var k string
		rows.Scan(&k)
		keys = append(keys, k)
	}
	rows.Close()
	if len(keys) < 100 {
		t.Fatalf("only %d nodes", len(keys))
	}
	total := 0
	for _, k := range keys {
		args := []interface{}{k, payloadTypeAdvert, advertRouteTypeFlood, "2026-09-10", 5}
		got, want := queryAllRows(t, db.conn, floodAdvertCountSQL, args...), queryAllRows(t, db.conn, unpinned, args...)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("node %s: pinned rows %v, unpinned %v", k, got, want)
		}
		total += len(got)
	}
	if total == 0 {
		t.Fatal("no flood advert rows compared")
	}
}

func queryAllRows(t testing.TB, conn *sql.DB, q string, args ...interface{}) [][]interface{} {
	t.Helper()
	rows, err := conn.Query(q, args...)
	if err != nil {
		t.Fatalf("%v\n%s", err, q)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out [][]interface{}
	for rows.Next() {
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		out = append(out, vals)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// GetChannels returns the same channels, in the same order, with the same
// fields whether it runs pinned (index present) or falls back to the
// unpinned query (index dropped) - and the raw rows of the pinned SQL equal
// those of the unpinned SQL, for every schema, index order and region shape.
func TestGetChannels_PinnedMatchesUnpinned(t *testing.T) {
	regions := []string{"", "AAR", "cph", "AAR,CPH", " aar , osl ", "sto", "ZZZ"}
	for _, v3 := range []bool{true, false} {
		for _, last := range []bool{false, true} {
			db := pinTestDB(t, v3, last)
			pinTestSeed(t, db, v3, 4000, 400)
			label := fmt.Sprintf("v3=%v payloadTypeLast=%v", v3, last)

			for _, rc := range channelsRegionCases {
				pinned := queryAllRows(t, db.conn, channelsSQL(v3, rc.placeholder, true), rc.args...)
				unpinned := queryAllRows(t, db.conn, channelsSQL(v3, rc.placeholder, false), rc.args...)
				if len(pinned) < 5 {
					t.Fatalf("%s %s: only %d channel rows", label, rc.name, len(pinned))
				}
				if !reflect.DeepEqual(pinned, unpinned) {
					t.Errorf("%s %s: raw rows differ\npinned:   %v\nunpinned: %v", label, rc.name, pinned, unpinned)
				}
			}

			viaPin := map[string][]map[string]interface{}{}
			for _, r := range regions {
				resetDBChannelsCache(db)
				res, err := db.GetChannels(r)
				if err != nil {
					t.Fatalf("%s region %q: %v", label, r, err)
				}
				viaPin[r] = res
			}
			if n := db.channelsPinFallbacks.Load(); n != 0 {
				t.Fatalf("%s: %d fallbacks with %s present, want the pinned query", label, n, channelHashIndex)
			}

			if _, err := db.conn.Exec("DROP INDEX " + channelHashIndex); err != nil {
				t.Fatal(err)
			}
			for _, r := range regions {
				resetDBChannelsCache(db)
				res, err := db.GetChannels(r)
				if err != nil {
					t.Fatalf("%s region %q without index: %v", label, r, err)
				}
				if !reflect.DeepEqual(res, viaPin[r]) {
					t.Errorf("%s region %q: pinned result\n%v\nunpinned result\n%v", label, r, viaPin[r], res)
				}
			}
			if n := db.channelsPinFallbacks.Load(); n != int64(len(regions)) {
				t.Fatalf("%s: %d fallbacks without %s, want %d", label, n, channelHashIndex, len(regions))
			}
			if len(viaPin[""]) == 0 || len(viaPin["AAR"]) == 0 || len(viaPin["ZZZ"]) != 0 {
				t.Fatalf("%s: implausible channel counts all=%d AAR=%d ZZZ=%d", label, len(viaPin[""]), len(viaPin["AAR"]), len(viaPin["ZZZ"]))
			}
		}
	}
}

// Without idx_tx_channel_hash (setupTestDB has only a non-partial
// channel_hash index) INDEXED BY cannot prepare: /api/channels must still
// answer 200 from the unpinned query, with and without a region.
func TestGetChannels_MissingIndexFallsBack(t *testing.T) {
	srv, router := setupTestServer(t)
	for _, url := range []string{"/api/channels", "/api/channels?region=SJC", "/api/channels?includeEncrypted=true"} {
		req := httptest.NewRequest(http.MethodGet, url, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d, body %s", url, w.Code, w.Body.String())
		}
		var body struct {
			Channels []map[string]interface{} `json:"channels"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Channels) == 0 || body.Channels[0]["hash"] != "#test" {
			t.Fatalf("%s: channels = %v, want #test", url, body.Channels)
		}
	}
	if srv.db.channelsPinFallbacks.Load() == 0 {
		t.Fatal("no fallback recorded: GetChannels did not try the pinned query first")
	}
}
