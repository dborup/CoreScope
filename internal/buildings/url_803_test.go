package buildings

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// #803: the live smoke check requests SegmentURL. It must be exactly the URL
// FetchSegment sends.
func TestSegmentURLIsWhatFetchSegmentSends(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.RequestURI()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := testClient(srv.URL + "/items")
	x := dokk1Segment(t)
	if !x.Valid() {
		t.Fatal("test Segment not valid")
	}
	if _, err := c.FetchSegment(context.Background(), x); err == nil {
		t.Fatal("fetch succeeded against a failing upstream")
	}
	if want := c.SegmentURL(x); srv.URL+got != want {
		t.Fatalf("sent %s\nwant %s", srv.URL+got, want)
	}
}
