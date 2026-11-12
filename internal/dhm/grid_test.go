package dhm

import (
	"math"
	"math/rand"
	"testing"
)

// #681 M2 — bilinear sampling with nodata propagation.

// plane is an analytic ramp: bilinear interpolation reproduces any plane
// exactly between pixel centres.
func plane(e, n float64) float64 { return 25 + 0.013*(e-575000) - 0.021*(n-6224000) }

// rampGrid is a w×h grid at 10 m pixels whose value at each pixel centre
// is plane().
func rampGrid(w, h int) *Grid {
	g := &Grid{Width: w, Height: h, Georef: Georef{OriginE: 575000, OriginN: 6226500, PixelE: 10, PixelN: 10}}
	g.Values = make([]float32, w*h)
	for row := 0; row < h; row++ {
		for col := 0; col < w; col++ {
			g.Values[row*w+col] = float32(plane(g.centre(col, row)))
		}
	}
	return g
}

func (g *Grid) centre(col, row int) (e, n float64) {
	return g.Georef.OriginE + (float64(col)+0.5)*g.Georef.PixelE, g.Georef.OriginN - (float64(row)+0.5)*g.Georef.PixelN
}

func TestSampleReproducesAnalyticRamp(t *testing.T) {
	g := rampGrid(20, 15)
	rng := rand.New(rand.NewSource(681))
	x := g.Extent()
	for i := 0; i < 2000; i++ {
		// Anywhere between the outermost pixel centres.
		e := x[0] + 5 + rng.Float64()*(x[2]-x[0]-10)
		n := x[1] + 5 + rng.Float64()*(x[3]-x[1]-10)
		got, ok := g.Sample(e, n)
		if !ok || math.Abs(got-plane(e, n)) > 1e-4 {
			t.Fatalf("Sample(%.3f, %.3f) = %v %v, want %v", e, n, got, ok, plane(e, n))
		}
	}
	// Exactly on pixel centres the stored value comes back.
	for _, c := range [][2]int{{0, 0}, {19, 14}, {7, 3}, {19, 0}, {0, 14}} {
		e, n := g.centre(c[0], c[1])
		got, ok := g.Sample(e, n)
		if !ok || got != float64(g.Values[c[1]*20+c[0]]) {
			t.Fatalf("centre %v: %v %v", c, got, ok)
		}
	}
}

// Between the outermost pixel centres and the extent edge the edge pixels
// are held constant; outside the extent there is no value.
func TestSampleEdgesAndOutside(t *testing.T) {
	g := rampGrid(20, 15)
	x := g.Extent()
	e0, n0 := g.centre(0, 0)
	if got, ok := g.Sample(x[0], x[3]); !ok || math.Abs(got-plane(e0, n0)) > 1e-4 {
		t.Fatalf("north-west corner %v %v, want %v", got, ok, plane(e0, n0))
	}
	e1, n1 := g.centre(19, 14)
	if got, ok := g.Sample(x[2], x[1]); !ok || math.Abs(got-plane(e1, n1)) > 1e-4 {
		t.Fatalf("south-east corner %v %v, want %v", got, ok, plane(e1, n1))
	}
	mid := [2]float64{(x[0] + x[2]) / 2, (x[1] + x[3]) / 2}
	for name, p := range map[string][2]float64{
		"west":  {x[0] - 0.01, mid[1]},
		"east":  {x[2] + 0.01, mid[1]},
		"south": {mid[0], x[1] - 0.01},
		"north": {mid[0], x[3] + 0.01},
		"far":   {0, 0},
		"NaN e": {math.NaN(), mid[1]},
		"NaN n": {mid[0], math.NaN()},
		"Inf":   {math.Inf(1), mid[1]},
	} {
		if v, ok := g.Sample(p[0], p[1]); ok {
			t.Errorf("%s: %v accepted", name, v)
		}
	}
	// A 1×1 grid is one constant pixel.
	one := &Grid{Width: 1, Height: 1, Values: []float32{7}, Georef: Georef{575000, 6226500, 10, 10}}
	if v, ok := one.Sample(575003, 6226497); !ok || v != 7 {
		t.Fatalf("1x1: %v %v", v, ok)
	}
}

