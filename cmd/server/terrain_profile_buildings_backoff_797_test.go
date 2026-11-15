package main

// #797 review findings 2 and 5 — the building retry of a degraded
// profile is spaced: at most one per cached profile per
// terrainBuildingsRetryEvery, the failed miss counting as the first, and
// concurrent hits share it. A hit with no retry due is a plain hit: no
// wait, no admission slot and no per-client charge. Like the M2 and M3
// suites, every test runs the real buildings.Client and Fetcher against
// stubOGC.

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dhm"
)

// TestTerrainBuildings_RetryBacksOff counts the footprint calls while the
// service is down: one for the miss, then none until the cadence has
// passed, however often the profile is asked for, then exactly one.
func TestTerrainBuildings_RetryBacksOff(t *testing.T) {
	ogc := newStubOGC(t, dokk1Footprint)
	var failing atomic.Bool
	failing.Store(true)
	ogc.answer = func([4]float64) (int, []byte, bool) { return 503, nil, failing.Load() }
	env := newBuildingsEnv(t, dokk1Ground, dokk1Surface, ogc, dhm.ProfileConfig{})
	start := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	now := start
	env.p.now = func() time.Time { return now }
	q := pathQuery(dokk1A, dokk1B)

	at := func(after time.Duration, hits int, wantCache, wantState string, wantCalls int32) {
		t.Helper()
		now = start.Add(after)
		for i := 0; i < hits; i++ {
			w := env.get(q)
			resp := decodeProfile(t, w)
			if w.Header().Get("X-CoreScope-Cache") != wantCache || resp.Classification == nil || resp.Classification.State != wantState {
				t.Fatalf("at +%v, request %d: cache %q, classification %+v; want %s, %s", after, i, w.Header().Get("X-CoreScope-Cache"), resp.Classification, wantCache, wantState)
			}
		}
		if n, r := ogc.calls.Load(), env.src.calls.Load(); n != wantCalls || r != 2 {
			t.Fatalf("at +%v: %d corridor calls and %d rasters, want %d and 2", after, n, r, wantCalls)
		}
	}
	every := terrainBuildingsRetryEvery
	at(0, 1, "miss", "unavailable", 1)
	// The miss was the try: hits inside the cadence ask nothing.
	at(0, 20, "hit", "unavailable", 1)
	at(every-time.Second, 20, "hit", "unavailable", 1)
	// Due: the first hit retries once, the rest of the burst does not.
	at(every, 20, "hit", "unavailable", 2)
	at(2*every-time.Second, 20, "hit", "unavailable", 2)
	at(2*every, 20, "hit", "unavailable", 3)
	// Back up: the next due retry classifies, and that is the last call.
	failing.Store(false)
	at(3*every-time.Second, 1, "hit", "unavailable", 3)
	at(3*every, 1, "hit", "full", 4)
	at(4*every, 20, "hit", "full", 4)
}

// TestTerrainBuildings_HitDuringOutageIsFree is the review's probe: the
// footprint service hangs, maxInFlight is 1 and the per-client budget 2.
// A hit on the degraded profile inside the cadence is served at once,
// holds no slot and is not charged, so another path's miss still gets
// its 200.
func TestTerrainBuildings_HitDuringOutageIsFree(t *testing.T) {
	ogc := newStubOGC(t, dokk1Footprint)
	ogc.hold = make(chan struct{}) // the footprint service never answers
	env := newBuildingsEnv(t, dokk1Ground, dokk1Surface, ogc, dhm.ProfileConfig{MaxInFlight: intp(1), RateLimitPerMin: intp(2)})
	env.p.buildingsDeadline = 300 * time.Millisecond
	q := pathQuery(dokk1A, dokk1B)

	wantClassification(t, decodeProfile(t, env.get(q)), "unavailable", false) // charge 1
	calls := ogc.calls.Load()
	for i := 0; i < 5; i++ {
		start := time.Now()
		w := env.get(q)
		took := time.Since(start)
		if w.Code != 200 || w.Header().Get("X-CoreScope-Cache") != "hit" {
			t.Fatalf("hit %d: %d %q", i, w.Code, w.Header().Get("X-CoreScope-Cache"))
		}
		if took > 150*time.Millisecond {
			t.Fatalf("hit %d took %v: it waited for the footprint service", i, took)
		}
	}
	if n := ogc.calls.Load(); n != calls {
		t.Fatalf("%d corridor calls after the hits, want the miss's %d", n, calls)
	}
	if n := len(env.p.admit); n != 0 {
		t.Fatalf("%d admission slots held after the hits, want 0", n)
	}
	// Charge 2: the hits charged nothing, so this miss is admitted.
	if w := env.get(pathQuery(forestA, forestB)); w.Code != 200 {
		t.Fatalf("another path: %d, want 200", w.Code)
	}
}

