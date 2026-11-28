package buildings

import (
	"errors"
	"math"

	"github.com/meshcore-analyzer/dhm"
)

// Class is one sample's classification. The values are single bytes, so
// a profile's classes are one byte per sample.
type Class byte

const (
	// Unclassified: not elevated, or no usable footprints for its segment
	// (not fetched, failed, truncated, too many skipped, over the work
	// cap), or off the track.
	Unclassified Class = 0
	// Building: at least MinBuildingFraction of the window lies inside a
	// footprint.
	Building Class = 'b'
	// Vegetation: elevated, but not a building. That is trees in practice,
	// and also masts, cranes and anything else the footprints do not hold.
	Vegetation Class = 'v'
)

// Samples is a terrain profile along a corridor: evenly spaced positions
// in EPSG:25832 metres (dhm.Plan's E and N), the terrain and surface
// elevations there (NaN where there is none) and the step between them.
type Samples struct {
	E, N     []float64
	DTM, DSM []float32
	StepM    float64
}

// Options are the classification's thresholds (#730 coordinator
// decisions). They are hard-coded defaults for now; the customizer
// exposure comes later (AGENTS.md rule 8).
type Options struct {
	// ThresholdM: a sample is elevated when DSM − DTM is above it.
	ThresholdM float64
	// MinBuildingFraction: the share of the window inside a footprint
	// from which an elevated sample is a building, 0 < f ≤ 1.
	MinBuildingFraction float64
}

// DefaultOptions are T = 2.5 m and f = 0.25.
func DefaultOptions() Options { return Options{ThresholdM: 2.5, MinBuildingFraction: 0.25} }

// ErrBadInput is returned for samples, pages or options that do not fit
// the corridor.
var ErrBadInput = errors.New("buildings: samples, pages or options out of shape")

const (
	windowPoints = len(windowOffsets) * len(windowOffsets)

	// bucketM is the along-track length of one index bucket. A window
	// point tests only the footprints of its bucket.
	bucketM = 25.0

	// maxIncidences bounds one segment's index: the sum over its
	// footprints' polygons of vertices × buckets spanned. A window point's
	// work is at most its bucket's share of it, so this bounds the
	// classification of a broken or hostile answer. A real full page
	// (1,000 footprints of ~11 vertices over ~2 buckets) is ~22,000.
	maxIncidences = 250000
)

func (o Options) valid() bool {
	return o.ThresholdM >= 0 && !math.IsInf(o.ThresholdM, 1) &&
		o.MinBuildingFraction > 0 && o.MinBuildingFraction <= 1
}

func (s Samples) valid() bool {
	n := len(s.E)
	return len(s.N) == n && len(s.DTM) == n && len(s.DSM) == n &&
		s.StepM > 0 && !math.IsInf(s.StepM, 1)
}

// Elevated reports whether sample i's surface stands more than
// thresholdM above the terrain, the samples Needed and Classify look at;
// NaN in either model is not elevated.
func (s Samples) Elevated(i int, thresholdM float64) bool {
	return float64(s.DSM[i])-float64(s.DTM[i]) > thresholdM
}

// Needed reports, per segment of c, whether any of its samples is
// elevated, i.e. whether its footprints must be fetched. A path where
// nothing stands above the terrain costs no request.
func (c Corridor) Needed(s Samples, o Options) ([]bool, error) {
	if c.segs == 0 || !s.valid() || !o.valid() {
		return nil, ErrBadInput
	}
	need := make([]bool, c.segs)
	for i := range s.E {
		if !s.Elevated(i, o.ThresholdM) {
			continue
		}
		if k := c.segmentOf(c.track(s.E[i], s.N[i])); k >= 0 {
			need[k] = true
		}
	}
	return need, nil
}

