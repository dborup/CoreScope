package dhm

import (
	"context"
	"math"
	"math/rand"
	"runtime"
	"testing"
)

// #941 M1 — the predicted-reach planner, sweep and raster (coverage.go).

const covAarhusLat, covAarhusLon = 56.1629, 10.2039

// TestCoverageTiles941_Counts pins the aligned tile planner: one 512 px
// tile is 15.36 km at 30 m, so a 40 km disk needs 6–7 tiles per side and
// about 32–41 of them touch it (M0 §3.1). The exact counts for Aarhus
// are pinned so a change to the planner shows here.
func TestCoverageTiles941_Counts(t *testing.T) {
	e0, n0 := ToUTM32(covAarhusLat, covAarhusLon)
	for _, c := range []struct {
		radiusKm float64
		res      float64
		want     int
		lo, hi   int
	}{
		{20, 30, 10, 9, 16},
		{40, 30, 33, 32, 49},
		{60, 30, 66, 60, 81},
		{40, 20, 64, 64, 100},
	} {
		tiles := PlanCoverageTiles(e0, n0, c.radiusKm*1000, c.res)
		if len(tiles) < c.lo || len(tiles) > c.hi {
			t.Fatalf("r=%v km res=%v: %d tiles, outside %d–%d", c.radiusKm, c.res, len(tiles), c.lo, c.hi)
		}
		if len(tiles) != c.want {
			t.Fatalf("r=%v km res=%v: %d tiles, pinned %d", c.radiusKm, c.res, len(tiles), c.want)
		}
		seen := map[CoverageTile]bool{}
		for _, tl := range tiles {
			if seen[tl] {
				t.Fatalf("tile %+v planned twice", tl)
			}
			seen[tl] = true
		}
	}
}

// TestCoverageTiles941_CoverTheDisk: every point of the disk, its
// bilinear margin included, lies in a planned tile, and no planned tile
// misses the disk.
func TestCoverageTiles941_CoverTheDisk(t *testing.T) {
	e0, n0 := ToUTM32(covAarhusLat, covAarhusLon)
	const r, res = 40000.0, 30.0
	tiles := PlanCoverageTiles(e0, n0, r, res)
	span := CoverageTilePx * res
	has := map[CoverageTile]bool{}
	for _, tl := range tiles {
		has[tl] = true
		b := tl.BBox(res)
		dx := math.Max(0, math.Max(b[0]-e0, e0-b[2]))
		dy := math.Max(0, math.Max(b[1]-n0, n0-b[3]))
		if math.Hypot(dx, dy) > r+2*res {
			t.Fatalf("tile %+v is %.0f m from the centre, beyond the disk", tl, math.Hypot(dx, dy))
		}
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		a := rng.Float64() * 2 * math.Pi
		d := (r + res) * math.Sqrt(rng.Float64())
		e, n := e0+d*math.Sin(a), n0+d*math.Cos(a)
		tl := CoverageTile{X: int(math.Floor(e / span)), Y: int(math.Floor(n / span))}
		if !has[tl] {
			t.Fatalf("point %.0f,%.0f (%.0f m out) is in no planned tile", e, n, d)
		}
	}
}

// TestCoverageTiles941_RastersAreAllowListedAndAligned: a tile's raster
// is the allow-listed DTM shape, inside RasterExtent, at most 512 px, on
// the res grid, so the grid's pixel centres are the tile's cells; a tile
// off the extent asks for nothing.
func TestCoverageTiles941_RastersAreAllowListedAndAligned(t *testing.T) {
	for _, pt := range [][2]float64{{covAarhusLat, covAarhusLon}, {54.95, 8.1}, {57.7, 10.6}, {55.6, 12.6}, {54.8, 9.4}} {
		e0, n0 := ToUTM32(pt[0], pt[1])
		for _, tl := range PlanCoverageTiles(e0, n0, 40000, 30) {
			r, ok := tl.Raster(30)
			b := tl.BBox(30)
			ext := RasterExtent
			inside := b[2] > ext[0] && b[0] < ext[2] && b[3] > ext[1] && b[1] < ext[3]
			if ok != inside {
				t.Fatalf("tile %+v: raster ok=%v, overlaps extent=%v", tl, ok, inside)
			}
			if !ok {
				continue
			}
			if err := r.Validate(); err != nil {
				t.Fatalf("tile %+v: raster %+v not allow-listed: %v", tl, r, err)
			}
			if r.Model != ModelTerrain || r.Width > CoverageTilePx || r.Height > CoverageTilePx {
				t.Fatalf("tile %+v: raster %+v", tl, r)
			}
			for i, v := range r.BBox {
				if q := v / 30; math.Abs(q-math.Round(q)) > 1e-9 {
					t.Fatalf("tile %+v: bbox[%d] %v not on the 30 m grid", tl, i, v)
				}
			}
			if float64(r.Width)*30 != r.BBox[2]-r.BBox[0] || float64(r.Height)*30 != r.BBox[3]-r.BBox[1] {
				t.Fatalf("tile %+v: %dx%d px over %v", tl, r.Width, r.Height, r.BBox)
			}
		}
	}
}

