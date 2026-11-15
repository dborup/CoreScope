package dhm

// #737 M1 — RangeRead, the strict range read shared by internal/copdem
// (#738) and internal/skraafoto. Only httptest servers are contacted.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

func rangeServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func serveRange(w http.ResponseWriter, body []byte, from, total int64) {
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, from+int64(len(body))-1, total))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusPartialContent)
	w.Write(body)
}

func TestRangeRead737_Exact(t *testing.T) {
	var gotRange string
	srv, _ := rangeServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		serveRange(w, []byte("abcd"), 10, 100)
	})
	var sent atomic.Int64
	body, total, err := RangeRead(context.Background(), NewNoRedirectClient(0), srv.URL+"/x.tif", Range{From: 10, To: 13, MaxBody: 4}, &sent)
	if err != nil || string(body) != "abcd" || total != 100 {
		t.Fatalf("%q %d %v", body, total, err)
	}
	if gotRange != "bytes=10-13" || sent.Load() != 1 {
		t.Fatalf("Range %q, sent %d", gotRange, sent.Load())
	}
	// A read past the end is answered up to the last byte.
	srv2, _ := rangeServer(t, func(w http.ResponseWriter, r *http.Request) { serveRange(w, []byte("xy"), 98, 100) })
	if body, _, err := RangeRead(context.Background(), NewNoRedirectClient(0), srv2.URL, Range{From: 98, To: 200, MaxBody: 103}, nil); err != nil || string(body) != "xy" {
		t.Fatalf("tail: %q %v", body, err)
	}
}

func TestRangeRead737_Refusals(t *testing.T) {
	cases := []struct {
		name   string
		r      Range
		h      http.HandlerFunc
		reason string
	}{
		{"status 200", Range{From: 0, To: 3, MaxBody: 4}, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("abcd")) }, "status 200"},
		{"redirect", Range{From: 0, To: 3, MaxBody: 4}, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://elsewhere.invalid/", http.StatusFound)
		}, "status 302"},
		{"no content range", Range{From: 0, To: 3, MaxBody: 4}, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte("abcd"))
		}, "bad content range"},
		{"other start", Range{From: 0, To: 3, MaxBody: 4}, func(w http.ResponseWriter, r *http.Request) { serveRange(w, []byte("abcd"), 1, 100) }, "bad content range"},
		{"later start, same end", Range{From: 0, To: 3, MaxBody: 4}, func(w http.ResponseWriter, r *http.Request) { serveRange(w, []byte("bcd"), 1, 100) }, "bad content range"},
		{"200 with a content range", Range{From: 0, To: 3, MaxBody: 4}, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Range", "bytes 0-3/100")
			w.Write([]byte("abcd"))
		}, "status 200"},
		{"short end", Range{From: 0, To: 3, MaxBody: 4}, func(w http.ResponseWriter, r *http.Request) { serveRange(w, []byte("abc"), 0, 100) }, "bad content range"},
		{"wrong size", Range{From: 0, To: 3, Size: 99, MaxBody: 4}, func(w http.ResponseWriter, r *http.Request) { serveRange(w, []byte("abcd"), 0, 100) }, "bad content range"},
		{"multi range", Range{From: 0, To: 3, MaxBody: 4}, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Range", "bytes 0-1/100, 2-3/100")
			w.WriteHeader(http.StatusPartialContent)
		}, "bad content range"},
		{"long body", Range{From: 0, To: 3, MaxBody: 4}, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Range", "bytes 0-3/100")
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte("abcdef"))
		}, "bad content length"},
	}
	for _, c := range cases {
		srv, _ := rangeServer(t, c.h)
		_, _, err := RangeRead(context.Background(), NewNoRedirectClient(0), srv.URL, c.r, nil)
		if FailureReason(err) != c.reason {
			t.Errorf("%s: %v, want %q", c.name, err, c.reason)
		}
	}
}

func TestRangeRead737_BadRangeSendsNothing(t *testing.T) {
	srv, hits := rangeServer(t, func(w http.ResponseWriter, r *http.Request) { serveRange(w, []byte("a"), 0, 1) })
	var sent atomic.Int64
	for _, r := range []Range{{From: -1, To: 3, MaxBody: 8}, {From: 4, To: 3, MaxBody: 8}, {From: 0, To: 8, MaxBody: 8}} {
		if _, _, err := RangeRead(context.Background(), NewNoRedirectClient(0), srv.URL, r, &sent); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%+v: %v", r, err)
		}
	}
	if hits.Load() != 0 || sent.Load() != 0 {
		t.Fatalf("%d requests reached the server, %d counted", hits.Load(), sent.Load())
	}
}
