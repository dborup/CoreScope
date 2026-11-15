package main

// #730 M3 — the follow-ups to the building classification from the #786
// review (F2, F4). Like the M2 suite, every test runs the real
// buildings.Client and Fetcher against stubOGC; nothing reaches
// Dataforsyningen.

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
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

// TestTerrainBuildings_FailedLookupRetriesOnlyTheBuildings (#786 review
// F2): a profile whose footprint lookup failed is cached for the
// profile's TTL like any other, and a hit on it asks the footprint
// service again, on the cached rasters: no raster is fetched twice,
// however long the service stays down or the profile is asked for. The
// first classified answer replaces the entry, and later hits ask nothing.
func TestTerrainBuildings_FailedLookupRetriesOnlyTheBuildings(t *testing.T) {
	ogc := newStubOGC(t, dokk1Footprint)
	var failing atomic.Bool
	failing.Store(true)
	ogc.answer = func([4]float64) (int, []byte, bool) { return 503, nil, failing.Load() }
	env := newBuildingsEnv(t, dokk1Ground, dokk1Surface, ogc, dhm.ProfileConfig{})
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	env.p.now = func() time.Time { return now }
	q := pathQuery(dokk1A, dokk1B)

	get := func(wantCache, wantState string, wantCalls int32) TerrainProfileResponse {
		t.Helper()
		w := env.get(q)
		resp := decodeProfile(t, w)
		if w.Header().Get("X-CoreScope-Cache") != wantCache || resp.Classification == nil || resp.Classification.State != wantState {
			t.Fatalf("at %v: cache %q, classification %+v; want %s, %s", now, w.Header().Get("X-CoreScope-Cache"), resp.Classification, wantCache, wantState)
		}
		if n, r := ogc.calls.Load(), env.src.calls.Load(); n != wantCalls || r != 2 {
			t.Fatalf("at %v: %d corridor calls and %d rasters, want %d and 2", now, n, r, wantCalls)
		}
		return resp
	}
	get("miss", "unavailable", 1)
	// Still down: each hit asks again, and serves the cached profile.
	get("hit", "unavailable", 2)
	now = now.Add(11 * time.Minute) // past M2's 10-minute retry TTL
	get("hit", "unavailable", 3)
	failing.Store(false)
	resp := get("hit", "full", 4)
	wantCause(t, resp, "building")
	sameVerdictAsOff(t, resp, "dokk1/0")
	now = now.Add(time.Hour)
	get("hit", "full", 4)
}

// TestTerrainBuildings_RetryAsksOnlyTheFailedSegment: of a 15 km
// profile's two segments, the one that failed is asked again on a hit;
// the other's page comes from the Fetcher's cache.
func TestTerrainBuildings_RetryAsksOnlyTheFailedSegment(t *testing.T) {
	ogc := newStubOGC(t)
	var failing atomic.Bool
	failing.Store(true)
	ogc.answer = func(line [4]float64) (int, []byte, bool) { return 503, nil, failing.Load() && firstSegment(line) }
	env := newBuildingsEnv(t, forestGround, fifteenSurface, ogc, dhm.ProfileConfig{})
	q := pathQuery(fifteenA, fifteenB)

	resp := decodeProfile(t, env.get(q))
	wantClassification(t, resp, "partial", false)
	if n := ogc.calls.Load(); n != 2 {
		t.Fatalf("%d corridor calls on the miss, want 2", n)
	}
	rasters := env.src.calls.Load()
	failing.Store(false)
	w := env.get(q)
	resp = decodeProfile(t, w)
	if w.Header().Get("X-CoreScope-Cache") != "hit" {
		t.Fatalf("cache %q", w.Header().Get("X-CoreScope-Cache"))
	}
	wantClassification(t, resp, "full", false)
	wantCause(t, resp, "vegetation")
	if n, r := ogc.calls.Load(), env.src.calls.Load(); n != 3 || r != rasters {
		t.Fatalf("%d corridor calls and %d rasters, want 3 (the failed segment again) and the miss's %d", n, r, rasters)
	}
	var lines []string
	for _, r := range ogc.requests()[2:] {
		lines = append(lines, r.URL.Query().Get("filter"))
	}
	if m := stubLineRe.FindStringSubmatch(lines[0]); m == nil || !firstSegment(lineOf(m)) {
		t.Fatalf("the retry asked %v, want the first segment only", lines)
	}
}