// TestCoverageTileDm941_RoundTrip: a tile is cached as int16 decimetres
// (512 KiB); NaN survives as a gap, values round to 0.1 m, and junk does
// not decode.
func TestCoverageTileDm941_RoundTrip(t *testing.T) {
	v := make([]float32, CoverageTileCells)
	for i := range v {
		v[i] = float32(i%2000)*0.37 - 50
	}
	v[7] = float32(math.NaN())
	v[8] = 5000 // beyond int16 dm: a gap, not a wrapped value
	b := EncodeCoverageTile(v)
	if len(b) != CoverageTileBytes {
		t.Fatalf("encoded %d bytes, want %d", len(b), CoverageTileBytes)
	}
	got, err := DecodeCoverageTile(b)
	if err != nil {
		t.Fatal(err)
	}
	for i := range v {
		switch i {
		case 7, 8:
			if !math.IsNaN(float64(got[i])) {
				t.Fatalf("cell %d = %v, want a gap", i, got[i])
			}
		default:
			if math.Abs(float64(got[i]-v[i])) > 0.05+1e-4 {
				t.Fatalf("cell %d = %v, want %v ±0.05", i, got[i], v[i])
			}
		}
	}
	if _, err := DecodeCoverageTile(b[:100]); err == nil {
		t.Fatal("a short tile decoded")
	}
}

// TestCoverageTile941_FromGridMasksUncovered: a DHM raster is read at the
// tile's cell centres, and a cell the mask does not cover is a gap
// whatever the raster says (the WCS answers 0 on foreign land, #692).
func TestCoverageTile941_FromGridMasksUncovered(t *testing.T) {
	tl := CoverageTile{X: 37, Y: 405}
	r, ok := tl.Raster(30)
	if !ok {
		t.Fatal("tile off the extent")
	}
	g := Grid{Width: r.Width, Height: r.Height, Values: make([]float32, r.Width*r.Height),
		Georef: Georef{OriginE: r.BBox[0], OriginN: r.BBox[3], PixelE: 30, PixelN: 30}}
	for i := range g.Values {
		g.Values[i] = float32(i % 997)
	}
	maskE := r.BBox[0] + 100*30 // cells west of it are "foreign"
	vals := CoverageTileFromGrid(&g, tl, 30, func(e, n float64) bool { return e > maskE })
	ox, oy := tl.BBox(30)[0], tl.BBox(30)[3]
	for row := 0; row < CoverageTilePx; row += 37 {
		for col := 0; col < CoverageTilePx; col += 29 {
			e := ox + (float64(col)+0.5)*30
			v := vals[row*CoverageTilePx+col]
			if e <= maskE {
				if !math.IsNaN(float64(v)) {
					t.Fatalf("masked cell %d,%d = %v", col, row, v)
				}
				continue
			}
			gc := int((e - r.BBox[0]) / 30)
			gr := int((r.BBox[3] - (oy - (float64(row)+0.5)*30)) / 30)
			if gc < 0 || gc >= g.Width || gr < 0 || gr >= g.Height {
				continue
			}
			if want := g.Values[gr*g.Width+gc]; v != want {
				t.Fatalf("cell %d,%d = %v, want the raster's %v", col, row, v, want)
			}
		}
	}
}

