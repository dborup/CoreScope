package dhm

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// #681 M1 — the WCS GetCoverage shape and the ElevationSource. Every test
// runs against a fake upstream (httptest); none reaches Dataforsyningen.

// rasterTIFF is a valid DHM-shaped GeoTIFF for r: a ramp, georeferenced on
// r.BBox at r.Width × r.Height.
func rasterTIFF(r RasterRequest, order binary.ByteOrder) []byte {
	f := newFixture(r.Width, r.Height, ramp(r.Width, r.Height, func(c, row int) float32 { return float32(c + row) }))
	f.order = order
	f.georeferenced(r.BBox[0], r.BBox[3], (r.BBox[2]-r.BBox[0])/float64(r.Width))
	f.scale[1] = (r.BBox[3] - r.BBox[1]) / float64(r.Height)
	return f.bytes()
}

var tiffStub = rasterTIFF(someRaster(), binary.LittleEndian)

// tiffUpstream answers every GetCoverage with a GeoTIFF for exactly the
// requested bbox and size.
func tiffUpstream(t *testing.T) *upstream {
	t.Helper()
	return newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		got, err := parseCoverageQuery(r.URL.Query())
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "image/tiff")
		w.Write(rasterTIFF(got, binary.LittleEndian))
	})
}

// someRaster is a 2.5 km chunk near Aarhus at 179 px, the plan's default
// worst-case chunk (§1.3).
func someRaster() RasterRequest {
	return RasterRequest{
		Model:  ModelTerrain,
		BBox:   [4]float64{575000, 6224000, 577500, 6226500},
		Width:  179,
		Height: 179,
	}
}

func newTestSource(t *testing.T, u *upstream, logs *logSink) ElevationSource {
	t.Helper()
	return NewWCSSource(newTestClient(t, u, logs))
}

func TestWCSSourceIsAnElevationSource(t *testing.T) {
	var _ ElevationSource = NewWCSSource(nil)
}

// The upstream query is exactly the allow-listed GetCoverage shape, rebuilt
// from the validated request, on the WCS service path.
func TestWCSFetchSendsExactGetCoverage(t *testing.T) {
	u := tiffUpstream(t)
	src := newTestSource(t, u, &logSink{})
	g, err := src.FetchRaster(context.Background(), someRaster())
	if err != nil {
		t.Fatal(err)
	}
	if g.Width != 179 || g.Height != 179 || g.Extent() != someRaster().BBox {
		t.Fatalf("grid %dx%d extent %v", g.Width, g.Height, g.Extent())
	}
	if u.paths[0] != "/"+WCSService || WCSService != "dhm_wcs_DAF" {
		t.Fatalf("upstream path %q", u.paths[0])
	}
	got, err := url.ParseQuery(u.rawQuery[0])
	if err != nil {
		t.Fatal(err)
	}
	want := url.Values{
		"service":      {"WCS"},
		"version":      {"1.0.0"},
		"request":      {"GetCoverage"},
		"coverage":     {"dhm_terraen"},
		"crs":          {"EPSG:25832"},
		"response_crs": {"EPSG:25832"},
		"format":       {"GTiff"},
		"bbox":         {"575000,6224000,577500,6226500"},
		"width":        {"179"},
		"height":       {"179"},
	}
	if len(got) != len(want) {
		t.Fatalf("upstream query %v, want %v", got, want)
	}
	for k, v := range want {
		if got.Get(k) != v[0] || len(got[k]) != 1 {
			t.Fatalf("upstream %s = %v, want %v (query %s)", k, got[k], v, u.rawQuery[0])
		}
	}
}