func lineOf(m []string) [4]float64 {
	var line [4]float64
	for i := range line {
		line[i], _ = strconv.ParseFloat(m[i+1], 64)
	}
	return line
}

// TestTerrainBuildings_RetryIsBudgeted: the retry is upstream work, so it
// is charged to the client like a miss and holds a profile admission
// slot. A retry the budget or the admission refuses is not made: the
// cached profile is served, 200, without a call, and the refused
// admission gives its charge back.
func TestTerrainBuildings_RetryIsBudgeted(t *testing.T) {
	t.Run("per client", func(t *testing.T) {
		ogc := newStubOGC(t, dokk1Footprint)
		ogc.answer = func([4]float64) (int, []byte, bool) { return 503, nil, true }
		env := newBuildingsEnv(t, dokk1Ground, dokk1Surface, ogc, dhm.ProfileConfig{RateLimitPerMin: intp(2)})
		q := pathQuery(dokk1A, dokk1B)
		for i, want := range []int32{1, 2, 2, 2} {
			w := env.get(q)
			if w.Code != 200 {
				t.Fatalf("request %d: %d, want 200 from the cache", i, w.Code)
			}
			if n := ogc.calls.Load(); n != want {
				t.Fatalf("request %d: %d corridor calls, want %d", i, n, want)
			}
		}
	})
	t.Run("admission", func(t *testing.T) {
		ogc := newStubOGC(t, dokk1Footprint)
		var failing atomic.Bool
		failing.Store(true)
		ogc.answer = func([4]float64) (int, []byte, bool) { return 503, nil, failing.Load() }
		env := newBuildingsEnv(t, dokk1Ground, dokk1Surface, ogc, dhm.ProfileConfig{MaxInFlight: intp(1), RateLimitPerMin: intp(3)})
		q := pathQuery(dokk1A, dokk1B)
		decodeProfile(t, env.get(q)) // charge 1
		failing.Store(false)
		release := make(chan struct{})
		env.src.hook = func(ctx context.Context, r dhm.RasterRequest) error {
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		other := make(chan int, 1)
		go func() { other <- env.get(pathQuery(forestA, forestB)).Code }() // charge 2, holds the slot
		waitUntil(t, "another profile in the slot", func() bool { return len(env.p.admit) == 1 })
		w := env.get(q) // charge 3, refused by the admission and given back
		resp := decodeProfile(t, w)
		wantClassification(t, resp, "unavailable", false)
		if n := ogc.calls.Load(); n != 1 {
			t.Fatalf("%d corridor calls, want 1: the retry ran without a slot", n)
		}
		close(release)
		if c := <-other; c != 200 {
			t.Fatalf("other profile: %d", c)
		}
		env.src.hook = nil
		// The refund left one charge: the retry runs and classifies.
		w = env.get(q)
		resp = decodeProfile(t, w)
		wantClassification(t, resp, "full", false)
		if n := ogc.calls.Load(); n < 2 {
			t.Fatalf("%d corridor calls, want the retry", n)
		}
	})
}

// TestTerrainBuildings_RetryKeepsTheProfileTTL: a retry that fails again
// does not store the profile anew, so the rasters still expire on the
// profile's TTL from the miss that fetched them, however often the
// degraded profile is asked for.
func TestTerrainBuildings_RetryKeepsTheProfileTTL(t *testing.T) {
	ogc := newStubOGC(t, dokk1Footprint)
	ogc.answer = func([4]float64) (int, []byte, bool) { return 503, nil, true }
	env := newBuildingsEnv(t, dokk1Ground, dokk1Surface, ogc, dhm.ProfileConfig{})
	start := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	now := start
	env.p.now = func() time.Time { return now }
	q := pathQuery(dokk1A, dokk1B)
	for _, c := range []struct {
		at    time.Duration
		cache string
	}{{0, "miss"}, {env.p.set.cacheTTL - time.Minute, "hit"}, {env.p.set.cacheTTL + time.Second, "miss"}} {
		now = start.Add(c.at)
		if w := env.get(q); w.Code != 200 || w.Header().Get("X-CoreScope-Cache") != c.cache {
			t.Fatalf("at +%v: %d %q, want %s", c.at, w.Code, w.Header().Get("X-CoreScope-Cache"), c.cache)
		}
	}
}
