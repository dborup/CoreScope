package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Node-detail advert route breakdown (port/extension of upstream
// `Kpa-clawbot/CoreScope#2073`): recentAdvertsByRoute lists the newest
// adverts per route class (class filtered in SQL) and advertCounts counts
// distinct adverts per class over 24h and 7d, both classified exactly like
// Relay Airtime Share (#89).

const narNode = "2073aa0011223344556677889900aabbccddeeff00112233445566778899aabb"

// narDB is a test DB with the route_mask column, like a migrated database.
func narDB(t *testing.T) *DB {
	t.Helper()
	db := setupTestDB(t)
	narAddRouteMask(t, db)
	return db
}

func narAddRouteMask(t *testing.T, db *DB) {
	t.Helper()
	if _, err := db.conn.Exec(`ALTER TABLE transmissions ADD COLUMN route_mask INTEGER`); err != nil {
		t.Fatal(err)
	}
	db.hasRouteMaskFlag.forceTrue()
}

// narInsert seeds one transmission from pubkey. mask and routeType may be
// nil (NULL); withMask=false omits the route_mask column (legacy schema).
func narInsert(t *testing.T, db *DB, pubkey, hash string, payloadType int, routeType, mask interface{}, firstSeen string, withMask bool) int64 {
	t.Helper()
	var res sql.Result
	var err error
	if withMask {
		res, err = db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey, route_mask)
			VALUES ('1100', ?, ?, ?, ?, '{"type":"ADVERT"}', ?, ?)`, hash, firstSeen, routeType, payloadType, pubkey, mask)
	} else {
		res, err = db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey)
			VALUES ('1100', ?, ?, ?, ?, '{"type":"ADVERT"}', ?)`, hash, firstSeen, routeType, payloadType, pubkey)
	}
	if err != nil {
		t.Fatalf("insert %s: %v", hash, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func narAgo(d time.Duration) string {
	return time.Now().UTC().Add(-d).Format(time.RFC3339)
}

func narHashes(rows NodeAdvertRows) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		h, _ := r["hash"].(string)
		out = append(out, h)
	}
	return out
}

func narRouteTypes() []*int {
	out := []*int{nil}
	for _, v := range []int{-1, 0, 1, 2, 3, 4, 99} {
		v := v
		out = append(out, &v)
	}
	return out
}

// classifyAdvertRoute is the rule behind advertRouteClass: Relay Airtime
// Share must classify every (mask, known, route_type) exactly as before.
func TestClassifyAdvertRoute_ParityWithRelayAirtimeShare(t *testing.T) {
	for mask := 0; mask < 32; mask++ {
		for _, known := range []bool{false, true} {
			for _, rt := range narRouteTypes() {
				tx := &StoreTx{RouteType: rt, routeMask: uint8(mask), routeMaskKnown: known}
				if got, want := classifyAdvertRoute(int64(mask), known, rt), advertRouteClass(tx); got != want {
					t.Fatalf("mask=%d known=%v rt=%v: classifyAdvertRoute=%v, advertRouteClass=%v", mask, known, rt, got, want)
				}
			}
		}
	}
}

// The SQL CASE that filters the per-class lists must agree with the Go
// classifier for every mask (NULL, 0..31 including bits above route 3) and
// every route_type (NULL, invalid, 0..3), with and without the column.
func TestAdvertRouteClassSQL_MatchesGoClassifier(t *testing.T) {
	db := narDB(t)
	masks := []interface{}{nil}
	for m := 0; m < 32; m++ {
		masks = append(masks, m)
	}
	masks = append(masks, -1)
	type want struct {
		mask sql.NullInt64
		rt   sql.NullInt64
	}
	seeded := map[string]want{}
	i := 0
	for _, m := range masks {
		for _, rt := range narRouteTypes() {
			var rtv interface{}
			w := want{}
			if rt != nil {
				rtv = *rt
				w.rt = sql.NullInt64{Int64: int64(*rt), Valid: true}
			}
			if m != nil {
				w.mask = sql.NullInt64{Int64: int64(m.(int)), Valid: true}
			}
			hash := fmt.Sprintf("parity%04d", i)
			i++
			narInsert(t, db, narNode, hash, payloadTypeAdvert, rtv, m, narAgo(time.Hour), true)
			seeded[hash] = w
		}
	}
	for _, withMask := range []bool{true, false} {
		rows, err := db.conn.Query(`SELECT hash, ` + advertRouteClassSQL(withMask) + ` FROM transmissions WHERE hash LIKE 'parity%'`)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for rows.Next() {
			var hash, class string
			if err := rows.Scan(&hash, &class); err != nil {
				t.Fatal(err)
			}
			w := seeded[hash]
			mask := w.mask
			if !withMask {
				mask = sql.NullInt64{} // column absent: always the legacy fallback
			}
			if goClass := advertRouteClassName(classifyAdvertRouteRow(mask, w.rt)); class != goClass {
				t.Fatalf("withMask=%v %s (mask=%v rt=%v): SQL=%q Go=%q", withMask, hash, w.mask, w.rt, class, goClass)
			}
			n++
		}
		rows.Close()
		if n != len(seeded) {
			t.Fatalf("withMask=%v: compared %d rows, seeded %d", withMask, n, len(seeded))
		}
	}
}