// Each model maps to exactly one DHM coverage; anything else is rejected
// before any upstream call.
func TestWCSModelSelectsCoverage(t *testing.T) {
	for model, coverage := range map[Model]string{ModelTerrain: "dhm_terraen", ModelSurface: "dhm_overflade"} {
		u := tiffUpstream(t)
		r := someRaster()
		r.Model = model
		if _, err := newTestSource(t, u, &logSink{}).FetchRaster(context.Background(), r); err != nil {
			t.Fatalf("%s: %v", model, err)
		}
		q, _ := url.ParseQuery(u.rawQuery[0])
		if q.Get("coverage") != coverage {
			t.Fatalf("model %s requested coverage %q, want %q", model, q.Get("coverage"), coverage)
		}
	}
	u := tiffUpstream(t)
	src := newTestSource(t, u, &logSink{})
	for _, m := range []Model{"", "dhm_terraen", "DTM", "dsm ", "dhm_punktoprindelse"} {
		r := someRaster()
		r.Model = m
		if _, err := src.FetchRaster(context.Background(), r); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("model %q: err %v, want ErrBadRequest", m, err)
		}
	}
	if u.hits.Load() != 0 {
		t.Fatal("rejected requests reached upstream")
	}
}

// The bbox must lie inside the DHM extent in EPSG:25832, be ordered and
// finite, and span at most MaxRasterSpanM; the size is 1..MaxRasterSize.
func TestWCSRasterRequestBounds(t *testing.T) {
	mut := func(f func(*RasterRequest)) RasterRequest { r := someRaster(); f(&r); return r }
	nan := func() float64 { var z float64; return z / z }()
	bad := map[string]RasterRequest{
		"west of DK":         mut(func(r *RasterRequest) { r.BBox = [4]float64{430000, 6224000, 432500, 6226500} }),
		"east of DK":         mut(func(r *RasterRequest) { r.BBox = [4]float64{893000, 6224000, 895500, 6226500} }),
		"south of DK":        mut(func(r *RasterRequest) { r.BBox = [4]float64{575000, 6046000, 577500, 6048500} }),
		"north of DK":        mut(func(r *RasterRequest) { r.BBox = [4]float64{575000, 6403000, 577500, 6405500} }),
		"Paris in UTM32":     mut(func(r *RasterRequest) { r.BBox = [4]float64{-17000, 5420000, -14500, 5422500} }),
		"too wide":           mut(func(r *RasterRequest) { r.BBox = [4]float64{500000, 6224000, 500000 + MaxRasterSpanM + 1, 6226500} }),
		"too tall":           mut(func(r *RasterRequest) { r.BBox = [4]float64{575000, 6100000, 577500, 6100000 + MaxRasterSpanM + 1} }),
		"inverted x":         mut(func(r *RasterRequest) { r.BBox = [4]float64{577500, 6224000, 575000, 6226500} }),
		"empty y":            mut(func(r *RasterRequest) { r.BBox = [4]float64{575000, 6224000, 577500, 6224000} }),
		"NaN":                mut(func(r *RasterRequest) { r.BBox[2] = nan }),
		"width 0":            mut(func(r *RasterRequest) { r.Width = 0 }),
		"height -1":          mut(func(r *RasterRequest) { r.Height = -1 }),
		"width over limit":   mut(func(r *RasterRequest) { r.Width = MaxRasterSize + 1 }),
		"height over limit":  mut(func(r *RasterRequest) { r.Height = MaxRasterSize + 1 }),
		"10000 px (WMS max)": mut(func(r *RasterRequest) { r.Width, r.Height = 10000, 10000 }),
	}
	u := tiffUpstream(t)
	src := newTestSource(t, u, &logSink{})
	for name, r := range bad {
		if err := r.Validate(); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: Validate err %v, want ErrBadRequest", name, err)
		}
		if _, err := src.FetchRaster(context.Background(), r); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: FetchRaster err %v, want ErrBadRequest", name, err)
		}
	}
	if u.hits.Load() != 0 {
		t.Fatal("rejected requests reached upstream")
	}
	good := map[string]RasterRequest{
		"default chunk": someRaster(),
		"max span and size": mut(func(r *RasterRequest) {
			r.BBox = [4]float64{500000, 6100000, 500000 + MaxRasterSpanM, 6100000 + MaxRasterSpanM}
			r.Width, r.Height = MaxRasterSize, MaxRasterSize
		}),
		"extent corners":     mut(func(r *RasterRequest) { r.BBox = [4]float64{440000, 6048000, 442500, 6050500} }),
		"extent far corner":  mut(func(r *RasterRequest) { r.BBox = [4]float64{891500, 6401500, 894000, 6404000} }),
		"1x1":                mut(func(r *RasterRequest) { r.Width, r.Height = 1, 1 }),
		"thin N-S sliver":    mut(func(r *RasterRequest) { r.BBox = [4]float64{575000, 6224000, 575010, 6226500}; r.Width = 1 }),
		"surface model, Fyn": mut(func(r *RasterRequest) { r.Model = ModelSurface; r.BBox = [4]float64{585000, 6130000, 587500, 6132500} }),
		"Bornholm (UTM32 E)": mut(func(r *RasterRequest) { r.BBox = [4]float64{870000, 6120000, 872500, 6122500} }),
		"fractional metres":  mut(func(r *RasterRequest) { r.BBox = [4]float64{575000.25, 6224000.5, 577500.75, 6226500.125} }),
		"plan single-call 80": mut(func(r *RasterRequest) {
			r.BBox = [4]float64{500000, 6150000, 556569, 6206569}
			r.Width, r.Height = 362, 362
		}),
	}
	for name, r := range good {
		if err := r.Validate(); err != nil {
			t.Errorf("%s: Validate err %v", name, err)
		}
		if _, err := src.FetchRaster(context.Background(), r); err != nil {
			t.Errorf("%s: FetchRaster err %v", name, err)
		}
	}
}

