package main

// #730 M3 — the follow-ups to the building classification from the #786
// review (F2, F4). Like the M2 suite, every test runs the real
// buildings.Client and Fetcher against stubOGC; nothing reaches
// Dataforsyningen.

import (
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dhm"
)

// TestTerrainBuildings_ProfileDeadlineDuringBuildingPhase (#786 review
// F4): the profile's own deadline running out while the footprints are
// still being asked for (the phase's deadline is longer) does not fail
// the profile. The rasters are in, so the answer is 200 with the same
// verdicts, the classification is unavailable, the failure is logged as
// a timeout, and the profile is cached: the next request is a hit that
// fetches no raster again.
func TestTerrainBuildings_ProfileDeadlineDuringBuildingPhase(t *testing.T) {
	ogc := newStubOGC(t, dokk1Footprint)
	ogc.hold = make(chan struct{}) // the footprint service never answers
	env := newBuildingsEnv(t, dokk1Ground, dokk1Surface, ogc, dhm.ProfileConfig{})
	env.p.deadline = 400 * time.Millisecond
	env.p.buildingsDeadline = time.Minute
	q := pathQuery(dokk1A, dokk1B)

	start := time.Now()
	w := env.get(q)
	took := time.Since(start)
	resp := decodeProfile(t, w)
	if took > 3*time.Second {
		t.Fatalf("took %v: the profile deadline did not end the building phase", took)
	}
	wantClassification(t, resp, "unavailable", false)
	wantCause(t, resp, "unknown")
	if resp.Attribution != dhm.AttributionDerived {
		t.Fatalf("attribution %q credits footprints that were not used", resp.Attribution)
	}
	sameVerdictAsOff(t, resp, "dokk1/0")
	if logs := env.logs.String(); !strings.Contains(logs, "[dhm] upstream terrain profile buildings failed (timeout)") {
		t.Fatalf("log %q, want the building phase's timeout", logs)
	}
	if r := env.src.calls.Load(); r != 2 {
		t.Fatalf("%d rasters, want 2", r)
	}
	if env.p.cache.Len() != 1 {
		t.Fatalf("%d cache entries, want the profile", env.p.cache.Len())
	}
	if w := env.get(q); w.Code != 200 || w.Header().Get("X-CoreScope-Cache") != "hit" {
		t.Fatalf("second request: %d %q, want a 200 hit", w.Code, w.Header().Get("X-CoreScope-Cache"))
	}
	if r := env.src.calls.Load(); r != 2 {
		t.Fatalf("%d rasters after the hit, want still 2", r)
	}
}
