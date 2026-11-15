package dhm

import (
	"context"
	"errors"
	"math"
	"sort"

	"golang.org/x/sync/errgroup"
)

// #789 M1 — the surface above the ground around one point: how far the
// surface model (roofs, trees, towers) stands above the terrain model in a
// small window around a node, as the evidence for an estimated antenna
// height.
//
// The window is fetched at DHM's native resolution, not sampled from a
// profile's coarse chunk raster: the advertised position is often a few
// metres off, and a single resampled pixel can then be off by more than
// 10 m (#789 research §3). It is one GetCoverage per model, with a bbox a
// few tens of metres wide.

const (
	// SurfacePixelM is the window's pixel size: DHM's native 0.4 m grid.
	SurfacePixelM = 0.4
	// MaxSurfaceRadiusM bounds the window radius, so the raster stays far
	// below MaxRasterSize (252 px per side at the bound).
	MaxSurfaceRadiusM = 50
)

// ErrNotCovered is returned, without any upstream call, for a point DHM
// does not cover (Covers), or whose window would leave RasterExtent.
var ErrNotCovered = errors.New("dhm: point not covered")

// Surface is DSM − DTM over the cells of a window whose centre is within
// the radius, ignoring cells where either model has no data. A negative
// difference (surface noise just below the terrain) counts as 0. All
// heights are zero when no cell has data.
type Surface struct {
	MaxM float64 // the highest DSM − DTM
	P90M float64 // its 90th percentile (nearest rank)
	// SpreadM is P90 − P10: about 0 on a flat roof or open ground, large
	// under trees or across a roof edge.
	SpreadM float64
	// Coverage is the share of the window's cells with data in both
	// models, 0 to 1.
	Coverage float64
	// Cells is the number of cells in the window.
	Cells int
}

// surfaceWindow is the raster for the window of radius r around (e, n),
// one pixel of margin included, at SurfacePixelM. ok is false when the
// window leaves RasterExtent.
func surfaceWindow(e, n, r float64) (RasterRequest, bool) {
	half := r + SurfacePixelM
	px := int(math.Ceil(2 * half / SurfacePixelM))
	side := float64(px) * SurfacePixelM
	bbox := [4]float64{e - side/2, n - side/2, e + side/2, n + side/2}
	ext := RasterExtent
	if !(bbox[0] >= ext[0] && bbox[1] >= ext[1] && bbox[2] <= ext[2] && bbox[3] <= ext[3]) {
		return RasterRequest{}, false
	}
	return RasterRequest{BBox: bbox, Width: px, Height: px}, true
}

// SurfaceAt measures the surface above the ground within radiusM metres of
// at: one DSM and one DTM raster from src, fetched concurrently (src is the
// shared Client, which bounds the upstream concurrency). The caller
// coalesces and admits calls (cmd/server); perCall bounds each raster
// (0: no own limit). A point DHM does not cover is ErrNotCovered with no
// call; a radius outside (0, MaxSurfaceRadiusM] is ErrBadRequest.
func SurfaceAt(ctx context.Context, src ElevationSource, at LatLon, radiusM float64) (Surface, error) {
	if !(radiusM > 0 && radiusM <= MaxSurfaceRadiusM) {
		return Surface{}, ErrBadRequest
	}
	e, n := ToUTM32(at.Lat, at.Lon)
	if !Covers(e, n) {
		return Surface{}, ErrNotCovered
	}
	r, ok := surfaceWindow(e, n, radiusM)
	if !ok {
		return Surface{}, ErrNotCovered
	}
	var dsm, dtm Grid
	g, gctx := errgroup.WithContext(ctx)
	for _, m := range [...]struct {
		model Model
		out   *Grid
	}{{ModelSurface, &dsm}, {ModelTerrain, &dtm}} {
		m := m
		g.Go(func() (err error) {
			req := r
			req.Model = m.model
			*m.out, err = src.FetchRaster(gctx, req)
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return Surface{}, err
	}
	return surfaceStats(&dsm, &dtm, r, e, n, radiusM), nil
}

// surfaceStats is DSM − DTM at the pixel centres of r within radius of
// (e, n). It is O(pixels): one pass, then a sort of the cells with data.
func surfaceStats(dsm, dtm *Grid, r RasterRequest, e, n, radius float64) Surface {
	pxE := (r.BBox[2] - r.BBox[0]) / float64(r.Width)
	pxN := (r.BBox[3] - r.BBox[1]) / float64(r.Height)
	vals := make([]float64, 0, r.Width*r.Height)
	cells := 0
	r2 := radius * radius
	for row := 0; row < r.Height; row++ {
		cn := r.BBox[3] - (float64(row)+0.5)*pxN
		dn := cn - n
		for col := 0; col < r.Width; col++ {
			ce := r.BBox[0] + (float64(col)+0.5)*pxE
			de := ce - e
			if de*de+dn*dn > r2 {
				continue
			}
			cells++
			s, ok1 := dsm.Sample(ce, cn)
			t, ok2 := dtm.Sample(ce, cn)
			if !ok1 || !ok2 {
				continue
			}
			vals = append(vals, math.Max(0, s-t))
		}
	}
	out := Surface{Cells: cells}
	if len(vals) == 0 {
		return out
	}
	sort.Float64s(vals)
	out.MaxM = vals[len(vals)-1]
	out.P90M = nearestRank(vals, 0.9)
	out.SpreadM = out.P90M - nearestRank(vals, 0.1)
	out.Coverage = float64(len(vals)) / float64(cells)
	return out
}

// nearestRank is the q-quantile of sorted, by the nearest-rank method.
func nearestRank(sorted []float64, q float64) float64 {
	i := int(math.Ceil(q*float64(len(sorted)))) - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}
