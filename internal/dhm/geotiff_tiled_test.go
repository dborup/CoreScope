package dhm

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"runtime"
	"testing"

	"github.com/meshcore-analyzer/dhm/dhmtest"
)

// #738 M1 — the tiled EPSG:4326 GeoTIFF reader for the Copernicus GLO-30
// foreign fill. Every input is synthetic (dhmtest.COG) except the IFDs of
// one real GLO-30 tile in testdata, which hold no elevation data.

// smallCOG is a 40 × 24 COG in 16-pixel tiles (3 × 2 blocks, the last
// column and row padded) whose value at a pixel centre is a plane, so
// bilinear sampling is exact anywhere.
func smallCOG() dhmtest.COGOptions {
	return dhmtest.COGOptions{
		Width: 40, Height: 24, Tile: 16,
		Lon0: 12, Lat0: 56, PixelLon: 0.01, PixelLat: 0.005,
		Value:    func(lon, lat float64) float32 { return float32(1000*(lon-12) - 500*(56-lat) + 3) },
		Overview: true,
	}
}

func cogPlane(lon, lat float64) float64 { return 1000*(lon-12) - 500*(56-lat) + 3 }

func parseCOG(t *testing.T, o dhmtest.COGOptions) (*TiledGeoTIFF, []byte) {
	t.Helper()
	b := dhmtest.COG(o)
	tt, err := ParseTiledGeoTIFF(b, int64(len(b)))
	if err != nil {
		t.Fatalf("ParseTiledGeoTIFF: %v", err)
	}
	return tt, b
}

func decodeBlockOf(t *testing.T, tt *TiledGeoTIFF, b []byte, level, i int) Grid {
	t.Helper()
	l := &tt.Levels[level]
	off, n := l.BlockRange(i)
	g, err := tt.DecodeBlock(level, i, b[off:off+n], nil)
	if err != nil {
		t.Fatalf("DecodeBlock(%d, %d): %v", level, i, err)
	}
	return g
}

func TestParseTiledGeoTIFFLayout(t *testing.T) {
	tt, b := parseCOG(t, smallCOG())
	if len(tt.Levels) != 2 {
		t.Fatalf("%d levels, want 2", len(tt.Levels))
	}
	l0, l1 := tt.Levels[0], tt.Levels[1]
	if l0.Width != 40 || l0.Height != 24 || l0.TileWidth != 16 || l0.TileHeight != 16 || l0.NumBlocks() != 6 {
		t.Fatalf("level 0 %+v", l0)
	}
	// PixelIsPoint: the tiepoint is the centre of pixel (0, 0), so the
	// outer corner is half a pixel west and north of it.
	want := Georef{OriginE: 12 - 0.005, OriginN: 56 + 0.0025, PixelE: 0.01, PixelN: 0.005}
	if !closeGeoref(l0.Georef, want) {
		t.Fatalf("level 0 georef %+v, want %+v", l0.Georef, want)
	}
	// The overview: the same outer extent at twice the pixel size.
	want1 := Georef{OriginE: want.OriginE, OriginN: want.OriginN, PixelE: 0.02, PixelN: 0.01}
	if l1.Width != 20 || l1.Height != 12 || l1.NumBlocks() != 2 || !closeGeoref(l1.Georef, want1) {
		t.Fatalf("level 1 %+v, want 20×12, 2 blocks, %+v", l1, want1)
	}
	for li := range tt.Levels {
		l := &tt.Levels[li]
		for i := 0; i < l.NumBlocks(); i++ {
			off, n := l.BlockRange(i)
			if off < 8 || n < 1 || off+n > int64(len(b)) || n > int64(l.MaxBlockBytes()) {
				t.Fatalf("level %d block %d at %d+%d", li, i, off, n)
			}
		}
	}
}

func closeGeoref(a, b Georef) bool {
	const eps = 1e-12
	return math.Abs(a.OriginE-b.OriginE) < eps && math.Abs(a.OriginN-b.OriginN) < eps &&
		math.Abs(a.PixelE-b.PixelE) < eps && math.Abs(a.PixelN-b.PixelN) < eps
}

