package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

// #881 M1: the SQL delta, the shared returning-node rule, the read-only DB,
// GET /api/moments, the WebSocket frame, the flag-off path and the wiring.

func momentsTestServer(t *testing.T, on bool) (*Server, *mux.Router) {
	t.Helper()
	srv, router := setupTestServer(t)
	ensureInactiveNodesTable(t, srv)
	if on {
		srv.cfg.Moments = &MomentsConfig{Enabled: true}
	}
	return srv, router
}

func TestMomentsDeltaQueryBothFormats_881(t *testing.T) {
	srv, _ := momentsTestServer(t, true)
	if _, err := srv.db.conn.Exec(`DELETE FROM nodes`); err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	rows := map[string]string{
		"rfc-after":          "2026-11-18T10:00:30Z",
		"rfc-millis-after":   "2026-11-18T10:00:00.500Z",
		"rfc-same-second":    "2026-11-18T10:00:00Z", // re-read: harmless, last_seen unchanged
		"rfc-before":         "2026-11-18T09:59:59Z",
		"rfc-earlier-day":    "2026-11-17T23:00:00Z",
		"sqlite-after":       "2026-11-18 10:00:30",
		"sqlite-before":      "2026-11-18 09:59:59",
		"sqlite-next-day":    "2026-11-19 00:00:01",
		"sqlite-earlier-day": "2026-11-17 23:00:00",
		"null":               "",
	}
	for pk, ls := range rows {
		var v interface{} = ls
		if ls == "" {
			v = nil
		}
		if _, err := srv.db.conn.Exec(`INSERT INTO nodes (public_key, name, role, last_seen, first_seen) VALUES (?, 'n', 'repeater', ?, '2026-01-01T00:00:00Z')`, pk, v); err != nil {
			t.Fatal(err)
		}
	}
	got, err := srv.readMomentNodes(context.Background(), since, false)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, r := range got {
		keys = append(keys, r.PublicKey)
	}
	sort.Strings(keys)
	want := "rfc-after,rfc-millis-after,rfc-same-second,sqlite-after,sqlite-next-day"
	if strings.Join(keys, ",") != want {
		t.Fatalf("delta rows %v, want %s", keys, want)
	}
	all, err := srv.readMomentNodes(context.Background(), since, true)
	if err != nil || len(all) != len(rows) {
		t.Fatalf("full read %d rows (%v), want %d", len(all), err, len(rows))
	}
}

// ingestorNodesSchema mirrors the nodes / inactive_nodes DDL and indexes
// cmd/ingestor/db.go creates, so the plan below is the production plan.
const ingestorNodesSchema = `
	CREATE TABLE nodes (public_key TEXT PRIMARY KEY, name TEXT, role TEXT, lat REAL, lon REAL, last_seen TEXT, first_seen TEXT,
		advert_count INTEGER DEFAULT 0, battery_mv INTEGER, temperature_c REAL, foreign_advert INTEGER DEFAULT 0);
	CREATE INDEX idx_nodes_last_seen ON nodes(last_seen);
	CREATE TABLE inactive_nodes (public_key TEXT PRIMARY KEY, name TEXT, role TEXT, lat REAL, lon REAL, last_seen TEXT, first_seen TEXT,
		advert_count INTEGER DEFAULT 0, battery_mv INTEGER, temperature_c REAL, foreign_advert INTEGER DEFAULT 0);
	CREATE INDEX idx_inactive_nodes_last_seen ON inactive_nodes(last_seen);`

func momentsFileDB(t *testing.T, seed string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "m.db")
	rw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Exec(ingestorNodesSchema + seed); err != nil {
		t.Fatal(err)
	}
	rw.Close()
	return path
}

