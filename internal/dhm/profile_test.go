package dhm

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// #681 M3 — the path planner (sampling, chunking, the bbox gate) and the
// fan-out over an ElevationSource. These use a fake source; the WCS wire
// path is covered in wcs_test.go and cmd/server's endpoint tests.

// fakeSource answers every raster with elev evaluated at the pixel
// centres, after checking the request is inside the allow-list.
type fakeSource struct {
	t     *testing.T
	calls atomic.Int64
	elev  func(e, n float64) float32
	// hook runs first; a non-nil error is returned instead of a grid.
	hook func(ctx context.Context, r RasterRequest) error
	mu   sync.Mutex
	reqs []RasterRequest
}

func (f *fakeSource) FetchRaster(ctx context.Context, r RasterRequest) (Grid, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.reqs = append(f.reqs, r)
	f.mu.Unlock()
	if err := r.Validate(); err != nil {
		f.t.Errorf("planner sent an invalid raster %+v: %v", r, err)
		return Grid{}, err
	}
	if f.hook != nil {
		if err := f.hook(ctx, r); err != nil {
			return Grid{}, err
		}
	}
	g := Grid{Width: r.Width, Height: r.Height, Values: make([]float32, r.Width*r.Height),
		NoData: -9999, HasNoData: true,
		Georef: Georef{OriginE: r.BBox[0], OriginN: r.BBox[3],
			PixelE: (r.BBox[2] - r.BBox[0]) / float64(r.Width), PixelN: (r.BBox[3] - r.BBox[1]) / float64(r.Height)}}
	for row := 0; row < r.Height; row++ {
		for col := 0; col < r.Width; col++ {
			e := g.Georef.OriginE + (float64(col)+0.5)*g.Georef.PixelE
			n := g.Georef.OriginN - (float64(row)+0.5)*g.Georef.PixelN
			g.Values[row*r.Width+col] = f.elev(e, n)
		}
	}
	return g, nil
}

// planeElev is exact under bilinear interpolation.
func planeElev(e, n float64) float32 { return float32(0.001*(e-570000) + 0.002*(n-6220000) + 20) }

var aarhus = LatLon{56.1629, 10.2039}

// toward returns the point d metres from a at bearing 45° in UTM, the
// plan's most expensive bearing (§1.3).
func toward(a LatLon, d, bearingDeg float64) LatLon {
	e, n := ToUTM32(a.Lat, a.Lon)
	s, c := math.Sincos(bearingDeg * math.Pi / 180)
	lat, lon := FromUTM32(e+d*s, n+d*c)
	return LatLon{lat, lon}
}

func defaultPlan() PlanOptions { return PlanOptions{MaxSamples: 3200, ChunkM: 2500} }

func TestPlanStepAndSampleCount(t *testing.T) {
	for _, c := range []struct {
		km       float64
		n        int
		stepWant float64
	}{
		{1, 101, 10},
		{5, 501, 10},
		{20, 1025, 20000.0 / 1024},
		{40, 1601, 25},
		{80, 3200, 80000.0 / 3199},
	} {
		p := PlanProfile(aarhus, toward(aarhus, c.km*1000, 90), defaultPlan())
		// The count is a rounding of distance / step: allow one either way.
		if len(p.E) < c.n-1 || len(p.E) > c.n+1 || len(p.E) > 3200 {
			t.Errorf("%v km: %d samples, want %d", c.km, len(p.E), c.n)
		}
		near(t, "step", p.StepM, c.stepWant, 0.02*c.stepWant)
		// UTM 32 metres at 10.2° E are 0.03 % short of ground metres.
		near(t, "distance", p.DistanceM, c.km*1000, 0.001*c.km*1000)
	}
	// Against a known distance: 1° of latitude at 56° N is 111.3 km on
	// GRS80 (a sphere gives 111.2 km).
	near(t, "1° of latitude", GroundDistance(LatLon{55.5, 10}, LatLon{56.5, 10}), 111334, 30)
	// Never fewer than three samples: an interior point is needed.
	if p := PlanProfile(aarhus, toward(aarhus, 3, 90), defaultPlan()); len(p.E) != 3 {
		t.Fatalf("3 m path: %d samples", len(p.E))
	}
}

func TestPlanCallBudgetAtWorstBearing(t *testing.T) {
	for _, c := range []struct {
		km    float64
		calls int
	}{{1, 1}, {5, 2}, {10, 4}, {20, 8}, {40, 16}, {80, 16}} {
		p := PlanProfile(aarhus, toward(aarhus, c.km*1000, 45), defaultPlan())
		if got := p.Calls(); got != c.calls || got > MaxCallsPerModel {
			t.Errorf("%v km: %d calls per model, want %d", c.km, got, c.calls)
		}
		for _, ch := range p.chunks {
			r := ch.raster
			r.Model = ModelTerrain
			if err := r.Validate(); err != nil {
				t.Errorf("%v km: chunk raster %+v invalid", c.km, r)
			}
		}
	}
	// chunkKm = 0: one call per model, even on an axis-aligned 80 km path
	// whose bbox with margins would exceed the 80 km raster span (it then
	// takes two).
	o := defaultPlan()
	o.ChunkM = 0
	if p := PlanProfile(aarhus, toward(aarhus, 40000, 45), o); p.Calls() != 1 {
		t.Fatalf("chunkKm=0, 40 km: %d calls", p.Calls())
	}
	p := PlanProfile(LatLon{55.6, 8.3}, toward(LatLon{55.6, 8.3}, 80000, 90), o)
	if p.Calls() > 2 {
		t.Fatalf("chunkKm=0, 80 km E-W: %d calls", p.Calls())
	}
	for _, ch := range p.chunks {
		r := ch.raster
		r.Model = ModelTerrain
		if err := r.Validate(); err != nil {
			t.Fatalf("chunkKm=0, 80 km E-W: raster %+v invalid", r)
		}
	}
}

