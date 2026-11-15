package dhm

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/meshcore-analyzer/dhm/dhmtest"
)

// #728 M2 — the orthophoto upstream (orto_foraar_webm_DAF). Every test
// runs against a fake upstream (httptest); none reaches Dataforsyningen.

// jpegStub is a real 256×256 JPEG standing in for an upstream ortho tile.
var jpegStub = dhmtest.JPEG(256, 256, 1)

func serveJPEG(t *testing.T, body []byte) *upstream {
	return newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(body)
	})
}

// wantOrthoQuery is the test's own, literal copy of the GetTile shape from
// the #728 plan, so a change to orthoParams cannot also hide here.
func wantOrthoQuery(z, x, y int) url.Values {
	return url.Values{
		"service":       {"WMTS"},
		"request":       {"GetTile"},
		"version":       {"1.0.0"},
		"layer":         {"orto_foraar_webm"},
		"style":         {"default"},
		"tilematrixset": {"DFD_GoogleMapsCompatible"},
		"format":        {"image/jpeg"},
		"tilematrix":    {strconv.Itoa(z)},
		"tilerow":       {strconv.Itoa(y)},
		"tilecol":       {strconv.Itoa(x)},
	}
}

// The tile is fetched from the ortho service with exactly the plan's
// GetTile query, rebuilt from the key, and the token in the header only.
func TestFetchOrthoForwardsFixedShape(t *testing.T) {
	u := serveJPEG(t, jpegStub)
	logs := &logSink{}
	k := someTile()
	data, err := newTestClient(t, u, logs).FetchOrthoAdmitted(context.Background(), nil, k)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, jpegStub) {
		t.Fatal("tile body not passed through")
	}
	if OrthoWMTSService != "orto_foraar_webm_DAF" {
		t.Fatalf("OrthoWMTSService = %q", OrthoWMTSService)
	}
	if u.hits.Load() != 1 || u.paths[0] != "/orto_foraar_webm_DAF" {
		t.Fatalf("upstream hits %d, paths %v", u.hits.Load(), u.paths)
	}
	got, err := url.ParseQuery(u.rawQuery[0])
	if err != nil {
		t.Fatal(err)
	}
	if got.Encode() != wantOrthoQuery(k.Z, k.X, k.Y).Encode() {
		t.Fatalf("upstream query\n got %s\nwant %s", got.Encode(), wantOrthoQuery(k.Z, k.X, k.Y).Encode())
	}
	if u.tokens[0] != testToken {
		t.Fatalf("token header %q", u.tokens[0])
	}
	if strings.Contains(u.rawQuery[0], testToken) || strings.Contains(strings.ToLower(u.rawQuery[0]), "token") {
		t.Fatalf("token in the upstream URL: %s", u.rawQuery[0])
	}
	if strings.Contains(logs.String(), testToken) {
		t.Fatal("token in the log")
	}
}

func TestParseOrthoTile(t *testing.T) {
	for _, tc := range []struct{ z, x, y string }{
		{"7", "0", "0"},
		{"7", "127", "127"},
		{"10", "540", "316"},
		{"20", "1048575", "1048575"},
	} {
		k, err := ParseOrthoTile(tc.z, tc.x, tc.y)
		if err != nil {
			t.Errorf("%v rejected: %v", tc, err)
			continue
		}
		if k.String() != tc.z+"/"+tc.x+"/"+tc.y {
			t.Errorf("%v parsed as %v", tc, k)
		}
	}
	for name, tc := range map[string]struct{ z, x, y string }{
		"z below min":   {"6", "0", "0"},
		"z above max":   {"21", "0", "0"},
		"z zero":        {"0", "0", "0"},
		"x off grid":    {"7", "128", "0"},
		"y off grid":    {"7", "0", "128"},
		"x off grid 20": {"20", "1048576", "0"},
		"negative":      {"10", "-1", "0"},
		"plus sign":     {"10", "+5", "0"},
		"leading zero":  {"10", "05", "0"},
		"padded z":      {"07", "0", "0"},
		"space":         {"10", " 5", "0"},
		"decimal":       {"10", "5.0", "0"},
		"hex":           {"10", "0x5", "0"},
		"empty":         {"10", "", "0"},
		"too long":      {"10", "00000005", "0"},
		"huge":          {"10", "99999999999999999999", "0"},
		"unicode digit": {"10", "５", "0"},
		"suffix":        {"10", "5.jpg", "0"},
	} {
		if k, err := ParseOrthoTile(tc.z, tc.x, tc.y); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: %v accepted as %v (err %v)", name, tc, k, err)
		}
	}
}

