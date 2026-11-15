package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/dhm/dhmtest"
)

// #792 — the colour-by-elevation overlay: the proxy serves the id
// "elevation" by rendering the WCS DTM, with the token, budget,
// admission, coalescing, coverage gate and cache every DHM overlay
// shares. Every test runs against a fake WCS (httptest); none reaches
// Dataforsyningen, and no real token is used.

// newWCSUpstream is the DHM WCS faked: a GetCoverage answered with a
// DEFLATE GeoTIFF of the requested size, every cell `height`. Any other
// path is stray (404).
func newWCSUpstream(t *testing.T, height float32, before func()) *fakeUpstream {
	t.Helper()
	return newWCSUpstreamFunc(t, func(int, int, int, int) float32 { return height }, before)
}

// newWCSUpstreamFunc is newWCSUpstream with cell (col, row) of a w × h
// raster at height f(col, row, w, h).
func newWCSUpstreamFunc(t *testing.T, f func(col, row, w, h int) float32, before func()) *fakeUpstream {
	t.Helper()
	return newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if before != nil {
			before()
		}
		q := r.URL.Query()
		if r.URL.Path != "/"+dhm.WCSService || q.Get("coverage") != dhm.TerrainCoverage {
			w.WriteHeader(404)
			return
		}
		wd, _ := strconv.Atoi(q.Get("width"))
		ht, _ := strconv.Atoi(q.Get("height"))
		vals := make([]float32, wd*ht)
		for i := range vals {
			vals[i] = f(i%wd, i/wd, wd, ht)
		}
		w.Header().Set("Content-Type", "image/tiff")
		w.Write(dhmtest.GeoTIFF(wd, ht, vals, nil, true))
	})
}

func newElevationProxy(t *testing.T, up *fakeUpstream, logs *dhmLogSink, tweak func(*dhmProxyOptions)) *dhmProxy {
	t.Helper()
	return newTestDHMProxyLayer(t, up, "", logs, tweak)
}

// elevationColour decodes a served tile and returns the colour at its
// centre; the tile must be a 256×256 PNG.
func elevationColour(t *testing.T, w *httptest.ResponseRecorder) color.NRGBA {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(w.Body.Bytes()))
	if err != nil || img.Bounds() != image.Rect(0, 0, 256, 256) {
		t.Fatalf("not a 256x256 PNG (%v)", err)
	}
	return color.NRGBAModel.Convert(img.At(128, 128)).(color.NRGBA)
}

func TestDHM792_ServesElevationRenderedFromTheWCS(t *testing.T) {
	up := newWCSUpstream(t, 100, nil)
	logs := &dhmLogSink{}
	p := newElevationProxy(t, up, logs, nil)
	q := overlayTile("elevation", 13, 0)
	w := doTile(p, q)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("X-CoreScope-Tile") != "miss" ||
		w.Header().Get("Cache-Control") != ccTile {
		t.Fatalf("status %d headers %v", w.Code, w.Header())
	}
	// 100 m is the ramp's stop #d9a96a.
	if got := elevationColour(t, w); got != (color.NRGBA{0xd9, 0xa9, 0x6a, 255}) {
		t.Fatalf("centre %v, want the 100 m colour", got)
	}
	if up.hits.Load() != 1 {
		t.Fatalf("%d upstream calls", up.hits.Load())
	}
	// The one call is the exact allow-listed GetCoverage, the token in
	// its header only.
	got := up.queries[0]
	for k, v := range map[string]string{"service": "WCS", "version": "1.0.0", "request": "GetCoverage",
		"coverage": "dhm_terraen", "crs": "EPSG:25832", "response_crs": "EPSG:25832", "format": "GTiff",
		"width": "128", "height": "128"} {
		if got.Get(k) != v {
			t.Errorf("upstream %s = %q, want %q", k, got.Get(k), v)
		}
	}
	if len(got) != 10 { // and bbox
		t.Errorf("upstream query has %d keys: %v", len(got), got)
	}
	if up.tokens[0] != dhmTestToken {
		t.Errorf("token header %q", up.tokens[0])
	}
	assertNoToken(t, "miss", w, logs)
	// The rendered tile is cached under its own product: a hit, no call.
	w2 := doTile(p, q)
	if w2.Header().Get("X-CoreScope-Tile") != "hit" || !bytes.Equal(w2.Body.Bytes(), w.Body.Bytes()) || up.hits.Load() != 1 {
		t.Fatalf("second request: %s, %d calls", w2.Header().Get("X-CoreScope-Tile"), up.hits.Load())
	}
	if _, ok := p.cache.Get(dhmCacheKey{Product: dhmProductElevation, Tile: dhm.TileKey{Z: 13, X: mustX(13), Y: mustY(13)}}, p.now()); !ok {
		t.Error("not cached as the elevation product")
	}
	if logs.String() != "" {
		t.Errorf("log: %q", logs.String())
	}
}

