package main

import (
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/dhm"
)

// #727 M1 — the /p share route and its per-profile Open Graph tags.

const sharePublicDir = "../../public"

var (
	shareKeyA = strings.Repeat("1", 64)
	shareKeyB = strings.Repeat("2", 64)
)

type shareEnv struct {
	*terrainEnv
	src    *fakeElevation
	doc    *indexDoc
	router *mux.Router
}

// newShareEnv is a test server with the profile on, sharing on or off, and
// the real public/index.html behind the production route composition:
// RegisterRoutes, then /p, then the SPA catch-all.
func newShareEnv(t *testing.T, share bool, brand map[string]string) *shareEnv {
	t.Helper()
	src := newFakeElevation(t, flatElev(10))
	env := newTerrainEnv(t, src, dhm.ProfileConfig{Share: dhm.ShareConfig{Enabled: share}}, nil)
	doc := newIndexDoc(sharePublicDir, newOGMeta(brandPick(brand), sharePublicDir, nil))
	router := mux.NewRouter()
	env.srv.RegisterRoutes(router)
	env.srv.registerShareRoutes(router, doc)
	router.PathPrefix("/").Handler(spaHandlerDoc(sharePublicDir, http.FileServer(http.Dir(sharePublicDir)), doc))
	se := &shareEnv{terrainEnv: env, src: src, doc: doc, router: router}
	se.addNode(t, shareKeyA, "Søby Bakker", "repeater", 56.1, 10.1)
	se.addNode(t, shareKeyB, "Ørsted Mast", "companion", 56.2, 10.1)
	return se
}

func (e *shareEnv) addNode(t *testing.T, pk, name, role string, lat, lon any) {
	t.Helper()
	if _, err := e.srv.db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count)
		VALUES (?, ?, ?, ?, ?, datetime('now'), datetime('now'), 1)`, pk, name, role, lat, lon); err != nil {
		t.Fatal(err)
	}
	e.srv.store.InvalidateNodeCache()
}

func (e *shareEnv) share(target string, mod ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "http://example.test"+target, nil)
	for _, m := range mod {
		m(req)
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

func shareQ(a, b string, extra ...string) string {
	q := "/p?profile=" + url.QueryEscape(a+","+b)
	for _, x := range extra {
		q += "&" + x
	}
	return q
}

// metaContent returns the content of <meta property|name="key"> and
// whether exactly one such tag is present.
func metaContent(body, key string) (string, bool) {
	re := regexp.MustCompile(`<meta (?:property|name)="` + regexp.QuoteMeta(key) + `" content="([^"<>]*)">`)
	m := re.FindAllStringSubmatch(body, -1)
	if len(m) != 1 {
		return "", false
	}
	return m[0][1], true
}

func mustMeta(t *testing.T, body, key string) string {
	t.Helper()
	v, ok := metaContent(body, key)
	if !ok {
		t.Fatalf("no single well-formed %s tag", key)
	}
	return html.UnescapeString(v)
}

func titleOf(t *testing.T, body string) string {
	t.Helper()
	m := regexp.MustCompile(`<title>([^<]*)</title>`).FindAllStringSubmatch(body, -1)
	if len(m) != 1 {
		t.Fatalf("%d well-formed <title> elements", len(m))
	}
	return html.UnescapeString(m[0][1])
}

// siteDocument is what / serves on the same env: the reference for "this is
// the plain site document, carrying nothing from the request".
func (e *shareEnv) siteDocument(t *testing.T) string {
	t.Helper()
	w := e.share("/")
	if w.Code != 200 {
		t.Fatalf("/ status %d", w.Code)
	}
	return w.Body.String()
}

// ── The shared grammar ─────────────────────────────────────────────────────

type profileParamCase struct {
	Name  string `json:"name"`
	Input string `json:"input"`
	Valid bool   `json:"valid"`
	Refs  []struct {
		Kind   string  `json:"kind"`
		Pubkey string  `json:"pubkey"`
		Lat    float64 `json:"lat"`
		Lon    float64 `json:"lon"`
	} `json:"refs"`
}