// Client.Get to the ortho service accepts only the exact GetTile shape:
// every other query is refused before any upstream request.
func TestOrthoQueryCheckRejectsOtherShapes(t *testing.T) {
	u := serveJPEG(t, jpegStub)
	c := newTestClient(t, u, &logSink{})
	k := someTile()
	if _, err := c.Get(OrthoWMTSService, wantOrthoQuery(k.Z, k.X, k.Y), "image/jpeg"); err != nil {
		t.Fatalf("the exact shape is refused: %v", err)
	}
	u.hits.Store(0)
	with := func(f func(q url.Values)) url.Values {
		q := wantOrthoQuery(k.Z, k.X, k.Y)
		f(q)
		return q
	}
	for name, q := range map[string]url.Values{
		"other layer":          with(func(q url.Values) { q.Set("layer", "orto_foraar_wmts") }),
		"quickorto layer":      with(func(q url.Values) { q.Set("layer", "quickorto") }),
		"png format":           with(func(q url.Values) { q.Set("format", "image/png") }),
		"utm tile matrix set":  with(func(q url.Values) { q.Set("tilematrixset", "KortforsyningTilingDK") }),
		"other style":          with(func(q url.Values) { q.Set("style", "") }),
		"WMS service":          with(func(q url.Values) { q.Set("service", "WMS") }),
		"GetCapabilities":      with(func(q url.Values) { q.Set("request", "GetCapabilities") }),
		"GetFeatureInfo":       with(func(q url.Values) { q.Set("request", "GetFeatureInfo") }),
		"other version":        with(func(q url.Values) { q.Set("version", "1.1.0") }),
		"extra parameter":      with(func(q url.Values) { q.Set("infoformat", "text/html") }),
		"token parameter":      with(func(q url.Values) { q.Set("token", testToken) }),
		"TOKEN parameter":      with(func(q url.Values) { q.Set("TOKEN", testToken) }),
		"duplicated key":       with(func(q url.Values) { q.Add("layer", OrthoLayer) }),
		"upper-case key":       with(func(q url.Values) { q["Layer"] = q["layer"]; delete(q, "layer") }),
		"missing tilecol":      with(func(q url.Values) { q.Del("tilecol") }),
		"missing format":       with(func(q url.Values) { q.Del("format") }),
		"z below min":          with(func(q url.Values) { q.Set("tilematrix", "6") }),
		"z above max":          with(func(q url.Values) { q.Set("tilematrix", "21") }),
		"x off grid":           with(func(q url.Values) { q.Set("tilecol", "1024") }),
		"y off grid":           with(func(q url.Values) { q.Set("tilerow", "1024") }),
		"tile matrix prefixed": with(func(q url.Values) { q.Set("tilematrix", OrthoTileMatrixSet+":10") }),
		"empty":                {},
	} {
		if _, err := c.Get(OrthoWMTSService, q, "image/jpeg"); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: err %v, want ErrBadRequest", name, err)
		}
	}
	if n := u.hits.Load(); n != 0 {
		t.Fatalf("%d upstream requests for refused shapes", n)
	}
	// The hillshade shape is not an ortho request, and the reverse.
	if _, err := c.Get(OrthoWMTSService, someTile().hillshadeQuery(HillshadeTerrain), "image/jpeg"); !errors.Is(err, ErrBadRequest) {
		t.Errorf("hillshade query accepted by the ortho service: %v", err)
	}
	if _, err := c.Get(WMSService, k.orthoQuery(), "image/png"); !errors.Is(err, ErrBadRequest) {
		t.Errorf("ortho query accepted by the hillshade service: %v", err)
	}
	if n := u.hits.Load(); n != 0 {
		t.Fatalf("%d upstream requests for crossed shapes", n)
	}
}