// covGridFrom builds the elevation grid of a disk from elev, as the
// server does from its tiles.
func covGridFrom(e0, n0, r, res float64, elev func(e, n float64) float32) *CoverageGrid {
	g := NewCoverageGrid(e0, n0, r, res)
	for _, tl := range PlanCoverageTiles(e0, n0, r, res) {
		vals := make([]float32, CoverageTileCells)
		b := tl.BBox(res)
		for row := 0; row < CoverageTilePx; row++ {
			for col := 0; col < CoverageTilePx; col++ {
				vals[row*CoverageTilePx+col] = elev(b[0]+(float64(col)+0.5)*res, b[3]-(float64(row)+0.5)*res)
			}
		}
		g.Paste(tl, vals)
	}
	return g
}

// TestCoverageGrid941_PasteAndSample: the pasted tiles are the grid, and
// sampling between cell centres is bilinear.
func TestCoverageGrid941_PasteAndSample(t *testing.T) {
	e0, n0 := ToUTM32(covAarhusLat, covAarhusLon)
	plane := func(e, n float64) float32 { return float32(0.001*(e-e0) + 0.002*(n-n0) + 50) }
	g := covGridFrom(e0, n0, 10000, 30, plane)
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 2000; i++ {
		e := e0 + (rng.Float64()*2-1)*9900
		n := n0 + (rng.Float64()*2-1)*9900
		v := g.Sample(e, n)
		if math.Abs(float64(v)-float64(plane(e, n))) > 1e-3 {
			t.Fatalf("sample at %.1f,%.1f = %v, want %v", e, n, v, plane(e, n))
		}
	}
	if !math.IsNaN(float64(g.Sample(e0+50000, n0))) {
		t.Fatal("a point off the grid has a value")
	}
}

// sweepFor runs one radial of h with the reach sweep and returns its
// per-receiver loss and line-of-sight ratio.
func sweepFor(h []float32, res, txH, rxH, freq, k float64, rxEvery int) ([]float32, []float32) {
	s := newRadialSweep(res, freq, k, len(h)-1, rxEvery)
	nRx := (len(h) - 1) / rxEvery
	loss, ratio := make([]float32, nRx), make([]float32, nRx)
	s.run(h, txH, rxH, loss, ratio)
	return loss, ratio
}

// TestCoverageSweep941_MatchesLinkReference is the DRY property: for
// random terrain and antennas, every receiver of the fast sweep has the
// worst clearance ratio dhm.Link.Assess reports for the same path, and
// the free-space plus Bullington loss of dhm.Link.Bullington.
func TestCoverageSweep941_MatchesLinkReference(t *testing.T) {
	rng := rand.New(rand.NewSource(9410))
	for trial := 0; trial < 60; trial++ {
		res := []float64{20, 30, 60}[trial%3]
		n := 50 + rng.Intn(300)
		h := make([]float32, n+1)
		ph := rng.Float64() * 10
		for i := range h {
			h[i] = float32(25 + 25*math.Sin(float64(i)/(5+ph)) + 15*math.Sin(float64(i)/2.1+ph) + rng.Float64()*3)
		}
		txH, rxH := 1+rng.Float64()*40, 1+rng.Float64()*10
		freq := 433 + rng.Float64()*500
		k := []float64{DefaultKFactor, 1, 4}[trial%3]
		rxEvery := 1 + trial%3
		loss, ratio := sweepFor(h, res, txH, rxH, freq, k, rxEvery)
		lambda := Wavelength(freq)
		for r := range loss {
			j := (r + 1) * rxEvery
			link := Link{DistanceM: float64(j) * res, HeightA: float64(h[0]) + txH, HeightB: float64(h[j]) + rxH, LambdaM: lambda, K: k}
			b, ok := link.Bullington(h[:j+1])
			if !ok {
				t.Fatal("reference failed")
			}
			want := FreeSpaceLoss(link.DistanceM, lambda) + b.Loss
			if math.Abs(float64(loss[r])-want) > 1e-3 {
				t.Fatalf("trial %d rx %d (j=%d): loss %v, reference %v", trial, r, j, loss[r], want)
			}
			if j < 2 {
				continue
			}
			a := link.Assess(h[:j+1])
			if d := math.Abs(float64(ratio[r]) - a.WorstRatio); d > 1e-4*math.Max(1, math.Abs(a.WorstRatio)) {
				t.Fatalf("trial %d rx %d: ratio %v, Assess %v", trial, r, ratio[r], a.WorstRatio)
			}
		}
	}
}

