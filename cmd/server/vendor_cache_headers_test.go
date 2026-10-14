package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVendoredDependencyCacheHeaders (#442): a vendored browser dependency at
// public/vendor/<name>/<exact version>/<file> is immutable -- the version is
// part of the URL, so a bump is a new path rather than a mutated file at the
// old one, and the browser can cache it forever. A legacy, non-versioned
// vendor file (public/vendor/<file>, cache-busted via ?v=__BUST__) must keep
// the pre-#442 no-cache-on-js/css behaviour: its URL does not change when the
// file does, so caching it long-lived would serve stale bytes after a bump.
func TestVendoredDependencyCacheHeaders(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<html></html>"), 0644); err != nil {
		t.Fatal(err)
	}

	versioned := filepath.Join(root, "vendor", "leaflet", "1.9.4")
	if err := os.MkdirAll(versioned, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versioned, "leaflet.js"), []byte("/* leaflet */"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versioned, "leaflet.css"), []byte("/* leaflet css */"), 0644); err != nil {
		t.Fatal(err)
	}

	// #443 F1: leaflet.css's images (layers-control icon, default marker)
	// live one level deeper, at .../1.9.4/images/<file>. They must get the
	// same immutable caching as the sibling .js/.css -- the versioned-asset
	// regex matches on the URL prefix only, so it should not care how many
	// path segments follow the version.
	images := filepath.Join(versioned, "images")
	if err := os.MkdirAll(images, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(images, "marker-icon.png"), []byte("\x89PNG fake"), 0644); err != nil {
		t.Fatal(err)
	}

	legacy := filepath.Join(root, "vendor")
	if err := os.WriteFile(filepath.Join(legacy, "jsqr.min.js"), []byte("/* jsqr */"), 0644); err != nil {
		t.Fatal(err)
	}

	fs := http.FileServer(http.Dir(root))
	handler := spaHandler(root, fs)

	get := func(p string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", p, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}

	for _, p := range []string{
		"/vendor/leaflet/1.9.4/leaflet.js",
		"/vendor/leaflet/1.9.4/leaflet.css",
		"/vendor/leaflet/1.9.4/images/marker-icon.png",
	} {
		t.Run("versioned "+p, func(t *testing.T) {
			w := get(p)
			if w.Code != http.StatusOK {
				t.Fatalf("got %d, want 200", w.Code)
			}
			cc := w.Header().Get("Cache-Control")
			if !strings.Contains(cc, "immutable") || !strings.Contains(cc, "max-age") {
				t.Errorf("Cache-Control = %q, want long-lived immutable caching", cc)
			}
		})
	}

	t.Run("legacy non-versioned vendor file keeps the unchanged no-cache behaviour", func(t *testing.T) {
		w := get("/vendor/jsqr.min.js")
		if w.Code != http.StatusOK {
			t.Fatalf("got %d, want 200", w.Code)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache, no-store, must-revalidate" {
			t.Errorf("Cache-Control = %q, want %q (cache busting via ?v=__BUST__ relies on this)", cc, "no-cache, no-store, must-revalidate")
		}
	})
}
