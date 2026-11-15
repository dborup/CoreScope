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
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dhm/dhmtest"
)

// #792 — the colour-by-elevation overlay: WCS DTM rendered into coloured
// PNG tiles. Every test uses synthetic grids or a fake upstream
// (httptest); none reaches Dataforsyningen.

// A Copenhagen tile at the overlay's top zoom, and one at its first.
var (
	elevTileZ13 = TileKey{Z: 13, X: 4382, Y: 2560}
	elevTileZ9  = TileKey{Z: 9, X: 273, Y: 160}
)

// The ramp is the documented one, spelled out here: a changed stop fails.
func TestElevation792_RampIsDocumented(t *testing.T) {
	want := []RampStop{
		{0, 0x4a, 0x8c, 0x5c},
		{25, 0x7f, 0xb0, 0x69},
		{50, 0xc2, 0xcf, 0x7e},
		{75, 0xec, 0xd9, 0x8b},
		{100, 0xd9, 0xa9, 0x6a},
		{125, 0xb0, 0x7c, 0x4f},
		{150, 0xa8, 0x90, 0x7e},
		{175, 0xf2, 0xee, 0xe9},
	}
	if len(ElevationRamp) != len(want) {
		t.Fatalf("ramp %v, want %v", ElevationRamp, want)
	}
	for i := range want {
		if ElevationRamp[i] != want[i] {
			t.Errorf("stop %d = %+v, want %+v", i, ElevationRamp[i], want[i])
		}
	}
	if ElevationRampMaxM != 175 {
		t.Errorf("ElevationRampMaxM = %d", ElevationRampMaxM)
	}
}

// Golden: the colour of a height. Stops are exact, a height between two
// stops is the linear mix rounded per channel, and a height is rounded to
// the whole metre first. Out of range clamps; 0 m exactly (the WCS fill
// for sea and for land outside Denmark, #692), nodata, NaN and ±Inf are
// transparent.
func TestElevation792_RampGolden(t *testing.T) {
	opaque := func(r, g, b uint8) color.NRGBA { return color.NRGBA{r, g, b, 255} }
	cases := []struct {
		h    float64
		want color.NRGBA
	}{
		{0, color.NRGBA{}},
		{math.NaN(), color.NRGBA{}},
		{math.Inf(1), color.NRGBA{}},
		{math.Inf(-1), color.NRGBA{}},
		{0.3, opaque(0x4a, 0x8c, 0x5c)},   // rounds to 0 m, but is land
		{-0.3, opaque(0x4a, 0x8c, 0x5c)},  // likewise
		{-7, opaque(0x4a, 0x8c, 0x5c)},    // Lammefjord: below sea, still land
		{1, opaque(0x4c, 0x8d, 0x5d)},     // 1/25 of the way to 25 m
		{12.5, opaque(0x66, 0x9f, 0x63)},  // rounds to 13 m
		{25, opaque(0x7f, 0xb0, 0x69)},    // a stop
		{62, opaque(0xd6, 0xd4, 0x84)},    // 12/25 of 50 → 75
		{100, opaque(0xd9, 0xa9, 0x6a)},   // a stop
		{170.8, opaque(0xe6, 0xdf, 0xd8)}, // Møllehøj, 171 m
		{175, opaque(0xf2, 0xee, 0xe9)},
		{2000, opaque(0xf2, 0xee, 0xe9)}, // clamps to the top
	}
	for _, c := range cases {
		got := elevationPalette[elevationIndex(c.h)]
		if got != c.want {
			t.Errorf("height %v: %v, want %v", c.h, got, c.want)
		}
	}
	if len(elevationPalette) != ElevationRampMaxM+2 {
		t.Errorf("palette has %d colours, want %d", len(elevationPalette), ElevationRampMaxM+2)
	}
}

// grid places vals (w × h, northernmost row first) on r's bbox.
func elevGrid(r RasterRequest, w, h int, f func(col, row int) float32) Grid {
	vals := make([]float32, w*h)
	for row := 0; row < h; row++ {
		for col := 0; col < w; col++ {
			vals[row*w+col] = f(col, row)
		}
	}
	return Grid{Width: w, Height: h, Values: vals, Georef: Georef{
		OriginE: r.BBox[0], OriginN: r.BBox[3],
		PixelE: (r.BBox[2] - r.BBox[0]) / float64(w), PixelN: (r.BBox[3] - r.BBox[1]) / float64(h),
	}}
}

