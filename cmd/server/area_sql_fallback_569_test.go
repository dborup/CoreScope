package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// #569: the GPS `area` filter (originator membership) must survive the SQL
// path — the store's history fallback (since/until older than oldestLoaded)
// and the direct-DB handler path — with exactly the in-memory semantics:
//   - originator = decoded ADVERT pubkey; packets without one are excluded;
//   - an unknown area key, or an area no positioned node falls inside,
//     applies no filter at all (resolveAreaNodes returns nil);
//   - nodes without GPS (NULL or 0,0) are never members;
//   - area and region are cumulative.

var area569Config = &Config{Areas: map[string]AreaEntry{
	"BEL": {Label: "Belgium", Polygon: [][2]float64{{50.0, 2.5}, {51.5, 2.5}, {51.5, 6.4}, {50.0, 6.4}}},
	"BOX": {Label: "Box", LatMin: f64p569(50.9), LatMax: f64p569(51.5), LonMin: f64p569(4.9), LonMax: f64p569(6.4)},
	"SEA": {Label: "Empty", LatMin: f64p569(10), LatMax: f64p569(11), LonMin: f64p569(10), LonMax: f64p569(11)},
}}

func f64p569(v float64) *float64 { return &v }

func pk569(c string) string { return strings.Repeat(c, 64) }

// seedArea569 writes nodes, observers and packets. Every packet has a unique
// first_seen and observation timestamp so ordinary (id) and grouped (latest)
// orderings are total and identical across the two paths.
func seedArea569(t *testing.T, db *DB) {
	t.Helper()
	nodes := []struct {
		pk       string
		lat, lon interface{}
	}{
		{pk569("a"), 50.85, 4.35}, // BEL only
		{pk569("b"), 51.0, 5.0},   // BEL + BOX
		{pk569("c"), 48.0, 4.35},  // outside
		{pk569("d"), nil, nil},    // no GPS
		{pk569("e"), 0.0, 0.0},    // (0,0) = no GPS
	}
	for _, n := range nodes {
		if _, err := db.conn.Exec(`INSERT INTO nodes (public_key, name, lat, lon) VALUES (?, ?, ?, ?)`, n.pk, "n-"+n.pk[:1], n.lat, n.lon); err != nil {
			t.Fatal(err)
		}
	}
	mustExecDB(t, db, `INSERT INTO observers (id, name, iata) VALUES ('obsA', 'A', 'AAA'), ('obsB', 'B', 'BBB')`)

	// originator ("" = no pubkey), payload type, observer rowid.
	packets := []struct {
		pk  string
		typ int
		obs int
	}{
		{pk569("a"), 4, 1}, {pk569("b"), 4, 2}, {pk569("c"), 4, 1}, {pk569("a"), 4, 2},
		{pk569("d"), 4, 1}, {pk569("e"), 4, 2}, {"", 4, 1}, {"", 5, 1},
		{pk569("b"), 4, 1}, {pk569("a"), 4, 1}, {pk569("c"), 4, 2}, {pk569("b"), 4, 2},
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, p := range packets {
		ts := base.Add(time.Duration(i) * time.Minute)
		decoded := `{"type":"GRP_TXT","channelHashHex":"AA"}`
		if p.typ == 4 {
			decoded = `{"type":"ADVERT"}`
			if p.pk != "" {
				decoded = fmt.Sprintf(`{"type":"ADVERT","pubKey":%q}`, p.pk)
			}
		}
		res, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json) VALUES ('AA', ?, ?, 1, ?, ?)`,
			fmt.Sprintf("%016x", i+1), ts.Format(time.RFC3339), p.typ, decoded)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		if _, err := db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp) VALUES (?, ?, '[]', ?)`, id, p.obs, ts.Unix()); err != nil {
			t.Fatal(err)
		}
	}
}

type area569Page struct {
	Total  int
	Hashes []string
}

func newArea569Server(t *testing.T, mode string) *Server {
	t.Helper()
	db := setupTestDB(t)
	t.Cleanup(func() { db.Close() })
	seedArea569(t, db)
	srv := NewServer(db, area569Config, NewHub())
	if mode == "database" {
		return srv
	}
	srv.store = NewPacketStore(db, nil)
	srv.store.config = area569Config
	if err := srv.store.Load(); err != nil {
		t.Fatal(err)
	}
	if !srv.store.WaitIndexesReady(5 * time.Second) {
		t.Fatal("indexes not ready")
	}
	if mode == "memory" {
		srv.store.oldestLoaded = "" // never fall back
	} else {
		srv.store.oldestLoaded = "2026-02-01T00:00:00Z" // newer than every window bound below
	}
	return srv
}

func getArea569(t *testing.T, srv *Server, query string) area569Page {
	t.Helper()
	w := httptest.NewRecorder()
	srv.handlePackets(w, httptest.NewRequest("GET", "/api/packets?"+query, nil))
	if w.Code != 200 {
		t.Fatalf("%s: %d %s", query, w.Code, w.Body.String())
	}
	var got PacketResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	page := area569Page{Total: got.Total, Hashes: []string{}}
	for _, p := range got.Packets {
		page.Hashes = append(page.Hashes, fmt.Sprint(p["hash"]))
	}
	return page
}

