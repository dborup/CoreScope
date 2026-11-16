package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/nodecard"
)

// #811 M3 — /p?node= and GET /api/nodes/{pubkey}/card.png (node_share.go).

var (
	nsRepeater  = strings.Repeat("3f2a91c0", 8)
	nsRoom      = strings.Repeat("4b", 32)
	nsSensor    = strings.Repeat("5c", 32)
	nsCompanion = strings.Repeat("6d", 32)
	nsOutside   = strings.Repeat("7e", 32) // positioned outside every area
	nsNoGPS     = strings.Repeat("8f", 32)
	nsUnknown   = strings.Repeat("9a", 32)
	// The fixed clock: every node was heard at nsHeard, 3 h before nsNow.
	nsNow   = time.Date(2026, 11, 15, 13, 20, 0, 0, time.UTC)
	nsHeard = nsNow.Add(-3*time.Hour - 10*time.Minute)
)

type nodeShareEnv struct {
	srv    *Server
	router *mux.Router
	doc    *indexDoc
	c      *nodeCards
	now    time.Time
}

// newNodeShareEnv is a test server with node sharing on or off, edit
// applied to the config before the node cards are built, the production
// route composition (RegisterRoutes, /p, the SPA), and the fixture nodes.
func newNodeShareEnv(t testing.TB, enabled bool, edit func(*Config)) *nodeShareEnv {
	t.Helper()
	srv := newNodeShareServer(t)
	srv.cfg.Share.Nodes.Enabled = enabled
	srv.cfg.Areas = map[string]AreaEntry{"MJ": {Label: "Midtjylland", LatMin: fptr(55.8), LatMax: fptr(56.6), LonMin: fptr(8.1), LonMax: fptr(10.9)}}
	if edit != nil {
		edit(srv.cfg)
	}
	e := &nodeShareEnv{srv: srv, now: nsNow}
	e.build(t)
	for _, n := range []struct {
		pk, name, role string
		lat, lon       any
	}{
		{nsRepeater, "DK_7800_Kjeldbjerg", "repeater", 56.3, 9.4},
		{nsRoom, "Aarhus Room", "room", 56.15, 10.2},
		{nsSensor, "Mark Sensor", "sensor", 56.0, 9.0},
		{nsCompanion, "Phone of Someone", "companion", 56.1, 10.1},
		{nsOutside, "Odense Repeater", "repeater", 55.4, 10.4},
		{nsNoGPS, "Nowhere Repeater", "repeater", nil, nil},
	} {
		e.addNode(t, n.pk, n.name, n.role, n.lat, n.lon)
	}
	return e
}

// newNodeShareServer is setupTestServer's wiring without its seed data,
// for tests and benchmarks alike.
func newNodeShareServer(t testing.TB) *Server {
	t.Helper()
	db := setupTestDB(t)
	cfg := &Config{Port: 3000}
	srv := NewServer(db, cfg, NewHub())
	store := NewPacketStore(db, nil)
	if err := store.Load(); err != nil {
		t.Fatalf("store.Load failed: %v", err)
	}
	if !store.WaitIndexesReady(5 * time.Second) {
		t.Fatalf("background indexes never became ready")
	}
	store.config = cfg
	srv.store = store
	return srv
}

// build (re)builds the node cards and the router from the current config
// and terrain profiler, as NewServer and newHTTPRouter do.
func (e *nodeShareEnv) build(t testing.TB) {
	t.Helper()
	e.srv.initNodeCards()
	e.c = e.srv.nodeCards
	if e.c != nil {
		e.c.now = func() time.Time { return e.now }
	}
	e.doc = newIndexDoc(sharePublicDir, newOGMeta(brandPick(nil), sharePublicDir, nil))
	e.router = mux.NewRouter()
	e.srv.RegisterRoutes(e.router)
	e.srv.registerShareRoutes(e.router, e.doc)
	e.router.PathPrefix("/").Handler(spaHandlerDoc(sharePublicDir, http.FileServer(http.Dir(sharePublicDir)), e.doc))
}