// Client.Get re-checks every WCS query against the allow-list, so a caller
// that bypasses RasterRequest cannot reach upstream with another shape:
// unknown coverage, wrong CRS or format, a bbox outside DK, an oversize
// raster, or a caller-supplied token key.
func TestClientGetWCSOnlyAllowListedShape(t *testing.T) {
	valid := func() url.Values { return someRaster().coverageQuery() }
	u := tiffUpstream(t)
	c := newTestClient(t, u, &logSink{})
	if _, err := c.Get(WCSService, valid(), "image/tiff"); err != nil {
		t.Fatalf("valid query rejected: %v", err)
	}
	cases := map[string]func(url.Values){
		"unknown coverage":      func(q url.Values) { q.Set("coverage", "dhm_punktoprindelse") },
		"coverage case":         func(q url.Values) { q.Set("coverage", "DHM_TERRAEN") },
		"two coverages":         func(q url.Values) { q.Set("coverage", "dhm_terraen,dhm_overflade") },
		"wrong crs":             func(q url.Values) { q.Set("crs", "EPSG:4326") },
		"wrong response_crs":    func(q url.Values) { q.Set("response_crs", "EPSG:3857") },
		"missing response_crs":  func(q url.Values) { q.Del("response_crs") },
		"wrong format":          func(q url.Values) { q.Set("format", "image/png") },
		"format case":           func(q url.Values) { q.Set("format", "gtiff") },
		"wrong version":         func(q url.Values) { q.Set("version", "2.0.1") },
		"DescribeCoverage":      func(q url.Values) { q.Set("request", "DescribeCoverage") },
		"bbox outside DK":       func(q url.Values) { q.Set("bbox", "100000,6224000,102500,6226500") },
		"bbox too large":        func(q url.Values) { q.Set("bbox", "450000,6100000,880000,6300000") },
		"bbox three numbers":    func(q url.Values) { q.Set("bbox", "575000,6224000,577500") },
		"bbox not a number":     func(q url.Values) { q.Set("bbox", "575000,6224000,577500,x") },
		"bbox Inf":              func(q url.Values) { q.Set("bbox", "575000,6224000,577500,Inf") },
		"bbox overlong number":  func(q url.Values) { q.Set("bbox", "575000,6224000,577500,"+strings.Repeat("6", 70)) },
		"bbox padded number":    func(q url.Values) { q.Set("bbox", "575000,6224000,577500,6226500."+strings.Repeat("0", 70)) },
		"width over limit":      func(q url.Values) { q.Set("width", strconv.Itoa(MaxRasterSize+1)) },
		"width not an integer":  func(q url.Values) { q.Set("width", "179.5") },
		"width leading plus":    func(q url.Values) { q.Set("width", "+179") },
		"height zero":           func(q url.Values) { q.Set("height", "0") },
		"token key":             func(q url.Values) { q.Set("token", testToken) },
		"TOKEN key":             func(q url.Values) { q.Set("TOKEN", testToken) },
		"extra key":             func(q url.Values) { q.Set("interpolation", "bilinear") },
		"resx instead of width": func(q url.Values) { q.Del("width"); q.Set("resx", "10") },
		"repeated bbox":         func(q url.Values) { q.Add("bbox", q.Get("bbox")) },
		"uppercase key":         func(q url.Values) { q.Set("COVERAGE", q.Get("coverage")); q.Del("coverage") },
	}
	before := u.hits.Load()
	for name, f := range cases {
		q := valid()
		f(q)
		if _, err := c.Get(WCSService, q, "image/tiff"); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: err %v, want ErrBadRequest", name, err)
		}
	}
	if u.hits.Load() != before {
		t.Fatal("rejected WCS queries reached upstream")
	}
}

