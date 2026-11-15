package main

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/meshcore-analyzer/buildings"
	"github.com/meshcore-analyzer/copdem"
	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/heritage"
	"github.com/meshcore-analyzer/mastedb"
	"github.com/meshcore-analyzer/skraafoto"
)

// No test here reaches the network: every request goes through fake's
// transport to an httptest server, and no real token is used.

const testToken = "SENTINEL-smoke-token-803"

// seen is one request the fake upstream answered.
type seen struct {
	host, path, rawQuery string
	header               http.Header
}

// fake answers every upstream the smoke check calls, by host and path,
// with a body of the right shape unless override answers first.
type fake struct {
	srv      *httptest.Server
	mu       sync.Mutex
	log      []seen
	override func(w http.ResponseWriter, r *http.Request) bool
}

func newFake(t *testing.T) *fake {
	f := &fake{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// transport sends every https request to the fake, keeping the original
// host in Host so the fake can tell the upstreams apart.
func (f *fake) transport() http.RoundTripper {
	target, _ := url.Parse(f.srv.URL)
	inner := f.srv.Client().Transport
	return roundTrip(func(r *http.Request) (*http.Response, error) {
		out := r.Clone(r.Context())
		out.URL.Scheme, out.URL.Host = target.Scheme, target.Host
		out.Host = r.URL.Host
		return inner.RoundTrip(out)
	})
}

type roundTrip func(*http.Request) (*http.Response, error)

func (fn roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func (f *fake) requests() []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seen(nil), f.log...)
}

func tile(t testing.TB, format string) []byte {
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 256, 256))
	var err error
	if format == "png" {
		err = png.Encode(&buf, img)
	} else {
		err = jpeg.Encode(&buf, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// searchID and searchPath are the photo the fake search finds, in the href
// shape the live STAC API returns (#819): on CDNHost, under a resolution/
// year/10 km/1 km path.
const (
	searchID   = "2025_82_24_2_0138_00000126_100mm"
	searchPath = "/skraafoto_server/100mm/2025/10km_622_57/1km_6223_575/" + searchID + ".tif"
)

const searchAnswer = `{"type":"FeatureCollection","features":[{"type":"Feature","id":"` + searchID + `",` +
	`"collection":"skraafotos2025","properties":{"direction":"north","datetime":"2025-04-01T10:00:00Z"},` +
	`"assets":{"data":{"href":"` + skraafoto.CDNBase + searchPath + `"}}}]}`

func writeRange(w http.ResponseWriter, r *http.Request, size int) {
	var from, to int
	if _, err := fmtSscanRange(r.Header.Get("Range"), &from, &to); err != nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	body := make([]byte, to-from+1)
	copy(body, "II*\x00")
	w.Header().Set("Content-Type", "image/tiff")
	w.Header().Set("Content-Range", "bytes "+strconv.Itoa(from)+"-"+strconv.Itoa(to)+"/"+strconv.Itoa(size))
	w.WriteHeader(http.StatusPartialContent)
	w.Write(body)
}

func fmtSscanRange(h string, from, to *int) (int, error) {
	a, b, ok := strings.Cut(strings.TrimPrefix(h, "bytes="), "-")
	var err error
	if *from, err = strconv.Atoi(a); err != nil || !ok {
		return 0, http.ErrNotSupported
	}
	if *to, err = strconv.Atoi(b); err != nil {
		return 0, err
	}
	return 2, nil
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.log = append(f.log, seen{host: r.Host, path: r.URL.Path, rawQuery: r.URL.RawQuery, header: r.Header.Clone()})
	f.mu.Unlock()
	if f.override != nil && f.override(w, r) {
		return
	}
	answer := func(ct string, body []byte) {
		w.Header().Set("Content-Type", ct)
		w.Write(body)
	}
	switch {
	case r.Host == "api.dataforsyningen.dk" && (r.URL.Path == "/"+dhm.WMSService || r.URL.Path == "/"+dhm.ExaggeratedWMSService):
		answer("image/png", tile(nil, "png"))
	case r.Host == "api.dataforsyningen.dk" && r.URL.Path == "/"+dhm.OrthoWMTSService:
		answer("image/jpeg", tile(nil, "jpeg"))
	case r.Host == "api.dataforsyningen.dk" && r.URL.Path == "/"+dhm.WCSService:
		answer("image/tiff", append([]byte("II*\x00"), make([]byte, 64)...))
	case r.Host == "api.dataforsyningen.dk" && strings.HasPrefix(r.URL.Path, "/ogcapi/"):
		answer("application/json", []byte(`{"type":"FeatureCollection","features":[{"type":"Feature"}]}`))
	case r.Host == "api.dataforsyningen.dk" && strings.HasPrefix(r.URL.Path, "/rest/skraafoto_api/"):
		answer("application/geo+json", []byte(searchAnswer))
	case r.Host == "www.kulturarv.dk":
		answer("application/json;charset=UTF-8", []byte(`{"type":"FeatureCollection","features":[]}`))
	case r.Host == "mastedatabasen.dk":
		answer("application/gml+xml", []byte(`<?xml version="1.0"?><wfs:FeatureCollection xmlns:wfs="http://www.opengis.net/wfs"></wfs:FeatureCollection>`))
	case r.Host == copdem.Host:
		writeRange(w, r, 1<<20)
	case r.Host == skraafoto.CDNHost:
		writeRange(w, r, 300<<20)
	default:
		http.NotFound(w, r)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func runSmoke(t *testing.T, f *fake, args ...string) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut, f.transport())
	return code, out.String() + errOut.String()
}

// resultLine is the output line of the probe named name.
func resultLine(t *testing.T, out, name string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, " "+name+" ") || strings.HasSuffix(l, " "+name) {
			return l
		}
	}
	t.Fatalf("no line for %q in\n%s", name, out)
	return ""
}

