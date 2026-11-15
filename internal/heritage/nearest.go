package heritage

import "math"

// Result is the zone Nearest picked for a point.
type Result struct {
	Zone      *Zone   // nil: no zone holds the point or lies within the radius
	DistanceM float64 // from the point to the zone's edge; 0 inside
	Inside    bool    // the point lies inside Zone
	Count     int     // zones holding the point or within the radius of it
}

// Nearest picks, for the point (e, n) in EPSG:25832 metres, the zone that
// holds it, or else the nearest one whose edge lies within radiusM.
//
// When several zones hold the point (overlapping zones of neighbouring
// monuments), the one it lies deepest inside wins: for buffer zones of
// equal width that is the nearest monument. Ties go to the lower system
// number, so the answer does not depend on the order upstream returned
// the zones in.
//
// The work is one pass over every edge: O(vertices), at most MaxVertices
// per page.
func Nearest(zones []Zone, e, n, radiusM float64) Result {
	var best Result
	bestKey := math.Inf(-1) // depth inside (positive) or −distance outside
	for i := range zones {
		z := &zones[i]
		inside, edge := z.locate(e, n)
		if !inside && edge > radiusM {
			continue
		}
		best.Count++
		key := -edge
		if inside {
			key = edge
		}
		if key > bestKey || key == bestKey && best.Zone != nil && lessID(z.SystemNo, best.Zone.SystemNo) {
			bestKey = key
			best.Zone, best.Inside = z, inside
			best.DistanceM = 0
			if !inside {
				best.DistanceM = edge
			}
		}
	}
	return best
}

// locate reports whether (e, n) lies inside z (inside any polygon's outer
// ring and none of its holes, even-odd) and the distance to z's nearest
// edge.
func (z *Zone) locate(e, n float64) (inside bool, edge float64) {
	edge = math.Inf(1)
	for _, poly := range z.Polygons {
		in := false
		for ri, ring := range poly {
			c, d := ringLocate(ring, e, n)
			edge = math.Min(edge, d)
			if ri == 0 {
				in = c
			} else if c {
				in = false // in a hole
			}
		}
		inside = inside || in
	}
	return inside, edge
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

// lessID orders system numbers numerically when both are digit strings of
// different length, else lexically.
func lessID(a, b string) bool {
	if len(a) != len(b) && digits(a) && digits(b) {
		return len(a) < len(b)
	}
	return a < b
}

func digits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}