func mustX(z int) int { x, _ := lonLatTile(z, 10.2039, 56.1629); return x }
func mustY(z int) int { _, y := lonLatTile(z, 10.2039, 56.1629); return y }

// The id's zoom range is the table's, z9–13; every other zoom, the
// coverage name and case variants are refused before any upstream call.
func TestDHM792_AllowListAndZoomRange(t *testing.T) {
	up := newWCSUpstream(t, 40, nil)
	logs := &dhmLogSink{}
	p := newElevationProxy(t, up, logs, nil)
	for z := 7; z <= 19; z++ {
		w := doTile(p, overlayTile("elevation", z, 0))
		if want := z >= 9 && z <= 13; (w.Code == 200) != want {
			t.Errorf("z%d: status %d", z, w.Code)
		}
	}
	set := func(v string) func(q map[string][]string) {
		return func(q map[string][]string) { q["layers"] = []string{v} }
	}
	for _, l := range []string{"dhm_terraen", "dhm_wcs_DAF", "Elevation", "elevation,terrain", "elevation "} {
		q := overlayTile("elevation", 11, 0)
		set(l)(q)
		if w := doTile(p, q); w.Code != http.StatusBadRequest {
			t.Errorf("layers=%q: status %d", l, w.Code)
		}
	}
	if up.hits.Load() != 5 {
		t.Errorf("%d upstream calls, want one per zoom 9..13", up.hits.Load())
	}
	// Outside Denmark: transparent, cached a day, no upstream call.
	q := leafletQuery(9, 100, 160)
	q.Set("layers", "elevation")
	w := doTile(p, q)
	assertTile(t, "outside", w, dhmTransparentPNG, "outside", ccTile)
	if up.hits.Load() != 5 {
		t.Error("an outside tile reached upstream")
	}
	assertNoToken(t, "allow-list", w, logs)
}

