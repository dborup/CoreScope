package dhm

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
)

// #941 M1 — the predicted reach of a radio from one point: the area
// within RadiusM where a receiver RxHeightM above the ground is predicted
// to hear a transmitter TxHeightM above the ground at the centre.
//
// Pure: no I/O. The server (cmd/server/terrain_coverage.go) fetches and
// caches the elevation tiles PlanCoverageTiles names, pastes them into a
// CoverageGrid and calls ComputeCoverage.
//
//   - Tiles: CoverageTilePx² cells of ResolutionM, on a fixed grid in
//     EPSG:25832 (tile X covers [X·span, (X+1)·span), span = 512·res), so
//     neighbouring requests share tiles and one tile is one allow-listed
//     WCS raster (Raster). A tile is cached as int16 decimetres:
//     CoverageTileBytes, 512 KiB.
//   - Radials: one per output pixel at the edge (2πR / OutPixelM), each
//     sampled every ResolutionM from the centre; a receiver every
//     OutPixelM/ResolutionM samples (every 90 m at the 30 m / 100 m
//     defaults).
//   - Model: free-space loss plus the Bullington diffraction of P.526
//     §4.5.1 (diffraction.go) over the terrain with the earth bulge, per
//     receiver, in one O(n) pass. The pass also yields the line-of-sight
//     clearance ratio Link.Assess reports for the same path
//     (TestCoverageSweep941_MatchesLinkReference). With PathDelta (#941
//     M2) the same pass also takes the obstruction terms of P.526
//     §4.5.2's complete method, and two running sums per radial fit the
//     smooth surface in O(1) per receiver (delta.go).
//   - Output: the loss in dB, quantised to CoverageLossStepDb above
//     CoverageLossBaseDb into one byte per pixel of a Web Mercator raster
//     around the disk (CoverageNoData outside it or without terrain),
//     placed through the elevation overlay's lattice (mercLattice).
//     Power, gains and sensitivity are not part of it: they only shift
//     the margin, so the browser applies them.

const (
	// CoverageTilePx is a tile's edge in cells: one MaxRasterSize raster.
	CoverageTilePx    = MaxRasterSize
	CoverageTileCells = CoverageTilePx * CoverageTilePx
	// CoverageTileBytes is one cached tile: int16 decimetres.
	CoverageTileBytes = 2 * CoverageTileCells

	// CoverageLossBaseDb is the loss of raster value 0; each step adds
	// CoverageLossStepDb, so 0–254 span 60–187 dB (free space at 30 m is
	// 61 dB; the default link budget is 158 dB). A larger loss is 254.
	CoverageLossBaseDb = 60.0
	CoverageLossStepDb = 0.5
	// CoverageNoData is a raster pixel without a prediction: outside the
	// radius, or a path with a gap in the terrain.
	CoverageNoData = 255

	// CoverageModel names the Bullington model and its version (PathModel
	// ID; CoverageModelDelta is the complete method's): part of the cache
	// key and the response.
	CoverageModel = "bullington-p526-v1"

	// CoverageOutPixelM is the default raster pixel; coverageMaxEdge caps
	// the raster edge, so a larger radius gets coarser pixels.
	CoverageOutPixelM = 100.0
	coverageMaxEdge   = 1008
	// coverageLatticeCell is the raster lattice's cell in output pixels,
	// as the elevation tiles' (elevationCell).
	coverageLatticeCell = 16
	// coverageTileNoData is a gap in an encoded tile.
	coverageTileNoData = math.MinInt16
)

// ErrCoverageTile: a cached tile of the wrong size.
var ErrCoverageTile = errors.New("dhm: malformed coverage tile")

// CoverageTile is one aligned elevation tile.
type CoverageTile struct{ X, Y int }

// BBox is the tile's extent in EPSG:25832 at resolution res: minE, minN,
// maxE, maxN.
func (t CoverageTile) BBox(res float64) [4]float64 {
	span := CoverageTilePx * res
	return [4]float64{float64(t.X) * span, float64(t.Y) * span, float64(t.X+1) * span, float64(t.Y+1) * span}
}

