package buildings

import (
	"errors"
	"math"
	"strconv"

	"github.com/meshcore-analyzer/dhm"
)

// ErrBadCorridor is the single error for a corridor outside the
// allow-listed shape. It never carries caller input.
var ErrBadCorridor = errors.New("buildings: corridor outside the allowed shape")

// Corridor is the straight EPSG:25832 line of one terrain profile, from A
// to B, with the radius and segment length its queries use. Only
// NewCorridor makes one, so every Corridor is valid; the zero Corridor has
// no segments.
type Corridor struct {
	a, b     [2]float64 // rounded to 0.1 m
	radiusM  int
	segmentM int
	length   float64
	u        [2]float64 // unit vector from a to b
	segs     int
}

// NewCorridor validates a profile's ends a and b (easting, northing in
// EPSG:25832 metres): both inside dhm.RasterExtent, at most MaxCorridorM
// apart and not equal once rounded to 0.1 m, with a radius from
// AllowedRadiiM and a segment length of MinSegmentM..MaxSegmentM.
// Anything else, NaN and ±Inf included, is ErrBadCorridor.
func NewCorridor(a, b [2]float64, radiusM, segmentM int) (Corridor, error) {
	if !radiusAllowed(radiusM) || segmentM < MinSegmentM || segmentM > MaxSegmentM {
		return Corridor{}, ErrBadCorridor
	}
	a, b = roundDM(a), roundDM(b)
	if !inExtent(a) || !inExtent(b) {
		return Corridor{}, ErrBadCorridor
	}
	l := math.Hypot(b[0]-a[0], b[1]-a[1])
	if !(l > 0 && l <= MaxCorridorM) {
		return Corridor{}, ErrBadCorridor
	}
	return Corridor{
		a: a, b: b, radiusM: radiusM, segmentM: segmentM, length: l,
		u:    [2]float64{(b[0] - a[0]) / l, (b[1] - a[1]) / l},
		segs: int(math.Ceil(l / float64(segmentM))),
	}, nil
}

func radiusAllowed(r int) bool {
	for _, ok := range AllowedRadiiM {
		if r == ok {
			return true
		}
	}
	return false
}

// roundDM rounds a position to 0.1 m, the precision of the query.
func roundDM(p [2]float64) [2]float64 {
	return [2]float64{math.Round(p[0]*10) / 10, math.Round(p[1]*10) / 10}
}

// inExtent: inside dhm.RasterExtent, edges included. NaN is outside.
func inExtent(p [2]float64) bool {
	x := dhm.RasterExtent
	return p[0] >= x[0] && p[0] <= x[2] && p[1] >= x[1] && p[1] <= x[3]
}

// LengthM is the corridor's length in EPSG:25832 metres.
func (c Corridor) LengthM() float64 { return c.length }

// Segment is one query: the buildings within RadiusM of the straight line
// Line, whose ends are E1, N1, E2, N2 in whole EPSG:25832 decimetres. The
// URL is written from these integers alone, and FetchSegment checks Valid
// first, so a Segment built by hand cannot widen the query either.
type Segment struct {
	Line    [4]int64
	RadiusM int
}

// lineWindowDM is where a query line's ends may lie, in decimetres:
// dhm.RasterExtent grown by MaxRadiusM plus a metre for rounding, since a
// segment's line extends its span by the radius past the corridor's ends.
var lineWindowDM = func() [4]int64 {
	x, g := dhm.RasterExtent, float64(MaxRadiusM+1)
	return [4]int64{int64((x[0] - g) * 10), int64((x[1] - g) * 10), int64((x[2] + g) * 10), int64((x[3] + g) * 10)}
}()

// maxLineDM is the longest query line in decimetres: a whole segment, the
// radius at both ends, and a metre for rounding.
const maxLineDM = (MaxSegmentM + 2*MaxRadiusM + 1) * 10