func (e *nodeShareEnv) addNode(t testing.TB, pk, name, role string, lat, lon any) {
	t.Helper()
	if _, err := e.srv.db.conn.Exec(`INSERT OR REPLACE INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1)`, pk, name, role, lat, lon, nsHeard.Format(time.RFC3339), nsHeard.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	e.srv.store.InvalidateNodeCache()
}

func (e *nodeShareEnv) get(target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "http://example.test"+target, nil)
	req.RemoteAddr = "192.0.2.10:1234"
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

func (e *nodeShareEnv) siteDocument(t *testing.T) string {
	t.Helper()
	w := e.get("/")
	if w.Code != 200 {
		t.Fatalf("/ status %d", w.Code)
	}
	return w.Body.String()
}

// setRelay publishes a relay snapshot, as the background recomputer does.
func (e *nodeShareEnv) setRelay(m map[string]RepeaterRelayInfo) {
	e.srv.store.repeaterEnrichMu.Lock()
	e.srv.store.repeaterRelayCache = m
	e.srv.store.repeaterEnrichMu.Unlock()
}

// input is what the card would be drawn from for pk right now.
func (e *nodeShareEnv) input(t *testing.T, pk string) nodecard.NodeCardInput {
	t.Helper()
	ns, ok := e.srv.resolveNodeShare(context.Background(), e.c, pk)
	if !ok || !ns.carded {
		t.Fatalf("%s: found %t carded %t", pk[:8], ok, ns.carded)
	}
	return ns.in
}

// ── Off by default ─────────────────────────────────────────────────────────

func TestNodeShare_OffByDefault(t *testing.T) {
	if (&Config{}).Share.Nodes.Enabled {
		t.Fatal("share.nodes.enabled defaults on")
	}
	var cfg Config
	if err := json.Unmarshal([]byte(`{"share":{"nodes":{"enabled":true}}}`), &cfg); err != nil || !cfg.Share.Nodes.Enabled {
		t.Fatalf("share.nodes.enabled does not parse: %v", err)
	}
	e := newNodeShareEnv(t, false, nil)
	if e.srv.nodeCards != nil {
		t.Fatal("node cards built while off")
	}
	site := e.siteDocument(t)
	w := e.get("/p?node=" + nsRepeater)
	if w.Code != 200 || w.Body.String() != site {
		t.Errorf("off: /p?node= is %d, site document %t", w.Code, w.Body.String() == site)
	}
	if w := e.get("/api/nodes/" + nsRepeater + "/card.png"); w.Code != 404 || w.Header().Get("Content-Type") == "image/png" {
		t.Errorf("off: card is %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	// Off, a /p profile link is untouched: still the profile path's answer.
	if w := e.get(shareQ(shareKeyA, shareKeyB)); w.Code != 200 || w.Body.String() != site {
		t.Errorf("off: /p?profile= is %d", w.Code)
	}
}

// ── The grammar ────────────────────────────────────────────────────────────

func TestNodeShare_ParseTable(t *testing.T) {
	e := newNodeShareEnv(t, true, nil)
	site := e.siteDocument(t)
	for _, c := range []struct {
		name, query string
		code        int
	}{
		{"valid", "node=" + nsRepeater, 200},
		{"uppercase", "node=" + strings.ToUpper(nsRepeater), 200},
		{"mixed case", "node=" + strings.ToUpper(nsRepeater[:10]) + nsRepeater[10:], 200},
		{"v is ignored", "node=" + nsRepeater + "&v=abc123", 200},
		{"tracking keys are ignored", "node=" + nsRepeater + "&fbclid=x1&utm_source=discord&gclid=y", 200},
		{"63 characters", "node=" + nsRepeater[:63], 400},
		{"65 characters", "node=" + nsRepeater + "a", 400},
		{"an 8-character prefix (#772)", "node=" + nsRepeater[:8], 400},
		{"not hex", "node=" + strings.Repeat("g", 64), 400},
		{"empty", "node=", 400},
		{"a duplicated key", "node=" + nsRepeater + "&node=" + nsRepeater, 400},
		{"a duplicated v", "node=" + nsRepeater + "&v=1&v=2", 400},
		{"an unknown key", "node=" + nsRepeater + "&evil=%3Cscript%3E", 400},
		{"with a profile", "node=" + nsRepeater + "&profile=" + shareKeyA + "," + shareKeyB, 400},
		{"a long v", "node=" + nsRepeater + "&v=" + strings.Repeat("a", 65), 400},
		{"an unknown key, well formed", "node=" + nsUnknown, 404},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := e.get("/p?" + c.query)
			if w.Code != c.code {
				t.Fatalf("status %d, want %d", w.Code, c.code)
			}
			body := w.Body.String()
			if c.code != 200 {
				if body != site {
					t.Error("a refused request is not the plain site document")
				}
				return
			}
			if got := mustMeta(t, body, "og:url"); got != "http://example.test/p?node="+nsRepeater {
				t.Errorf("og:url %q", got)
			}
			// Nothing of the request but the parsed key is echoed: every
			// accepted spelling gives the same document.
			if plain := e.get("/p?node=" + nsRepeater).Body.String(); body != plain {
				t.Error("the document differs from the plain key's: something was echoed")
			}
		})
	}
	// The card route takes the same keys, and v only.
	for path, code := range map[string]int{
		"/api/nodes/" + nsRepeater[:63] + "/card.png":                 400,
		"/api/nodes/" + nsRepeater[:8] + "/card.png":                  400,
		"/api/nodes/" + nsRepeater + "/card.png?x=1":                  400,
		"/api/nodes/" + nsRepeater + "/card.png?v=1&v=2":              400,
		"/api/nodes/" + strings.ToUpper(nsRepeater) + "/card.png?v=1": 200,
	} {
		if w := e.get(path); w.Code != code {
			t.Errorf("%s: %d, want %d", path, w.Code, code)
		}
	}
}

// ── Visibility ─────────────────────────────────────────────────────────────

func TestNodeShare_Visibility(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*Config)
		setup func(*testing.T, *nodeShareEnv)
	}{
		{"node blacklist", func(c *Config) { c.SetNodeBlacklist([]string{strings.ToUpper(nsRepeater)}) }, nil},
		{"observer blacklist", func(c *Config) { c.ObserverBlacklist = []string{nsRepeater} }, nil},
		{"hidden name prefix", func(c *Config) { c.SetHiddenNamePrefixes([]string{"DK_7800"}) }, nil},
		{"hidden observer alias", func(c *Config) { c.SetHiddenNamePrefixes([]string{"secret-"}) }, func(t *testing.T, e *nodeShareEnv) {
			if _, err := e.srv.db.conn.Exec(`INSERT INTO observers (id, name, iata, last_seen) VALUES (?, 'secret-obs', 'AAR', datetime('now'))`, nsRepeater); err != nil {
				t.Fatal(err)
			}
		}},
		{"inactive node", nil, func(t *testing.T, e *nodeShareEnv) {
			if _, err := e.srv.db.conn.Exec(`DELETE FROM nodes WHERE public_key = ?`, nsRepeater); err != nil {
				t.Fatal(err)
			}
			if _, err := e.srv.db.conn.Exec(`CREATE TABLE IF NOT EXISTS inactive_nodes (public_key TEXT PRIMARY KEY, name TEXT, role TEXT)`); err != nil {
				t.Fatal(err)
			}
			if _, err := e.srv.db.conn.Exec(`INSERT INTO inactive_nodes (public_key, name, role) VALUES (?, 'DK_7800_Kjeldbjerg', 'repeater')`, nsRepeater); err != nil {
				t.Fatal(err)
			}
			e.srv.store.InvalidateNodeCache()
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newNodeShareEnv(t, true, c.edit)
			if c.setup != nil {
				c.setup(t, e)
			}
			site := e.siteDocument(t)
			w := e.get("/p?node=" + nsRepeater + "&utm_source=x")
			if w.Code != 404 || w.Body.String() != site {
				t.Errorf("/p: %d, site document %t", w.Code, w.Body.String() == site)
			}
			if strings.Contains(w.Body.String(), "Kjeldbjerg") || strings.Contains(w.Body.String(), nsRepeater) {
				t.Error("the hidden node is named or echoed")
			}
			cw := e.get("/api/nodes/" + nsRepeater + "/card.png")
			uw := e.get("/api/nodes/" + nsUnknown + "/card.png")
			if cw.Code != 404 || cw.Body.String() != uw.Body.String() {
				t.Errorf("card: %d %q, unknown %q", cw.Code, cw.Body.String(), uw.Body.String())
			}
			// Another node stays shareable: the rule is per identity.
			if w := e.get("/p?node=" + nsRoom); w.Code != 200 {
				t.Errorf("an unrelated node is %d", w.Code)
			}
		})
	}
}

