package main

// #737 M1 — GET /api/skraafoto/search and /api/skraafoto/preview/{c}/{id}.
// Every test runs against the skraafototest fakes through a RoundTripper;
// neither the STAC API nor the CDN is ever contacted, and the only token is
// the fakes' sentinel.

import (
	"encoding/json"
	"fmt"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/dhm/dhmtest"
	"github.com/meshcore-analyzer/skraafoto"
	"github.com/meshcore-analyzer/skraafoto/skraafototest"
)

// Aarhus C, inside Denmark, and Malmö, inside DHM's box but on foreign
// land.
const (
	skraafotoAarhus = "lat=56.1567&lon=10.2108"
	skraafotoMalmo  = "lat=55.6050&lon=13.0038"
)

type skraafotoClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *skraafotoClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *skraafotoClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type skraafotoEnv struct {
	srv    *Server
	router *mux.Router
	p      *skraafotoProxy
	up     *skraafototest.Upstream
	logs   *dhmLogSink
	clock  *skraafotoClock
	mu     sync.Mutex
	seen   []*httptest.ResponseRecorder // every response, for the leak scan
}

// newSkraafotoEnv serves photos whose IFD4 is real JPEG for every CDN path.
func newSkraafotoEnv(t *testing.T, sc SkraafotoConfig) *skraafotoEnv {
	t.Helper()
	srv, router := setupTestServer(t)
	up := skraafototest.New()
	t.Cleanup(up.Close)
	cog := dhmtest.ImageCOG(dhmtest.ImageCOGOptions{Levels: dhmtest.SkraafotoLevels, Encode: func(l int) bool { return l == 4 }, Pad: 1 << 20})
	up.COG = func(string) []byte { return cog }
	logs := &dhmLogSink{}
	clock := &skraafotoClock{t: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
	sc.Enabled = true
	opts := skraafoto.Options{Transport: up.Transport(), Timeout: 5 * time.Second}
	p := newSkraafotoProxy(resolveSkraafotoSettings(sc, logs.logf), skraafotoDeps{
		stac: skraafoto.NewSTAC(skraafototest.Token, opts),
		cdn:  skraafoto.NewCDN(opts),
		logf: logs.logf,
		now:  clock.now,
	})
	if p == nil {
		t.Fatal("newSkraafotoProxy returned nil")
	}
	srv.skraafoto = p
	return &skraafotoEnv{srv: srv, router: router, p: p, up: up, logs: logs, clock: clock}
}

func skraafotoFrom(ip string) func(*http.Request) {
	return func(r *http.Request) { r.RemoteAddr = ip + ":1000" }
}

func (e *skraafotoEnv) get(target string, mod ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", target, nil)
	req.RemoteAddr = "203.0.113.7:1000"
	for _, m := range mod {
		m(req)
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	e.mu.Lock()
	e.seen = append(e.seen, w)
	e.mu.Unlock()
	return w
}

func (e *skraafotoEnv) search(query string, mod ...func(*http.Request)) *httptest.ResponseRecorder {
	return e.get("/api/skraafoto/search?"+query, mod...)
}

func (e *skraafotoEnv) preview(c, id string, mod ...func(*http.Request)) *httptest.ResponseRecorder {
	return e.get("/api/skraafoto/preview/"+c+"/"+id, mod...)
}

func decodeSkraafotoSearch(t *testing.T, w *httptest.ResponseRecorder) SkraafotoSearchResponse {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var resp SkraafotoSearchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func (e *skraafotoEnv) upstream() (stac, cdn int64) {
	return e.up.STACRequests.Load(), e.up.CDNRequests.Load()
}

func TestSkraafoto737_DisabledIs404(t *testing.T) {
	srv, router := setupTestServer(t)
	for _, target := range []string{"/api/skraafoto/search?" + skraafotoAarhus, "/api/skraafoto/preview/skraafotos2025/2025_83_36_1_0020_00003045"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", target, nil))
		if w.Code != 404 {
			t.Errorf("%s: %d", target, w.Code)
		}
	}
	if srv.skraafotoClientConfig() != nil {
		t.Fatal("client config offers skraafoto while it is off")
	}
	// On, but without the map.dhm token it uses: off.
	cfg := &Config{Skraafoto: SkraafotoConfig{Enabled: true}}
	if newSkraafotoProxyFromConfig(cfg) != nil {
		t.Fatal("a proxy without a token")
	}
	cfg.DHM.Token = "  "
	if newSkraafotoProxyFromConfig(cfg) != nil {
		t.Fatal("a proxy with a blank token")
	}
	// A token alone does not switch it on.
	cfg = &Config{DHM: DHMConfig{Enabled: true, Token: "t"}}
	if newSkraafotoProxyFromConfig(cfg) != nil {
		t.Fatal("on without map.skraafoto.enabled")
	}
	cfg.Skraafoto.Enabled = true
	if newSkraafotoProxyFromConfig(cfg) == nil {
		t.Fatal("off with map.skraafoto.enabled and a token")
	}
}

func TestSkraafoto737_ConfigLiftedFromMap(t *testing.T) {
	cfg := &Config{Map: map[string]interface{}{
		"skraafoto": map[string]interface{}{"enabled": true, "dailyCap": 50},
		"Skraafoto": map[string]interface{}{"enabled": false},
	}}
	cfg.extractTypedMapBlocks()
	if _, ok := cfg.Map["skraafoto"]; ok {
		t.Fatal("map.skraafoto still in the client-visible Map")
	}
	if _, ok := cfg.Map["Skraafoto"]; ok {
		t.Fatal("a case variant still in the client-visible Map")
	}
	if !cfg.Skraafoto.Enabled || cfg.Skraafoto.DailyCap == nil || *cfg.Skraafoto.DailyCap != 50 {
		t.Fatalf("%+v", cfg.Skraafoto)
	}
}

func TestSkraafoto737_Settings(t *testing.T) {
	logs := &dhmLogSink{}
	set := resolveSkraafotoSettings(SkraafotoConfig{}, logs.logf)
	if set.searchPerMin != 30 || set.previewPerMin != 120 || set.dailyCap != skraafotoDailyCapDefault || set.maxInFlight != 8 || set.cacheBytes != 64<<20 {
		t.Fatalf("defaults %+v", set)
	}
	neg, huge, zero := -1, 1<<30, 0
	set = resolveSkraafotoSettings(SkraafotoConfig{SearchRateLimitPerMin: &neg, PreviewRateLimitPerMin: &huge, DailyCap: &neg, MaxInFlight: &zero, CacheMB: &huge}, logs.logf)
	if set.searchPerMin != 30 || set.previewPerMin != 120 || set.dailyCap != skraafotoDailyCapDefault || set.maxInFlight != 8 || set.cacheBytes != 64<<20 {
		t.Fatalf("out of range %+v", set)
	}
	for _, k := range []string{"searchRateLimitPerMin", "previewRateLimitPerMin", "dailyCap", "maxInFlight", "cacheMB"} {
		if !strings.Contains(logs.String(), "map.skraafoto."+k) {
			t.Errorf("no warning for %s", k)
		}
	}
	if set := resolveSkraafotoSettings(SkraafotoConfig{DailyCap: &zero}, logs.logf); set.dailyCap != 0 {
		t.Fatal("dailyCap 0 is off")
	}
}

func TestSkraafoto737_SearchQueryAllowList(t *testing.T) {
	e := newSkraafotoEnv(t, SkraafotoConfig{})
	for _, q := range []string{
		"", "lat=56.1567", "lon=10.2108", skraafotoAarhus + "&x=1", skraafotoAarhus + "&lat=56.1", "LAT=56.1567&lon=10.2108",
		"lat=NaN&lon=10.2108", "lat=Inf&lon=10.2108", "lat=1e1&lon=10.2108", "lat=56.1567&lon=+10.2", "lat=91&lon=10",
		skraafotoAarhus + "&year=2017", skraafotoAarhus + "&year=25", skraafotoAarhus + "&year=20255", skraafotoAarhus + "&year=",
		skraafotoAarhus + "&year=2025&year=2023", skraafotoAarhus + "&token=" + skraafototest.Token, skraafotoAarhus + "&Year=2025",
		"lat=56.1567&&lon=10.2108", "lat=56.1567;lon=10.2108", "lat=56.15%zz&lon=10.2108", skraafotoAarhus + "&limit=5",
	} {
		if w := e.search(q); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid skraafoto request") {
			t.Errorf("%q: %d %s", q, w.Code, w.Body.String())
		}
	}
	if s, c := e.upstream(); s != 0 || c != 0 {
		t.Fatalf("%d STAC and %d CDN requests for refused queries", s, c)
	}
}

func TestSkraafoto737_SearchFiveDirections(t *testing.T) {
	e := newSkraafotoEnv(t, SkraafotoConfig{})
	w := e.search(skraafotoAarhus)
	resp := decodeSkraafotoSearch(t, w)
	if w.Header().Get("X-CoreScope-Cache") != "miss" {
		t.Fatalf("cache %q", w.Header().Get("X-CoreScope-Cache"))
	}
	if resp.Collection != "skraafotos2025" || resp.Year != 2025 || fmt.Sprint(resp.Years) != "[2025 2023 2021 2019]" {
		t.Fatalf("%+v", resp)
	}
	if len(resp.Photos) != 5 {
		t.Fatalf("%d photos", len(resp.Photos))
	}
	for i, ph := range resp.Photos {
		d := skraafototest.Directions[i]
		id := skraafototest.ItemID("skraafotos2025", d)
		want := SkraafotoPhoto{ID: id, Collection: "skraafotos2025", Direction: d, Datetime: "2025-05-07T10:21:33Z",
			ViewerURL:  "https://skraafoto.dataforsyningen.dk/?item=" + id,
			PreviewURL: "/api/skraafoto/preview/skraafotos2025/" + id}
		if ph != want {
			t.Fatalf("photo %d: %+v\nwant %+v", i, ph, want)
		}
	}
	if resp.Credit != (SkraafotoCredit{Attribution: "Klimadatastyrelsen, Skråfotos, CC BY 4.0", Owner: "Klimadatastyrelsen",
		DatasetName: "Skråfotos", DatasetURL: skraafoto.DatasetURL, LicenseName: "CC BY 4.0", LicenseURL: "https://creativecommons.org/licenses/by/4.0/"}) {
		t.Fatalf("credit %+v", resp.Credit)
	}
	if strings.Contains(w.Body.String(), "cdn.dataforsyningen.dk") || strings.Contains(w.Body.String(), "api.dataforsyningen.dk") {
		t.Fatal("an upstream URL in the response")
	}
	// The collections list and one request per direction.
	if s, c := e.upstream(); s != 1+5 || c != 0 {
		t.Fatalf("%d STAC and %d CDN requests", s, c)
	}
	// Again, and a point a metre away (the same 10 m cell): cached, free.
	for _, q := range []string{skraafotoAarhus, "lat=56.15671&lon=10.21081"} {
		if w := e.search(q); w.Code != 200 || w.Header().Get("X-CoreScope-Cache") != "hit" {
			t.Fatalf("%s: %d %q", q, w.Code, w.Header().Get("X-CoreScope-Cache"))
		}
	}
	if s, _ := e.upstream(); s != 6 {
		t.Fatalf("cached searches went upstream: %d", s)
	}
}

func TestSkraafoto737_SearchMissingDirections(t *testing.T) {
	e := newSkraafotoEnv(t, SkraafotoConfig{})
	e.up.Directions = []string{"nadir", "north", "south", "west"} // as on staging near Aarhus
	resp := decodeSkraafotoSearch(t, e.search(skraafotoAarhus))
	var got []string
	for _, ph := range resp.Photos {
		got = append(got, ph.Direction)
	}
	if strings.Join(got, ",") != "north,south,west,nadir" {
		t.Fatalf("%v", got)
	}
}

func TestSkraafoto737_SearchYearSelector(t *testing.T) {
	e := newSkraafotoEnv(t, SkraafotoConfig{})
	resp := decodeSkraafotoSearch(t, e.search(skraafotoAarhus+"&year=2021"))
	if resp.Collection != "skraafotos2021" || resp.Year != 2021 || resp.Photos[0].ID != skraafototest.ItemID("skraafotos2021", "north") {
		t.Fatalf("%+v", resp)
	}
	s0, _ := e.upstream()
	// A year the API does not have, once the list is cached: 400, free.
	if w := e.search(skraafotoAarhus + "&year=2027"); w.Code != 400 {
		t.Fatalf("unknown year: %d", w.Code)
	}
	if s, _ := e.upstream(); s != s0 {
		t.Fatal("an unknown year went upstream")
	}
}

func TestSkraafoto737_OutsideDenmark(t *testing.T) {
	e := newSkraafotoEnv(t, SkraafotoConfig{SearchRateLimitPerMin: intPtr(1)})
	for i := 0; i < 3; i++ {
		w := e.search(skraafotoMalmo)
		resp := decodeSkraafotoSearch(t, w)
		if len(resp.Photos) != 0 || w.Header().Get("X-CoreScope-Cache") != "none" || resp.Photos == nil || resp.Years == nil {
			t.Fatalf("%+v %q", resp, w.Header().Get("X-CoreScope-Cache"))
		}
	}
	if s, c := e.upstream(); s != 0 || c != 0 {
		t.Fatal("a point outside Denmark went upstream")
	}
}

func TestSkraafoto737_PreviewAfterSearch(t *testing.T) {
	e := newSkraafotoEnv(t, SkraafotoConfig{})
	resp := decodeSkraafotoSearch(t, e.search(skraafotoAarhus))
	s0, _ := e.upstream()
	w := e.get(resp.Photos[2].PreviewURL)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/jpeg" || w.Header().Get("X-CoreScope-Cache") != "miss" {
		t.Fatalf("%d %q %q %s", w.Code, w.Header().Get("Content-Type"), w.Header().Get("X-CoreScope-Cache"), w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "public, max-age=604800" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers %v", w.Header())
	}
	cfg, err := jpeg.DecodeConfig(w.Body)
	if err != nil || cfg.Width != 1024 || cfg.Height != 699 {
		t.Fatalf("%+v %v", cfg, err)
	}
	// The search cached where the photo is: no STAC lookup, two CDN reads.
	if s, c := e.upstream(); s != s0 || c != 2 {
		t.Fatalf("%d STAC, %d CDN requests", s-s0, c)
	}
	if w := e.get(resp.Photos[2].PreviewURL); w.Code != 200 || w.Header().Get("X-CoreScope-Cache") != "hit" {
		t.Fatalf("again: %d %q", w.Code, w.Header().Get("X-CoreScope-Cache"))
	}
	if _, c := e.upstream(); c != 2 {
		t.Fatal("a cached preview went upstream")
	}
}

func TestSkraafoto737_PreviewDeepLink(t *testing.T) {
	e := newSkraafotoEnv(t, SkraafotoConfig{})
	id := skraafototest.ItemID("skraafotos2023", "west")
	if w := e.preview("skraafotos2023", id); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	log := e.up.Log()
	if len(log) != 3 || log[0].Host != skraafoto.STACHost || log[0].Path != "/rest/skraafoto_api/v2/collections/skraafotos2023/items/"+id {
		t.Fatalf("%+v", log)
	}
	if w := e.preview("skraafotos2023", "2023_83_36_9_0020_00003045"); w.Code != 404 || !strings.Contains(w.Body.String(), "photo not found") {
		t.Fatalf("unknown photo: %d %s", w.Code, w.Body.String())
	}
}

func TestSkraafoto737_PreviewPathAllowList(t *testing.T) {
	e := newSkraafotoEnv(t, SkraafotoConfig{})
	id := skraafototest.ItemID("skraafotos2025", "north")
	for _, target := range []string{
		"/api/skraafoto/preview/skraafotos2017/2017_83_36_1_0020_00003045",
		"/api/skraafoto/preview/skraafotos2025test/" + id,
		"/api/skraafoto/preview/SKRAAFOTOS2025/" + id,
		"/api/skraafoto/preview/skraafotos2023/" + id,
		"/api/skraafoto/preview/skraafotos2025/2025-83",
		"/api/skraafoto/preview/skraafotos2025/" + id + "?token=x",
		"/api/skraafoto/preview/skraafotos2025/" + id + "?",
		"/api/skraafoto/preview/skraafotos2025/" + id + ".tif",
	} {
		if w := e.get(target); w.Code != 400 {
			t.Errorf("%s: %d", target, w.Code)
		}
	}
	if s, c := e.upstream(); s != 0 || c != 0 {
		t.Fatal("a refused preview went upstream")
	}
}

// The token reaches the STAC API in its header and nowhere else: never
// the CDN, never a response, never a log line, also when upstream fails.
func TestSkraafoto737_TokenNeverLeaks(t *testing.T) {
	e := newSkraafotoEnv(t, SkraafotoConfig{})
	resp := decodeSkraafotoSearch(t, e.search(skraafotoAarhus))
	e.get(resp.Photos[0].PreviewURL)
	e.preview("skraafotos2021", skraafototest.ItemID("skraafotos2021", "east"))
	e.up.STACHook = func(w http.ResponseWriter, r *http.Request) bool {
		http.Error(w, "upstream says "+r.Header.Get("token"), 500)
		return true
	}
	e.up.CDNHook = func(w http.ResponseWriter, r *http.Request) bool {
		http.Error(w, "boom", 503)
		return true
	}
	if w := e.search("lat=55.6761&lon=12.5683"); w.Code != 502 {
		t.Fatalf("failing STAC: %d", w.Code)
	}
	if w := e.preview("skraafotos2019", skraafototest.ItemID("skraafotos2019", "east")); w.Code != 502 {
		t.Fatalf("failing STAC lookup: %d", w.Code)
	}
	e.up.STACHook = nil
	if w := e.preview("skraafotos2019", skraafototest.ItemID("skraafotos2019", "south")); w.Code != 502 {
		t.Fatalf("failing CDN: %d", w.Code)
	}
	tok := skraafototest.Token
	for i, w := range e.seen {
		if strings.Contains(w.Body.String(), tok) || strings.Contains(fmt.Sprint(w.Header()), tok) {
			t.Fatalf("response %d carries the token", i)
		}
	}
	if strings.Contains(e.logs.String(), tok) || e.logs.String() == "" {
		t.Fatalf("logs: %q", e.logs.String())
	}
	var stac, cdn int
	for _, r := range e.up.Log() {
		inURL := strings.Contains(r.Path+"?"+r.RawQuery, tok)
		var inHeaders []string
		for k, vs := range r.Header {
			for _, v := range vs {
				if strings.Contains(v, tok) {
					inHeaders = append(inHeaders, k)
				}
			}
		}
		switch r.Host {
		case skraafoto.STACHost:
			stac++
			if inURL || len(inHeaders) != 1 || !strings.EqualFold(inHeaders[0], "token") {
				t.Fatalf("STAC request %s: token in URL %v, headers %v", r.Path, inURL, inHeaders)
			}
		case skraafoto.CDNHost:
			cdn++
			if inURL || len(inHeaders) != 0 || r.Header.Get("token") != "" {
				t.Fatalf("CDN request %s carries the token", r.Path)
			}
		default:
			t.Fatalf("request to %s", r.Host)
		}
	}
	if stac == 0 || cdn == 0 {
		t.Fatalf("%d STAC, %d CDN requests", stac, cdn)
	}
}

func TestSkraafoto737_PerIPLimits(t *testing.T) {
	e := newSkraafotoEnv(t, SkraafotoConfig{SearchRateLimitPerMin: intPtr(2), PreviewRateLimitPerMin: intPtr(2)})
	e.p.searchLimiter.now, e.p.previewLimiter.now = e.clock.now, e.clock.now
	a, b := skraafotoFrom("198.51.100.1"), skraafotoFrom("198.51.100.2")
	for _, q := range []string{skraafotoAarhus, "lat=55.6761&lon=12.5683"} {
		if w := e.search(q, a); w.Code != 200 {
			t.Fatalf("%s: %d", q, w.Code)
		}
	}
	// Cached: not charged.
	if w := e.search(skraafotoAarhus, a); w.Code != 200 {
		t.Fatalf("cached: %d", w.Code)
	}
	s0, _ := e.upstream()
	w := e.search("lat=55.4038&lon=10.4024", a)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("third cold search: %d %q", w.Code, w.Header().Get("Retry-After"))
	}
	if s, _ := e.upstream(); s != s0 {
		t.Fatal("a refused search went upstream")
	}
	if w := e.search("lat=55.4038&lon=10.4024", b); w.Code != 200 {
		t.Fatalf("another client: %d", w.Code)
	}
	// Previews have their own budget.
	for _, d := range []string{"north", "east"} {
		if w := e.preview("skraafotos2025", skraafototest.ItemID("skraafotos2025", d), a); w.Code != 200 {
			t.Fatalf("preview %s: %d", d, w.Code)
		}
	}
	if w := e.preview("skraafotos2025", skraafototest.ItemID("skraafotos2025", "south"), a); w.Code != 429 {
		t.Fatalf("third cold preview: %d", w.Code)
	}
	if w := e.preview("skraafotos2025", skraafototest.ItemID("skraafotos2025", "north"), a); w.Code != 200 {
		t.Fatalf("cached preview: %d", w.Code)
	}
	e.clock.add(time.Minute)
	if w := e.preview("skraafotos2025", skraafototest.ItemID("skraafotos2025", "south"), a); w.Code != 200 {
		t.Fatalf("a minute later: %d", w.Code)
	}
}

func TestSkraafoto737_GlobalDailyCap(t *testing.T) {
	e := newSkraafotoEnv(t, SkraafotoConfig{DailyCap: intPtr(2), SearchRateLimitPerMin: intPtr(5)})
	e.p.searchLimiter.now = e.clock.now
	ips := []func(*http.Request){skraafotoFrom("198.51.100.1"), skraafotoFrom("198.51.100.2"), skraafotoFrom("198.51.100.3")}
	if w := e.search(skraafotoAarhus, ips[0]); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := e.preview("skraafotos2025", skraafototest.ItemID("skraafotos2025", "north"), ips[1]); w.Code != 200 {
		t.Fatal(w.Code)
	}
	// Many clients together: the third cold request of the day is refused,
	// whoever sends it, before any upstream work.
	s0, c0 := e.upstream()
	w := e.search("lat=55.6761&lon=12.5683", ips[2])
	if w.Code != 503 || !strings.Contains(w.Body.String(), "daily") {
		t.Fatalf("over the cap: %d %s", w.Code, w.Body.String())
	}
	// 12:00 UTC: twelve hours until the budget refills.
	if ra, _ := strconv.Atoi(w.Header().Get("Retry-After")); ra != 12*3600 {
		t.Fatalf("Retry-After %q", w.Header().Get("Retry-After"))
	}
	if w := e.preview("skraafotos2025", skraafototest.ItemID("skraafotos2025", "east"), ips[2]); w.Code != 503 {
		t.Fatalf("preview over the cap: %d", w.Code)
	}
	if s, c := e.upstream(); s != s0 || c != c0 {
		t.Fatal("a capped request went upstream")
	}
	// The per-client charge of a capped request is given back.
	if n := e.p.searchLimiter.chargedFor(t, "198.51.100.3"); n {
		t.Fatal("the capped client is still charged")
	}
	// Cached answers are still served.
	if w := e.search(skraafotoAarhus, ips[2]); w.Code != 200 {
		t.Fatalf("cached search over the cap: %d", w.Code)
	}
	// The next UTC day the budget is full again.
	e.clock.add(12 * time.Hour)
	if w := e.search("lat=55.6761&lon=12.5683", ips[2]); w.Code != 200 {
		t.Fatalf("next day: %d", w.Code)
	}
}

// chargedFor reports whether ip has an entry in the limiter's table.
func (l *ipRateLimiter) chargedFor(t *testing.T, ip string) bool {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.clients[ip]
	return ok
}

func TestSkraafoto737_AdmissionFullRefunds(t *testing.T) {
	e := newSkraafotoEnv(t, SkraafotoConfig{MaxInFlight: intPtr(1), DailyCap: intPtr(10)})
	gate := make(chan struct{})
	var held atomic.Int32
	e.up.STACHook = func(w http.ResponseWriter, r *http.Request) bool {
		held.Add(1)
		<-gate
		return false
	}
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- e.search(skraafotoAarhus, skraafotoFrom("198.51.100.1")) }()
	skraafotoWait(t, func() bool { return held.Load() > 0 })
	w := e.search("lat=55.6761&lon=12.5683", skraafotoFrom("198.51.100.2"))
	if w.Code != 503 || w.Header().Get("Retry-After") != "5" || !strings.Contains(w.Body.String(), "busy") {
		t.Fatalf("second cold search with one slot: %d %q %s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	if e.p.searchLimiter.chargedFor(t, "198.51.100.2") {
		t.Fatal("a refused search stayed charged per client")
	}
	if used := e.p.daily.usedNow(e.clock.now()); used != 1 {
		t.Fatalf("daily budget %d after a refusal, want 1", used)
	}
	close(gate)
	if w := <-done; w.Code != 200 {
		t.Fatalf("admitted search: %d", w.Code)
	}
}

func TestSkraafoto737_CoalescesPreviews(t *testing.T) {
	e := newSkraafotoEnv(t, SkraafotoConfig{DailyCap: intPtr(10)})
	gate := make(chan struct{})
	var held atomic.Int32
	e.up.CDNHook = func(w http.ResponseWriter, r *http.Request) bool {
		if held.Add(1) == 1 {
			<-gate
		}
		return false
	}
	id := skraafototest.ItemID("skraafotos2025", "nadir")
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = e.preview("skraafotos2025", id, skraafotoFrom(fmt.Sprintf("198.51.100.%d", i+1))).Code
		}(i)
	}
	skraafotoWait(t, func() bool { return held.Load() > 0 && e.p.flight.Pending() == 4 })
	close(gate)
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("caller %d: %d", i, c)
		}
	}
	// One lookup, one header read, one tile read; one day's unit.
	if s, c := e.upstream(); s != 1 || c != 2 {
		t.Fatalf("%d STAC, %d CDN requests", s, c)
	}
	if used := e.p.daily.usedNow(e.clock.now()); used != 1 {
		t.Fatalf("daily budget %d for one upstream preview", used)
	}
}

