package main

import (
	"context"
	"testing"

	"github.com/meshcore-analyzer/copdem/copdemtest"
	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/dhm/dhmtest"
)

// #738 perf proof (AGENTS.md rule 0), end to end on the server side: one
// cold 80 km cross-border profile with model=both (DHM from the fake
// ElevationSource, the fill from the fake bucket at the real GLO-30
// layout on dhmtest.Terrain, then the response), and the per-request work
// once the same profile is cached. internal/copdem's
// BenchmarkFill80kmCrossBorder* isolates the fill.

func fillBenchPath() (dhm.LatLon, dhm.LatLon, dhm.Plan) {
	a := dhm.LatLon{Lat: 55.67610, Lon: 12.56830} // Copenhagen City Hall
	e, n := dhm.ToUTM32(a.Lat, a.Lon)
	lat, lon := dhm.FromUTM32(e+80000*0.9396926, n+80000*0.3420201) // 80 km at 70°: into Skåne
	b := dhm.LatLon{Lat: lat, Lon: lon}
	return a, b, dhm.PlanProfile(a, b, dhm.PlanOptions{MaxSamples: 3200, ChunkM: 2500})
}

func newFillBenchProfiler(bk *copdemtest.Bucket) *terrainProfiler {
	pc := dhm.ProfileConfig{Enabled: true, ForeignFill: dhm.ForeignFillConfig{Enabled: true}}
	return newTerrainProfiler(pc, nil, terrainProfilerDeps{src: newFakeElevation(nil, flatElev(fillDHMHeight)), fill: fillClient(bk), logf: func(string, ...interface{}) {}})
}

func BenchmarkTerrainProfileFill80kmCold(b *testing.B) {
	bk := copdemtest.New()
	b.Cleanup(bk.Close)
	bk.Generate = copdemtest.Synthetic(copdemtest.GLO30, dhmtest.Terrain)
	pa, pb, plan := fillBenchPath()
	ends := [2]terrainEnd{{pt: pa, kind: "point"}, {pt: pb, kind: "point"}}
	// Build the tiles outside the timing.
	if _, err := newFillBenchProfiler(bk).fetch(context.Background(), &plan, true); err != nil {
		b.Fatal(err)
	}
	reqs0, bytes0 := bk.Requests.Load(), bk.Bytes.Load()
	var resp TerrainProfileResponse
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		p := newFillBenchProfiler(bk) // empty caches
		b.StartTimer()
		e, err := p.fetch(context.Background(), &plan, true)
		if err != nil || e.fillFailed {
			b.Fatalf("fetch: %v, fill failed %v", err, e.fillFailed)
		}
		resp = p.buildResponse(terrainQuery{both: true}, ends, pa, pb, &plan, e)
	}
	b.StopTimer()
	if !resp.Coverage.Mixed || resp.Coverage.State != "full" {
		b.Fatalf("coverage %+v", resp.Coverage)
	}
	b.ReportMetric(float64(bk.Requests.Load()-reqs0)/float64(b.N), "fill-requests/op")
	b.ReportMetric(float64(bk.Bytes.Load()-bytes0)/float64(b.N), "fill-bytes/op")
	b.ReportMetric(float64(len(resp.Coverage.Fill)), "fill-ranges")
}

// The cached profile: decode the cache entry and build the mixed
// response, as every hit does.
func BenchmarkTerrainProfileFill80kmCached(b *testing.B) {
	bk := copdemtest.New()
	b.Cleanup(bk.Close)
	bk.Generate = copdemtest.Synthetic(copdemtest.GLO30, dhmtest.Terrain)
	pa, pb, plan := fillBenchPath()
	ends := [2]terrainEnd{{pt: pa, kind: "point"}, {pt: pb, kind: "point"}}
	p := newFillBenchProfiler(bk)
	e, err := p.fetch(context.Background(), &plan, true)
	if err != nil || e.fill == fillNone {
		b.Fatalf("fetch: %v, fill %d", err, e.fill)
	}
	enc := e.encode()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e, err := decodeTerrainElevation(enc, len(plan.E), true, false)
		if err != nil {
			b.Fatal(err)
		}
		if r := p.buildResponse(terrainQuery{both: true}, ends, pa, pb, &plan, e); !r.Coverage.Mixed {
			b.Fatal("not mixed")
		}
	}
}

// The same cold profile with the fill off: the baseline the fill adds to.
func BenchmarkTerrainProfileFill80kmColdFillOff(b *testing.B) {
	pa, pb, plan := fillBenchPath()
	ends := [2]terrainEnd{{pt: pa, kind: "point"}, {pt: pb, kind: "point"}}
	pc := dhm.ProfileConfig{Enabled: true}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		p := newTerrainProfiler(pc, nil, terrainProfilerDeps{src: newFakeElevation(nil, flatElev(fillDHMHeight))})
		b.StartTimer()
		e, err := p.fetch(context.Background(), &plan, true)
		if err != nil {
			b.Fatal(err)
		}
		if r := p.buildResponse(terrainQuery{both: true}, ends, pa, pb, &plan, e); r.Coverage.State != "partial" {
			b.Fatalf("coverage %+v", r.Coverage)
		}
	}
}
