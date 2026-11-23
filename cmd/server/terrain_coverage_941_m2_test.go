package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/meshcore-analyzer/dhm"
)

// #941 M2 — the propagation model of GET /api/terrain/coverage: a
// request's `model` (bullington or delta), else map.coverage.model, else
// Bullington; the model is part of the cache key. Every test runs on a
// fake ElevationSource.

func rasterBytes(t *testing.T, env *coverageEnv, resp CoverageResponse) []byte {
	t.Helper()
	w := env.raster(resp.Raster.URL, true)
	if w.Code != 200 {
		t.Fatalf("raster %d: %s", w.Code, w.Body.String())
	}
	zr, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestCoverage941M2_ModelIsInTheCacheKey: each model is its own cached
// result with its own versioned id, naming the default explicitly is the
// default's key, the second model reuses the elevation tiles (no upstream
// call), and the delta raster is never less lossy than Bullington's and
// lossier somewhere (a 2 m receiver on gently rolling ground).
func TestCoverage941M2_ModelIsInTheCacheKey(t *testing.T) {
	src := newFakeElevation(t, hillyElev)
	env := newCoverageEnv(t, src, CoverageConfig{}, nil, nil)
	at := "56.1629,10.2039"
	w := env.get(reachQuery(at, "r=8"))
	bull := decodeCoverage(t, w)
	if w.Header().Get("X-CoreScope-Cache") != "miss" || bull.Params.Model != dhm.CoverageModel {
		t.Fatalf("default: %s %+v", w.Header().Get("X-CoreScope-Cache"), bull.Params)
	}
	calls := src.calls.Load()
	w = env.get(reachQuery(at, "r=8", "model=delta"))
	delta := decodeCoverage(t, w)
	if w.Header().Get("X-CoreScope-Cache") != "miss" || delta.Key == bull.Key || delta.Params.Model != dhm.CoverageModelDelta {
		t.Fatalf("delta: %s key %s (bullington %s) %+v", w.Header().Get("X-CoreScope-Cache"), delta.Key, bull.Key, delta.Params)
	}
	if src.calls.Load() != calls {
		t.Fatalf("the second model fetched %d more rasters: the tiles are shared", src.calls.Load()-calls)
	}
	for _, c := range []struct {
		q   string
		key string
	}{{"model=bullington", bull.Key}, {"model=delta", delta.Key}} {
		w := env.get(reachQuery(at, "r=8", c.q))
		if got := decodeCoverage(t, w); got.Key != c.key || w.Header().Get("X-CoreScope-Cache") != "hit" {
			t.Fatalf("%s: key %s (%s), want %s hit", c.q, got.Key, w.Header().Get("X-CoreScope-Cache"), c.key)
		}
	}
	bb, db := rasterBytes(t, env, bull), rasterBytes(t, env, delta)
	if len(bb) != len(db) {
		t.Fatalf("raster sizes %d vs %d", len(bb), len(db))
	}
	more := 0
	for i := range bb {
		if (bb[i] == dhm.CoverageNoData) != (db[i] == dhm.CoverageNoData) {
			t.Fatalf("pixel %d: no-data differs (%d vs %d)", i, bb[i], db[i])
		}
		if bb[i] != dhm.CoverageNoData && db[i]+1 < bb[i] { // one 0.5 dB step of interpolation
			t.Fatalf("pixel %d: delta %d below Bullington %d", i, db[i], bb[i])
		}
		if db[i] > bb[i]+4 {
			more++
		}
	}
	if more == 0 {
		t.Fatal("the complete method changed no pixel by more than 2 dB")
	}
	// Junk models are the grammar's fixed refusal, echoing nothing.
	for _, q := range []string{"model=", "model=Delta", "model=fspl", "model=itm", "model=delta&model=delta", "model=%3Cscript%3E"} {
		w := env.get(reachQuery(at, "r=8", q))
		wantError(t, w, 400, "invalid terrain request")
		if strings.Contains(w.Body.String(), "script") || strings.Contains(w.Body.String(), "itm") {
			t.Fatalf("%q echoed: %s", q, w.Body.String())
		}
	}
}

// TestCoverage941M2_ConfigModel: map.coverage.model is the default a
// request without `model` gets, a request may still name the other, a
// junk value warns (echoing nothing) and falls back to Bullington, and
// the client config offers both models with the default.
func TestCoverage941M2_ConfigModel(t *testing.T) {
	env := newCoverageEnv(t, newFakeElevation(t, flatElev(4)), CoverageConfig{Model: strp("delta")}, nil, nil)
	if got := decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "r=5"))); got.Params.Model != dhm.CoverageModelDelta {
		t.Fatalf("configured delta: %+v", got.Params)
	}
	if got := decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "r=5", "model=bullington"))); got.Params.Model != dhm.CoverageModel {
		t.Fatalf("requested bullington: %+v", got.Params)
	}
	cc := env.srv.coverageClientConfig()
	if cc.Model != "delta" || strings.Join(cc.Models, ",") != "bullington,delta" {
		t.Fatalf("client config %+v", cc)
	}
	b, _ := json.Marshal(cc)
	if !strings.Contains(string(b), `"model":"delta"`) || !strings.Contains(string(b), `"models":["bullington","delta"]`) {
		t.Fatalf("client config JSON %s", b)
	}

	logs := &dhmLogSink{}
	if s := resolveCoverageSettings(CoverageConfig{Model: strp("itm<x>")}, logs.logf); s.model != dhm.PathBullington ||
		!strings.Contains(logs.String(), "map.coverage.model") || strings.Contains(logs.String(), "itm") {
		t.Fatalf("junk model: %v, log %q", s.model, logs.String())
	}
	if s := resolveCoverageSettings(CoverageConfig{}, logs.logf); s.model != dhm.PathBullington {
		t.Fatalf("default model %v", s.model)
	}
	// The default stays Bullington until the validation report has run.
	env = newCoverageEnv(t, newFakeElevation(t, flatElev(4)), CoverageConfig{}, nil, nil)
	if cc := env.srv.coverageClientConfig(); cc.Model != "bullington" {
		t.Fatalf("default client model %q", cc.Model)
	}
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, httptest.NewRequest("GET", "/api/config/client", nil))
	if !strings.Contains(w.Body.String(), `"models":["bullington","delta"]`) {
		t.Fatalf("/api/config/client: %s", w.Body.String())
	}
}

