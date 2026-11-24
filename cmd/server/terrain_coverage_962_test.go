package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #962 — the environment loss of the predicted reach: a term of the
// browser's link budget for the buildings, trees and other clutter the
// terrain model does not include. The server only resolves the operator's
// value (map.coverage.environmentLossDb, canonical key only, 0–60 dB,
// default 25) and passes it to the client config; the raster, the losses,
// the server's own counts and the validation harness stay raw.

// TestCoverage962_EnvironmentLossDefaultAndBounds: 25 dB by default, 0 and
// 60 accepted, anything else one warning and the default.
func TestCoverage962_EnvironmentLossDefaultAndBounds(t *testing.T) {
	logs := &dhmLogSink{}
	if got := resolveCoverageSettings(CoverageConfig{}, logs.logf).envLossDb; got != 25 || logs.String() != "" {
		t.Fatalf("default %v (log %q)", got, logs.String())
	}
	for _, v := range []float64{0, 12.5, 60} {
		logs := &dhmLogSink{}
		if got := resolveCoverageSettings(CoverageConfig{EnvironmentLossDb: floatp(v)}, logs.logf).envLossDb; got != v || logs.String() != "" {
			t.Fatalf("%v: got %v (log %q)", v, got, logs.String())
		}
	}
	for _, v := range []float64{-0.5, 60.5, 1000, math.NaN(), math.Inf(1)} {
		logs := &dhmLogSink{}
		got := resolveCoverageSettings(CoverageConfig{EnvironmentLossDb: floatp(v)}, logs.logf).envLossDb
		if got != 25 {
			t.Fatalf("%v: got %v, want the default 25", v, got)
		}
		if n := strings.Count(logs.String(), "map.coverage.environmentLossDb"); n != 1 {
			t.Fatalf("%v: %d warnings, want 1: %q", v, n, logs.String())
		}
	}
}

