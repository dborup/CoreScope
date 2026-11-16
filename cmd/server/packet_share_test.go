package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/packetcard"
)

// #812 M2 — /p?packet= and GET /api/packets/{hash}/card.png (packet_share.go).

var (
	psR1 = strings.Repeat("a1", 32) // "Relay One"
	psR2 = strings.Repeat("a2", 32) // "Relay Two"
	psRH = strings.Repeat("a3", 32) // "Secret Relay": hidden in some cases
	psO1 = strings.Repeat("b1", 32) // "Obs Aarhus": the first observer
	psO2 = strings.Repeat("b2", 32) // "Obs Viborg"
	psO3 = strings.Repeat("b3", 32) // "Obs Silkeborg"

	// A GRP_TXT flood with decrypted text, a channel and a sender: O1 at
	// 0 hops first, O2 via R1, R2 and O3 via R1.
	psFlood = "f100000000000001"
	// A direct TXT_MSG with text and a sender, heard by O1 via R1.
	psDirect = "d200000000000002"
	// An ADVERT from Relay Two, and one from the Secret Relay.
	psAdvert       = "a300000000000003"
	psHiddenAdvert = "a400000000000004"
	// A flood whose first hearer, O1, also heard it later via the Secret
	// Relay (its deepest branch), and O2 via R1 (#852 review, finding 5).
	psFirstUncounted = "f500000000000005"
	psUnknown        = "0123456789abcdef"

	// The words of the payloads above; none may reach a preview.
	psSecrets = []string{"secret place", "privatechan", "Alice", "Bobs house", "Carol", "meet at"}

	// The fixed clock: the seeded packets were first heard 30 min before.
	psNow = time.Date(2026, 11, 15, 13, 20, 0, 0, time.UTC)
	psT0  = psNow.Add(-30 * time.Minute)
)

type packetShareEnv struct {
	srv    *Server
	router *mux.Router
	doc    *indexDoc
	c      *packetCards
	now    time.Time
}

// newPacketShareEnv is a test server with packet sharing on or off, edit
// applied to the config before the cards are built, the production route
// composition (RegisterRoutes, /p, the SPA), and the fixture packets in
// the store and the DB.
func newPacketShareEnv(t testing.TB, enabled bool, edit func(*Config)) *packetShareEnv {
	t.Helper()
	db := setupTestDB(t)
	seedPacketShare(t, db)
	cfg := &Config{Port: 3000}
	cfg.Share.Packets.Enabled = enabled
	cfg.Areas = map[string]AreaEntry{"MJ": {Label: "Midtjylland", LatMin: fptr(55.8), LatMax: fptr(56.6), LonMin: fptr(8.1), LonMax: fptr(10.9)}}
	if edit != nil {
		edit(cfg)
	}
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
	e := &packetShareEnv{srv: srv, now: psNow}
	e.build(t)
	return e
}

// build (re)builds the cards and the router, as NewServer and
// newHTTPRouter do.
func (e *packetShareEnv) build(t testing.TB) {
	t.Helper()
	e.srv.initNodeCards()
	e.srv.initPacketCards()
	e.c = e.srv.packetCards
	if e.c != nil {
		e.c.now = func() time.Time { return e.now }
	}
	e.doc = newIndexDoc(sharePublicDir, newOGMeta(brandPick(nil), sharePublicDir, nil))
	e.router = mux.NewRouter()
	e.srv.RegisterRoutes(e.router)
	e.srv.registerShareRoutes(e.router, e.doc)
	e.router.PathPrefix("/").Handler(spaHandlerDoc(sharePublicDir, http.FileServer(http.Dir(sharePublicDir)), e.doc))
}

func seedPacketShare(t testing.TB, db *DB) {
	t.Helper()
	for _, n := range []struct {
		pk, name, role string
		lat, lon       float64
	}{
		{psR1, "Relay One", "repeater", 56.20, 9.50},
		{psR2, "Relay Two", "repeater", 56.30, 9.80},
		{psRH, "Secret Relay", "repeater", 56.25, 9.65},
		{psO1, "Obs Aarhus", "repeater", 56.15, 10.20},
		{psO2, "Obs Viborg", "repeater", 56.45, 9.40},
		{psO3, "Obs Silkeborg", "repeater", 56.17, 9.55},
	} {
		if _, err := db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count) VALUES (?, ?, ?, ?, ?, ?, ?, 1)`,
			n.pk, n.name, n.role, n.lat, n.lon, psT0.Format(time.RFC3339), psT0.Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	for _, o := range []struct{ pk, name, iata string }{
		{psO1, "Obs Aarhus", "AAR"}, {psO2, "Obs Viborg", "VIB"}, {psO3, "Obs Silkeborg", "SIL"},
	} {
		if _, err := db.conn.Exec(`INSERT INTO observers (id, name, iata, last_seen) VALUES (?, ?, ?, ?)`, o.pk, o.name, o.iata, psT0.Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	type obs struct {
		observer string
		path     []string // relay keys in path order
		after    int      // seconds after psT0
	}
	add := func(hash string, pt, rt int, decoded string, observations ...obs) {
		seedSharePacket(t, db, hash, pt, rt, decoded, psT0)
		for _, o := range observations {
			seedShareObservation(t, db, hash, o.observer, o.path, psT0.Add(time.Duration(o.after)*time.Second))
		}
	}
	add(psFlood, 5, 1, `{"type":"CHAN","channel":"#privatechan","text":"Alice: meet at the secret place","sender":"Alice"}`,
		obs{psO1, nil, 0}, obs{psO2, []string{psR1, psR2}, 2}, obs{psO3, []string{psR1}, 4})
	add(psDirect, 2, 2, `{"type":"TXT_MSG","text":"see you at Bobs house","sender":"Carol","destHash":"b1","srcHash":"c4"}`,
		obs{psO1, []string{psR1}, 0})
	add(psAdvert, 4, 1, `{"type":"ADVERT","pubKey":"`+psR2+`","name":"Relay Two"}`,
		obs{psO1, nil, 0}, obs{psO2, []string{psR1}, 1})
	add(psHiddenAdvert, 4, 1, `{"type":"ADVERT","pubKey":"`+psRH+`","name":"Secret Relay"}`,
		obs{psO1, nil, 0})
	add(psFirstUncounted, 5, 1, `{"type":"CHAN","channel":"#privatechan","text":"Alice: meet at the secret place","sender":"Alice"}`,
		obs{psO1, nil, 0}, obs{psO2, []string{psR1}, 3}, obs{psO1, []string{psRH, psR1}, 5})
}

func seedSharePacket(t testing.TB, db *DB, hash string, pt, rt int, decoded string, first time.Time) {
	t.Helper()
	if _, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json) VALUES ('AABB', ?, ?, ?, ?, ?)`,
		hash, first.Format(time.RFC3339), rt, pt, decoded); err != nil {
		t.Fatal(err)
	}
}

