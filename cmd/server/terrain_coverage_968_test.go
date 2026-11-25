package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// #968 M3 — the viewer's default environment loss (Customize → Display →
// Predicted reach) is client-only, but it must use the bounds the server
// applies to the operator's map.coverage.environmentLossDb. The customizer
// reads them from PredictedReach.BOUNDS.env in public/coverage.js; this
// pins that array to the server's resolver.

// TestCoverage968_EnvBoundsMatchTheClient: BOUNDS.env in coverage.js is
// exported, and its [min, max] is exactly the range resolveCoverageSettings
// accepts: both ends kept, a step past either end the default.
func TestCoverage968_EnvBoundsMatchTheClient(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "public", "coverage.js"))
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)var BOUNDS = \{(.*?)\};`).FindSubmatch(src)
	if block == nil {
		t.Fatal("no BOUNDS in coverage.js")
	}
	m := regexp.MustCompile(`\benv: \[(-?[0-9.]+), (-?[0-9.]+), ([0-9]+)\]`).FindSubmatch(block[1])
	if m == nil {
		t.Fatal("no env entry in coverage.js BOUNDS")
	}
	lo, _ := strconv.ParseFloat(string(m[1]), 64)
	hi, _ := strconv.ParseFloat(string(m[2]), 64)
	if !regexp.MustCompile(`(?s)global\.PredictedReach = \{.*\n\s*BOUNDS: BOUNDS,`).Match(src) {
		t.Fatal("PredictedReach does not export BOUNDS: the customizer would have to copy the bounds")
	}
	resolve := func(v float64) float64 {
		return resolveCoverageSettings(CoverageConfig{EnvironmentLossDb: floatp(v)}, func(string, ...interface{}) {}).envLossDb
	}
	// The probes must differ from the default, or a rejection would look
	// like an accepted value.
	for _, c := range []struct {
		v      float64
		kept   bool
		reason string
	}{
		{lo, true, "the client's minimum"},
		{hi, true, "the client's maximum"},
		{lo - 0.5, false, "below the client's minimum"},
		{hi + 0.5, false, "above the client's maximum"},
	} {
		if c.v == coverageEnvLossDefaultDb {
			t.Fatalf("probe %v is the default; pick another bound", c.v)
		}
		got := resolve(c.v)
		if c.kept && got != c.v {
			t.Fatalf("%s %v: the server resolves %v (client BOUNDS.env [%v, %v])", c.reason, c.v, got, lo, hi)
		}
		if !c.kept && got != coverageEnvLossDefaultDb {
			t.Fatalf("%s %v: the server keeps %v, the client refuses it (BOUNDS.env [%v, %v])", c.reason, c.v, got, lo, hi)
		}
	}
}