// Classify classifies every sample of s along c, with pages[k] the
// footprints of segment k (nil when it was not fetched or failed). A
// sample is Unclassified unless it is elevated and its segment's page is
// usable; then it is a Building when at least MinBuildingFraction of its
// window lies inside a footprint, and Vegetation otherwise.
//
// The window is a 5 × 5 grid aligned with the track, ± half the step,
// clamped to MinHalfWindowM..MaxHalfWindowM. Each segment's footprints are
// indexed in along-track buckets of bucketM, built only for segments with
// an elevated sample, so the cost is O(samples + vertices) rather than
// O(samples × footprints) (BenchmarkClassify80km). The pages are only
// read.
func Classify(c Corridor, s Samples, pages []*Page, o Options) ([]Class, error) {
	if c.segs == 0 || len(pages) != c.segs || !s.valid() || !o.valid() {
		return nil, ErrBadInput
	}
	h := math.Max(MinHalfWindowM, math.Min(MaxHalfWindowM, s.StepM/2))
	need := int(math.Ceil(o.MinBuildingFraction*float64(windowPoints) - 1e-9))
	idx := make([]*segmentIndex, c.segs)
	built := make([]bool, c.segs)
	out := make([]Class, len(s.E))
	for i := range out {
		if !s.Elevated(i, o.ThresholdM) {
			continue
		}
		sv, tv := c.track(s.E[i], s.N[i])
		k := c.segmentOf(sv, tv)
		if k < 0 {
			continue
		}
		if !built[k] {
			built[k] = true
			if p := pages[k]; p != nil && p.usable() {
				idx[k] = c.index(k, p, h)
			}
		}
		if idx[k] == nil {
			continue
		}
		out[i] = Vegetation
		if idx[k].insideCount(c, sv, tv, h, need) >= need {
			out[i] = Building
		}
	}
	return out, nil
}

// segmentIndex is one segment's footprints, one entry per polygon, with
// their track-frame bounding boxes, in along-track buckets.
type segmentIndex struct {
	from    float64 // track distance where bucket 0 starts
	buckets [][]int32
	boxes   []polygonBox
}

type polygonBox struct {
	poly           dhm.MultiPolygon // one polygon of a footprint
	s0, s1, t0, t1 float64
}

// boxPadM pads a box against the rounding of the frame change, so a point
// on a polygon's edge is still tested.
const boxPadM = 1e-6

// index builds segment k's index for a half-window of h, or returns nil
// when the page would exceed maxIncidences. Only the polygons that can
// reach a window of the segment's samples are kept.
func (c Corridor) index(k int, p *Page, h float64) *segmentIndex {
	s0, s1 := c.span(k)
	reach := h + trackSlackM
	ix := &segmentIndex{from: s0 - reach}
	nb := int(math.Ceil((s1 + reach - ix.from) / bucketM))
	ix.buckets = make([][]int32, nb)
	incidences := 0
	for _, fp := range p.Footprints {
		for _, poly := range fp {
			b := polygonBox{poly: dhm.MultiPolygon{poly},
				s0: math.Inf(1), s1: math.Inf(-1), t0: math.Inf(1), t1: math.Inf(-1)}
			// The outer ring bounds the polygon; its holes lie inside it.
			for _, v := range poly[0] {
				sv, tv := c.track(v[0], v[1])
				b.s0, b.s1 = math.Min(b.s0, sv), math.Max(b.s1, sv)
				b.t0, b.t1 = math.Min(b.t0, tv), math.Max(b.t1, tv)
			}
			b.s0, b.s1, b.t0, b.t1 = b.s0-boxPadM, b.s1+boxPadM, b.t0-boxPadM, b.t1+boxPadM
			if b.t1 < -reach || b.t0 > reach || b.s1 < s0-reach || b.s0 > s1+reach {
				continue
			}
			first, last := ix.bucket(b.s0), ix.bucket(b.s1)
			vertices := 0
			for _, ring := range poly {
				vertices += len(ring)
			}
			if incidences += vertices * (last - first + 1); incidences > maxIncidences {
				return nil
			}
			id := int32(len(ix.boxes))
			ix.boxes = append(ix.boxes, b)
			for j := first; j <= last; j++ {
				ix.buckets[j] = append(ix.buckets[j], id)
			}
		}
	}
	return ix
}

// bucket is the bucket holding track distance s, clamped to the index.
func (ix *segmentIndex) bucket(s float64) int {
	return max(0, min(int(math.Floor((s-ix.from)/bucketM)), len(ix.buckets)-1))
}

// insideCount counts the window points around the sample at (sv, tv)
// that lie inside a footprint, stopping once the answer is settled: at
// need, or when the points left cannot reach it.
func (ix *segmentIndex) insideCount(c Corridor, sv, tv, h float64, need int) int {
	inside, left := 0, windowPoints
	for _, a := range windowOffsets {
		ps := sv + a*h
		bucket := ix.buckets[ix.bucket(ps)]
		for _, b := range windowOffsets {
			if inside >= need || inside+left < need {
				return inside
			}
			left--
			pt := tv + b*h
			e, n := c.point(ps, pt)
			for _, id := range bucket {
				bx := &ix.boxes[id]
				if ps < bx.s0 || ps > bx.s1 || pt < bx.t0 || pt > bx.t1 {
					continue
				}
				if in, _ := dhm.Locate(bx.poly, e, n); in {
					inside++
					break
				}
			}
		}
	}
	return inside
}