// Raster is the DTM raster that fills tile t from the WCS: the tile's
// bbox clipped to RasterExtent on the res grid, one pixel per cell. ok is
// false when nothing of the tile is inside the extent.
func (t CoverageTile) Raster(res float64) (RasterRequest, bool) {
	b := t.BBox(res)
	ext := RasterExtent
	c := [4]float64{
		math.Max(b[0], math.Ceil(ext[0]/res)*res), math.Max(b[1], math.Ceil(ext[1]/res)*res),
		math.Min(b[2], math.Floor(ext[2]/res)*res), math.Min(b[3], math.Floor(ext[3]/res)*res),
	}
	if !(c[2] > c[0] && c[3] > c[1]) {
		return RasterRequest{}, false
	}
	return RasterRequest{Model: ModelTerrain, BBox: c,
		Width: int(math.Round((c[2] - c[0]) / res)), Height: int(math.Round((c[3] - c[1]) / res))}, true
}

// Cell is the EPSG:25832 centre of cell (col, row) of tile t, row 0 the
// northernmost.
func (t CoverageTile) Cell(res float64, col, row int) (e, n float64) {
	b := t.BBox(res)
	return b[0] + (float64(col)+0.5)*res, b[3] - (float64(row)+0.5)*res
}

// coverageMarginM is the margin around the disk the grid holds: the last
// sample's bilinear neighbours.
func coverageMarginM(res float64) float64 { return 2 * res }

// PlanCoverageTiles lists the tiles a disk of radius radiusM around
// e0, n0 needs at resolution res: every aligned tile within
// radiusM + 2·res of the centre, row by row from the south-west.
func PlanCoverageTiles(e0, n0, radiusM, res float64) []CoverageTile {
	span := CoverageTilePx * res
	m := radiusM + coverageMarginM(res)
	var out []CoverageTile
	for y := int(math.Floor((n0 - m) / span)); y <= int(math.Floor((n0+m)/span)); y++ {
		for x := int(math.Floor((e0 - m) / span)); x <= int(math.Floor((e0+m)/span)); x++ {
			t := CoverageTile{X: x, Y: y}
			b := t.BBox(res)
			dx := math.Max(0, math.Max(b[0]-e0, e0-b[2]))
			dy := math.Max(0, math.Max(b[1]-n0, n0-b[3]))
			if math.Hypot(dx, dy) <= m {
				out = append(out, t)
			}
		}
	}
	return out
}

// CoverageTileFromGrid reads tile t's cells from a DHM raster g: a cell
// that covered rejects (foreign land, where the WCS answers 0, #692; nil
// covers everything), or that g has no value for, is a gap (NaN). A cell
// centre on a pixel centre is read exactly, otherwise bilinearly.
func CoverageTileFromGrid(g *Grid, t CoverageTile, res float64, covered func(e, n float64) bool) []float32 {
	out := make([]float32, CoverageTileCells)
	nan := float32(math.NaN())
	valid := g.Width >= 1 && g.Height >= 1 && len(g.Values) == g.Width*g.Height && g.Georef.valid()
	for row := 0; row < CoverageTilePx; row++ {
		for col := 0; col < CoverageTilePx; col++ {
			i := row*CoverageTilePx + col
			out[i] = nan
			e, n := t.Cell(res, col, row)
			if !valid || (covered != nil && !covered(e, n)) {
				continue
			}
			fx := (e-g.Georef.OriginE)/g.Georef.PixelE - 0.5
			fy := (g.Georef.OriginN-n)/g.Georef.PixelN - 0.5
			ix, iy := math.Round(fx), math.Round(fy)
			if math.Abs(fx-ix) < 1e-6 && math.Abs(fy-iy) < 1e-6 && ix >= 0 && iy >= 0 && int(ix) < g.Width && int(iy) < g.Height {
				if v, ok := g.cell(int(ix), int(iy)); ok {
					out[i] = float32(v)
				}
				continue
			}
			if v, ok := g.Sample(e, n); ok {
				out[i] = float32(v)
			}
		}
	}
	return out
}

// EncodeCoverageTile packs a tile as little-endian int16 decimetres; a
// gap, or a height beyond ±3276.7 m, is coverageTileNoData.
func EncodeCoverageTile(v []float32) []byte {
	out := make([]byte, 2*len(v))
	for i, x := range v {
		dm := math.Round(float64(x) * 10)
		q := int16(coverageTileNoData)
		if dm > coverageTileNoData && dm <= math.MaxInt16 { // NaN fails
			q = int16(dm)
		}
		binary.LittleEndian.PutUint16(out[2*i:], uint16(q))
	}
	return out
}

