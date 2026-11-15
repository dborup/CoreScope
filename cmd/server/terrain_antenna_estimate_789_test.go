package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dhm"
)

// #789 M1 — the antenna height estimated from the surface model: the
// precedence, the role rule, the per-node LRU and its key, the bound on
// upstream calls, and the #743 hidden-node case. Upstream is a fake
// ElevationSource only; no token is involved.

// surfaceFake serves a DSM standing surfaceM above a flat DTM, and counts
// the rasters per call.
type surfaceFake struct {
	mu       sync.Mutex
	surfaceM float32
	err      error
	delay    time.Duration
	calls    atomic.Int64
	reqs     []dhm.RasterRequest
}

func (f *surfaceFake) FetchRaster(ctx context.Context, r dhm.RasterRequest) (dhm.Grid, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.reqs = append(f.reqs, r)
	h, err, delay := f.surfaceM, f.err, f.delay
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return dhm.Grid{}, ctx.Err()
		}
	}
	if err != nil {
		return dhm.Grid{}, err
	}
	v := float32(30)
	if r.Model == dhm.ModelSurface {
		v += h
	}
	g := dhm.Grid{Width: r.Width, Height: r.Height, Values: make([]float32, r.Width*r.Height),
		Georef: dhm.Georef{OriginE: r.BBox[0], OriginN: r.BBox[3],
			PixelE: (r.BBox[2] - r.BBox[0]) / float64(r.Width), PixelN: (r.BBox[3] - r.BBox[1]) / float64(r.Height)}}
	for i := range g.Values {
		g.Values[i] = v
	}
	return g, nil
}

func (f *surfaceFake) set(h float32, err error) {
	f.mu.Lock()
	f.surfaceM, f.err = h, err
	f.mu.Unlock()
}

const est789Lat, est789Lon = 56.1629, 10.2039

type est789Env struct {
	*terrainEnv
	profile *fakeElevation
	surface *surfaceFake
	clock   *time.Time
	mu      *sync.Mutex
}

// newEst789Env is a profile server whose estimate measures on surface,
// with a settable clock.
func newEst789Env(t *testing.T, surface *surfaceFake, radio *RadioConfig) *est789Env {
	t.Helper()
	profile := newFakeElevation(t, flatElev(30))
	// No per-IP limit: these tests count rasters, not requests.
	env := newTerrainEnvWith(t, profile, dhm.ProfileConfig{RateLimitPerMin: intp(0)}, radio, func(d *terrainProfilerDeps) { d.surface = surface })
	clock := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	mu := &sync.Mutex{}
	env.p.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	return &est789Env{terrainEnv: env, profile: profile, surface: surface, clock: &clock, mu: mu}
}

func (e *est789Env) advance(d time.Duration) {
	e.mu.Lock()
	*e.clock = e.clock.Add(d)
	e.mu.Unlock()
}