func loadProfileParamCases(t *testing.T) []profileParamCase {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "profile-param-cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []profileParamCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) < 30 {
		t.Fatalf("only %d cases", len(f.Cases))
	}
	return f.Cases
}

// The same table drives test-issue-727-share-link.js against
// parseProfilePair, so the two parsers cannot drift apart.
func TestShareProfile_ParamFixture(t *testing.T) {
	for _, c := range loadProfileParamCases(t) {
		tq, err := parseSharedProfile(url.Values{"profile": {c.Input}})
		if (err == nil) != c.Valid {
			t.Errorf("%s: %q valid=%v, want %v", c.Name, c.Input, err == nil, c.Valid)
			continue
		}
		if !c.Valid {
			continue
		}
		for i, ref := range [2]terrainRef{tq.a, tq.b} {
			want := c.Refs[i]
			switch want.Kind {
			case "node":
				if ref.pubkey != want.Pubkey {
					t.Errorf("%s: ref %d pubkey %q, want %q", c.Name, i, ref.pubkey, want.Pubkey)
				}
			case "point":
				if ref.pubkey != "" || ref.pt.Lat != want.Lat || ref.pt.Lon != want.Lon {
					t.Errorf("%s: ref %d = %+v, want %v,%v", c.Name, i, ref, want.Lat, want.Lon)
				}
			default:
				t.Fatalf("%s: bad fixture kind %q", c.Name, want.Kind)
			}
		}
	}
}