// One budget, one admission bound and one flight for every overlay: an
// elevation miss is charged, a hit is not, identical tiles share one WCS
// call, and a full admission answers busy and refunds the charge.
func TestDHM792_SharesBudgetAdmissionAndCoalescing(t *testing.T) {
	t.Run("budget", func(t *testing.T) {
		up := newWCSUpstream(t, 40, nil)
		p := newElevationProxy(t, up, &dhmLogSink{}, func(o *dhmProxyOptions) { o.rateLimitPerMin = 2 })
		do := func(q map[string][]string) int {
			w := httptest.NewRecorder()
			p.ServeHTTP(w, overlayRequest(q, "203.0.113.9:1"))
			return w.Code
		}
		if do(overlayTile("elevation", 12, 0)) != 200 || do(overlayTile("elevation", 12, 0)) != 200 {
			t.Fatal("miss then hit refused")
		}
		if do(overlayTile("elevation", 12, 1)) != 200 {
			t.Fatal("second miss refused")
		}
		// The budget is shared: a hillshade miss now finds it spent.
		if c := do(overlayTile("surface", 12, 0)); c != http.StatusTooManyRequests {
			t.Fatalf("third miss across overlays: %d, want 429", c)
		}
		if do(overlayTile("elevation", 12, 0)) != 200 {
			t.Fatal("a cached tile was charged")
		}
	})
	t.Run("coalescing", func(t *testing.T) {
		up := newWCSUpstream(t, 40, func() { time.Sleep(80 * time.Millisecond) })
		p := newElevationProxy(t, up, &dhmLogSink{}, nil)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); doTile(p, overlayTile("elevation", 11, 0)) }()
		}
		wg.Wait()
		if up.hits.Load() != 1 {
			t.Fatalf("8 concurrent requests made %d WCS calls", up.hits.Load())
		}
	})
	t.Run("admission", func(t *testing.T) {
		release := make(chan struct{})
		up := newWCSUpstream(t, 40, func() { <-release })
		logs := &dhmLogSink{}
		p := newElevationProxy(t, up, logs, func(o *dhmProxyOptions) { o.tileMaxInFlight = 1; o.rateLimitPerMin = 2 })
		held := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			w := httptest.NewRecorder()
			p.ServeHTTP(w, overlayRequest(overlayTile("elevation", 12, 0), "203.0.113.9:1"))
			held <- w
		}()
		for p.admit.InUse() != 1 {
			time.Sleep(time.Millisecond)
		}
		w := httptest.NewRecorder()
		p.ServeHTTP(w, overlayRequest(overlayTile("elevation", 12, 1), "203.0.113.9:1"))
		assertBusyTile(t, w)
		close(release)
		if w := <-held; w.Code != 200 || w.Header().Get("X-CoreScope-Tile") != "miss" {
			t.Fatalf("held tile: %d %s", w.Code, w.Header().Get("X-CoreScope-Tile"))
		}
		// The busy tile's charge came back: one more miss fits the budget of 2.
		w = httptest.NewRecorder()
		p.ServeHTTP(w, overlayRequest(overlayTile("elevation", 12, 2), "203.0.113.9:1"))
		if w.Code != 200 {
			t.Fatalf("after the refund: %d", w.Code)
		}
		if !strings.Contains(logs.String(), "[dhm] elevation tile refused: admission full") {
			t.Errorf("log %q", logs.String())
		}
	})
}

// An upstream failure is a transparent no-store tile, logged by category
// as an elevation tile, with no fallback and no token anywhere.
func TestDHM792_UpstreamFailureIsUnavailable(t *testing.T) {
	for _, c := range []struct {
		name   string
		h      func(w http.ResponseWriter, r *http.Request)
		reason string
	}{
		{"status", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			fmt.Fprint(w, "upstream says "+dhmTestToken)
		}, "status 500"},
		{"not a tiff", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/tiff")
			w.Write([]byte("<ServiceExceptionReport/>"))
		}, "invalid raster"},
		{"xml", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.ogc.se_xml")
			w.Write([]byte("<ServiceExceptionReport/>"))
		}, "unexpected content type"},
	} {
		t.Run(c.name, func(t *testing.T) {
			up := newFakeUpstream(t, c.h)
			logs := &dhmLogSink{}
			p := newElevationProxy(t, up, logs, nil)
			w := doTile(p, overlayTile("elevation", 10, 0))
			assertTile(t, c.name, w, dhmTransparentPNG, "unavailable", ccNoStore)
			if up.hits.Load() != 1 {
				t.Errorf("%d upstream calls (a fallback?)", up.hits.Load())
			}
			if want := "[dhm] upstream elevation tile failed (" + c.reason + ")"; !strings.Contains(logs.String(), want) {
				t.Errorf("log %q, want %q", logs.String(), want)
			}
			assertNoToken(t, c.name, w, logs)
		})
	}
}