// Frequent zero-hop adverts must not push the rare flood adverts out: the
// class filter runs in SQL before the per-class limit. The chronological
// 20 hold no flood advert at all, so a client-side split would show none.
func TestNodeAdvertRoutes_PerClassLimitInSQL(t *testing.T) {
	db := narDB(t)
	narInsert(t, db, narNode, "flood-a", payloadTypeAdvert, 0, 0b0001, narAgo(6*time.Hour), true)
	narInsert(t, db, narNode, "flood-b", payloadTypeAdvert, 1, 0b0010, narAgo(5*time.Hour), true)
	for i := 0; i < 100; i++ {
		narInsert(t, db, narNode, fmt.Sprintf("zh%03d", i), payloadTypeAdvert, 2, 0b0100, narAgo(time.Duration(100-i)*time.Minute), true)
	}
	byRoute, counts, _, err := db.GetNodeAdvertRoutes(narNode, nodeAdvertRouteLimit, time.Now(), floodAdvertRowCap)
	if err != nil {
		t.Fatal(err)
	}
	if got := narHashes(byRoute.Flood); !reflect.DeepEqual(got, []string{"flood-b", "flood-a"}) {
		t.Fatalf("flood list = %v, want both flood adverts, newest ingest first", got)
	}
	if len(byRoute.ZeroHop) != nodeAdvertRouteLimit || narHashes(byRoute.ZeroHop)[0] != "zh099" {
		t.Fatalf("zero_hop list = %v, want the newest %d", narHashes(byRoute.ZeroHop), nodeAdvertRouteLimit)
	}
	for _, r := range byRoute.Flood {
		if r["route_class"] != advertClassFlood {
			t.Fatalf("flood row route_class = %v", r["route_class"])
		}
		// The per-class rows are lean: no observation arrays (cached, and
		// repeated next to recentAdverts), but the best observation's fields.
		if _, ok := r["observations"]; ok {
			t.Fatal("per-class rows must not carry observations")
		}
		if _, ok := r["observation_count"]; !ok {
			t.Fatal("per-class rows keep observation_count")
		}
	}
	recent, err := db.GetRecentTransmissionsForNode(narNode, 20, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recent {
		if r["route_class"] == advertClassFlood {
			t.Fatalf("chronological list unexpectedly holds a flood advert: %v", r["hash"])
		}
	}
	if counts.H24 != (AdvertRouteCounts{Flood: 2, ZeroHop: 100}) {
		t.Fatalf("24h counts = %+v", counts.H24)
	}
}

// A hash seen on flood and zero-hop routes is one mixed advert: it is listed
// and counted only as mixed, never as flood or zero-hop.
func TestNodeAdvertRoutes_MixedOnlyInMixed(t *testing.T) {
	db := narDB(t)
	narInsert(t, db, narNode, "mixed1", payloadTypeAdvert, 1, 0b0110, narAgo(time.Hour), true)
	narInsert(t, db, narNode, "flood1", payloadTypeAdvert, 1, 0b0010, narAgo(2*time.Hour), true)
	narInsert(t, db, narNode, "zh1", payloadTypeAdvert, 2, 0b0100, narAgo(3*time.Hour), true)
	byRoute, counts, _, err := db.GetNodeAdvertRoutes(narNode, nodeAdvertRouteLimit, time.Now(), floodAdvertRowCap)
	if err != nil {
		t.Fatal(err)
	}
	if got := narHashes(byRoute.Mixed); !reflect.DeepEqual(got, []string{"mixed1"}) {
		t.Fatalf("mixed list = %v", got)
	}
	if got := narHashes(byRoute.Flood); !reflect.DeepEqual(got, []string{"flood1"}) {
		t.Fatalf("flood list = %v (mixed advert must not appear)", got)
	}
	if got := narHashes(byRoute.ZeroHop); !reflect.DeepEqual(got, []string{"zh1"}) {
		t.Fatalf("zero_hop list = %v (mixed advert must not appear)", got)
	}
	want := AdvertRouteCounts{Flood: 1, ZeroHop: 1, Mixed: 1}
	if counts.H24 != want || counts.D7 != want {
		t.Fatalf("counts 24h=%+v 7d=%+v, want %+v in both", counts.H24, counts.D7, want)
	}
}

// Rows the backfill has not reached (NULL mask) and databases without the
// column fall back to the first-inserted route_type, as #89 does: routes 0
// and 1 are flood, 2 and 3 zero-hop, NULL route unknown. Unknown is only
// listed when there is data.
func TestNodeAdvertRoutes_LegacyFallback(t *testing.T) {
	for _, withMask := range []bool{true, false} {
		t.Run(fmt.Sprintf("column=%v", withMask), func(t *testing.T) {
			db := setupTestDB(t)
			if withMask {
				narAddRouteMask(t, db)
			}
			narInsert(t, db, narNode, "legacy-r0", payloadTypeAdvert, 0, nil, narAgo(time.Hour), withMask)
			narInsert(t, db, narNode, "legacy-r1", payloadTypeAdvert, 1, nil, narAgo(2*time.Hour), withMask)
			narInsert(t, db, narNode, "legacy-r2", payloadTypeAdvert, 2, nil, narAgo(3*time.Hour), withMask)
			byRoute, counts, _, err := db.GetNodeAdvertRoutes(narNode, nodeAdvertRouteLimit, time.Now(), floodAdvertRowCap)
			if err != nil {
				t.Fatal(err)
			}
			if got := narHashes(byRoute.Flood); !reflect.DeepEqual(got, []string{"legacy-r1", "legacy-r0"}) {
				t.Fatalf("flood list = %v", got)
			}
			if got := narHashes(byRoute.ZeroHop); !reflect.DeepEqual(got, []string{"legacy-r2"}) {
				t.Fatalf("zero_hop list = %v", got)
			}
			if byRoute.Unknown != nil {
				t.Fatalf("unknown list must be absent without data, got %v", narHashes(byRoute.Unknown))
			}
			if counts.H24 != (AdvertRouteCounts{Flood: 2, ZeroHop: 1}) {
				t.Fatalf("24h counts = %+v", counts.H24)
			}
			narInsert(t, db, narNode, "legacy-null", payloadTypeAdvert, nil, nil, narAgo(4*time.Hour), withMask)
			byRoute, counts, _, err = db.GetNodeAdvertRoutes(narNode, nodeAdvertRouteLimit, time.Now(), floodAdvertRowCap)
			if err != nil {
				t.Fatal(err)
			}
			if got := narHashes(byRoute.Unknown); !reflect.DeepEqual(got, []string{"legacy-null"}) {
				t.Fatalf("unknown list = %v", got)
			}
			if counts.H24.Unknown != 1 {
				t.Fatalf("24h unknown = %d, want 1", counts.H24.Unknown)
			}
		})
	}
}

// Only ADVERTs (payload_type 4) are listed and counted.
func TestNodeAdvertRoutes_AdvertsOnly(t *testing.T) {
	db := narDB(t)
	narInsert(t, db, narNode, "grp1", 5, 1, 0b0010, narAgo(time.Hour), true)
	narInsert(t, db, narNode, "adv1", payloadTypeAdvert, 1, 0b0010, narAgo(2*time.Hour), true)
	byRoute, counts, _, err := db.GetNodeAdvertRoutes(narNode, nodeAdvertRouteLimit, time.Now(), floodAdvertRowCap)
	if err != nil {
		t.Fatal(err)
	}
	if got := narHashes(byRoute.Flood); !reflect.DeepEqual(got, []string{"adv1"}) {
		t.Fatalf("flood list = %v", got)
	}
	if counts.H24.Flood != 1 {
		t.Fatalf("24h flood = %d, want 1", counts.H24.Flood)
	}
}

// Window boundaries on first_seen, across the first_seen formats the
// ingestor writes: the 24h window holds only the last 24 hours; 7d excludes
// an advert that sits inside the SQL date floor (window + 1 day slack) but
// outside 7 days. A legacy space-separated value is skipped, exactly as
// flood_advert_count_7d skips it (shared parseRelayTS window).
func TestNodeAdvertRoutes_WindowsAndTimestampFormats(t *testing.T) {
	db := narDB(t)
	now := time.Now().UTC()
	at := func(d time.Duration, layout string) string { return now.Add(-d).Format(layout) }
	narInsert(t, db, narNode, "w-1h-rfc", payloadTypeAdvert, 1, 0b0010, at(time.Hour, time.RFC3339), true)
	narInsert(t, db, narNode, "w-2h-nano", payloadTypeAdvert, 1, 0b0010, at(2*time.Hour, time.RFC3339Nano), true)
	narInsert(t, db, narNode, "w-2h-space", payloadTypeAdvert, 1, 0b0010, at(2*time.Hour, "2006-01-02 15:04:05"), true)
	narInsert(t, db, narNode, "w-3h-millis", payloadTypeAdvert, 2, 0b0100, at(3*time.Hour, "2006-01-02T15:04:05.000Z"), true)
	narInsert(t, db, narNode, "w-23h", payloadTypeAdvert, 2, 0b1000, at(23*time.Hour, time.RFC3339), true)
	narInsert(t, db, narNode, "w-25h", payloadTypeAdvert, 1, 0b0001, at(25*time.Hour, time.RFC3339), true)
	narInsert(t, db, narNode, "w-6d-millis", payloadTypeAdvert, 2, 0b0100, at(6*24*time.Hour, "2006-01-02T15:04:05.000Z"), true)
	narInsert(t, db, narNode, "w-7.5d", payloadTypeAdvert, 1, 0b0010, at(180*time.Hour, time.RFC3339), true)
	narInsert(t, db, narNode, "w-9d", payloadTypeAdvert, 1, 0b0010, at(9*24*time.Hour, time.RFC3339), true)
	narInsert(t, db, narNode, "w-bad", payloadTypeAdvert, 1, 0b0010, "not-a-time-but-sorts-high", true)
	_, counts, _, err := db.GetNodeAdvertRoutes(narNode, nodeAdvertRouteLimit, now, floodAdvertRowCap)
	if err != nil {
		t.Fatal(err)
	}
	if want := (AdvertRouteCounts{Flood: 2, ZeroHop: 2}); counts.H24 != want {
		t.Fatalf("24h = %+v, want %+v", counts.H24, want)
	}
	if want := (AdvertRouteCounts{Flood: 3, ZeroHop: 3}); counts.D7 != want {
		t.Fatalf("7d = %+v, want %+v", counts.D7, want)
	}
	if counts.Truncated {
		t.Fatal("not truncated below the row cap")
	}
}

// Pure counter: duplicate hashes count once, hash-less rows dedup by
// timestamp, unparseable timestamps are skipped, every class is counted.
func TestCountAdvertRoutes_DedupAndClasses(t *testing.T) {
	now := time.Now()
	entries := []advertRouteEntry{
		{ts: advertTS(1), hash: "d1", class: advertClassFlood},
		{ts: advertTS(2), hash: "d1", class: advertClassFlood}, // same advert, second row
		{ts: advertTS(3), hash: "d2", class: advertClassZeroHop},
		{ts: advertTS(4), hash: "d3", class: advertClassMixed},
		{ts: advertTS(5), hash: "d4", class: advertClassUnknown},
		{ts: advertTS(6), class: advertClassZeroHop},
		{ts: advertTS(7), class: advertClassZeroHop},
		{ts: "garbage", hash: "d5", class: advertClassFlood},
		{ts: advertTS(30), hash: "d6", class: advertClassFlood},
	}
	if got, want := countAdvertRoutes(entries, now, 24), (AdvertRouteCounts{Flood: 1, ZeroHop: 3, Mixed: 1, Unknown: 1}); got != want {
		t.Fatalf("24h = %+v, want %+v", got, want)
	}
	if got, want := countAdvertRoutes(entries, now, 7*24), (AdvertRouteCounts{Flood: 2, ZeroHop: 3, Mixed: 1, Unknown: 1}); got != want {
		t.Fatalf("7d = %+v, want %+v", got, want)
	}
}

// The row cap bounds per-request work: past it the counts saturate (newest
// rows win) and the response says so; the per-class lists are unaffected.
func TestNodeAdvertRoutes_RowCap(t *testing.T) {
	db := narDB(t)
	narInsert(t, db, narNode, "cap-flood", payloadTypeAdvert, 1, 0b0010, narAgo(10*time.Hour), true)
	for i := 0; i < 3; i++ {
		narInsert(t, db, narNode, fmt.Sprintf("cap%d", i), payloadTypeAdvert, 2, 0b0100, narAgo(time.Duration(3-i)*time.Hour), true)
	}
	byRoute, counts, _, err := db.GetNodeAdvertRoutes(narNode, nodeAdvertRouteLimit, time.Now(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if !counts.Truncated {
		t.Fatal("want truncated=true past the row cap")
	}
	if counts.H24 != (AdvertRouteCounts{ZeroHop: 2}) {
		t.Fatalf("24h = %+v, want the newest 2 rows only", counts.H24)
	}
	if got := narHashes(byRoute.Flood); !reflect.DeepEqual(got, []string{"cap-flood"}) {
		t.Fatalf("flood list = %v, the row cap must not starve the lists", got)
	}
}

type narResponse struct {
	Node          map[string]interface{}   `json:"node"`
	RecentAdverts []map[string]interface{} `json:"recentAdverts"`
	ByRoute       *struct {
		Limit   int                      `json:"limit"`
		Flood   []map[string]interface{} `json:"flood"`
		ZeroHop []map[string]interface{} `json:"zero_hop"`
		Mixed   []map[string]interface{} `json:"mixed"`
		Unknown []map[string]interface{} `json:"unknown"`
	} `json:"recentAdvertsByRoute"`
	Counts *struct {
		H24       map[string]int          `json:"24h"`
		D7        map[string]int          `json:"7d"`
		Truncated bool                    `json:"truncated"`
		Backfill  RouteMaskBackfillStatus `json:"route_mask_backfill"`
	} `json:"advertCounts"`
}

func narGetNode(t *testing.T, router http.Handler, pubkey string, wantCode int) (narResponse, string) {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/nodes/"+pubkey, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != wantCode {
		t.Fatalf("GET /api/nodes/%s = %d, want %d: %s", pubkey, w.Code, wantCode, w.Body.String())
	}
	var body narResponse
	if wantCode == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("bad JSON: %v", err)
		}
	}
	return body, w.Body.String()
}

func narServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	srv, router := setupTestServer(t)
	narAddRouteMask(t, srv.db)
	if _, err := srv.db.conn.Exec(`INSERT INTO nodes (public_key, name, role, last_seen, first_seen) VALUES (?, 'Route Mix', 'repeater', ?, ?)`,
		narNode, narAgo(time.Hour), narAgo(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	narInsert(t, srv.db, narNode, "h-flood-r0", payloadTypeAdvert, 0, 0b0001, narAgo(time.Hour), true)
	narInsert(t, srv.db, narNode, "h-flood-r1", payloadTypeAdvert, 1, 0b0010, narAgo(2*time.Hour), true)
	narInsert(t, srv.db, narNode, "h-mixed", payloadTypeAdvert, 2, 0b0110, narAgo(3*time.Hour), true)
	narInsert(t, srv.db, narNode, "h-zh", payloadTypeAdvert, 2, 0b0100, narAgo(4*time.Hour), true)
	narInsert(t, srv.db, narNode, "h-legacy", payloadTypeAdvert, 1, nil, narAgo(30*time.Hour), true)
	return srv, router
}

// The wire contract: recentAdverts keeps its shape (plus route_class),
// recentAdvertsByRoute and advertCounts are added, and flood_advert_count_7d
// keeps its external meaning.
func TestNodeDetail_AdvertRouteFields(t *testing.T) {
	_, router := narServer(t)
	body, _ := narGetNode(t, router, narNode+narIncludeQuery, 200)
	if len(body.RecentAdverts) != 5 {
		t.Fatalf("recentAdverts = %d rows, want 5", len(body.RecentAdverts))
	}
	for _, key := range []string{"id", "hash", "first_seen", "timestamp", "route_type", "payload_type", "observations", "observation_count", "route_class"} {
		if _, ok := body.RecentAdverts[0][key]; !ok {
			t.Fatalf("recentAdverts row lacks %q: %v", key, body.RecentAdverts[0])
		}
	}
	if body.RecentAdverts[0]["hash"] != "h-legacy" || body.RecentAdverts[0]["route_class"] != advertClassFlood {
		t.Fatalf("recentAdverts must stay in ingest (id DESC) order: first = %v/%v", body.RecentAdverts[0]["hash"], body.RecentAdverts[0]["route_class"])
	}
	if body.ByRoute == nil || body.Counts == nil {
		t.Fatal("recentAdvertsByRoute / advertCounts missing")
	}
	if body.ByRoute.Limit != nodeAdvertRouteLimit || len(body.ByRoute.Flood) != 3 || len(body.ByRoute.Mixed) != 1 || len(body.ByRoute.ZeroHop) != 1 {
		t.Fatalf("byRoute limit=%d flood=%d mixed=%d zero_hop=%d", body.ByRoute.Limit, len(body.ByRoute.Flood), len(body.ByRoute.Mixed), len(body.ByRoute.ZeroHop))
	}
	if body.ByRoute.Unknown != nil {
		t.Fatal("unknown list must be omitted without data")
	}
	if got := body.Counts.H24; got["flood"] != 2 || got["zero_hop"] != 1 || got["mixed"] != 1 || got["unknown"] != 0 {
		t.Fatalf("24h = %v", got)
	}
	if got := body.Counts.D7; got["flood"] != 3 || got["zero_hop"] != 1 || got["mixed"] != 1 {
		t.Fatalf("7d = %v", got)
	}
	if body.Counts.Backfill.Status == "" {
		t.Fatal("route_mask_backfill status missing")
	}
	// flood_advert_count_7d is an external contract (ArcScope advisor): it
	// still counts route_type 1 only, so it leaves out route 0 (transport
	// flood) and the mixed advert whose first-inserted route was zero-hop -
	// here 2 against advertCounts["7d"].flood = 3 (+1 mixed).
	if got := body.Node["flood_advert_count_7d"]; got != float64(2) {
		t.Fatalf("flood_advert_count_7d = %v, want 2 (route_type 1 only, unchanged)", got)
	}
}

// Privacy (#68): the new fields follow the node-detail visibility rule and
// identityHidden. Blacklisted and hidden-name nodes stay 404; an identity
// hidden only through the observer blacklist gets no route breakdown.
func TestNodeDetail_AdvertRouteFieldsPrivacy(t *testing.T) {
	t.Run("node blacklist", func(t *testing.T) {
		srv, router := narServer(t)
		srv.cfg.SetNodeBlacklist([]string{narNode})
		_, raw := narGetNode(t, router, narNode+narIncludeQuery, 404)
		if strings.Contains(raw, "advertCounts") || strings.Contains(raw, "h-flood") {
			t.Fatalf("blacklisted node leaked: %s", raw)
		}
	})
	t.Run("hidden name prefix", func(t *testing.T) {
		srv, router := narServer(t)
		srv.cfg.SetHiddenNamePrefixes([]string{"Route"})
		_, raw := narGetNode(t, router, narNode+narIncludeQuery, 404)
		if strings.Contains(raw, "advertCounts") || strings.Contains(raw, "h-flood") {
			t.Fatalf("hidden node leaked: %s", raw)
		}
	})
	// route_class is route_mask data too: it must not ride along on the
	// recentAdverts rows of a hidden identity.
	leaked := func(body narResponse, raw string) bool {
		return body.ByRoute != nil || body.Counts != nil || strings.Contains(raw, "advertCounts") ||
			strings.Contains(raw, "recentAdvertsByRoute") || strings.Contains(raw, "route_class")
	}
	t.Run("observer blacklist", func(t *testing.T) {
		srv, router := narServer(t)
		srv.cfg.ObserverBlacklist = []string{strings.ToUpper(narNode)} // read lazily on first use
		body, raw := narGetNode(t, router, narNode+narIncludeQuery, 200)
		if leaked(body, raw) || len(body.RecentAdverts) != 5 {
			t.Fatalf("identity hidden via observer blacklist leaked route fields (or lost recentAdverts): %s", raw)
		}
	})
	t.Run("hidden observer name", func(t *testing.T) {
		srv, router := narServer(t)
		if _, err := srv.db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES (?, 'Secret Observer', 'TST')`, narNode); err != nil {
			t.Fatal(err)
		}
		srv.cfg.SetHiddenNamePrefixes([]string{"Secret"})
		body, raw := narGetNode(t, router, narNode+narIncludeQuery, 200)
		if leaked(body, raw) {
			t.Fatalf("identity hidden via its observer name leaked route fields: %s", raw)
		}
	})
}

// recentAdverts keeps its chronological role for nodeWithHealthActivity and
// other consumers: same 20-row limit and id DESC order as before. route_class
// is added only when asked for (include=advertRoutes); otherwise the rows are
// exactly master's.
func TestGetRecentTransmissionsForNode_RouteClassAdditive(t *testing.T) {
	db := narDB(t)
	for i := 0; i < 25; i++ {
		narInsert(t, db, narNode, fmt.Sprintf("r%02d", i), payloadTypeAdvert, 2, 0b0100, narAgo(time.Duration(25-i)*time.Minute), true)
	}
	narInsert(t, db, narNode, "r-grp", 5, 1, 0b0010, narAgo(time.Minute), true)
	for _, withRouteClass := range []bool{true, false} {
		rows, err := db.GetRecentTransmissionsForNode(narNode, 20, withRouteClass)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 20 || rows[0]["hash"] != "r-grp" || rows[1]["hash"] != "r24" {
			t.Fatalf("withRouteClass=%v: rows=%d first=%v second=%v", withRouteClass, len(rows), rows[0]["hash"], rows[1]["hash"])
		}
		if _, ok := rows[0]["route_class"]; ok {
			t.Fatal("route_class is an ADVERT classification; non-advert rows must not carry it")
		}
		if _, ok := rows[1]["observations"]; !ok {
			t.Fatalf("withRouteClass=%v: recentAdverts rows keep their observations", withRouteClass)
		}
		got, ok := rows[1]["route_class"]
		if withRouteClass && got != advertClassZeroHop {
			t.Fatalf("advert route_class = %v", got)
		}
		if !withRouteClass && ok {
			t.Fatalf("without include=advertRoutes the rows must not carry route_class, got %v", got)
		}
	}
}

// OpenAPI documents every JSON field the new response types emit.
func TestOpenAPI_NodeAdvertRouteSchemas(t *testing.T) {
	spec := fetchSpec(t)
	schemas := asMap(t, asMap(t, spec["components"], "components")["schemas"], "schemas")
	props := func(name string) map[string]interface{} {
		return asMap(t, asMap(t, schemas[name], name)["properties"], name+".properties")
	}
	check := func(schema string, v interface{}) {
		p := props(schema)
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			tag := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
			if tag == "" || tag == "-" {
				continue
			}
			if _, ok := p[tag]; !ok {
				t.Errorf("%s.%s is emitted but not documented", schema, tag)
			}
		}
	}
	check("NodeDetailResponse", NodeDetailResponse{})
	check("NodeAdvertsByRoute", NodeAdvertsByRoute{})
	check("NodeAdvertCounts", NodeAdvertCounts{})
	check("AdvertRouteCounts", AdvertRouteCounts{})
	if _, ok := props("NodeAdvert")["route_class"]; !ok {
		t.Error("NodeAdvert.route_class is emitted but not documented")
	}
	get := asMap(t, asMap(t, asMap(t, spec["paths"], "paths")["/api/nodes/{pubkey}"], "/api/nodes/{pubkey}")["get"], "get")
	params, _ := get["parameters"].([]interface{})
	documented := false
	for _, p := range params {
		if m, ok := p.(map[string]interface{}); ok && m["name"] == "include" && m["in"] == "query" &&
			strings.Contains(fmt.Sprint(m["description"]), nodeDetailIncludeAdvertRoutes) {
			documented = true
		}
	}
	if !documented {
		t.Errorf("GET /api/nodes/{pubkey} must document the include=%s opt-in: %v", nodeDetailIncludeAdvertRoutes, params)
	}
}
