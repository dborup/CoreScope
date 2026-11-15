package dhm

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"math"
)

// #792 — the colour-by-elevation overlay.
//
// No upstream layer colours DHM by height (the #734 token checks: dhm_DAF
// has hillshades, contours and spot heights; wms/dhm_temp's quickDHM
// terrain layers answer unstyled black tiles), so the server renders it:
// for one Web Mercator tile it fetches the DTM through the WCS the terrain
// profile already uses (one GetCoverage, the exact #681 allow-listed
// shape), maps every height through ElevationRamp and encodes a 256×256
// paletted PNG. The browser asks for it like any other overlay, by the id
// "elevation" through the tile proxy, so it shares the token, the budget,
// the admission bound, the coalescing, the coverage gate and the cache.
//
//   - Zoom 9–13. Below z9 a tile spans more than the WCS allow-list's
//     80 km (MaxRasterSpanM; TestElevation792_ZoomRangeFitsTheWCSAllowList
//     measures every tile); at z13 a pixel is ~10 m, and above it the page
//     upscales the z13 tiles (maxNativeZoom), where the hillshade shows the
//     detail.
//   - The raster is ElevationRasterSize (128) pixels across the tile and is
//     sampled bilinearly into the 256 output pixels: a quarter of the WCS
//     bytes of 256, and a 1 m colour step does not need more.
//   - Transparent: nodata, NaN, ±Inf, and exactly 0 m, which is what the
//     WCS answers on the sea and on foreign land (#692). Land below sea
//     level (Lammefjord) is coloured as 0 m. Lakes are coloured at their
//     surface height: the DTM has no water mask.
//   - The tile is derived from DHM, so its credit carries "data
//     bearbejdet" (AttributionDerived, #733).

const (
	ElevationMinZoom = 9
	ElevationMaxZoom = 13
	// ElevationRasterSize is the WCS raster's pixels across a whole tile.
	ElevationRasterSize = 128
	// ElevationRampMaxM is the top of the ramp: Denmark's highest ground is
	// about 171 m (Møllehøj). Higher values take the top colour.
	ElevationRampMaxM = 175

	// elevationCell is the edge, in output pixels, of the lattice the tile
	// is projected on: EPSG:25832 is computed at its corners and
	// interpolated inside, where the projection is linear to well under a
	// millimetre at z9.
	elevationCell = 16
)

// RampStop is a height in whole metres and its colour.
type RampStop struct {
	M       int
	R, G, B uint8
}

// ElevationRamp is the fixed colour ramp, lowland green through yellow and
// brown to light high ground. Between stops a whole-metre height is the
// linear mix, rounded per channel. public/style.css carries the same stops
// as --dhm-elevation-N for the legend (pinned by cmd/server's
// TestDHM792_LegendCSSMatchesTheRamp).
var ElevationRamp = []RampStop{
	{0, 0x4a, 0x8c, 0x5c},
	{25, 0x7f, 0xb0, 0x69},
	{50, 0xc2, 0xcf, 0x7e},
	{75, 0xec, 0xd9, 0x8b},
	{100, 0xd9, 0xa9, 0x6a},
	{125, 0xb0, 0x7c, 0x4f},
	{150, 0xa8, 0x90, 0x7e},
	{175, 0xf2, 0xee, 0xe9},
}

// elevationPalette is index 0 transparent, then index 1+m the colour of m
// whole metres, m = 0..ElevationRampMaxM.
var elevationPalette = func() color.Palette {
	p := make(color.Palette, ElevationRampMaxM+2)
	p[0] = color.NRGBA{}
	for m := 0; m <= ElevationRampMaxM; m++ {
		i := 1
		for i < len(ElevationRamp)-1 && ElevationRamp[i].M < m {
			i++
		}
		a, b := ElevationRamp[i-1], ElevationRamp[i]
		t := float64(m-a.M) / float64(b.M-a.M)
		mix := func(x, y uint8) uint8 { return uint8(math.Round(float64(x) + (float64(y)-float64(x))*t)) }
		p[1+m] = color.NRGBA{mix(a.R, b.R), mix(a.G, b.G), mix(a.B, b.B), 255}
	}
	return p
}()

// elevationIndex is the palette index of height h in metres.
func elevationIndex(h float64) uint8 {
	if h == 0 || math.IsNaN(h) || math.IsInf(h, 0) {
		return 0
	}
	m := math.Round(h)
	if m < 0 {
		m = 0
	} else if m > ElevationRampMaxM {
		m = ElevationRampMaxM
	}
	return uint8(1 + m)
}

