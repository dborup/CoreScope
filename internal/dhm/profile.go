package dhm

import (
	"context"
	"math"
	"time"

	"golang.org/x/sync/errgroup"
)

// #681 M3 — the terrain profile planner: where to sample along a path,
// and which rasters to fetch for those samples (plan §1.3, §6, §7).
//
// WCS 1.0 only serves axis-aligned rasters, so one raster around a
// diagonal path costs O(L²) pixels. Splitting the path into chunks of
// about ChunkM keeps the total near O(L), at most MaxCallsPerModel
// rasters per elevation model. Samples outside the DHM raster extent are never
// requested: a path that leaves Denmark gets a gap there, and a path with
// no sample inside makes no upstream call at all.

const (
	MinStepM = 10 // finer than any Fresnel radius needs (plan §1.3)
	MaxStepM = 25
	// PlanSamples is the path length in samples at which the step starts
	// to grow from MinStepM to MaxStepM.
	PlanSamples      = 1024
	MaxCallsPerModel = 16
	minPlanSamples   = 3 // a verdict needs an interior sample
)

// LatLon is a WGS84 position in degrees.
type LatLon struct{ Lat, Lon float64 }

// GroundDistance is the distance in metres between a and b on the GRS80
// ellipsoid, with the meridian and prime-vertical radii taken at the
// mid-latitude. For the ≤ 80 km paths the profile allows that is within a
// metre of the geodesic; a spherical haversine is 0.3 % short at Danish
// latitudes. Far longer distances are only compared against the limit.
func GroundDistance(a, b LatLon) float64 {
	const (
		rad = math.Pi / 180
		aa  = 6378137.0
		e2  = 0.00669438002290 // GRS80
	)
	phi := (a.Lat + b.Lat) / 2 * rad
	w := 1 - e2*math.Pow(math.Sin(phi), 2)
	nRad := aa / math.Sqrt(w)                  // prime vertical
	mRad := aa * (1 - e2) / (w * math.Sqrt(w)) // meridian
	return math.Hypot((b.Lat-a.Lat)*rad*mRad, (b.Lon-a.Lon)*rad*nRad*math.Cos(phi))
}

// PlanOptions are the operator's limits (config map.dhm.profile).
type PlanOptions struct {
	MaxSamples int     // samples computed along the path
	ChunkM     float64 // path length per raster; 0 = one raster per model
}

// Plan is the sampling of one path: evenly spaced samples from A to B in
// EPSG:25832, and the rasters that cover the ones inside the DHM extent.
type Plan struct {
	DistanceM float64
	StepM     float64
	E, N      []float64
	inside    []bool
	chunks    []planChunk
}

// planChunk is one raster and the samples [first, last] it is fetched
// for. Chunks never share a sample, so their fetches write disjoint
// ranges of the result.
type planChunk struct {
	first, last int
	raster      RasterRequest // Model set per fetch
}

// PlanProfile samples the path every clamp(L/PlanSamples, MinStepM,
// MaxStepM) metres, capped at MaxSamples samples, and plans its rasters.
func PlanProfile(a, b LatLon, o PlanOptions) Plan {
	d := GroundDistance(a, b)
	step := math.Max(MinStepM, math.Min(MaxStepM, d/PlanSamples))
	n := int(math.Round(d/step)) + 1
	n = max(minPlanSamples, min(n, o.MaxSamples))
	p := Plan{DistanceM: d, StepM: d / float64(n-1),
		E: make([]float64, n), N: make([]float64, n), inside: make([]bool, n)}

	// For the ≤ 80 km paths allowed, a straight line in UTM 32 is the
	// geodesic to well under a metre, and ETRS89 equals WGS84 to ~0.9 m,
	// against 10–25 m steps and 20–60 m Fresnel radii.
	ea, na := ToUTM32(a.Lat, a.Lon)
	eb, nb := ToUTM32(b.Lat, b.Lon)
	ext := RasterExtent
	for i := range p.E {
		t := float64(i) / float64(n-1)
		e, nn := ea+(eb-ea)*t, na+(nb-na)*t
		p.E[i], p.N[i] = e, nn
		// Written so NaN (a projection far outside zone 32) is outside.
		p.inside[i] = e >= ext[0] && e <= ext[2] && nn >= ext[1] && nn <= ext[3]
	}

	// Chunks of about ChunkM: rounding, so a path a few metres over a
	// multiple of ChunkM does not cost one more raster.
	m := 1
	if o.ChunkM > 0 {
		m = int(math.Round(d / o.ChunkM))
	}
	// Keep every raster edge, with its margins, inside MaxRasterSpanM.
	for d/float64(m) > 0.99*MaxRasterSpanM {
		m++
	}
	m = max(1, min(m, MaxCallsPerModel, n))
	for j := 0; j < m; j++ {
		if c, ok := p.chunk(j*n/m, (j+1)*n/m-1); ok {
			p.chunks = append(p.chunks, c)
		}
	}
	return p
}