// TestTerrainBuildings_ConcurrentHitsShareOneRetry: while a due retry
// waits on the footprint service, the hits that arrive are served the
// cached profile at once and start no retry of their own.
func TestTerrainBuildings_ConcurrentHitsShareOneRetry(t *testing.T) {
	ogc := newStubOGC(t, dokk1Footprint)
	ogc.hold = make(chan struct{})
	env := newBuildingsEnv(t, dokk1Ground, dokk1Surface, ogc, dhm.ProfileConfig{})
	env.p.buildingsDeadline = 300 * time.Millisecond
	start := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	now := start
	env.p.now = func() time.Time { return now }
	q := pathQuery(dokk1A, dokk1B)

	wantClassification(t, decodeProfile(t, env.get(q)), "unavailable", false)
	if n := ogc.calls.Load(); n != 1 {
		t.Fatalf("%d corridor calls on the miss, want 1", n)
	}
	now = start.Add(terrainBuildingsRetryEvery) // due; not changed again
	env.p.buildingsDeadline = time.Minute
	first := make(chan TerrainProfileResponse, 1)
	go func() { first <- decodeProfile(t, env.get(q)) }()
	waitUntil(t, "the retry at the footprint service", func() bool { return ogc.running.Load() == 1 })
	for i := 0; i < 8; i++ {
		done := make(chan TerrainProfileResponse, 1)
		go func() { done <- decodeProfile(t, env.get(q)) }()
		select {
		case resp := <-done:
			wantClassification(t, resp, "unavailable", false)
		case <-time.After(2 * time.Second):
			close(ogc.hold)
			t.Fatalf("hit %d waited for the running retry", i)
		}
	}
	if n := ogc.calls.Load(); n != 2 {
		t.Fatalf("%d corridor calls, want 2: the hits started retries of their own", n)
	}
	close(ogc.hold) // the service answers: the one retry classifies
	wantClassification(t, <-first, "full", false)
	wantClassification(t, decodeProfile(t, env.get(q)), "full", false)
	if n := ogc.calls.Load(); n != 2 {
		t.Fatalf("%d corridor calls after the retry, want still 2", n)
	}
}

// TestTerrainBuildings_RetryEndingTruncatedIsStored (#797 review finding
// 5, mutant S3): a retry whose answer is no longer degraded but partial —
// a segment with more buildings than a page holds — replaces the entry
// like a full one. It is deterministic, so it is never retried again.
func TestTerrainBuildings_RetryEndingTruncatedIsStored(t *testing.T) {
	ogc := newStubOGC(t)
	var mode atomic.Int32 // 0: down; 1: the first segment is over a page
	ogc.answer = func(line [4]float64) (int, []byte, bool) {
		if mode.Load() == 0 {
			return 503, nil, true
		}
		return 200, []byte(`{"type":"FeatureCollection","features":[],"numberMatched":1500,"numberReturned":0}`), firstSegment(line)
	}
	env := newBuildingsEnv(t, forestGround, fifteenSurface, ogc, dhm.ProfileConfig{})
	start := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	now := start
	env.p.now = func() time.Time { return now }
	q := pathQuery(fifteenA, fifteenB)

	wantClassification(t, decodeProfile(t, env.get(q)), "unavailable", false)
	if n := ogc.calls.Load(); n != 2 {
		t.Fatalf("%d corridor calls on the miss, want 2", n)
	}
	mode.Store(1)
	now = start.Add(terrainBuildingsRetryEvery)
	resp := decodeProfile(t, env.get(q))
	wantClassification(t, resp, "partial", true)
	if n := ogc.calls.Load(); n != 4 {
		t.Fatalf("%d corridor calls after the retry, want 4", n)
	}
	for _, after := range []time.Duration{2, 5} {
		now = start.Add(after * terrainBuildingsRetryEvery)
		w := env.get(q)
		wantClassification(t, decodeProfile(t, w), "partial", true)
		if w.Header().Get("X-CoreScope-Cache") != "hit" {
			t.Fatalf("at +%v: cache %q", after*terrainBuildingsRetryEvery, w.Header().Get("X-CoreScope-Cache"))
		}
		if n := ogc.calls.Load(); n != 4 {
			t.Fatalf("at +%v: %d corridor calls, want still 4: the truncated answer was not stored", after*terrainBuildingsRetryEvery, n)
		}
	}
}

// TestTerrainRetryGate_Bounded: the gate holds at most max profiles. A
// full gate makes room by dropping the holds that have run out, and with
// none to drop it refuses a new retry rather than grow.
func TestTerrainRetryGate_Bounded(t *testing.T) {
	g := newTerrainRetryGate(time.Minute, 2)
	t0 := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	g.hold("a", t0)
	g.hold("b", t0)
	g.hold("c", t0) // no room
	if n := g.len(); n != 2 {
		t.Fatalf("%d held, want 2", n)
	}
	if g.claim("a", t0.Add(30*time.Second)) {
		t.Fatal("a claimed inside its cadence")
	}
	if g.claim("c", t0.Add(30*time.Second)) {
		t.Fatal("c claimed with the gate full: a retry outside the bound")
	}
	if !g.claim("c", t0.Add(time.Minute)) {
		t.Fatal("c refused once a and b had run out")
	}
	if n := g.len(); n != 1 {
		t.Fatalf("%d held after the prune, want c alone", n)
	}
	g.release("c")
	if !g.claim("c", t0.Add(time.Minute)) {
		t.Fatal("c refused after its release")
	}
	if terrainBuildingsRetryEvery != 10*time.Minute {
		t.Fatalf("cadence %v, want M2's 10 minutes", terrainBuildingsRetryEvery)
	}
}