// The node snapshot is up to 30 s old. A node renamed in the DB out of a
// hidden prefix while the snapshot still holds the hidden name stays
// hidden until the snapshot catches up: either name hides it.
func TestNodeShare_StaleSnapshotNameStillHides(t *testing.T) {
	e := newNodeShareEnv(t, true, func(c *Config) { c.SetHiddenNamePrefixes([]string{"secret-"}) })
	e.addNode(t, nsRepeater, "secret-DK_7800", "repeater", 56.3, 9.4)
	if w := e.get("/p?node=" + nsRepeater); w.Code != 404 {
		t.Fatalf("hidden: %d", w.Code)
	}
	// The snapshot is built while the name is hidden (other pages build
	// it all the time), then the node is renamed in the DB only.
	e.srv.store.getCachedNodesAndPM()
	if _, err := e.srv.db.conn.Exec(`UPDATE nodes SET name = 'DK_7800' WHERE public_key = ?`, nsRepeater); err != nil {
		t.Fatal(err)
	}
	if w := e.get("/p?node=" + nsRepeater); w.Code != 404 || strings.Contains(w.Body.String(), "secret-") {
		t.Errorf("stale hidden name: %d", w.Code)
	}
	if w := e.get("/api/nodes/" + nsRepeater + "/card.png"); w.Code != 404 {
		t.Errorf("stale hidden name, card: %d", w.Code)
	}
	e.srv.store.InvalidateNodeCache()
	if w := e.get("/p?node=" + nsRepeater); w.Code != 200 {
		t.Errorf("after the snapshot caught up: %d", w.Code)
	}
}