// Every block decodes to the values written, edge blocks cropped to the
// raster, and sampling a block at any point inside it is the plane.
func TestDecodeBlockValuesAndSampling(t *testing.T) {
	for _, uncompressed := range []bool{false, true} {
		o := smallCOG()
		o.Uncompressed = uncompressed
		tt, b := parseCOG(t, o)
		for li := range tt.Levels {
			l := &tt.Levels[li]
			for i := 0; i < l.NumBlocks(); i++ {
				g := decodeBlockOf(t, tt, b, li, i)
				bx, by := i%l.BlocksAcross(), i/l.BlocksAcross()
				wantW, wantH := min(16, l.Width-16*bx), min(16, l.Height-16*by)
				if g.Width != wantW || g.Height != wantH || len(g.Values) != wantW*wantH {
					t.Fatalf("level %d block %d: %d×%d (%d values), want %d×%d", li, i, g.Width, g.Height, len(g.Values), wantW, wantH)
				}
				// Every pixel centre, read through Sample.
				for row := 0; row < g.Height; row++ {
					for col := 0; col < g.Width; col++ {
						lon := g.Georef.OriginE + (float64(col)+0.5)*g.Georef.PixelE
						lat := g.Georef.OriginN - (float64(row)+0.5)*g.Georef.PixelN
						v, ok := g.Sample(lon, lat)
						if !ok || math.Abs(v-cogPlane(lon, lat)) > 1e-3 {
							t.Fatalf("level %d block %d pixel (%d, %d): %v %v, want %v", li, i, col, row, v, ok, cogPlane(lon, lat))
						}
					}
				}
			}
		}
	}
}

// Locate picks the block that holds the pixel nearest a point and clamps
// the point into it, so a point between two blocks, or in the half pixel
// outside the outermost pixel centres, still samples.
func TestTiledLevelLocate(t *testing.T) {
	tt, b := parseCOG(t, smallCOG())
	l := &tt.Levels[0]
	for _, c := range []struct {
		name      string
		lon, lat  float64
		wantBlock int
	}{
		{"first pixel centre", 12, 56, 0},
		{"inside block 4", 12 + 0.01*20.3, 56 - 0.005*17.6, 4},
		{"west of the column 15/16 cell edge", 12 + 0.01*15.4, 56 - 0.005*3, 0},
		{"east of it", 12 + 0.01*15.6, 56 - 0.005*3, 1},
		{"west of the outer edge", 11.9, 56, 0},
		{"south-east, past the last centre", 12 + 0.01*39.9, 56 - 0.005*23.9, 5},
		{"far outside", 20, 50, 5},
	} {
		blk, lon, lat := l.Locate(c.lon, c.lat)
		if blk != c.wantBlock {
			t.Errorf("%s: block %d, want %d", c.name, blk, c.wantBlock)
			continue
		}
		g := decodeBlockOf(t, tt, b, 0, blk)
		if _, ok := g.Sample(lon, lat); !ok {
			t.Errorf("%s: clamped point (%v, %v) does not sample in block %d", c.name, lon, lat, blk)
		}
	}
}

// The IFDs of the real Copernicus GLO-30 tile N55E012, as served on
// 2026-11-14 (first 1224 bytes of 15 746 303; see testdata/README.md):
// 2400 × 3600, 1024² tiles, DEFLATE + predictor 3, PixelIsPoint at 12° E
// 56° N, and three overviews.
func TestParseTiledGeoTIFFRealGLO30Header(t *testing.T) {
	b, err := os.ReadFile("testdata/glo30-n55e012-ifds.bin")
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != "2281b1a32b2b02b4ff021480c13e4dd26782ddbf75e3acff95deb7b1d2dbb5cb" {
		t.Fatal("testdata/glo30-n55e012-ifds.bin changed; see testdata/README.md")
	}
	const size = 15746303
	tt, err := ParseTiledGeoTIFF(b, size)
	if err != nil {
		t.Fatalf("real GLO-30 header: %v", err)
	}
	if len(tt.Levels) != 2 || tt.HasNoData {
		t.Fatalf("%d levels, nodata %v; want full resolution + ½ overview, no nodata", len(tt.Levels), tt.HasNoData)
	}
	l0, l1 := &tt.Levels[0], &tt.Levels[1]
	want := Georef{OriginE: 12 - 1.0/4800, OriginN: 56 + 1.0/7200, PixelE: 1.0 / 2400, PixelN: 1.0 / 3600}
	if l0.Width != 2400 || l0.Height != 3600 || l0.TileWidth != 1024 || l0.NumBlocks() != 12 || !closeGeoref(l0.Georef, want) {
		t.Fatalf("level 0 %d×%d tile %d, %d blocks, %+v", l0.Width, l0.Height, l0.TileWidth, l0.NumBlocks(), l0.Georef)
	}
	if l1.Width != 1200 || l1.Height != 1800 || l1.NumBlocks() != 4 {
		t.Fatalf("level 1 %d×%d, %d blocks", l1.Width, l1.Height, l1.NumBlocks())
	}
	// Byte ranges exactly as the file lists them.
	if off, n := l0.BlockRange(0); off != 4082928 || n != 2777476 {
		t.Fatalf("level 0 block 0 at %d+%d", off, n)
	}
	if off, n := l0.BlockRange(11); off != 15742213 || n != 4086 || off+n > size {
		t.Fatalf("level 0 block 11 at %d+%d", off, n)
	}
	if off, n := l1.BlockRange(3); off != 4044532 || n != 38388 {
		t.Fatalf("level 1 block 3 at %d+%d", off, n)
	}
	// Copenhagen City Hall, 55.6761° N 12.5683° E, is in block 4 (column
	// 1364 of 2400, row 1166 of 3600 → block column 1, block row 1).
	if blk, _, _ := l0.Locate(12.5683, 55.6761); blk != 1*3+1 {
		t.Fatalf("Copenhagen in block %d, want 4", blk)
	}
	// The strip decoder still refuses it: tiled, and not EPSG:25832.
	if _, err := decodeGeoTIFF(b); err == nil {
		t.Fatal("decodeGeoTIFF accepted a tiled EPSG:4326 file")
	}
}

