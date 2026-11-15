package heritage

import "math"

// CellSizeM is the edge of one grid cell in EPSG:25832 metres. Every
// click inside one cell shares one upstream request and one cache entry.
const CellSizeM = 250

// MaxMarginM bounds the margin a cell's query box is grown by: the
// largest radius the server may be configured with. The widest box is
// then 750 m.
const MaxMarginM = 250

// The cell index extent covers the WGS84 bounding box that the WMS
// capabilities advertise for the two monument layers
// (8.0899–15.5689° E, 54.4646–57.7289° N), projected to EPSG:25832 and
// snapped outwards to whole cells. TestCellExtentCoversCapabilities
// recomputes it.
const (
	MinCellE = 1764
	MaxCellE = 3701
	MinCellN = 24140
	MaxCellN = 25670
)

// Cell is one grid cell by index: it spans [E·CellSizeM, (E+1)·CellSizeM)
// in easting and the same in northing.
type Cell struct {
	E int
	N int
}

// CellOf is the cell holding the point (e, n), and whether that cell lies
// inside the extent.
func CellOf(e, n float64) (Cell, bool) {
	fe, fn := math.Floor(e/CellSizeM), math.Floor(n/CellSizeM)
	// NaN and ±Inf fail the range check before the conversion.
	if !(fe >= MinCellE && fe <= MaxCellE && fn >= MinCellN && fn <= MaxCellN) {
		return Cell{}, false
	}
	return Cell{E: int(fe), N: int(fn)}, true
}

// Valid reports whether c lies inside the extent.
func (c Cell) Valid() bool {
	return c.E >= MinCellE && c.E <= MaxCellE && c.N >= MinCellN && c.N <= MaxCellN
}

// QueryBox is c grown by marginM on every side, in EPSG:25832 metres:
// minE, minN, maxE, maxN. With marginM equal to the query radius, the
// radius around any point of c lies inside the box, so the box holds every
// zone within the radius of that point (the WFS bbox filter returns a
// polygon that intersects the box).
func (c Cell) QueryBox(marginM int) [4]int {
	e, n := c.E*CellSizeM, c.N*CellSizeM
	return [4]int{e - marginM, n - marginM, e + CellSizeM + marginM, n + CellSizeM + marginM}
}