// Owner decision Q3: companions get the plain site card and have no card.
func TestNodeShare_CompanionGetsTheSiteCard(t *testing.T) {
	e := newNodeShareEnv(t, true, nil)
	site := e.siteDocument(t)
	w := e.get("/p?node=" + nsCompanion)
	if w.Code != 200 || w.Body.String() != site {
		t.Errorf("companion /p: %d, site document %t", w.Code, w.Body.String() == site)
	}
	if strings.Contains(w.Body.String(), "Phone of Someone") {
		t.Error("a companion is named")
	}
	if w := e.get("/api/nodes/" + nsCompanion + "/card.png"); w.Code != 404 {
		t.Errorf("companion card: %d", w.Code)
	}
	for _, pk := range []string{nsRepeater, nsRoom, nsSensor} {
		if w := e.get("/p?node=" + pk); w.Code != 200 || w.Body.String() == site {
			t.Errorf("%s: %d, plain site %t", pk[:8], w.Code, w.Body.String() == site)
		}
	}
}

// ── The tags ───────────────────────────────────────────────────────────────

func TestNodeShare_Tags(t *testing.T) {
	e := newNodeShareEnv(t, true, nil)
	e.setRelay(map[string]RepeaterRelayInfo{nsRepeater: {RelayCount24h: 1234, RelayCount1h: 56, EstAirtimePct24h: 2.34, EstAirtimePeakPct: 12.5}})
	w := e.get("/p?node=" + nsRepeater)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control %q", cc)
	}
	body := w.Body.String()
	if got := mustMeta(t, body, "og:title"); got != "DK_7800_Kjeldbjerg · Repeater" {
		t.Errorf("og:title %q", got)
	}
	if got := titleOf(t, body); got != "DK_7800_Kjeldbjerg · Repeater" {
		t.Errorf("<title> %q", got)
	}
	desc := mustMeta(t, body, "og:description")
	want := "Midtjylland · Last heard: 3h ago · Relayed 24 h: 1,234 · Relayed 1 h: 56 · Seen airtime 24 h: ≥2.3% · Busiest hour: ≥12.5%"
	if desc != want {
		t.Errorf("og:description\n got %q\nwant %q", desc, want)
	}
	if got := mustMeta(t, body, "robots"); got != "noindex" {
		t.Errorf("robots %q", got)
	}
	in := e.input(t, nsRepeater)
	wantImg := "http://example.test/api/nodes/" + nsRepeater + "/card.png?v=" + e.c.version(in)
	if got := mustMeta(t, body, "og:image"); got != wantImg {
		t.Errorf("og:image %q, want %q", got, wantImg)
	}
	if got := mustMeta(t, body, "twitter:image"); got != wantImg {
		t.Errorf("twitter:image %q", got)
	}
	if mustMeta(t, body, "og:image:width") != "1200" || mustMeta(t, body, "og:image:height") != "630" {
		t.Error("card size not stated")
	}
	alt := mustMeta(t, body, "og:image:alt")
	if !strings.HasPrefix(alt, "Node card: DK_7800_Kjeldbjerg · Repeater. Midtjylland. Last heard: 3h ago") {
		t.Errorf("og:image:alt %q", alt)
	}
	// No coordinate reaches a tag.
	for _, k := range []string{"og:title", "og:description", "og:image:alt", "og:url", "og:image"} {
		if v := mustMeta(t, body, k); strings.Contains(v, "56.3") || strings.Contains(v, "9.4") {
			t.Errorf("%s carries a coordinate: %q", k, v)
		}
	}
	// The card itself is the 1200×630 PNG of that v.
	cw := e.get(strings.TrimPrefix(wantImg, "http://example.test"))
	if cw.Code != 200 || cw.Header().Get("Content-Type") != "image/png" || cw.Header().Get("Cache-Control") != nodeCardCacheControl {
		t.Fatalf("card: %d %s %s", cw.Code, cw.Header().Get("Content-Type"), cw.Header().Get("Cache-Control"))
	}
	img, err := png.Decode(bytes.NewReader(cw.Body.Bytes()))
	if err != nil || img.Bounds().Dx() != 1200 || img.Bounds().Dy() != 630 {
		t.Fatalf("card decodes %v, %v", err, img.Bounds())
	}
}

// Owner decision Q2: the names switch of map.dhm.profile.card applies to
// the node card too.
func TestNodeShare_NamesOff(t *testing.T) {
	no := false
	e := newNodeShareEnv(t, true, func(c *Config) { c.DHM.Profile.Card.Names = &no })
	body := e.get("/p?node=" + nsRepeater).Body.String()
	if got := mustMeta(t, body, "og:title"); got != "Node 3f2a91c0 · Repeater" {
		t.Errorf("og:title %q", got)
	}
	if strings.Contains(body, "Kjeldbjerg") {
		t.Error("named with names off")
	}
	var got nodecard.Options
	e.c.render = func(_ nodecard.NodeCardInput, o nodecard.Options) ([]byte, error) { got = o; return []byte("png"), nil }
	e.get("/api/nodes/" + nsRepeater + "/card.png")
	if !got.HideNames || got.Palette == nil {
		t.Errorf("card options %+v", got)
	}
}

