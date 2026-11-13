package dhm

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// #681 M2 — FetchRaster returns a decoded grid (#683 review N2), and the
// client nits from the same review (N1, N4–N7). All against fake upstreams.

// FetchRaster decodes the GeoTIFF and keeps the file's georeferencing,
// falls back to the request when the file has none, and rejects a raster
// that does not cover the request.
func TestFetchRasterReturnsGeoreferencedGrid(t *testing.T) {
	r := someRaster()
	px := (r.BBox[2] - r.BBox[0]) / float64(r.Width)
	serve := func(body []byte) *upstream {
		return newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/tiff")
			w.Write(body)
		})
	}
	vals := ramp(r.Width, r.Height, func(c, row int) float32 { return float32(c) - float32(row) })

	// Georeferenced and snapped by a fraction of a pixel, one pixel larger.
	snapped := newFixture(r.Width+1, r.Height+1, ramp(r.Width+1, r.Height+1, func(c, row int) float32 { return 1 })).
		georeferenced(r.BBox[0]-px/3, r.BBox[3]+px/3, px)
	g, err := newTestSource(t, serve(snapped.bytes()), &logSink{}).FetchRaster(context.Background(), r)
	if err != nil {
		t.Fatalf("snapped raster: %v", err)
	}
	if g.Georef.OriginE != r.BBox[0]-px/3 || g.Width != r.Width+1 {
		t.Fatalf("file georef not kept: %+v %d", g.Georef, g.Width)
	}

	// Snapped inward by a third of a pixel: still within one pixel.
	inward := newFixture(r.Width, r.Height, vals).georeferenced(r.BBox[0]+px/3, r.BBox[3]-px/3, px)
	if _, err := newTestSource(t, serve(inward.bytes()), &logSink{}).FetchRaster(context.Background(), r); err != nil {
		t.Fatalf("raster snapped inward: %v", err)
	}

	// No georeferencing in the file: the request is the grid.
	bare := newFixture(r.Width, r.Height, vals)
	bare.compression, bare.predictor = compressionDeflate, predictorFloatingPoint
	g, err = newTestSource(t, serve(bare.bytes()), &logSink{}).FetchRaster(context.Background(), r)
	if err != nil {
		t.Fatalf("bare raster: %v", err)
	}
	if g.Extent() != r.BBox {
		t.Fatalf("bare extent %v, want %v", g.Extent(), r.BBox)
	}
	e, n := g.centre(4, 9)
	if v, ok := g.Sample(e, n); !ok || math.Abs(v-float64(4-9)) > 1e-6 {
		t.Fatalf("bare sample %v %v", v, ok)
	}

	var fe FetchError
	for name, body := range map[string][]byte{
		"bare, wrong size":   newFixture(r.Width-1, r.Height, vals[:(r.Width-1)*r.Height]).bytes(),
		"shifted 2 px east":  newFixture(r.Width, r.Height, vals).georeferenced(r.BBox[0]+2*px, r.BBox[3], px).bytes(),
		"shifted 2 px south": newFixture(r.Width, r.Height, vals).georeferenced(r.BBox[0], r.BBox[3]-2*px, px).bytes(),
		"half the size":      newFixture(r.Width/2, r.Height/2, vals[:(r.Width/2)*(r.Height/2)]).georeferenced(r.BBox[0], r.BBox[3], px).bytes(),
		"somewhere else":     newFixture(r.Width, r.Height, vals).georeferenced(700000, 6100000, px).bytes(),
	} {
		_, err := newTestSource(t, serve(body), &logSink{}).FetchRaster(context.Background(), r)
		if !errors.As(err, &fe) || fe.Reason != "raster mismatch" {
			t.Errorf("%s: err %v, want raster mismatch", name, err)
		}
	}
	// #686 review N4: a georeferenced grid must also have the requested
	// resolution, up to the WCS snapping one pixel. Each of these covers
	// the bbox, so a coverage check alone accepted them.
	pyr := func(w, h int, e, n, pxE, pxN float64) []byte {
		f := newFixture(w, h, make([]float32, w*h)).georeferenced(e, n, pxE)
		f.scale[1] = pxN
		return f.bytes()
	}
	B := r.BBox[2] - r.BBox[0]
	for name, body := range map[string][]byte{
		"2x2 over the bbox":       pyr(2, 2, r.BBox[0], r.BBox[3], B/2, B/2),
		"10x coarser, same count": pyr(r.Width, r.Height, r.BBox[0], r.BBox[3], 10*px, 10*px),
		"2 px wider":              pyr(r.Width+2, r.Height, r.BBox[0], r.BBox[3], px, px),
		"2 px taller":             pyr(r.Width, r.Height+2, r.BBox[0], r.BBox[3], px, px),
		"twice the extent":        pyr(2*r.Width, 2*r.Height, r.BBox[0]-B/2, r.BBox[3]+B/2, px, px),
		"pixels 2x tall":          pyr(r.Width, r.Height/2+1, r.BBox[0], r.BBox[3], px, 2*px),
		"west edge 1.5 px out":    pyr(r.Width+1, r.Height, r.BBox[0]-1.5*px, r.BBox[3], px, px),
		// Same extent, more pixels: only the size bound catches these.
		"2 px more, same extent":  pyr(r.Width+2, r.Height+2, r.BBox[0], r.BBox[3], B/float64(r.Width+2), B/float64(r.Height+2)),
		"2 px fewer, same extent": pyr(r.Width-2, r.Height, r.BBox[0], r.BBox[3], B/float64(r.Width-2), px),
		"twice as fine":           pyr(2*r.Width, 2*r.Height, r.BBox[0], r.BBox[3], px/2, px/2),
	} {
		_, err := newTestSource(t, serve(body), &logSink{}).FetchRaster(context.Background(), r)
		if !errors.As(err, &fe) || fe.Reason != "raster mismatch" {
			t.Errorf("N4 %s: err %v, want raster mismatch", name, err)
		}
	}
	for name, body := range map[string][]byte{
		"one px narrower":       pyr(r.Width-1, r.Height-1, r.BBox[0]+px/2, r.BBox[3]-px/2, px, px),
		"one px larger, offset": pyr(r.Width+1, r.Height+1, r.BBox[0]-0.9*px, r.BBox[3]+0.9*px, px, px),
		"pixel 0.5% larger":     pyr(r.Width, r.Height, r.BBox[0], r.BBox[3], px*1.005, px*1.005),
	} {
		if _, err := newTestSource(t, serve(body), &logSink{}).FetchRaster(context.Background(), r); err != nil {
			t.Errorf("N4 %s: %v, want accepted", name, err)
		}
	}

	bad := newFixture(r.Width, r.Height, vals)
	bad.fields = map[uint16]*fixtureField{tagSampleFormat: bad.shorts(1)}
	if _, err := newTestSource(t, serve(bad.bytes()), &logSink{}).FetchRaster(context.Background(), r); !errors.As(err, &fe) || fe.Reason != "invalid raster" {
		t.Errorf("uint raster: err %v, want invalid raster", err)
	}
}

