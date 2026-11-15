package heritage

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

// fakeWFS is an httptest stand-in for the agency's WFS. The real service
// is never contacted by these tests.
func fakeWFS(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
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

func testClient(t *testing.T, base string) *Client {
	t.Helper()
	opts := DefaultClientOptions()
	opts.BaseURL = base
	opts.Timeout = 2 * time.Second
	opts.MarginM = 100
	c, err := NewClient(opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func wantReason(t *testing.T, err error, reason string) {
	t.Helper()
	var fe dhm.FetchError
	if !errors.As(err, &fe) || fe.Reason != reason {
		t.Fatalf("err %v, want FetchError %q", err, reason)
	}
}

var testCell = Cell{2105, 24718}

// TestFetchCellRequestShape pins the one request the client can make: the
// fixed WFS 2.0 GetFeature on the protection-zone layer with a bbox, in
// EPSG:25832, with a bounded count. Only the bbox varies, and only by
// cell.
func TestFetchCellRequestShape(t *testing.T) {
	var got *http.Request
	srv, _ := fakeWFS(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		answer(200, "application/json;charset=UTF-8", readFixture(t, "zones_two.json"))(w, r)
	})
	page, err := testClient(t, srv.URL+"/ffgeoserver/public/wfs").FetchCell(context.Background(), testCell)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Zones) != 2 {
		t.Fatalf("%d zones", len(page.Zones))
	}
	if got.Method != http.MethodGet || got.URL.Path != "/ffgeoserver/public/wfs" {
		t.Fatalf("%s %s", got.Method, got.URL.Path)
	}
	const want = "service=WFS&version=2.0.0&request=GetFeature" +
		"&typeNames=public:fundogfortidsminder_areal_beskyttelse" +
		"&outputFormat=application/json&srsName=urn:ogc:def:crs:EPSG::25832&count=100" +
		"&bbox=526150,6179400,526600,6179850,urn:ogc:def:crs:EPSG::25832"
	if got.URL.RawQuery != want {
		t.Fatalf("query\n%s\nwant\n%s", got.URL.RawQuery, want)
	}
	if got.Header.Get("Cookie") != "" || got.Header.Get("Accept") != "application/json" {
		t.Fatalf("headers %v", got.Header)
	}
}

// TestFetchCellNeverSendsAFilter: the obvious "everything within 100 m"
// request, cql_filter=DWITHIN(...), does not work on this service (#723
// research: a lat/lon point returned nonsensical coordinates, a native
// point returned nothing in either axis order). The client therefore
// sends a bbox and tests the geometry itself; no filter parameter of any
// kind may ever appear.
func TestFetchCellNeverSendsAFilter(t *testing.T) {
	q, err := url.ParseQuery(fixedQuery)
	if err != nil {
		t.Fatal(err)
	}
	for k := range q {
		switch strings.ToLower(k) {
		case "cql_filter", "filter", "resourceid", "featureid", "startindex", "propertyname":
			t.Fatalf("fixed query carries %s", k)
		}
	}
	for _, r := range fixedQuery {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_=&:,./", r)
		if !ok {
			t.Fatalf("fixedQuery has %q, which would need escaping", r)
		}
	}
	if len(q) != 7 || q.Get("typeNames") != TypeName || q.Get("count") != strconv.Itoa(MaxFeatures) {
		t.Fatalf("fixedQuery parses to %v", q)
	}
}

func TestFetchCellInvalidCellNeverCallsUpstream(t *testing.T) {
	srv, hits := fakeWFS(t, answer(200, "application/json", readFixture(t, "zones_two.json")))
	for _, c := range []Cell{{0, 0}, {MinCellE - 1, MinCellN}, {MaxCellE, MaxCellN + 1}} {
		if _, err := testClient(t, srv.URL).FetchCell(context.Background(), c); !errors.Is(err, ErrBadCell) {
			t.Errorf("%+v: err %v", c, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("%d upstream requests", hits.Load())
	}
}

func TestNewClientBoundsTheMargin(t *testing.T) {
	for _, m := range []int{-1, MaxMarginM + 1} {
		opts := DefaultClientOptions()
		opts.MarginM = m
		if _, err := NewClient(opts); err == nil {
			t.Errorf("margin %d accepted", m)
		}
	}
}

// TestFetchCellFailuresAreCategories: every failure is a fixed category
// that carries no upstream text.
func TestFetchCellFailuresAreCategories(t *testing.T) {
	for _, c := range []struct {
		name   string
		h      http.HandlerFunc
		reason string
	}{
		{"500", answer(500, "text/html", []byte("<h1>secret stack trace</h1>")), "status 500"},
		{"ows exception as xml", answer(200, "application/xml", []byte("<ows:ExceptionReport>secret</ows:ExceptionReport>")), "unexpected content type"},
		{"malformed json", answer(200, "application/json", []byte(`{"type":"FeatureCollection","features":[secret`)), "bad response"},
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://www.kulturarv.dk/elsewhere", http.StatusMovedPermanently)
		}, "status 301"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := fakeWFS(t, c.h)
			_, err := testClient(t, srv.URL).FetchCell(context.Background(), testCell)
			wantReason(t, err, c.reason)
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("error carries upstream text")
			}
		})
	}
}

func TestFetchCellOversize(t *testing.T) {
	body := readFixture(t, "zones_two.json")
	srv, _ := fakeWFS(t, answer(200, "application/json", body))
	opts := DefaultClientOptions()
	opts.BaseURL, opts.MaxBodyBytes = srv.URL, len(body)-1
	c, _ := NewClient(opts)
	_, err := c.FetchCell(context.Background(), testCell)
	wantReason(t, err, "oversize response")
}

func TestFetchCellCancel(t *testing.T) {
	srv, _ := fakeWFS(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err := testClient(t, srv.URL).FetchCell(ctx, testCell)
	wantReason(t, err, "canceled")
	if d := time.Since(start); d > time.Second {
		t.Fatalf("cancel took %v", d)
	}
}

// TestDefaultBaseURLIsThePublicHTTPSPrefix: only /ffgeoserver/public/*
// answers over HTTPS; neighbouring paths redirect to http:// (#723
// research), and the client follows no redirect.
func TestDefaultBaseURLIsThePublicHTTPSPrefix(t *testing.T) {
	if DefaultBaseURL != "https://www.kulturarv.dk/ffgeoserver/public/wfs" {
		t.Fatal(DefaultBaseURL)
	}
}