// The token travels only in the header: never in the upstream URL, an
// error, or a log line, on success or on failure.
func TestWCSTokenNeverInURLErrorOrLog(t *testing.T) {
	u := tiffUpstream(t)
	logs := &logSink{}
	src := newTestSource(t, u, logs)
	if _, err := src.FetchRaster(context.Background(), someRaster()); err != nil {
		t.Fatal(err)
	}
	if u.tokens[0] != testToken {
		t.Fatalf("token header %q", u.tokens[0])
	}
	if strings.Contains(u.rawQuery[0], testToken) || strings.Contains(strings.ToLower(u.rawQuery[0]), "token") {
		t.Fatalf("token in upstream URL: %s", u.rawQuery[0])
	}
	failing := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "error for token "+r.Header.Get("token"))
	})
	fc := newTestClient(t, failing, logs)
	_, err := NewWCSSource(fc).FetchRaster(context.Background(), someRaster())
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Fatalf("err %v", err)
	}
	fc.LogFailure("terrain raster", err)
	if strings.Contains(logs.String(), testToken) || !strings.Contains(logs.String(), "status 500") {
		t.Fatalf("log %q", logs.String())
	}
}

// A missing or wrong token on the WCS is a 403 with the bare text body
// `User not authorized` (probed against dhm_wcs_DAF DescribeCoverage); an
// OGC exception is handled the same way. Only the category comes back.
func TestWCS403IsCategoryOnly(t *testing.T) {
	bodies := map[string]string{
		"text/plain":                 "User not authorized",
		"application/vnd.ogc.se_xml": `<ServiceExceptionReport version="1.2.0"><ServiceException>User not authorized</ServiceException></ServiceExceptionReport>`,
	}
	for ct, body := range bodies {
		u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ct)
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, body)
		})
		logs := &logSink{}
		c := newTestClient(t, u, logs)
		_, err := NewWCSSource(c).FetchRaster(context.Background(), someRaster())
		var fe FetchError
		if !errors.As(err, &fe) || fe.Reason != "status 403" {
			t.Fatalf("%s: err %v, want FetchError status 403", ct, err)
		}
		c.LogFailure("terrain raster", err)
		if strings.Contains(logs.String(), "not authorized") || strings.Contains(logs.String(), testToken) {
			t.Fatalf("%s: upstream text or token logged: %q", ct, logs.String())
		}
	}
}

// WCS 1.0 servers often answer a bad GetCoverage with HTTP 200 and an OGC
// exception document. Only a GeoTIFF content type with a TIFF header is a
// raster.
func TestWCSRejectsNonTIFFResponses(t *testing.T) {
	cases := []struct {
		name, ct string
		body     []byte
		reason   string
	}{
		{"200 OGC exception", "application/vnd.ogc.se_xml", []byte("<ServiceExceptionReport/>"), "unexpected content type"},
		{"200 xml", "text/xml", []byte("<ServiceExceptionReport/>"), "unexpected content type"},
		{"png", "image/png", pngStub, "unexpected content type"},
		{"no content type", "", tiffStub, "unexpected content type"},
		{"tiff type, html body", "image/tiff", []byte("<html>oops</html>"), "invalid raster"},
		{"tiff type, empty body", "image/tiff", nil, "invalid raster"},
		{"tiff type, BigTIFF", "image/tiff", []byte("II+\x00\x08\x00\x00\x00"), "invalid raster"},
		{"tiff prefix type", "image/tiffany", tiffStub, "unexpected content type"},
		{"tiff-fx type", "image/tiff-fx", tiffStub, "unexpected content type"},
		{"geotiff prefix type", "image/geotiffx", tiffStub, "unexpected content type"},
	}
	for _, tc := range cases {
		u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header()["Content-Type"] = []string{tc.ct}
			w.Write(tc.body)
		})
		_, err := newTestSource(t, u, &logSink{}).FetchRaster(context.Background(), someRaster())
		var fe FetchError
		if !errors.As(err, &fe) || fe.Reason != tc.reason {
			t.Errorf("%s: err %v, want FetchError %q", tc.name, err, tc.reason)
		}
	}
	accepted := []struct {
		ct   string
		body []byte
	}{
		{"image/tiff", tiffStub},
		{"image/tiff; subtype=geotiff", tiffStub},
		{"image/geotiff", tiffStub},
		{"IMAGE/TIFF", rasterTIFF(someRaster(), binary.BigEndian)},
	}
	for _, a := range accepted {
		u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", a.ct)
			w.Write(a.body)
		})
		if _, err := newTestSource(t, u, &logSink{}).FetchRaster(context.Background(), someRaster()); err != nil {
			t.Errorf("%s: %v", a.ct, err)
		}
	}
}

func TestWCSDoesNotFollowRedirects(t *testing.T) {
	elsewhere := tiffUpstream(t)
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.srv.URL+"/steal", http.StatusFound)
	})
	if _, err := newTestSource(t, u, &logSink{}).FetchRaster(context.Background(), someRaster()); err == nil {
		t.Fatal("redirect response accepted")
	}
	if elsewhere.hits.Load() != 0 {
		t.Fatal("redirect followed with the token header")
	}
}

// One raster chunk may exceed the 1 MiB tile cap; the WCS has its own
// budget, sized for a MaxRasterSize² float32 raster, and the WMS keeps its
// cap.
func TestBodyCapIsPerService(t *testing.T) {
	o := DefaultClientOptions()
	if o.MaxBodyBytes != 1<<20 {
		t.Fatalf("WMS tile cap changed to %d", o.MaxBodyBytes)
	}
	if o.MaxCoverageBodyBytes != MaxRasterSize*MaxRasterSize*4+64<<10 {
		t.Fatalf("WCS cap %d", o.MaxCoverageBodyBytes)
	}
	big := 1<<20 + 4096 // over the tile cap, under the raster cap
	// serve answers with body padded with zeros to n bytes; a TIFF stays
	// valid, since nothing points into the padding.
	serve := func(ct string, body []byte, n int) *upstream {
		return newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ct)
			w.Write(append(append([]byte{}, body...), make([]byte, n-len(body))...))
		})
	}
	if _, err := newTestSource(t, serve("image/tiff", tiffStub, big), &logSink{}).FetchRaster(context.Background(), someRaster()); err != nil {
		t.Fatalf("raster of %d bytes rejected: %v", big, err)
	}
	var fe FetchError
	_, err := newTestSource(t, serve("image/tiff", tiffStub, o.MaxCoverageBodyBytes+1), &logSink{}).FetchRaster(context.Background(), someRaster())
	if !errors.As(err, &fe) || fe.Reason != "oversize response" {
		t.Fatalf("raster over the WCS cap: err %v", err)
	}
	_, err = newTestClient(t, serve("image/png", pngStub, big), &logSink{}).FetchHillshadeTile(someTile())
	if !errors.As(err, &fe) || fe.Reason != "oversize response" {
		t.Fatalf("tile of %d bytes: err %v, want oversize response", big, err)
	}
	if _, err := newTestClient(t, serve("image/png", pngStub, 1<<20), &logSink{}).FetchHillshadeTile(someTile()); err != nil {
		t.Fatalf("tile at the cap rejected: %v", err)
	}
}

// The coverage query round-trips through the allow-list parser unchanged.
func TestCoverageQueryRoundTrip(t *testing.T) {
	r := someRaster()
	r.Model = ModelSurface
	got, err := parseCoverageQuery(r.coverageQuery())
	if err != nil || got != r {
		t.Fatalf("round trip: %+v %v, want %+v", got, err, r)
	}
}