// N1: only the canonical spelling coverageQuery builds goes upstream.
func TestClientGetWCSRejectsNonCanonicalSpelling(t *testing.T) {
	u := tiffUpstream(t)
	c := newTestClient(t, u, &logSink{})
	cases := map[string]func(url.Values){
		"hex float bbox":     func(q url.Values) { q.Set("bbox", "0x1.18p19,6224000,577500,6226500") },
		"exponent bbox":      func(q url.Values) { q.Set("bbox", "5.75e5,6224000,577500,6226500") },
		"exponent upper":     func(q url.Values) { q.Set("bbox", "5.75E+5,6224000,577500,6226500") },
		"trailing dot":       func(q url.Values) { q.Set("bbox", "575000.,6224000,577500,6226500") },
		"trailing zero":      func(q url.Values) { q.Set("bbox", "575000.0,6224000,577500,6226500") },
		"leading zero bbox":  func(q url.Values) { q.Set("bbox", "0575000,6224000,577500,6226500") },
		"leading plus bbox":  func(q url.Values) { q.Set("bbox", "+575000,6224000,577500,6226500") },
		"underscore digits":  func(q url.Values) { q.Set("bbox", "575_000,6224000,577500,6226500") },
		"leading zero width": func(q url.Values) { q.Set("width", "0179") },
		"leading zero hght":  func(q url.Values) { q.Set("height", "0179") },
	}
	for name, f := range cases {
		q := someRaster().coverageQuery()
		f(q)
		if _, err := c.Get(WCSService, q, "image/tiff"); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: err %v, want ErrBadRequest", name, err)
		}
	}
	if u.hits.Load() != 0 {
		t.Fatalf("%d non-canonical queries reached upstream", u.hits.Load())
	}
	if _, err := c.Get(WCSService, someRaster().coverageQuery(), "image/tiff"); err != nil {
		t.Fatalf("canonical query rejected: %v", err)
	}
}

// N4: a hand-built ClientOptions without MaxCoverageBodyBytes still accepts
// rasters, up to the default raster cap.
func TestCoverageBodyCapZeroValueFallsBack(t *testing.T) {
	serve := func(n int) *upstream {
		return newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/tiff")
			w.Write(append(append([]byte{}, tiffStub...), make([]byte, n-len(tiffStub))...))
		})
	}
	client := func(u *upstream) *Client {
		return NewClient(Config{Enabled: true, Token: testToken}, ClientOptions{
			BaseURL:      u.srv.URL + "/",
			Timeout:      2 * time.Second,
			Concurrency:  1,
			MaxBodyBytes: 1 << 20,
			Now:          time.Now,
			Logf:         (&logSink{}).logf,
		})
	}
	if _, err := NewWCSSource(client(serve(1<<20+4096))).FetchRaster(context.Background(), someRaster()); err != nil {
		t.Fatalf("raster over the tile cap with a zero raster cap: %v", err)
	}
	var fe FetchError
	if _, err := NewWCSSource(client(serve(maxRasterBodyBytes+1))).FetchRaster(context.Background(), someRaster()); !errors.As(err, &fe) || fe.Reason != "oversize response" {
		t.Fatalf("raster over the default cap: %v", err)
	}
}