// /api/config/client offers the elevation id with its zoom range, the
// DHM/Terræn dataset, the derived credit ("data bearbejdet") and the ramp
// metres for the legend. The other entries are unchanged on the wire, and
// nothing upstream or secret is in the block.
func TestDHM792_ClientConfigOffersElevation(t *testing.T) {
	var got *DHMOverlayClientConfig
	for _, e := range dhmClientConfig().Overlays {
		e := e
		if e.ID == "elevation" {
			got = &e
		}
	}
	if got == nil {
		t.Fatal("elevation is not offered")
	}
	want := DHMOverlayClientConfig{ID: "elevation", MinZoom: 9, MaxZoom: 13,
		DatasetName: "Danmarks Højdemodel", DatasetURL: "https://dataforsyningen.dk/data/930",
		Attribution: dhm.AttributionDerived, LegendM: []int{0, 25, 50, 75, 100, 125, 150, 175}}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("entry %+v, want %+v", *got, want)
	}
	if !strings.HasSuffix(got.Attribution, " — data bearbejdet") || !strings.HasPrefix(got.Attribution, dhm.Attribution) {
		t.Errorf("credit %q", got.Attribution)
	}
	raw, _ := json.Marshal(dhmClientConfig())
	for _, s := range []string{`"dhm_terraen"`, "dhm_wcs", "GetCoverage", "EPSG:25832", dhmTestToken} {
		if strings.Contains(string(raw), s) {
			t.Errorf("client config carries %q", s)
		}
	}
	// The #734 entries keep their exact keys.
	var block struct {
		Overlays []map[string]json.RawMessage `json:"overlays"`
	}
	json.Unmarshal(raw, &block)
	for _, e := range block.Overlays {
		var id string
		json.Unmarshal(e["id"], &id)
		keys := make([]string, 0, len(e))
		for k := range e {
			keys = append(keys, k)
		}
		want := 5
		if id == "elevation" {
			want = 7
		}
		if len(keys) != want {
			t.Errorf("%s: keys %v", id, keys)
		}
	}
}

// The legend's colours are CSS variables (public/style.css, :root) that
// must be the ramp the server renders: one --dhm-elevation-<m> per stop.
func TestDHM792_LegendCSSMatchesTheRamp(t *testing.T) {
	css, err := os.ReadFile("../../public/style.css")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`--dhm-elevation-(\d+):\s*#([0-9a-fA-F]{6})\s*;`)
	found := map[int]string{}
	for _, m := range re.FindAllStringSubmatch(string(css), -1) {
		n, _ := strconv.Atoi(m[1])
		if prev, dup := found[n]; dup && !strings.EqualFold(prev, m[2]) {
			t.Errorf("--dhm-elevation-%d defined twice: %s and %s", n, prev, m[2])
		}
		found[n] = strings.ToLower(m[2])
	}
	if len(found) != len(dhm.ElevationRamp) {
		t.Errorf("%d legend variables, %d ramp stops", len(found), len(dhm.ElevationRamp))
	}
	for _, s := range dhm.ElevationRamp {
		want := fmt.Sprintf("%02x%02x%02x", s.R, s.G, s.B)
		if found[s.M] != want {
			t.Errorf("--dhm-elevation-%d = #%s, want #%s", s.M, found[s.M], want)
		}
	}
}

// The tile route through the server: the same handler as every overlay.
func TestDHM792_RoutedThroughTheTileEndpoint(t *testing.T) {
	up := newWCSUpstream(t, 60, nil)
	s := &Server{dhm: newElevationProxy(t, up, &dhmLogSink{}, nil)}
	r := httptest.NewRequest("GET", dhmClientPath+"?"+overlayTile("elevation", 12, 0).Encode(), nil).WithContext(context.Background())
	w := httptest.NewRecorder()
	s.handleDHMTile(w, r)
	if w.Code != 200 || up.hits.Load() != 1 {
		t.Fatalf("status %d, %d calls", w.Code, up.hits.Load())
	}
}

