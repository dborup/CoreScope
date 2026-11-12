package dhm

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strings"
	"testing"
)

// #684 — the upstream truncates gzip bodies over HTTP/2 when the request
// carries Accept-Encoding: gzip (Go's default transport adds this on its
// own and decompresses transparently). The client must disable transparent
// compression so it always gets the identity body. Every test runs against
// a fake upstream (httptest); none reaches Dataforsyningen.

// truncatedGzipUpstream fakes the behaviour verified against the real
// service: a gzip-truncated body whenever the request asks for gzip, and
// the full identity body otherwise. aeCh receives the Accept-Encoding
// header the upstream saw, once per request.
func truncatedGzipUpstream(t *testing.T, contentType string, body []byte) (*upstream, <-chan string) {
	t.Helper()
	aeCh := make(chan string, 1)
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		ae := r.Header.Get("Accept-Encoding")
		aeCh <- ae
		w.Header().Set("Content-Type", contentType)
		if !strings.Contains(ae, "gzip") {
			w.Write(body)
			return
		}
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		gz.Write(body)
		gz.Close()
		full := buf.Bytes()
		w.Header().Set("Content-Encoding", "gzip")
		// Drop the trailing CRC32 + size, leaving the stream without its
		// end-of-stream marker — the truncation observed against the real
		// upstream over HTTP/2.
		w.Write(full[:len(full)-4])
	})
	return u, aeCh
}

func TestClientDisablesCompressionForHillshadeTile(t *testing.T) {
	u, aeCh := truncatedGzipUpstream(t, "image/png", pngStub)
	c := newTestClient(t, u, &logSink{})
	data, err := c.FetchHillshadeTile(someTile())
	if err != nil {
		t.Fatalf("FetchHillshadeTile: %v", err)
	}
	if !bytes.Equal(data, pngStub) {
		t.Fatalf("body %q, want %q", data, pngStub)
	}
	if ae := <-aeCh; strings.Contains(ae, "gzip") {
		t.Fatalf("upstream request Accept-Encoding %q carries gzip", ae)
	}
}

func TestClientDisablesCompressionForRaster(t *testing.T) {
	u, aeCh := truncatedGzipUpstream(t, "image/tiff", tiffStub)
	src := NewWCSSource(newTestClient(t, u, &logSink{}))
	data, err := src.FetchRaster(someRaster())
	if err != nil {
		t.Fatalf("FetchRaster: %v", err)
	}
	if !bytes.Equal(data, tiffStub) {
		t.Fatalf("body %q, want %q", data, tiffStub)
	}
	if ae := <-aeCh; strings.Contains(ae, "gzip") {
		t.Fatalf("upstream request Accept-Encoding %q carries gzip", ae)
	}
}
