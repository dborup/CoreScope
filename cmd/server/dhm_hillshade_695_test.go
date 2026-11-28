package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/meshcore-analyzer/dhm/dhmtest"
)

// #695 — the proxy only caches a tile that decodes as a whole 256×256 PNG.
// A body that merely starts with the PNG signature (the upstream truncates
// bodies, #684) is an "invalid image" failure: fallback-eligible on the
// exaggerated layer, unavailable on terrain, and never cached. Every test
// runs against fake upstreams (httptest); none reaches Dataforsyningen.

var (
	sigOnlyPNG   = []byte("\x89PNG\r\n\x1a\nnot a PNG")
	truncatedPNG = dhmtest.PNG(256, 256, 4)[:30_000]
	wrongSizePNG = dhmtest.PNG(512, 512, 5)
)

func servePNGBody(body []byte) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(body)
	}
}

var invalidTileBodies = map[string][]byte{
	"signature only": sigOnlyPNG,
	"truncated png":  truncatedPNG,
	"wrong size png": wrongSizePNG,
}

// Terrain layer: every request for an invalid tile goes upstream again and
// is unavailable; once upstream recovers the real tile is cached.
func TestDHM695_TerrainInvalidTileNeverCached(t *testing.T) {
	for name, body := range invalidTileBodies {
		t.Run(name, func(t *testing.T) {
			u := newDHMUpstreams(t)
			u.terrain.set(servePNGBody(body))
			logs := &dhmLogSink{}
			p := newLayerProxy(t, u, "terrain", logs, nil)
			z, x, y := dkTile()
			q := leafletQuery(z, x, y)
			for i := 1; i <= 2; i++ {
				assertTile(t, "invalid", doTile(p, q), dhmTransparentPNG, "unavailable", ccNoStore)
				if u.terrain.hits() != i || p.cache.Len() != 0 {
					t.Fatalf("request %d: terrain hits %d, cache entries %d; want %d and 0", i, u.terrain.hits(), p.cache.Len(), i)
				}
			}
			// LogEvery is 0 in tests: one line per failed request.
			if want := strings.Repeat("[dhm] upstream hillshade tile failed (invalid image)\n", 2); logs.String() != want {
				t.Fatalf("log %q, want %q", logs.String(), want)
			}
			u.terrain.set(nil)
			assertTile(t, "recovered", doTile(p, q), terrainStub, "miss", ccTile)
			assertTile(t, "recovered hit", doTile(p, q), terrainStub, "hit", ccTile)
			if u.terrain.hits() != 3 || p.cache.Len() != 1 || u.exaggerated.hits() != 0 {
				t.Fatalf("hits terrain=%d exaggerated=%d, cache %d", u.terrain.hits(), u.exaggerated.hits(), p.cache.Len())
			}
		})
	}
}

// Default layer: an invalid exaggerated tile falls back to terrain; when
// terrain is invalid too the tile is unavailable and neither is cached.
func TestDHM695_DefaultBothInvalidNeverCached(t *testing.T) {
	for name, body := range invalidTileBodies {
		t.Run(name, func(t *testing.T) {
			u := newDHMUpstreams(t)
			u.exaggerated.set(servePNGBody(body))
			u.terrain.set(servePNGBody(body))
			logs := &dhmLogSink{}
			p := newLayerProxy(t, u, "", logs, nil)
			z, x, y := dkTile()
			q := leafletQuery(z, x, y)
			for i := 1; i <= 2; i++ {
				assertTile(t, "both invalid", doTile(p, q), dhmTransparentPNG, "unavailable", ccNoStore)
				if u.exaggerated.hits() != i || u.terrain.hits() != i || p.cache.Len() != 0 {
					t.Fatalf("request %d: hits exaggerated=%d terrain=%d, cache %d", i, u.exaggerated.hits(), u.terrain.hits(), p.cache.Len())
				}
			}
			if want := strings.Repeat("[dhm] upstream hillshade tile failed (exaggerated invalid image, terrain invalid image)\n", 2); logs.String() != want {
				t.Fatalf("log %q, want %q", logs.String(), want)
			}
		})
	}
}