// Owner decision Q5: the area is shown when known and dropped when not:
// outside every area, and without a position.
func TestNodeShare_AreaPresentAndAbsent(t *testing.T) {
	e := newNodeShareEnv(t, true, nil)
	if a := e.input(t, nsRepeater).Area; a != "Midtjylland" {
		t.Errorf("inside: area %q", a)
	}
	for _, pk := range []string{nsOutside, nsNoGPS} {
		in := e.input(t, pk)
		if in.Area != "" {
			t.Errorf("%s: area %q", pk[:8], in.Area)
		}
		desc := mustMeta(t, e.get("/p?node="+pk).Body.String(), "og:description")
		if !strings.HasPrefix(desc, "Last heard: ") || strings.Contains(desc, "Midtjylland") {
			t.Errorf("%s: og:description %q", pk[:8], desc)
		}
	}
	// No areas configured at all: no area, never a guess.
	e2 := newNodeShareEnv(t, true, func(c *Config) { c.Areas = nil })
	if a := e2.input(t, nsRepeater).Area; a != "" {
		t.Errorf("no areas: area %q", a)
	}
	// The card is drawn with the area it shows in the tags.
	var drawn string
	e.c.render = func(in nodecard.NodeCardInput, _ nodecard.Options) ([]byte, error) {
		drawn = in.Area
		return []byte("png"), nil
	}
	e.get("/api/nodes/" + nsRepeater + "/card.png")
	if drawn != "Midtjylland" {
		t.Errorf("card drawn with area %q", drawn)
	}
}

// A trace airtime is "<0.1%" on the card; the tags drop angle brackets, so
// the text says "under 0.1%" rather than an overstated "0.1%".
func TestNodeShare_TraceAirtimeIsNotOverstated(t *testing.T) {
	e := newNodeShareEnv(t, true, nil)
	e.setRelay(map[string]RepeaterRelayInfo{nsRepeater: {RelayCount24h: 7, RelayCount1h: 2, EstAirtimePct24h: 0.05, EstAirtimePeakPct: 0.12}})
	body := e.get("/p?node=" + nsRepeater).Body.String()
	desc := mustMeta(t, body, "og:description")
	if !strings.Contains(desc, "Seen airtime 24 h: under 0.1% · Busiest hour: ≥0.1%") {
		t.Errorf("og:description %q", desc)
	}
	if alt := mustMeta(t, body, "og:image:alt"); !strings.Contains(alt, "Seen airtime 24 h: under 0.1%.") {
		t.Errorf("og:image:alt %q", alt)
	}
}

func TestNodeShare_XSSNamesAreInert(t *testing.T) {
	for _, name := range []string{`" onerror="alert(1)" x="`, `</title><script>alert(1)</script>`, "a‮b<img src=x onerror=alert(1)>"} {
		e := newNodeShareEnv(t, true, nil)
		e.addNode(t, nsRepeater, name, "repeater", 56.3, 9.4)
		body := e.get("/p?node=" + nsRepeater).Body.String()
		for _, bad := range []string{"<script>alert", `" onerror="alert`, "<img", "‮"} {
			if strings.Contains(body, bad) {
				t.Errorf("%q: %q in the document", name, bad)
			}
		}
		for _, k := range []string{"og:title", "twitter:title", "og:description", "og:image:alt", "twitter:image:alt", "og:url", "og:image"} {
			if _, ok := metaContent(body, k); !ok {
				t.Errorf("%q: %s is not one well-formed tag", name, k)
			}
		}
		titleOf(t, body)
	}
}

// ── v ──────────────────────────────────────────────────────────────────────