// TestAreaFilterSQLMatchesMemory569 is the equivalence matrix: every SQL
// route (store fallback on since, on until, and the store-less direct DB
// path) must return the same rows, order and total as the in-memory path.
func TestAreaFilterSQLMatchesMemory569(t *testing.T) {
	memory := newArea569Server(t, "memory")
	sqlModes := map[string]*Server{"fallback": newArea569Server(t, "fallback"), "database": newArea569Server(t, "database")}
	windows := []string{"since=2025-12-01T00:00:00Z", "until=2026-01-15T00:00:00Z", "since=2026-01-01T00:02:30Z&until=2026-01-15T00:00:00Z"}

	for _, window := range windows {
		for _, grouped := range []bool{false, true} {
			for _, area := range []string{"", "BEL", "BOX", "SEA", "NOPE"} {
				for _, extra := range []string{"", "&region=AAA", "&region=BBB", "&type=4", "&observer=obsB"} {
					for _, page := range []string{"limit=50", "limit=2", "limit=2&offset=2", "limit=2&offset=40"} {
						query := fmt.Sprintf("%s&%s&groupByHash=%t%s", page, window, grouped, extra)
						if area != "" {
							query += "&area=" + area
						}
						want := getArea569(t, memory, query)
						for name, srv := range sqlModes {
							if got := getArea569(t, srv, query); !reflect.DeepEqual(got, want) {
								t.Errorf("%s %s:\n got  %+v\n want %+v", name, query, got, want)
							}
						}
					}
				}
			}
		}
	}
}

// TestAreaFilterSQLFallbackContract569 pins the absolute contract on the
// fallback, so the matrix above cannot pass by both paths drifting together.
func TestAreaFilterSQLFallbackContract569(t *testing.T) {
	for _, mode := range []string{"memory", "fallback", "database"} {
		srv := newArea569Server(t, mode)
		for _, grouped := range []bool{false, true} {
			for _, window := range []string{"since=2025-12-01T00:00:00Z", "until=2026-01-15T00:00:00Z"} {
				base := fmt.Sprintf("limit=50&%s&groupByHash=%t", window, grouped)
				cases := []struct {
					query  string
					hashes []string // set, sorted
				}{
					// a×3 + b×3 inside the polygon; c (outside), d/e (no GPS),
					// the pubkey-less ADVERT and the GRP_TXT are excluded.
					{"&area=BEL", []string{"0000000000000001", "0000000000000002", "0000000000000004", "0000000000000009", "000000000000000a", "000000000000000c"}},
					{"&area=BOX", []string{"0000000000000002", "0000000000000009", "000000000000000c"}},
					{"&area=BEL&region=AAA", []string{"0000000000000001", "0000000000000009", "000000000000000a"}},
					// Unknown key / empty area: no filter (in-memory nil set).
					{"&area=NOPE", all569()},
					{"&area=SEA", all569()},
					{"&area=BOX&type=5", nil},
				}
				for _, c := range cases {
					got := getArea569(t, srv, base+c.query)
					sorted := append([]string{}, got.Hashes...)
					sort.Strings(sorted)
					if got.Total != len(c.hashes) || !reflect.DeepEqual(sorted, nonNil569(c.hashes)) {
						t.Errorf("%s %s: total=%d hashes=%v want %v", mode, base+c.query, got.Total, sorted, c.hashes)
					}
				}
			}
		}
	}
}

func all569() []string {
	out := make([]string, 12)
	for i := range out {
		out[i] = fmt.Sprintf("%016x", i+1)
	}
	return out
}

func nonNil569(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// TestAreaFilterAcrossCacheBoundary569: history older than the loaded window
// exists only in SQLite. A since/until window reaching past oldestLoaded
// must return the old in-area packets and none of the old outside ones,
// with a total that counts both sides of the boundary.
func TestAreaFilterAcrossCacheBoundary569(t *testing.T) {
	srv := newArea569Server(t, "memory")
	// Old rows, SQL-only: inside (a), outside (c), inside (b).
	for i, pk := range []string{pk569("a"), pk569("c"), pk569("b")} {
		ts := time.Date(2025, 12, 1, 0, i, 0, 0, time.UTC)
		res, err := srv.db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json) VALUES ('AA', ?, ?, 1, 4, ?)`,
			fmt.Sprintf("old%013x", i+1), ts.Format(time.RFC3339), fmt.Sprintf(`{"type":"ADVERT","pubKey":%q}`, pk))
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		if _, err := srv.db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp) VALUES (?, 1, '[]', ?)`, id, ts.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	srv.store.oldestLoaded = "2026-01-01T00:00:00Z" // the real loaded boundary

	for _, grouped := range []bool{false, true} {
		g := fmt.Sprintf("&groupByHash=%t", grouped)
		// In-memory control: the old rows are invisible, area applies.
		if got := getArea569(t, srv, "limit=50&area=BEL"+g); got.Total != 6 {
			t.Fatalf("memory control total=%d want 6", got.Total)
		}
		for _, window := range []string{"since=2025-11-30T00:00:00Z", "since=2025-11-30T00:00:00Z&until=2026-01-15T00:00:00Z", "until=2025-12-31T00:00:00Z"} {
			wantTotal := 8 // 6 in memory + old a, old b
			if strings.HasPrefix(window, "until=") {
				wantTotal = 2 // only the SQL-only side
			}
			seen := map[string]bool{}
			for offset := 0; offset < wantTotal+2; offset += 3 {
				got := getArea569(t, srv, fmt.Sprintf("limit=3&offset=%d&%s&area=BEL%s", offset, window, g))
				if got.Total != wantTotal {
					t.Fatalf("%s grouped=%t offset=%d total=%d want %d", window, grouped, offset, got.Total, wantTotal)
				}
				for _, h := range got.Hashes {
					if h == "old0000000000002" || seen[h] {
						t.Fatalf("%s grouped=%t: outside or duplicate row %s", window, grouped, h)
					}
					seen[h] = true
				}
			}
			if len(seen) != wantTotal || !seen["old0000000000001"] || !seen["old0000000000003"] {
				t.Fatalf("%s grouped=%t: pages covered %v, want %d rows incl. both old inside rows", window, grouped, seen, wantTotal)
			}
		}
	}
}