var tokenProbes = []string{
	"dhm-wms-terrain-exaggerated", "dhm-wms-terrain", "dhm-wms-surface", "dhm-wms-contours",
	"dhm-wcs-dtm", "dhm-wcs-dsm", "dhm-ortho-wmts", "skraafoto-stac-search",
}

var tokenlessProbes = []string{
	"buildings-inspire-ogcapi", "heritage-wfs", "mastedb-wfs", "copdem-cog-range",
}

// testCDNPath is a photo path for -skraafoto-cdn-path.
const testCDNPath = "/skraafoto_server/100mm/2025/10km_614_57/1km_6145_573/2025_83_36_1_0020_00003045_100mm.tif"

// Without a config, every probe that needs the token is SKIP, nothing is
// sent for it, and the tokenless probes still run: the exit is 0.
func TestNoTokenSkipsTokenProbesAndSendsNothingForThem(t *testing.T) {
	f := newFake(t)
	code, out := runSmoke(t, f)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	// The CDN read has no photo without the search or -skraafoto-cdn-path.
	if l := resultLine(t, out, "skraafoto-cdn-range"); !strings.HasPrefix(l, "SKIP") || !strings.Contains(l, "-skraafoto-cdn-path") {
		t.Errorf("skraafoto-cdn-range: %q", l)
	}
	for _, n := range tokenProbes {
		if l := resultLine(t, out, n); !strings.HasPrefix(l, "SKIP") || !strings.Contains(l, "map.dhm.token") {
			t.Errorf("%s: %q", n, l)
		}
	}
	for _, n := range tokenlessProbes {
		if l := resultLine(t, out, n); !strings.HasPrefix(l, "PASS") {
			t.Errorf("%s: %q", n, l)
		}
	}
	for _, s := range f.requests() {
		if len(s.header.Values(dhm.TokenHeader)) != 0 {
			t.Errorf("token header sent to %s%s", s.host, s.path)
		}
		if s.host == "api.dataforsyningen.dk" && !strings.HasPrefix(s.path, "/ogcapi/") {
			t.Errorf("token-only service called without a token: %s", s.path)
		}
	}
}