func seedShareObservation(t testing.TB, db *DB, hash, observer string, relays []string, at time.Time) {
	t.Helper()
	hops := make([]string, len(relays))
	for i, r := range relays {
		hops[i] = r[:2]
	}
	pathJSON, _ := json.Marshal(hops)
	if relays == nil {
		relays = []string{}
	}
	resolved, _ := json.Marshal(relays)
	if _, err := db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, resolved_path, timestamp)
		SELECT t.id, o.rowid, 8, -90, ?, ?, ? FROM transmissions t, observers o WHERE t.hash = ? AND o.id = ?`,
		string(pathJSON), string(resolved), at.Unix(), hash, observer); err != nil {
		t.Fatal(err)
	}
}

func (e *packetShareEnv) get(target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "http://example.test"+target, nil)
	req.RemoteAddr = "192.0.2.10:1234"
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

func (e *packetShareEnv) siteDocument(t *testing.T) string {
	t.Helper()
	w := e.get("/")
	if w.Code != 200 {
		t.Fatalf("/ status %d", w.Code)
	}
	return w.Body.String()
}

func (e *packetShareEnv) share(t *testing.T, hash string) packetShare {
	t.Helper()
	ps, ok := e.srv.resolvePacketShare(context.Background(), e.c, hash)
	if !ok {
		t.Fatalf("%s: not found", hash)
	}
	return ps
}

// ── Off by default ─────────────────────────────────────────────────────────

func TestPacketShare_OffByDefault(t *testing.T) {
	if (&Config{}).Share.Packets.Enabled {
		t.Fatal("share.packets.enabled defaults on")
	}
	var cfg Config
	if err := json.Unmarshal([]byte(`{"share":{"packets":{"enabled":true}}}`), &cfg); err != nil || !cfg.Share.Packets.Enabled {
		t.Fatalf("share.packets.enabled does not parse: %v", err)
	}
	e := newPacketShareEnv(t, false, nil)
	if e.srv.packetCards != nil {
		t.Fatal("packet cards built while off")
	}
	site := e.siteDocument(t)
	if w := e.get("/p?packet=" + psFlood); w.Code != 200 || w.Body.String() != site {
		t.Errorf("off: /p?packet= is %d, site document %t", w.Code, w.Body.String() == site)
	}
	if w := e.get("/api/packets/" + psFlood + "/card.png"); w.Code != 404 || w.Header().Get("Content-Type") == "image/png" {
		t.Errorf("off: card is %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	// The packet page's own routes are untouched by the card route.
	if w := e.get("/api/packets/" + psFlood + "/path"); w.Code != 200 {
		t.Errorf("off: /path is %d", w.Code)
	}
}

// ── The grammar ────────────────────────────────────────────────────────────

func TestPacketShare_ParseTable(t *testing.T) {
	e := newPacketShareEnv(t, true, nil)
	site := e.siteDocument(t)
	for _, c := range []struct {
		name, query string
		code        int
	}{
		{"valid", "packet=" + psFlood, 200},
		{"uppercase", "packet=" + strings.ToUpper(psFlood), 200},
		{"v is ignored", "packet=" + psFlood + "&v=abc123", 200},
		{"tracking keys are ignored", "packet=" + psFlood + "&fbclid=x1&utm_source=discord&gclid=y", 200},
		{"15 characters", "packet=" + psFlood[:15], 400},
		{"17 characters", "packet=" + psFlood + "a", 400},
		{"a numeric id", "packet=12345", 400},
		{"a 16-digit numeric id is a hash", "packet=1000000000000001", 404},
		{"not hex", "packet=" + strings.Repeat("g", 16), 400},
		{"empty", "packet=", 400},
		{"a duplicated key", "packet=" + psFlood + "&packet=" + psFlood, 400},
		{"a duplicated v", "packet=" + psFlood + "&v=1&v=2", 400},
		{"an unknown key", "packet=" + psFlood + "&evil=%3Cscript%3E", 400},
		{"with a profile", "packet=" + psFlood + "&profile=" + shareKeyA + "," + shareKeyB, 400},
		{"a long v", "packet=" + psFlood + "&v=" + strings.Repeat("a", 65), 400},
		{"an unknown hash, well formed", "packet=" + psUnknown, 404},
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
			if got := mustMeta(t, body, "og:url"); got != "http://example.test/p?packet="+psFlood {
				t.Errorf("og:url %q", got)
			}
			// Nothing of the request but the parsed hash is echoed.
			if plain := e.get("/p?packet=" + psFlood).Body.String(); body != plain {
				t.Error("the document differs from the plain hash's: something was echoed")
			}
		})
	}
	// A node key still goes to the node route (off here: the site document).
	if w := e.get("/p?node=" + psR1 + "&packet=" + psFlood); w.Code != 200 || w.Body.String() != site {
		t.Errorf("node + packet: %d", w.Code)
	}
	// The card route takes the same hashes, and v only.
	for path, code := range map[string]int{
		"/api/packets/" + psFlood[:15] + "/card.png":                 400,
		"/api/packets/12345/card.png":                                400,
		"/api/packets/" + psFlood + "/card.png?x=1":                  400,
		"/api/packets/" + psFlood + "/card.png?v=1&v=2":              400,
		"/api/packets/" + psUnknown + "/card.png":                    404,
		"/api/packets/" + strings.ToUpper(psFlood) + "/card.png?v=1": 200,
	} {
		if w := e.get(path); w.Code != code {
			t.Errorf("%s: %d, want %d", path, w.Code, code)
		}
	}
}

// ── The preview ────────────────────────────────────────────────────────────

func TestPacketShare_Tags(t *testing.T) {
	e := newPacketShareEnv(t, true, nil)
	w := e.get("/p?packet=" + psFlood)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control %q", cc)
	}
	body := w.Body.String()
	if got := mustMeta(t, body, "og:title"); got != "GRP_TXT · Flood · 2 hops · heard by 3" {
		t.Errorf("og:title %q", got)
	}
	if got := titleOf(t, body); got != "GRP_TXT · Flood · 2 hops · heard by 3" {
		t.Errorf("<title> %q", got)
	}
	desc := mustMeta(t, body, "og:description")
	// The airtime clause depends on the store's background path resolution,
	// so only its form is pinned.
	want := "First heard by Obs Aarhus · Observers: 3 · Spread: 4.0s · Farthest from first: 59.6 km · "
	if !strings.HasPrefix(desc, want) || !strings.HasSuffix(desc, " · Areas: Midtjylland") {
		t.Errorf("og:description\n got %q\nwant %q… · Areas: Midtjylland", desc, want)
	}
	if strings.Contains(desc, "airtime") && !strings.Contains(desc, "Est. flood airtime: ≥") {
		t.Errorf("the airtime is not stated as a lower bound: %q", desc)
	}
	if got := mustMeta(t, body, "robots"); got != "noindex" {
		t.Errorf("robots %q", got)
	}
	ps := e.share(t, psFlood)
	wantImg := "http://example.test/api/packets/" + psFlood + "/card.png?v=" + e.c.version(ps.in)
	if got := mustMeta(t, body, "og:image"); got != wantImg {
		t.Errorf("og:image %q, want %q", got, wantImg)
	}
	if mustMeta(t, body, "og:image:width") != "1200" || mustMeta(t, body, "og:image:height") != "630" {
		t.Error("card size not stated")
	}
	alt := mustMeta(t, body, "og:image:alt")
	if !strings.HasPrefix(alt, "Packet path schematic: GRP_TXT · Flood · 2 hops · heard by 3. First heard by Obs Aarhus. Observers: 3.") || !strings.HasSuffix(alt, "Areas: Midtjylland.") {
		t.Errorf("og:image:alt %q", alt)
	}
	// No coordinate and no relay or other observer name reaches a tag.
	for _, k := range []string{"og:title", "og:description", "og:image:alt", "og:url", "og:image"} {
		v := mustMeta(t, body, k)
		for _, s := range []string{"56.", "9.5", "10.2", "Relay One", "Relay Two", "Viborg", "Silkeborg"} {
			if strings.Contains(v, s) {
				t.Errorf("%s carries %q: %q", k, s, v)
			}
		}
	}
	// The card itself is the 1200×630 PNG of that v.
	cw := e.get(strings.TrimPrefix(wantImg, "http://example.test"))
	if cw.Code != 200 || cw.Header().Get("Content-Type") != "image/png" || cw.Header().Get("Cache-Control") != "public, max-age=86400" {
		t.Fatalf("card: %d %s %s", cw.Code, cw.Header().Get("Content-Type"), cw.Header().Get("Cache-Control"))
	}
	img, err := png.Decode(bytes.NewReader(cw.Body.Bytes()))
	if err != nil || img.Bounds().Dx() != 1200 || img.Bounds().Dy() != 630 {
		t.Fatalf("card decodes %v, %v", err, img.Bounds())
	}
}

// The card's input is exactly the filtered path: one branch per visible
// observer, positions only, the first observation and the areas.
func TestPacketShare_InputIsTheFilteredPath(t *testing.T) {
	e := newPacketShareEnv(t, true, nil)
	in := e.share(t, psFlood).in
	if in.PayloadType != 5 || in.RouteType != 1 || !in.FirstHeard.Equal(psT0) {
		t.Errorf("type %d route %d first %v", in.PayloadType, in.RouteType, in.FirstHeard)
	}
	if len(in.Branches) != 3 || in.First == nil || in.FirstObserverName != "Obs Aarhus" || in.AdvertName != "" {
		t.Fatalf("branches %d first %v name %q advert %q", len(in.Branches), in.First, in.FirstObserverName, in.AdvertName)
	}
	if b := in.Branches[0]; b.Hops != 2 || len(b.Points) != 2 || b.Observer == nil || b.Points[0] != (packetcard.LatLon{Lat: 56.20, Lon: 9.50}) {
		t.Errorf("deepest branch %+v", b)
	}
	if len(in.Areas) != 1 || len(in.Areas[0].Ring) != 4 {
		t.Errorf("areas %+v", in.Areas)
	}
}

// ── Privacy ────────────────────────────────────────────────────────────────

// assertNoPayload checks the preview document, the card's input (on the
// struct, by reflection and as JSON, not by OCR) and the alt text.
func assertNoPayload(t *testing.T, e *packetShareEnv, hash string) {
	t.Helper()
	w := e.get("/p?packet=" + hash)
	if w.Code != 200 {
		t.Fatalf("%s: %d", hash, w.Code)
	}
	ps := e.share(t, hash)
	js, _ := json.Marshal(ps.in)
	texts := []string{w.Body.String(), string(js), fmt.Sprintf("%+v", ps.in), strings.Join(ps.areas, "|"), e.c.page(sharedPacket{Hash: hash}, ps).ImageAlt}
	for _, s := range packetCardStrings(reflect.ValueOf(ps.in)) {
		texts = append(texts, s)
	}
	for _, text := range texts {
		for _, secret := range psSecrets {
			if strings.Contains(text, secret) {
				t.Errorf("%s: %q reaches the preview: %.200q", hash, secret, text)
			}
		}
	}
}

// packetCardStrings is every string field reachable from v.
func packetCardStrings(v reflect.Value) []string {
	switch v.Kind() {
	case reflect.String:
		return []string{v.String()}
	case reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		return packetCardStrings(v.Elem())
	case reflect.Struct:
		var out []string
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				out = append(out, packetCardStrings(v.Field(i))...)
			}
		}
		return out
	case reflect.Slice, reflect.Array:
		var out []string
		for i := 0; i < v.Len(); i++ {
			out = append(out, packetCardStrings(v.Index(i))...)
		}
		return out
	}
	return nil
}

func TestPacketShare_PrivacyGrpTxtTextAndChannel(t *testing.T) {
	e := newPacketShareEnv(t, true, nil)
	assertNoPayload(t, e, psFlood)
	assertNoPayload(t, e, psFirstUncounted)
}

// Owner decision Q2 (revised): a direct TXT_MSG gets a card too, and
// nothing from its payload appears.
func TestPacketShare_PrivacyDirectTxtMsg(t *testing.T) {
	e := newPacketShareEnv(t, true, nil)
	assertNoPayload(t, e, psDirect)
	body := e.get("/p?packet=" + psDirect).Body.String()
	if got := mustMeta(t, body, "og:title"); got != "TXT_MSG · Direct · 1 hop · heard by 1" {
		t.Errorf("og:title %q", got)
	}
	w := e.get("/api/packets/" + psDirect + "/card.png")
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
		t.Errorf("a direct TXT_MSG card is %d", w.Code)
	}
}

// Privacy rule 2: only an advert names its origin, and a hidden origin is
// the plain site document with a 404.
func TestPacketShare_AdvertOrigin(t *testing.T) {
	e := newPacketShareEnv(t, true, func(c *Config) { c.SetHiddenNamePrefixes([]string{"Secret"}) })
	body := e.get("/p?packet=" + psAdvert).Body.String()
	if got := mustMeta(t, body, "og:description"); !strings.HasPrefix(got, "Advert from Relay Two · First heard by Obs Aarhus · ") {
		t.Errorf("og:description %q", got)
	}
	if in := e.share(t, psAdvert).in; in.AdvertName != "Relay Two" {
		t.Errorf("advert name %q", in.AdvertName)
	}
	// No other type carries an origin, whatever its payload says.
	if in := e.share(t, psFlood).in; in.AdvertName != "" {
		t.Errorf("a GRP_TXT names an origin: %q", in.AdvertName)
	}
	site := e.siteDocument(t)
	for name, edit := range map[string]func(*Config){
		"hidden name prefix": nil,
		"node blacklist":     func(c *Config) { c.SetNodeBlacklist([]string{psRH}) },
		"observer blacklist": func(c *Config) { c.ObserverBlacklist = []string{psRH} },
	} {
		t.Run(name, func(t *testing.T) {
			e := e
			if edit != nil {
				e = newPacketShareEnv(t, true, edit)
			}
			w := e.get("/p?packet=" + psHiddenAdvert)
			if w.Code != 404 || w.Body.String() != site {
				t.Errorf("/p: %d, site document %t", w.Code, w.Body.String() == site)
			}
			cw, uw := e.get("/api/packets/"+psHiddenAdvert+"/card.png"), e.get("/api/packets/"+psUnknown+"/card.png")
			if cw.Code != 404 || cw.Body.String() != uw.Body.String() {
				t.Errorf("card: %d %q, unknown %q", cw.Code, cw.Body.String(), uw.Body.String())
			}
		})
	}
	// A rename into a hidden prefix applies on the next request.
	if _, err := e.srv.db.conn.Exec(`UPDATE nodes SET name = 'Secret Two' WHERE public_key = ?`, psR2); err != nil {
		t.Fatal(err)
	}
	if w := e.get("/p?packet=" + psAdvert); w.Code != 404 {
		t.Errorf("after a rename into a hidden prefix: %d", w.Code)
	}
}

// Privacy rule 3: a branch through a blacklisted repeater is removed whole,
// and the counts match what is drawn.
func TestPacketShare_BlacklistedRelayRemovesTheBranch(t *testing.T) {
	e := newPacketShareEnv(t, true, func(c *Config) { c.SetNodeBlacklist([]string{psR2}) })
	in := e.share(t, psFlood).in
	if len(in.Branches) != 2 || packetcard.Observers(in) != 2 {
		t.Fatalf("branches %d, observers %d; want O1 and O3 only", len(in.Branches), packetcard.Observers(in))
	}
	for _, b := range in.Branches {
		for _, p := range b.Points {
			if p == (packetcard.LatLon{Lat: 56.30, Lon: 9.80}) {
				t.Error("the blacklisted relay's position is drawn")
			}
		}
	}
	body := e.get("/p?packet=" + psFlood).Body.String()
	if got := mustMeta(t, body, "og:title"); got != "GRP_TXT · Flood · 1 hop · heard by 2" {
		t.Errorf("og:title %q", got)
	}
	if got := mustMeta(t, body, "og:description"); !strings.Contains(got, "Observers: 2") || strings.Contains(got, "Est. flood airtime") {
		t.Errorf("og:description %q", got)
	}
	if in.AirtimeMs != nil {
		t.Error("the flood airtime survives a removed branch")
	}
}

// Privacy rules 3 and 4: the observer blacklist and a hidden observer name
// remove that observer's branch.
func TestPacketShare_HiddenObservers(t *testing.T) {
	for name, edit := range map[string]func(*Config){
		"observer blacklist": func(c *Config) { c.ObserverBlacklist = []string{psO3} },
		"hidden name prefix": func(c *Config) { c.SetHiddenNamePrefixes([]string{"Obs Silke"}) },
	} {
		t.Run(name, func(t *testing.T) {
			e := newPacketShareEnv(t, true, edit)
			in := e.share(t, psFlood).in
			if len(in.Branches) != 2 || in.FirstObserverName != "Obs Aarhus" {
				t.Errorf("branches %d first %q", len(in.Branches), in.FirstObserverName)
			}
			for _, b := range in.Branches {
				if b.Observer != nil && *b.Observer == (packetcard.LatLon{Lat: 56.17, Lon: 9.55}) {
					t.Error("the hidden observer is drawn")
				}
			}
		})
	}
}

// Owner decision Q3 (revised): a hidden first observer falls back to
// counts only, and no other observer is promoted.
func TestPacketShare_HiddenFirstObserverFallsBackToCounts(t *testing.T) {
	e := newPacketShareEnv(t, true, func(c *Config) { c.ObserverBlacklist = []string{psO1} })
	ps := e.share(t, psFlood)
	in := ps.in
	if in.First != nil || in.FirstObserverName != "" || packetcard.Observers(in) != 2 {
		t.Fatalf("first %v name %q observers %d", in.First, in.FirstObserverName, packetcard.Observers(in))
	}
	for _, b := range in.Branches {
		if b.SecondsAfterFirst != nil || b.DistanceFromFirstKm != nil {
			t.Error("a measurement from the hidden first survives")
		}
	}
	body := e.get("/p?packet=" + psFlood).Body.String()
	desc := mustMeta(t, body, "og:description")
	for _, s := range []string{"First heard", "Aarhus", "Viborg", "Silkeborg", "Spread", "Farthest"} {
		if strings.Contains(desc, s) || strings.Contains(mustMeta(t, body, "og:image:alt"), s) {
			t.Errorf("%q in the preview without a visible first: %q", s, desc)
		}
	}
	if desc != "Observers: 2 · Areas: Midtjylland" {
		t.Errorf("og:description %q", desc)
	}
}

// #852 review, finding 5: the first observation is visible but its
// observer's deepest branch went through a hidden relay. Naming that
// observer while the count leaves it out would tell that a branch is
// missing, so the card falls back to counts.
func TestPacketShare_FirstWithoutAVisibleBranchIsDropped(t *testing.T) {
	e := newPacketShareEnv(t, true, nil)
	in := e.share(t, psFirstUncounted).in
	if in.First == nil || in.FirstObserverName != "Obs Aarhus" || packetcard.Observers(in) != 2 {
		t.Fatalf("unfiltered: first %v %q, observers %d", in.First, in.FirstObserverName, packetcard.Observers(in))
	}
	e = newPacketShareEnv(t, true, func(c *Config) { c.SetHiddenNamePrefixes([]string{"Secret"}) })
	in = e.share(t, psFirstUncounted).in
	if in.First != nil || in.FirstObserverName != "" || packetcard.Observers(in) != 1 {
		t.Fatalf("filtered: first %v %q, observers %d", in.First, in.FirstObserverName, packetcard.Observers(in))
	}
	if b := in.Branches[0]; b.SecondsAfterFirst != nil || b.DistanceFromFirstKm != nil {
		t.Error("a measurement from the dropped first survives")
	}
	if body := e.get("/p?packet=" + psFirstUncounted).Body.String(); strings.Contains(body, "Aarhus") {
		t.Error("the uncounted first observer is named")
	}
}

func TestSameObserver(t *testing.T) {
	a := &PacketPathObserver{PublicKey: psO1, Name: "A", IATA: "AAR"}
	for _, c := range []struct {
		a, b *PacketPathObserver
		want bool
	}{
		{a, &PacketPathObserver{PublicKey: strings.ToUpper(psO1), Name: "other"}, true},
		{a, &PacketPathObserver{PublicKey: psO2, Name: "A", IATA: "AAR"}, false},
		{a, &PacketPathObserver{Name: "A", IATA: "AAR"}, false},
		{&PacketPathObserver{Name: "A", IATA: "AAR"}, &PacketPathObserver{Name: "A", IATA: "AAR"}, true},
		{&PacketPathObserver{Name: "A", IATA: "AAR"}, &PacketPathObserver{Name: "A", IATA: "CPH"}, false},
		{nil, a, false},
		{a, nil, false},
	} {
		if got := sameObserver(c.a, c.b); got != c.want {
			t.Errorf("sameObserver(%+v, %+v) = %t", c.a, c.b, got)
		}
	}
}

// Rule 5: everything filtered out is a text-only card with the counts,
// never an empty map.
func TestPacketShare_EverythingFilteredIsTextOnly(t *testing.T) {
	e := newPacketShareEnv(t, true, func(c *Config) { c.ObserverBlacklist = []string{psO1, psO2, psO3} })
	ps := e.share(t, psFlood)
	if len(ps.in.Branches) != 0 || ps.in.First != nil || positioned(ps.in) {
		t.Fatalf("input %+v", ps.in)
	}
	body := e.get("/p?packet=" + psFlood).Body.String()
	if got := mustMeta(t, body, "og:title"); got != "GRP_TXT · Flood · no visible observers" {
		t.Errorf("og:title %q", got)
	}
	if alt := mustMeta(t, body, "og:image:alt"); !strings.HasPrefix(alt, "Packet card: GRP_TXT · Flood · no visible observers. No station on this path has an advertised position.") {
		t.Errorf("og:image:alt %q", alt)
	}
	if w := e.get("/api/packets/" + psFlood + "/card.png"); w.Code != 200 {
		t.Errorf("card %d", w.Code)
	}
}

// A failed visibility lookup refuses the packet, cached path or not.
func TestPacketShare_VisibilityLookupFailsClosed(t *testing.T) {
	e := newPacketShareEnv(t, true, nil)
	if w := e.get("/api/packets/" + psFlood + "/card.png"); w.Code != 200 {
		t.Fatalf("card %d", w.Code)
	}
	e.srv.cfg.SetHiddenNamePrefixes([]string{"Secret"})
	if _, err := e.srv.db.conn.Exec(`DROP TABLE observers`); err != nil {
		t.Fatal(err)
	}
	if w := e.get("/p?packet=" + psFlood); w.Code != 404 {
		t.Errorf("/p: %d", w.Code)
	}
	if w := e.get("/api/packets/" + psFlood + "/card.png"); w.Code != 404 {
		t.Errorf("card: %d", w.Code)
	}
}

// The names switch of map.dhm.profile.card applies: no name at all.
func TestPacketShare_NamesOff(t *testing.T) {
	no := false
	e := newPacketShareEnv(t, true, func(c *Config) { c.DHM.Profile.Card.Names = &no })
	for _, h := range []string{psFlood, psAdvert} {
		in := e.share(t, h).in
		if in.FirstObserverName != "" || in.AdvertName != "" {
			t.Errorf("%s: names %q %q", h, in.FirstObserverName, in.AdvertName)
		}
		body := e.get("/p?packet=" + h).Body.String()
		for _, s := range []string{"Aarhus", "Relay Two"} {
			if strings.Contains(body, s) {
				t.Errorf("%s: %q named with names off", h, s)
			}
		}
	}
}

func TestPacketShare_XSSNamesAreInert(t *testing.T) {
	evil := `<script>alert(1)</script>"><img src=x onerror=alert(2)>&amp;`
	e := newPacketShareEnv(t, true, nil)
	for _, q := range []string{`UPDATE observers SET name = ? WHERE id = '` + psO1 + `'`,
		`UPDATE transmissions SET decoded_json = json_set(decoded_json, '$.name', ?) WHERE hash = '` + psAdvert + `'`} {
		if _, err := e.srv.db.conn.Exec(q, evil); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range []string{psFlood, psAdvert} {
		body := e.get("/p?packet=" + h).Body.String()
		for _, bad := range []string{"<script>alert", "<img", `"><img`, "onerror=alert(2)>"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s: %q survives in the document", h, bad)
			}
		}
		alt := mustMeta(t, body, "og:image:alt")
		if !strings.Contains(alt, "scriptalert(1)/script") {
			t.Errorf("%s: alt %q does not carry the name as inert text", h, alt)
		}
	}
}

