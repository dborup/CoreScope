package dhm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// #803: the live smoke check builds each DHM request with RequestURL and
// the exported query builders. They must be exactly what the Client sends,
// token header included, or the smoke check tests a different shape.
func TestRequestURLIsWhatTheClientSends(t *testing.T) {
	var mu sync.Mutex
	var got, tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.URL.RequestURI())
		tokens = append(tokens, r.Header.Get(TokenHeader))
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	opts := DefaultClientOptions()
	opts.BaseURL = srv.URL + "/"
	opts.Logf = func(string, ...interface{}) {}
	c := NewClient(Config{Enabled: true, Token: "tok-803"}, opts)
	ctx := context.Background()
	k := TileKey{Z: 14, X: 8764, Y: 5120}

	var want []string
	for _, id := range OverlayIDs() {
		if o, _ := LookupOverlay(id); o.Rendered() {
			// #792: one WCS raster for its tile, at its top zoom.
			ek := TileKey{Z: ElevationMaxZoom, X: k.X >> (k.Z - ElevationMaxZoom), Y: k.Y >> (k.Z - ElevationMaxZoom)}
			c.FetchOverlayAdmitted(ctx, nil, o, ek)
			r, _ := ElevationRaster(ek)
			want = append(want, RequestURL(opts.BaseURL, WCSService, r.CoverageQuery()))
			continue
		}
		for _, h := range []Hillshade{HillshadeExaggerated, HillshadeTerrain} {
			o, _ := LookupOverlay(id)
			o = o.Resolve(h)
			c.FetchOverlayAdmitted(ctx, nil, o, k)
			want = append(want, RequestURL(opts.BaseURL, o.Service, k.OverlayQuery(o)))
		}
	}
	c.FetchOrthoAdmitted(ctx, nil, k)
	want = append(want, RequestURL(opts.BaseURL, OrthoWMTSService, k.OrthoQuery()))
	for _, m := range []Model{ModelTerrain, ModelSurface} {
		r := RasterRequest{Model: m, BBox: [4]float64{724000, 6175000, 725000, 6176000}, Width: 100, Height: 100}
		NewWCSSource(c).FetchRaster(ctx, r)
		want = append(want, RequestURL(opts.BaseURL, WCSService, r.CoverageQuery()))
	}

	if len(got) != len(want) {
		t.Fatalf("%d requests, want %d", len(got), len(want))
	}
	for i := range want {
		if srv.URL+got[i] != want[i] {
			t.Errorf("request %d\n got %s\nwant %s", i, srv.URL+got[i], want[i])
		}
		if tokens[i] != "tok-803" {
			t.Errorf("request %d: %s header %q", i, TokenHeader, tokens[i])
		}
	}
}

// RequestURL is the service path below the base and the encoded query.
func TestRequestURLShape(t *testing.T) {
	q := TileKey{Z: 14, X: 8764, Y: 5120}.OrthoQuery()
	if got, want := RequestURL(DefaultBaseURL, OrthoWMTSService, q), DefaultBaseURL+OrthoWMTSService+"?"+q.Encode(); got != want {
		t.Fatalf("%s\nwant %s", got, want)
	}
}

// Range.Header is the Range header RangeRead sends.
func TestRangeHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Range")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	r := Range{From: 7, To: 16383, MaxBody: 1 << 20}
	RangeRead(context.Background(), NewNoRedirectClient(0), srv.URL, r, nil)
	if got != r.Header() || got != "bytes=7-16383" {
		t.Fatalf("sent %q, Header %q", got, r.Header())
	}
}
