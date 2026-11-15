package buildings

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dhm"
)

// fakeOGC is an httptest stand-in for the OGC API. The real service is
// never contacted by these tests.
func fakeOGC(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func answer(status int, contentType string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		w.Write(body)
	}
}

func testClient(base string) *Client {
	opts := DefaultClientOptions()
	opts.BaseURL = base
	opts.Timeout = 2 * time.Second
	return NewClient(opts)
}

func wantReason(t *testing.T, err error, reason string) {
	t.Helper()
	var fe dhm.FetchError
	if !errors.As(err, &fe) || fe.Reason != reason {
		t.Fatalf("err %v, want FetchError %q", err, reason)
	}
}

// dokk1Segment is the one segment of a 916 m diagonal path from Dokk1
// north-west into Aarhus C.
func dokk1Segment(t testing.TB) Segment {
	t.Helper()
	segs := mustCorridor(t, dokk1, [2]float64{574537.6, 6224090.5}).Segments()
	if len(segs) != 1 {
		t.Fatalf("%d segments", len(segs))
	}
	return segs[0]
}

// TestSegmentURLGolden pins the one request the client can make, byte for
// byte: the building collection's items, GeoJSON in EPSG:25832, at most
// MaxFeatures, filtered by CQL2 DWITHIN of the segment's line. Only the
// four decimetre coordinates and the allow-listed radius vary. The line
// is the path grown by 15 m at both ends: A − 15·u and B + 15·u, where
// u = (−880.4, 251.8) / 915.70 m.
func TestSegmentURLGolden(t *testing.T) {
	seg := dokk1Segment(t)
	if seg.Line != [4]int64{5754324, 62238346, 5745232, 62240946} {
		t.Fatalf("line %v", seg.Line)
	}
	const want = "https://api.dataforsyningen.dk/ogcapi/features/building_inspire/collections/building/items" +
		"?f=application/geo%2Bjson" +
		"&crs=http://www.opengis.net/def/crs/EPSG/0/25832" +
		"&filter-lang=ecql-text" +
		"&filter-crs=http://www.opengis.net/def/crs/EPSG/0/25832" +
		"&limit=1000" +
		"&filter=DWITHIN%28geometry%2CLINESTRING%28575432.4%206223834.6%2C574523.2%206224094.6%29%2C15%2Cmeters%29"
	if got := NewClient(DefaultClientOptions()).segmentURL(seg); got != want {
		t.Fatalf("URL\n%s\nwant\n%s", got, want)
	}
}

// TestSegmentURLDecodesToTheFixedQuery: the URL decodes to exactly the six
// fixed keys, none of them a token, and to the CQL2 text intended, for
// every allowed radius; the escaping is the client's own, not
// url.Values'.
func TestSegmentURLDecodesToTheFixedQuery(t *testing.T) {
	segs := append(mustCorridor(t, [2]float64{560000, 6280000}, [2]float64{581213.2, 6221213.2}).Segments(), dokk1Segment(t))
	for _, r := range AllowedRadiiM {
		c, err := NewCorridor(dokk1, [2]float64{574537.6, 6224090.5}, r, DefaultSegmentM)
		if err != nil {
			t.Fatal(err)
		}
		segs = append(segs, c.Segments()...)
	}
	for _, seg := range segs {
		u, err := url.Parse(NewClient(DefaultClientOptions()).segmentURL(seg))
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		if len(q) != 6 {
			t.Fatalf("%d keys: %v", len(q), q)
		}
		for k, v := range map[string]string{
			"f": "application/geo+json", "crs": CRS, "filter-lang": "ecql-text", "filter-crs": CRS,
			"limit": "1000",
			"filter": "DWITHIN(geometry,LINESTRING(" + fmtDM(seg.Line[0]) + " " + fmtDM(seg.Line[1]) + "," +
				fmtDM(seg.Line[2]) + " " + fmtDM(seg.Line[3]) + ")," + strconv.Itoa(seg.RadiusM) + ",meters)",
		} {
			if got := q[k]; len(got) != 1 || got[0] != v {
				t.Errorf("%s = %q, want %q", k, got, v)
			}
		}
		for k := range q {
			if strings.EqualFold(k, "token") {
				t.Fatal("token in the query")
			}
		}
	}
	for _, r := range fixedQuery {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_-=&:/.%", r)
		if !ok {
			t.Fatalf("fixedQuery has %q, which would need escaping", r)
		}
	}
}