// ── Freshness, v and the caches ────────────────────────────────────────────

func TestPacketCardTTL(t *testing.T) {
	first := psT0
	for _, c := range []struct {
		name  string
		first time.Time
		now   time.Time
		want  time.Duration
	}{
		{"just heard", first, first, 2 * time.Minute},
		{"just under 10 min", first, first.Add(10*time.Minute - time.Nanosecond), 2 * time.Minute},
		{"at 10 min", first, first.Add(10 * time.Minute), 24 * time.Hour},
		{"a day old", first, first.Add(24 * time.Hour), 24 * time.Hour},
		{"unknown first heard", time.Time{}, first, 2 * time.Minute},
		{"first heard in the future", first.Add(time.Hour), first, 2 * time.Minute},
	} {
		if got := packetCardTTL(c.first, c.now); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if packetCardCacheControl(packetCardTTLFresh) != "public, max-age=120" || packetCardCacheControl(packetCardTTLFinal) != "public, max-age=86400" {
		t.Error("Cache-Control does not match the TTLs")
	}
}

// The TTL switches at the 10 min boundary on the wire too.
func TestPacketShare_CacheControlSwitchesAtTenMinutes(t *testing.T) {
	e := newPacketShareEnv(t, true, nil)
	path := "/api/packets/" + psFlood + "/card.png"
	for _, c := range []struct {
		age  time.Duration
		want string
	}{
		{10*time.Minute - time.Second, "public, max-age=120"},
		{10 * time.Minute, "public, max-age=86400"},
	} {
		e.now = psT0.Add(c.age)
		e.c.paths = dhm.NewCache[string](packetCardCacheEntries, packetCardCacheEntries*packetPathCacheEntryMax, packetCardTTLFinal)
		if w := e.get(path); w.Code != 200 || w.Header().Get("Cache-Control") != c.want {
			t.Errorf("age %v: %d %q, want %q", c.age, w.Code, w.Header().Get("Cache-Control"), c.want)
		}
	}
}

// v is stable while the packet is unchanged and changes when an
// observation is added: at once for the drawn values, and for a packet
// still arriving after its 2 min path cache.
func TestPacketShare_VersionFollowsTheDrawnValues(t *testing.T) {
	e := newPacketShareEnv(t, true, nil)
	fresh := "f600000000000006"
	seedSharePacket(t, e.srv.db, fresh, 5, 1, `{"type":"CHAN","text":"x"}`, psNow.Add(-time.Minute))
	seedShareObservation(t, e.srv.db, fresh, psO1, nil, psNow.Add(-time.Minute))
	v0 := e.c.version(e.share(t, fresh).in)
	if v := e.c.version(e.share(t, fresh).in); v != v0 {
		t.Fatalf("v moved without a change: %s → %s", v0, v)
	}
	if !strings.Contains(e.get("/p?packet="+fresh).Body.String(), "v="+v0) {
		t.Error("og:image does not carry v")
	}
	seedShareObservation(t, e.srv.db, fresh, psO2, []string{psR1}, psNow.Add(-50*time.Second))
	// Within the 2 min the cached path answers; after it, the new branch.
	e.now = psNow.Add(time.Minute)
	if v := e.c.version(e.share(t, fresh).in); v != v0 {
		t.Errorf("v moved inside the path cache's 2 min: %s → %s", v0, v)
	}
	e.now = psNow.Add(2*time.Minute + time.Second)
	in := e.share(t, fresh).in
	if v := e.c.version(in); v == v0 || len(in.Branches) != 2 {
		t.Errorf("after an added observation: v %s (was %s), %d branches", v, v0, len(in.Branches))
	}
	// Every drawn value moves v; the palette, names and site do too.
	base := e.share(t, psFlood).in
	vb := e.c.version(base)
	for name, mut := range map[string]func(*packetcard.PacketCardInput){
		"hops":        func(in *packetcard.PacketCardInput) { in.Branches[0].Hops++ },
		"a point":     func(in *packetcard.PacketCardInput) { in.Branches[0].Points[0].Lat += 0.001 },
		"first name":  func(in *packetcard.PacketCardInput) { in.FirstObserverName = "x" },
		"first heard": func(in *packetcard.PacketCardInput) { in.FirstHeard = in.FirstHeard.Add(time.Second) },
		"airtime":     func(in *packetcard.PacketCardInput) { v := 1.0; in.AirtimeMs = &v },
		"area":        func(in *packetcard.PacketCardInput) { in.Areas = nil },
		"route":       func(in *packetcard.PacketCardInput) { in.RouteType = 2 },
	} {
		in := e.share(t, psFlood).in
		mut(&in)
		if e.c.version(in) == vb {
			t.Errorf("%s does not move v", name)
		}
	}
	c := &packetCards{names: e.c.names, palFP: e.c.palFP, site: "other.example"}
	if c.version(base) == vb {
		t.Error("the site does not move v")
	}
}

func TestPacketShare_CacheHitAndChangedValueMisses(t *testing.T) {
	e := newPacketShareEnv(t, true, nil)
	var renders atomic.Int32
	e.c.render = func(in packetcard.PacketCardInput, _ packetcard.Options) ([]byte, error) {
		renders.Add(1)
		return []byte(fmt.Sprintf("png:%d", len(in.Branches))), nil
	}
	path := "/api/packets/" + psFlood + "/card.png"
	if w := e.get(path); w.Header().Get("X-CoreScope-Card") != "miss" {
		t.Fatalf("first: %s", w.Header().Get("X-CoreScope-Card"))
	}
	if w := e.get(path + "?v=whatever"); w.Header().Get("X-CoreScope-Card") != "hit" || renders.Load() != 1 {
		t.Fatalf("second: %s, %d renders", w.Header().Get("X-CoreScope-Card"), renders.Load())
	}
	// A visibility change reaches a cached card: the filter runs on every
	// request, and the changed v misses.
	e.srv.cfg.SetNodeBlacklist([]string{psR2})
	w := e.get(path)
	if w.Header().Get("X-CoreScope-Card") != "miss" || w.Body.String() != "png:2" {
		t.Errorf("after a blacklist change: %s %q", w.Header().Get("X-CoreScope-Card"), w.Body.String())
	}
	// A packet gone from the store and the DB is 404, cached path or not.
	gone := "f700000000000007"
	seedSharePacket(t, e.srv.db, gone, 5, 1, `{}`, psT0)
	seedShareObservation(t, e.srv.db, gone, psO1, nil, psT0)
	if w := e.get("/api/packets/" + gone + "/card.png"); w.Code != 200 {
		t.Fatalf("a DB-only packet: %d", w.Code)
	}
	if _, err := e.srv.db.conn.Exec(`DELETE FROM transmissions WHERE hash = ?`, gone); err != nil {
		t.Fatal(err)
	}
	if w := e.get("/p?packet=" + gone); w.Code != 404 {
		t.Errorf("a packet gone from the DB: %d", w.Code)
	}
}

func TestPacketShare_CacheBound(t *testing.T) {
	e := newPacketShareEnv(t, true, func(c *Config) { zero := 0; c.DHM.Profile.RateLimitPerMin = &zero })
	e.c.render = func(packetcard.PacketCardInput, packetcard.Options) ([]byte, error) { return make([]byte, 1000), nil }
	n := 3 * packetCardCacheEntries
	for i := 0; i < n; i++ {
		h := fmt.Sprintf("%016x", i+1)
		seedSharePacket(t, e.srv.db, h, 5, 1, `{}`, psT0)
		seedShareObservation(t, e.srv.db, h, psO1, []string{psR1}, psT0)
	}
	for i := 0; i < n; i++ {
		if w := e.get("/api/packets/" + fmt.Sprintf("%016x", i+1) + "/card.png"); w.Code != 200 {
			t.Fatalf("card %d: %d", i, w.Code)
		}
	}
	if c := e.c.cards.Len(); c > packetCardCacheEntries || c == 0 {
		t.Errorf("card cache holds %d, bound %d", c, packetCardCacheEntries)
	}
	if c := e.c.paths.Len(); c > packetCardCacheEntries || c == 0 {
		t.Errorf("path cache holds %d, bound %d", c, packetCardCacheEntries)
	}
}

// A path is read from the DB once per cache lifetime, not per request.
func TestPacketShare_PathIsCachedNotRequeried(t *testing.T) {
	e := newPacketShareEnv(t, true, nil)
	e.get("/p?packet=" + psFlood)
	if e.c.paths.Len() != 1 {
		t.Fatalf("path cache %d", e.c.paths.Len())
	}
	// With the observations gone, a cached path still answers: proof the
	// second request did not query them.
	if _, err := e.srv.db.conn.Exec(`DELETE FROM observations`); err != nil {
		t.Fatal(err)
	}
	if in := e.share(t, psFlood).in; len(in.Branches) != 3 {
		t.Errorf("a cached request re-read the path: %d branches", len(in.Branches))
	}
}

// All card types draw from one per-IP budget and one set of render slots.
func TestPacketShare_SharesLimiterAndRenderSlots(t *testing.T) {
	one := 1
	te := newShareEnvWith(t, dhm.ProfileConfig{Share: dhm.ShareConfig{Enabled: true}, RateLimitPerMin: &one}, nil)
	te.srv.cfg.Share.Packets.Enabled = true
	te.srv.initPacketCards()
	if c := te.srv.packetCards; c == nil || c.limiter != te.srv.terrain.limiter || c.admit != te.srv.terrain.card.admit {
		t.Fatal("packet cards do not share the terrain card's limiter and render slots")
	}
	e := newPacketShareEnv(t, true, func(c *Config) { c.DHM.Profile.RateLimitPerMin = &one; c.Share.Nodes.Enabled = true })
	if e.c.limiter != e.srv.nodeCards.limiter || e.c.admit != e.srv.nodeCards.admit {
		t.Fatal("without the terrain card, packet cards do not share the node card's pair")
	}
	e = newPacketShareEnv(t, true, func(c *Config) { c.DHM.Profile.RateLimitPerMin = &one })
	if e.c.limiter == nil || e.c.admit == nil || e.c.admit.Cap() != cardRenderSlots() {
		t.Fatal("no limiter or render slots of its own")
	}
	e.c.render = func(packetcard.PacketCardInput, packetcard.Options) ([]byte, error) { return []byte("png"), nil }
	if w := e.get("/api/packets/" + psFlood + "/card.png"); w.Code != 200 {
		t.Fatalf("first card %d", w.Code)
	}
	if w := e.get("/api/packets/" + psFlood + "/card.png"); w.Code != 200 {
		t.Fatalf("cached card %d", w.Code)
	}
	if w := e.get("/api/packets/" + psAdvert + "/card.png"); w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("second uncached card %d", w.Code)
	}
}

