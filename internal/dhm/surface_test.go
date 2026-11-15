package dhm

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
)

// #789 M1 — SurfaceAt on synthetic DSM/DTM fields.

// fieldSource serves rasters sampled from one function per model, and
// records every request.
type fieldSource struct {
	dsm, dtm func(e, n float64) float32
	err      error
	mu       sync.Mutex
	reqs     []RasterRequest
}

func (f *fieldSource) FetchRaster(_ context.Context, r RasterRequest) (Grid, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r)
	f.mu.Unlock()
	if f.err != nil {
		return Grid{}, f.err
	}
	fn := f.dtm
	if r.Model == ModelSurface {
		fn = f.dsm
	}
	g := Grid{Width: r.Width, Height: r.Height, Values: make([]float32, r.Width*r.Height),
		NoData: -9999, HasNoData: true,
		Georef: Georef{OriginE: r.BBox[0], OriginN: r.BBox[3],
			PixelE: (r.BBox[2] - r.BBox[0]) / float64(r.Width), PixelN: (r.BBox[3] - r.BBox[1]) / float64(r.Height)}}
	for row := 0; row < r.Height; row++ {
		for col := 0; col < r.Width; col++ {
			g.Values[row*r.Width+col] = fn(g.Georef.OriginE+(float64(col)+0.5)*g.Georef.PixelE,
				g.Georef.OriginN-(float64(row)+0.5)*g.Georef.PixelN)
		}
	}
	return g, nil
}

func flat(h float32) func(e, n float64) float32 { return func(float64, float64) float32 { return h } }

// aboveGround is a DSM of ground plus dh(de, dn), offsets from the node.
func aboveGround(ground float32, at LatLon, dh func(de, dn float64) float32) func(e, n float64) float32 {
	e0, n0 := ToUTM32(at.Lat, at.Lon)
	return func(e, n float64) float32 { return ground + dh(e-e0, n-n0) }
}

func surfaceAt(t *testing.T, src *fieldSource, at LatLon, r float64) Surface {
	t.Helper()
	s, err := SurfaceAt(context.Background(), src, at, r)
	if err != nil {
		t.Fatalf("SurfaceAt: %v", err)
	}
	return s
}

func approx(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if math.IsNaN(got) || math.Abs(got-want) > tol {
		t.Fatalf("%s = %v, want %v ± %v", what, got, want, tol)
	}
}

// A lattice mast is invisible in the DSM: the surface equals the terrain.
func TestSurfaceAt789_MastIsZero(t *testing.T) {
	src := &fieldSource{dsm: flat(31), dtm: flat(31)}
	s := surfaceAt(t, src, aarhus, 10)
	approx(t, "max", s.MaxM, 0, 1e-9)
	approx(t, "p90", s.P90M, 0, 1e-9)
	approx(t, "spread", s.SpreadM, 0, 1e-9)
	approx(t, "coverage", s.Coverage, 1, 1e-9)
}

// A flat roof: every cell 12 m up, no spread.
func TestSurfaceAt789_FlatRoof(t *testing.T) {
	src := &fieldSource{dsm: flat(42), dtm: flat(30)}
	s := surfaceAt(t, src, aarhus, 10)
	approx(t, "max", s.MaxM, 12, 1e-4)
	approx(t, "p90", s.P90M, 12, 1e-4)
	approx(t, "spread", s.SpreadM, 0, 1e-4)
	approx(t, "coverage", s.Coverage, 1, 1e-9)
	// π·(10/0.4)² ≈ 1963 cells of 0.4 m in a 10 m circle.
	if s.Cells < 1900 || s.Cells > 2030 {
		t.Fatalf("cells %d, want about 1963", s.Cells)
	}
}

// Trees: an uneven canopy from 0 to 15 m has a high spread, unlike a roof.
func TestSurfaceAt789_TreesHighSpread(t *testing.T) {
	canopy := func(de, dn float64) float32 {
		return float32(7.5 + 7.5*math.Sin(de*1.7)*math.Cos(dn*2.3))
	}
	src := &fieldSource{dsm: aboveGround(20, aarhus, canopy), dtm: flat(20)}
	s := surfaceAt(t, src, aarhus, 10)
	if s.SpreadM < 8 {
		t.Fatalf("spread %v under trees, want ≥ 8", s.SpreadM)
	}
	if s.MaxM < 14 || s.MaxM > 15.01 {
		t.Fatalf("max %v, want about 15", s.MaxM)
	}
	if !(s.P90M > s.SpreadM/2 && s.P90M <= s.MaxM) {
		t.Fatalf("p90 %v out of order (max %v, spread %v)", s.P90M, s.MaxM, s.SpreadM)
	}
}

// The advertised position misses the building by a few metres: the
// window still finds the roof. A tower 15 m east, outside a 10 m radius,
// is not counted.
func TestSurfaceAt789_WindowRadius(t *testing.T) {
	roofEast := func(de, dn float64) float32 {
		if de > 4 && de < 9 {
			return 18
		}
		if de > 14 {
			return 60
		}
		return 0
	}
	src := &fieldSource{dsm: aboveGround(10, aarhus, roofEast), dtm: flat(10)}
	s := surfaceAt(t, src, aarhus, 10)
	approx(t, "max", s.MaxM, 18, 1e-3)
	// The roof is about 14 % of the circle: above the 90th percentile it
	// is all roof, below the 10th all ground.
	approx(t, "p90", s.P90M, 18, 1e-3)
	approx(t, "spread", s.SpreadM, 18, 1e-3)
}

