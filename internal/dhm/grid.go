package dhm

import "math"

// #681 M2 — a decoded elevation raster and bilinear sampling on it.

// Grid is one decoded elevation raster in EPSG:25832. It is what an
// ElevationSource returns, so the wire format stays the source's business.
type Grid struct {
	Width, Height int
	// Values holds Width × Height elevations in metres, row-major, the
	// northernmost row first.
	Values []float32
	// NoData marks cells without elevation when HasNoData is set. NaN and
	// ±Inf cells are always treated as nodata.
	NoData    float32
	HasNoData bool
	Georef    Georef
}

// Georef places a north-up grid: the outer corner of pixel (0, 0) and the
// pixel size, all in EPSG:25832 metres. Pixel (col, row) covers
// [OriginE + col·PixelE, OriginE + (col+1)·PixelE] east and
// [OriginN − (row+1)·PixelN, OriginN − row·PixelN] north; its value sits at
// the pixel centre.
type Georef struct {
	OriginE, OriginN float64 // west edge, north edge
	PixelE, PixelN   float64 // both > 0
}

func (g Georef) valid() bool {
	for _, v := range []float64{g.OriginE, g.OriginN, g.PixelE, g.PixelN} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return g.PixelE > 0 && g.PixelN > 0
}

// Extent is the grid's outer bbox in EPSG:25832: minE, minN, maxE, maxN.
func (g *Grid) Extent() [4]float64 {
	r := g.Georef
	return [4]float64{r.OriginE, r.OriginN - float64(g.Height)*r.PixelN, r.OriginE + float64(g.Width)*r.PixelE, r.OriginN}
}

// covers reports whether the grid spans bbox to within one pixel on every
// edge, which allows for a server that snaps its output grid.
func (g *Grid) covers(bbox [4]float64) bool {
	x, r := g.Extent(), g.Georef
	return x[0] <= bbox[0]+r.PixelE && x[2] >= bbox[2]-r.PixelE &&
		x[1] <= bbox[1]+r.PixelN && x[3] >= bbox[3]-r.PixelN
}

// cell returns the value at (col, row) and whether it holds elevation.
func (g *Grid) cell(col, row int) (float64, bool) {
	v := g.Values[row*g.Width+col]
	if g.HasNoData && v == g.NoData {
		return 0, false
	}
	f := float64(v)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// Sample interpolates the elevation at easting e, northing n bilinearly
// between the four surrounding pixel centres. ok is false outside the
// grid's extent, and when any pixel that carries weight in the result is
// nodata: a value next to a gap is unknown, not smeared into it. Between
// the outermost pixel centres and the extent edge the edge pixels are held
// constant.
func (g *Grid) Sample(e, n float64) (float64, bool) {
	if g.Width < 1 || g.Height < 1 || len(g.Values) != g.Width*g.Height {
		return 0, false
	}
	x := g.Extent()
	if !(e >= x[0] && e <= x[2] && n >= x[1] && n <= x[3]) {
		return 0, false
	}
	// Continuous pixel coordinates with pixel centres on the integers.
	fx := clamp((e-g.Georef.OriginE)/g.Georef.PixelE-0.5, 0, float64(g.Width-1))
	fy := clamp((g.Georef.OriginN-n)/g.Georef.PixelN-0.5, 0, float64(g.Height-1))
	c0, r0 := int(fx), int(fy)
	c1, r1 := min(c0+1, g.Width-1), min(r0+1, g.Height-1)
	tx, ty := fx-float64(c0), fy-float64(r0)

	var sum float64
	for _, p := range [4]struct {
		col, row int
		w        float64
	}{
		{c0, r0, (1 - tx) * (1 - ty)},
		{c1, r0, tx * (1 - ty)},
		{c0, r1, (1 - tx) * ty},
		{c1, r1, tx * ty},
	} {
		if p.w == 0 {
			continue
		}
		v, ok := g.cell(p.col, p.row)
		if !ok {
			return 0, false
		}
		sum += p.w * v
	}
	return sum, true
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }
