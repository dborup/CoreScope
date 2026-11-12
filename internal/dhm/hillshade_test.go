package dhm

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// #687 — the exaggerated hillshade (wms/dhm_temp) next to the #680 terrain
// hillshade (dhm_DAF). Every test runs against a fake upstream (httptest);
// none reaches Dataforsyningen.

func TestParseHillshade(t *testing.T) {
	cases := []struct {
		in   string
		want Hillshade
		ok   bool
	}{
		{"", HillshadeExaggerated, true}, // the default
		{"exaggerated", HillshadeExaggerated, true},
		{"terrain", HillshadeTerrain, true},
		// Anything else is rejected and falls back to terrain.
		{"Exaggerated", HillshadeTerrain, false},
		{"TERRAIN", HillshadeTerrain, false},
		{" terrain", HillshadeTerrain, false},
		{"exaggerated ", HillshadeTerrain, false},
		{"hillshade", HillshadeTerrain, false},
		{ExaggeratedHillshadeLayer, HillshadeTerrain, false},
		{HillshadeLayer, HillshadeTerrain, false},
	}
	for _, c := range cases {
		got, ok := ParseHillshade(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseHillshade(%q) = %v, %v; want %v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
	if HillshadeExaggerated.String() != "exaggerated" || HillshadeTerrain.String() != "terrain" {
		t.Errorf("String: %q %q", HillshadeExaggerated, HillshadeTerrain)
	}
}

// wantHillshadeQuery is the test's own spelling of the upstream GetMap
// query, so a change to the client's query builder cannot hide here.
func wantHillshadeQuery(layer string, k TileKey) url.Values {
	b := k.BBox()
	q := url.Values{}
	for key, v := range map[string]string{
		"service": "WMS", "version": "1.3.0", "request": "GetMap", "layers": layer,
		"styles": "", "format": "image/png", "transparent": "TRUE",
		"crs": "EPSG:3857", "width": "256", "height": "256",
	} {
		q.Set(key, v)
	}
	q.Set("bbox", formatBBox(b))
	return q
}

// formatBBox spells a bbox the way WMS clients do: shortest exact decimal.
func formatBBox(b [4]float64) string {
	s := make([]string, 4)
	for i, v := range b {
		s[i] = strconv.FormatFloat(v, 'f', -1, 64)
	}
	return strings.Join(s, ",")
}

func TestFetchHillshadeUpstreamPerLayer(t *testing.T) {
	cases := []struct {
		h       Hillshade
		path    string
		layer   string
		service string
	}{
		{HillshadeExaggerated, "/wms/dhm_temp", "quickDHM_skyggekort_terraen_overdrevet", "wms/dhm_temp"},
		{HillshadeTerrain, "/dhm_DAF", "dhm_terraen_skyggekort", "dhm_DAF"},
	}
	for _, c := range cases {
		t.Run(c.h.String(), func(t *testing.T) {
			u := newUpstream(t, nil)
			logs := &logSink{}
			cl := newTestClient(t, u, logs)
			data, err := cl.FetchHillshade(c.h, someTile())
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != string(pngStub) {
				t.Fatalf("body %q", data)
			}
			if u.paths[0] != c.path {
				t.Fatalf("upstream path %q, want %q", u.paths[0], c.path)
			}
			got, _ := url.ParseQuery(u.rawQuery[0])
			if want := wantHillshadeQuery(c.layer, someTile()); got.Encode() != want.Encode() {
				t.Fatalf("upstream query\n got %s\nwant %s", got.Encode(), want.Encode())
			}
			if u.tokens[0] != testToken {
				t.Fatalf("token header %q", u.tokens[0])
			}
			if strings.Contains(u.rawQuery[0], testToken) || strings.Contains(strings.ToLower(u.rawQuery[0]), "token") {
				t.Fatalf("token in upstream URL: %s", u.rawQuery[0])
			}
			if c.h.service() != c.service || c.h.layer() != c.layer {
				t.Fatalf("service/layer %q %q", c.h.service(), c.h.layer())
			}
		})
	}
	// #680's FetchHillshadeTile stays the terrain layer.
	u := newUpstream(t, nil)
	if _, err := newTestClient(t, u, &logSink{}).FetchHillshadeTile(someTile()); err != nil || u.paths[0] != "/dhm_DAF" {
		t.Fatalf("FetchHillshadeTile: %v %v", err, u.paths)
	}
}

// The exaggerated service only accepts exactly the GetMap query of one
// tile with its own layer: no other layer, format or parameter reaches it.
func TestExaggeratedServiceExactShape(t *testing.T) {
	u := newUpstream(t, nil)
	c := newTestClient(t, u, &logSink{})
	k := someTile()
	good := wantHillshadeQuery(ExaggeratedHillshadeLayer, k)
	if _, err := c.Get(ExaggeratedWMSService, good, "image/png"); err != nil {
		t.Fatalf("exact shape rejected: %v", err)
	}
	if u.hits.Load() != 1 {
		t.Fatalf("upstream hits %d", u.hits.Load())
	}
	mutate := func(f func(q url.Values)) url.Values {
		q := wantHillshadeQuery(ExaggeratedHillshadeLayer, k)
		f(q)
		return q
	}
	bad := map[string]url.Values{
		"terrain layer":     mutate(func(q url.Values) { q.Set("layers", HillshadeLayer) }),
		"other layer":       mutate(func(q url.Values) { q.Set("layers", "quickDHM_skyggekort") }),
		"two layers":        mutate(func(q url.Values) { q.Set("layers", ExaggeratedHillshadeLayer+","+HillshadeLayer) }),
		"layer twice":       mutate(func(q url.Values) { q.Add("layers", ExaggeratedHillshadeLayer) }),
		"jpeg":              mutate(func(q url.Values) { q.Set("format", "image/jpeg") }),
		"tiff":              mutate(func(q url.Values) { q.Set("format", "image/tiff") }),
		"png8":              mutate(func(q url.Values) { q.Set("format", "image/png8") }),
		"GetCapabilities":   mutate(func(q url.Values) { q.Set("request", "GetCapabilities") }),
		"GetFeatureInfo":    mutate(func(q url.Values) { q.Set("request", "GetFeatureInfo") }),
		"version 1.1.1":     mutate(func(q url.Values) { q.Set("version", "1.1.1") }),
		"crs 25832":         mutate(func(q url.Values) { q.Set("crs", "EPSG:25832") }),
		"width 512":         mutate(func(q url.Values) { q.Set("width", "512") }),
		"styles":            mutate(func(q url.Values) { q.Set("styles", "default") }),
		"transparent false": mutate(func(q url.Values) { q.Set("transparent", "FALSE") }),
		"extra param":       mutate(func(q url.Values) { q.Set("sld_body", "<x/>") }),
		"token param":       mutate(func(q url.Values) { q.Set("token", "x") }),
		"missing format":    mutate(func(q url.Values) { q.Del("format") }),
		"missing bbox":      mutate(func(q url.Values) { q.Del("bbox") }),
		"non-tile bbox":     mutate(func(q url.Values) { q.Set("bbox", "0,0,1000,1000") }),
		"bbox respelled":    mutate(func(q url.Values) { q.Set("bbox", strings.ReplaceAll(q.Get("bbox"), ",", ".0,")+".0") }),
		"zoom 6":            mutate(func(q url.Values) { q.Set("bbox", formatBBox(TileKey{6, 33, 19}.BBox())) }),
		"empty":             {},
	}
	for name, q := range bad {
		if _, err := c.Get(ExaggeratedWMSService, q, "image/png"); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: err %v, want ErrBadRequest", name, err)
		}
	}
	if u.hits.Load() != 1 {
		t.Fatalf("rejected shapes reached upstream: %d hits", u.hits.Load())
	}
	// The other spellings of the service path are not allow-listed.
	for _, svc := range []string{"dhm_temp", "/wms/dhm_temp", "wms/dhm_temp/", "WMS/dhm_temp", "wms/dhm_temp?x", "wms/../dhm_DAF"} {
		if _, err := c.Get(svc, good, "image/png"); !errors.Is(err, ErrBadRequest) {
			t.Errorf("service %q: err %v", svc, err)
		}
	}
}

// Upstream failures of the exaggerated service are fixed categories with
// no token, URL or upstream text, as for #680.
func TestExaggeratedFailuresAreCategories(t *testing.T) {
	cases := map[string]struct {
		h    http.HandlerFunc
		want string
	}{
		"404": {func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(404)
			w.Write([]byte(r.URL.String() + r.Header.Get("token")))
		}, "status 404"},
		"xml": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/xml")
			w.Write([]byte("<ServiceExceptionReport>" + r.Header.Get("token")))
		}, "unexpected content type"},
		"not png": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("GIF89a" + r.Header.Get("token")))
		}, "non-image response"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			logs := &logSink{}
			c := newTestClient(t, newUpstream(t, tc.h), logs)
			_, err := c.FetchHillshade(HillshadeExaggerated, someTile())
			var fe FetchError
			if !errors.As(err, &fe) || fe.Reason != tc.want {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
			c.LogFailure("exaggerated hillshade tile", err)
			if strings.Contains(err.Error()+logs.String(), testToken) || strings.Contains(logs.String(), "dhm_temp?") {
				t.Fatalf("leak: %v / %s", err, logs.String())
			}
		})
	}
}

// The token header must never follow a redirect, on the new service too.
func TestExaggeratedDoesNotFollowRedirects(t *testing.T) {
	var elsewhere int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere++
		w.Header().Set("Content-Type", "image/png")
		w.Write(pngStub)
	}))
	defer other.Close()
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	})
	_, err := newTestClient(t, u, &logSink{}).FetchHillshade(HillshadeExaggerated, someTile())
	var fe FetchError
	if !errors.As(err, &fe) || fe.Reason != "status 302" {
		t.Fatalf("err %v, want status 302", err)
	}
	if elsewhere != 0 {
		t.Fatalf("redirect followed: %d requests to the redirect target", elsewhere)
	}
}

// #684: the exaggerated service also uses the transport without
// transparent gzip.
func TestExaggeratedDisablesTransparentGzip(t *testing.T) {
	body := append(append([]byte{}, pngStub...), make([]byte, 4096)...)
	u, aeCh := truncatedGzipUpstream(t, "image/png", body)
	data, err := newTestClient(t, u, &logSink{}).FetchHillshade(HillshadeExaggerated, someTile())
	if err != nil {
		t.Fatalf("FetchHillshade: %v", err)
	}
	if ae := <-aeCh; strings.Contains(ae, "gzip") {
		t.Fatalf("Accept-Encoding %q sent to the exaggerated service", ae)
	}
	if string(data) != string(body) {
		t.Fatalf("body %d bytes, want %d", len(data), len(body))
	}
	if u.paths[0] != "/"+ExaggeratedWMSService {
		t.Fatalf("path %q", u.paths[0])
	}
}

// #688 review F1: only the failures the issue lists trigger the terrain
// fallback: non-200, a transport error, a wrong content type or an invalid
// PNG. A timeout or a full concurrency budget (busy) does not: a second
// attempt would wait on the same budget again.
func TestFallbackEligible(t *testing.T) {
	for reason, want := range map[string]bool{
		"status 404": true, "status 500": true, "status 302": true, "status 401": true,
		"transport": true, "read": true, "unexpected content type": true, "non-image response": true,
		"timeout": false, "busy": false, "oversize response": false, "request": false,
		"unknown": false, "": false, "status": false, "status404": false,
	} {
		if got := FallbackEligible(FetchError{reason}); got != want {
			t.Errorf("FallbackEligible(%q) = %v, want %v", reason, got, want)
		}
	}
	for _, err := range []error{nil, ErrBadRequest, errors.New("status 500")} {
		if FallbackEligible(err) {
			t.Errorf("FallbackEligible(%v) = true", err)
		}
	}
}

// The real client produces the two non-eligible categories for the
// exaggerated service: a hanging upstream is a timeout, and a held
// concurrency budget is busy, each after one Timeout.
func TestExaggeratedTimeoutAndBusyAreNotEligible(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	o := DefaultClientOptions()
	o.BaseURL = u.srv.URL + "/"
	o.Timeout = 200 * time.Millisecond
	o.Concurrency = 1
	o.Logf = (&logSink{}).logf
	c := NewClient(Config{Enabled: true, Token: testToken}, o)

	_, err := c.FetchHillshade(HillshadeExaggerated, someTile())
	if FailureReason(err) != "timeout" || FallbackEligible(err) {
		t.Fatalf("hanging upstream: %v, eligible %v", err, FallbackEligible(err))
	}

	c.sem <- struct{}{} // the whole budget is in use
	defer func() { <-c.sem }()
	hits := u.hits.Load()
	_, err = c.FetchHillshade(HillshadeExaggerated, TileKey{Z: 10, X: 541, Y: 316})
	if FailureReason(err) != "busy" || FallbackEligible(err) {
		t.Fatalf("full budget: %v, eligible %v", err, FallbackEligible(err))
	}
	if u.hits.Load() != hits {
		t.Fatalf("a busy request reached upstream")
	}
}