// Cells where either model has no data are ignored, and counted in the
// coverage; DSM just below DTM is noise, not a negative height.
func TestSurfaceAt789_NoDataAndNegative(t *testing.T) {
	e0, _ := ToUTM32(aarhus.Lat, aarhus.Lon)
	dtm := func(e, n float64) float32 {
		if e < e0 {
			return -9999
		}
		return 5
	}
	src := &fieldSource{dsm: flat(4.8), dtm: dtm}
	s := surfaceAt(t, src, aarhus, 10)
	approx(t, "coverage", s.Coverage, 0.5, 0.03)
	approx(t, "max", s.MaxM, 0, 1e-9)

	none := &fieldSource{dsm: flat(-9999), dtm: flat(5)}
	s = surfaceAt(t, none, aarhus, 10)
	if s.Coverage != 0 || s.MaxM != 0 || s.Cells == 0 {
		t.Fatalf("no data: %+v, want zero coverage over a non-empty window", s)
	}
}

// One DSM and one DTM raster, small, at native resolution, centred on the
// node and valid for the allow-list.
func TestSurfaceAt789_TwoSmallRasters(t *testing.T) {
	src := &fieldSource{dsm: flat(1), dtm: flat(0)}
	surfaceAt(t, src, aarhus, 10)
	if len(src.reqs) != 2 {
		t.Fatalf("%d rasters, want 2", len(src.reqs))
	}
	seen := map[Model]bool{}
	e0, n0 := ToUTM32(aarhus.Lat, aarhus.Lon)
	for _, r := range src.reqs {
		seen[r.Model] = true
		if err := r.Validate(); err != nil {
			t.Fatalf("invalid raster %+v", r)
		}
		w, h := r.BBox[2]-r.BBox[0], r.BBox[3]-r.BBox[1]
		if w > 21.7 || h > 21.7 || w < 20 {
			t.Fatalf("bbox %v × %v m, want about 20.8 m", w, h)
		}
		approx(t, "pixel", w/float64(r.Width), SurfacePixelM, 1e-9)
		approx(t, "centre E", (r.BBox[0]+r.BBox[2])/2, e0, 1e-6)
		approx(t, "centre N", (r.BBox[1]+r.BBox[3])/2, n0, 1e-6)
	}
	if !seen[ModelSurface] || !seen[ModelTerrain] {
		t.Fatalf("models %v, want dsm and dtm", seen)
	}
}

func TestSurfaceAt789_Refusals(t *testing.T) {
	src := &fieldSource{dsm: flat(1), dtm: flat(0)}
	for _, r := range []float64{0, -1, math.NaN(), math.Inf(1), MaxSurfaceRadiusM + 0.1} {
		if _, err := SurfaceAt(context.Background(), src, aarhus, r); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("radius %v: %v, want ErrBadRequest", r, err)
		}
	}
	// Malmö (foreign land), the North Sea outside the extent, and NaN.
	for _, at := range []LatLon{{55.605, 13.0038}, {56, 5}, {math.NaN(), 10}} {
		if _, err := SurfaceAt(context.Background(), src, at, 10); !errors.Is(err, ErrNotCovered) {
			t.Fatalf("%v: %v, want ErrNotCovered", at, err)
		}
	}
	if len(src.reqs) != 0 {
		t.Fatalf("%d upstream calls for refused windows, want 0", len(src.reqs))
	}
	if _, err := SurfaceAt(context.Background(), &fieldSource{err: errFake789}, aarhus, 10); !errors.Is(err, errFake789) {
		t.Fatalf("upstream failure: %v", err)
	}
}

// #789 M1 perf proof: the window statistics over a 512² grid (the largest
// raster the client accepts), and over the 52² window M1 actually fetches.
func benchSurface(b *testing.B, px int) {
	r := RasterRequest{Width: px, Height: px}
	side := float64(px) * SurfacePixelM
	e0, n0 := ToUTM32(aarhus.Lat, aarhus.Lon)
	r.BBox = [4]float64{e0 - side/2, n0 - side/2, e0 + side/2, n0 + side/2}
	canopy := aboveGround(20, aarhus, func(de, dn float64) float32 { return float32(7.5 + 7.5*math.Sin(de*1.7)*math.Cos(dn*2.3)) })
	src := &fieldSource{dsm: canopy, dtm: flat(20)}
	r.Model = ModelSurface
	dsm, _ := src.FetchRaster(context.Background(), r)
	r.Model = ModelTerrain
	dtm, _ := src.FetchRaster(context.Background(), r)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := surfaceStats(&dsm, &dtm, r, e0, n0, side/2)
		if s.Cells == 0 {
			b.Fatal("empty window")
		}
	}
}

func BenchmarkSurfaceWindow52(b *testing.B)  { benchSurface(b, 52) }
func BenchmarkSurfaceWindow512(b *testing.B) { benchSurface(b, 512) }

var errFake789 = errors.New("fake upstream failure")