// Valid reports whether s has an allowed radius and a line of positive
// length, at most maxLineDM, with both ends in lineWindowDM.
func (s Segment) Valid() bool {
	if !radiusAllowed(s.RadiusM) {
		return false
	}
	w := lineWindowDM
	for i := 0; i < 4; i += 2 {
		if s.Line[i] < w[0] || s.Line[i] > w[2] || s.Line[i+1] < w[1] || s.Line[i+1] > w[3] {
			return false
		}
	}
	de, dn := s.Line[2]-s.Line[0], s.Line[3]-s.Line[1]
	l2 := de*de + dn*dn // the window bounds every term: no overflow
	return l2 > 0 && l2 <= maxLineDM*maxLineDM
}

// segmentKey identifies a segment's query regardless of its direction, so
// a path and its reverse share cache entries.
type segmentKey struct {
	e1, n1, e2, n2 int64
	radiusM        int
}

func (s Segment) key() segmentKey {
	l := s.Line
	if l[2] < l[0] || l[2] == l[0] && l[3] < l[1] {
		l = [4]int64{l[2], l[3], l[0], l[1]}
	}
	return segmentKey{l[0], l[1], l[2], l[3], s.RadiusM}
}

func (k segmentKey) String() string {
	return strconv.FormatInt(k.e1, 10) + "," + strconv.FormatInt(k.n1, 10) + "," +
		strconv.FormatInt(k.e2, 10) + "," + strconv.FormatInt(k.n2, 10) + "/" + strconv.Itoa(k.radiusM)
}

// Segments cuts the corridor into the fewest equal spans of at most its
// segment length, from A. Segment k holds the samples whose distance along
// the track lies in span(k); its query line is that span grown by the
// radius at both ends, along the track, so the windows of the span's
// first and last samples are covered too (TestRadiusCoversTheWindow).
func (c Corridor) Segments() []Segment {
	out := make([]Segment, c.segs)
	r := float64(c.radiusM)
	for k := range out {
		s0, s1 := c.span(k)
		e1, n1 := c.point(s0-r, 0)
		e2, n2 := c.point(s1+r, 0)
		out[k] = Segment{Line: [4]int64{dm(e1), dm(n1), dm(e2), dm(n2)}, RadiusM: c.radiusM}
	}
	return out
}

func dm(v float64) int64 { return int64(math.Round(v * 10)) }

// span is segment k's distance range along the track, [s0, s1) from A;
// the last span ends at B.
func (c Corridor) span(k int) (s0, s1 float64) {
	s0 = c.length * float64(k) / float64(c.segs)
	s1 = c.length
	if k+1 < c.segs {
		s1 = c.length * float64(k+1) / float64(c.segs)
	}
	return s0, s1
}

// track is the point (e, n) in the corridor's frame: s along the track
// from A, t across it, positive to the left of A→B.
func (c Corridor) track(e, n float64) (s, t float64) {
	de, dn := e-c.a[0], n-c.a[1]
	return de*c.u[0] + dn*c.u[1], dn*c.u[0] - de*c.u[1]
}

// point is track's inverse.
func (c Corridor) point(s, t float64) (e, n float64) {
	return c.a[0] + c.u[0]*s - c.u[1]*t, c.a[1] + c.u[1]*s + c.u[0]*t
}

// segmentOf is the segment holding the sample at (s, t) in the track
// frame, or -1 when it lies more than trackSlackM off the line or past an
// end (NaN included).
func (c Corridor) segmentOf(s, t float64) int {
	if !(s >= -trackSlackM && s <= c.length+trackSlackM && math.Abs(t) <= trackSlackM) {
		return -1
	}
	k := max(0, min(int(s*float64(c.segs)/c.length), c.segs-1))
	// The division may land one off at a span end: settle it against span
	// itself, so a sample belongs to exactly the span that holds it.
	if s0, s1 := c.span(k); s < s0 && k > 0 {
		k--
	} else if s >= s1 && k+1 < c.segs {
		k++
	}
	return k
}
