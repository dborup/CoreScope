// Package buildings tells buildings from trees along a terrain profile
// (#730): where the surface model (DSM) stands above the terrain (DTM), a
// sample inside a building footprint is a building, anything else is
// vegetation or other (masts, cranes, ...).
//
// The footprints come from INSPIRE – Bygninger (GeoDanmark buildings) on
// Dataforsyningen's OGC API Features, which needs no token and is CC BY
// 4.0. The package only ever asks for the buildings within a fixed radius
// of a straight piece of one profile's line, at most MaxSegmentM long, in
// EPSG:25832: one CQL2 DWITHIN(geometry, LINESTRING(...), r, meters)
// query per segment. It cannot ask for a bbox, a page, an attribute or
// anything else, and every number in the URL is rebuilt from validated
// decimetres, so no caller string ever reaches it.
//
// The package holds:
//
//   - Corridor: the validated straight line of one profile, its radius and
//     its segments; Segment is one segment's query line (corridor.go).
//   - Parse: a strict, geometry-only reader of the GeoJSON answer into a
//     Page of footprints. A truncated answer is marked, a broken footprint
//     is skipped and counted, and an answer in another CRS or over the
//     vertex budget is refused whole (parse.go).
//
// The polygon reading and the point-in-polygon maths are internal/dhm's
// (polygon.go), shared with internal/heritage.
//
// Nothing here is wired into the server yet (#730 M2).
package buildings

import "github.com/meshcore-analyzer/dhm"

// The corridor shape (#730 research §6, coordinator decisions).
const (
	// DefaultRadiusM is the DWITHIN radius: every footprint within it of
	// a segment's line is returned.
	DefaultRadiusM = 15
	// MaxRadiusM is the largest of AllowedRadiiM.
	MaxRadiusM = 25

	// DefaultSegmentM is the longest segment one request covers.
	DefaultSegmentM = 10000
	MinSegmentM     = 1000
	MaxSegmentM     = 10000

	// MaxCorridorM is the longest corridor in EPSG:25832 metres. The
	// terrain profile allows 80 km of ground distance
	// (dhm.MaxRasterSpanM), and UTM 32 stretches that by up to about
	// 0.16 % at the DHM extent's east edge (Bornholm, ~390 km from the
	// central meridian), so the projected line may be ~80.13 km long
	// (TestMaxCorridorCoversTheLongestProfile).
	MaxCorridorM = 80200
)

// AllowedRadiiM are the only DWITHIN radii a corridor may use, smallest
// first. Each covers the widest classification window plus the track slack
// (TestRadiusCoversTheWindow), so a segment's one request returns every
// footprint its samples' windows can touch.
var AllowedRadiiM = [...]int{15, 20, 25}

// The classification window (#730 research §6): a 5 × 5 grid of points
// around a sample, aligned with the track, ± half the profile step and at
// least ± MinHalfWindowM. It absorbs the footprint and DSM misalignment
// (footprints are 0.10 m urban, 0.75 m rural; the DSM is a 0.4 m raster
// read at the sample step).
const (
	MinHalfWindowM = 2.0
	// MaxHalfWindowM is half the profile's longest step: a coarser step
	// still gets this window, which the radius covers.
	MaxHalfWindowM = dhm.MaxStepM / 2.0
)

// windowOffsets are the grid's offsets in half-widths, along the track
// and across it.
var windowOffsets = [...]float64{-1, -0.5, 0, 0.5, 1}

// trackSlackM is how far a sample may lie off the corridor's line, or past
// one of its ends, and still be classified. The terrain profile's samples
// lie on the exact line between its ends, and the corridor's ends are
// those rounded to 0.1 m, so a real sample is within 0.071 m of it. A
// sample farther off could have a window the segment's query does not
// cover; it stays unclassified.
const trackSlackM = 0.5
