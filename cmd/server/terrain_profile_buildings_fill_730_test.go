package main

// #730 M2 with the foreign fill of #738: a profile DHM does not cover end
// to end has no surface verdict, filled or not, so it is not classified
// and asks the footprint service nothing; the fill's retry on a cache hit
// keeps the classification it was cached with.

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/meshcore-analyzer/copdem"
	"github.com/meshcore-analyzer/dhm"
)

func TestTerrainBuildings_FilledProfileIsNotClassified(t *testing.T) {
	b := newFillBucket(t, fillTenth)
	var failing atomic.Bool
	failing.Store(true)
	fill := &scriptedFill{fn: func(ctx context.Context, p *dhm.Plan) (copdem.Result, error) {
		if failing.Load() {
			return copdem.Result{}, dhm.FetchError{Reason: "status 503"}
		}
		return fillClient(b).Fill(ctx, p)
	}}
	ogc := newStubOGC(t)
	src := newFakeElevation(t, flatElev(fillDHMHeight))
	src.surface = flatElev(fillDHMHeight + 18) // trees everywhere DHM is
	pc := dhm.ProfileConfig{RateLimitPerMin: intp(0), ForeignFill: dhm.ForeignFillConfig{Enabled: true}, Buildings: dhm.BuildingsConfig{Enabled: true}}
	set := resolveTerrainSettings(pc, func(string, ...interface{}) {})
	env := newTerrainEnvWith(t, src, pc, nil, func(d *terrainProfilerDeps) {
		d.fill = fill
		d.buildings = ogc.source(set)
	})
	q := pathQuery(cphPt, malmoPt)

	// The fill fails: partial, unclassified.
	resp := decodeProfile(t, env.get(q))
	if resp.Coverage.State != "partial" {
		t.Fatalf("coverage %+v", resp.Coverage)
	}
	wantClassification(t, resp, "unavailable", false)

	// A hit retries the fill, which now succeeds: mixed, and still the
	// classification the entry was cached with.
	failing.Store(false)
	w := env.get(q)
	resp = decodeProfile(t, w)
	if w.Header().Get("X-CoreScope-Cache") != "hit" || !resp.Coverage.Mixed || resp.Coverage.State != "full" {
		t.Fatalf("cache %q, coverage %+v", w.Header().Get("X-CoreScope-Cache"), resp.Coverage)
	}
	wantClassification(t, resp, "unavailable", false)
	if resp.Verdict.Surface == nil || resp.Verdict.Surface.State != "unknown" || resp.Verdict.Surface.Cause != "" {
		t.Fatalf("surface %+v", resp.Verdict.Surface)
	}
	for _, s := range resp.Samples {
		if s.Cls != "" {
			t.Fatalf("sample at %.0f m classified %q", s.D, s.Cls)
		}
	}
	if fill.count() != 2 || ogc.calls.Load() != 0 {
		t.Fatalf("%d fills, %d corridor requests; want 2, 0", fill.count(), ogc.calls.Load())
	}

	// A profile filled on its first fetch is not classified either.
	env2 := newTerrainEnvWith(t, src, pc, nil, func(d *terrainProfilerDeps) {
		d.fill = fillClient(b)
		d.buildings = ogc.source(set)
	})
	resp = decodeProfile(t, env2.get(pathQuery(malmoPt, cphPt)))
	if !resp.Coverage.Mixed {
		t.Fatalf("coverage %+v", resp.Coverage)
	}
	wantClassification(t, resp, "unavailable", false)
	if n := ogc.calls.Load(); n != 0 {
		t.Fatalf("%d corridor requests for a mixed profile", n)
	}
}
