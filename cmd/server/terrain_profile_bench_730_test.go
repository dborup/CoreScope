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

// dokk1Shifted is the Dokk1 path moved i × 2 m east: a new cache key each
// time, past the 5-decimal snap.
func dokk1Shifted(i int) string {
	e := dokk1E + 2*float64(i%5000)
	return pathQuery(utmPt(e, dokk1N-138), utmPt(e, dokk1N+99))
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
}