// withSOFSize returns j with its SOF0 height and width set to h × w, so
// only the declared size is wrong.
func withSOFSize(t *testing.T, j []byte, w, h uint16) []byte {
	t.Helper()
	q := append([]byte{}, j...)
	i := bytes.Index(q, []byte{0xff, 0xc0})
	if i < 0 {
		t.Fatal("no SOF0 marker")
	}
	// marker(2) length(2) precision(1) height(2) width(2)
	binary.BigEndian.PutUint16(q[i+5:], h)
	binary.BigEndian.PutUint16(q[i+7:], w)
	return q
}

// Only a complete 256×256 JPEG is returned (#695's lesson for PNG): a
// truncated or wrongly sized one is "invalid image", and a body that is not
// a JPEG at all is "non-image response".
func TestFetchOrthoRejectsInvalidJPEG(t *testing.T) {
	sos := bytes.Index(jpegStub, []byte{0xff, 0xda})
	if sos < 0 || !bytes.HasSuffix(jpegStub, []byte{0xff, 0xd9}) {
		t.Fatal("jpegStub has no SOS marker or no EOI")
	}
	cases := map[string]struct {
		body []byte
		want string
	}{
		"SOI then text":       {[]byte("\xff\xd8\xffnot a JPEG"), "invalid image"},
		"SOI only":            {jpegSOI, "invalid image"},
		"headers only":        {jpegStub[:sos], "invalid image"},
		"truncated after SOS": {jpegStub[:sos+20], "invalid image"},
		"truncated in scan":   {jpegStub[:len(jpegStub)/2], "invalid image"},
		"truncated near end":  {jpegStub[:len(jpegStub)-64], "invalid image"},
		"no EOI":              {jpegStub[:len(jpegStub)-2], "invalid image"},
		"255x256":             {dhmtest.JPEG(255, 256, 1), "invalid image"},
		"256x255":             {dhmtest.JPEG(256, 255, 1), "invalid image"},
		"512x512":             {dhmtest.JPEG(512, 512, 1), "invalid image"},
		"1x1":                 {dhmtest.JPEG(1, 1, 1), "invalid image"},
		"8192 SOF":            {withSOFSize(t, dhmtest.JPEG(8, 8, 1), 8192, 8192), "invalid image"},
		"PNG tile":            {pngStub, "non-image response"},
		"XML error":           {[]byte(`<?xml version="1.0"?><ExceptionReport/>`), "non-image response"},
		"empty":               {nil, "non-image response"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			data, err := newTestClient(t, serveJPEG(t, tc.body), &logSink{}).FetchOrthoAdmitted(context.Background(), nil, someTile())
			var fe FetchError
			if data != nil || !errors.As(err, &fe) || fe.Reason != tc.want {
				t.Fatalf("got %d bytes, err %v; want FetchError %q", len(data), err, tc.want)
			}
		})
	}
}

// A JPEG body under another content type, an error status or a redirect
// is a FetchError by category; no redirect is followed.
func TestFetchOrthoUpstreamFailures(t *testing.T) {
	elsewhere := newUpstream(t, nil)
	for name, tc := range map[string]struct {
		h    http.HandlerFunc
		want string
	}{
		"png content type": {func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			w.Write(jpegStub)
		}, "unexpected content type"},
		"403": {func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte("User not authorized " + testToken))
		}, "status 403"},
		"redirect": {func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.srv.URL+"/steal", http.StatusFound)
		}, "status 302"},
	} {
		t.Run(name, func(t *testing.T) {
			logs := &logSink{}
			c := newTestClient(t, newUpstream(t, tc.h), logs)
			_, err := c.FetchOrthoAdmitted(context.Background(), nil, someTile())
			if FailureReason(err) != tc.want {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Fatal("token in the error")
			}
			c.LogFailure("ortho tile", err)
			if strings.Contains(logs.String(), testToken) {
				t.Fatal("token in the log")
			}
		})
	}
	if n := elsewhere.hits.Load(); n != 0 {
		t.Fatalf("redirect followed: %d requests", n)
	}
}