// Without a token, -skraafoto-cdn-path names the photo the CDN read asks
// for, through skraafoto.CDNURL; a path off its allow-list fails.
func TestCDNPathFlag(t *testing.T) {
	f := newFake(t)
	code, out := runSmoke(t, f, "-skraafoto-cdn-path", testCDNPath)
	if l := resultLine(t, out, "skraafoto-cdn-range"); code != 0 || !strings.HasPrefix(l, "PASS") || !strings.Contains(l, testCDNPath) {
		t.Fatalf("exit %d: %q", code, l)
	}
	code, out = runSmoke(t, f, "-skraafoto-cdn-path", "/elsewhere/x.tif")
	if l := resultLine(t, out, "skraafoto-cdn-range"); code != 1 || !strings.HasPrefix(l, "FAIL") {
		t.Fatalf("exit %d: %q", code, l)
	}
}

// A config whose map.dhm has no token is the same as no config.
func TestEmptyTokenSkips(t *testing.T) {
	f := newFake(t)
	code, out := runSmoke(t, f, "-config", writeConfig(t, `{"map":{"dhm":{"enabled":true,"token":"  "}}}`))
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if l := resultLine(t, out, "dhm-wcs-dtm"); !strings.HasPrefix(l, "SKIP") {
		t.Fatalf("%q", l)
	}
}

// With the token, every probe runs, the token goes in the header only of
// the requests that need it, and it is never printed.
func TestTokenIsSentOnlyWhereNeededAndNeverPrinted(t *testing.T) {
	f := newFake(t)
	code, out := runSmoke(t, f, "-config", writeConfig(t, `{"map":{"dhm":{"enabled":true,"token":"`+testToken+`"}}}`),
		"-skraafoto-cdn-path", testCDNPath)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, n := range append(append([]string(nil), tokenProbes...), append(tokenlessProbes, "skraafoto-cdn-range")...) {
		if l := resultLine(t, out, n); !strings.HasPrefix(l, "PASS") {
			t.Errorf("%s: %q", n, l)
		}
	}
	if strings.Contains(out, testToken) {
		t.Fatalf("token printed:\n%s", out)
	}
	var withToken int
	for _, s := range f.requests() {
		tok := s.header.Values(dhm.TokenHeader)
		needs := s.host == "api.dataforsyningen.dk" && !strings.HasPrefix(s.path, "/ogcapi/")
		if needs && (len(tok) != 1 || tok[0] != testToken) || !needs && len(tok) != 0 {
			t.Errorf("%s%s: token header %q", s.host, s.path, tok)
		}
		if len(tok) != 0 {
			withToken++
		}
	}
	if withToken != len(tokenProbes) {
		t.Fatalf("%d requests with the token, want %d", withToken, len(tokenProbes))
	}
	// The CDN read is the photo the search found.
	if l := resultLine(t, out, "skraafoto-cdn-range"); !strings.Contains(l, "url="+skraafoto.CDNBase+searchPath) {
		t.Fatalf("CDN read not the searched photo: %q", l)
	}
}

// An upstream that echoes the token back (an error page, a redirect
// target) cannot get it printed either.
func TestEchoedTokenIsRedacted(t *testing.T) {
	f := newFake(t)
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/"+dhm.WCSService {
			return false
		}
		w.Header().Set("Content-Type", "text/plain; token="+r.Header.Get(dhm.TokenHeader))
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(r.Header.Get(dhm.TokenHeader)))
		return true
	}
	code, out := runSmoke(t, f, "-config", writeConfig(t, `{"map":{"dhm":{"token":"`+testToken+`"}}}`))
	if code != 1 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if l := resultLine(t, out, "dhm-wcs-dtm"); !strings.HasPrefix(l, "FAIL") || !strings.Contains(l, "403") {
		t.Fatalf("%q", l)
	}
	if strings.Contains(out, testToken) {
		t.Fatalf("token printed:\n%s", out)
	}
}