// TestCoverage941M2_ModelReadsTheCanonicalKeyOnly: map.coverage.model is
// read by its exact-case key (#884/#886), like the block's gate. A case
// variant never picks the model, whatever the order of the keys, and is
// reported once with the other ignored keys.
func TestCoverage941M2_ModelReadsTheCanonicalKeyOnly(t *testing.T) {
	cases := []struct {
		name, block string
		want        dhm.PathModel
		warnings    int
	}{
		{"canonical delta", `"model":"delta"`, dhm.PathDelta, 0},
		{"Model alone", `"Model":"delta"`, dhm.PathBullington, 1},
		{"MODEL after canonical bullington", `"model":"bullington","MODEL":"delta"`, dhm.PathBullington, 1},
		{"Model before canonical bullington", `"Model":"delta","model":"bullington"`, dhm.PathBullington, 1},
		{"Model after canonical delta", `"model":"delta","Model":"bullington"`, dhm.PathDelta, 1},
		{"absent", `"radiusKm":20`, dhm.PathBullington, 0},
		{"null", `"model":null`, dhm.PathBullington, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, logged := loadConfigLogged(t, `{"map":{"coverage":{"enabled":true,`+tc.block+`}}}`)
			if !cfg.Coverage.Enabled {
				t.Fatal("the reach is off")
			}
			if got := resolveCoverageSettings(cfg.Coverage, quietLogf).model; got != tc.want {
				t.Fatalf("model %q, want %q", got, tc.want)
			}
			if n := ignoredWarnings(logged); n != tc.warnings {
				t.Fatalf("%d warnings, want %d:\n%s", n, tc.warnings, logged)
			}
			if tc.warnings > 0 && !strings.Contains(logged, "map.coverage.") {
				t.Fatalf("the warning does not name the key:\n%s", logged)
			}
		})
	}
}

// TestCoverage941M2_OpenAPIModel: the model parameter is documented.
func TestCoverage941M2_OpenAPIModel(t *testing.T) {
	m := routeDescriptions()["GET "+coverageRoute]
	found := false
	for _, p := range m.QueryParams {
		found = found || p.Name == "model"
	}
	if !found || !strings.Contains(m.Description, "delta") {
		t.Fatalf("model undocumented: %+v", m.QueryParams)
	}
}