func TestPacketShare_RenderSlotsFullIs503AndRefunded(t *testing.T) {
	one := 1
	e := newPacketShareEnv(t, true, func(c *Config) { c.DHM.Profile.RateLimitPerMin = &one })
	e.c.admit = dhm.NewAdmission(1)
	release, started := make(chan struct{}), make(chan struct{})
	e.c.render = func(packetcard.PacketCardInput, packetcard.Options) ([]byte, error) {
		close(started)
		<-release
		return []byte("png"), nil
	}
	done := make(chan int)
	go func() {
		req := httptest.NewRequest("GET", "/api/packets/"+psAdvert+"/card.png", nil)
		req.RemoteAddr = "192.0.2.99:1"
		w := httptest.NewRecorder()
		e.router.ServeHTTP(w, req)
		done <- w.Code
	}()
	<-started
	if w := e.get("/api/packets/" + psFlood + "/card.png"); w.Code != 503 || w.Header().Get("Retry-After") == "" {
		t.Errorf("slots full: %d", w.Code)
	}
	close(release)
	if code := <-done; code != 200 {
		t.Errorf("the running render: %d", code)
	}
	e.c.render = func(packetcard.PacketCardInput, packetcard.Options) ([]byte, error) { return []byte("png"), nil }
	if w := e.get("/api/packets/" + psFlood + "/card.png"); w.Code != 200 {
		t.Errorf("after a refused render: %d, the charge was not refunded", w.Code)
	}
}