func TestMomentsDeltaQueryPlanUsesIndex_881(t *testing.T) {
	db, err := OpenDB(momentsFileDB(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	q, args := momentNodesQuery(momentsT0, false, true)
	rows, err := db.conn.Query("EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatal(err)
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
	joined := strings.Join(plan, "\n")
	t.Logf("EXPLAIN QUERY PLAN:\n%s", joined)
	if strings.Count(joined, "USING INDEX idx_nodes_last_seen") != 2 {
		t.Fatalf("both delta branches must range-scan idx_nodes_last_seen:\n%s", joined)
	}
	for _, p := range plan {
		if strings.HasPrefix(p, "SCAN ") {
			t.Fatalf("full scan in the delta plan:\n%s", joined)
		}
	}
	if !strings.Contains(joined, "SEARCH i USING INDEX sqlite_autoindex_inactive_nodes_1 (public_key=?)") {
		t.Fatalf("returning-node lookup is not a primary-key search:\n%s", joined)
	}
}

// The detector reads through the server's own mode=ro handle (#1283).
func TestMomentsReadsThroughReadOnlyDB_881(t *testing.T) {
	path := momentsFileDB(t, `
		INSERT INTO nodes (public_key, role, last_seen, first_seen) VALUES ('aa', 'repeater', '2026-11-18T12:00:00Z', '2026-11-18T12:00:00Z');
		INSERT INTO nodes (public_key, role, last_seen, first_seen) VALUES ('bb', 'repeater', '2026-11-18T12:00:00Z', '2026-11-18T12:00:00Z');
		INSERT INTO inactive_nodes (public_key, last_seen) VALUES ('bb', '2026-11-01T00:00:00Z');
		INSERT INTO nodes (public_key, role, last_seen, first_seen) VALUES ('cc', 'repeater', '2026-11-18T12:00:00Z', '2026-11-18T12:00:00Z');
		INSERT INTO inactive_nodes (public_key, last_seen) VALUES ('cc', NULL);`)
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.conn.Exec(`DELETE FROM nodes`); err == nil {
		t.Fatal("test DB handle is writable; it must be the mode=ro one")
	}
	srv := &Server{db: db, cfg: &Config{}}
	got, err := srv.readMomentNodes(context.Background(), momentsT0.Add(-time.Hour), false)
	if err != nil {
		t.Fatal(err)
	}
	retired := map[string]string{}
	for _, r := range got {
		if r.Retired {
			retired[r.PublicKey] = r.RetiredLastSeen
		}
	}
	if len(got) != 3 || len(retired) != 2 || retired["bb"] != "2026-11-01T00:00:00Z" || retired["cc"] != "" {
		t.Fatalf("rows %d, retired %v", len(got), retired)
	}
}

// One rule decides "returning vs new" for New Nodes and for moments.
func TestInactiveNodeRuleShared_881(t *testing.T) {
	srv, _ := momentsTestServer(t, true)
	now := time.Now().UTC()
	insertNewNodeRow(t, srv, "sharedrule000001", "New", "repeater", nil, nil, now.Format(time.RFC3339))
	insertNewNodeRow(t, srv, "sharedrule000002", "Back", "repeater", nil, nil, now.Format(time.RFC3339))
	if _, err := srv.db.conn.Exec(`INSERT INTO inactive_nodes (public_key, last_seen) VALUES ('sharedrule000002', NULL)`); err != nil {
		t.Fatal(err)
	}
	entries, err := srv.computeNewNodes(500)
	if err != nil {
		t.Fatal(err)
	}
	inNew := map[string]bool{}
	for _, e := range entries {
		inNew[e.PublicKey] = true
	}
	rows, err := srv.readMomentNodes(context.Background(), time.Time{}, true)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, r := range rows {
		if !strings.HasPrefix(r.PublicKey, "sharedrule") {
			continue
		}
		seen++
		if inNew[r.PublicKey] == r.Retired {
			t.Errorf("%s: New Nodes lists it = %v, moments retired = %v; the rules disagree", r.PublicKey, inNew[r.PublicKey], r.Retired)
		}
	}
	if seen != 2 || !inNew["sharedrule000001"] {
		t.Fatalf("seen %d, new nodes %v", seen, inNew)
	}
	if !strings.Contains(inactiveNodeLastSeenSQL("n"), "i.public_key = n.public_key") {
		t.Fatal("helper does not correlate on the alias")
	}
}

func getMoments(t *testing.T, router *mux.Router, query string) MomentsResponse {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/moments"+query, nil))
	if w.Code != 200 {
		t.Fatalf("GET /api/moments%s = %d %s", query, w.Code, w.Body.String())
	}
	if !strings.HasPrefix(w.Body.String(), `{"moments":[`) {
		t.Fatalf("body %s", w.Body.String())
	}
	var resp MomentsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestMomentsAPIHistory_881(t *testing.T) {
	srv, router := momentsTestServer(t, true)
	srv.cfg.Moments.HistorySize = 5
	d := newMomentsDetector(srv.cfg.momentsSettings(), momentsDeps{cfg: srv.cfg})
	srv.moments.Store(d)
	for i := 0; i < 8; i++ {
		d.publish([]Moment{{ID: string(rune('a' + i)), Kind: MomentKindNewNode, Node: &MomentNode{PublicKey: momentPK(i), Name: "N", Role: "repeater"}}})
	}
	ids := func(r MomentsResponse) string {
		var s []string
		for _, m := range r.Moments {
			s = append(s, m.ID)
		}
		return strings.Join(s, "")
	}
	for q, want := range map[string]string{"": "hgfed", "?limit=2": "hg", "?limit=999": "hgfed", "?limit=x": "hgfed", "?limit=0": "hgfed", "?limit=-3": "hgfed"} {
		if got := ids(getMoments(t, router, q)); got != want {
			t.Errorf("%q: %s, want %s", q, got, want)
		}
	}
	// A node blacklisted at runtime drops out of the history on read, and a
	// link with one hidden endpoint goes too.
	d.publish([]Moment{{ID: "L", Kind: MomentKindRecordLink, Link: &MomentLink{A: MomentNode{PublicKey: momentPK(100)}, B: MomentNode{PublicKey: momentPK(6), Name: "N"}}}})
	srv.cfg.SetNodeBlacklist([]string{momentPK(6)})
	if got := ids(getMoments(t, router, "")); got != "hfe" {
		t.Fatalf("after blacklisting node 6 (moment g and the link L): %s, want hfe", got)
	}
	srv.cfg.SetHiddenNamePrefixes([]string{"N"})
	if got := ids(getMoments(t, router, "")); got != "" {
		t.Fatalf("after hiding N*: %s", got)
	}
}

func TestMomentJSONShape_881(t *testing.T) {
	lat, lon, h, km, relays, win := 55.5, 12.25, 30.5, 10.0, 42, 10
	for _, c := range []struct {
		m    Moment
		want string
	}{
		{Moment{ID: "x-1", Kind: MomentKindNewNode, At: "2026-11-18T12:00:00Z", Node: &MomentNode{PublicKey: "aa", Name: "R", Role: "repeater", Lat: &lat, Lon: &lon}, More: 2},
			`{"id":"x-1","kind":"new_node","at":"2026-11-18T12:00:00Z","node":{"publicKey":"aa","name":"R","role":"repeater","lat":55.5,"lon":12.25},"more":2}`},
		{Moment{ID: "x-2", Kind: MomentKindBackOnline, At: "2026-11-18T12:00:00Z", Node: &MomentNode{PublicKey: "aa", Role: "repeater"}, SilentHours: &h},
			`{"id":"x-2","kind":"back_online","at":"2026-11-18T12:00:00Z","node":{"publicKey":"aa","role":"repeater"},"silentHours":30.5}`},
		{Moment{ID: "x-3", Kind: MomentKindRecordLink, At: "2026-11-18T12:00:00Z", Link: &MomentLink{A: MomentNode{PublicKey: "aa", Role: "repeater"}, B: MomentNode{PublicKey: "bb", Role: "room"}, DistanceKm: 61.2}, PreviousKm: &km},
			`{"id":"x-3","kind":"record_link","at":"2026-11-18T12:00:00Z","link":{"a":{"publicKey":"aa","role":"repeater"},"b":{"publicKey":"bb","role":"room"},"distanceKm":61.2},"previousKm":10}`},
		{Moment{ID: "x-4", Kind: MomentKindBusiest, At: "2026-11-18T12:00:00Z", Node: &MomentNode{PublicKey: "aa", Role: "repeater"}, Relays: &relays, WindowMinutes: &win},
			`{"id":"x-4","kind":"busiest_repeater","at":"2026-11-18T12:00:00Z","node":{"publicKey":"aa","role":"repeater"},"relays":42,"windowMinutes":10}`},
	} {
		b, _ := json.Marshal(WSMessage{Type: "moment", Data: c.m})
		if want := `{"type":"moment","data":` + c.want + `}`; string(b) != want {
			t.Errorf("got  %s\nwant %s", b, want)
		}
	}
}

func dialMomentsHub(t *testing.T, hub *Hub) *websocket.Conn {
	t.Helper()
	hs := httptest.NewServer(http.HandlerFunc(hub.ServeWS))
	t.Cleanup(hs.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+hs.URL[4:], nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	deadline := time.Now().Add(5 * time.Second)
	for hub.ClientCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	return conn
}

// readFrames reads text frames until the timeout, skipping heartbeats.
func readFrames(t *testing.T, conn *websocket.Conn, wait time.Duration) []WSMessage {
	t.Helper()
	var out []WSMessage
	deadline := time.Now().Add(wait)
	for {
		conn.SetReadDeadline(deadline)
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return out
		}
		var m WSMessage
		if json.Unmarshal(msg, &m) == nil && m.Type != "heartbeat" {
			out = append(out, m)
		}
	}
}

// fireAllMomentSources drives every source the detector listens to: the
// graph swap, the relay recompute and (when on) a node tick.
func fireAllMomentSources(t *testing.T, srv *Server) {
	t.Helper()
	srv.store.refreshNeighborGraphFromSnapshot()
	recomputeRepeaterEnrichmentSafe(srv.store, 24)
	if d := srv.moments.Load(); d != nil {
		d.tickNodes(context.Background())
	}
}

func TestMomentsWebSocketAndFlag_881(t *testing.T) {
	for _, on := range []bool{false, true} {
		srv, router := momentsTestServer(t, on)
		conn := dialMomentsHub(t, srv.hub)
		stop := srv.startMoments(time.Hour)
		defer stop()
		d := srv.moments.Load()
		if (d != nil) != on {
			t.Fatalf("on=%v: detector %v", on, d)
		}
		if !on {
			if srv.store.momentsGraphHook.Load() != nil || srv.store.momentsRelayHook.Load() != nil || srv.store.relayRecentWindow.Load() != 0 {
				t.Fatal("flag off installed hooks or a relay window")
			}
			fireAllMomentSources(t, srv)
			if f := readFrames(t, conn, 300*time.Millisecond); len(f) != 0 {
				t.Fatalf("flag off: %d frames: %+v", len(f), f)
			}
			if got := getMoments(t, router, ""); len(got.Moments) != 0 {
				t.Fatalf("flag off: API %+v", got)
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("GET", "/api/config/client", nil))
			if strings.Contains(w.Body.String(), `"moments"`) {
				t.Fatal("flag off: client config has a moments block")
			}
			continue
		}
		if srv.store.relayRecentWindow.Load() != int64(10*time.Minute) {
			t.Fatalf("relay window %d", srv.store.relayRecentWindow.Load())
		}
		if again := srv.startMoments(time.Hour); srv.moments.Load() != d {
			t.Fatal("second start replaced the detector")
		} else {
			again()
		}
		if srv.store.momentsGraphHook.Load() == nil {
			t.Fatal("a second start's stop removed the first start's hooks")
		}
		// The hub frame for a published moment, and packet frames unchanged.
		d.publish([]Moment{{ID: "w1", Kind: MomentKindNewNode, At: "2026-11-18T12:00:00Z", Node: &MomentNode{PublicKey: "aa", Role: "room"}}})
		srv.hub.Broadcast(WSMessage{Type: "packet", Data: map[string]string{"hash": "h1"}})
		frames := readFrames(t, conn, 500*time.Millisecond)
		if len(frames) != 2 || frames[0].Type != "moment" || frames[1].Type != "packet" {
			t.Fatalf("frames %+v", frames)
		}
		if data, _ := json.Marshal(frames[0].Data); string(data) != `{"at":"2026-11-18T12:00:00Z","id":"w1","kind":"new_node","node":{"publicKey":"aa","role":"room"}}` {
			t.Fatalf("moment frame data %s", data)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/api/config/client", nil))
		if !strings.Contains(w.Body.String(), `"moments":{"enabled":true,"historySize":50}`) {
			t.Fatalf("client config: %s", w.Body.String())
		}
		stop()
		if srv.store.momentsGraphHook.Load() != nil || srv.store.momentsRelayHook.Load() != nil {
			t.Fatal("stop left hooks installed")
		}
	}
}

// The hooks reach the detector from the real recomputer paths.
func TestMomentsHooksFireFromRecomputers_881(t *testing.T) {
	srv, _ := momentsTestServer(t, true)
	stop := srv.startMoments(time.Hour)
	defer stop()
	var graphs, relays int
	gh := func(*NeighborGraph) { graphs++ }
	rh := func(s *repeaterRelaySnapshot) {
		if s != nil && s.info != nil {
			relays++
		}
	}
	srv.store.momentsGraphHook.Store(&gh)
	srv.store.momentsRelayHook.Store(&rh)
	srv.store.refreshNeighborGraphFromSnapshot()
	recomputeRepeaterEnrichmentSafe(srv.store, 24)
	if graphs != 1 || relays != 1 {
		t.Fatalf("graph hook %d, relay hook %d calls", graphs, relays)
	}
}

func TestMomentsStoreReady_881(t *testing.T) {
	s := &PacketStore{}
	if s.momentsStoreReady() {
		t.Fatal("ready before load")
	}
	s.loaded = true
	if s.momentsStoreReady() {
		t.Fatal("ready while the background load runs")
	}
	s.backgroundLoadDone.Store(true)
	if !s.momentsStoreReady() {
		t.Fatal("not ready after load")
	}
	s2 := &PacketStore{loaded: true}
	s2.backgroundLoadFailed.Store(true)
	if !s2.momentsStoreReady() {
		t.Fatal("a failed background load must not block moments forever")
	}
}

// relayCountRecent: the busy window inside the existing 24 h count.
func TestRelayCountRecent_881(t *testing.T) {
	now := momentsT0
	e := func(age time.Duration, pt int) relayEntry {
		ts := now.Add(-age)
		return relayEntry{ts: ts.Format(time.RFC3339), pt: pt, parsed: true, t: ts, valid: true}
	}
	entries := []relayEntry{e(5*time.Minute, 1), e(9*time.Minute, 2), e(11*time.Minute, 1), e(30*time.Minute, 1), e(time.Minute, payloadTypeAdvert), e(-time.Minute, 1)}
	if got := computeRelayInfoFromEntriesWindowed(entries, 24, now, 10*time.Minute).relayCountRecent; got != 3 {
		t.Fatalf("relayCountRecent = %d, want 3 (5, 9 min and the future-dated row; not the advert)", got)
	}
	base := computeRelayInfoFromEntriesAt(entries, 24, now)
	withWin := computeRelayInfoFromEntriesWindowed(entries, 24, now, 10*time.Minute)
	if base.relayCountRecent != 0 {
		t.Fatal("no window must count nothing")
	}
	a, _ := json.Marshal(base)
	b, _ := json.Marshal(withWin)
	if string(a) != string(b) || strings.Contains(string(b), "ecent\"") {
		t.Fatalf("node JSON changed:\n%s\n%s", a, b)
	}
}

// The bulk snapshot passes the store's busy window through.
func TestRelaySnapshotUsesMomentsWindow_881(t *testing.T) {
	store, _ := heardByBenchStore(t, 50, 300, 5) // transmissions 12 h old
	for _, tx := range store.packets {
		for _, o := range tx.Observations {
			o.wireRoute = RouteFlood + 1
		}
	}
	sum := func() (recent, day int) {
		for _, ri := range store.computeRepeaterRelaySnapshot(24).info {
			recent += ri.relayCountRecent
			day += ri.RelayCount24h
		}
		return
	}
	if r, d := sum(); r != 0 || d == 0 {
		t.Fatalf("window off: recent %d, 24h %d", r, d)
	}
	store.relayRecentWindow.Store(int64(11 * time.Hour))
	if r, _ := sum(); r != 0 {
		t.Fatalf("11 h window over 12 h old relays: recent %d", r)
	}
	store.relayRecentWindow.Store(int64(13 * time.Hour))
	if r, d := sum(); r == 0 || r != d {
		t.Fatalf("13 h window over 12 h old relays: recent %d, 24h %d", r, d)
	}
}

// Zero work on the per-packet path: nothing on the ingest or broadcast path
// refers to moments.
func TestMomentsNotOnPacketPath_881(t *testing.T) {
	for file, funcs := range map[string][]string{
		"store.go":     {"func (s *PacketStore) IngestNewFromDB(", "func (s *PacketStore) IngestNewObservations("},
		"websocket.go": {"func (p *Poller) pollStore(", "func (h *Hub) Broadcast("},
	} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, sig := range funcs {
			i := strings.Index(string(src), sig)
			if i < 0 {
				t.Fatalf("%s: %s not found", file, sig)
			}
			body := string(src[i:])
			if j := strings.Index(body, "\n}\n"); j > 0 {
				body = body[:j]
			}
			if strings.Contains(strings.ToLower(body), "moment") {
				t.Errorf("%s %s references moments", file, sig)
			}
		}
	}
}

// #881 M3: hidden-name prefixes also apply to a node's CURRENT names (a
// rename after the moment fired, or an observer name on the same key), by
// the shared hiddenIdentityNames lookup, and hidden moments do not eat the
// limit.
func TestMomentsAPIHidesLiveNames_881(t *testing.T) {
	srv, router := momentsTestServer(t, true)
	d := newMomentsDetector(srv.cfg.momentsSettings(), momentsDeps{cfg: srv.cfg})
	srv.moments.Store(d)
	pkRenamed, pkObserver, pkClean, pkLinkB := momentPK(1), momentPK(2), momentPK(3), momentPK(4)
	for pk, name := range map[string]string{pkRenamed: "Fine-name", pkClean: "Clean", pkLinkB: "Link-B"} {
		if _, err := srv.db.conn.Exec(`INSERT INTO nodes (public_key, name, role, last_seen, first_seen) VALUES (?, ?, 'repeater', '2026-11-18T10:00:00Z', '2026-11-01T00:00:00Z')`, pk, name); err != nil {
			t.Fatal(err)
		}
	}
	node := func(id, pk string) Moment {
		return Moment{ID: id, Kind: MomentKindNewNode, Node: &MomentNode{PublicKey: pk, Name: "Fine-name", Role: "repeater"}}
	}
	d.publish([]Moment{
		node("a-clean", pkClean),
		node("b-renamed", pkRenamed),
		node("c-observer", pkObserver),
		{ID: "d-link", Kind: MomentKindRecordLink, Link: &MomentLink{A: MomentNode{PublicKey: pkClean, Name: "Clean"}, B: MomentNode{PublicKey: pkLinkB, Name: "Link-B"}}},
		node("e-clean2", pkClean),
	})
	ids := func(r MomentsResponse) string {
		var s []string
		for _, m := range r.Moments {
			s = append(s, m.ID[:1])
		}
		return strings.Join(s, "")
	}
	queries := 0
	srv.db.identityNamesQueryHook = func([]string) { queries++ }

	// No prefix configured: every moment shows, and no names are looked up.
	if got := ids(getMoments(t, router, "")); got != "edcba" || queries != 0 {
		t.Fatalf("no prefix: %q, %d lookups", got, queries)
	}

	srv.cfg.SetHiddenNamePrefixes([]string{"Zz-"})
	if _, err := srv.db.conn.Exec(`UPDATE nodes SET name = 'Zz-renamed' WHERE public_key = ?`, pkRenamed); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES (?, 'Zz-observer', 'CPH')`, pkObserver); err != nil {
		t.Fatal(err)
	}
	if got := ids(getMoments(t, router, "")); got != "eda" {
		t.Fatalf("renamed and observer-named nodes: %q, want eda", got)
	}
	if queries != 1 {
		t.Fatalf("%d name lookups for one request, want 1 bulk query", queries)
	}

	// One end of a link renamed: the link goes.
	if _, err := srv.db.conn.Exec(`UPDATE nodes SET name = 'Zz-linkb' WHERE public_key = ?`, pkLinkB); err != nil {
		t.Fatal(err)
	}
	if got := ids(getMoments(t, router, "")); got != "ea" {
		t.Fatalf("link with a renamed end: %q, want ea", got)
	}

	// Hidden moments are dropped before the limit: of the five (newest
	// first e d c b a), d, c and b are hidden, so limit=2 is e and a, not
	// "the two newest minus the hidden".
	if got := ids(getMoments(t, router, "?limit=2")); got != "ea" {
		t.Fatalf("limit=2: %q, want ea", got)
	}
	if got := ids(getMoments(t, router, "?limit=1")); got != "e" {
		t.Fatalf("limit=1: %q, want e", got)
	}
}

// A failed name lookup fails closed: an error, never the unfiltered list.
func TestMomentsAPILiveNamesFailClosed_881(t *testing.T) {
	srv, router := momentsTestServer(t, true)
	d := newMomentsDetector(srv.cfg.momentsSettings(), momentsDeps{cfg: srv.cfg})
	srv.moments.Store(d)
	d.publish([]Moment{{ID: "x", Kind: MomentKindNewNode, Node: &MomentNode{PublicKey: momentPK(9), Name: "Fine", Role: "repeater"}}})
	srv.cfg.SetHiddenNamePrefixes([]string{"Zz-"})
	if _, err := srv.db.conn.Exec(`DROP TABLE observers`); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/moments", nil))
	if w.Code != 500 || strings.Contains(w.Body.String(), `"x"`) {
		t.Fatalf("lookup failure served %d %s", w.Code, w.Body.String())
	}
}