// elevationFixture is the tile the browser E2E
// (test-issue-792-elevation-overlay-e2e.js) serves for every elevation
// request: rendered by this proxy from a fake WCS raster, a sea strip in
// the west and ground rising 0-200 m eastwards, so the stubbed tile is a
// real one. DHM792_WRITE_FIXTURE=1 rewrites it.
const elevationFixture = "../../test-fixtures/dhm-elevation-792.png"

func TestDHM792_E2ETileFixtureIsARenderedTile(t *testing.T) {
	up := newWCSUpstreamFunc(t, func(col, _, w, _ int) float32 {
		if col < w/8 {
			return 0
		}
		return float32(col) * 200 / float32(w)
	}, nil)
	p := newElevationProxy(t, up, &dhmLogSink{}, nil)
	w := doTile(p, overlayTile("elevation", 11, 0))
	if w.Code != 200 || w.Header().Get("X-CoreScope-Tile") != "miss" {
		t.Fatalf("status %d %s", w.Code, w.Header().Get("X-CoreScope-Tile"))
	}
	if os.Getenv("DHM792_WRITE_FIXTURE") == "1" {
		if err := os.WriteFile(elevationFixture, w.Body.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(elevationFixture)
	if err != nil {
		t.Fatal(err)
	}
	a, errA := png.Decode(bytes.NewReader(want))
	b, errB := png.Decode(bytes.NewReader(w.Body.Bytes()))
	if errA != nil || errB != nil {
		t.Fatalf("decode: %v %v", errA, errB)
	}
	// Pixels, not bytes: the PNG compressor may change with the Go version.
	for y := 0; y < 256; y++ {
		for x := 0; x < 256; x++ {
			if color.NRGBAModel.Convert(a.At(x, y)) != color.NRGBAModel.Convert(b.At(x, y)) {
				t.Fatalf("pixel (%d,%d) differs from the fixture: rerun with DHM792_WRITE_FIXTURE=1", x, y)
			}
		}
	}
}

// #831 F7: an elevation tile that meets the DHM coverage but misses the
// WCS raster extent is "outside": transparent, no upstream call, not
// charged to the client's budget and not cached as a miss.
func TestDHM792_OffExtentTileIsOutside(t *testing.T) {
	var off dhm.TileKey
	found := false
	for x := 0; x < 1<<9 && !found; x++ {
		for y := 0; y < 1<<9; y++ {
			k := dhm.TileKey{Z: 9, X: x, Y: y}
			if _, ok := dhm.ElevationRaster(k); k.IntersectsCoverage() && !ok {
				off, found = k, true
				break
			}
		}
	}
	if !found {
		t.Fatal("no z9 tile meets the coverage but misses the raster extent")
	}
	up := newWCSUpstream(t, 40, nil)
	logs := &dhmLogSink{}
	p := newElevationProxy(t, up, logs, func(o *dhmProxyOptions) { o.rateLimitPerMin = 1 })
	q := leafletQuery(off.Z, off.X, off.Y)
	q.Set("layers", "elevation")
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, overlayRequest(q, "203.0.113.9:1"))
		assertTile(t, "off extent", w, dhmTransparentPNG, "outside", ccTile)
	}
	if up.hits.Load() != 0 {
		t.Errorf("%d upstream calls", up.hits.Load())
	}
	if _, ok := p.cache.Get(dhmCacheKey{Product: dhmProductElevation, Tile: off}, p.now()); ok {
		t.Error("cached")
	}
	// The one-tile budget is still whole: a real miss is served.
	w := httptest.NewRecorder()
	p.ServeHTTP(w, overlayRequest(overlayTile("elevation", 12, 0), "203.0.113.9:1"))
	if w.Code != 200 || w.Header().Get("X-CoreScope-Tile") != "miss" {
		t.Fatalf("a miss after off-extent tiles: %d %s", w.Code, w.Header().Get("X-CoreScope-Tile"))
	}
}