// N5: the Accept header names exactly the media types the call accepts.
func TestClientSendsAcceptHeader(t *testing.T) {
	var mu sync.Mutex
	var accept []string
	record := func(ct string, body []byte) *upstream {
		return newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			accept = append(accept, r.Header.Get("Accept"))
			mu.Unlock()
			w.Header().Set("Content-Type", ct)
			w.Write(body)
		})
	}
	if _, err := newTestSource(t, record("image/tiff", tiffStub), &logSink{}).FetchRaster(context.Background(), someRaster()); err != nil {
		t.Fatal(err)
	}
	if _, err := newTestClient(t, record("image/png", pngStub), &logSink{}).FetchHillshadeTile(someTile()); err != nil {
		t.Fatal(err)
	}
	if len(accept) != 2 || accept[0] != "image/tiff, image/geotiff" || accept[1] != "image/png" {
		t.Fatalf("Accept headers %q", accept)
	}
}

// N6: the PNG path also matches the media type exactly; parameters and
// case are allowed.
func TestTileContentTypeIsExact(t *testing.T) {
	for ct, ok := range map[string]bool{
		"image/png":                 true,
		"IMAGE/PNG":                 true,
		"image/png; charset=binary": true,
		"image/pngx":                false,
		"image/png-foo":             false,
		"image/png8":                false,
		"image/":                    false,
		"text/html; image/png":      false,
	} {
		u := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", ct)
			w.Write(pngStub)
		})
		_, err := newTestClient(t, u, &logSink{}).FetchHillshadeTile(someTile())
		if (err == nil) != ok {
			t.Errorf("Content-Type %q: err %v, want accepted=%v", ct, err, ok)
		}
	}
}

// N7: Client.Get sends nothing to dhm_DAF but the exact GetMap query the
// tile proxy builds, so no in-repo caller can spend the token on another
// WMS request.
func TestClientGetWMSOnlyTileShape(t *testing.T) {
	u := newUpstream(t, nil)
	c := newTestClient(t, u, &logSink{})
	if _, err := c.Get(WMSService, someTile().upstreamQuery(), "image/png"); err != nil {
		t.Fatalf("tile query rejected: %v", err)
	}
	before := u.hits.Load()
	b := someTile().BBox()
	cases := map[string]func(url.Values){
		"browser transparent":    func(q url.Values) { q.Set("transparent", "true") },
		"other layer":            func(q url.Values) { q.Set("layers", "dhm_overflade_skyggekort") },
		"bigger image":           func(q url.Values) { q.Set("width", "512"); q.Set("height", "512") },
		"jpeg":                   func(q url.Values) { q.Set("format", "image/jpeg") },
		"extra key":              func(q url.Values) { q.Set("exceptions", "XML") },
		"token key":              func(q url.Values) { q.Set("tOkEn", testToken) },
		"repeated bbox":          func(q url.Values) { q.Add("bbox", q.Get("bbox")) },
		"no bbox":                func(q url.Values) { q.Del("bbox") },
		"not a tile":             func(q url.Values) { q.Set("bbox", "1000000,7000000,1500000,7500000") },
		"tile within tolerance":  func(q url.Values) { q.Set("bbox", bboxString(b[0]+1e-6, b[1], b[2], b[3])) },
		"version 1.1.1":          func(q url.Values) { q.Set("version", "1.1.1") },
		"srs instead of crs":     func(q url.Values) { q.Set("srs", q.Get("crs")); q.Del("crs") },
		"lowercase GetMap value": func(q url.Values) { q.Set("request", "getmap") },
	}
	for name, f := range cases {
		q := someTile().upstreamQuery()
		f(q)
		if _, err := c.Get(WMSService, q, "image/png"); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: err %v, want ErrBadRequest", name, err)
		}
	}
	for name, q := range map[string]url.Values{
		"empty":            {},
		"GetCapabilities":  {"service": {"WMS"}, "request": {"GetCapabilities"}},
		"WCS-shaped query": someRaster().coverageQuery(),
	} {
		if _, err := c.Get(WMSService, q, "image/png"); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: err %v, want ErrBadRequest", name, err)
		}
	}
	if u.hits.Load() != before {
		t.Fatalf("%d off-shape WMS queries reached upstream", u.hits.Load()-before)
	}
}

func bboxString(v ...float64) string {
	out := make([]string, len(v))
	for i, f := range v {
		out[i] = strconv.FormatFloat(f, 'f', -1, 64)
	}
	return strings.Join(out, ",")
}