func TestPlanOutsideDenmarkMakesNoCalls(t *testing.T) {
	hamburg := LatLon{53.55, 9.99}
	p := PlanProfile(hamburg, toward(hamburg, 10000, 45), defaultPlan())
	if p.Calls() != 0 || p.Covered() != 0 {
		t.Fatalf("Hamburg: %d calls, %d covered samples", p.Calls(), p.Covered())
	}
	src := &fakeSource{t: t, elev: planeElev}
	vals, err := p.Fetch(context.Background(), src, ModelTerrain, time.Second)
	if err != nil || src.calls.Load() != 0 {
		t.Fatalf("Fetch: %v, %d calls", err, src.calls.Load())
	}
	for i, v := range vals {
		if !math.IsNaN(float64(v)) {
			t.Fatalf("sample %d outside DK = %v, want NaN", i, v)
		}
	}
}

func TestPlanLeavingTheRasterExtent(t *testing.T) {
	// Rødby (Lolland) south across the Fehmarn Belt, out of the extent.
	a, b := LatLon{54.65, 11.35}, LatLon{54.42, 11.2}
	p := PlanProfile(a, b, defaultPlan())
	cov := p.Covered()
	if cov == 0 || cov == len(p.E) {
		t.Fatalf("covered %d of %d, want a partial plan", cov, len(p.E))
	}
	src := &fakeSource{t: t, elev: planeElev}
	vals, err := p.Fetch(context.Background(), src, ModelTerrain, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if math.IsNaN(float64(vals[0])) || !math.IsNaN(float64(vals[len(vals)-1])) {
		t.Fatalf("first %v (inside), last %v (outside)", vals[0], vals[len(vals)-1])
	}
}

func TestFetchSamplesTheSurface(t *testing.T) {
	b := toward(aarhus, 7000, 30)
	p := PlanProfile(aarhus, b, defaultPlan())
	src := &fakeSource{t: t, elev: planeElev}
	vals, err := p.Fetch(context.Background(), src, ModelSurface, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if int(src.calls.Load()) != p.Calls() {
		t.Fatalf("%d calls, plan says %d", src.calls.Load(), p.Calls())
	}
	for _, r := range src.reqs {
		if r.Model != ModelSurface {
			t.Fatalf("model %q requested", r.Model)
		}
	}
	for i := range vals {
		near(t, "sample", float64(vals[i]), float64(planeElev(p.E[i], p.N[i])), 0.01)
	}
}

func TestFetchNoDataIsNaN(t *testing.T) {
	p := PlanProfile(aarhus, toward(aarhus, 5000, 90), defaultPlan())
	eMid := (p.E[0] + p.E[len(p.E)-1]) / 2
	src := &fakeSource{t: t, elev: func(e, n float64) float32 {
		if e > eMid {
			return -9999 // sea
		}
		return 5
	}}
	vals, err := p.Fetch(context.Background(), src, ModelTerrain, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if vals[0] != 5 || !math.IsNaN(float64(vals[len(vals)-1])) {
		t.Fatalf("first %v, last %v", vals[0], vals[len(vals)-1])
	}
}

func TestFetchFirstErrorCancelsTheRest(t *testing.T) {
	p := PlanProfile(aarhus, toward(aarhus, 20000, 45), defaultPlan())
	var stopped atomic.Int64
	src := &fakeSource{t: t, elev: planeElev, hook: func(ctx context.Context, r RasterRequest) error {
		if r.BBox == p.chunks[0].raster.BBox {
			return FetchError{"status 500"}
		}
		select {
		case <-ctx.Done():
			stopped.Add(1)
			return FetchError{"canceled"}
		case <-time.After(5 * time.Second):
			return nil
		}
	}}
	start := time.Now()
	_, err := p.Fetch(context.Background(), src, ModelTerrain, 10*time.Second)
	var fe FetchError
	if !errors.As(err, &fe) || fe.Reason != "status 500" {
		t.Fatalf("err %v, want the first failure", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("other chunks not cancelled: %v", el)
	}
	if int(stopped.Load()) != p.Calls()-1 {
		t.Fatalf("%d of %d other chunk calls saw cancellation", stopped.Load(), p.Calls()-1)
	}
}

func TestFetchPerCallTimeout(t *testing.T) {
	p := PlanProfile(aarhus, toward(aarhus, 1000, 45), defaultPlan())
	src := &fakeSource{t: t, elev: planeElev, hook: func(ctx context.Context, r RasterRequest) error {
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
		return FetchError{"timeout"}
	}}
	start := time.Now()
	if _, err := p.Fetch(context.Background(), src, ModelTerrain, 50*time.Millisecond); err == nil {
		t.Fatal("hung source did not fail")
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("per-call timeout not applied: %v", el)
	}
}