// tileLattice is the EPSG:25832 position of the lattice points of tile k,
// (TileSize/elevationCell+1)² of them, row by row from the north-west
// corner, elevationCell output pixels apart.
func tileLattice(k TileKey) (e, n []float64) {
	const side = TileSize/elevationCell + 1
	b := k.BBox()
	span := b[2] - b[0]
	e, n = make([]float64, side*side), make([]float64, side*side)
	for j := 0; j < side; j++ {
		for i := 0; i < side; i++ {
			lon, lat := mercatorToLonLat(b[0]+span*float64(i)/(side-1), b[3]-span*float64(j)/(side-1))
			e[j*side+i], n[j*side+i] = ToUTM32(lat, lon)
		}
	}
	return e, n
}

// ElevationRaster is the WCS request that renders tile k: the DTM over
// the tile's EPSG:25832 bbox in whole metres, clipped to RasterExtent, at
// ElevationRasterSize pixels across the whole tile. ok is false when the
// tile misses the extent: it is transparent without an upstream call.
func ElevationRaster(k TileKey) (r RasterRequest, ok bool) {
	e, n := tileLattice(k)
	bb := [4]float64{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	for i := range e {
		bb[0], bb[1] = math.Min(bb[0], e[i]), math.Min(bb[1], n[i])
		bb[2], bb[3] = math.Max(bb[2], e[i]), math.Max(bb[3], n[i])
	}
	bb = [4]float64{math.Floor(bb[0]), math.Floor(bb[1]), math.Ceil(bb[2]), math.Ceil(bb[3])}
	pxE := (bb[2] - bb[0]) / ElevationRasterSize
	pxN := (bb[3] - bb[1]) / ElevationRasterSize
	ext := RasterExtent
	c := [4]float64{math.Max(bb[0], ext[0]), math.Max(bb[1], ext[1]), math.Min(bb[2], ext[2]), math.Min(bb[3], ext[3])}
	if !(c[2] > c[0] && c[3] > c[1]) {
		return RasterRequest{}, false
	}
	return RasterRequest{
		Model:  ModelTerrain,
		BBox:   c,
		Width:  rasterPixels(c[2]-c[0], pxE),
		Height: rasterPixels(c[3]-c[1], pxN),
	}, true
}

// rasterPixels is how many pixels of size px cover span, at least one;
// float noise in a whole tile's span/px does not add a pixel.
func rasterPixels(span, px float64) int {
	return max(1, int(math.Ceil(span/px-1e-9)))
}

// RenderElevationTile colours g over tile k: each output pixel centre is
// placed in EPSG:25832, sampled bilinearly (a pixel next to nodata or off
// the grid is transparent) and mapped through the ramp. The work is pure
// and deterministic.
func RenderElevationTile(g *Grid, k TileKey) ([]byte, error) {
	const side = TileSize/elevationCell + 1
	e, n := tileLattice(k)
	img := image.NewPaletted(image.Rect(0, 0, TileSize, TileSize), elevationPalette)
	for y := 0; y < TileSize; y++ {
		cj, fy := y/elevationCell, (float64(y%elevationCell)+0.5)/elevationCell
		for x := 0; x < TileSize; x++ {
			ci, fx := x/elevationCell, (float64(x%elevationCell)+0.5)/elevationCell
			i00 := cj*side + ci
			i01, i10, i11 := i00+1, i00+side, i00+side+1
			pe := (e[i00]*(1-fx)+e[i01]*fx)*(1-fy) + (e[i10]*(1-fx)+e[i11]*fx)*fy
			pn := (n[i00]*(1-fx)+n[i01]*fx)*(1-fy) + (n[i10]*(1-fx)+n[i11]*fx)*fy
			if h, ok := g.Sample(pe, pn); ok {
				img.Pix[y*img.Stride+x] = elevationIndex(h)
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// elevationEmptyTile is the transparent tile of a tile off the extent.
var elevationEmptyTile = func() []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewPaletted(image.Rect(0, 0, TileSize, TileSize), elevationPalette)); err != nil {
		panic(err)
	}
	return buf.Bytes()
}()

// fetchElevationTile renders one elevation tile from one admitted WCS
// call. o must be the table's elevation overlay and k inside its range:
// FetchOverlayAdmitted's callers are not trusted.
func (c *Client) fetchElevationTile(ctx context.Context, adm *Admission, o Overlay, k TileKey) ([]byte, error) {
	if o != overlayElevation || k.Z < o.MinZoom || k.Z > o.MaxZoom {
		return nil, ErrBadRequest
	}
	r, ok := ElevationRaster(k)
	if !ok {
		return elevationEmptyTile, nil
	}
	data, err := c.get(ctx, adm, WCSService, r.CoverageQuery(), geoTIFFContentTypes)
	if err != nil {
		return nil, err
	}
	g, err := rasterGrid(data, r)
	if err != nil {
		return nil, err
	}
	out, err := RenderElevationTile(&g, k)
	if err != nil {
		return nil, FetchError{"render"}
	}
	return out, nil
}