func TestNodeShare_VersionFollowsTheDisplayedValues(t *testing.T) {
	e := newNodeShareEnv(t, true, nil)
	e.setRelay(map[string]RepeaterRelayInfo{nsRepeater: {RelayCount24h: 10, RelayCount1h: 1}})
	base := e.c.version(e.input(t, nsRepeater))
	if again := e.c.version(e.input(t, nsRepeater)); again != base {
		t.Fatal("v is not stable for unchanged input")
	}
	// Last heard is bucketed by its wording: 3h10m and 3h50m both say
	// "3h ago", though the absolute times on the card differ.
	e.now = nsNow.Add(40 * time.Minute)
	if v := e.c.version(e.input(t, nsRepeater)); v != base {
		t.Error("v changed within one wording bucket")
	}
	e.now = nsNow
	// So is a node heard again within the bucket: last heard 3h40m ago
	// instead of 3h10m is still "3h ago".
	if _, err := e.srv.db.conn.Exec(`UPDATE nodes SET last_seen = ? WHERE public_key = ?`, nsNow.Add(-3*time.Hour-40*time.Minute).Format(time.RFC3339), nsRepeater); err != nil {
		t.Fatal(err)
	}
	e.srv.store.InvalidateNodeCache()
	if in := e.input(t, nsRepeater); !in.LastHeard.Equal(nsNow.Add(-3*time.Hour - 40*time.Minute)) {
		t.Fatalf("last heard %v", in.LastHeard)
	} else if v := e.c.version(in); v != base {
		t.Error("v changed with the absolute last-heard time inside one wording bucket")
	}
	e.now = nsNow.Add(25 * time.Minute) // 4h05m: "4h ago"
	if v := e.c.version(e.input(t, nsRepeater)); v == base {
		t.Error("v did not change with the wording")
	}
	e.now = nsNow
	changes := map[string]func(){
		"relay count": func() {
			e.setRelay(map[string]RepeaterRelayInfo{nsRepeater: {RelayCount24h: 11, RelayCount1h: 1}})
		},
		"neighbours": func() {
			g := NewNeighborGraph()
			g.upsertEdge(nsRepeater, nsRoom, "4b", "obs1", nil, nsNow)
			e.srv.neighborGraph.Store(g)
		},
		"name": func() { e.addNode(t, nsRepeater, "DK_7800_Renamed", "repeater", 56.3, 9.4) },
		"area": func() { e.addNode(t, nsRepeater, "DK_7800_Renamed", "repeater", 55.4, 10.4) },
	}
	prev := base
	for _, what := range []string{"relay count", "neighbours", "name", "area"} {
		changes[what]()
		v := e.c.version(e.input(t, nsRepeater))
		if v == prev {
			t.Errorf("v unchanged after the %s changed", what)
		}
		prev = v
	}
	// The page and the card agree on v.
	body := e.get("/p?node=" + nsRepeater).Body.String()
	if !strings.HasSuffix(mustMeta(t, body, "og:image"), "?v="+prev) {
		t.Errorf("og:image %q, v %s", mustMeta(t, body, "og:image"), prev)
	}
}

// ── The cache and the shared budget ────────────────────────────────────────

func TestNodeShare_CacheHitAndChangedValueMisses(t *testing.T) {
	e := newNodeShareEnv(t, true, nil)
	var renders atomic.Int32
	e.c.render = func(in nodecard.NodeCardInput, _ nodecard.Options) ([]byte, error) {
		renders.Add(1)
		return []byte("png:" + in.Name), nil
	}
	path := "/api/nodes/" + nsRepeater + "/card.png"
	if w := e.get(path); w.Header().Get("X-CoreScope-Card") != "miss" {
		t.Fatalf("first: %s", w.Header().Get("X-CoreScope-Card"))
	}
	if w := e.get(path + "?v=whatever"); w.Header().Get("X-CoreScope-Card") != "hit" || renders.Load() != 1 {
		t.Fatalf("second: %s, %d renders", w.Header().Get("X-CoreScope-Card"), renders.Load())
	}
	e.addNode(t, nsRepeater, "DK_7800_Renamed", "repeater", 56.3, 9.4)
	w := e.get(path)
	if w.Header().Get("X-CoreScope-Card") != "miss" || w.Body.String() != "png:DK_7800_Renamed" {
		t.Errorf("after a rename: %s %q", w.Header().Get("X-CoreScope-Card"), w.Body.String())
	}
	// A visibility change reaches a cached card too.
	e.srv.cfg.SetNodeBlacklist([]string{nsRepeater})
	if w := e.get(path); w.Code != 404 {
		t.Errorf("blacklisted after caching: %d", w.Code)
	}
}

func TestNodeShare_CacheBound(t *testing.T) {
	e := newNodeShareEnv(t, true, func(c *Config) { zero := 0; c.DHM.Profile.RateLimitPerMin = &zero })
	e.c.render = func(nodecard.NodeCardInput, nodecard.Options) ([]byte, error) { return make([]byte, 1000), nil }
	for i := 0; i < 3*nodeCardCacheEntries; i++ {
		pk := fmt.Sprintf("%064x", i+1)
		e.addNode(t, pk, fmt.Sprintf("R%d", i), "repeater", 56.3, 9.4)
	}
	for i := 0; i < 3*nodeCardCacheEntries; i++ {
		if w := e.get("/api/nodes/" + fmt.Sprintf("%064x", i+1) + "/card.png"); w.Code != 200 {
			t.Fatalf("card %d: %d", i, w.Code)
		}
	}
	if n := e.c.cache.Len(); n > nodeCardCacheEntries || n == 0 {
		t.Errorf("cache holds %d cards, bound %d", n, nodeCardCacheEntries)
	}
	if nodeCardCacheTTL != 10*time.Minute || nodeCardCacheControl != "public, max-age=600" {
		t.Error("cache TTL and Cache-Control disagree with the plan's 10 minutes")
	}
}

