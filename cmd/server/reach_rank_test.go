package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
	_ "modernc.org/sqlite"
)

// ---- fixtures ---------------------------------------------------------------

type rankTestNode struct {
	pk, name string
}

type rankTestObserver struct {
	id, name string // id as stored (observer ids are upper-case in production)
}

// newReachRankDB builds an in-memory DB with the columns the Reach and
// leaderboard paths read. edges are [a, b] pubkey pairs; each is one
// neighbor_edges row (canonical order is irrelevant to the degree count).
func newReachRankDB(t testing.TB, nodes []rankTestNode, observers []rankTestObserver, edges [][2]string) *DB {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1) // one shared :memory: database
	stmts := []string{
		`CREATE TABLE nodes (public_key TEXT PRIMARY KEY, name TEXT, role TEXT, lat REAL, lon REAL, last_seen TEXT, first_seen TEXT, advert_count INTEGER DEFAULT 0, battery_mv INTEGER, temperature_c REAL, foreign_advert INTEGER DEFAULT 0)`,
		`CREATE TABLE observers (id TEXT PRIMARY KEY, name TEXT)`,
		`CREATE TABLE transmissions (id INTEGER PRIMARY KEY, from_pubkey TEXT, payload_type INTEGER)`,
		`CREATE TABLE observations (id INTEGER PRIMARY KEY, transmission_id INTEGER, observer_idx INTEGER, snr REAL, path_json TEXT, timestamp INTEGER)`,
		`CREATE TABLE neighbor_edges (node_a TEXT NOT NULL, node_b TEXT NOT NULL, count INTEGER DEFAULT 1, last_seen TEXT, PRIMARY KEY (node_a, node_b))`,
	}
	for _, s := range stmts {
		if _, err := conn.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if _, err := tx.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count) VALUES (?, ?, 'repeater', 56.1, 10.2, '2026-09-01T00:00:00Z', '2026-06-01T00:00:00Z', 1)`, n.pk, n.name); err != nil {
			t.Fatal(err)
		}
	}
	for _, o := range observers {
		if _, err := tx.Exec(`INSERT INTO observers (id, name) VALUES (?, ?)`, o.id, o.name); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range edges {
		if _, err := tx.Exec(`INSERT INTO neighbor_edges (node_a, node_b, count, last_seen) VALUES (?, ?, 1, '2026-09-01T00:00:00Z')`, e[0], e[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	db := &DB{conn: conn}
	db.isV3Flag.forceTrue()
	t.Cleanup(func() { conn.Close() })
	return db
}

func newReachRankServer(t *testing.T, db *DB, cfg *Config) *Server {
	t.Helper()
	srv := &Server{store: newTestStoreWithDB(t, db, cfg), db: db, cfg: cfg, perfStats: NewPerfStats()}
	resetReachState(t, srv)
	return srv
}

func serveRankRoutes(srv *Server, path string) *httptest.ResponseRecorder {
	router := mux.NewRouter()
	router.HandleFunc("/api/nodes/{pubkey}/reach", srv.handleNodeReach).Methods("GET")
	router.HandleFunc("/api/reach-rank", srv.handleReachRank).Methods("GET")
	req := httptest.NewRequest("GET", path, nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func getRank(t *testing.T, srv *Server, path string) ReachRankResponse {
	t.Helper()
	rr := serveRankRoutes(srv, path)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", path, rr.Code, rr.Body.String())
	}
	var resp ReachRankResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("GET %s: bad json: %v", path, err)
	}
	return resp
}

func getReachImportance(t *testing.T, srv *Server, pk string) NodeReachImportance {
	t.Helper()
	rr := serveRankRoutes(srv, "/api/nodes/"+pk+"/reach?days=30")
	if rr.Code != http.StatusOK {
		t.Fatalf("reach %s: status=%d body=%s", pk, rr.Code, rr.Body.String())
	}
	var resp NodeReachResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("reach %s: bad json: %v", pk, err)
	}
	return resp.Importance
}

// star returns edges joining hub to each of spokes.
func star(hub string, spokes ...string) [][2]string {
	out := make([][2]string, 0, len(spokes))
	for _, s := range spokes {
		out = append(out, [2]string{hub, s})
	}
	return out
}

func snapOf(deg map[string]int, ident map[string]rankIdent) *degreeSnapshot {
	return &degreeSnapshot{at: time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC), deg: deg, ident: ident}
}

func node(name string) rankIdent { return rankIdent{hasNode: true, nodeName: name} }

// expireDegreeSnapshot backdates the published snapshot past its TTL (test
// only — single goroutine) and returns its new timestamp.
func expireDegreeSnapshot(srv *Server) time.Time {
	srv.reach.degreeMu.Lock()
	defer srv.reach.degreeMu.Unlock()
	srv.reach.degreeSnap.at = srv.reach.degreeSnap.at.Add(-reachDegreeTTL - time.Minute)
	return srv.reach.degreeSnap.at
}

func rowPKs(rows []reachRankRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Pubkey
	}
	return out
}

func rowRanks(rows []reachRankRow) []int {
	out := make([]int, len(rows))
	for i, r := range rows {
		out[i] = r.Rank
	}
	return out
}

// ---- ranking contract (pure) -----------------------------------------------

func TestReachRankView_CompetitionRankingDeterministicOrder(t *testing.T) {
	a, b, c, d, e := pk64("a1"), pk64("b2"), pk64("c3"), pk64("d4"), pk64("e5")
	snap := snapOf(
		map[string]int{e: 5, a: 5, d: 3, c: 3, b: 1},
		map[string]rankIdent{a: node("A"), b: node("B"), c: node("C"), d: node("D"), e: node("E")},
	)
	want := []string{a, e, c, d, b}
	for i := 0; i < 25; i++ { // map iteration order is randomised per build
		v := buildReachRankView(snap, &Config{}, 1, 0, 0)
		if got := rowPKs(v.rows); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("build %d: order=%v want %v (neighbours desc, pubkey asc)", i, got, want)
		}
		if got := rowRanks(v.rows); fmt.Sprint(got) != "[1 1 3 3 5]" {
			t.Fatalf("build %d: ranks=%v want [1 1 3 3 5] (competition ranking)", i, got)
		}
	}
	v := buildReachRankView(snap, &Config{}, 1, 0, 0)
	if deg, rank, total, ok := v.lookup(d); !ok || deg != 3 || rank != 3 || total != 5 {
		t.Fatalf("lookup(d)=(%d,%d,%d,%v) want (3,3,5,true)", deg, rank, total, ok)
	}
}

func TestReachRankView_EmptyAndUnranked(t *testing.T) {
	v := buildReachRankView(snapOf(map[string]int{}, map[string]rankIdent{}), &Config{}, 1, 0, 0)
	if len(v.rows) != 0 {
		t.Fatalf("empty snapshot: %d rows", len(v.rows))
	}
	rows, matched := v.page("", 0, 50)
	if rows == nil || len(rows) != 0 || matched != 0 {
		t.Fatalf("empty page: rows=%v matched=%d (want non-nil empty, 0)", rows, matched)
	}
	if deg, rank, total, ok := v.lookup(pk64("aa")); ok || deg != 0 || rank != 0 || total != 0 {
		t.Fatalf("lookup on empty view = (%d,%d,%d,%v)", deg, rank, total, ok)
	}

	// Known node without edges, and edge endpoints with no node/observer
	// record (incl. the empty pubkey legacy rows carry) are not ranked.
	lonely, a, ghost := pk64("10"), pk64("a1"), pk64("99")
	snap := snapOf(
		map[string]int{a: 2, ghost: 9, "": 55},
		map[string]rankIdent{lonely: node("Lonely"), a: node("A")},
	)
	v = buildReachRankView(snap, &Config{}, 1, 0, 0)
	if got := rowPKs(v.rows); fmt.Sprint(got) != fmt.Sprint([]string{a}) {
		t.Fatalf("ranked=%v want only %s", got, a)
	}
	if _, rank, total, ok := v.lookup(a); !ok || rank != 1 || total != 1 {
		t.Fatalf("a must be #1/1 once unknown endpoints are excluded, got #%d/%d", rank, total)
	}
	if deg, _, _, ok := v.lookup(lonely); ok || deg != 0 {
		t.Fatalf("node without edges must be unranked with degree 0, got degree=%d ranked=%v", deg, ok)
	}
}

func TestReachRankView_SearchAndPagingKeepGlobalRanks(t *testing.T) {
	deg := map[string]int{}
	ident := map[string]rankIdent{}
	for i := 0; i < 120; i++ {
		pk := pk64(fmt.Sprintf("%04x", i))
		deg[pk] = 200 - i/2 // pairs tie: ranks 1,1,3,3,...
		name := fmt.Sprintf("Relay-%03d", i)
		if i%10 == 0 {
			name = fmt.Sprintf("Aarhus Hub %03d", i)
		}
		ident[pk] = node(name)
	}
	v := buildReachRankView(snapOf(deg, ident), &Config{}, 1, 0, 0)

	p2, matched := v.page("", 50, 50)
	if matched != 120 || len(p2) != 50 || p2[0].Rank != 51 || p2[0].Pubkey != pk64("0032") {
		t.Fatalf("page 2: matched=%d len=%d first=%+v", matched, len(p2), p2[0])
	}
	if last, m := v.page("", 100, 50); m != 120 || len(last) != 20 {
		t.Fatalf("last page: len=%d matched=%d want 20/120", len(last), m)
	}
	if past, m := v.page("", 500, 50); len(past) != 0 || m != 120 {
		t.Fatalf("offset past end: len=%d matched=%d", len(past), m)
	}

	// Case-insensitive name search keeps the global placements.
	hubs, m := v.page("aarhus HUB", 0, 5)
	if m != 12 || len(hubs) != 5 {
		t.Fatalf("hub search: matched=%d len=%d want 12/5", m, len(hubs))
	}
	for _, r := range hubs {
		if want := v.rows[v.pos[r.Pubkey]].Rank; r.Rank != want {
			t.Fatalf("search renumbered %s: rank %d want global %d", r.Name, r.Rank, want)
		}
	}
	if hubs[1].Rank != 11 { // Aarhus Hub 010: index 10 ties index 11 → rank 11
		t.Fatalf("second hub rank=%d want 11", hubs[1].Rank)
	}
	more, m2 := v.page("aarhus hub", 5, 5)
	if m2 != 12 || len(more) != 5 || more[0].Name != "Aarhus Hub 050" {
		t.Fatalf("hub search page 2: matched=%d first=%+v", m2, more)
	}

	// Pubkey search (upper-case input, prefix) resolves to the global rank.
	byPK, m := v.page("0045", 0, 50)
	if m != 1 || byPK[0].Pubkey != pk64("0045") || byPK[0].Rank != 69 {
		t.Fatalf("pubkey search: matched=%d rows=%+v (want rank 69)", m, byPK)
	}
	if none, m := v.page("no such node", 0, 50); len(none) != 0 || m != 0 {
		t.Fatalf("no-match search: len=%d matched=%d", len(none), m)
	}
}

func TestReachRankView_NameFallbackMirrorsReachPage(t *testing.T) {
	both, obsOnly, blank := pk64("b0"), pk64("0b"), pk64("ba")
	snap := snapOf(
		map[string]int{both: 3, obsOnly: 2, blank: 1},
		map[string]rankIdent{
			both:    {hasNode: true, nodeName: "", obsName: "Observer name"}, // node row wins, even empty
			obsOnly: {obsName: "Rooftop observer"},
			blank:   {hasNode: true},
		},
	)
	v := buildReachRankView(snap, &Config{}, 1, 0, 0)
	names := map[string]string{}
	for _, r := range v.rows {
		names[r.Pubkey] = r.Name
	}
	if names[both] != "" || names[obsOnly] != "Rooftop observer" || names[blank] != "" {
		t.Fatalf("names=%v (node row wins; observer-only uses observer name; empty → client pubkey fallback)", names)
	}
	// An unnamed node is still findable by pubkey.
	if rows, _ := v.page(strings.ToUpper(blank[:6]), 0, 10); len(rows) != 1 || rows[0].Pubkey != blank {
		t.Fatalf("unnamed node not searchable by pubkey: %+v", rows)
	}
}

func TestReachRankView_HiddenAndBlacklistedNeverPlaced(t *testing.T) {
	top, bl, obsBl, hidNode, hidObs, a, b := pk64("f0"), pk64("f1"), pk64("f2"), pk64("f3"), pk64("f4"), pk64("a1"), pk64("b2")
	snap := snapOf(
		map[string]int{top: 50, bl: 40, obsBl: 30, hidNode: 20, hidObs: 10, a: 5, b: 5},
		map[string]rankIdent{
			top:     node("🚫 private top"),
			bl:      node("Blacklisted"),
			obsBl:   {obsName: "Blocked observer"},
			hidNode: {hasNode: true, nodeName: "", obsName: "🚫 hidden obs name"},
			hidObs:  {obsName: "🚫 observer"},
			a:       node("A"),
			b:       node("B"),
		},
	)
	cfg := &Config{
		NodeBlacklist:      []string{strings.ToUpper(bl)},
		ObserverBlacklist:  []string{obsBl},
		HiddenNamePrefixes: []string{"🚫"},
	}
	v := buildReachRankView(snap, cfg, 1, 0, 0)
	if got := rowPKs(v.rows); fmt.Sprint(got) != fmt.Sprint([]string{a, b}) {
		t.Fatalf("ranked=%v want only the visible nodes %v", got, []string{a, b})
	}
	if got := rowRanks(v.rows); fmt.Sprint(got) != "[1 1]" {
		t.Fatalf("ranks=%v: hidden nodes above must not leave a gap", got)
	}
	for _, pk := range []string{top, bl, obsBl, hidNode, hidObs} {
		if _, rank, total, ok := v.lookup(pk); ok || rank != 0 || total != 2 {
			t.Fatalf("lookup(%s)=rank %d total %d ranked %v; want unranked, total 2", pk[:4], rank, total, ok)
		}
		if rows, m := v.page(pk[:8], 0, 50); len(rows) != 0 || m != 0 {
			t.Fatalf("hidden pubkey %s leaked via search", pk[:4])
		}
	}
	for _, q := range []string{"private", "blacklisted", "blocked", "hidden", "🚫"} {
		if rows, m := v.page(q, 0, 50); len(rows) != 0 || m != 0 {
			t.Fatalf("hidden name leaked via search %q: %+v", q, rows)
		}
	}
}

// ---- endpoint -----------------------------------------------------------------

func TestHandleReachRank_ShapeAndInputValidation(t *testing.T) {
	a, b, c, d := pk64("a1"), pk64("b2"), pk64("c3"), pk64("d4")
	db := newReachRankDB(t,
		[]rankTestNode{{a, "Alpha <script>alert(1)</script>"}, {b, "Bravo"}, {c, ""}, {d, "Delta"}},
		[]rankTestObserver{{strings.ToUpper(d), "Delta observer"}},
		append(star(a, b, c, d), [2]string{b, c}))
	srv := newReachRankServer(t, db, &Config{})

	resp := getRank(t, srv, "/api/reach-rank")
	if resp.Total != 4 || resp.Matched != 4 || resp.Offset != 0 || resp.Limit != 50 || resp.Query != "" {
		t.Fatalf("meta=%+v", resp)
	}
	if _, err := time.Parse(time.RFC3339, resp.SnapshotAt); err != nil {
		t.Fatalf("snapshot_at %q: %v", resp.SnapshotAt, err)
	}
	if got := fmt.Sprint(rowRanks(resp.Rows)); got != "[1 2 2 4]" {
		t.Fatalf("ranks=%s want [1 2 2 4]", got)
	}
	if resp.Rows[0].Name != "Alpha <script>alert(1)</script>" || resp.Rows[0].Neighbors != 3 {
		t.Fatalf("row 0=%+v (name returned verbatim as data)", resp.Rows[0])
	}
	// The raw body must not carry a literal tag (encoding/json escapes <, >).
	if rr := serveRankRoutes(srv, "/api/reach-rank"); strings.Contains(rr.Body.String(), "<script>") {
		t.Fatalf("raw body carries an unescaped tag: %s", rr.Body.String())
	}

	if r := getRank(t, srv, "/api/reach-rank?limit=500"); r.Limit != reachRankMaxLimit {
		t.Fatalf("limit clamp: %d", r.Limit)
	}
	if r := getRank(t, srv, "/api/reach-rank?limit=abc"); r.Limit != reachRankDefaultLimit {
		t.Fatalf("limit default: %d", r.Limit)
	}
	if r := getRank(t, srv, "/api/reach-rank?limit=1&offset=1"); len(r.Rows) != 1 || r.Rows[0].Rank != 2 {
		t.Fatalf("limit=1&offset=1: %+v", r.Rows)
	}
	if r := getRank(t, srv, "/api/reach-rank?q=%20delta%20"); r.Query != "delta" || r.Matched != 1 || r.Rows[0].Pubkey != d {
		t.Fatalf("trimmed search: %+v", r)
	}
	for _, bad := range []string{
		"/api/reach-rank?offset=-1",
		"/api/reach-rank?offset=x",
		"/api/reach-rank?q=" + strings.Repeat("a", reachRankMaxQueryLen+1),
		"/api/reach-rank?q=%ff",
	} {
		if rr := serveRankRoutes(srv, bad); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d want 400", bad, rr.Code)
		}
	}
	if rr := serveRankRoutes(srv, "/api/reach-rank?q="+url.QueryEscape(strings.Repeat("ø", reachRankMaxQueryLen))); rr.Code != http.StatusOK {
		t.Fatalf("64-rune multibyte query must be accepted, got %d", rr.Code)
	}
}

func TestHandleReachRank_EmptyGraphIsValidEmptyLeaderboard(t *testing.T) {
	db := newReachRankDB(t, []rankTestNode{{pk64("a1"), "A"}}, nil, nil)
	srv := newReachRankServer(t, db, &Config{})
	resp := getRank(t, srv, "/api/reach-rank")
	if resp.Total != 0 || resp.Matched != 0 || resp.Rows == nil || len(resp.Rows) != 0 || resp.SnapshotAt == "" {
		t.Fatalf("empty graph: %+v", resp)
	}
}

// Rank, total and neighbour count agree between the leaderboard and every
// node's Reach page for the same snapshot — including an unranked node.
func TestReachRank_ConsistentWithNodeReach(t *testing.T) {
	a, b, c, d, e, lonely := pk64("a1"), pk64("b2"), pk64("c3"), pk64("d4"), pk64("e5"), pk64("f6")
	edges := append(star(a, b, c, d, e), star(b, c, d)...)
	edges = append(edges, [2]string{"", a}, [2]string{pk64("99"), a}) // unknown endpoints
	db := newReachRankDB(t,
		[]rankTestNode{{a, "A"}, {b, "B"}, {c, "C"}, {d, "D"}, {e, "E"}, {lonely, "Lonely"}},
		nil, edges)
	srv := newReachRankServer(t, db, &Config{})

	lb := getRank(t, srv, "/api/reach-rank")
	if lb.Total != 5 {
		t.Fatalf("total=%d want 5 (unknown endpoints excluded)", lb.Total)
	}
	for _, row := range lb.Rows {
		imp := getReachImportance(t, srv, row.Pubkey)
		if imp.RankStatus != reachRankRanked || imp.DegreeRank != row.Rank || imp.NodesWithEdges != lb.Total ||
			imp.NeighborDegree != row.Neighbors || imp.RankSnapshotAt != lb.SnapshotAt {
			t.Fatalf("%s: reach=%+v leaderboard=%+v total=%d snapshot=%s", row.Name, imp, row, lb.Total, lb.SnapshotAt)
		}
	}
	imp := getReachImportance(t, srv, lonely)
	if imp.RankStatus != reachRankUnranked || imp.DegreeRank != 0 || imp.NodesWithEdges != lb.Total || imp.NeighborDegree != 0 {
		t.Fatalf("unranked node: %+v", imp)
	}
}

// Visibility changes after the view and the Reach cache are warm: the
// leaderboard and a cached Reach body both drop / re-place immediately.
func TestReachRank_VisibilityChangesAfterWarmCache(t *testing.T) {
	a, b, c, d := pk64("a1"), pk64("b2"), pk64("c3"), pk64("d4")
	db := newReachRankDB(t,
		[]rankTestNode{{a, "Alpha"}, {b, "Bravo"}, {c, "Charlie"}, {d, "Delta"}},
		nil, append(star(a, b, c, d), star(b, c, d)...))
	cfg := &Config{}
	srv := newReachRankServer(t, db, cfg)

	if lb := getRank(t, srv, "/api/reach-rank"); lb.Total != 4 || lb.Rows[0].Pubkey != a {
		t.Fatalf("warm: %+v", lb)
	}
	if imp := getReachImportance(t, srv, d); imp.DegreeRank != 3 || imp.NodesWithEdges != 4 {
		t.Fatalf("warm reach(d)=%+v want #3/4", imp)
	}

	// Hide the #1 node by name prefix. The Reach cache is NOT purged by a
	// prefix change, so d's cached body must be re-ranked from the new view.
	cfg.SetHiddenNamePrefixes([]string{"Alph"})
	lb := getRank(t, srv, "/api/reach-rank")
	if lb.Total != 3 || lb.Rows[0].Pubkey != b || lb.Rows[0].Rank != 1 {
		t.Fatalf("after hide: %+v", lb)
	}
	if r := getRank(t, srv, "/api/reach-rank?q=alpha"); r.Matched != 0 {
		t.Fatalf("hidden node still searchable: %+v", r)
	}
	if r := getRank(t, srv, "/api/reach-rank?q="+a[:10]); r.Matched != 0 {
		t.Fatalf("hidden pubkey still searchable: %+v", r)
	}
	if imp := getReachImportance(t, srv, d); imp.DegreeRank != 2 || imp.NodesWithEdges != 3 {
		t.Fatalf("cached reach(d) after hide=%+v want #2/3", imp)
	}
	if rr := serveRankRoutes(srv, "/api/nodes/"+a+"/reach?days=30"); rr.Code != http.StatusNotFound {
		t.Fatalf("hidden node reach page: %d want 404", rr.Code)
	}

	// Blacklist b too.
	cfg.SetNodeBlacklist([]string{b})
	lb = getRank(t, srv, "/api/reach-rank")
	if lb.Total != 2 || fmt.Sprint(rowPKs(lb.Rows)) != fmt.Sprint([]string{c, d}) || fmt.Sprint(rowRanks(lb.Rows)) != "[1 1]" {
		t.Fatalf("after blacklist: %+v", lb)
	}
	if imp := getReachImportance(t, srv, d); imp.DegreeRank != 1 || imp.NodesWithEdges != 2 {
		t.Fatalf("reach(d) after blacklist=%+v want #1/2", imp)
	}

	// Un-hide: a is placed again without waiting for the snapshot TTL.
	cfg.SetHiddenNamePrefixes(nil)
	if lb := getRank(t, srv, "/api/reach-rank"); lb.Total != 3 || lb.Rows[0].Pubkey != a {
		t.Fatalf("after un-hide: %+v", lb)
	}
}

// ---- cache, concurrency, failures ----------------------------------------------

func TestReachRank_SnapshotCacheHitAndExpiry(t *testing.T) {
	a, b, c := pk64("a1"), pk64("b2"), pk64("c3")
	db := newReachRankDB(t, []rankTestNode{{a, "A"}, {b, "B"}, {c, "C"}}, nil, star(a, b))
	srv := newReachRankServer(t, db, &Config{})
	ctx := context.Background()

	v1, err := srv.reachRankView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.Exec(`INSERT INTO neighbor_edges (node_a, node_b) VALUES (?, ?)`, b, c); err != nil {
		t.Fatal(err)
	}
	v2, _ := srv.reachRankView(ctx)
	if v2 != v1 || len(v2.rows) != 2 {
		t.Fatalf("within TTL the cached view must be served (rows=%d)", len(v2.rows))
	}

	// Expire the snapshot: the next request re-reads and re-ranks.
	expireDegreeSnapshot(srv)
	v3, _ := srv.reachRankView(ctx)
	if v3 == v1 || v3.snap == v1.snap || len(v3.rows) != 3 || v3.id <= v1.id {
		t.Fatalf("expired snapshot not rebuilt: rows=%d id %d→%d", len(v3.rows), v1.id, v3.id)
	}
	if _, rank, _, _ := v3.lookup(b); rank != 1 {
		t.Fatalf("b now has 2 neighbours and must be #1, got #%d", rank)
	}
}

func TestReachRank_ConcurrentColdRequestsShareOneLoad(t *testing.T) {
	a, b := pk64("a1"), pk64("b2")
	db := newReachRankDB(t, []rankTestNode{{a, "A"}, {b, "B"}}, nil, star(a, b))
	srv := newReachRankServer(t, db, &Config{})

	var loads atomic.Int32
	release := make(chan struct{})
	prev := onDegreeSnapshotLoad
	onDegreeSnapshotLoad = func() { loads.Add(1); <-release }
	t.Cleanup(func() { onDegreeSnapshotLoad = prev })

	const n = 32
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = serveRankRoutes(srv, "/api/reach-rank").Code
		}(i)
	}
	time.Sleep(100 * time.Millisecond) // let the herd pile onto the in-flight load
	close(release)
	wg.Wait()
	// Timing-independent: a late request finds the fresh snapshot instead.
	if got := loads.Load(); got != 1 {
		t.Fatalf("%d concurrent cold requests ran %d snapshot loads, want 1", n, got)
	}
	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("request %d: status %d", i, c)
		}
	}
}

func TestReachRank_CanceledWaiterDoesNotFailSharedLoad(t *testing.T) {
	a, b := pk64("a1"), pk64("b2")
	db := newReachRankDB(t, []rankTestNode{{a, "A"}, {b, "B"}}, nil, star(a, b))
	srv := newReachRankServer(t, db, &Config{})

	release := make(chan struct{})
	prev := onDegreeSnapshotLoad
	onDegreeSnapshotLoad = func() { <-release }
	t.Cleanup(func() { onDegreeSnapshotLoad = prev })

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := srv.reachRankView(ctx); errc <- err }()
	time.Sleep(50 * time.Millisecond)
	cancel() // the triggering client goes away mid-load
	if err := <-errc; err == nil {
		t.Fatalf("canceled cold caller must return its context error")
	}
	close(release)
	// The load itself ran on a detached context and still publishes.
	deadline := time.Now().Add(2 * time.Second)
	for {
		srv.reach.degreeMu.Lock()
		snap := srv.reach.degreeSnap
		srv.reach.degreeMu.Unlock()
		if snap != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("detached load never published a snapshot")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if lb := getRank(t, srv, "/api/reach-rank"); lb.Total != 2 {
		t.Fatalf("after canceled waiter: %+v", lb)
	}
}

func TestReachRank_ColdDBErrorIsNotAnEmptyLeaderboard(t *testing.T) {
	a, b := pk64("a1"), pk64("b2")
	db := newReachRankDB(t, []rankTestNode{{a, "A"}, {b, "B"}}, nil, star(a, b))
	if _, err := db.conn.Exec(`DROP TABLE neighbor_edges`); err != nil {
		t.Fatal(err)
	}
	srv := newReachRankServer(t, db, &Config{})

	rr := serveRankRoutes(srv, "/api/reach-rank")
	if rr.Code != http.StatusInternalServerError || strings.Contains(rr.Body.String(), `"rows"`) {
		t.Fatalf("cold DB error: status=%d body=%s (want 500, no rows)", rr.Code, rr.Body.String())
	}
	// The Reach report itself still renders; its rank is marked unavailable.
	if imp := getReachImportance(t, srv, a); imp.RankStatus != reachRankUnavailable || imp.DegreeRank != 0 || imp.RankSnapshotAt != "" {
		t.Fatalf("reach rank on DB error: %+v", imp)
	}
	// Recovery: once the table is back the rank appears, including in the
	// Reach body cached while it was unavailable.
	if _, err := db.conn.Exec(`CREATE TABLE neighbor_edges (node_a TEXT NOT NULL, node_b TEXT NOT NULL, count INTEGER DEFAULT 1, last_seen TEXT, PRIMARY KEY (node_a, node_b))`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.Exec(`INSERT INTO neighbor_edges (node_a, node_b) VALUES (?, ?)`, a, b); err != nil {
		t.Fatal(err)
	}
	if lb := getRank(t, srv, "/api/reach-rank"); lb.Total != 2 {
		t.Fatalf("after recovery: %+v", lb)
	}
	if imp := getReachImportance(t, srv, a); imp.RankStatus != reachRankRanked || imp.DegreeRank != 1 {
		t.Fatalf("cached reach after recovery: %+v", imp)
	}
}

func TestReachRank_RefreshErrorServesPreviousSnapshot(t *testing.T) {
	a, b := pk64("a1"), pk64("b2")
	db := newReachRankDB(t, []rankTestNode{{a, "A"}, {b, "B"}}, nil, star(a, b))
	srv := newReachRankServer(t, db, &Config{})
	warm := getRank(t, srv, "/api/reach-rank")

	if _, err := db.conn.Exec(`DROP TABLE neighbor_edges`); err != nil {
		t.Fatal(err)
	}
	expiredAt := expireDegreeSnapshot(srv)

	stale := getRank(t, srv, "/api/reach-rank")
	if stale.Total != warm.Total || stale.Rows[0].Pubkey != warm.Rows[0].Pubkey ||
		stale.SnapshotAt != expiredAt.UTC().Format(time.RFC3339) {
		t.Fatalf("refresh error must keep serving the last complete snapshot with its own (old) timestamp %s: warm=%+v stale=%+v",
			expiredAt.UTC().Format(time.RFC3339), warm, stale)
	}
}

// A read that fails part-way (rows.Err after some rows were delivered) must
// not publish the partial result.
func TestReachRank_PartialReadIsNotPublished(t *testing.T) {
	a, b := pk64("a1"), pk64("b2")
	db := newReachRankDB(t, []rankTestNode{{a, "A"}, {b, "B"}}, nil, star(a, b))
	stmts := []string{
		`ALTER TABLE observers RENAME TO observers_t`,
		`INSERT INTO observers_t (id, name) VALUES ('A0', 'fine'), ('ZZ', 'boom')`,
		// json(id || '{') is per-row, so "malformed JSON" is raised only
		// when the ZZ row is stepped — after A0 was already delivered.
		`CREATE VIEW observers AS SELECT id, CASE WHEN id = 'ZZ' THEN json(id || '{') ELSE name END AS name FROM observers_t ORDER BY id`,
	}
	for _, s := range stmts {
		if _, err := db.conn.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	srv := newReachRankServer(t, db, &Config{})
	if _, err := srv.loadDegreeSnapshot(context.Background()); err == nil || !strings.Contains(err.Error(), "observers") {
		t.Fatalf("partial observers read must fail the load, got %v", err)
	}
	if rr := serveRankRoutes(srv, "/api/reach-rank"); rr.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", rr.Code)
	}
	srv.reach.degreeMu.Lock()
	published := srv.reach.degreeSnap
	srv.reach.degreeMu.Unlock()
	if published != nil {
		t.Fatalf("partial snapshot was published")
	}
}

func TestReachRank_NoDB(t *testing.T) {
	srv := &Server{cfg: &Config{}}
	resetReachState(t, srv)
	if rr := serveRankRoutes(srv, "/api/reach-rank"); rr.Code != http.StatusInternalServerError {
		t.Fatalf("no DB: status=%d want 500", rr.Code)
	}
}
