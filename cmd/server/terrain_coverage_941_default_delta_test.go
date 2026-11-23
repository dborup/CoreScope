package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/meshcore-analyzer/dhm"
)

// #941 — delta-Bullington is the default propagation model, as the
// validation report on real Danish links decided. A request's own model
// and an explicit map.coverage.model (bullington included) still win.

// TestCoverage941Default_AbsentModelIsDelta: with no map.coverage.model
// and no request model, the params name delta and the raster is the
// delta raster, byte for byte, and not Bullington's.
func TestCoverage941Default_AbsentModelIsDelta(t *testing.T) {
	env := newCoverageEnv(t, newFakeElevation(t, hillyElev), CoverageConfig{}, nil, nil)
	at := "56.1629,10.2039"
	def := decodeCoverage(t, env.get(reachQuery(at, "r=8")))
	if def.Params.Model != dhm.CoverageModelDelta {
		t.Fatalf("default params model %q, want %q", def.Params.Model, dhm.CoverageModelDelta)
	}
	delta := decodeCoverage(t, env.get(reachQuery(at, "r=8", "model=delta")))
	bull := decodeCoverage(t, env.get(reachQuery(at, "r=8", "model=bullington")))
	if def.Key != delta.Key || def.Key == bull.Key {
		t.Fatalf("keys: default %s, delta %s, bullington %s", def.Key, delta.Key, bull.Key)
	}
	db, bb := rasterBytes(t, env, def), rasterBytes(t, env, bull)
	if !bytes.Equal(db, rasterBytes(t, env, delta)) {
		t.Fatal("the default losses are not delta's")
	}
	if bytes.Equal(db, bb) {
		t.Fatal("the default losses are Bullington's")
	}
	if cc := env.srv.coverageClientConfig(); cc.Model != "delta" {
		t.Fatalf("client config default %q", cc.Model)
	}
}

// TestCoverage941Default_ExplicitBullingtonWins: map.coverage.model
// "bullington" keeps Bullington, and a request may name either model.
func TestCoverage941Default_ExplicitBullingtonWins(t *testing.T) {
	env := newCoverageEnv(t, newFakeElevation(t, hillyElev), CoverageConfig{Model: strp("bullington")}, nil, nil)
	at := "56.1629,10.2039"
	def := decodeCoverage(t, env.get(reachQuery(at, "r=8")))
	if def.Params.Model != dhm.CoverageModel {
		t.Fatalf("configured bullington: %+v", def.Params)
	}
	bull := decodeCoverage(t, env.get(reachQuery(at, "r=8", "model=bullington")))
	if def.Key != bull.Key || !bytes.Equal(rasterBytes(t, env, def), rasterBytes(t, env, bull)) {
		t.Fatal("the default is not Bullington's result")
	}
	if got := decodeCoverage(t, env.get(reachQuery(at, "r=8", "model=delta"))); got.Params.Model != dhm.CoverageModelDelta {
		t.Fatalf("requested delta: %+v", got.Params)
	}
	if cc := env.srv.coverageClientConfig(); cc.Model != "bullington" {
		t.Fatalf("client config default %q", cc.Model)
	}
}

// TestCoverage941Default_InvalidModelWarnsOnceAndUsesDelta: a junk value
// logs one warning that echoes nothing and the model is delta.
func TestCoverage941Default_InvalidModelWarnsOnceAndUsesDelta(t *testing.T) {
	logs := &dhmLogSink{}
	s := resolveCoverageSettings(CoverageConfig{Model: strp("itm<x>")}, logs.logf)
	out := logs.String()
	if s.model != dhm.PathDelta {
		t.Fatalf("junk model resolved to %q", s.model)
	}
	if n := strings.Count(out, "map.coverage.model"); n != 1 || strings.Contains(out, "itm") || !strings.Contains(out, "using delta") {
		t.Fatalf("want one warning naming the key and delta, echoing nothing; got %q", out)
	}
	logs = &dhmLogSink{}
	if s := resolveCoverageSettings(CoverageConfig{}, logs.logf); s.model != dhm.PathDelta || logs.String() != "" {
		t.Fatalf("absent model: %q, log %q", s.model, logs.String())
	}
}

// TestCoverage941Default_OpenAPINamesDelta: the documented default is delta.
func TestCoverage941Default_OpenAPINamesDelta(t *testing.T) {
	d := routeDescriptions()["GET "+coverageRoute]
	if !strings.Contains(d.Description, "default map.coverage.model (delta)") || strings.Contains(d.Description, "(bullington)") {
		t.Fatal("the route description does not name delta as the default")
	}
	for _, p := range d.QueryParams {
		if p.Name == "model" && (!strings.Contains(p.Description, "map.coverage.model, delta)") || strings.Contains(p.Description, "bullington)")) {
			t.Fatalf("model parameter: %q", p.Description)
		}
	}
}