// With the terrain card on, both card types draw from one per-IP budget
// and one set of render slots.
func TestNodeShare_SharesLimiterAndRenderSlotsWithTheProfileCard(t *testing.T) {
	one := 1
	te := newShareEnvWith(t, dhm.ProfileConfig{Share: dhm.ShareConfig{Enabled: true}, RateLimitPerMin: &one}, nil)
	te.srv.cfg.Share.Nodes.Enabled = true
	te.srv.initNodeCards()
	c := te.srv.nodeCards
	if c == nil || c.limiter != te.srv.terrain.limiter || c.admit != te.srv.terrain.card.admit {
		t.Fatal("node cards do not share the terrain card's limiter and render slots")
	}
	// Without the terrain card it has its own pair, from the same settings.
	e := newNodeShareEnv(t, true, func(c *Config) { c.DHM.Profile.RateLimitPerMin = &one })
	if e.c.limiter == nil || e.c.admit == nil || e.c.admit.Cap() != cardRenderSlots() {
		t.Fatal("no limiter or render slots without the terrain card")
	}
	e.c.render = func(nodecard.NodeCardInput, nodecard.Options) ([]byte, error) { return []byte("png"), nil }
	if w := e.get("/api/nodes/" + nsRepeater + "/card.png"); w.Code != 200 {
		t.Fatalf("first card %d", w.Code)
	}
	// A cached card is free; the next uncached one is over the budget.
	if w := e.get("/api/nodes/" + nsRepeater + "/card.png"); w.Code != 200 {
		t.Fatalf("cached card %d", w.Code)
	}
	if w := e.get("/api/nodes/" + nsRoom + "/card.png"); w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("second uncached card %d", w.Code)
	}
}

func TestNodeShare_RenderSlotsFullIs503AndRefunded(t *testing.T) {
	one := 1
	e := newNodeShareEnv(t, true, func(c *Config) { c.DHM.Profile.RateLimitPerMin = &one })
	e.c.admit = dhm.NewAdmission(1)
	release := make(chan struct{})
	started := make(chan struct{})
	e.c.render = func(nodecard.NodeCardInput, nodecard.Options) ([]byte, error) {
		close(started)
		<-release
		return []byte("png"), nil
	}
	done := make(chan int)
	go func() {
		req := httptest.NewRequest("GET", "/api/nodes/"+nsRoom+"/card.png", nil)
		req.RemoteAddr = "192.0.2.99:1"
		w := httptest.NewRecorder()
		e.router.ServeHTTP(w, req)
		done <- w.Code
	}()
	<-started
	w := e.get("/api/nodes/" + nsRepeater + "/card.png")
	if w.Code != 503 || w.Header().Get("Retry-After") == "" {
		t.Errorf("slots full: %d", w.Code)
	}
	close(release)
	if code := <-done; code != 200 {
		t.Errorf("the running render: %d", code)
	}
	// Refunded: this client's single token is still there.
	e.c.render = func(nodecard.NodeCardInput, nodecard.Options) ([]byte, error) { return []byte("png"), nil }
	if w := e.get("/api/nodes/" + nsRepeater + "/card.png"); w.Code != 200 {
		t.Errorf("after a refused render: %d, the charge was not refunded", w.Code)
	}
}

// ── The data path ──────────────────────────────────────────────────────────

// The neighbour count is the number of rows the node page lists.
func TestNodeShare_NeighbourCountMatchesTheNodePage(t *testing.T) {
	e := newNodeShareEnv(t, true, nil)
	g := NewNeighborGraph()
	g.upsertEdge(nsRepeater, nsRoom, "4b", "obs1", nil, nsNow)
	g.upsertEdge(nsRepeater, nsSensor, "5c", "obs1", nil, nsNow)
	g.upsertEdge(nsRepeater, "", "4b", "obs2", nil, nsNow) // unresolved, deduplicated into nsRoom
	g.upsertEdge(nsRepeater, "", "ee", "obs2", nil, nsNow) // unresolved, kept
	e.srv.neighborGraph.Store(g)
	w := e.get("/api/nodes/" + nsRepeater + "/neighbors")
	var resp NeighborResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	in := e.input(t, nsRepeater)
	if in.Neighbors == nil || *in.Neighbors != len(resp.Neighbors) || len(resp.Neighbors) < 2 {
		t.Errorf("card %v, node page %d", nilOr(in.Neighbors), len(resp.Neighbors))
	}
	t.Logf("%d neighbour rows from %d edges", len(resp.Neighbors), len(g.Neighbors(nsRepeater)))
}