// TestCoverageSweep941_GapEndsTheRadial: past a gap no receiver has a
// loss; before it they all do.
func TestCoverageSweep941_GapEndsTheRadial(t *testing.T) {
	h := make([]float32, 101)
	h[40] = float32(math.NaN())
	loss, _ := sweepFor(h, 30, 10, 2, FirmwareFreqMHz, DefaultKFactor, 1)
	for r, v := range loss {
		j := r + 1
		if gap := math.IsNaN(float64(v)); gap != (j >= 40) {
			t.Fatalf("receiver %d: loss %v", j, v)
		}
	}
	h[40] = 0
	h[0] = float32(math.NaN())
	loss, _ = sweepFor(h, 30, 10, 2, FirmwareFreqMHz, DefaultKFactor, 1)
	for _, v := range loss {
		if !math.IsNaN(float64(v)) {
			t.Fatal("a receiver has a loss without the transmitter's ground")
		}
	}
}

// TestCoverageSweep941_FlatEarthHorizon: over flat ground the line of
// sight ends at the radio horizon √(2kR·h1) + √(2kR·h2), within one
// sample, and the loss jumps by the grazing knife edge there.
func TestCoverageSweep941_FlatEarthHorizon(t *testing.T) {
	for _, c := range []struct{ tx, rx, k float64 }{
		{10, 2, DefaultKFactor}, {30, 2, DefaultKFactor}, {10, 10, 1},
	} {
		const res = 30.0
		n := 2000
		h := make([]float32, n+1)
		_, ratio := sweepFor(h, res, c.tx, c.rx, FirmwareFreqMHz, c.k, 1)
		kr := 2 * c.k * EarthRadiusM
		horizon := math.Sqrt(kr*c.tx) + math.Sqrt(kr*c.rx)
		first := -1
		for r, v := range ratio {
			if v <= 0 {
				first = r + 1
				break
			}
		}
		if first < 0 {
			t.Fatalf("tx %v rx %v: no horizon within %v m (expected %.0f m)", c.tx, c.rx, float64(n)*res, horizon)
		}
		got := float64(first) * res
		if math.Abs(got-horizon) > res {
			t.Fatalf("tx %v rx %v k %v: horizon at %.0f m, analytic %.0f m", c.tx, c.rx, c.k, got, horizon)
		}
		// Every receiver before it is in line of sight, every one after it
		// is not: flat ground has no second horizon.
		for r, v := range ratio {
			if (v > 0) != (r+1 < first) {
				t.Fatalf("receiver %d out of order: ratio %v", r+1, v)
			}
		}
	}
}