// chunk plans the raster for samples [first, last]: their bbox plus one
// pixel of margin for bilinear sampling, clipped to the raster extent, at
// the sample step or coarser if the edge would exceed MaxRasterSize. ok is
// false when no sample is inside the extent.
func (p *Plan) chunk(first, last int) (planChunk, bool) {
	minE, minN := math.Inf(1), math.Inf(1)
	maxE, maxN := math.Inf(-1), math.Inf(-1)
	for i := first; i <= last; i++ {
		if !p.inside[i] {
			continue
		}
		minE, maxE = math.Min(minE, p.E[i]), math.Max(maxE, p.E[i])
		minN, maxN = math.Min(minN, p.N[i]), math.Max(maxN, p.N[i])
	}
	if minE > maxE {
		return planChunk{}, false
	}
	span := math.Max(maxE-minE, maxN-minN)
	// MaxRasterSize-3 leaves room for the two margin pixels plus rounding.
	px := math.Max(1, math.Max(p.StepM, span/(MaxRasterSize-3)))
	ext := RasterExtent
	bbox := [4]float64{
		math.Max(minE-px, ext[0]), math.Max(minN-px, ext[1]),
		math.Min(maxE+px, ext[2]), math.Min(maxN+px, ext[3]),
	}
	return planChunk{first: first, last: last, raster: RasterRequest{
		BBox:   bbox,
		Width:  max(2, int(math.Ceil((bbox[2]-bbox[0])/px))),
		Height: max(2, int(math.Ceil((bbox[3]-bbox[1])/px))),
	}}, true
}

// Calls is the number of rasters fetched per elevation model.
func (p *Plan) Calls() int { return len(p.chunks) }

// Covered is the number of samples inside the DHM raster extent.
func (p *Plan) Covered() int {
	n := 0
	for _, in := range p.inside {
		if in {
			n++
		}
	}
	return n
}

// Fetch fetches model m's rasters concurrently (the Client's semaphore
// bounds the upstream concurrency) and returns the elevation at every
// sample, NaN where there is none: outside the extent, or nodata. Each
// raster gets perCall (0: no own limit); the first failure cancels the
// others and is returned. A raster is decoded, sampled and dropped; none
// is kept.
func (p *Plan) Fetch(ctx context.Context, src ElevationSource, m Model, perCall time.Duration) ([]float32, error) {
	out := make([]float32, len(p.E))
	nan := float32(math.NaN())
	for i := range out {
		out[i] = nan
	}
	g, gctx := errgroup.WithContext(ctx)
	for _, c := range p.chunks {
		c := c
		g.Go(func() error {
			cctx, cancel := gctx, context.CancelFunc(func() {})
			if perCall > 0 {
				cctx, cancel = context.WithTimeout(gctx, perCall)
			}
			defer cancel()
			r := c.raster
			r.Model = m
			grid, err := src.FetchRaster(cctx, r)
			if err != nil {
				return err
			}
			for i := c.first; i <= c.last; i++ {
				if !p.inside[i] {
					continue
				}
				if v, ok := grid.Sample(p.E[i], p.N[i]); ok {
					out[i] = float32(v)
				}
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}