// TestFmtDM: decimetres print as metres with one decimal, the way the
// rounded corridor ends were measured.
func TestFmtDM(t *testing.T) {
	for in, want := range map[int64]string{5754324: "575432.4", 62238340: "6223834.0", 10: "1.0", 9: "0.9", 0: "0.0"} {
		if got := fmtDM(in); got != want {
			t.Errorf("fmtDM(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestFetchSegmentRequestShape: what reaches the server is that URL as a
// plain GET with Accept: application/geo+json and no token, cookie or
// authorisation header; the answer is parsed into the page.
func TestFetchSegmentRequestShape(t *testing.T) {
	var got *http.Request
	srv, _ := fakeOGC(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		answer(200, "application/json", readFixture(t, "dokk1_cell.json"))(w, r)
	})
	seg := dokk1Segment(t)
	c := testClient(srv.URL + "/ogcapi/features/building_inspire/collections/building/items")
	page, err := c.FetchSegment(context.Background(), seg)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Footprints) != 25 {
		t.Fatalf("%d footprints", len(page.Footprints))
	}
	if got.Method != http.MethodGet || got.URL.Path != "/ogcapi/features/building_inspire/collections/building/items" {
		t.Fatalf("%s %s", got.Method, got.URL.Path)
	}
	if want := strings.SplitN(NewClient(DefaultClientOptions()).segmentURL(seg), "?", 2)[1]; got.URL.RawQuery != want {
		t.Fatalf("query\n%s\nwant\n%s", got.URL.RawQuery, want)
	}
	if got.Header.Get("Accept") != "application/geo+json" {
		t.Fatalf("Accept %q", got.Header.Get("Accept"))
	}
	for _, h := range []string{"Token", "Cookie", "Authorization"} {
		if v := got.Header.Get(h); v != "" {
			t.Fatalf("%s header sent: %q", h, v)
		}
	}
}

// TestFetchSegmentAcceptsBothJSONTypes: the service answers
// f=application/geo+json as application/json (#730 research); the
// registered GeoJSON type is accepted too.
func TestFetchSegmentAcceptsBothJSONTypes(t *testing.T) {
	for _, ct := range []string{"application/json", "application/json;charset=UTF-8", "application/geo+json"} {
		srv, _ := fakeOGC(t, answer(200, ct, readFixture(t, "dokk1_cell.json")))
		if _, err := testClient(srv.URL).FetchSegment(context.Background(), dokk1Segment(t)); err != nil {
			t.Errorf("%s: %v", ct, err)
		}
	}
}

func TestFetchSegmentInvalidNeverCallsUpstream(t *testing.T) {
	srv, hits := fakeOGC(t, answer(200, "application/json", readFixture(t, "dokk1_cell.json")))
	good := dokk1Segment(t)
	for name, s := range map[string]Segment{
		"zero":            {},
		"radius 16":       {Line: good.Line, RadiusM: 16},
		"zero length":     {Line: [4]int64{good.Line[0], good.Line[1], good.Line[0], good.Line[1]}, RadiusM: 15},
		"outside":         {Line: [4]int64{0, 0, 10, 10}, RadiusM: 15},
		"over 10 km long": {Line: [4]int64{good.Line[0], good.Line[1], good.Line[0] + maxLineDM + 1, good.Line[1]}, RadiusM: 15},
	} {
		if _, err := testClient(srv.URL).FetchSegment(context.Background(), s); !errors.Is(err, ErrBadSegment) {
			t.Errorf("%s: err %v", name, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("%d upstream requests", hits.Load())
	}
}

// TestFetchSegmentFailuresAreCategories: every failure is a fixed category
// that carries no upstream text.
func TestFetchSegmentFailuresAreCategories(t *testing.T) {
	for _, c := range []struct {
		name   string
		h      http.HandlerFunc
		reason string
	}{
		{"500", answer(500, "text/html", []byte("<h1>secret stack trace</h1>")), "status 500"},
		{"400 OGC exception", answer(400, "application/json", []byte(`{"code":"InvalidParameterValue","description":"secret"}`)), "status 400"},
		{"OGC exception with 200", answer(200, "application/json", []byte(`{"code":"InvalidParameterValue","description":"secret"}`)), "bad response"},
		{"xml exception", answer(200, "application/xml", []byte("<ows:ExceptionReport>secret</ows:ExceptionReport>")), "unexpected content type"},
		{"html with 200", answer(200, "text/html", []byte("<h1>secret</h1>")), "unexpected content type"},
		{"malformed json", answer(200, "application/json", []byte(`{"type":"FeatureCollection","features":[secret`)), "bad response"},
		{"lat/lon page", answer(200, "application/json", fc(featureJSON("Polygon", `[[[10.21,56.15],[10.22,56.15],[10.22,56.16],[10.21,56.15]]]`), ``)), "bad response"},
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://elsewhere.invalid/secret", http.StatusFound)
		}, "status 302"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := fakeOGC(t, c.h)
			_, err := testClient(srv.URL).FetchSegment(context.Background(), dokk1Segment(t))
			wantReason(t, err, c.reason)
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("error carries upstream text")
			}
		})
	}
}

func TestFetchSegmentOversize(t *testing.T) {
	body := readFixture(t, "dokk1_cell.json")
	srv, _ := fakeOGC(t, answer(200, "application/json", body))
	opts := DefaultClientOptions()
	opts.BaseURL, opts.MaxBodyBytes = srv.URL, len(body)-1
	_, err := NewClient(opts).FetchSegment(context.Background(), dokk1Segment(t))
	wantReason(t, err, "oversize response")
	if DefaultClientOptions().MaxBodyBytes != 2<<20 {
		t.Fatalf("default body cap %d, want 2 MiB", DefaultClientOptions().MaxBodyBytes)
	}
}

func TestFetchSegmentCancel(t *testing.T) {
	srv, _ := fakeOGC(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err := testClient(srv.URL).FetchSegment(ctx, dokk1Segment(t))
	wantReason(t, err, "canceled")
	if d := time.Since(start); d > time.Second {
		t.Fatalf("cancel took %v", d)
	}
}

// TestDefaultBaseURL pins the INSPIRE – Bygninger collection on
// Dataforsyningen's OGC API, which needs no token (#730 research §2.1).
func TestDefaultBaseURL(t *testing.T) {
	if DefaultBaseURL != "https://api.dataforsyningen.dk/ogcapi/features/building_inspire/collections/building/items" {
		t.Fatal(DefaultBaseURL)
	}
}