// ── Perf ───────────────────────────────────────────────────────────────────

// benchPacketShareEnv adds a 30-branch flood to the fixture: 30 observers,
// each 3 relays deep, the shape of a busy flood.
func benchPacketShareEnv(b *testing.B) (*packetShareEnv, string) {
	b.Helper()
	e := newPacketShareEnv(b, true, func(c *Config) {
		zero := 0
		c.DHM.Profile.RateLimitPerMin = &zero
		c.SetHiddenNamePrefixes([]string{"🚫"})
	})
	h := "fb00000000000030"
	seedSharePacket(b, e.srv.db, h, 5, 1, `{}`, psT0)
	for i := 0; i < 30; i++ {
		obs := fmt.Sprintf("%064x", 0xc000+i)
		relay := fmt.Sprintf("%064x", 0xd000+i)
		e.srv.db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen) VALUES (?, ?, 'repeater', ?, ?, ?)`,
			obs, fmt.Sprintf("Obs %d", i), 55.9+float64(i)*0.02, 8.5+float64(i)*0.07, psT0.Format(time.RFC3339))
		e.srv.db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen) VALUES (?, ?, 'repeater', ?, ?, ?)`,
			relay, fmt.Sprintf("Relay %d", i), 56.0+float64(i)*0.015, 9.0+float64(i)*0.04, psT0.Format(time.RFC3339))
		e.srv.db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES (?, ?, 'AAR')`, obs, fmt.Sprintf("Obs %d", i))
		seedShareObservation(b, e.srv.db, h, obs, []string{psR1, psR2, relay}, psT0.Add(time.Duration(i)*time.Second))
	}
	return e, h
}

// BenchmarkPacketCard_Cold is one uncached card: lookup, getPacketPath, the
// visibility filter, v and the render.
func BenchmarkPacketCard_Cold(b *testing.B) {
	e, h := benchPacketShareEnv(b)
	path := "/api/packets/" + h + "/card.png"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.c.cards = dhm.NewCache[packetCardKey](packetCardCacheEntries, packetCardCacheEntries*cardBytesPerEntry, packetCardTTLFinal)
		e.c.paths = dhm.NewCache[string](packetCardCacheEntries, packetCardCacheEntries*packetPathCacheEntryMax, packetCardTTLFinal)
		if w := e.get(path); w.Code != 200 || w.Header().Get("X-CoreScope-Card") != "miss" {
			b.Fatalf("%d %s", w.Code, w.Header().Get("X-CoreScope-Card"))
		}
	}
}

// BenchmarkPacketCard_Cached is a cached card: lookup, the cached path, the
// visibility filter and v.
func BenchmarkPacketCard_Cached(b *testing.B) {
	e, h := benchPacketShareEnv(b)
	path := "/api/packets/" + h + "/card.png"
	e.get(path)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if w := e.get(path); w.Header().Get("X-CoreScope-Card") != "hit" {
			b.Fatal(w.Header().Get("X-CoreScope-Card"))
		}
	}
}

// BenchmarkPacketShare_Page is /p?packet= with the path cached.
func BenchmarkPacketShare_Page(b *testing.B) {
	e, h := benchPacketShareEnv(b)
	e.get("/p?packet=" + h)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if w := e.get("/p?packet=" + h); w.Code != 200 {
			b.Fatal(w.Code)
		}
	}
}