// TestCoverage962_EnvironmentLossReadsTheCanonicalKeyOnly: a case variant
// never sets the value, whatever the order of the keys, and is reported
// once with the other ignored keys (#884).
func TestCoverage962_EnvironmentLossReadsTheCanonicalKeyOnly(t *testing.T) {
	cases := []struct {
		name, block string
		want        float64
		warnings    int
	}{
		{"canonical", `"environmentLossDb":7`, 7, 0},
		{"canonical 0", `"environmentLossDb":0`, 0, 0},
		{"EnvironmentLossDb alone", `"EnvironmentLossDb":7`, 25, 1},
		{"variant after canonical", `"environmentLossDb":7,"ENVIRONMENTLOSSDB":30`, 7, 1},
		{"variant before canonical", `"environmentlossdb":30,"environmentLossDb":7`, 7, 1},
		{"variant 0 after canonical", `"environmentLossDb":7,"EnvironmentLossDb":0`, 7, 1},
		{"absent", `"radiusKm":20`, 25, 0},
		{"null", `"environmentLossDb":null`, 25, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, logged := loadConfigLogged(t, `{"map":{"coverage":{"enabled":true,`+tc.block+`}}}`)
			if !cfg.Coverage.Enabled {
				t.Fatal("the reach is off")
			}
			if got := resolveCoverageSettings(cfg.Coverage, quietLogf).envLossDb; got != tc.want {
				t.Fatalf("environment loss %v, want %v", got, tc.want)
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

// TestCoverage962_ClientConfigCarriesTheEnvironmentLoss: the operator's
// value reaches /api/config/client beside the other coverage defaults,
// and the radio block (the raw budget) is unchanged by it.
func TestCoverage962_ClientConfigCarriesTheEnvironmentLoss(t *testing.T) {
	for _, tc := range []struct {
		cc   CoverageConfig
		want float64
	}{{CoverageConfig{}, 25}, {CoverageConfig{EnvironmentLossDb: floatp(0)}, 0}, {CoverageConfig{EnvironmentLossDb: floatp(35)}, 35}} {
		env := newCoverageEnv(t, newFakeElevation(t, flatElev(4)), tc.cc, nil, nil)
		cc := env.srv.coverageClientConfig()
		if cc.EnvironmentLossDb != tc.want {
			t.Fatalf("client environmentLossDb %v, want %v", cc.EnvironmentLossDb, tc.want)
		}
		if cc.Radio != (CoverageRadio{TxPowerDbm: 22, AntennaGainDbi: 3, RxGainDbi: 3, LossDb: 0, SensitivityDbm: -130}) {
			t.Fatalf("radio %+v", cc.Radio)
		}
		b, _ := json.Marshal(cc)
		want := `"environmentLossDb":` + reachEnvJSON(tc.want)
		if !strings.Contains(string(b), want) {
			t.Fatalf("client config JSON %s lacks %s", b, want)
		}
	}
}

func reachEnvJSON(v float64) string { b, _ := json.Marshal(v); return string(b) }

// TestCoverage962_ServerLossesStayRaw: the environment loss changes
// nothing the server computes or serves — the cache key, the raster
// bytes, the nodes' losses, the reach's budgetDb and counts.
func TestCoverage962_ServerLossesStayRaw(t *testing.T) {
	type served struct {
		key    string
		raster []byte
		reach  []byte
	}
	get := func(envLoss *float64) served {
		env := newCoverageEnv(t, newFakeElevation(t, hillyElev), CoverageConfig{EnvironmentLossDb: envLoss}, nil, nil)
		resp := decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "r=5")))
		w := env.raster(resp.Raster.URL, false)
		if w.Code != 200 {
			t.Fatalf("raster %d", w.Code)
		}
		reach, _ := json.Marshal(resp.Reach)
		return served{resp.Key, w.Body.Bytes(), reach}
	}
	base := get(floatp(0))
	if len(base.raster) == 0 {
		t.Fatal("empty raster")
	}
	for _, v := range []*float64{nil, floatp(25), floatp(60)} {
		got := get(v)
		if got.key != base.key || !bytes.Equal(got.raster, base.raster) || !bytes.Equal(got.reach, base.reach) {
			t.Fatalf("environment loss %v changed what the server serves: key %s/%s, raster equal %v, reach %s vs %s",
				v, got.key, base.key, bytes.Equal(got.raster, base.raster), got.reach, base.reach)
		}
	}
}

// TestCoverage962_ValidationReportStaysRaw: the harness scores raw losses
// at the operator's radio budget, and its report names the environment
// loss the panel applies by default without applying it.
func TestCoverage962_ValidationReportStaysRaw(t *testing.T) {
	c := &terrainCoverage{
		set:   resolveCoverageSettings(CoverageConfig{EnvironmentLossDb: floatp(35)}, quietLogf),
		radio: resolveCoverageRadio(nil, quietLogf),
		link: &terrainProfiler{radio: resolveTerrainRadio(nil, quietLogf), now: time.Now, logf: quietLogf,
			observers: func() ([]Observer, error) { return nil, nil }},
	}
	rep := buildReachValidationReport(&reachValidationInput{asOf: time.Now()}, c, nil,
		reachValidationOptions{radiusM: 40000, window: 7 * 24 * time.Hour, minEdgeCount: 3}, 0, 0)
	if rep.Params.BudgetDb != 158 {
		t.Fatalf("budgetDb %v, want the raw 158", rep.Params.BudgetDb)
	}
	if rep.Params.EnvironmentLossDb != 35 {
		t.Fatalf("environmentLossDb %v, want 35", rep.Params.EnvironmentLossDb)
	}
	md := rep.markdown()
	if !strings.Contains(md, "environment loss 35 dB, not applied here") {
		t.Fatalf("the markdown does not name the environment loss:\n%s", md)
	}
	if !strings.Contains(strings.Join(rep.Notes, "\n"), "map.coverage.environmentLossDb") {
		t.Fatalf("notes %q", rep.Notes)
	}
}

// TestCoverage962_Documented: the example config documents the key at its
// default, the OpenAPI text says the browser subtracts it and the server's
// budgetDb does not, and the validation doc says the harness stays raw.
func TestCoverage962_Documented(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := loadConfigLogged(t, string(data))
	if v := cfg.Coverage.EnvironmentLossDb; v == nil || *v != coverageEnvLossDefaultDb {
		t.Fatalf("config.example.json map.coverage.environmentLossDb %v, want %v", v, coverageEnvLossDefaultDb)
	}
	if !strings.Contains(string(data), `"_comment_environmentLossDb"`) {
		t.Fatal("config.example.json does not explain environmentLossDb")
	}
	d := routeDescriptions()["GET "+coverageRoute].Description
	if !strings.Contains(d, "coverage.environmentLossDb") || !strings.Contains(d, "map.coverage.environmentLossDb") {
		t.Fatalf("OpenAPI does not describe the environment loss: %s", d)
	}
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "predicted-reach-validation.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "map.coverage.environmentLossDb") || !strings.Contains(string(doc), "raw") {
		t.Fatal("docs/predicted-reach-validation.md does not say the harness ignores the environment loss")
	}
}
