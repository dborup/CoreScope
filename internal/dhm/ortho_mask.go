package dhm

import (
	"math"
	"sync"
)

// #728 M3 — the tiles the orthophoto base layer never needs.
//
// The ortho base layer is limited by Leaflet's `bounds` to Coverage, but
// that rectangle also holds Skåne, Schleswig and Rügen. GeoDanmark
// photographs Denmark, so a tile there could only paint over the Esri
// imagery underneath, and it would spend an upstream fetch on the
// operator's token. The browser therefore gets OrthoForeignMask with the
// ortho block and skips every tile it marks.
//
// A tile is marked only when no point of it is Covered (boundary.go),
// the terrain profile's coverage gate: it lies outside RasterExtent, which
// holds all of Denmark, or on foreign land (foreign_land.txt), or partly
// each. The test is by index cell: the part of the tile's footprint
// inside RasterExtent must lie on cellIn cells only. Edge cells never
// count, so a tile with any Danish land, any Danish sea or any coast in it
// is never marked. The mask is conservative: it can leave a foreign tile
// unmarked, never mark one that shows Denmark.

// OrthoMaskZoom is the zoom whose tiles OrthoForeignMask marks. A z12
// tile is about 5.5 km across in Denmark: fine enough that Skåne and
// Schleswig away from the coast are marked, small enough to send (about
// 1 KB of base64).
const OrthoMaskZoom = 12

// TileMask marks tiles of zoom Z: tile (X0+c, Y0+r) for column c < Cols
// and row r < Rows is bit i = r*Cols+c, set in Bits[i/8] as 1<<(i%8).
// Tiles outside the window are unmarked. Bits is base64 in JSON.
type TileMask struct {
	Z    int    `json:"z"`
	X0   int    `json:"x0"`
	Y0   int    `json:"y0"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
	Bits []byte `json:"bits"`
}

// Has reports whether k, a tile of any zoom >= m.Z, lies in a marked
// tile.
func (m TileMask) Has(k TileKey) bool {
	if k.Z < m.Z {
		return false
	}
	c := k.X>>(k.Z-m.Z) - m.X0
	r := k.Y>>(k.Z-m.Z) - m.Y0
	if c < 0 || r < 0 || c >= m.Cols || r >= m.Rows {
		return false
	}
	i := r*m.Cols + c
	return m.Bits[i>>3]&(1<<(i&7)) != 0
}

// OrthoForeignMask is the mask of OrthoMaskZoom tiles DHM does not cover,
// over every tile that meets Coverage. It is built once;
// callers must not change it.
var OrthoForeignMask = sync.OnceValue(func() TileMask {
	return foreignTileMask(foreignLand(), OrthoMaskZoom)
})

func foreignTileMask(b *boundary, z int) TileMask {
	n := math.Exp2(float64(z))
	x0, x1 := lonToTileX(Coverage.West, n), lonToTileX(Coverage.East, n)
	y0, y1 := latToTileY(Coverage.North, n), latToTileY(Coverage.South, n)
	m := TileMask{Z: z, X0: x0, Y0: y0, Cols: x1 - x0 + 1, Rows: y1 - y0 + 1}
	m.Bits = make([]byte, (m.Cols*m.Rows+7)/8)
	for r := 0; r < m.Rows; r++ {
		for c := 0; c < m.Cols; c++ {
			if b.uncoveredTile(TileKey{Z: z, X: x0 + c, Y: y0 + r}) {
				i := r*m.Cols + c
				m.Bits[i>>3] |= 1 << (i & 7)
			}
		}
	}
	return m
}

func lonToTileX(lon, n float64) int { return int(math.Floor((lon + 180) / 360 * n)) }

func latToTileY(lat, n float64) int {
	phi := lat * math.Pi / 180
	return int(math.Floor((1 - math.Log(math.Tan(phi)+1/math.Cos(phi))/math.Pi) / 2 * n))
}

// footprintSamples is how many points each tile edge is sampled at to
// bound its EPSG:25832 footprint. Web Mercator tile edges are parallels
// and meridians, which bend slightly in UTM; between two of 9 samples on
// a z12 edge that bend is centimetres, and footprintPadM covers it.
const (
	footprintSamples = 9
	footprintPadM    = 1.0
)

// uncoveredTile reports whether no point of tile k is Covered: the part
// of its padded EPSG:25832 bounding box inside RasterExtent, if any,
// touches cellIn cells only.
func (b *boundary) uncoveredTile(k TileKey) bool {
	bb := k.BBox()
	minE, minN := math.Inf(1), math.Inf(1)
	maxE, maxN := math.Inf(-1), math.Inf(-1)
	add := func(x, y float64) {
		lon, lat := mercatorToLonLat(x, y)
		e, n := ToUTM32(lat, lon)
		minE, maxE = math.Min(minE, e), math.Max(maxE, e)
		minN, maxN = math.Min(minN, n), math.Max(maxN, n)
	}
	for i := 0; i < footprintSamples; i++ {
		f := float64(i) / (footprintSamples - 1)
		x := bb[0] + f*(bb[2]-bb[0])
		y := bb[1] + f*(bb[3]-bb[1])
		add(x, bb[1])
		add(x, bb[3])
		add(bb[0], y)
		add(bb[2], y)
	}
	// Outside RasterExtent nothing is covered, so only the part inside it
	// is tested. The extent is closed, like in Covers.
	ext := RasterExtent
	minE, minN = math.Max(minE-footprintPadM, ext[0]), math.Max(minN-footprintPadM, ext[1])
	maxE, maxN = math.Min(maxE+footprintPadM, ext[2]), math.Min(maxN+footprintPadM, ext[3])
	if minE > maxE || minN > maxN {
		return true
	}
	// Inside the extent but off the polygon's index: not foreign land.
	if minE < b.minE || minN < b.minN || maxE > b.maxE || maxN > b.maxN {
		return false
	}
	cx0, cx1 := b.cellRange(minE, maxE, b.minE, b.nx)
	cy0, cy1 := b.cellRange(minN, maxN, b.minN, b.ny)
	for cy := cy0; cy <= cy1; cy++ {
		for cx := cx0; cx <= cx1; cx++ {
			if b.cells[cy*b.nx+cx] != cellIn {
				return false
			}
		}
	}
	return true
}