// Everything after the pair goes through parseTerrainQuery unchanged.
func TestShareProfile_ParsesTheOtherKeysLikeTheAPI(t *testing.T) {
	q := url.Values{"profile": {shareKeyA + "," + shareKeyB}, "model": {"dtm"}, "ha": {"12.5"}, "hb": {"0"}, "k": {"1.3333"}, "freq": {"869.618"}, "v": {"abc123"}}
	tq, err := parseSharedProfile(q)
	if err != nil {
		t.Fatal(err)
	}
	if tq.both || *tq.ha != 12.5 || *tq.hb != 0 || *tq.k != 1.3333 || *tq.freq != 869.618 {
		t.Fatalf("parsed %+v", tq)
	}
	for _, bad := range []url.Values{
		{"profile": {shareKeyA + "," + shareKeyB}, "evil": {"1"}},
		{"profile": {shareKeyA + "," + shareKeyB}, "a": {shareKeyA}},
		{"profile": {shareKeyA + "," + shareKeyB, shareKeyA + "," + shareKeyB}},
		{"profile": {shareKeyA + "," + shareKeyB}, "ha": {"1", "2"}},
		{"profile": {shareKeyA + "," + shareKeyB}, "ha": {"1" + strings.Repeat("0", 64)}},
		{"profile": {shareKeyA + "," + shareKeyB}, "v": {strings.Repeat("v", 65)}},
		{"profile": {shareKeyA + "," + shareKeyB}, "ha": {"101"}},
		{"profile": {shareKeyA + "," + shareKeyB}, "k": {"1e1"}},
		{"profile": {shareKeyA + "," + shareKeyB}, "model": {"dsm"}},
		{"model": {"dtm"}},
		{},
	} {
		if _, err := parseSharedProfile(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

// ── The route ──────────────────────────────────────────────────────────────

func TestShareProfile_TagsForTwoNodes(t *testing.T) {
	env := newShareEnv(t, true, map[string]string{"siteName": "MeshView DK"})
	w := env.share(shareQ(shareKeyA, shareKeyB))
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("status %d, type %q", w.Code, w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	if got := titleOf(t, body); got != "Path profile: 11.1 km" {
		t.Fatalf("title %q", got)
	}
	for _, k := range []string{"og:title", "twitter:title"} {
		if got := mustMeta(t, body, k); got != "Path profile: 11.1 km" {
			t.Fatalf("%s %q", k, got)
		}
	}
	const wantDesc = "Søby Bakker → Ørsted Mast · terrain + surface · antenna heights assumed"
	for _, k := range []string{"og:description", "twitter:description"} {
		if got := mustMeta(t, body, k); got != wantDesc {
			t.Fatalf("%s %q, want %q", k, got, wantDesc)
		}
	}
	if got := mustMeta(t, body, "og:site_name"); got != "MeshView DK" {
		t.Fatalf("og:site_name %q: the site keeps its name", got)
	}
	if got := mustMeta(t, body, "og:url"); got != "http://example.test/p?profile="+shareKeyA+","+shareKeyB+"&model=both" {
		t.Fatalf("og:url %q", got)
	}
	if got := mustMeta(t, body, "robots"); got != "noindex" {
		t.Fatalf("robots %q", got)
	}
	// The image is still the site card until #727 M3.
	img := mustMeta(t, body, "og:image")
	if !strings.HasPrefix(img, "http://example.test/og-image.png?v=") || mustMeta(t, body, "twitter:image") != img {
		t.Fatalf("og:image %q", img)
	}
	if strings.Contains(body, "__OG_") || strings.Contains(body, "__BUST__") {
		t.Fatal("unfilled placeholder")
	}
	// The page is the site shell plus exactly two tags.
	site := env.siteDocument(t)
	if strings.Count(body, "<meta") != strings.Count(site, "<meta")+2 || strings.Count(body, "<script") != strings.Count(site, "<script") {
		t.Fatal("the per-profile document is not the site shell plus og:url and robots")
	}
	if _, ok := metaContent(site, "og:url"); ok {
		t.Fatal("og:url on a non-share page (#1890)")
	}
	if _, ok := metaContent(site, "robots"); ok {
		t.Fatal("robots noindex on a non-share page")
	}
}

// The title states the distance exactly as the panel does for the same
// query: fmtMetres over the JSON endpoint's distanceM.
func TestShareProfile_DistanceMatchesTheProfile(t *testing.T) {
	env := newShareEnv(t, true, nil)
	b := offsetPt(56.1, 10.1, 3456, 2100)
	resp := decodeProfile(t, env.get(pathQuery(shareKeyA, b)))
	if got, want := titleOf(t, env.share(shareQ(shareKeyA, b)).Body.String()), "Path profile: "+fmtMetres(resp.DistanceM); got != want || !strings.HasSuffix(got, " m") {
		t.Fatalf("title %q, want %q", got, want)
	}
}

func TestShareProfile_DescriptionClauses(t *testing.T) {
	env := newShareEnv(t, true, nil)
	pt := "56.15000,10.20000"
	for _, tc := range []struct {
		name, target, want string
	}{
		{"heights stated: no caveat", shareQ(shareKeyA, shareKeyB, "ha=12", "hb=3"), "Søby Bakker → Ørsted Mast · terrain + surface"},
		{"one height assumed", shareQ(shareKeyA, shareKeyB, "ha=12"), "Søby Bakker → Ørsted Mast · terrain + surface · antenna heights assumed"},
		{"terrain only", shareQ(shareKeyA, shareKeyB, "model=dtm", "ha=1", "hb=1"), "Søby Bakker → Ørsted Mast · terrain only"},
		{"a point has coordinates, never a name", shareQ(pt, shareKeyB, "ha=2", "hb=2"), "56.1500, 10.2000 → Ørsted Mast · terrain + surface"},
	} {
		if got := mustMeta(t, env.share(tc.target).Body.String(), "og:description"); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// og:url is rebuilt from the parsed values: keys lower-cased, numbers in
// their shortest form, the model stated, v dropped.
func TestShareProfile_OGURLIsCanonical(t *testing.T) {
	env := newShareEnv(t, true, nil)
	body := env.share(shareQ(strings.ToUpper(shareKeyA), "56.20000,10.10000", "ha=09.50", "v=abc", "freq=869.6180", "model=dtm")).Body.String()
	want := "http://example.test/p?profile=" + shareKeyA + ",56.2,10.1&model=dtm&ha=9.5&freq=869.618"
	if got := mustMeta(t, body, "og:url"); got != want {
		t.Fatalf("og:url %q, want %q", got, want)
	}
	// Opened again, the canonical link is accepted and canonical itself.
	u, _ := url.Parse(want)
	if got := mustMeta(t, env.share(u.RequestURI()).Body.String(), "og:url"); got != want {
		t.Fatalf("canonical link is not a fixed point: %q", got)
	}
}

func TestShareProfile_OGURLOrigin(t *testing.T) {
	env := newShareEnv(t, true, map[string]string{"publicBaseUrl": "https://share.example/sub/"})
	if got := mustMeta(t, env.share(shareQ(shareKeyA, shareKeyB)).Body.String(), "og:url"); !strings.HasPrefix(got, "https://share.example/sub/p?profile=") {
		t.Fatalf("og:url %q ignores publicBaseUrl", got)
	}
	plain := newShareEnv(t, true, nil)
	body := plain.share(shareQ(shareKeyA, shareKeyB), func(r *http.Request) { r.Host = `evil"><script>x` }).Body.String()
	if _, ok := metaContent(body, "og:url"); ok || strings.Contains(body, "evil") {
		t.Fatal("an unusable Host reached the document")
	}
	if mustMeta(t, body, "robots") != "noindex" {
		t.Fatal("noindex lost without an origin")
	}
}

// ── Injection ──────────────────────────────────────────────────────────────

func TestShareProfile_HostileNamesAreInert(t *testing.T) {
	env := newShareEnv(t, true, nil)
	site := env.siteDocument(t)
	long := strings.Repeat("ø", 400)
	for i, tc := range []struct{ name, label string }{
		{`" onerror=alert(1) x="`, `" onerror=alert(1) x="`},
		{`</title><script>alert(1)</script>`, `/titlescriptalert(1)/script`},
		{"Mast\nNorth\r\n\tx", "Mast North x"},
		{"A &amp; B", "A &amp; B"},
		{"\u202eevil\u2028ok\x00\x1b", "evilok"},
		{"__OG_TITLE__ __OG_ORIGIN__", "__OG_TITLE__ __OG_ORIGIN__"},
		{"中文", "中文"},
		{long, strings.Repeat("ø", shareNameMaxRunes-1) + "…"},
		{" \t ", ""}, // nothing printable: the start of the key
	} {
		pk := strings.Repeat(string("3456789abcdef"[i]), 64)
		if tc.label == "" {
			tc.label = pk[:8] + "…"
		}
		env.addNode(t, pk, tc.name, "repeater", 56.15, 10.1)
		w := env.share(shareQ(pk, shareKeyB))
		if w.Code != 200 {
			t.Fatalf("%q: status %d", tc.name, w.Code)
		}
		body := w.Body.String()
		desc := mustMeta(t, body, "og:description")
		if !strings.HasPrefix(desc, tc.label+" → Ørsted Mast") {
			t.Errorf("%q: description %q", tc.name, desc)
		}
		if utf8.RuneCountInString(desc) > ogDescriptionMaxRunes {
			t.Errorf("%q: description is %d runes", tc.name, utf8.RuneCountInString(desc))
		}
		if strings.Count(body, "<script") != strings.Count(site, "<script") || strings.Count(body, "<meta") != strings.Count(site, "<meta")+2 ||
			strings.Count(body, "<title>") != 1 || strings.Contains(body, "onerror=alert") && !strings.Contains(body, "&#34; onerror=alert(1) x=&#34;") {
			t.Errorf("%q changed the document's markup", tc.name)
		}
	}
}

func TestOGClip(t *testing.T) {
	for _, tc := range []struct {
		in   string
		max  int
		want string
	}{
		{"  a \n\t b  ", 90, "a b"},
		{"<b>x</b>", 90, "bx/b"},
		{"a\u0085b c", 90, "a b c"},
		{"x\u009by", 90, "xy"},
		{"\u2066a\u2069", 90, "a"},
		{"abcdef", 4, "abc…"},
		{"ab   cdef", 4, "ab…"},
		{"æøåæøå", 5, "æøåæ…"},
		{"abc", 3, "abc"},
	} {
		if got := ogClip(tc.in, tc.max); got != tc.want {
			t.Errorf("ogClip(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
	// Truncation happens before escaping, so an entity is never cut.
	if got := ogText(strings.Repeat("&", 10), 4); got != "&amp;&amp;&amp;…" {
		t.Fatalf("ogText %q", got)
	}
}

// ── Refusals ───────────────────────────────────────────────────────────────

// A refused request is the plain site document with the error status:
// nothing from the request, and a browser still reaches the map.
func wantSiteDocument(t *testing.T, env *shareEnv, w *httptest.ResponseRecorder, code int, what string) {
	t.Helper()
	if w.Code != code {
		t.Fatalf("%s: status %d, want %d", what, w.Code, code)
	}
	if w.Body.String() != env.siteDocument(t) {
		t.Fatalf("%s: not the plain site document", what)
	}
}

func TestShareProfile_BadRequestsAre400(t *testing.T) {
	env := newShareEnv(t, true, nil)
	far := offsetPt(56.1, 10.1, 0, 90000)
	for _, target := range []string{
		"/p",
		"/p?profile=",
		shareQ(shareKeyA, shareKeyB, "evil=1"),
		shareQ(shareKeyA, shareKeyB, "ha=1", "ha=2"),
		shareQ(shareKeyA, shareKeyB, "ha=1"+strings.Repeat("0", 64)),
		"/p?profile=" + shareKeyA + "," + shareKeyB + "," + strings.Repeat("1", 11),
		shareQ(shareKeyA, shareKeyB, "model=dsm"),
		shareQ(shareKeyA, shareKeyB, "k=NaN"),
		shareQ(shareKeyA, far),
		shareQ("56.10000,10.10000", "56.10000,10.10000"),
	} {
		wantSiteDocument(t, env, env.share(target), 400, target)
	}
}

func TestShareProfile_HiddenUnknownUnplacedAre404(t *testing.T) {
	env := newShareEnv(t, true, nil)
	unplaced, hidden, private, unknown := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64)
	env.addNode(t, unplaced, "unplaced-mast", "repeater", nil, nil)
	env.addNode(t, hidden, "blacklisted-mast", "repeater", 56.2, 10.2)
	env.addNode(t, private, "private-mast", "repeater", 56.3, 10.3)
	env.srv.cfg.SetNodeBlacklist([]string{hidden})
	env.srv.cfg.SetHiddenNamePrefixes([]string{"private"})
	for _, pk := range []string{unplaced, hidden, private, unknown} {
		for _, target := range []string{shareQ(pk, shareKeyB), shareQ(shareKeyA, strings.ToUpper(pk))} {
			w := env.share(target)
			wantSiteDocument(t, env, w, 404, target)
			if strings.Contains(w.Body.String(), "-mast") || strings.Contains(strings.ToLower(w.Body.String()), pk) {
				t.Fatal("a hidden node's name or key reached the page")
			}
		}
	}
}

func TestShareProfile_OffIsThePlainSite(t *testing.T) {
	off := newShareEnv(t, false, nil)
	wantSiteDocument(t, off, off.share(shareQ(shareKeyA, shareKeyB)), 200, "share disabled")
	if off.srv.dhmClientConfig() != nil {
		t.Fatal("test setup has a DHM proxy")
	}
	noProfile := newShareEnv(t, true, nil)
	noProfile.srv.terrain = nil
	wantSiteDocument(t, noProfile, noProfile.share(shareQ(shareKeyA, shareKeyB)), 200, "profile disabled")
	// Junk is not judged when the route is off: it is the plain site.
	wantSiteDocument(t, off, off.share("/p?evil=1"), 200, "junk with share disabled")
}

// /p needs nothing but the node snapshot: no upstream call, ever, and no
// per-client charge, even for a profile nobody has measured.
func TestShareProfile_MakesNoUpstreamCall(t *testing.T) {
	env := newShareEnv(t, true, nil)
	for i := 0; i < 25; i++ {
		if w := env.share(shareQ(shareKeyA, offsetPt(56.1, 10.1, float64(1000+i*100), 0))); w.Code != 200 {
			t.Fatalf("status %d", w.Code)
		}
	}
	if n := env.src.calls.Load(); n != 0 {
		t.Fatalf("%d upstream calls from /p", n)
	}
	if env.p.cache.Len() != 0 {
		t.Fatal("/p touched the elevation cache")
	}
}

func TestShareProfile_SlashRedirectsAndHead(t *testing.T) {
	env := newShareEnv(t, true, nil)
	q := "profile=" + url.QueryEscape(shareKeyA+","+shareKeyB) + "&ha=9"
	w := env.share("/p/?" + q)
	if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/p?"+q {
		t.Fatalf("/p/: %d → %q", w.Code, w.Header().Get("Location"))
	}
	if w := env.share("/p/"); w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/p" {
		t.Fatalf("/p/ without a query: %d → %q", w.Code, w.Header().Get("Location"))
	}
	req := httptest.NewRequest("HEAD", "http://example.test/p?"+q, nil)
	hw := httptest.NewRecorder()
	env.router.ServeHTTP(hw, req)
	if hw.Code != 200 || !strings.HasPrefix(hw.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("HEAD /p: %d", hw.Code)
	}
}

// ── The shipped index.html ─────────────────────────────────────────────────

// Every placeholder in the shipped index.html is one both renderers fill,
// and the list holds nothing the file does not use.
func TestShareProfile_ShippedPlaceholders(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(sharePublicDir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`__OG_[A-Z_]+__`).FindAllString(string(raw), -1) {
		seen[m] = true
	}
	var got []string
	for k := range seen {
		got = append(got, k)
	}
	want := append([]string(nil), ogPlaceholders...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("index.html placeholders %v, ogPlaceholders %v", got, want)
	}
	o := newOGMeta(nil, sharePublicDir, nil)
	req := httptest.NewRequest("GET", "http://example.test/p", nil)
	for name, out := range map[string]string{
		"site":    string(o.render([]byte(o.prepare(string(raw))), req)),
		"profile": string(o.renderProfile(string(raw), req, ogPage{Title: "t", Description: "d", Path: "/p?x"})),
	} {
		if strings.Contains(out, "__OG_") {
			t.Fatalf("%s document keeps a placeholder", name)
		}
	}
}

// index.html must resolve every asset the same at /p as at /: no <base>
// tag, and every script/stylesheet/icon URL either relative to the
// directory or root-absolute. A deeper share path (/p/, /share/profile)
// would break that, which is why the route is one segment.
func TestShareProfile_AssetsResolveTheSameAtP(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(sharePublicDir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`(?i)<base[\s>]`).Match(raw) {
		t.Fatal("index.html has a <base> tag")
	}
	if strings.Count(sharePath, "/") != 1 || strings.HasSuffix(sharePath, "/") {
		t.Fatalf("sharePath %q is not a single segment", sharePath)
	}
	root, _ := url.Parse("http://h/")
	share, _ := url.Parse("http://h" + sharePath)
	n := 0
	for _, m := range regexp.MustCompile(`<(?:script|link)\b[^>]*?\b(?:src|href)="([^"]+)"`).FindAllStringSubmatch(string(raw), -1) {
		ref, err := url.Parse(m[1])
		if err != nil {
			t.Fatal(err)
		}
		if a, b := root.ResolveReference(ref).String(), share.ResolveReference(ref).String(); a != b {
			t.Errorf("%q resolves to %s at / but %s at %s", m[1], a, b, sharePath)
		}
		n++
	}
	if n < 20 {
		t.Fatalf("only %d asset URLs found", n)
	}
}

// The production composition (newHTTPRouter) matches /p before the SPA
// catch-all would.
func TestShareProfile_ProductionRouter(t *testing.T) {
	env := newShareEnv(t, true, nil)
	router := newHTTPRouter(env.srv, NewHub(), sharePublicDir)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "http://example.test"+shareQ(shareKeyA, shareKeyB), nil))
	if _, ok := metaContent(w.Body.String(), "og:url"); w.Code != 200 || !ok {
		t.Fatalf("/p through newHTTPRouter: %d, og:url %v", w.Code, ok)
	}
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "http://example.test/p/", nil))
	if w.Code != http.StatusMovedPermanently {
		t.Fatalf("/p/ through newHTTPRouter: %d", w.Code)
	}
}

// ── Config ─────────────────────────────────────────────────────────────────

func TestShareProfile_ClientFlag(t *testing.T) {
	up := wcsUpstream(t, tiffAnswer)
	for _, tc := range []struct {
		name           string
		profile, share bool
		want           bool
	}{
		{"profile and share", true, true, true},
		{"profile only", true, false, false},
		{"share without the profile", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, router := setupTestServer(t)
			srv.cfg.DHM = DHMConfig{Enabled: true, Token: dhmTestToken}
			srv.dhm = newTestDHMProxy(t, up, &dhmLogSink{}, nil)
			if tc.profile {
				srv.terrain = newTerrainProfiler(dhm.ProfileConfig{Enabled: true, Share: dhm.ShareConfig{Enabled: tc.share}},
					nil, terrainProfilerDeps{src: newFakeElevation(t, flatElev(10)), logf: (&dhmLogSink{}).logf})
			}
			block := clientDHMBlock(t, router)
			if block == nil || block.Share != tc.want || block.Profile != tc.profile {
				t.Fatalf("dhm block %+v, want share %v", block, tc.want)
			}
		})
	}
}

func TestShareProfile_Settings(t *testing.T) {
	logs := &dhmLogSink{}
	if s := resolveTerrainSettings(dhm.ProfileConfig{}, logs.logf).share; s != (shareSettings{enabled: false, upstreamPerMin: 2}) {
		t.Fatalf("defaults %+v", s)
	}
	for v, want := range map[int]int{-1: 2, 61: 2, 0: 0, 1: 1, 60: 60} {
		got := resolveTerrainSettings(dhm.ProfileConfig{Share: dhm.ShareConfig{Enabled: true, UpstreamPerMin: intp(v)}}, logs.logf).share
		if got.upstreamPerMin != want || !got.enabled {
			t.Fatalf("upstreamPerMin %d → %+v, want %d", v, got, want)
		}
	}
	if !strings.Contains(logs.String(), "share.upstreamPerMin") {
		t.Fatalf("out-of-range share.upstreamPerMin not warned: %q", logs.String())
	}
	cfgJSON := `{"map":{"dhm":{"profile":{"enabled":true,"share":{"enabled":true,"upstreamPerMin":5}}}}}`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if sc := cfg.DHM.Profile.Share; !sc.Enabled || sc.UpstreamPerMin == nil || *sc.UpstreamPerMin != 5 {
		t.Fatalf("share config %+v", sc)
	}
}

// fmtMetres is the panel's rule (public/terrain-profile.js fmtMetres).
func TestFmtMetres(t *testing.T) {
	for v, want := range map[float64]string{
		0: "0 m", 12.34: "12.3 m", 9999.94: "9999.9 m", 9999.96: "10000 m",
		10000: "10 km", 11149.99: "11.1 km", 11150: "11.2 km", 80000: "80 km",
	} {
		if got := fmtMetres(v); got != want {
			t.Errorf("fmtMetres(%v) = %q, want %q", v, got, want)
		}
	}
}

// ── Perf proof ─────────────────────────────────────────────────────────────

// TestShareProfile_Cost measures one /p request end to end through the
// router (parse, resolve against the node snapshot, plan, render) next to
// the plain site document on the same env, and bounds the ratio: /p is
// hit once per shared link and preview fetch, not per page view, but it
// must stay in the same class as serving index.html. Run with -v for the
// numbers.
func TestShareProfile_Cost(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	env := newShareEnv(t, true, nil)
	run := func(target string) testing.BenchmarkResult {
		return testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if w := env.share(target); w.Code != 200 {
					b.Fatalf("status %d", w.Code)
				}
			}
		})
	}
	// An 80 km path: the longest allowed, so the most samples to plan.
	share := run(shareQ(shareKeyA, offsetPt(56.1, 10.1, 0, 79000), "ha=12", "hb=3"))
	site := run("/")
	t.Logf("/p: %d ns/op, %d B/op, %d allocs/op", share.NsPerOp(), share.AllocedBytesPerOp(), share.AllocsPerOp())
	t.Logf("/ : %d ns/op, %d B/op, %d allocs/op", site.NsPerOp(), site.AllocedBytesPerOp(), site.AllocsPerOp())
	if share.NsPerOp() > 20*site.NsPerOp()+2_000_000 {
		t.Fatalf("/p costs %d ns, the site document %d ns", share.NsPerOp(), site.NsPerOp())
	}
}