// addNode inserts a positioned node.
func (e *est789Env) addNode(t *testing.T, pk, name, role string, lat, lon float64) {
	t.Helper()
	if _, err := e.srv.db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count)
		VALUES (?, ?, ?, ?, ?, datetime('now'), datetime('now'), 1)`, pk, name, role, lat, lon); err != nil {
		t.Fatal(err)
	}
	e.srv.store.InvalidateNodeCache()
}

func (e *est789Env) move(t *testing.T, pk string, lat, lon float64) {
	t.Helper()
	if _, err := e.srv.db.conn.Exec(`UPDATE nodes SET lat = ?, lon = ? WHERE public_key = ?`, lat, lon, pk); err != nil {
		t.Fatal(err)
	}
	e.srv.store.InvalidateNodeCache()
}

// endA requests a profile from node pk to a point 3 km east and returns
// the node's endpoint.
func (e *est789Env) endA(t *testing.T, pk string, extra ...string) TerrainEndpoint {
	t.Helper()
	return decodeProfile(t, e.get(pathQuery(pk, offsetPt(est789Lat, est789Lon, 3000, 0), extra...))).A
}

func pk789(c string) string { return strings.Repeat(c, 64) }

// ── Precedence ────────────────────────────────────────────────────────────

func TestAntennaEstimate789_PrecedenceTable(t *testing.T) {
	pk := pk789("a")
	plain := &terrainProfiler{radio: resolveTerrainRadio(nil, func(string, ...interface{}) {})}
	op := &terrainProfiler{radio: resolveTerrainRadio(&RadioConfig{AntennaHeightsM: map[string]float64{
		pk: 18, "repeater": 15,
	}}, func(string, ...interface{}) {})}
	node := func(role string, s *float64) terrainEnd {
		return terrainEnd{kind: "node", role: role, pubkey: pk, surfaceM: s}
	}
	other := func(role string, s *float64) terrainEnd {
		return terrainEnd{kind: "node", role: role, pubkey: pk789("c"), surfaceM: s}
	}
	for _, tc := range []struct {
		name    string
		p       *terrainProfiler
		e       terrainEnd
		ov      *float64
		h       float64
		src     string
		assumed bool
	}{
		{"request beats the estimate", plain, node("repeater", floatp(40)), floatp(3), 3, "request", false},
		{"operator-node beats the estimate", op, node("repeater", floatp(40)), nil, 18, "operator-node", false},
		{"repeater on a roof", plain, node("repeater", floatp(20)), nil, 22, "dsm-estimate", true},
		{"room on a roof", plain, node("room", floatp(12)), nil, 14, "dsm-estimate", true},
		{"low roof never lowers the repeater default", plain, node("repeater", floatp(5)), nil, 10, "role-default", true},
		{"equal to the default keeps the default", plain, node("repeater", floatp(8)), nil, 10, "role-default", true},
		{"mast (surface 0) keeps the default", plain, node("repeater", floatp(0)), nil, 10, "role-default", true},
		{"low room keeps the room default", plain, node("room", floatp(2)), nil, 5, "role-default", true},
		{"companion keeps its default", plain, node("companion", floatp(30)), nil, 2, "role-default", true},
		{"sensor keeps its default", plain, node("sensor", floatp(30)), nil, 2, "role-default", true},
		{"unknown role keeps the handheld default", plain, node("weird", floatp(30)), nil, 2, "role-default", true},
		{"no measurement keeps the default", plain, node("repeater", nil), nil, 10, "role-default", true},
		{"estimate above operator-role", op, other("repeater", floatp(20)), nil, 22, "dsm-estimate", true},
		{"estimate below operator-role keeps it", op, other("repeater", floatp(10)), nil, 15, "operator-role", true},
		{"estimate is capped", plain, node("repeater", floatp(150)), nil, terrainMaxAntennaM, "dsm-estimate", true},
		{"a point never gets one", plain, terrainEnd{kind: "point", surfaceM: floatp(30)}, nil, 2, "point-default", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, src, assumed := tc.p.antenna(tc.e, tc.ov)
			if h != tc.h || src != tc.src || assumed != tc.assumed {
				t.Fatalf("antenna = %v, %q, %v; want %v, %q, %v", h, src, assumed, tc.h, tc.src, tc.assumed)
			}
		})
	}
}

// ── End to end ────────────────────────────────────────────────────────────

func TestAntennaEstimate789_RepeaterOnRoof(t *testing.T) {
	env := newEst789Env(t, &surfaceFake{surfaceM: 20}, nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof repeater", "repeater", est789Lat, est789Lon)
	a := env.endA(t, pk)
	if a.AntennaM != 22 || a.AntennaSource != "dsm-estimate" || !a.Assumed {
		t.Fatalf("endpoint %+v, want 22 m dsm-estimate assumed", a)
	}
	if n := env.surface.calls.Load(); n != 2 {
		t.Fatalf("%d surface rasters, want 2 (DSM + DTM)", n)
	}
	// The window is small, centred on the snapped position.
	e0, n0 := dhm.ToUTM32(est789Lat, est789Lon)
	for _, r := range env.surface.reqs {
		if w := r.BBox[2] - r.BBox[0]; w > 25 || math.Abs((r.BBox[0]+r.BBox[2])/2-e0) > 1 || math.Abs((r.BBox[1]+r.BBox[3])/2-n0) > 1 {
			t.Fatalf("window %+v not a small bbox around the node", r)
		}
	}
}

// A hit is served from the LRU: no further raster, for any profile from
// the node, until the TTL runs out.
func TestAntennaEstimate789_CachedPerNode(t *testing.T) {
	env := newEst789Env(t, &surfaceFake{surfaceM: 20}, nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof repeater", "repeater", est789Lat, est789Lon)
	env.endA(t, pk)
	for i := 0; i < 5; i++ {
		b := offsetPt(est789Lat, est789Lon, 0, float64(2000+500*i))
		if a := decodeProfile(t, env.get(pathQuery(pk, b))).A; a.AntennaSource != "dsm-estimate" {
			t.Fatalf("profile %d: %+v", i, a)
		}
		if bEnd := decodeProfile(t, env.get(pathQuery(b, pk))).B; bEnd.AntennaM != 22 {
			t.Fatalf("reversed profile %d: %+v", i, bEnd)
		}
	}
	if n := env.surface.calls.Load(); n != 2 {
		t.Fatalf("%d surface rasters after 11 profiles, want 2", n)
	}
	// The spec's 30 days, not the constant: a shorter TTL is a regression.
	env.advance(30*24*time.Hour - time.Minute)
	env.endA(t, pk)
	if n := env.surface.calls.Load(); n != 2 {
		t.Fatalf("%d rasters inside the TTL, want 2", n)
	}
	env.advance(2 * time.Minute)
	env.endA(t, pk)
	if n := env.surface.calls.Load(); n != 4 {
		t.Fatalf("%d rasters after the TTL, want 4", n)
	}
}

// A move to a new rounded position misses; jitter below the rounding hits.
func TestAntennaEstimate789_MoveMisses(t *testing.T) {
	env := newEst789Env(t, &surfaceFake{surfaceM: 20}, nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof repeater", "repeater", est789Lat, est789Lon)
	env.endA(t, pk)
	env.move(t, pk, est789Lat+3e-6, est789Lon-3e-6) // rounds to the same 1e-5
	env.endA(t, pk)
	if n := env.surface.calls.Load(); n != 2 {
		t.Fatalf("%d rasters after jitter, want 2", n)
	}
	env.surface.set(4, nil) // the new spot is a low shed
	env.move(t, pk, est789Lat+2e-4, est789Lon)
	a := env.endA(t, pk)
	if n := env.surface.calls.Load(); n != 4 {
		t.Fatalf("%d rasters after a move, want 4", n)
	}
	if a.AntennaM != 10 || a.AntennaSource != "role-default" {
		t.Fatalf("after the move %+v, want the 10 m role default", a)
	}
}

// Only repeaters and rooms are measured; a request or operator height
// for the node skips the measurement altogether.
func TestAntennaEstimate789_NoMeasurementWhenNotUsed(t *testing.T) {
	pk := pk789("a")
	for _, tc := range []struct {
		name, role string
		radio      *RadioConfig
		extra      []string
		h          float64
		src        string
	}{
		{"companion", "companion", nil, nil, 2, "role-default"},
		{"sensor", "sensor", nil, nil, 2, "role-default"},
		{"request height", "repeater", nil, []string{"ha=7"}, 7, "request"},
		{"operator-node", "repeater", &RadioConfig{AntennaHeightsM: map[string]float64{pk: 25}}, nil, 25, "operator-node"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newEst789Env(t, &surfaceFake{surfaceM: 40}, tc.radio)
			env.addNode(t, pk, "Node", tc.role, est789Lat, est789Lon)
			a := env.endA(t, pk, tc.extra...)
			if a.AntennaM != tc.h || a.AntennaSource != tc.src {
				t.Fatalf("endpoint %+v, want %v %s", a, tc.h, tc.src)
			}
			if n := env.surface.calls.Load(); n != 0 {
				t.Fatalf("%d surface rasters, want 0", n)
			}
		})
	}
	// A room is measured.
	env := newEst789Env(t, &surfaceFake{surfaceM: 12}, nil)
	env.addNode(t, pk, "Room", "room", est789Lat, est789Lon)
	if a := env.endA(t, pk); a.AntennaM != 14 || a.AntennaSource != "dsm-estimate" {
		t.Fatalf("room %+v, want 14 m dsm-estimate", a)
	}
}

// #743: a hidden node is 404 before anything is measured.
func TestAntennaEstimate789_HiddenNodeNoEstimate(t *testing.T) {
	env := newEst789Env(t, &surfaceFake{surfaceM: 20}, nil)
	env.srv.cfg.SetHiddenNamePrefixes([]string{"private-"})
	pk := pk789("a")
	env.addNode(t, pk, "private-roof", "repeater", est789Lat, est789Lon)
	wantError(t, env.get(pathQuery(pk, offsetPt(est789Lat, est789Lon, 3000, 0))), 404, "node not found")
	if n := env.surface.calls.Load(); n != 0 {
		t.Fatalf("%d surface rasters for a hidden node, want 0", n)
	}
	if n := env.p.estimate.cache.Len(); n != 0 {
		t.Fatalf("%d cached estimates for a hidden node, want 0", n)
	}
}

// A failed measurement keeps the role default, and is not retried for
// terrainEstimateFailTTL: a failing upstream costs 2 rasters per node per
// hour at most.
func TestAntennaEstimate789_FailureBackoff(t *testing.T) {
	env := newEst789Env(t, &surfaceFake{err: errors.New("boom")}, nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof repeater", "repeater", est789Lat, est789Lon)
	if a := env.endA(t, pk); a.AntennaSource != "role-default" || a.AntennaM != 10 {
		t.Fatalf("after a failure %+v, want the role default", a)
	}
	first := env.surface.calls.Load()
	if first < 1 || first > 2 {
		t.Fatalf("%d rasters for one failed measurement, want 1-2", first)
	}
	env.surface.set(20, nil)
	for i := 0; i < 3; i++ {
		env.endA(t, pk)
	}
	if n := env.surface.calls.Load(); n != first {
		t.Fatalf("%d rasters inside the back-off, want %d", n, first)
	}
	env.advance(terrainEstimateFailTTL + time.Second)
	if a := env.endA(t, pk); a.AntennaSource != "dsm-estimate" {
		t.Fatalf("after the back-off %+v, want the estimate", a)
	}
	if n := env.surface.calls.Load(); n != first+2 {
		t.Fatalf("%d rasters after the back-off, want %d", n, first+2)
	}
}

// Outside DHM there is nothing to measure, and the answer is cached too.
func TestAntennaEstimate789_NotCoveredCached(t *testing.T) {
	env := newEst789Env(t, &surfaceFake{surfaceM: 20}, nil)
	pk := pk789("a")
	// Malmö is foreign land: SurfaceAt refuses it without a call.
	env.addNode(t, pk, "Malmö repeater", "repeater", 55.605, 13.0038)
	b := offsetPt(55.605, 13.0038, -3000, 0)
	for i := 0; i < 2; i++ {
		a := decodeProfile(t, env.get(pathQuery(pk, b))).A
		if a.AntennaSource != "role-default" {
			t.Fatalf("foreign node %+v, want the role default", a)
		}
	}
	if n := env.surface.calls.Load(); n != 0 {
		t.Fatalf("%d rasters for an uncovered node, want 0", n)
	}
	if n := env.p.estimate.cache.Len(); n != 1 {
		t.Fatalf("%d cached estimates, want 1 (the uncovered answer)", n)
	}
}

// Concurrent profiles from one node share one measurement.
func TestAntennaEstimate789_Coalesced(t *testing.T) {
	env := newEst789Env(t, &surfaceFake{surfaceM: 20, delay: 50 * time.Millisecond}, nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof repeater", "repeater", est789Lat, est789Lon)
	var wg sync.WaitGroup
	srcs := make([]string, 8)
	for i := range srcs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := env.get(pathQuery(pk, offsetPt(est789Lat, est789Lon, 3000, 0)))
			if w.Code == 200 {
				srcs[i] = decodeProfile(t, w).A.AntennaSource
			}
		}(i)
	}
	wg.Wait()
	if n := env.surface.calls.Load(); n != 2 {
		t.Fatalf("%d rasters for 8 concurrent profiles, want 2", n)
	}
	for i, s := range srcs {
		if s != "dsm-estimate" {
			t.Fatalf("profile %d: source %q", i, s)
		}
	}
}

// With every admission slot taken the measurement is skipped, not
// remembered, so the next profile measures.
func TestAntennaEstimate789_AdmissionFull(t *testing.T) {
	env := newEst789Env(t, &surfaceFake{surfaceM: 20}, nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof repeater", "repeater", est789Lat, est789Lon)
	end, ok := env.srv.resolveTerrainEnd(context.Background(), terrainRef{pubkey: pk})
	if !ok {
		t.Fatal("node not resolved")
	}
	for i := 0; i < cap(env.p.admit); i++ {
		env.p.admit <- struct{}{}
	}
	if s := env.p.surfaceFor(context.Background(), end); s != nil {
		t.Fatalf("surface %v with admission full, want none", *s)
	}
	for i := 0; i < cap(env.p.admit); i++ {
		<-env.p.admit
	}
	if env.surface.calls.Load() != 0 || env.p.estimate.cache.Len() != 0 || env.p.estimate.failed.Len() != 0 {
		t.Fatal("a refused measurement did upstream work or was remembered")
	}
	if s := env.p.surfaceFor(context.Background(), end); s == nil || *s != 20 {
		t.Fatalf("surface after the refusal %v, want 20", s)
	}
}

// The LRU is bounded at terrainEstimateEntries, least recently used out
// first, and the byte bound never fires before the entry bound.
func TestAntennaEstimate789_LRUBound(t *testing.T) {
	est := newTerrainEstimator(&surfaceFake{})
	now := time.Now()
	val := []byte{0, 0, 0, 0, 0, 0, 0, 0}
	key := func(i int) terrainEstimateKey {
		return terrainEstimateKey{pubkey: fmt.Sprintf("%064x", i), lat: 5616290, lon: 1020390}
	}
	for i := 0; i < terrainEstimateEntries+500; i++ {
		est.cache.Put(key(i), val, now)
		if i == 10 {
			est.cache.Get(key(0), now) // keep the first one in use
		}
	}
	if terrainEstimateEntries != 4096 {
		t.Fatalf("bound %d, want 4096 (#789 M1)", terrainEstimateEntries)
	}
	if n := est.cache.Len(); n != terrainEstimateEntries {
		t.Fatalf("%d entries, want the bound %d", n, terrainEstimateEntries)
	}
	if _, ok := est.cache.Get(key(1), now); ok {
		t.Fatal("least recently used entry not evicted")
	}
	if _, ok := est.cache.Get(key(terrainEstimateEntries+499), now); !ok {
		t.Fatal("newest entry evicted")
	}
	if b := est.cache.Size(); b > terrainEstimateEntries*8 {
		t.Fatalf("%d bytes over the bound", b)
	}
	// Same pubkey, a different rounded position: a different entry.
	a := terrainEstimateKey{pubkey: pk789("a"), lat: terrainSnapCoord(est789Lat), lon: terrainSnapCoord(est789Lon)}
	b := terrainEstimateKey{pubkey: pk789("a"), lat: terrainSnapCoord(est789Lat + 1e-5), lon: a.lon}
	if a == b || a.String() == b.String() {
		t.Fatal("a move by 1e-5 does not change the key")
	}
}

// The server built from config measures on the same WCS client.
func TestAntennaEstimate789_WiredFromConfig(t *testing.T) {
	on := &Config{DHM: dhm.Config{Enabled: true, Token: "t", Profile: dhm.ProfileConfig{Enabled: true}}}
	p := newTerrainProfilerFromConfig(on, newDHMProxy(on.DHM, defaultDHMProxyOptions()), nil)
	if p == nil || p.estimate.src == nil || p.estimate.src != p.src {
		t.Fatal("the estimate is not measured on the profile's WCS source")
	}
	// Without the dependency the estimate is off.
	off := newTerrainProfiler(dhm.ProfileConfig{Enabled: true}, nil, terrainProfilerDeps{src: &surfaceFake{}})
	if off.estimate.src != nil || off.wantsEstimate(terrainEnd{kind: "node", role: "repeater", pubkey: pk789("a")}, nil) {
		t.Fatal("estimate on without a surface source")
	}
}

// #789 perf proof: a cache hit is a map lookup, no allocation per call
// beyond the returned value.
func BenchmarkAntennaEstimate789_Hit(b *testing.B) {
	p := &terrainProfiler{estimate: newTerrainEstimator(&surfaceFake{surfaceM: 20}), admit: make(chan struct{}, 4), now: time.Now, deadline: time.Second}
	e := terrainEnd{kind: "node", role: "repeater", pubkey: pk789("a"), pt: dhm.LatLon{Lat: est789Lat, Lon: est789Lon}}
	if s := p.surfaceFor(context.Background(), e); s == nil {
		b.Fatal("no surface")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if p.surfaceFor(context.Background(), e) == nil {
			b.Fatal("miss")
		}
	}
}

// The miss: two 52² rasters through the fake source, sampled.
func BenchmarkAntennaEstimate789_Miss(b *testing.B) {
	src := &surfaceFake{surfaceM: 20}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := dhm.SurfaceAt(context.Background(), src, dhm.LatLon{Lat: est789Lat, Lon: est789Lon}, terrainEstimateRadiusM); err != nil {
			b.Fatal(err)
		}
	}
}
