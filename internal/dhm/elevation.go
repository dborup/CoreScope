package dhm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
//     WCS answers on the sea and on foreign land (#692). A pixel whose
//     bilinear sample weighs a 0 m cell is transparent too (#831 F4), so
//     a coast is not fringed with the land's colour on the sea side; the
//     land's edge loses at most half a raster cell. Land below sea
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
	// interpolated inside. The interpolation is off by at most ~0.2 m at
	// z9 and under a millimetre at z13, against an output pixel of 171 m
	// and 11 m (TestElevation792_LatticeAccuracy).
	elevationCell = 16
)

// RampStop is a height in whole metres and its colour.
type RampStop struct {
	M       int
	R, G, B uint8
}

// ElevationRamp is the fixed colour ramp, tuned to Denmark's flat land
// (#860). Between stops a whole-metre height is the linear mix, rounded
// per channel. public/style.css carries the same stops as
// --dhm-elevation-N for the legend (pinned by cmd/server's
// TestDHM792_LegendCSSMatchesTheRamp).
//
// The stops come from the land's height distribution, measured over all
// Danish land (TestElevation860_MeasureLandDistribution on Copernicus DEM
// GLO-90: 42,650 km², median 27.9 m; p10 3.8, p20 9.0, p30 14.7, p40 21.0,
// p50 27.9, p60 35.0, p70 42.7, p80 52.4, p90 67.7, p95 80.7, p98 96.6,
// p99.9 129.8 m). Below 80 m they sit at round metres near the deciles, so
// each band holds 5-14 % of the land instead of the #792 linear ramp's
// 45 % in its first band (TestElevation860_BandsShareTheLand); 2 m splits
// off the marsh and reclaimed land; 80, 100 and 130 m keep three bands for
// the hills (the top 5 %).
//
// The colours run teal (0-2 m: low-lying, kept apart from the lowland),
// green (5-20 m), yellow (25-30 m), orange (40-65 m) and red from 80 m,
// to crimson and plum on the highest ground. They were picked in OKLCH:
// neighbouring stops differ by at least 0.035 (OKLab), and each shows on
// light and dark base maps at the default opacity (TestElevation860_*).
var ElevationRamp = []RampStop{
	{0, 0x00, 0x79, 0x74},
	{2, 0x1f, 0x9b, 0x82},
	{5, 0x4e, 0xa9, 0x54},
	{10, 0x7a, 0xbd, 0x4f},
	{15, 0xa6, 0xcf, 0x51},
	{20, 0xc5, 0xdb, 0x55},
	{25, 0xed, 0xdf, 0x5c},
	{30, 0xfe, 0xc9, 0x43},
	{40, 0xfc, 0xba, 0x43},
	{50, 0xf8, 0x96, 0x2d},
	{65, 0xea, 0x70, 0x28},
	{80, 0xd6, 0x4c, 0x29},
	{100, 0xba, 0x2b, 0x2e},
	{130, 0x90, 0x20, 0x47},
	{175, 0x64, 0x25, 0x60},
}

// ElevationStyle is the ramp's id: the `styles` value of an elevation
// tile URL (#860), derived from every stop, so a changed ramp is a new URL
// that no browser or shared cache holds a tile of.
var ElevationStyle = rampStyle(ElevationRamp)

// rampStyle is "ramp-" and the first 8 hex digits of the SHA-256 of the
// stops, in order.
func rampStyle(ramp []RampStop) string {
	h := sha256.New()
	for _, s := range ramp {
		h.Write([]byte{byte(s.M >> 8), byte(s.M), s.R, s.G, s.B})
	}
	return "ramp-" + hex.EncodeToString(h.Sum(nil))[:8]
}

// elevationPalette is index 0 transparent, then index 1+m the colour of m
// whole metres, m = 0..ElevationRampMaxM.
var elevationPalette = rampPalette(ElevationRamp)

// rampPalette is the tile palette of ramp: index 0 transparent, then
// index 1+m the colour of m whole metres, m = 0 up to its top stop.
// Between stops a colour is the linear mix, rounded per channel.
func rampPalette(ramp []RampStop) color.Palette {
	top := ramp[len(ramp)-1].M
	p := make(color.Palette, top+2)
	p[0] = color.NRGBA{}
	for m := 0; m <= top; m++ {
		i := 1
		for i < len(ramp)-1 && ramp[i].M < m {
			i++
		}
		a, b := ramp[i-1], ramp[i]
		t := float64(m-a.M) / float64(b.M-a.M)
		mix := func(x, y uint8) uint8 { return uint8(math.Round(float64(x) + (float64(y)-float64(x))*t)) }
		p[1+m] = color.NRGBA{mix(a.R, b.R), mix(a.G, b.G), mix(a.B, b.B), 255}
	}
	return p
}

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

// latticePoint is the EPSG:25832 position of output pixel (x, y)'s centre,
// interpolated in the lattice e, n of tileLattice.
func latticePoint(e, n []float64, x, y int) (pe, pn float64) {
	const side = TileSize/elevationCell + 1
	cj, fy := y/elevationCell, (float64(y%elevationCell)+0.5)/elevationCell
	ci, fx := x/elevationCell, (float64(x%elevationCell)+0.5)/elevationCell
	i00 := cj*side + ci
	i01, i10, i11 := i00+1, i00+side, i00+side+1
	pe = (e[i00]*(1-fx)+e[i01]*fx)*(1-fy) + (e[i10]*(1-fx)+e[i11]*fx)*fy
	pn = (n[i00]*(1-fx)+n[i01]*fx)*(1-fy) + (n[i10]*(1-fx)+n[i11]*fx)*fy
	return pe, pn
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
// placed in EPSG:25832, sampled bilinearly (a pixel next to nodata, the
// 0 m sea fill or off the grid is transparent) and mapped through the
// ramp. The work is pure and deterministic.
func RenderElevationTile(g *Grid, k TileKey) ([]byte, error) {
	e, n := tileLattice(k)
	img := image.NewPaletted(image.Rect(0, 0, TileSize, TileSize), elevationPalette)
	for y := 0; y < TileSize; y++ {
		for x := 0; x < TileSize; x++ {
			pe, pn := latticePoint(e, n, x, y)
			if h, ok := g.sampleLand(pe, pn); ok {
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
