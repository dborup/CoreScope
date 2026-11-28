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

// #746 — a free map point has no per-node key, so the server's whole rule
// for its antenna height is the operator's radio.antennaHeightsM.point,
// else the built-in default. The block publishes that rule so the Map page
// can apply a customizer point default in the one request a drop makes,
// instead of asking twice. Absent unless the profile is served.
func TestClientConfigPointAntenna(t *testing.T) {
	up := wcsUpstream(t, tiffAnswer)
	for _, tc := range []struct {
		name    string
		profile bool
		radio   *RadioConfig
		want    *DHMPointAntenna
	}{
		{"built-in default", true, nil, &DHMPointAntenna{M: 2, Source: "point-default"}},
		{"operator point height", true, &RadioConfig{AntennaHeightsM: map[string]float64{"point": 6.5}},
			&DHMPointAntenna{M: 6.5, Source: "operator-role"}},
		// A role height is no rule for a point.
		{"operator companion height only", true, &RadioConfig{AntennaHeightsM: map[string]float64{"companion": 4}},
			&DHMPointAntenna{M: 2, Source: "point-default"}},
		{"profile disabled", false, &RadioConfig{AntennaHeightsM: map[string]float64{"point": 6.5}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, router := setupTestServer(t)
			srv.cfg.DHM = DHMConfig{Enabled: true, Token: dhmTestToken}
			srv.dhm = newTestDHMProxy(t, up, &dhmLogSink{}, nil)
			if tc.profile {
				srv.terrain = newTerrainProfiler(dhm.ProfileConfig{Enabled: true},
					tc.radio, terrainProfilerDeps{src: newFakeElevation(t, flatElev(10)), logf: (&dhmLogSink{}).logf})
			}
			block := clientDHMBlock(t, router)
			if block == nil {
				t.Fatal("no dhm block while the proxy is active")
			}
			if (block.PointAntenna == nil) != (tc.want == nil) || (tc.want != nil && *block.PointAntenna != *tc.want) {
				t.Fatalf("dhm.pointAntenna = %+v, want %+v", block.PointAntenna, tc.want)
			}
			if tc.want == nil {
				return
			}
			// The published rule is the one a point request actually gets.
			h, src, _ := srv.terrain.antenna(terrainEnd{kind: "point"}, nil)
			if h != block.PointAntenna.M || src != block.PointAntenna.Source {
				t.Fatalf("published %+v, but a point resolves to %v m (%s)", block.PointAntenna, h, src)
			}
		})
	}
}
