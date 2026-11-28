package mastedb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dhm"
)

func testClient(base string) *Client {
	opts := DefaultClientOptions()
	opts.BaseURL = base
	opts.Timeout = 2 * time.Second
	return NewClient(opts)
}

// serve answers every request with status, content type and body, and
// counts the requests.
func serve(t *testing.T, status int, contentType string, body []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func wantReason(t *testing.T, err error, reason string) {
	t.Helper()
	var fe dhm.FetchError
	if !errors.As(err, &fe) || fe.Reason != reason {
		t.Fatalf("err %v, want FetchError %q", err, reason)
	}
}

// TestFetchCellRequestShape: the upstream request is exactly the fixed
// GetFeature shape with the cell's bbox, and nothing else.
func TestFetchCellRequestShape(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Header().Set("Content-Type", "application/gml+xml; version=3.2")
		w.Write(readFixture(t, "cell_small.gml"))
	}))
	defer srv.Close()
	page, err := testClient(srv.URL+"/viskort/map.ashx").FetchCell(context.Background(), testCell)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 8 {
		t.Fatalf("%d rows", len(page.Rows))
	}
	if got.Method != http.MethodGet || got.URL.Path != "/viskort/map.ashx" {
		t.Fatalf("%s %s", got.Method, got.URL.Path)
	}
	const want = "service=wfs&request=GetFeature&version=1.1.1" +
		"&typeName=mastedatabasen:MasteIBrug" +
		"&propertyName=MasteID,Unik_Station_navn,UTM_E,UTM_N,Vejnavn,Husnummer,Postnr,By," +
		"TjenesteArt_navn,Teknologi_navn,Frekvensbaand,Idriftsaettelsesdato,Objekt_type" +
		"&BBOX=526000,6254000,528000,6256000,EPSG:25832"
	if got.URL.RawQuery != want {
		t.Fatalf("query\n%s\nwant\n%s", got.URL.RawQuery, want)
	}
	if c := got.Header.Get("Cookie"); c != "" {
		t.Errorf("sent a cookie %q", c)
	}
}

// TestFixedQueryNeedsNoEscaping: the hand-built query only holds
// characters that are literal in a query string, and parses back to the
// intended parameters.
func TestFixedQueryNeedsNoEscaping(t *testing.T) {
	for _, r := range fixedQuery {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_=&:,.", r)
		if !ok {
			t.Fatalf("fixedQuery has %q", r)
		}
	}
	q, err := url.ParseQuery(fixedQuery)
	if err != nil {
		t.Fatal(err)
	}
	if len(q) != 5 || q.Get("typeName") != TypeName || q.Get("propertyName") != PropertyNames ||
		q.Get("request") != "GetFeature" || q.Get("service") != "wfs" {
		t.Fatalf("fixedQuery parses to %v", q)
	}
}

func TestFetchCellInvalidCellNeverCallsUpstream(t *testing.T) {
	srv, hits := serve(t, 200, "application/gml+xml", readFixture(t, "cell_small.gml"))
	for _, c := range []Cell{{0, 0}, {MinCellE - 1, MinCellN}, {MaxCellE, MaxCellN + 1}} {
		if _, err := testClient(srv.URL).FetchCell(context.Background(), c); !errors.Is(err, ErrBadCell) {
			t.Errorf("%+v: err %v, want ErrBadCell", c, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("%d upstream requests for invalid cells", hits.Load())
	}
}

func TestFetchCellHTML500(t *testing.T) {
	srv, _ := serve(t, 500, "text/html; charset=utf-8",
		[]byte(`<!DOCTYPE html><html><body><h1>Server Error in '/' Application.</h1></body></html>`))
	_, err := testClient(srv.URL).FetchCell(context.Background(), testCell)
	wantReason(t, err, "status 500")
	if strings.Contains(err.Error(), "Server Error") {
		t.Fatal("error carries upstream text")
	}
}

func TestFetchCellHTMLWith200(t *testing.T) {
	srv, _ := serve(t, 200, "text/html", []byte(`<html><body>maintenance</body></html>`))
	_, err := testClient(srv.URL).FetchCell(context.Background(), testCell)
	wantReason(t, err, "unexpected content type")
}

func TestFetchCellBadBody(t *testing.T) {
	srv, _ := serve(t, 200, "application/xml", []byte(`<html><body>maintenance</body></html>`))
	_, err := testClient(srv.URL).FetchCell(context.Background(), testCell)
	wantReason(t, err, "bad response")
}

func TestFetchCellNoRedirect(t *testing.T) {
	target, targetHits := serve(t, 200, "application/gml+xml", readFixture(t, "cell_small.gml"))
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/elsewhere", http.StatusFound)
	}))
	defer redirect.Close()
	_, err := testClient(redirect.URL).FetchCell(context.Background(), testCell)
	wantReason(t, err, "status 302")
	if targetHits.Load() != 0 {
		t.Fatal("redirect was followed")
	}
}

func TestFetchCellOversize(t *testing.T) {
	body := readFixture(t, "cell_small.gml")
	srv, _ := serve(t, 200, "application/gml+xml", body)
	opts := DefaultClientOptions()
	opts.BaseURL = srv.URL
	opts.MaxBodyBytes = len(body) - 1
	_, err := NewClient(opts).FetchCell(context.Background(), testCell)
	wantReason(t, err, "oversize response")
	opts.MaxBodyBytes = len(body)
	if _, err := NewClient(opts).FetchCell(context.Background(), testCell); err != nil {
		t.Fatalf("body of exactly MaxBodyBytes: %v", err)
	}
}

// stall answers only when the request is abandoned, or after 5 s.
func stall(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchCellTimeout(t *testing.T) {
	opts := DefaultClientOptions()
	opts.BaseURL = stall(t).URL
	opts.Timeout = 50 * time.Millisecond
	start := time.Now()
	_, err := NewClient(opts).FetchCell(context.Background(), testCell)
	wantReason(t, err, "timeout")
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("timeout took %v", d)
	}
}

func TestFetchCellContextCanceled(t *testing.T) {
	c := testClient(stall(t).URL)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err := c.FetchCell(ctx, testCell)
	wantReason(t, err, "canceled")
	if d := time.Since(start); d > time.Second {
		t.Fatalf("cancel took %v", d)
	}
}

func TestFetchCellContextDeadline(t *testing.T) {
	c := testClient(stall(t).URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.FetchCell(ctx, testCell)
	wantReason(t, err, "timeout")
}