// A prefix that ends inside the IFDs asks for more; the same bytes as a
// whole file are malformed.
func TestParseTiledGeoTIFFShortHeader(t *testing.T) {
	b := dhmtest.COG(smallCOG())
	for _, n := range []int{8, 100, 200} {
		if _, err := ParseTiledGeoTIFF(b[:n], int64(len(b))); !errors.Is(err, ErrShortHeader) {
			t.Errorf("prefix %d: %v, want ErrShortHeader", n, err)
		}
		if _, err := ParseTiledGeoTIFF(b[:n], int64(n)); err == nil || errors.Is(err, ErrShortHeader) {
			t.Errorf("whole file of %d bytes: %v, want malformed", n, err)
		}
	}
	if _, err := ParseTiledGeoTIFF(b[:4], int64(len(b))); err == nil || errors.Is(err, ErrShortHeader) {
		t.Errorf("4 bytes: %v, want malformed (no TIFF header)", err)
	}
	// A block that ends past the file is malformed, whatever the prefix.
	if _, err := ParseTiledGeoTIFF(b, int64(len(b))-1); err == nil || errors.Is(err, ErrShortHeader) {
		t.Errorf("size one short of the last block: %v, want malformed", err)
	}
}

func geoKeysWith(edit func(k []uint16) []uint16) []uint16 { return edit(dhmtest.GLO30GeoKeys()) }