// DecodeCoverageTile unpacks EncodeCoverageTile's bytes of one tile.
func DecodeCoverageTile(b []byte) ([]float32, error) {
	if len(b) != CoverageTileBytes {
		return nil, ErrCoverageTile
	}
	out := make([]float32, CoverageTileCells)
	nan := float32(math.NaN())
	for i := range out {
		q := int16(binary.LittleEndian.Uint16(b[2*i:]))
		if q == coverageTileNoData {
			out[i] = nan
		} else {
			out[i] = float32(q) / 10
		}
	}
	return out, nil
}

// PointsPlan is a plan of free points that are not fetched from DHM:
// every point counts as not covered, so the foreign fill (internal/copdem)
// fills them all. The reach fills the cells of a tile DHM does not cover
// with it.
func PointsPlan(e, n []float64) Plan {
	return Plan{E: e, N: n, inside: make([]bool, len(e))}
}

// CoverageGrid is the elevation of one disk: cells of Res on the tiles'
// grid, NaN without data, row 0 the northernmost.
type CoverageGrid struct {
	OriginE, OriginN float64 // west edge, north edge
	Res              float64
	Width, Height    int
	Values           []float32
}

// NewCoverageGrid is an empty grid over the disk of radius radiusM around
// e0, n0 and its margin.
func NewCoverageGrid(e0, n0, radiusM, res float64) *CoverageGrid {
	m := radiusM + coverageMarginM(res)
	x0, x1 := math.Floor((e0-m)/res)*res, math.Ceil((e0+m)/res)*res
	y0, y1 := math.Floor((n0-m)/res)*res, math.Ceil((n0+m)/res)*res
	g := &CoverageGrid{OriginE: x0, OriginN: y1, Res: res,
		Width: int(math.Round((x1 - x0) / res)), Height: int(math.Round((y1 - y0) / res))}
	g.Values = make([]float32, g.Width*g.Height)
	nan := float32(math.NaN())
	for i := range g.Values {
		g.Values[i] = nan
	}
	return g
}

// Paste copies tile t's cells into the part of the grid it overlaps.
func (g *CoverageGrid) Paste(t CoverageTile, vals []float32) {
	if len(vals) != CoverageTileCells {
		return
	}
	b := t.BBox(g.Res)
	col0 := int(math.Round((b[0] - g.OriginE) / g.Res))
	row0 := int(math.Round((g.OriginN - b[3]) / g.Res))
	for r := max(0, -row0); r < CoverageTilePx && row0+r < g.Height; r++ {
		c0 := max(0, -col0)
		c1 := min(CoverageTilePx, g.Width-col0)
		if c1 <= c0 {
			return
		}
		copy(g.Values[(row0+r)*g.Width+col0+c0:(row0+r)*g.Width+col0+c1], vals[r*CoverageTilePx+c0:r*CoverageTilePx+c1])
	}
}

// Sample interpolates the elevation at e, n bilinearly between cell
// centres; NaN off the grid or next to a gap.
func (g *CoverageGrid) Sample(e, n float64) float32 {
	fx := (e-g.OriginE)/g.Res - 0.5
	fy := (g.OriginN-n)/g.Res - 0.5
	if !(fx >= 0 && fy >= 0 && fx <= float64(g.Width-1) && fy <= float64(g.Height-1)) {
		return float32(math.NaN())
	}
	c0, r0 := int(fx), int(fy)
	c1, r1 := min(c0+1, g.Width-1), min(r0+1, g.Height-1)
	tx, ty := float32(fx-float64(c0)), float32(fy-float64(r0))
	v := g.Values
	top := v[r0*g.Width+c0]*(1-tx) + v[r0*g.Width+c1]*tx
	bot := v[r1*g.Width+c0]*(1-tx) + v[r1*g.Width+c1]*tx
	// A NaN neighbour poisons the sum, so a value next to a gap is a gap
	// even at zero weight: the conservative choice.
	return top*(1-ty) + bot*ty
}