// One failing upstream fails the run: exit 1, and its line says FAIL.
func TestAFailureExitsNonZero(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter, r *http.Request) bool{
		"heritage-wfs": func(w http.ResponseWriter, r *http.Request) bool {
			if r.Host != "www.kulturarv.dk" {
				return false
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			return true
		},
		"buildings-inspire-ogcapi": func(w http.ResponseWriter, r *http.Request) bool {
			if !strings.HasPrefix(r.URL.Path, "/ogcapi/") {
				return false
			}
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(`<ows:ExceptionReport/>`))
			return true
		},
		"copdem-cog-range": func(w http.ResponseWriter, r *http.Request) bool {
			if r.Host != copdem.Host {
				return false
			}
			w.Write(make([]byte, 100)) // 200, the range ignored
			return true
		},
	}
	for name, override := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.override = override
			code, out := runSmoke(t, f)
			if code != 1 {
				t.Fatalf("exit %d\n%s", code, out)
			}
			if l := resultLine(t, out, name); !strings.HasPrefix(l, "FAIL") {
				t.Fatalf("%q", l)
			}
		})
	}
}

// An unreadable or malformed config stops the run (exit 2) without
// printing the file's content.
func TestBadConfigExitsTwoWithoutItsContent(t *testing.T) {
	f := newFake(t)
	code, out := runSmoke(t, f, "-config", filepath.Join(t.TempDir(), "missing.json"))
	if code != 2 {
		t.Fatalf("missing: exit %d\n%s", code, out)
	}
	code, out = runSmoke(t, f, "-config", writeConfig(t, `{"map":{"dhm":{"token":"`+testToken+`",}}}`))
	if code != 2 || strings.Contains(out, testToken) {
		t.Fatalf("malformed: exit %d\n%s", code, out)
	}
	if len(f.requests()) != 0 {
		t.Fatalf("%d requests after a config error", len(f.requests()))
	}
}

// The token is read the way the server reads map.dhm: the exact key only.
func TestLoadTokens(t *testing.T) {
	cases := []struct {
		body, want string
	}{
		{`{"map":{"dhm":{"enabled":true,"token":" abc "}}}`, "abc"},
		{`{"map":{"dhm":{"enabled":false,"token":"abc"}}}`, "abc"},
		{`{"map":{"DHM":{"token":"abc"}}}`, ""},
		{`{"map":{}}`, ""},
		{`{}`, ""},
	}
	for _, c := range cases {
		toks, err := loadTokens(writeConfig(t, c.body))
		if err != nil {
			t.Fatalf("%s: %v", c.body, err)
		}
		if got := toks[tokenDHM]; got != c.want {
			t.Errorf("%s: token %q, want %q", c.body, got, c.want)
		}
	}
	if _, err := loadTokens(writeConfig(t, `{"map":{"dhm":{"token":["leak-me"]}}}`)); err == nil || strings.Contains(err.Error(), "leak-me") {
		t.Fatalf("malformed map.dhm: %v", err)
	}
}

func TestRedact(t *testing.T) {
	secret := "s3cr/et+tok en"
	r := newRedactor(secret, "")
	in := []string{
		"https://h/x?a=1&key=" + url.QueryEscape(secret),
		"https://h/x?q=" + url.QueryEscape(secret) + "&x=1",
		"https://h/" + url.PathEscape(secret) + "/x",
		"error: " + secret,
		"https://h/x?token=other&x=1",
		"https://h/x?a=2&api_key=k2#f",
		"https://h/x?access_token=k3&apikey=k4",
		"https://h/x?TOKEN=k5",
	}
	for _, s := range in {
		got := r.redact(s)
		for _, leak := range []string{secret, url.QueryEscape(secret), url.PathEscape(secret), "other", "k2", "k3", "k4", "k5"} {
			if strings.Contains(got, leak) {
				t.Errorf("%q -> %q leaks %q", s, got, leak)
			}
		}
		if !strings.Contains(got, redacted) {
			t.Errorf("%q -> %q: nothing redacted", s, got)
		}
	}
	if got := r.redact("https://h/x?a=1&x=2"); got != "https://h/x?a=1&x=2" {
		t.Fatalf("clean URL changed: %q", got)
	}
}