// #769 review N1: the ortho service has the 1 MiB tile cap, not the
// raster cap the WCS gets. The JPEG decoder stops at the end-of-image
// marker, so a real tile padded with zeros to the cap is still a valid
// tile: exactly 1 MiB passes, one byte more is "oversize response", and
// so is a body under the WCS raster cap.
func TestFetchOrthoBodyCap(t *testing.T) {
	const tileCap = 1 << 20
	if o := DefaultClientOptions(); o.MaxBodyBytes != tileCap {
		t.Fatalf("tile cap changed to %d", o.MaxBodyBytes)
	}
	padded := func(n int) *upstream {
		return serveJPEG(t, append(append([]byte{}, jpegStub...), make([]byte, n-len(jpegStub))...))
	}
	data, err := newTestClient(t, padded(tileCap), &logSink{}).FetchOrthoAdmitted(context.Background(), nil, someTile())
	if err != nil || len(data) != tileCap {
		t.Fatalf("tile of exactly %d bytes: %d bytes, err %v", tileCap, len(data), err)
	}
	for _, n := range []int{tileCap + 1, tileCap + 4096} {
		data, err := newTestClient(t, padded(n), &logSink{}).FetchOrthoAdmitted(context.Background(), nil, someTile())
		var fe FetchError
		if data != nil || !errors.As(err, &fe) || fe.Reason != "oversize response" {
			t.Fatalf("tile of %d bytes: %d bytes, err %v; want oversize response", n, len(data), err)
		}
	}
	if allowedServices[OrthoWMTSService].raster {
		t.Fatal("the ortho service is marked raster: it would get the WCS body cap")
	}
}

// A full Admission refuses a cold ortho tile without an upstream request,
// like a hillshade tile (#744).
func TestFetchOrthoAdmissionFull(t *testing.T) {
	u := serveJPEG(t, jpegStub)
	adm := NewAdmission(1)
	if !adm.tryAcquire() {
		t.Fatal("fresh admission is full")
	}
	defer adm.release()
	_, err := newTestClient(t, u, &logSink{}).FetchOrthoAdmitted(context.Background(), adm, someTile())
	if !errors.Is(err, ErrAdmissionFull) {
		t.Fatalf("err %v, want ErrAdmissionFull", err)
	}
	if n := u.hits.Load(); n != 0 {
		t.Fatalf("%d upstream requests while admission is full", n)
	}
}

// The orthophoto credit is its own line in the style of #733: it names
// owner, dataset and licence, is plain text, and does not claim the data
// was changed (the tiles are shown as served).
func TestOrthoCredit(t *testing.T) {
	if OrthoAttribution != "Indeholder data fra GeoDanmark, Ortofoto forår, CC BY 4.0" {
		t.Errorf("OrthoAttribution = %q, want the #728 plan's wording", OrthoAttribution)
	}
	for _, part := range []string{OrthoOwner, OrthoDatasetName, LicenseName} {
		if !strings.Contains(OrthoAttribution, part) {
			t.Errorf("OrthoAttribution %q does not name %q", OrthoAttribution, part)
		}
	}
	if strings.Contains(OrthoAttribution, DerivedNote) {
		t.Errorf("OrthoAttribution %q claims the data was changed", OrthoAttribution)
	}
	// The orthophoto is not DHM: its credit must not name the DHM dataset.
	if strings.Contains(OrthoAttribution, DatasetName) || OrthoAttribution == Attribution {
		t.Errorf("OrthoAttribution %q credits DHM", OrthoAttribution)
	}
	for name, v := range map[string]string{
		"OrthoOwner": OrthoOwner, "OrthoDatasetName": OrthoDatasetName,
		"OrthoDatasetURL": OrthoDatasetURL, "OrthoAttribution": OrthoAttribution,
	} {
		if v == "" || strings.ContainsAny(v, "<>&\"'") || strings.TrimSpace(v) != v {
			t.Errorf("%s must be non-empty plain text: %q", name, v)
		}
	}
	// The credit links the dataset's own product page (verified 2026-11-15).
	if OrthoDatasetURL != "https://dataforsyningen.dk/data/981" {
		t.Errorf("OrthoDatasetURL = %q, want the product page /data/981", OrthoDatasetURL)
	}
	if OrthoOwner != "GeoDanmark" || LicenseName != "CC BY 4.0" {
		t.Errorf("owner/licence = %q/%q, want GeoDanmark/CC BY 4.0 (product page terms)", OrthoOwner, LicenseName)
	}
}

// BenchmarkCheckTileJPEG is the cost of the full decode on every ortho
// tile fetched upstream (cache hits are not re-checked).
func BenchmarkCheckTileJPEG(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(jpegStub)))
	for i := 0; i < b.N; i++ {
		if err := checkTileJPEG(jpegStub); err != nil {
			b.Fatal(err)
		}
	}
}