// CoverageSpec is one reach computation.
type CoverageSpec struct {
	Center      LatLon
	RadiusM     float64
	ResolutionM float64 // grid cell and radial sample step
	TxHeightM   float64 // transmitter above the ground at the centre
	RxHeightM   float64 // receiver above the ground everywhere
	FreqMHz     float64
	K           float64
	OutPixelM   float64 // raster pixel; 0 is CoverageOutPixelFor(RadiusM)
	Workers     int     // radial workers; < 1 is 1
	// Model is the propagation model; "" is PathBullington (#941 M2).
	Model PathModel
}

// CoverageOutPixelFor is the raster pixel for a radius: CoverageOutPixelM,
// coarser when the raster edge would exceed coverageMaxEdge.
func CoverageOutPixelFor(radiusM float64) float64 {
	return math.Max(CoverageOutPixelM, 2*radiusM/float64(coverageMaxEdge-2*coverageLatticeCell))
}

// CoverageWorkers is the server's radial workers: half the CPUs, at least
// one, so a computation never takes the whole machine from the API.
func CoverageWorkers() int { return max(1, runtime.GOMAXPROCS(0)/2) }

// CoverageResult is a computed reach.
type CoverageResult struct {
	Width, Height int
	// Bounds is the raster's extent, [[south, west], [north, east]], in
	// WGS84 degrees: Leaflet's image overlay bounds.
	Bounds [2][2]float64
	// Loss is Width × Height bytes, row 0 north: CoverageNoData, or the
	// loss in CoverageLossStepDb steps above CoverageLossBaseDb.
	Loss []uint8
	// GroundM is the terrain at the centre, NaN without data.
	GroundM float64
	// CoveredPct is the share of the disk's pixels that have a value.
	CoveredPct float64
	// Iterations is the inner-loop count: receiver × interior sample.
	Iterations int64
}

// coveragePolar is the polar loss of one computation: nRad radials of nRx
// receivers, receiver k at (k+1)·rxEvery samples from the centre.
type coveragePolar struct {
	spec                   CoverageSpec
	e0, n0                 float64
	nRad, nRx, nS, rxEvery int
	loss                   []float32
}

func newCoveragePolar(spec CoverageSpec) *coveragePolar {
	if spec.OutPixelM <= 0 {
		spec.OutPixelM = CoverageOutPixelFor(spec.RadiusM)
	}
	p := &coveragePolar{spec: spec}
	p.e0, p.n0 = ToUTM32(spec.Center.Lat, spec.Center.Lon)
	p.nS = int(math.Floor(spec.RadiusM / spec.ResolutionM))
	p.rxEvery = max(1, int(spec.OutPixelM/spec.ResolutionM))
	p.nRx = p.nS / p.rxEvery
	p.nRad = max(8, int(math.Ceil(2*math.Pi*spec.RadiusM/spec.OutPixelM)))
	p.loss = make([]float32, p.nRad*p.nRx)
	return p
}

// innerPerRadial is the inner-loop iterations of one radial: receiver j
// visits its j−1 interior samples.
func (p *coveragePolar) innerPerRadial() int64 {
	var n int64
	for k := 0; k < p.nRx; k++ {
		n += int64((k+1)*p.rxEvery - 1)
	}
	return n
}

// ComputeCoverage predicts the reach of spec over g. It checks ctx between
// radials and returns its error once it is done.
func ComputeCoverage(ctx context.Context, g *CoverageGrid, spec CoverageSpec) (CoverageResult, error) {
	p := newCoveragePolar(spec)
	ground := g.Sample(p.e0, p.n0)
	var iters atomic.Int64
	if p.nRx > 0 {
		workers := max(1, spec.Workers)
		sweep := newRadialSweep(spec.ResolutionM, spec.FreqMHz, spec.K, p.nS, p.rxEvery, spec.Model)
		var next atomic.Int64
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				h := make([]float32, p.nS+1)
				scratch := sweep.scratch()
				for {
					a := int(next.Add(1) - 1)
					if a >= p.nRad || ctx.Err() != nil {
						return
					}
					sin, cos := math.Sincos(2 * math.Pi * float64(a) / float64(p.nRad))
					de, dn := spec.ResolutionM*sin, spec.ResolutionM*cos
					for i := range h {
						h[i] = g.Sample(p.e0+float64(i)*de, p.n0+float64(i)*dn)
					}
					iters.Add(scratch.run(h, spec.TxHeightM, spec.RxHeightM, p.loss[a*p.nRx:(a+1)*p.nRx], nil))
				}
			}()
		}
		wg.Wait()
	} else {
		nan := float32(math.NaN())
		for i := range p.loss {
			p.loss[i] = nan
		}
	}
	if err := ctx.Err(); err != nil {
		return CoverageResult{}, err
	}
	out := p.render()
	out.GroundM = float64(ground)
	out.Iterations = iters.Load()
	return out, nil
}