// decodeElevTile decodes a rendered tile: a 256×256 paletted PNG whose
// palette is the ramp.
func decodeElevTile(t *testing.T, data []byte) *image.Paletted {
	t.Helper()
	if err := checkTilePNG(data); err != nil {
		t.Fatalf("not a 256x256 PNG: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := img.(*image.Paletted)
	if !ok {
		t.Fatalf("decoded %T, want a paletted image", img)
	}
	return p
}

// pixelHash is the SHA-256 of the decoded pixels as NRGBA: independent of
// the PNG compressor, so it holds across Go versions.
func pixelHash(img image.Image) string {
	b := img.Bounds()
	h := sha256.New()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			h.Write([]byte{c.R, c.G, c.B, c.A})
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func renderElev(t *testing.T, k TileKey, f func(col, row int) float32, noData *float32) *image.Paletted {
	t.Helper()
	r, ok := ElevationRaster(k)
	if !ok {
		t.Fatalf("%v: no raster", k)
	}
	g := elevGrid(r, r.Width, r.Height, f)
	if noData != nil {
		g.NoData, g.HasNoData = *noData, true
	}
	data, err := RenderElevationTile(&g, k)
	if err != nil {
		t.Fatal(err)
	}
	return decodeElevTile(t, data)
}

// Golden tiles from synthetic grids: flat land is one colour, the sea
// fill and nodata are transparent everywhere, the extremes clamp, and a
// west–east slope runs through the ramp in order. Every case carries the
// hash of its decoded pixels; the spot checks say what they mean. Since
// #831 F4 a pixel weighing a 0 m cell is transparent, so the slope's 0 m
// west column and the half-sea case's coast are clear on the sea side.
func TestElevation792_RenderGolden(t *testing.T) {
	nd := float32(-9999)
	type spot struct {
		x, y int
		want color.NRGBA
	}
	cases := []struct {
		name  string
		f     func(col, row int) float32
		nd    *float32
		spots []spot
		hash  string
	}{
		{"flat 40 m", func(int, int) float32 { return 40 }, nil,
			[]spot{{0, 0, elevationPalette[elevationIndex(40)].(color.NRGBA)}, {255, 255, elevationPalette[elevationIndex(40)].(color.NRGBA)}}, "6df1e9edc40f8249"},
		{"sea fill", func(int, int) float32 { return 0 }, nil,
			[]spot{{0, 0, color.NRGBA{}}, {128, 128, color.NRGBA{}}}, "8a39d2abd3999ab7"},
		{"nodata", func(int, int) float32 { return nd }, &nd,
			[]spot{{0, 0, color.NRGBA{}}, {200, 17, color.NRGBA{}}}, "8a39d2abd3999ab7"},
		{"NaN", func(int, int) float32 { return float32(math.NaN()) }, nil,
			[]spot{{5, 5, color.NRGBA{}}}, "8a39d2abd3999ab7"},
		{"extreme high", func(int, int) float32 { return 3.0e38 }, nil,
			[]spot{{0, 0, color.NRGBA{0xf2, 0xee, 0xe9, 255}}}, "3dda33cc1d973441"},
		{"extreme low", func(int, int) float32 { return -3.0e38 }, nil,
			[]spot{{0, 0, color.NRGBA{0x4a, 0x8c, 0x5c, 255}}}, "c44a814c5340f7ab"},
		{"slope 0-200 m west to east", func(col, _ int) float32 { return float32(col) * 200 / 127 }, nil,
			[]spot{{255, 128, color.NRGBA{0xf2, 0xee, 0xe9, 255}}}, "371aeccf43789561"},
		{"west half sea, east half 60 m", func(col, _ int) float32 {
			if col < 64 {
				return 0
			}
			return 60
		}, nil, []spot{{2, 128, color.NRGBA{}}, {253, 128, elevationPalette[elevationIndex(60)].(color.NRGBA)}}, "2681b4766de686eb"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			img := renderElev(t, elevTileZ13, c.f, c.nd)
			for _, s := range c.spots {
				if got := color.NRGBAModel.Convert(img.At(s.x, s.y)).(color.NRGBA); got != s.want {
					t.Errorf("pixel (%d,%d) = %v, want %v", s.x, s.y, got, s.want)
				}
			}
			if got := pixelHash(img); got != c.hash {
				t.Errorf("pixel hash %s, want %s", got, c.hash)
			}
		})
	}
}

// The slope runs through the ramp monotonically west to east: no pixel's
// height index is below its western neighbour's.
func TestElevation792_SlopeIsMonotonic(t *testing.T) {
	img := renderElev(t, elevTileZ13, func(col, _ int) float32 { return float32(col) * 200 / 127 }, nil)
	for y := 0; y < TileSize; y += 37 {
		prev := uint8(0)
		for x := 0; x < TileSize; x++ {
			i := img.ColorIndexAt(x, y)
			if i < prev {
				t.Fatalf("row %d: index %d at x=%d after %d", y, i, x, prev)
			}
			prev = i
		}
		// The UTM grid is turned ~3° against the tile at 12.6° E, so the
		// west edge starts a few metres up the slope further south.
		if first, last := img.ColorIndexAt(0, y), img.ColorIndexAt(TileSize-1, y); first > 12 || last != ElevationRampMaxM+1 {
			t.Errorf("row %d runs %d..%d, want 1..12 to %d", y, first, last, ElevationRampMaxM+1)
		}
	}
}

// The encoder: a 256×256 paletted PNG, its palette the ramp with index 0
// transparent, every other entry opaque.
func TestElevation792_EncoderShape(t *testing.T) {
	img := renderElev(t, elevTileZ13, func(int, int) float32 { return 40 }, nil)
	if b := img.Bounds(); b.Dx() != TileSize || b.Dy() != TileSize {
		t.Fatalf("bounds %v", b)
	}
	if len(img.Palette) != ElevationRampMaxM+2 {
		t.Fatalf("palette of %d", len(img.Palette))
	}
	for i, c := range img.Palette {
		_, _, _, a := c.RGBA()
		if (i == 0) != (a == 0) || (i != 0 && a != 0xffff) {
			t.Errorf("palette %d alpha %d", i, a)
		}
	}
}

// ElevationRaster: the tile's bbox in EPSG:25832, clipped to the extent,
// at ElevationRasterSize pixels across the whole tile; a request the WCS
// allow-list accepts for every z9..z13 tile in the coverage.
func TestElevation792_RasterRequest(t *testing.T) {
	r, ok := ElevationRaster(elevTileZ13)
	if !ok {
		t.Fatal("no raster for Copenhagen")
	}
	if r.Model != ModelTerrain || r.Width != ElevationRasterSize || r.Height != ElevationRasterSize {
		t.Fatalf("request %+v", r)
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	for i, v := range r.BBox {
		if v != math.Trunc(v) {
			t.Errorf("bbox[%d] = %v is not whole metres", i, v)
		}
	}
	// It covers every pixel centre of the tile.
	e, n := tileLattice(elevTileZ13)
	for i := range e {
		if e[i] < r.BBox[0] || e[i] > r.BBox[2] || n[i] < r.BBox[1] || n[i] > r.BBox[3] {
			t.Fatalf("lattice point %d (%v, %v) outside %v", i, e[i], n[i], r.BBox)
		}
	}
	// A tile beyond the extent has none; one across its edge is clipped.
	if _, ok := ElevationRaster(TileKey{Z: 9, X: 200, Y: 160}); ok {
		t.Error("a tile west of the extent has a raster")
	}
}

// The zoom range, measured: every z9..z13 tile that meets the coverage
// gives a request inside the WCS allow-list (MaxRasterSpanM), and z8
// cannot: its tiles span more than 80 km.
func TestElevation792_ZoomRangeFitsTheWCSAllowList(t *testing.T) {
	maxSpan := map[int]float64{}
	for z := ElevationMinZoom - 1; z <= ElevationMaxZoom; z++ {
		n := 1 << z
		lo, hi := elevLonLatTile(Coverage.West, Coverage.North, z), elevLonLatTile(Coverage.East, Coverage.South, z)
		step := 1
		if z >= 12 {
			step = 7 // a sample is enough up there; spans only shrink
		}
		bad := 0
		for x := lo.X; x <= hi.X && x < n; x += step {
			for y := lo.Y; y <= hi.Y && y < n; y += step {
				k := TileKey{Z: z, X: x, Y: y}
				if !k.IntersectsCoverage() {
					continue
				}
				r, ok := ElevationRaster(k)
				if !ok {
					continue
				}
				span := math.Max(r.BBox[2]-r.BBox[0], r.BBox[3]-r.BBox[1])
				maxSpan[z] = math.Max(maxSpan[z], span)
				if err := r.Validate(); err != nil {
					bad++
				}
			}
		}
		if z >= ElevationMinZoom && bad > 0 {
			t.Errorf("z%d: %d tiles give a request the WCS refuses", z, bad)
		}
		if z < ElevationMinZoom && bad == 0 {
			t.Errorf("z%d fits the WCS allow-list: ElevationMinZoom could be lower", z)
		}
		t.Logf("z%d: widest raster bbox %.1f km", z, maxSpan[z]/1000)
	}
}

// offExtentTile is a z9 tile the coverage gate lets through whose bbox
// misses RasterExtent: the corners of the lat/lon coverage box.
func offExtentTile() (TileKey, bool) {
	lo, hi := elevLonLatTile(Coverage.West, Coverage.North, 9), elevLonLatTile(Coverage.East, Coverage.South, 9)
	for x := lo.X; x <= hi.X; x++ {
		for y := lo.Y; y <= hi.Y; y++ {
			k := TileKey{Z: 9, X: x, Y: y}
			if _, ok := ElevationRaster(k); k.IntersectsCoverage() && !ok {
				return k, true
			}
		}
	}
	return TileKey{}, false
}

func elevLonLatTile(lon, lat float64, z int) TileKey {
	n := math.Exp2(float64(z))
	la := lat * math.Pi / 180
	return TileKey{Z: z, X: int((lon + 180) / 360 * n), Y: int((1 - math.Log(math.Tan(la)+1/math.Cos(la))/math.Pi) / 2 * n)}
}

// The overlay is in the id allow-list, served from the WCS DTM, z9–13,
// credited to DHM/Terræn.
func TestElevation792_InTheOverlayTable(t *testing.T) {
	o, ok := LookupOverlay("elevation")
	if !ok {
		t.Fatal("elevation is not an overlay id")
	}
	want := Overlay{ID: "elevation", Service: "dhm_wcs_DAF", Layer: "dhm_terraen", MinZoom: 9, MaxZoom: 13,
		DatasetURL: "https://dataforsyningen.dk/data/930"}
	if o != want {
		t.Errorf("overlay %+v, want %+v", o, want)
	}
	if !o.Rendered() {
		t.Error("elevation is not a rendered overlay")
	}
	for _, id := range []string{"terrain", "surface", "contours"} {
		if o, _ := LookupOverlay(id); o.Rendered() {
			t.Errorf("%s is rendered", id)
		}
	}
	for z := 7; z <= 19; z++ {
		_, _, err := ParseTileRequest(overlayTileQuery("elevation", TileKey{Z: z, X: 1 << (z - 1), Y: 1 << (z - 2)}))
		if (err == nil) != (z >= 9 && z <= 13) {
			t.Errorf("z%d: %v", z, err)
		}
	}
	// Neither the coverage name nor a case variant is an id.
	for _, l := range []string{"dhm_terraen", "Elevation", "ELEVATION", "elevation ", "elevation,terrain"} {
		if _, _, err := ParseTileRequest(overlayTileQuery(l, elevTileZ13)); err == nil {
			t.Errorf("layers=%q accepted", l)
		}
	}
	// The WMS allow-list never lets the WCS coverage through as a layer.
	q := elevTileZ13.OverlayQuery(o)
	if err := overlayQueryCheck(WMSService)(q); err == nil {
		t.Error("dhm_DAF accepts layers=dhm_terraen")
	}
}

// wcsFake answers GetCoverage with a GeoTIFF of the requested size from
// f, and records the query and the token header.
type wcsFake struct {
	srv    *httptest.Server
	hits   atomic.Int32
	query  atomic.Value // url.Values
	token  atomic.Value // string
	status int
	f      func(col, row int) float32
}

func newWCSFake(t *testing.T, f func(col, row int) float32) *wcsFake {
	w := &wcsFake{status: 200, f: f}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.hits.Add(1)
		w.query.Store(r.URL.Query())
		w.token.Store(r.Header.Get(TokenHeader))
		if r.URL.Path != "/"+WCSService {
			rw.WriteHeader(404)
			return
		}
		if w.status != 200 {
			rw.WriteHeader(w.status)
			return
		}
		rr, err := parseCoverageQuery(r.URL.Query())
		if err != nil {
			rw.WriteHeader(400)
			return
		}
		vals := make([]float32, rr.Width*rr.Height)
		for row := 0; row < rr.Height; row++ {
			for col := 0; col < rr.Width; col++ {
				vals[row*rr.Width+col] = w.f(col, row)
			}
		}
		rw.Header().Set("Content-Type", "image/tiff")
		rw.Write(dhmtest.GeoTIFF(rr.Width, rr.Height, vals, nil, true))
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func elevClient(t *testing.T, base string) *Client {
	opts := DefaultClientOptions()
	opts.BaseURL = base + "/"
	opts.Logf = func(string, ...interface{}) {}
	return NewClient(Config{Enabled: true, Token: "tok-792-secret"}, opts)
}

// FetchOverlayAdmitted renders the elevation id from one WCS GetCoverage:
// the exact allow-listed shape, the token in the header only, and an
// admission slot taken for it.
func TestElevation792_FetchFromWCS(t *testing.T) {
	up := newWCSFake(t, func(col, _ int) float32 { return float32(col) })
	c := elevClient(t, up.srv.URL)
	o, _ := LookupOverlay("elevation")
	adm := NewAdmission(1)
	data, err := c.FetchOverlayAdmitted(context.Background(), adm, o, elevTileZ13)
	if err != nil {
		t.Fatal(err)
	}
	decodeElevTile(t, data)
	if up.hits.Load() != 1 {
		t.Fatalf("%d upstream calls", up.hits.Load())
	}
	r, _ := ElevationRaster(elevTileZ13)
	if got := up.query.Load().(url.Values); got.Encode() != r.CoverageQuery().Encode() {
		t.Errorf("query %s, want %s", got.Encode(), r.CoverageQuery().Encode())
	}
	if up.token.Load().(string) != "tok-792-secret" {
		t.Error("token header missing")
	}
	if strings.Contains(up.query.Load().(url.Values).Encode(), "tok-792") {
		t.Error("token in the query")
	}
	if adm.InUse() != 0 {
		t.Errorf("admission slot kept: %d", adm.InUse())
	}
	// A full admission refuses before any upstream call.
	held := NewAdmission(1)
	held.tryAcquire()
	if _, err := c.FetchOverlayAdmitted(context.Background(), held, o, TileKey{Z: 13, X: 4383, Y: 2560}); err != ErrAdmissionFull {
		t.Errorf("full admission: %v", err)
	}
	if up.hits.Load() != 1 {
		t.Errorf("full admission reached upstream")
	}
}

// Upstream failures are fixed categories and nothing is rendered; a tile
// off the extent is a transparent tile with no upstream call; an overlay
// value outside the table, or a zoom outside its range, is refused.
func TestElevation792_FetchFailures(t *testing.T) {
	up := newWCSFake(t, func(int, int) float32 { return 5 })
	c := elevClient(t, up.srv.URL)
	o, _ := LookupOverlay("elevation")
	up.status = 500
	if _, err := c.FetchOverlayAdmitted(context.Background(), nil, o, elevTileZ13); FailureReason(err) != "status 500" {
		t.Errorf("500: %v", err)
	}
	up.status = 200
	n := up.hits.Load()
	off, found := offExtentTile()
	if !found {
		t.Fatal("no z9 tile meets the coverage but misses the extent")
	}
	data, err := c.FetchOverlayAdmitted(context.Background(), nil, o, off)
	if err != nil || up.hits.Load() != n {
		t.Fatalf("off-extent tile: err %v, %d calls", err, up.hits.Load()-n)
	}
	img := decodeElevTile(t, data)
	for _, i := range img.Pix {
		if i != 0 {
			t.Fatal("off-extent tile is not transparent")
		}
	}
	bad := o
	bad.MaxZoom = 19
	for _, x := range []struct {
		o Overlay
		k TileKey
	}{{o, TileKey{Z: 14, X: 8764, Y: 5120}}, {bad, TileKey{Z: 14, X: 8764, Y: 5120}}, {Overlay{ID: "x", Service: WCSService, Layer: "dhm_overflade"}, elevTileZ13}} {
		if _, err := c.FetchOverlayAdmitted(context.Background(), nil, x.o, x.k); err != ErrBadRequest {
			t.Errorf("%+v %v: %v", x.o, x.k, err)
		}
	}
	if up.hits.Load() != n {
		t.Error("a refused request reached upstream")
	}
}

// FuzzElevationTile: no upstream body panics the decode or the render,
// and an accepted one always renders a complete 256×256 tile.
func FuzzElevationTile(f *testing.F) {
	r, _ := ElevationRaster(elevTileZ13)
	nd := float32(-9999)
	for _, v := range [][]float32{nil, {0}, {40}, {-9999}, {float32(math.NaN())}, {3e38}} {
		vals := make([]float32, r.Width*r.Height)
		for i := range vals {
			if len(v) > 0 {
				vals[i] = v[0]
			} else {
				vals[i] = float32(i % 200)
			}
		}
		f.Add(dhmtest.GeoTIFF(r.Width, r.Height, vals, &nd, true))
		f.Add(dhmtest.GeoTIFF(r.Width, r.Height, vals, nil, false))
	}
	f.Add(dhmtest.GeoTIFF(3, 2, []float32{1, 2, 3, 4, 5, 6}, nil, false))
	f.Add([]byte("II*\x00"))
	f.Fuzz(func(t *testing.T, b []byte) {
		g, err := rasterGrid(b, r)
		if err != nil {
			return
		}
		data, err := RenderElevationTile(&g, elevTileZ13)
		if err != nil {
			t.Fatal(err)
		}
		if err := checkTilePNG(data); err != nil {
			t.Fatal(err)
		}
	})
}

// BenchmarkElevationTile is the CPU of one tile on a miss: decode the
// DEFLATE GeoTIFF the WCS returns, colour and encode. The #792 target is
// well under 50 ms; it also reports the PNG size the cache holds.
func BenchmarkElevationTile(b *testing.B) {
	for _, size := range []int{128, 256} {
		b.Run(map[int]string{128: "raster128", 256: "raster256"}[size], func(b *testing.B) {
			r, _ := ElevationRaster(elevTileZ13)
			r.Width, r.Height = size, size
			vals := make([]float32, size*size)
			for i := range vals {
				col, row := float64(i%size), float64(i/size)
				vals[i] = float32(60 + 45*math.Sin(col/17)*math.Cos(row/23) + 10*math.Sin((col+row)/5) + float64(i%7)/10)
				if col < float64(size)/8 {
					vals[i] = 0 // a strip of sea
				}
			}
			body := dhmtest.GeoTIFF(size, size, vals, nil, true)
			b.ReportAllocs()
			b.ResetTimer()
			var n int
			for i := 0; i < b.N; i++ {
				g, err := rasterGrid(body, r)
				if err != nil {
					b.Fatal(err)
				}
				data, err := RenderElevationTile(&g, elevTileZ13)
				if err != nil {
					b.Fatal(err)
				}
				n = len(data)
			}
			b.ReportMetric(float64(len(body)), "wcs-bytes")
			b.ReportMetric(float64(n), "png-bytes")
		})
	}
}

// The CPU of one tile on a miss stays well under the #792 target of 50 ms:
// the best of 7 runs of the 128 px raster (the shipped size) fails above
// 50 ms and logs against a 25 ms target. Skipped under -race; CI runs it
// once more without (deploy.yml, next to the profilecard budget).
func TestElevation792_CPUWithinBudget(t *testing.T) {
	if raceEnabled {
		t.Skip("timing is meaningless under -race")
	}
	const target, limit = 25 * time.Millisecond, 50 * time.Millisecond
	r, _ := ElevationRaster(elevTileZ13)
	vals := make([]float32, r.Width*r.Height)
	for i := range vals {
		col, row := float64(i%r.Width), float64(i/r.Width)
		vals[i] = float32(60 + 45*math.Sin(col/17)*math.Cos(row/23) + 10*math.Sin((col+row)/5))
	}
	body := dhmtest.GeoTIFF(r.Width, r.Height, vals, nil, true)
	best := time.Duration(math.MaxInt64)
	for i := 0; i < 7; i++ {
		start := time.Now()
		g, err := rasterGrid(body, r)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := RenderElevationTile(&g, elevTileZ13); err != nil {
			t.Fatal(err)
		}
		best = min(best, time.Since(start))
	}
	switch {
	case best > limit:
		t.Errorf("%v per tile, limit %v (target %v)", best, limit, target)
	case best > target:
		t.Logf("%v per tile, over the %v target (limit %v)", best, target, limit)
	}
	t.Logf("per tile: %v (target %v, limit %v)", best, target, limit)
}

// #831 F4: the 0 m sea fill does not tint a coast. On a half-sea,
// half-land (3 m) raster, no output pixel whose centre lies in a sea cell
// is coloured, at z9 and z13; the land side stays coloured a cell in.
func TestElevation792_CoastHasNoSeaFringe(t *testing.T) {
	for _, k := range []TileKey{elevTileZ9, elevTileZ13} {
		r, ok := ElevationRaster(k)
		if !ok {
			t.Fatalf("%v: no raster", k)
		}
		half := r.Width / 2
		img := renderElev(t, k, func(col, _ int) float32 {
			if col < half {
				return 0
			}
			return 3
		}, nil)
		g := elevGrid(r, r.Width, r.Height, func(int, int) float32 { return 0 })
		coast := g.Georef.OriginE + float64(half)*g.Georef.PixelE
		e, n := tileLattice(k)
		fringe, land := 0, 0
		for y := 0; y < TileSize; y++ {
			for x := 0; x < TileSize; x++ {
				pe, _ := latticePoint(e, n, x, y)
				on := img.ColorIndexAt(x, y) != 0
				if pe < coast && on {
					fringe++
				}
				if pe > coast+1.5*g.Georef.PixelE && on {
					land++
				}
			}
		}
		if fringe != 0 {
			t.Errorf("z%d: %d coloured pixels on the sea side", k.Z, fringe)
		}
		if land == 0 {
			t.Errorf("z%d: no coloured land a cell in from the coast", k.Z)
		}
	}
}

// #831 F5: the lattice's interpolated position of every output pixel is
// within 0.5 m of the projection itself at z9 (measured ~0.19 m) and 1 cm
// at z13, so a coarser lattice is caught.
func TestElevation792_LatticeAccuracy(t *testing.T) {
	for _, c := range []struct {
		k   TileKey
		tol float64
	}{{elevTileZ9, 0.5}, {elevTileZ13, 0.01}} {
		e, n := tileLattice(c.k)
		b := c.k.BBox()
		span := b[2] - b[0]
		worst := 0.0
		for y := 0; y < TileSize; y++ {
			for x := 0; x < TileSize; x++ {
				pe, pn := latticePoint(e, n, x, y)
				lon, lat := mercatorToLonLat(b[0]+span*(float64(x)+0.5)/TileSize, b[3]-span*(float64(y)+0.5)/TileSize)
				te, tn := ToUTM32(lat, lon)
				worst = math.Max(worst, math.Hypot(pe-te, pn-tn))
			}
		}
		if worst > c.tol {
			t.Errorf("z%d: worst lattice error %.4f m, want <= %g m", c.k.Z, worst, c.tol)
		}
	}
}

// #831 F7: a tile that meets the DHM coverage but misses the WCS
// RasterExtent is not covered by the elevation overlay (the proxy serves
// it as "outside"); the WMS overlays still cover it.
func TestElevation792_CoversNeedsTheRasterExtent(t *testing.T) {
	var off *TileKey
	n := 1 << ElevationMinZoom
	for x := 0; x < n && off == nil; x++ {
		for y := 0; y < n; y++ {
			k := TileKey{Z: ElevationMinZoom, X: x, Y: y}
			if _, ok := ElevationRaster(k); k.IntersectsCoverage() && !ok {
				off = &k
				break
			}
		}
	}
	if off == nil {
		t.Fatal("no z9 tile meets the coverage but misses the raster extent")
	}
	if overlayElevation.Covers(*off) {
		t.Errorf("%v: covered by elevation", *off)
	}
	if !overlayTerrain.Covers(*off) {
		t.Errorf("%v: not covered by the terrain hillshade", *off)
	}
	if !overlayElevation.Covers(elevTileZ9) || overlayElevation.Covers(TileKey{Z: 9, X: 100, Y: 160}) {
		t.Error("Covers is wrong inside Denmark or far outside it")
	}
}
