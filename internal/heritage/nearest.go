package heritage

import (
	"math"

	"github.com/meshcore-analyzer/dhm"
)

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
// per page. The point-in-polygon and edge-distance maths itself is
// dhm.Locate, shared with the other polygon features (#730 M1a).
func Nearest(zones []Zone, e, n, radiusM float64) Result {
	var best Result
	bestKey := math.Inf(-1) // depth inside (positive) or −distance outside
	for i := range zones {
		z := &zones[i]
		inside, edge := dhm.Locate(z.Polygons, e, n)
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