// render draws the polar loss onto a Web Mercator raster around the disk.
func (p *coveragePolar) render() CoverageResult {
	spec := p.spec
	r := spec.RadiusM
	// The disk's Mercator bbox, from points on its rim, one pixel out.
	x0, y0, x1, y1 := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for a := 0; a < 144; a++ {
		s, c := math.Sincos(2 * math.Pi * float64(a) / 144)
		lat, lon := FromUTM32(p.e0+r*s, p.n0+r*c)
		x, y := lonLatToMercator(lon, lat)
		x0, x1 = math.Min(x0, x), math.Max(x1, x)
		y0, y1 = math.Min(y0, y), math.Max(y1, y)
	}
	px := spec.OutPixelM / math.Cos(spec.Center.Lat*math.Pi/180)
	x0, y1 = x0-px, y1+px
	cell := coverageLatticeCell
	w := int(math.Ceil((x1+px-x0)/px/float64(cell))) * cell
	h := int(math.Ceil((y1-(y0-px))/px/float64(cell))) * cell
	x1, y0 = x0+float64(w)*px, y1-float64(h)*px
	out := CoverageResult{Width: w, Height: h, Loss: make([]uint8, w*h)}
	west, south := mercatorToLonLat(x0, y0)
	east, north := mercatorToLonLat(x1, y1)
	out.Bounds = [2][2]float64{{south, west}, {north, east}}

	lat := newMercLattice(x0, y1, x1-x0, y1-y0, w/cell, h/cell, cell)
	step := float64(p.rxEvery) * spec.ResolutionM
	inDisk, valued := 0, 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			pe, pn := lat.point(x, y)
			v := p.at(pe-p.e0, pn-p.n0, step)
			if v != CoverageNoData {
				valued++
			}
			if math.Hypot(pe-p.e0, pn-p.n0) <= r {
				inDisk++
			}
			out.Loss[y*w+x] = v
		}
	}
	if inDisk > 0 {
		out.CoveredPct = math.Round(1000*float64(min(valued, inDisk))/float64(inDisk)) / 10
	}
	return out
}

// at is the quantised loss at dE, dN metres from the centre: bilinear
// between the two nearest radials and receivers, no data beyond the
// radius or next to a receiver without a value.
func (p *coveragePolar) at(dE, dN, step float64) uint8 {
	rho := math.Hypot(dE, dN)
	if rho > p.spec.RadiusM || p.nRx == 0 {
		return CoverageNoData
	}
	theta := math.Atan2(dE, dN)
	if theta < 0 {
		theta += 2 * math.Pi
	}
	fa := theta / (2 * math.Pi) * float64(p.nRad)
	a0 := int(fa)
	ta := fa - float64(a0)
	a0 %= p.nRad
	a1 := (a0 + 1) % p.nRad
	fb := math.Max(0, rho/step-1)
	b0 := min(int(fb), p.nRx-1)
	b1 := min(b0+1, p.nRx-1)
	tb := math.Min(1, fb-float64(b0))
	l := p.loss
	v := (float64(l[a0*p.nRx+b0])*(1-tb)+float64(l[a0*p.nRx+b1])*tb)*(1-ta) +
		(float64(l[a1*p.nRx+b0])*(1-tb)+float64(l[a1*p.nRx+b1])*tb)*ta
	return QuantizeCoverageLoss(v)
}

// QuantizeCoverageLoss is the raster byte of a loss in dB.
func QuantizeCoverageLoss(db float64) uint8 {
	if math.IsNaN(db) {
		return CoverageNoData
	}
	q := math.Round((db - CoverageLossBaseDb) / CoverageLossStepDb)
	return uint8(math.Max(0, math.Min(CoverageNoData-1, q)))
}

// radialSweep holds the tables of one computation's radials: they depend
// only on the resolution, the wavelength, k, the receiver spacing and the
// model.
type radialSweep struct {
	res, lambda, bulge, k float64
	nS, rxEvery           int
	delta                 bool           // PathDelta: the complete method
	se                    sphericalEarth // its spherical-earth loss
	inv, rs               []float64      // 1/(i·res), 1/√i
	fspl, ratioScale      []float64      // per receiver
}

