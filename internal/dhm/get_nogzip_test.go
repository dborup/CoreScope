package dhm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestNoRedirectClientNeverAsksForGzip: Dataforsyningen cuts gzip bodies
// short over HTTP/2 (#684, #730), so the keyless client must never send
// Accept-Encoding: gzip, and a server that gzips anyway is not decoded
// behind our back.
func TestNoRedirectClientNeverAsksForGzip(t *testing.T) {
	var gotAE string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAE = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	hc := NewNoRedirectClient(0)
	tr, ok := hc.Transport.(*http.Transport)
	if !ok || !tr.DisableCompression {
		t.Fatalf("transport %T does not disable compression", hc.Transport)
	}
	body, err := GetBody(context.Background(), hc, srv.URL, "application/json", []string{"application/json"}, 1<<10)
	if err != nil {
		t.Fatal(err)
	}
	if gotAE != "" {
		t.Fatalf("client sent Accept-Encoding %q, want none", gotAE)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("body %q", body)
	}
}