func nilOr(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// No snapshot, no clause: a card never builds the relay snapshot or the
// neighbour graph inline.
func TestNodeShare_NoInlineSnapshotBuild(t *testing.T) {
	e := newNodeShareEnv(t, true, nil)
	e.setRelay(nil)
	e.srv.neighborGraph.Store(nil)
	in := e.input(t, nsRepeater)
	if in.Relay != nil || in.Neighbors != nil {
		t.Errorf("relay %v, neighbours %v without snapshots", in.Relay, in.Neighbors)
	}
	e.srv.store.repeaterEnrichMu.Lock()
	built := e.srv.store.repeaterRelayCache != nil
	e.srv.store.repeaterEnrichMu.Unlock()
	if built || e.srv.neighborGraph.Load() != nil {
		t.Error("a snapshot was built inline")
	}
	desc := mustMeta(t, e.get("/p?node="+nsRepeater).Body.String(), "og:description")
	if desc != "Midtjylland · Last heard: 3h ago" {
		t.Errorf("og:description %q", desc)
	}
}

// The key index is built once per node snapshot, not per request.
func TestNodeShare_IndexRebuiltOncePerSnapshot(t *testing.T) {
	nodes := []nodeInfo{{PublicKey: strings.ToUpper(nsRepeater), Role: "repeater"}, {PublicKey: nsRoom, Role: "room"}}
	var x nodeKeyIndex
	if _, ok := x.find(nodes, nsRepeater); !ok {
		t.Fatal("upper-case key not found")
	}
	m := x.m
	for i := 0; i < 10; i++ {
		if n, ok := x.find(nodes, nsRoom); !ok || n.Role != "room" {
			t.Fatal("room not found")
		}
	}
	if fmt.Sprintf("%p", x.m) != fmt.Sprintf("%p", m) {
		t.Error("index rebuilt for the same snapshot")
	}
	next := append([]nodeInfo(nil), nodes[:1]...)
	if _, ok := x.find(next, nsRoom); ok {
		t.Error("a stale index answered for a new snapshot")
	}
	if _, ok := x.find(nil, nsRoom); ok {
		t.Error("found in an empty snapshot")
	}
}

// ── Perf proof ─────────────────────────────────────────────────────────────

// benchNodeShareEnv is a server with n nodes and the shipped hidden-name
// prefix, so the visibility lookup runs as it does in production.
func benchNodeShareEnv(b *testing.B, n int) *nodeShareEnv {
	b.Helper()
	e := newNodeShareEnv(b, true, func(c *Config) {
		zero := 0
		c.DHM.Profile.RateLimitPerMin = &zero
		c.SetHiddenNamePrefixes([]string{"🚫"})
	})
	tx, _ := e.srv.db.conn.Begin()
	relay := map[string]RepeaterRelayInfo{}
	for i := 0; i < n; i++ {
		pk := fmt.Sprintf("%064x", i+1)
		tx.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen) VALUES (?, ?, 'repeater', 56.2, 10.1, ?)`,
			pk, fmt.Sprintf("Node %d", i), nsHeard.Format(time.RFC3339))
		relay[pk] = RepeaterRelayInfo{RelayCount24h: i, RelayCount1h: i / 24, EstAirtimePct24h: 1.5}
	}
	tx.Commit()
	e.srv.store.InvalidateNodeCache()
	relay[nsRepeater] = RepeaterRelayInfo{RelayCount24h: 1234, RelayCount1h: 56, EstAirtimePct24h: 2.3, EstAirtimePeakPct: 8.1}
	e.setRelay(relay)
	g := NewNeighborGraph()
	g.upsertEdge(nsRepeater, nsRoom, "4b", "obs1", nil, nsNow)
	e.srv.neighborGraph.Store(g)
	// Warm the node snapshot and its key index, as any earlier request
	// in the same 30 s would have.
	e.get("/p?node=" + nsRepeater)
	return e
}

var benchNodeCounts = []int{2000, 20000}

// BenchmarkNodeCard_Cold is one uncached card: lookup, visibility, v and
// the render.
func BenchmarkNodeCard_Cold(b *testing.B) {
	for _, n := range benchNodeCounts {
		b.Run(fmt.Sprintf("nodes=%d", n), func(b *testing.B) {
			e := benchNodeShareEnv(b, n)
			path := "/api/nodes/" + nsRepeater + "/card.png"
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				e.c.cache = dhm.NewCache[nodeCardKey](nodeCardCacheEntries, nodeCardCacheEntries*cardBytesPerEntry, nodeCardCacheTTL)
				if w := e.get(path); w.Code != 200 || w.Header().Get("X-CoreScope-Card") != "miss" {
					b.Fatalf("%d %s", w.Code, w.Header().Get("X-CoreScope-Card"))
				}
			}
		})
	}
}

// BenchmarkNodeCard_Cached is a cached card: lookup, visibility and v only.
func BenchmarkNodeCard_Cached(b *testing.B) {
	for _, n := range benchNodeCounts {
		b.Run(fmt.Sprintf("nodes=%d", n), func(b *testing.B) {
			e := benchNodeShareEnv(b, n)
			path := "/api/nodes/" + nsRepeater + "/card.png"
			e.get(path)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if w := e.get(path); w.Header().Get("X-CoreScope-Card") != "hit" {
					b.Fatal(w.Header().Get("X-CoreScope-Card"))
				}
			}
		})
	}
}

// BenchmarkNodeShare_Page is /p?node=: the preview document.
func BenchmarkNodeShare_Page(b *testing.B) {
	for _, n := range benchNodeCounts {
		b.Run(fmt.Sprintf("nodes=%d", n), func(b *testing.B) {
			e := benchNodeShareEnv(b, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if w := e.get("/p?node=" + nsRepeater); w.Code != 200 {
					b.Fatal(w.Code)
				}
			}
		})
	}
}