func newRadialSweep(res, freqMHz, k float64, nS, rxEvery int, model PathModel) *radialSweep {
	s := &radialSweep{res: res, lambda: Wavelength(freqMHz), k: k, nS: nS, rxEvery: rxEvery,
		// EarthBulge(i·res, (j−i)·res, k) = bulge·i·(j−i).
		bulge: res * res / (2 * k * EarthRadiusM), delta: model == PathDelta}
	if s.delta {
		s.se = newSphericalEarth(s.lambda, k)
	}
	s.inv, s.rs = make([]float64, nS+1), make([]float64, nS+1)
	for i := 1; i <= nS; i++ {
		s.inv[i] = 1 / (float64(i) * res)
		s.rs[i] = 1 / math.Sqrt(float64(i))
	}
	nRx := nS / rxEvery
	s.fspl, s.ratioScale = make([]float64, nRx), make([]float64, nRx)
	for r := range s.fspl {
		j := (r + 1) * rxEvery
		s.fspl[r] = FreeSpaceLoss(float64(j)*res, s.lambda)
		s.ratioScale[r] = math.Sqrt(float64(j) / (s.lambda * res))
	}
	return s
}

// sweepScratch is one worker's buffers on a shared radialSweep: the
// radial's heights and, for the complete method, the running sums of the
// heights above the centre's ground, Σg_i and Σi·g_i, and the running
// maxima of the transmitter-side slopes (see run).
type sweepScratch struct {
	*radialSweep
	h, s0, s1, pa, po []float64
}

func (s *radialSweep) scratch() *sweepScratch {
	sc := &sweepScratch{radialSweep: s, h: make([]float64, s.nS+1)}
	if s.delta {
		n := s.nS + 1
		sc.s0, sc.s1, sc.pa, sc.po = make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)
	}
	return sc
}

// run computes one radial: h is the terrain at samples 0..nS from the
// centre; loss (and ratio, when not nil) get one value per receiver, NaN
// from the first gap on. It returns the inner iterations.
//
// Per receiver j it is Link.Bullington over samples 0..j, reorganised so
// the inner loop has no division or square root: with u_i the surface
// plus bulge above the transmitter and Δ the receiver above the
// transmitter, the clearance is c_i = Δ·i/j − u_i, and the three maxima
// (diffraction.go) take precomputed 1/d and 1/√i. With the complete
// method it is Link.DeltaBullington: the same pass takes the obstruction
// maxima of eqs. 89a and 89c, and the smooth surface's sums v1, v2 (eqs.
// 85–86) come from the running sums in O(1). It works in heights above the
// centre's ground, which the fit follows exactly (adding a constant to
// every height adds it to the smooth surface), so the sums stay small.
// The two slopes seen from the transmitter separate into a part of the
// sample alone and a part of the receiver alone: with H_i = h_i − hts −
// Δ·i/j (eq. 89d) and d_i = i·res,
//
//	H_i / d_i            = (h_i − hts)/d_i − Δ/(j·res)
//	(H_i + bulge_i)/d_i  = (h_i − hts)/d_i − β·i/res + β·j/res − Δ/(j·res)
//
// so their maxima over the interior (eq. 89b, and Bullington's slope from
// the transmitter) are running maxima along the radial, read in O(1) per
// receiver; the loop keeps the two receiver-side maxima and the ratio.
func (s *radialSweep) run(h []float32, txH, rxH float64, loss, ratio []float32) int64 {
	return s.scratch().run(h, txH, rxH, loss, ratio)
}

