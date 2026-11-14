package mastedb

// CRS is the only coordinate reference system the WFS is asked in, and the
// one its UTM_E / UTM_N fields are published in.
const CRS = "EPSG:25832"

// CellSizeM is the edge of one grid cell in EPSG:25832 metres (the plan's
// 2 km). A rural 10×10 km sample held 435 rows, so rural cells stay far
// below the WFS's documented 500-row page; central Copenhagen (225 rows in
// one km²) can exceed it, and such a cell comes back Truncated. Splitting
// those cells waits until it is observed (#724 plan, YAGNI).
const CellSizeM = 2000

// The cell index extent covers the WGS84 bounding box that the WFS
// capabilities advertise for mastedatabasen:MasteIBrug
// (8.0614–15.5668° E, 54.4556–57.7482° N), projected to EPSG:25832 and
// snapped outwards to whole cells. TestCellExtentCoversCapabilities
// recomputes it.
const (
	MinCellE = 219
	MaxCellE = 462
	MinCellN = 3017
	MaxCellN = 3209
)

// Cell is one grid cell by index: it spans [E·CellSizeM, (E+1)·CellSizeM)
// in easting and the same in northing.
type Cell struct {
	E int `json:"e"`
	N int `json:"n"`
}

// Valid reports whether c lies inside the Danish extent.
func (c Cell) Valid() bool {
	return c.E >= MinCellE && c.E <= MaxCellE && c.N >= MinCellN && c.N <= MaxCellN
}

// BBox is c's bounds in EPSG:25832 metres: minE, minN, maxE, maxN.
func (c Cell) BBox() [4]float64 {
	e, n := float64(c.E*CellSizeM), float64(c.N*CellSizeM)
	return [4]float64{e, n, e + CellSizeM, n + CellSizeM}
}

// Contains reports whether the point (e, n) lies in c. The bounds are
// half-open, so a point on a shared edge belongs to exactly one cell: the
// WFS BBOX filter is inclusive and returns it for both.
func (c Cell) Contains(e, n float64) bool {
	b := c.BBox()
	return e >= b[0] && e < b[2] && n >= b[1] && n < b[3]
}