// TestCoverageRaster941_PolarToMercator: the raster is Web Mercator
// aligned around the disk; a pixel's loss is the polar result at its
// distance and bearing from the centre, beyond the radius it is no data.
func TestCoverageRaster941_PolarToMercator(t *testing.T) {
	e0, n0 := ToUTM32(covAarhusLat, covAarhusLon)
	const r, res, out = 10000.0, 30.0, 100.0
	spec := CoverageSpec{Center: LatLon{Lat: covAarhusLat, Lon: covAarhusLon}, RadiusM: r, ResolutionM: res, OutPixelM: out}
	pol := newCoveragePolar(spec)
	// Loss is 60 dB + 1 dB per 250 m, +20 dB on the eastern half.
	for a := 0; a < pol.nRad; a++ {
		bearing := 2 * math.Pi * float64(a) / float64(pol.nRad)
		for k := 0; k < pol.nRx; k++ {
			d := float64((k+1)*pol.rxEvery) * res
			v := 60 + d/250
			if math.Sin(bearing) > 0 {
				v += 20
			}
			pol.loss[a*pol.nRx+k] = float32(v)
		}
	}
	ras := pol.render()
	if ras.Width%16 != 0 || ras.Height%16 != 0 || ras.Width < 190 || ras.Width > 230 {
		t.Fatalf("raster %dx%d", ras.Width, ras.Height)
	}
	s, w, nn, ee := ras.Bounds[0][0], ras.Bounds[0][1], ras.Bounds[1][0], ras.Bounds[1][1]
	// The bounds hold the disk.
	for a := 0.0; a < 360; a += 5 {
		la, lo := FromUTM32(e0+r*math.Sin(a*math.Pi/180), n0+r*math.Cos(a*math.Pi/180))
		if la < s || la > nn || lo < w || lo > ee {
			t.Fatalf("disk point at %v° (%v,%v) outside bounds %v", a, la, lo, ras.Bounds)
		}
	}
	px := func(lat, lon float64) uint8 {
		x0, y0 := lonLatToMercator(w, nn)
		x1, y1 := lonLatToMercator(ee, s)
		x, y := lonLatToMercator(lon, lat)
		col := int((x - x0) / (x1 - x0) * float64(ras.Width))
		row := int((y0 - y) / (y0 - y1) * float64(ras.Height))
		return ras.Loss[row*ras.Width+col]
	}
	for _, c := range []struct {
		dE, dN float64
		want   float64
	}{
		{0, 5000, 60 + 5000.0/250},      // north
		{0, -5000, 60 + 5000.0/250},     // south
		{6000, 0, 60 + 6000.0/250 + 20}, // east: the +20 dB half
		{-6000, 0, 60 + 6000.0/250},     // west
	} {
		la, lo := FromUTM32(e0+c.dE, n0+c.dN)
		v := px(la, lo)
		got := CoverageLossBaseDb + float64(v)*CoverageLossStepDb
		if math.Abs(got-c.want) > 1.0 {
			t.Fatalf("offset %v,%v: pixel %d = %.1f dB, want %.1f", c.dE, c.dN, v, got, c.want)
		}
	}
	la, lo := FromUTM32(e0+r*0.75, n0+r*0.75) // 1.06 r out
	if v := px(la, lo); v != CoverageNoData {
		t.Fatalf("a pixel beyond the radius is %d, want no data", v)
	}
}

// TestComputeCoverage941_EndToEnd runs the whole pure pipeline on a
// synthetic ridge: the ground under the centre is reported, the area
// behind the ridge loses more than the open side, and the inner loop
// stays within its analytic bound.
func TestComputeCoverage941_EndToEnd(t *testing.T) {
	e0, n0 := ToUTM32(covAarhusLat, covAarhusLon)
	const r, res = 15000.0, 30.0
	ridgeE := e0 + 3000
	g := covGridFrom(e0, n0, r, res, func(e, n float64) float32 {
		v := 10 + 0.0005*(n-n0)
		if math.Abs(e-ridgeE) < 60 {
			v += 80
		}
		return float32(v)
	})
	spec := CoverageSpec{Center: LatLon{Lat: covAarhusLat, Lon: covAarhusLon}, RadiusM: r, ResolutionM: res,
		TxHeightM: 10, RxHeightM: 2, FreqMHz: FirmwareFreqMHz, K: DefaultKFactor, OutPixelM: 100, Workers: 2}
	got, err := ComputeCoverage(context.Background(), g, spec)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got.GroundM-10) > 0.01 {
		t.Fatalf("ground %v", got.GroundM)
	}
	if got.CoveredPct < 99 {
		// Every pixel of the disk has terrain under its whole path.
		t.Fatalf("covered %.1f %%", got.CoveredPct)
	}
	at := func(dE, dN float64) float64 {
		la, lo := FromUTM32(e0+dE, n0+dN)
		x0, y0 := lonLatToMercator(got.Bounds[0][1], got.Bounds[1][0])
		x1, y1 := lonLatToMercator(got.Bounds[1][1], got.Bounds[0][0])
		x, y := lonLatToMercator(lo, la)
		col := int((x - x0) / (x1 - x0) * float64(got.Width))
		row := int((y0 - y) / (y0 - y1) * float64(got.Height))
		return CoverageLossBaseDb + float64(got.Loss[row*got.Width+col])*CoverageLossStepDb
	}
	east, west := at(8000, 0), at(-8000, 0)
	if east < west+15 {
		t.Fatalf("behind the ridge %.1f dB, open side %.1f dB: the ridge does not shadow", east, west)
	}
	pol := newCoveragePolar(spec)
	bound := int64(pol.nRad) * pol.innerPerRadial()
	if got.Iterations <= 0 || got.Iterations > bound {
		t.Fatalf("inner iterations %d, bound %d", got.Iterations, bound)
	}
	// Cancelling stops the sweep.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ComputeCoverage(ctx, g, spec); err == nil {
		t.Fatal("a cancelled computation finished")
	}
}