func TestSkraafoto737_FailuresAreNotCached(t *testing.T) {
	e := newSkraafotoEnv(t, SkraafotoConfig{})
	e.up.STACHook = func(w http.ResponseWriter, r *http.Request) bool { http.Error(w, "boom", 500); return true }
	w := e.search(skraafotoAarhus)
	if w.Code != 502 || !strings.Contains(w.Body.String(), "skraafoto search unavailable") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(e.logs.String(), "status 500") {
		t.Fatalf("log %q", e.logs.String())
	}
	e.up.STACHook = nil
	if w := e.search(skraafotoAarhus); w.Code != 200 || w.Header().Get("X-CoreScope-Cache") != "miss" {
		t.Fatalf("after the failure: %d %q", w.Code, w.Header().Get("X-CoreScope-Cache"))
	}
}

func TestSkraafoto737_ClientConfig(t *testing.T) {
	e := newSkraafotoEnv(t, SkraafotoConfig{})
	w := e.get("/api/config/client")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	var cc struct {
		Skraafoto *SkraafotoClientConfig `json:"skraafoto"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &cc); err != nil || cc.Skraafoto == nil {
		t.Fatalf("%v %s", err, w.Body.String())
	}
	want := SkraafotoClientConfig{Enabled: true, SearchURL: "/api/skraafoto/search", PreviewURL: "/api/skraafoto/preview/{collection}/{item}",
		MaxPx: 1024, Note: "Public aerial photos from Klimadatastyrelsen, shown at reduced size.", Credit: skraafotoCredit()}
	if *cc.Skraafoto != want {
		t.Fatalf("%+v", *cc.Skraafoto)
	}
	if strings.Contains(w.Body.String(), skraafototest.Token) {
		t.Fatal("the token in the client config")
	}
}

// skraafotoWait polls cond for up to 5 s.
func skraafotoWait(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not reached")
}

// config.example.json documents map.skraafoto, off, with the shipped
// defaults, and its comments name no token value.
func TestSkraafoto737_ExampleConfig(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Map map[string]json.RawMessage `json:"map"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	raw, ok := doc.Map["skraafoto"]
	if !ok {
		t.Fatal("config.example.json does not document map.skraafoto")
	}
	var ex SkraafotoConfig
	if err := json.Unmarshal(raw, &ex); err != nil {
		t.Fatal(err)
	}
	def := resolveSkraafotoSettings(SkraafotoConfig{}, nil)
	got := resolveSkraafotoSettings(ex, func(string, ...interface{}) { t.Fatal("example value out of range") })
	if ex.Enabled || got != def || ex.SearchRateLimitPerMin == nil || ex.PreviewRateLimitPerMin == nil ||
		ex.DailyCap == nil || ex.MaxInFlight == nil || ex.CacheMB == nil {
		t.Fatalf("example %+v, not the shipped defaults %+v", got, def)
	}
	if strings.Contains(string(raw), `"token"`) {
		t.Fatal("map.skraafoto has its own token key; it uses map.dhm.token")
	}
}

// The preview cache holds at most cacheMB: older previews are evicted.
func TestSkraafoto737_PreviewCacheBounded(t *testing.T) {
	e := newSkraafotoEnv(t, SkraafotoConfig{})
	// Textured photos: previews of some 57 KB, so the byte bound binds
	// before the entry bound (cacheBytes / 16 KB entries).
	cog := dhmtest.ImageCOG(dhmtest.ImageCOGOptions{Levels: dhmtest.SkraafotoLevels, Encode: func(l int) bool { return l == 4 },
		Samples: 3, Photometric: 6, Pad: 1 << 20, Color: func(level, x, y int) [4]uint8 {
			v := uint32(x*7919+y*104729) * 2654435761
			return [4]uint8{uint8(90 + v>>27), uint8(110 + (v>>22)&31), uint8(70 + (v>>17)&31), 255}
		}})
	e.up.COG = func(string) []byte { return cog }
	set := resolveSkraafotoSettings(SkraafotoConfig{}, nil)
	set.cacheBytes = 160 << 10 // room for two previews, ten entries
	e.p = newSkraafotoProxy(set, skraafotoDeps{stac: e.p.stac, cdn: e.p.cdn, logf: e.logs.logf, now: e.clock.now})
	e.srv.skraafoto = e.p
	for _, d := range skraafototest.Directions {
		if w := e.preview("skraafotos2025", skraafototest.ItemID("skraafotos2025", d)); w.Code != 200 {
			t.Fatalf("%s: %d", d, w.Code)
		}
	}
	if n, size := e.p.previews.Len(), e.p.previews.Size(); size > set.cacheBytes || n > 2 || n == 0 {
		t.Fatalf("%d previews, %d bytes cached; bound %d bytes", n, size, set.cacheBytes)
	}
}