func (s *sweepScratch) run(h []float32, txH, rxH float64, loss, ratio []float32) int64 {
	hb := s.h[:len(h)]
	gap := len(h)
	for i, v := range h {
		hb[i] = float64(v)
		if gap == len(h) && math.IsNaN(hb[i]) {
			gap = i
		}
	}
	nan := float32(math.NaN())
	hts := hb[0] + txH
	if s.delta && gap > 0 {
		var a0, a1 float64
		pa, po := math.Inf(-1), math.Inf(-1)
		for i := 0; i < gap; i++ {
			g := hb[i] - hb[0]
			a0 += g
			a1 += float64(i) * g
			s.s0[i], s.s1[i] = a0, a1
			if i > 0 {
				t := (hb[i] - hts) * s.inv[i]
				po = math.Max(po, t)
				pa = math.Max(pa, t-s.bulge*float64(i)/s.res)
			}
			s.pa[i], s.po[i] = pa, po
		}
	}
	var iters int64
	for r := range loss {
		j := (r + 1) * s.rxEvery
		if j >= gap {
			loss[r] = nan
			if ratio != nil {
				ratio[r] = nan
			}
			continue
		}
		delta := hb[j] + rxH - hts
		var a, b, rmin float64
		fit := noObstruction()
		if !s.delta {
			a, b, rmin = s.bullingtonPass(hb, hts, delta, j)
		} else {
			b, rmin, fit.hobs, fit.aobr = s.deltaPass(hb, hts, delta, j)
			a = math.Inf(-1)
			if j > 1 {
				lin := delta / float64(j) / s.res
				a = s.pa[j-1] + s.bulge*float64(j)/s.res - lin
				fit.aobt = s.po[j-1] - lin
			}
		}
		iters += int64(max(0, j-1))
		worst := rmin * s.ratioScale[r]
		dM := float64(j) * s.res
		bl := bullingtonFrom(worst, a, b, dM, s.lambda)
		l := bl.Loss
		if s.delta {
			// Eqs. 85–86 over g_i = h_i − h_0, with d_i = i·res:
			// v1 = res·(2Σg − g_0 − g_j), v2 = res²·(3Σ_1^j i·g_i −
			// Σ_1^j g_i + 3Σ_0^{j−1} i·g_i + Σ_0^{j−1} g_i); g_0 = 0.
			gj := hb[j] - hb[0]
			fit.v1 = s.res * (2*s.s0[j] - gj)
			fit.v2 = s.res * s.res * (3*s.s1[j] - s.s0[j] + 3*s.s1[j-1] + s.s0[j-1])
			l = deltaFrom(bl, fit, j, s.res, 0, gj, txH, gj+rxH, s.k, s.lambda, &s.se).Loss
		}
		loss[r] = float32(s.fspl[r] + l)
		if ratio != nil {
			ratio[r] = float32(worst)
		}
	}
	return iters
}

// bullingtonPass is Bullington's inner loop for receiver j: the two slope
// maxima and the worst clearance ratio over the interior samples, before
// ratioScale. Each pass is its own function so the compiler lays out the
// two loops independently.
func (s *sweepScratch) bullingtonPass(hb []float64, hts, delta float64, j int) (a, b, rmin float64) {
	invj := 1 / float64(j)
	a, b, rmin = math.Inf(-1), math.Inf(-1), math.Inf(1)
	inv, rs, c := s.inv, s.rs, s.bulge
	for i := 1; i < j; i++ {
		fi := float64(i)
		cl := delta*fi*invj - (hb[i] - hts + c*fi*float64(j-i))
		if t := -cl * inv[i]; t > a {
			a = t
		}
		if t := -cl * inv[j-i]; t > b {
			b = t
		}
		if t := cl * rs[i] * rs[j-i]; t < rmin {
			rmin = t
		}
	}
	return a, b, rmin
}

// deltaPass is the complete method's inner loop for receiver j: the
// receiver-side slope maximum and the worst ratio as in bullingtonPass,
// and the obstruction terms hobs and α_obr (eqs. 89a, 89c). The
// transmitter-side maxima come from the running maxima (see run).
func (s *sweepScratch) deltaPass(hb []float64, hts, delta float64, j int) (b, rmin, hobs, aobr float64) {
	invj := 1 / float64(j)
	b, rmin, hobs, aobr = math.Inf(-1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	inv, rs, c := s.inv, s.rs, s.bulge
	for i := 1; i < j; i++ {
		fi := float64(i)
		// hh is H_i of eq. 89d; hh plus the bulge is minus the clearance.
		hh := hb[i] - hts - delta*fi*invj
		u := hh + c*fi*float64(j-i)
		if t := u * inv[j-i]; t > b {
			b = t
		}
		if t := -u * rs[i] * rs[j-i]; t < rmin {
			rmin = t
		}
		if hh > hobs {
			hobs = hh
		}
		if t := hh * inv[j-i]; t > aobr {
			aobr = t
		}
	}
	return b, rmin, hobs, aobr
}