// covSyntheticDEM is rolling Danish-like terrain: 0–120 m hills.
func covSyntheticDEM(e0, n0 float64) func(e, n float64) float32 {
	return func(e, n float64) float32 {
		x, y := (e-e0)/1000, (n-n0)/1000
		v := 40 + 25*math.Sin(x/3.1)*math.Cos(y/4.3) + 15*math.Sin(x/0.9+y/1.7) + 8*math.Cos(x/0.37-y/0.53) + 0.4*x
		return float32(math.Max(0, v))
	}
}

// TestComputeCoverage941_MemoryBudget: one 40 km / 30 m computation, the
// elevation grid included, allocates at most 64 MB (M0 §6 perf budget).
func TestComputeCoverage941_MemoryBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	e0, n0 := ToUTM32(covAarhusLat, covAarhusLon)
	spec := CoverageSpec{Center: LatLon{Lat: covAarhusLat, Lon: covAarhusLon}, RadiusM: 40000, ResolutionM: 30,
		TxHeightM: 10, RxHeightM: 2, FreqMHz: FirmwareFreqMHz, K: DefaultKFactor, OutPixelM: 100, Workers: 4}
	elev := covSyntheticDEM(e0, n0)
	tiles := PlanCoverageTiles(e0, n0, spec.RadiusM, spec.ResolutionM)
	cached := make([][]float32, len(tiles))
	for i, tl := range tiles {
		cached[i] = make([]float32, CoverageTileCells)
		b := tl.BBox(30)
		for row := 0; row < CoverageTilePx; row++ {
			for col := 0; col < CoverageTilePx; col++ {
				cached[i][row*CoverageTilePx+col] = elev(b[0]+(float64(col)+0.5)*30, b[3]-(float64(row)+0.5)*30)
			}
		}
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	g := NewCoverageGrid(e0, n0, spec.RadiusM, spec.ResolutionM)
	for i, tl := range tiles {
		g.Paste(tl, cached[i])
	}
	if _, err := ComputeCoverage(context.Background(), g, spec); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	alloc := after.TotalAlloc - before.TotalAlloc
	t.Logf("40 km / 30 m: %.1f MB allocated", float64(alloc)/(1<<20))
	if alloc > 64<<20 {
		t.Fatalf("allocated %.1f MB, budget 64 MB", float64(alloc)/(1<<20))
	}
}

// BenchmarkCoverage40km30m is the warm-elevation computation of the
// default reach: 40 km at 30 m, receivers every 90 m, a 100 m raster, on
// synthetic terrain, with the server's worker count on this machine.
func BenchmarkCoverage40km30m(b *testing.B) {
	e0, n0 := ToUTM32(covAarhusLat, covAarhusLon)
	spec := CoverageSpec{Center: LatLon{Lat: covAarhusLat, Lon: covAarhusLon}, RadiusM: 40000, ResolutionM: 30,
		TxHeightM: 10, RxHeightM: 2, FreqMHz: FirmwareFreqMHz, K: DefaultKFactor, OutPixelM: 100,
		Workers: CoverageWorkers()}
	g := covGridFrom(e0, n0, spec.RadiusM, spec.ResolutionM, covSyntheticDEM(e0, n0))
	b.ResetTimer()
	var iters int64
	for i := 0; i < b.N; i++ {
		res, err := ComputeCoverage(context.Background(), g, spec)
		if err != nil {
			b.Fatal(err)
		}
		iters = res.Iterations
	}
	b.ReportMetric(float64(iters), "inner-iters")
	b.ReportMetric(float64(spec.Workers), "workers")
}
