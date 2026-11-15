package dhm

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// #730 M1a — GeoJSON polygon geometry in EPSG:25832 metres, shared by every
// feature that asks a keyless upstream for areas and then measures a point
// against them: internal/heritage's protection zones (#723) and the building
// footprints of the terrain profile (#730). Both read the same GeoJSON
// Polygon/MultiPolygon shape and both need the same two answers about a
// point, so the parsing, the vertex cap and the maths live here once, next
// to ToUTM32, the repository's one EPSG:25832 implementation.
//
// Nothing here talks to the network and nothing is dataset-specific: the
// caller brings its own coordinate window, its own vertex budget and its own
// property reading.

// MultiPolygon is one feature's geometry in EPSG:25832 metres: polygons →
// rings → (easting, northing), the first ring of each polygon its outer edge
// and the rest its holes.
type MultiPolygon [][][][2]float64

// ErrGeometry is every refusal of a geometry: a type that is not Polygon or
// MultiPolygon, a polygon without rings, a ring under four positions, a
// position with fewer than two values or outside the reader's window, or
// more vertices than the reader's budget. Callers wrap or replace it with
// their own response-shape error; the detail is never shown to a client.
var ErrGeometry = errors.New("dhm: unexpected polygon geometry")

// ErrVertexBudget is the refusal of a geometry that would take the reader
// past MaxVertices. It is an ErrGeometry too (errors.Is), so a caller that
// refuses the whole document on any bad geometry need not tell them apart;
// a caller that skips a bad feature and reads on (internal/buildings) stops
// here instead, since every later feature would be refused as well.
var ErrVertexBudget = fmt.Errorf("%w: vertex budget exceeded", ErrGeometry)

// PolygonReader decodes the GeoJSON polygon geometries of one document into
// EPSG:25832 metres. One reader reads every geometry of one document, so the
// vertex budget is shared across them: a response cannot evade the cap by
// spreading its vertices over many features.
//
// The zero reader refuses every geometry: its window is the single point
// (0, 0), which no EPSG:25832 position in Denmark reaches, and its budget is
// zero vertices. Both fields must be set.
type PolygonReader struct {
	// Window is the plausible coordinate box, minE, minN, maxE, maxN in
	// EPSG:25832 metres. A position outside it means the service answered in
	// another CRS or axis order (lat/lon degrees, or northing first), which
	// must not be measured as metres. Each caller sets the box its own
	// request can legitimately return, which is wider than the request's own
	// extent but far narrower than the float64 range.
	Window [4]float64
	// MaxVertices bounds the vertices of the whole document, so a hostile or
	// broken response cannot make the point-in-polygon work unbounded.
	MaxVertices int

	vertices int
}

// Vertices is the number of vertices of every geometry Read has returned,
// across every Read. A refused geometry is not counted.
func (r *PolygonReader) Vertices() int { return r.vertices }

// Read decodes one GeoJSON geometry's "type" and "coordinates" into a
// MultiPolygon, validating every position and charging its vertices to the
// reader's budget. A Polygon is returned as a MultiPolygon of one. A third
// value in a position (height) is allowed and ignored.
//
// A refused geometry charges nothing (#730 M1b): the rings it had already
// validated are refunded, so a caller that skips a bad feature keeps the
// whole budget for the good ones. The budget therefore bounds the vertices
// a caller measures against, while the decoding work of a refused geometry
// is bounded by the caller's body cap.
//
// JSON itself keeps NaN and ±Inf out: they are not JSON numbers, and a
// finite literal too large for a float64 is an encoding/json error. So a
// position that passes the window is always finite.
func (r *PolygonReader) Read(typ string, coords json.RawMessage) (MultiPolygon, error) {
	start := r.vertices
	out, err := r.read(typ, coords)
	if err != nil {
		r.vertices = start
	}
	return out, err
}

func (r *PolygonReader) read(typ string, coords json.RawMessage) (MultiPolygon, error) {
	var multi [][][][]float64
	switch typ {
	case "MultiPolygon":
		if err := json.Unmarshal(coords, &multi); err != nil {
			return nil, err
		}
	case "Polygon":
		var poly [][][]float64
		if err := json.Unmarshal(coords, &poly); err != nil {
			return nil, err
		}
		multi = [][][][]float64{poly}
	default:
		return nil, ErrGeometry
	}
	minE, minN, maxE, maxN := r.Window[0], r.Window[1], r.Window[2], r.Window[3]
	out := make(MultiPolygon, 0, len(multi))
	for _, poly := range multi {
		if len(poly) == 0 {
			return nil, ErrGeometry
		}
		rings := make([][][2]float64, 0, len(poly))
		for _, ring := range poly {
			if len(ring) < 4 {
				return nil, ErrGeometry
			}
			pts := make([][2]float64, len(ring))
			for i, pos := range ring {
				if len(pos) < 2 || pos[0] < minE || pos[0] > maxE || pos[1] < minN || pos[1] > maxN {
					return nil, ErrGeometry
				}
				pts[i] = [2]float64{pos[0], pos[1]}
			}
			if r.vertices += len(pts); r.vertices > r.MaxVertices {
				return nil, ErrVertexBudget
			}
			rings = append(rings, pts)
		}
		out = append(out, rings)
	}
	return out, nil
}

// Locate reports whether the EPSG:25832 point (e, n) lies inside polys
// (inside some polygon's outer ring and none of that polygon's holes,
// even-odd) and the distance in metres to the nearest edge of any ring,
// +Inf when polys holds no ring.
//
// Every ring is closed implicitly, so a ring whose last position does not
// repeat its first is treated as if it did. The crossing-number test counts
// an edge only when exactly one end lies strictly above n, which makes a
// horizontal edge and a vertex shared by two edges count once rather than
// zero or twice, so the answer does not depend on where a ring starts.
//
// The work is one pass over every edge: O(vertices).
func Locate(polys MultiPolygon, e, n float64) (inside bool, edgeM float64) {
	edgeM = math.Inf(1)
	for _, poly := range polys {
		in := false
		for ri, ring := range poly {
			c, d := ringLocate(ring, e, n)
			edgeM = math.Min(edgeM, d)
			if ri == 0 {
				in = c
			} else if c {
				in = false // in a hole
			}
		}
		inside = inside || in
	}
	return inside, edgeM
}

// ringLocate is the crossing-number test and the distance to the nearest
// segment of one ring; the ring is closed implicitly.
func ringLocate(ring [][2]float64, e, n float64) (inside bool, dist float64) {
	dist2 := math.Inf(1)
	j := len(ring) - 1
	for i := range ring {
		a, b := ring[j], ring[i]
		if (b[1] > n) != (a[1] > n) && e < (a[0]-b[0])*(n-b[1])/(a[1]-b[1])+b[0] {
			inside = !inside
		}
		dist2 = math.Min(dist2, segDist2(a, b, e, n))
		j = i
	}
	return inside, math.Sqrt(dist2)
}

// segDist2 is the squared distance from (e, n) to the segment ab.
func segDist2(a, b [2]float64, e, n float64) float64 {
	dx, dy := b[0]-a[0], b[1]-a[1]
	t := 0.0
	if l2 := dx*dx + dy*dy; l2 > 0 {
		t = math.Max(0, math.Min(1, ((e-a[0])*dx+(n-a[1])*dy)/l2))
	}
	px, py := a[0]+t*dx-e, a[1]+t*dy-n
	return px*px + py*py
}