// A gap is never smeared: any pixel carrying weight that is nodata makes
// the sample nodata, while a sample exactly on a valid pixel centre next to
// the gap keeps its value.
func TestSampleNoDataPropagates(t *testing.T) {
	for name, gap := range map[string]float32{
		"GDAL_NODATA": -9999,
		"NaN":         float32(math.NaN()),
		"+Inf":        float32(math.Inf(1)),
		"-Inf":        float32(math.Inf(-1)),
	} {
		g := rampGrid(6, 5)
		g.NoData, g.HasNoData = -9999, true
		g.Values[2*6+3] = gap // (col 3, row 2)
		eGap, nGap := g.centre(3, 2)
		eL, nL := g.centre(2, 2)
		if _, ok := g.Sample(eGap, nGap); ok {
			t.Errorf("%s: the gap itself has a value", name)
		}
		for _, p := range [][2]float64{
			{(eGap + eL) / 2, nL},     // between the gap and its west neighbour
			{eGap - 9.99, nGap + 0.1}, // just inside the west neighbour's reach
			{eGap + 5, nGap - 5},      // diagonal cell
			{eGap - 0.001, nGap},      // within a millimetre of the gap
		} {
			if v, ok := g.Sample(p[0], p[1]); ok {
				t.Errorf("%s: sample at %.3f %.3f next to the gap = %v", name, p[0], p[1], v)
			}
		}
		// On the neighbour's centre the gap carries no weight.
		if v, ok := g.Sample(eL, nL); !ok || v != float64(g.Values[2*6+2]) {
			t.Errorf("%s: valid centre next to the gap: %v %v", name, v, ok)
		}
		// Two cells away is untouched.
		eFar, nFar := g.centre(0, 0)
		if v, ok := g.Sample(eFar+3, nFar-3); !ok || math.Abs(v-plane(eFar+3, nFar-3)) > 1e-4 {
			t.Errorf("%s: far sample %v %v", name, v, ok)
		}
	}
	// Without GDAL_NODATA, -9999 is an elevation like any other.
	g := rampGrid(2, 1)
	g.Values[0], g.Values[1] = -9999, -9999
	if v, ok := g.Sample(575010, 6226495); !ok || v != -9999 {
		t.Fatalf("no nodata tag: %v %v", v, ok)
	}
}

// A grid whose Values do not match its size is unusable, not a panic.
func TestSampleInconsistentGrid(t *testing.T) {
	for _, g := range []*Grid{
		{Width: 3, Height: 3, Values: make([]float32, 8), Georef: Georef{575000, 6226500, 10, 10}},
		{Width: 0, Height: 3, Georef: Georef{575000, 6226500, 10, 10}},
		{},
	} {
		if v, ok := g.Sample(575005, 6226495); ok {
			t.Fatalf("%dx%d/%d: %v", g.Width, g.Height, len(g.Values), v)
		}
	}
}

// Decoded georeferencing and the sampler agree: a PixelIsPoint file and a
// PixelIsArea file of the same raster give the same samples.
func TestSampleOnDecodedGeoTIFF(t *testing.T) {
	src := rampGrid(8, 6)
	area := newFixture(8, 6, src.Values).georeferenced(575000, 6226500, 10)
	area.compression, area.predictor = compressionDeflate, predictorFloatingPoint
	point := newFixture(8, 6, src.Values).georeferenced(575005, 6226495, 10)
	point.geoKeys = utm32GeoKeys(rasterPixelIsPoint)
	for name, f := range map[string]*tiffFixture{"area": area, "point": point} {
		g, err := decodeGeoTIFF(f.bytes())
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range [][2]float64{{575012.5, 6226471}, {575040, 6226480}, {575075, 6226445.5}} {
			v, ok := g.Sample(p[0], p[1])
			if !ok || math.Abs(v-plane(p[0], p[1])) > 1e-4 {
				t.Errorf("%s: Sample(%v) = %v %v, want %v", name, p, v, ok, plane(p[0], p[1]))
			}
		}
	}
}

// 1024 samples on one 180×180 chunk, the plan's default (§1.3, §7).
func BenchmarkSample180(b *testing.B) {
	g := rampGrid(180, 180)
	x := g.Extent()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for k := 0; k < 1024; k++ {
			f := float64(k) / 1023
			if _, ok := g.Sample(x[0]+f*(x[2]-x[0]), x[3]-f*(x[3]-x[1])); !ok {
				b.Fatal("no value")
			}
		}
	}
}