func reply206(from, to, size int, body []byte) *reply {
	h := http.Header{}
	h.Set("Content-Range", "bytes "+strconv.Itoa(from)+"-"+strconv.Itoa(to)+"/"+strconv.Itoa(size))
	return &reply{status: 206, contentType: "application/octet-stream", header: h, body: body}
}

func TestShapeChecks(t *testing.T) {
	png256, jpg256 := tile(t, "png"), tile(t, "jpeg")
	tiff := append([]byte("MM\x00*"), make([]byte, 12)...)
	rng := dhm.Range{From: 0, To: 15, MaxBody: 16}
	cases := []struct {
		name  string
		check func(*reply) (string, error)
		r     *reply
		ok    bool
	}{
		{"png", checkImage("png", 256), &reply{contentType: "image/png", body: png256}, true},
		{"png wrong type", checkImage("png", 256), &reply{contentType: "text/xml", body: png256}, false},
		{"png is jpeg", checkImage("png", 256), &reply{contentType: "image/png", body: jpg256}, false},
		{"png wrong size", checkImage("png", 512), &reply{contentType: "image/png", body: png256}, false},
		{"png truncated", checkImage("png", 256), &reply{contentType: "image/png", body: png256[:20]}, false},
		{"jpeg", checkImage("jpeg", 256), &reply{contentType: "image/jpeg", body: jpg256}, true},
		{"tiff", checkTIFF, &reply{contentType: "image/tiff", body: tiff}, true},
		{"tiff little-endian", checkTIFF, &reply{contentType: "image/geotiff", body: []byte("II*\x00....")}, true},
		{"tiff xml", checkTIFF, &reply{contentType: "image/tiff", body: []byte("<ServiceExceptionReport/>")}, false},
		{"features", checkFeatures, &reply{contentType: "application/geo+json", body: []byte(`{"features":[{},{}]}`)}, true},
		{"features empty", checkFeatures, &reply{contentType: "application/json; charset=UTF-8", body: []byte(`{"features":[]}`)}, true},
		{"features missing", checkFeatures, &reply{contentType: "application/json", body: []byte(`{"type":"x"}`)}, false},
		{"features null", checkFeatures, &reply{contentType: "application/json", body: []byte(`{"features":null}`)}, false},
		{"features not json", checkFeatures, &reply{contentType: "application/json", body: []byte(`<html>`)}, false},
		{"features html type", checkFeatures, &reply{contentType: "text/html", body: []byte(`{"features":[]}`)}, false},
		{"gml", checkGML, &reply{contentType: "application/gml+xml", body: []byte(`<?xml version="1.0"?><wfs:FeatureCollection xmlns:wfs="w"/>`)}, true},
		{"gml exception", checkGML, &reply{contentType: "text/xml", body: []byte(`<ows:ExceptionReport xmlns:ows="o"/>`)}, false},
		{"gml json", checkGML, &reply{contentType: "application/json", body: []byte(`{}`)}, false},
		{"range", checkRange(rng, checkTIFF), reply206(0, 15, 99, tiff), true},
		{"range shorter object", checkRange(rng, checkTIFF), reply206(0, 9, 10, tiff[:10]), true},
		{"range no header", checkRange(rng, checkTIFF), &reply{status: 206, header: http.Header{}, body: tiff}, false},
		{"range wrong start", checkRange(rng, checkTIFF), reply206(1, 15, 99, tiff[:15]), false},
		{"range wrong end", checkRange(rng, checkTIFF), reply206(0, 14, 99, tiff[:15]), false},
		{"range short body", checkRange(rng, checkTIFF), reply206(0, 15, 99, tiff[:8]), false},
		{"range not tiff", checkRange(rng, checkTIFF), reply206(0, 15, 99, make([]byte, 16)), false},
	}
	for _, c := range cases {
		note, err := c.check(c.r)
		if (err == nil) != c.ok {
			t.Errorf("%s: note %q err %v, want ok=%v", c.name, note, err, c.ok)
		}
	}
}

