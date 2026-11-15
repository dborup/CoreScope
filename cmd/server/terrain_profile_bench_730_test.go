package main

// #730 M2 perf proof: the profile route's median latency on a cache hit
// and on a cold miss, against fake upstreams (no network beyond
// localhost). Run with
//
//	go test -run '^$' -bench BenchmarkTerrainProfileP50 -benchtime 400x .
//
// p50-µs is the median of the requests' wall times.

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dhm"
)

// benchTerrainP50 serves query(i) for b.N requests and reports their
// median wall time. Every answer must be a 200 with X-CoreScope-Cache
// wantCache.
func benchTerrainP50(b *testing.B, env *terrainEnv, query func(i int) string, wantCache string) {
	b.Helper()
	times := make([]time.Duration, b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		w := env.get(query(i))
		times[i] = time.Since(start)
		if w.Code != 200 || w.Header().Get("X-CoreScope-Cache") != wantCache {
			b.Fatalf("request %d: %d, cache %q: %s", i, w.Code, w.Header().Get("X-CoreScope-Cache"), w.Body.String())
		}
	}
	b.StopTimer()
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	b.ReportMetric(float64(times[len(times)/2].Microseconds()), "p50-µs")
}

// benchTerrainEnv is a profiler on the Dokk1 fakes with no per-client
// limit, so a run of misses is never refused. The test helpers want a
// *testing.T; a bare one only drops the fake's own checks, which the
// tests run, and benchTerrainP50 checks every answer.
func benchTerrainEnv(b *testing.B, pc dhm.ProfileConfig) *terrainEnv {
	t := &testing.T{}
	src := newFakeElevation(t, dokk1Ground)
	src.surface = dokk1Surface
	pc.RateLimitPerMin = intp(0)
	return newTerrainEnv(t, src, pc, nil)
}

// dokk1Shifted is the Dokk1 path moved by 2 m steps, up to 18 m east and
// 78 m north: 400 distinct cache keys past the 5-decimal snap, all over
// Dokk1's roof, so every miss with the classification on asks for
// footprints.
func dokk1Shifted(i int) string {
	e := dokk1E + 2*float64(i%10)
	n := dokk1N + 2*float64(i/10%40)
	return pathQuery(utmPt(e, n-138), utmPt(e, n+99))
}

func BenchmarkTerrainProfileP50(b *testing.B) {
	b.Run("off/hit", func(b *testing.B) {
		env := benchTerrainEnv(b, dhm.ProfileConfig{})
		q := pathQuery(dokk1A, dokk1B)
		env.get(q)
		benchTerrainP50(b, env, func(int) string { return q }, "hit")
	})
	b.Run("off/miss", func(b *testing.B) {
		env := benchTerrainEnv(b, dhm.ProfileConfig{CacheEntries: intp(4096)})
		benchTerrainP50(b, env, dokk1Shifted, "miss")
	})
	// With the building classification (#730 M2), on a local stub of the
	// footprint service: a hit reads the classes from the entry; a miss
	// also asks for and classifies the corridor's footprints.
	b.Run("on/hit", func(b *testing.B) {
		env := benchBuildingsEnv(b, dhm.ProfileConfig{})
		q := pathQuery(dokk1A, dokk1B)
		benchWantBuilding(b, env, q)
		benchTerrainP50(b, env.terrainEnv, func(int) string { return q }, "hit")
	})
	// #730 M3 (#786 review F2): with the footprint service down, a hit on
	// the degraded profile retries the building phase alone, on the
	// cached rasters, at most once per terrainBuildingsRetryEvery (#797).
	// Before M3 such a profile was a plain hit for 10 minutes and then a
	// full miss (on/miss), rasters included. on/hit-degraded is a hit
	// inside the cadence, on/hit-degraded-due one with a retry due.
	degraded := func(b *testing.B, every time.Duration, wantCalls func(n int32) bool) {
		env := benchBuildingsEnv(b, dhm.ProfileConfig{})
		env.ogc.answer = func([4]float64) (int, []byte, bool) { return 503, nil, true }
		env.p.buildingsRetry = newTerrainRetryGate(every, env.p.set.cacheEntries)
		q := pathQuery(dokk1A, dokk1B)
		env.get(q)
		rasters, calls := env.src.calls.Load(), env.ogc.calls.Load()
		benchTerrainP50(b, env.terrainEnv, func(int) string { return q }, "hit")
		if r := env.src.calls.Load(); r != rasters {
			b.Fatalf("%d rasters fetched by hits", r-rasters)
		}
		if n := env.ogc.calls.Load() - calls; !wantCalls(n) {
			b.Fatalf("%d footprint requests for %d hits", n, b.N)
		}
	}
	b.Run("on/hit-degraded", func(b *testing.B) {
		degraded(b, terrainBuildingsRetryEvery, func(n int32) bool { return n == 0 })
	})
	b.Run("on/hit-degraded-due", func(b *testing.B) {
		degraded(b, 0, func(n int32) bool { return n > 0 })
	})
	b.Run("on/miss", func(b *testing.B) {
		if b.N > 400 {
			b.Fatal("at most 400 distinct paths")
		}
		env := benchBuildingsEnv(b, dhm.ProfileConfig{CacheEntries: intp(4096)})
		benchTerrainP50(b, env.terrainEnv, dokk1Shifted, "miss")
		if n := env.ogc.calls.Load(); int(n) != b.N {
			b.Fatalf("%d footprint requests for %d misses", n, b.N)
		}
	})
}

// benchBuildingsEnv is benchTerrainEnv with the classification on, on a
// stub footprint service holding Dokk1.
func benchBuildingsEnv(b *testing.B, pc dhm.ProfileConfig) *buildingsEnv {
	t := &testing.T{}
	ogc := newStubOGC(t, dokk1Footprint)
	b.Cleanup(ogc.srv.Close)
	return newBuildingsEnv(t, dokk1Ground, dokk1Surface, ogc, pc)
}

// benchWantBuilding fetches q once and checks it is classified.
func benchWantBuilding(b *testing.B, env *buildingsEnv, q string) {
	w := env.get(q)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"cause":"building"`) {
		b.Fatalf("%d: %s", w.Code, w.Body.String())
	}
}