// #738 research §3.4 / round-2 review finding 8: the tiled reader accepts
// exactly EPSG:4326 PixelIsPoint, and the UTM reader is not loosened.
func TestParseTiledGeoTIFFRejectsWrongCRS(t *testing.T) {
	for name, keys := range map[string][]uint16{
		"ETRS89 (4258), not WGS84": geoKeysWith(func(k []uint16) []uint16 { k[15] = 4258; return k }),
		"projected model":          geoKeysWith(func(k []uint16) []uint16 { k[7] = 1; return k }),
		"PixelIsArea":              geoKeysWith(func(k []uint16) []uint16 { k[11] = 1; return k }),
		"no GeographicType key":    {1, 1, 0, 2, 1024, 0, 1, 2, 1025, 0, 1, 2},
		"no RasterType key":        {1, 1, 0, 2, 1024, 0, 1, 2, 2048, 0, 1, 4326},
		"no ModelType key":         {1, 1, 0, 2, 1025, 0, 1, 2, 2048, 0, 1, 4326},
		"plus a projected CRS":     append(geoKeysWith(func(k []uint16) []uint16 { k[3] = 4; return k }), 3072, 0, 1, 25832),
		"UTM 32 keys":              utm32GeoKeys(rasterPixelIsPoint),
		"GeographicType elsewhere": geoKeysWith(func(k []uint16) []uint16 { k[13] = 34736; return k }),
	} {
		o := smallCOG()
		o.GeoKeys = keys
		b := dhmtest.COG(o)
		if _, err := ParseTiledGeoTIFF(b, int64(len(b))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	o := smallCOG()
	o.Edit = func(level int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
		return dropTag(es, 34735)
	}
	b := dhmtest.COG(o)
	if _, err := ParseTiledGeoTIFF(b, int64(len(b))); err == nil {
		t.Error("no GeoKeyDirectory: accepted")
	}

	// The strip decoder still takes only UTM 32: a strip raster that says
	// EPSG:4326 is refused, as before #738.
	f := newFixture(3, 2, varied(3, 2))
	f.tie, f.scale = []float64{0, 0, 0, 12, 56, 0}, []float64{0.01, 0.01, 0}
	f.geoKeys = dhmtest.GLO30GeoKeys()
	if _, err := decodeGeoTIFF(f.bytes()); err == nil {
		t.Error("decodeGeoTIFF accepted an EPSG:4326 raster")
	}
}

func dropTag(es []dhmtest.COGEntry, tag uint16) []dhmtest.COGEntry {
	out := es[:0]
	for _, e := range es {
		if e.Tag != tag {
			out = append(out, e)
		}
	}
	return out
}

func replaceTag(es []dhmtest.COGEntry, e dhmtest.COGEntry) []dhmtest.COGEntry {
	es = dropTag(es, e.Tag)
	return append(es, e)
}

func tiledMalformed() map[string]dhmtest.COGOptions {
	at := func(level int, e dhmtest.COGEntry) func(int, []dhmtest.COGEntry) []dhmtest.COGEntry {
		return func(l int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
			if l == level {
				return replaceTag(es, e)
			}
			return es
		}
	}
	drop := func(level int, tag uint16) func(int, []dhmtest.COGEntry) []dhmtest.COGEntry {
		return func(l int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
			if l == level {
				return dropTag(es, tag)
			}
			return es
		}
	}
	big := uint32(1 << 30)
	cases := map[string]func(int, []dhmtest.COGEntry) []dhmtest.COGEntry{
		"16-bit samples":          at(0, dhmtest.ShortsEntry(258, 16)),
		"integer samples":         at(0, dhmtest.ShortsEntry(339, 1)),
		"two samples per pixel":   at(0, dhmtest.ShortsEntry(277, 2)),
		"LZW":                     at(0, dhmtest.ShortsEntry(259, 5)),
		"predictor 4":             at(0, dhmtest.ShortsEntry(317, 4)),
		"planar configuration 2":  at(0, dhmtest.ShortsEntry(284, 2)),
		"fill order 2":            at(0, dhmtest.ShortsEntry(266, 2)),
		"orientation 3":           at(0, dhmtest.ShortsEntry(274, 3)),
		"tile width 20":           at(0, dhmtest.ShortsEntry(322, 20)),
		"tile width 2048":         at(0, dhmtest.ShortsEntry(322, 2048)),
		"tile length 0":           at(0, dhmtest.ShortsEntry(323, 0)),
		"no tile length":          drop(0, 323),
		"no tile offsets":         drop(0, 324),
		"no tile byte counts":     drop(0, 325),
		"too few offsets":         at(0, dhmtest.LongsEntry(324, 100, 200)),
		"too many byte counts":    at(0, dhmtest.LongsEntry(325, 1, 1, 1, 1, 1, 1, 1)),
		"zero byte count":         at(0, dhmtest.LongsEntry(325, 1, 1, 1, 0, 1, 1)),
		"block past the file":     at(0, dhmtest.LongsEntry(325, 1, 1, 1, 1, 1, big)),
		"oversize block":          at(0, dhmtest.LongsEntry(325, 1, 1, 1, 1, 1, 16*16*4+8193)),
		"strips as well":          at(0, dhmtest.LongsEntry(273, 8)),
		"width 0":                 at(0, dhmtest.ShortsEntry(256, 0)),
		"width 5000":              at(0, dhmtest.LongsEntry(256, 5000)),
		"no tiepoint":             drop(0, 33922),
		"two tiepoints":           at(0, dhmtest.DoublesEntry(33922, 0, 0, 0, 12, 56, 0, 1, 1, 0, 12.01, 55.99, 0)),
		"no pixel scale":          drop(0, 33550),
		"zero pixel scale":        at(0, dhmtest.DoublesEntry(33550, 0, 0.005, 0)),
		"rotated":                 at(0, dhmtest.DoublesEntry(34264, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1)),
		"full level is a subfile": at(0, dhmtest.LongsEntry(254, 1)),
		"overview of wrong width": at(1, dhmtest.ShortsEntry(256, 21)),
		"overview 16-bit":         at(1, dhmtest.ShortsEntry(258, 16)),
		"overview LZW":            at(1, dhmtest.ShortsEntry(259, 5)),
		"overview too few blocks": at(1, dhmtest.LongsEntry(324, 100)),
	}
	out := map[string]dhmtest.COGOptions{}
	for name, edit := range cases {
		o := smallCOG()
		o.Edit = edit
		out[name] = o
	}
	return out
}

func TestParseTiledGeoTIFFRejectsMalformed(t *testing.T) {
	cases := tiledMalformed()
	// Consistent in every other way: the block tables match 24-pixel
	// tiles, which the TIFF spec does not allow.
	o := smallCOG()
	o.Tile = 24
	cases["24-pixel tiles"] = o
	for name, o := range cases {
		b := dhmtest.COG(o)
		if _, err := ParseTiledGeoTIFF(b, int64(len(b))); err == nil {
			t.Errorf("%s: accepted", name)
		} else if errors.Is(err, ErrShortHeader) {
			t.Errorf("%s: ErrShortHeader for a complete file", name)
		}
	}
}

// A second IFD that is not a reduced-resolution image (a mask) is not an
// overview: the file has one level.
func TestParseTiledGeoTIFFIgnoresMaskIFD(t *testing.T) {
	o := smallCOG()
	o.Edit = func(l int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
		if l == 1 {
			return replaceTag(es, dhmtest.LongsEntry(254, 4))
		}
		return es
	}
	tt, _ := parseCOG(t, o)
	if len(tt.Levels) != 1 {
		t.Fatalf("%d levels, want 1", len(tt.Levels))
	}
	o.Overview = false
	if tt, _ = parseCOG(t, o); len(tt.Levels) != 1 {
		t.Fatalf("no overview: %d levels", len(tt.Levels))
	}
}

// DecodeBlock takes exactly the block's stored bytes, and a block that
// inflates to anything but one full tile is an error.
func TestDecodeBlockRejectsBadData(t *testing.T) {
	tt, b := parseCOG(t, smallCOG())
	l := &tt.Levels[0]
	off, n := l.BlockRange(0)
	good := b[off : off+n]
	if _, err := tt.DecodeBlock(0, 0, good[:len(good)-1], nil); err == nil {
		t.Error("one byte short: accepted")
	}
	if _, err := tt.DecodeBlock(0, 0, append(append([]byte{}, good...), 0), nil); err == nil {
		t.Error("one byte long: accepted")
	}
	if _, err := tt.DecodeBlock(0, 6, good, nil); err == nil {
		t.Error("block index 6 of 6: accepted")
	}
	if _, err := tt.DecodeBlock(2, 0, good, nil); err == nil {
		t.Error("level 2 of 2: accepted")
	}
	bad := append([]byte{}, good...)
	bad[len(bad)-1] ^= 0xff // the Adler-32 checksum
	if _, err := tt.DecodeBlock(0, 0, bad, nil); err == nil {
		t.Error("bad checksum: accepted")
	}

	// A stored block that inflates past one tile, and one that is short.
	for name, raw := range map[string][]byte{
		"inflates past the tile": deflate(make([]byte, 16*16*4+4)),
		"inflates short":         deflate(make([]byte, 16*16*4-4)),
	} {
		o := smallCOG()
		o.Block = func(level, i int, b []byte) []byte {
			if level == 0 && i == 0 {
				return raw
			}
			return b
		}
		o.Overview = false
		tt, b := parseCOG(t, o)
		off, n := tt.Levels[0].BlockRange(0)
		if _, err := tt.DecodeBlock(0, 0, b[off:off+n], nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A caller's buffer is reused when it is large enough.
func TestDecodeBlockReusesBuffer(t *testing.T) {
	tt, b := parseCOG(t, smallCOG())
	off, n := tt.Levels[0].BlockRange(0)
	buf := make([]float32, 16*16)
	g, err := tt.DecodeBlock(0, 0, b[off:off+n], buf)
	if err != nil || &g.Values[0] != &buf[0] {
		t.Fatalf("buffer not used: %v", err)
	}
	// With a buffer, a decode allocates only the inflater and row
	// scratch, never a tile: a 1024² tile is 4 MiB.
	o := dhmtest.GLO30Tile(55, 12, dhmtest.Terrain)
	o.Width, o.Height, o.Overview = 1024, 1024, false
	big := dhmtest.COG(o)
	bt, err := ParseTiledGeoTIFF(big, int64(len(big)))
	if err != nil {
		t.Fatal(err)
	}
	boff, bn := bt.Levels[0].BlockRange(0)
	bbuf := make([]float32, 1024*1024)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < 4; i++ {
		if _, err := bt.DecodeBlock(0, 0, big[boff:boff+bn], bbuf); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&after)
	if per := (after.TotalAlloc - before.TotalAlloc) / 4; per > 256<<10 {
		t.Fatalf("%d bytes allocated per 1024² decode with a buffer, want < 256 KiB", per)
	}
}

// FuzzParseTiledGeoTIFF: no input panics, and an accepted header is
// always bounded and consistent: every block lies inside the file and
// within MaxBlockBytes, Locate always names a real block, and a decoded
// block samples to a finite value or none.
func FuzzParseTiledGeoTIFF(f *testing.F) {
	o := smallCOG()
	f.Add(dhmtest.COG(o))
	o.Uncompressed = true
	f.Add(dhmtest.COG(o))
	o = smallCOG()
	o.Overview = false
	f.Add(dhmtest.COG(o))
	if b, err := os.ReadFile("testdata/glo30-n55e012-ifds.bin"); err == nil {
		f.Add(b)
	}
	for _, o := range tiledMalformed() {
		f.Add(dhmtest.COG(o))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		for _, size := range []int64{int64(len(b)), int64(len(b)) + 1<<20} {
			tt, err := ParseTiledGeoTIFF(b, size)
			if err != nil {
				continue
			}
			if len(tt.Levels) < 1 || len(tt.Levels) > 2 {
				t.Fatalf("%d levels", len(tt.Levels))
			}
			for li := range tt.Levels {
				l := &tt.Levels[li]
				if l.Width < 1 || l.Height < 1 || l.Width > MaxTiledRasterSize || l.Height > MaxTiledRasterSize ||
					l.TileWidth < 16 || l.TileWidth > MaxTileSize || l.TileHeight < 16 || l.TileHeight > MaxTileSize ||
					!l.Georef.valid() {
					t.Fatalf("level %d: %+v", li, l)
				}
				if l.NumBlocks() != l.BlocksAcross()*((l.Height+l.TileHeight-1)/l.TileHeight) {
					t.Fatalf("level %d: %d blocks", li, l.NumBlocks())
				}
				for i := 0; i < l.NumBlocks(); i++ {
					off, n := l.BlockRange(i)
					if off < 0 || n < 1 || off+n > size || n > int64(l.MaxBlockBytes()) {
						t.Fatalf("level %d block %d at %d+%d of %d", li, i, off, n, size)
					}
				}
				x := l.Georef
				for _, p := range [][2]float64{{x.OriginE, x.OriginN}, {x.OriginE + 1e6, x.OriginN - 1e6}, {math.NaN(), 0}} {
					blk, lon, lat := l.Locate(p[0], p[1])
					if blk < 0 || blk >= l.NumBlocks() {
						t.Fatalf("Locate(%v) = block %d of %d", p, blk, l.NumBlocks())
					}
					off, n := l.BlockRange(blk)
					if off+n > int64(len(b)) {
						continue
					}
					g, err := tt.DecodeBlock(li, blk, b[off:off+n], nil)
					if err != nil {
						continue
					}
					if v, ok := g.Sample(lon, lat); ok && (math.IsNaN(v) || math.IsInf(v, 0)) {
						t.Fatalf("Sample = %v, true", v)
					}
				}
			}
		}
	})
}

// BenchmarkDecodeBlockGLO30 is the CPU of one full-resolution GLO-30
// block (#738 perf proof): inflate 4 MiB, undo predictor 3, with a reused
// buffer as the fill uses it.
func BenchmarkDecodeBlockGLO30(b *testing.B) {
	o := dhmtest.GLO30Tile(55, 12, dhmtest.Terrain)
	o.Width, o.Height, o.Overview = 1024, 1024, false // one block of the real layout
	data := dhmtest.COG(o)
	tt, err := ParseTiledGeoTIFF(data, int64(len(data)))
	if err != nil {
		b.Fatal(err)
	}
	off, n := tt.Levels[0].BlockRange(0)
	raw := data[off : off+n]
	buf := make([]float32, 1024*1024)
	b.SetBytes(1024 * 1024 * 4)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := tt.DecodeBlock(0, 0, raw, buf); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(n), "stored-bytes")
}

// The real header parses in microseconds: it is re-parsed from the header
// cache on every profile.
func BenchmarkParseTiledGeoTIFFRealHeader(b *testing.B) {
	data, err := os.ReadFile("testdata/glo30-n55e012-ifds.bin")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := ParseTiledGeoTIFF(data, 15746303); err != nil {
			b.Fatal(err)
		}
	}
}