// A status other than the expected one fails before the shape check.
func TestStatusIsCheckedFirst(t *testing.T) {
	f := newFake(t)
	p := probe{name: "x", url: func() (string, error) { return "https://www.kulturarv.dk/x", nil }, status: 200, check: checkFeatures}
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"features":[]}`))
		return true
	}
	res := execute(context.Background(), newHTTPClient(f.transport(), 0), p, nil)
	if res.verdict != fail || !strings.Contains(res.err, "202") {
		t.Fatalf("%+v", res)
	}
}

// Every request URL is the client's own builder, on its production base:
// the smoke check sends exactly what the server sends.
func TestProbeURLsAreTheClientBuilders(t *testing.T) {
	s := defaultSettings()
	s.cdnPath = testCDNPath
	ps := buildProbes(s)
	urls := map[string]string{}
	for _, p := range ps {
		if p.url == nil {
			continue
		}
		u, err := p.url()
		if err != nil {
			t.Fatalf("%s: %v", p.name, err)
		}
		urls[p.name] = u
	}
	e, n := dhm.ToUTM32(site.Lat, site.Lon)
	k := siteTile()
	if !k.IntersectsCoverage() {
		t.Fatalf("tile %v outside DHM coverage", k)
	}
	exag, _ := dhm.LookupOverlay("terrain")
	exag = exag.Resolve(dhm.HillshadeExaggerated)
	hc, _ := heritage.CellOf(e, n)
	hcli, err := heritage.NewClient(heritage.DefaultClientOptions())
	if err != nil {
		t.Fatal(err)
	}
	mc := mastedb.Cell{E: int(e) / mastedb.CellSizeM, N: int(n) / mastedb.CellSizeM}
	ct, _ := copdem.TileAt(site.Lat, site.Lon)
	search, err := skraafoto.SearchURL(s.year, int(e), int(n), skraafoto.North)
	if err != nil {
		t.Fatal(err)
	}
	cdn, err := skraafoto.CDNURL(s.cdnPath)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"dhm-wms-terrain-exaggerated": dhm.RequestURL(dhm.DefaultBaseURL, exag.Service, k.OverlayQuery(exag)),
		"dhm-ortho-wmts":              dhm.RequestURL(dhm.DefaultBaseURL, dhm.OrthoWMTSService, k.OrthoQuery()),
		"heritage-wfs":                hcli.CellURL(hc),
		"mastedb-wfs":                 mastedb.NewClient(mastedb.DefaultClientOptions()).CellURL(mc),
		"buildings-inspire-ogcapi":    buildings.NewClient(buildings.DefaultClientOptions()).SegmentURL(siteSegment()),
		"copdem-cog-range":            ct.URL(),
		"skraafoto-stac-search":       search,
		"skraafoto-cdn-range":         cdn,
	}
	for name, w := range want {
		if urls[name] != w {
			t.Errorf("%s\n got %s\nwant %s", name, urls[name], w)
		}
	}
	for _, name := range append(append([]string(nil), tokenProbes...), append(tokenlessProbes, "skraafoto-cdn-range")...) {
		if _, ok := urls[name]; !ok {
			t.Errorf("no probe %s", name)
		}
	}
}

// Skærmkort has no Go request builder (#741 builds its WMS URL in the
// browser), so it is listed as SKIP with that reason, never as a
// hand-written URL. #741 is merged (#805), so the reason must not say it
// is pending.
func TestSkaermkortIsSkippedWithoutABuilder(t *testing.T) {
	f := newFake(t)
	code, out := runSmoke(t, f, "-config", writeConfig(t, `{"map":{"dhm":{"token":"`+testToken+`"}}}`))
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if l := resultLine(t, out, "skaermkort-wms"); !strings.HasPrefix(l, "SKIP") || !strings.Contains(l, "#741") ||
		!strings.Contains(l, "built in the browser") || strings.Contains(l, "not merged") {
		t.Fatalf("%q", l)
	}
	for _, s := range f.requests() {
		if strings.Contains(strings.ToLower(s.rawQuery), "skaermkort") {
			t.Fatalf("Skærmkort requested: %s", s.rawQuery)
		}
	}
}
