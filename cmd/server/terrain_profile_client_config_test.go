package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/dhm"
)

// #681 M4 — /api/config/client tells the browser whether the Path profile
// tool may be offered at all. Without the flag the Map page has no way to
// know: the hillshade block (#680) is present whenever the proxy runs, while
// the profile also needs map.dhm.profile.enabled.

func clientDHMBlock(t *testing.T, router *mux.Router) *DHMClientConfig {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/config/client", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var body struct {
		DHM *DHMClientConfig `json:"dhm"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body.String())
	}
	return body.DHM
}

func TestClientConfigProfileFlag(t *testing.T) {
	up := wcsUpstream(t, tiffAnswer)
	for _, tc := range []struct {
		name    string
		profile bool
		want    bool
	}{
		{"profile enabled", true, true},
		{"profile disabled", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, router := setupTestServer(t)
			srv.cfg.DHM = DHMConfig{Enabled: true, Token: dhmTestToken}
			srv.dhm = newTestDHMProxy(t, up, &dhmLogSink{}, nil)
			if tc.profile {
				srv.terrain = newTerrainProfiler(dhm.ProfileConfig{Enabled: true},
					nil, terrainProfilerDeps{src: newFakeElevation(t, flatElev(10)), logf: (&dhmLogSink{}).logf})
				if srv.terrain == nil {
					t.Fatal("profiler is nil for an enabled profile")
				}
			}
			block := clientDHMBlock(t, router)
			if block == nil {
				t.Fatal("no dhm block while the proxy is active")
			}
			if block.Profile != tc.want {
				t.Fatalf("dhm.profile = %v, want %v", block.Profile, tc.want)
			}
			// The hillshade half is unaffected either way.
			if !block.Enabled || block.URL != dhmClientPath {
				t.Fatalf("hillshade block changed: %+v", block)
			}
		})
	}
}

// Without the DHM proxy there is no block at all, so the tool is hidden even
// if a profiler somehow existed.
func TestClientConfigNoDHMNoProfileFlag(t *testing.T) {
	srv, router := setupTestServer(t)
	srv.dhm = nil
	srv.terrain = newTerrainProfiler(dhm.ProfileConfig{Enabled: true},
		nil, terrainProfilerDeps{src: newFakeElevation(t, flatElev(10)), logf: (&dhmLogSink{}).logf})
	if block := clientDHMBlock(t, router); block != nil {
		t.Fatalf("dhm block without a proxy: %+v", block)
	}
}
