package dhm

import (
	"encoding/binary"
	"fmt"
	"math"
	"runtime"
	"slices"
	"sync"
	"testing"
)

// #681 M2 — the float32 GeoTIFF reader against golden fixtures generated
// in-test (tiff_fixture_test.go) and two hand-assembled byte sequences that
// do not depend on that writer.

// varied is a w×h raster with negative, fractional, zero and large values.
func varied(w, h int) []float32 {
	return ramp(w, h, func(c, r int) float32 {
		return float32(c)*0.37 - float32(r)*1.25 + float32((c*7+r*3)%5) - 3.5
	})
}

func sameValues(t *testing.T, name string, got, want []float32) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d values, want %d", name, len(got), len(want))
	}
	for i := range want {
		if math.Float32bits(got[i]) != math.Float32bits(want[i]) {
			t.Fatalf("%s: value %d = %v, want %v", name, i, got[i], want[i])
		}
	}
}

// Every supported layout decodes to the exact values: both byte orders,
// uncompressed and DEFLATE, every predictor, one strip, one row per strip,
// and a short last strip.
func TestDecodeGeoTIFFGoldenMatrix(t *testing.T) {
	const w, h = 13, 11
	want := varied(w, h)
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		for _, comp := range []uint16{compressionNone, compressionDeflate} {
			for _, pred := range []uint16{0, predictorNone, predictorHorizontal, predictorFloatingPoint} {
				for _, rps := range []int{0, 1, 4} {
					name := fmt.Sprintf("%v/comp%d/pred%d/rps%d", order, comp, pred, rps)
					f := newFixture(w, h, want)
					f.order, f.compression, f.predictor, f.rowsPerStr = order, comp, pred, rps
					g, err := decodeGeoTIFF(f.bytes())
					if err != nil {
						t.Fatalf("%s: %v", name, err)
					}
					if g.Width != w || g.Height != h {
						t.Fatalf("%s: %dx%d", name, g.Width, g.Height)
					}
					sameValues(t, name, g.Values, want)
				}
			}
		}
	}
}

// The old PKZIP-style DEFLATE code (32946) carries the same zlib stream.
func TestDecodeGeoTIFFOldDeflateCode(t *testing.T) {
	want := varied(5, 4)
	f := newFixture(5, 4, want)
	f.compression = compressionDeflate
	f.fields = map[uint16]*fixtureField{tagCompression: f.shorts(compressionDeflateOld)}
	g, err := decodeGeoTIFF(f.bytes())
	if err != nil {
		t.Fatal(err)
	}
	sameValues(t, "32946", g.Values, want)
}

// A big-endian 2×1 file assembled byte by byte from the TIFF 6.0 layout,
// independent of the fixture writer: values 1.5 and −2.25.
var handBuiltBigEndian = []byte("MM\x00\x2a\x00\x00\x00\x08" +
	"\x00\x09" + // 9 entries
	"\x01\x00\x00\x04\x00\x00\x00\x01\x00\x00\x00\x02" + // ImageWidth LONG 2
	"\x01\x01\x00\x04\x00\x00\x00\x01\x00\x00\x00\x01" + // ImageLength LONG 1
	"\x01\x02\x00\x03\x00\x00\x00\x01\x00\x20\x00\x00" + // BitsPerSample SHORT 32
	"\x01\x03\x00\x03\x00\x00\x00\x01\x00\x01\x00\x00" + // Compression SHORT 1
	"\x01\x11\x00\x04\x00\x00\x00\x01\x00\x00\x00\x7a" + // StripOffsets LONG 122
	"\x01\x15\x00\x03\x00\x00\x00\x01\x00\x01\x00\x00" + // SamplesPerPixel SHORT 1
	"\x01\x16\x00\x04\x00\x00\x00\x01\x00\x00\x00\x01" + // RowsPerStrip LONG 1
	"\x01\x17\x00\x04\x00\x00\x00\x01\x00\x00\x00\x08" + // StripByteCounts LONG 8
	"\x01\x53\x00\x03\x00\x00\x00\x01\x00\x03\x00\x00" + // SampleFormat SHORT 3 (IEEE float)
	"\x00\x00\x00\x00" + // no next IFD
	"\x3f\xc0\x00\x00\xc0\x10\x00\x00") // 1.5, -2.25

func TestDecodeGeoTIFFHandBuilt(t *testing.T) {
	g, err := decodeGeoTIFF(handBuiltBigEndian)
	if err != nil {
		t.Fatal(err)
	}
	if g.Width != 2 || g.Height != 1 || g.HasNoData || g.Georef != (Georef{}) {
		t.Fatalf("grid %+v", g)
	}
	sameValues(t, "hand-built", g.Values, []float32{1.5, -2.25})
}

// The predictors against hand-computed rows for 1.0 (0x3F800000) and 2.0
// (0x40000000). Floating point (libtiff fpDiff): byte planes MSB first,
// [3F 40 | 80 00 | 00 00 | 00 00], then byte differences
// [3F 01 40 80 00 00 00 00] — the same in either byte order. Horizontal:
// word differences 0x3F800000, 0x00800000 in file byte order.
func TestDecodeRowPredictorsHandComputed(t *testing.T) {
	want := []float32{1, 2}
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		r := &tiffReader{order: order}
		dst := make([]float32, 2)
		r.decodeRow(dst, []byte{0x3f, 0x01, 0x40, 0x80, 0, 0, 0, 0}, predictorFloatingPoint, make([]byte, 8))
		sameValues(t, fmt.Sprintf("fp %v", order), dst, want)
	}
	le := &tiffReader{order: binary.LittleEndian}
	dst := make([]float32, 2)
	le.decodeRow(dst, []byte{0, 0, 0x80, 0x3f, 0, 0, 0x80, 0}, predictorHorizontal, nil)
	sameValues(t, "horizontal LE", dst, want)
	be := &tiffReader{order: binary.BigEndian}
	be.decodeRow(dst, []byte{0x3f, 0x80, 0, 0, 0, 0x80, 0, 0}, predictorHorizontal, nil)
	sameValues(t, "horizontal BE", dst, want)
}

// GDAL_NODATA is read as written; NaN is accepted; garbage is an error.
func TestDecodeGeoTIFFNoData(t *testing.T) {
	vals := []float32{-9999, 1, 2, -9999}
	for _, tc := range []struct {
		s       string
		want    float32
		has     bool
		wantErr bool
	}{
		{"", 0, false, false},
		{"-9999", -9999, true, false},
		{"-9999.0 ", -9999, true, false},
		{"-3.4028234663852886e+38", -math.MaxFloat32, true, false},
		{"nan", float32(math.NaN()), true, false},
		{"abc", 0, false, true},
		{"1e400", 0, false, true},
		{fmt.Sprintf("%064d", 1), 0, false, true}, // 65 bytes with the NUL
	} {
		f := newFixture(2, 2, vals)
		f.noData = tc.s
		g, err := decodeGeoTIFF(f.bytes())
		if tc.wantErr {
			if err == nil {
				t.Errorf("nodata %q accepted", tc.s)
			}
			continue
		}
		if err != nil {
			t.Fatalf("nodata %q: %v", tc.s, err)
		}
		same := g.NoData == tc.want || (g.NoData != g.NoData && tc.want != tc.want)
		if g.HasNoData != tc.has || !same {
			t.Errorf("nodata %q: got %v %v, want %v %v", tc.s, g.HasNoData, g.NoData, tc.has, tc.want)
		}
	}
	// A GDAL_NODATA of another type is an error, not "no nodata": -9999
	// must never be read as an elevation.
	f0 := newFixture(2, 2, vals)
	f0.fields = map[uint16]*fixtureField{tagGDALNoData: {typ: 1, count: 6, data: []byte("-9999\x00")}}
	if g, err := decodeGeoTIFF(f0.bytes()); err == nil {
		t.Fatalf("BYTE nodata accepted: %+v", g)
	}

	// The NUL-terminated value written by the fixture is accepted.
	f := newFixture(2, 2, vals)
	f.fields = map[uint16]*fixtureField{tagGDALNoData: {typ: typeASCII, count: 6, data: []byte("-9999\x00")}}
	if g, err := decodeGeoTIFF(f.bytes()); err != nil || !g.HasNoData || g.NoData != -9999 {
		t.Fatalf("NUL-terminated nodata: %+v %v", g, err)
	}
}

// Georeferencing: GDAL's PixelIsArea tiepoint is the outer corner; a
// PixelIsPoint tiepoint is the first pixel's centre; a tiepoint may name
// any raster position.
func TestDecodeGeoTIFFGeoref(t *testing.T) {
	vals := varied(4, 3)
	area := newFixture(4, 3, vals).georeferenced(575000, 6226500, 10)
	g, err := decodeGeoTIFF(area.bytes())
	if err != nil {
		t.Fatal(err)
	}
	if g.Georef != (Georef{575000, 6226500, 10, 10}) {
		t.Fatalf("area georef %+v", g.Georef)
	}
	if g.Extent() != [4]float64{575000, 6226470, 575040, 6226500} {
		t.Fatalf("extent %v", g.Extent())
	}

	point := newFixture(4, 3, vals).georeferenced(575005, 6226495, 10)
	point.geoKeys = utm32GeoKeys(rasterPixelIsPoint)
	if g, err = decodeGeoTIFF(point.bytes()); err != nil || g.Georef != (Georef{575000, 6226500, 10, 10}) {
		t.Fatalf("point georef %+v %v", g.Georef, err)
	}

	offset := newFixture(4, 3, vals)
	offset.tie = []float64{2, 1, 0, 575020, 6226490, 0}
	offset.scale = []float64{10, 10, 0}
	if g, err = decodeGeoTIFF(offset.bytes()); err != nil || g.Georef != (Georef{575000, 6226500, 10, 10}) {
		t.Fatalf("offset tiepoint %+v %v", g.Georef, err)
	}

	// No GeoKeyDirectory: tiepoint and scale alone are enough.
	bare := newFixture(4, 3, vals).georeferenced(575000, 6226500, 10)
	bare.geoKeys = nil
	if g, err = decodeGeoTIFF(bare.bytes()); err != nil || g.Georef != (Georef{575000, 6226500, 10, 10}) {
		t.Fatalf("no geokeys %+v %v", g.Georef, err)
	}
}

// Georeferencing the sampler cannot use is an error, not a guess.
func TestDecodeGeoTIFFRejectsUnusableGeoref(t *testing.T) {
	vals := varied(4, 3)
	cases := map[string]func(f *tiffFixture){
		"negative scale": func(f *tiffFixture) { f.scale[1] = -10 },
		"zero scale":     func(f *tiffFixture) { f.scale[0] = 0 },
		"NaN origin":     func(f *tiffFixture) { f.tie[3] = math.NaN() },
		"Inf scale":      func(f *tiffFixture) { f.scale[0] = math.Inf(1) },
		"two tiepoints":  func(f *tiffFixture) { f.tie = append(f.tie, 1, 1, 0, 575010, 6226490, 0) },
		"tiepoint only":  func(f *tiffFixture) { f.drop = []uint16{tagModelPixelScale} },
		"scale only":     func(f *tiffFixture) { f.drop = []uint16{tagModelTiepoint} },
		"scale as LONG":  func(f *tiffFixture) { f.fields = map[uint16]*fixtureField{tagModelPixelScale: f.longs(10, 10, 0)} },
		"rotated (matrix)": func(f *tiffFixture) {
			f.fields = map[uint16]*fixtureField{tagModelTransform: f.doublesField(make([]float64, 16)...)}
		},
		"EPSG:4326 model":    func(f *tiffFixture) { f.geoKeys[7] = 2 },
		"EPSG:3857":          func(f *tiffFixture) { f.geoKeys[15] = 3857 },
		"UTM 33":             func(f *tiffFixture) { f.geoKeys[15] = 25833 },
		"raster type 3":      func(f *tiffFixture) { f.geoKeys[11] = 3 },
		"CRS key not inline": func(f *tiffFixture) { f.geoKeys[13] = tagGeoKeyDirectory },
		"keys past the tag":  func(f *tiffFixture) { f.geoKeys[3] = 9 },
		"directory too short": func(f *tiffFixture) {
			f.geoKeys = []uint16{1, 1, 0}
		},
	}
	for name, mutate := range cases {
		f := newFixture(4, 3, vals).georeferenced(575000, 6226500, 10)
		mutate(f)
		if g, err := decodeGeoTIFF(f.bytes()); err == nil {
			t.Errorf("%s: accepted with georef %+v", name, g.Georef)
		}
	}
}

// #745: GeoKeys are checked even without tiepoint and scale. Such a file
// takes the request's grid (rasterGrid), which is only right when it does
// not declare another CRS; one that explicitly says geographic, another
// projection or another raster type was relabelled as the requested UTM
// grid and sampled.
func TestDecodeGeoTIFFChecksGeoKeysWithoutAffine(t *testing.T) {
	vals := varied(4, 3)
	bare := func() *tiffFixture {
		f := newFixture(4, 3, vals)
		f.geoKeys = utm32GeoKeys(rasterPixelIsArea)
		return f
	}
	// Control: UTM 32 keys, or none at all, still take the request's grid.
	for name, f := range map[string]*tiffFixture{"UTM 32 keys": bare(), "no keys": newFixture(4, 3, vals)} {
		if g, err := decodeGeoTIFF(f.bytes()); err != nil || g.Georef != (Georef{}) {
			t.Fatalf("%s without tiepoint and scale: georef %+v, %v", name, g.Georef, err)
		}
	}
	for name, mutate := range map[string]func(f *tiffFixture){
		"geographic model":   func(f *tiffFixture) { f.geoKeys[7] = 2 },
		"EPSG:3857":          func(f *tiffFixture) { f.geoKeys[15] = 3857 },
		"UTM 33":             func(f *tiffFixture) { f.geoKeys[15] = 25833 },
		"raster type 3":      func(f *tiffFixture) { f.geoKeys[11] = 3 },
		"CRS key not inline": func(f *tiffFixture) { f.geoKeys[13] = tagGeoKeyDirectory },
		"keys past the tag":  func(f *tiffFixture) { f.geoKeys[3] = 9 },
		"directory too short": func(f *tiffFixture) {
			f.geoKeys = []uint16{1, 1, 0}
		},
		// The reviewer's file: ModelType geographic, GeographicType 4326.
		"EPSG:4326 lat/lon": func(f *tiffFixture) {
			f.geoKeys = []uint16{1, 1, 0, 2, geoKeyModelType, 0, 1, 2, 2048, 0, 1, 4326}
		},
	} {
		f := bare()
		mutate(f)
		if g, err := decodeGeoTIFF(f.bytes()); err == nil {
			t.Errorf("%s without tiepoint and scale: accepted with georef %+v", name, g.Georef)
		}
	}

	// End to end: the request's UTM grid is never applied to it.
	f := bare()
	f.geoKeys[7] = 2
	r := RasterRequest{Model: ModelTerrain, BBox: [4]float64{580000, 6200000, 580040, 6200030}, Width: 4, Height: 3}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if g, err := rasterGrid(f.bytes(), r); err == nil {
		v, ok := g.Sample(580010, 6200010)
		t.Fatalf("geographic raster sampled as the requested UTM grid: %v, %v", v, ok)
	}
}

// malformedTIFFs are inputs that must be rejected. Each is built from a
// valid fixture with one thing wrong.
func malformedTIFFs() map[string][]byte {
	base := func() *tiffFixture {
		f := newFixture(6, 5, varied(6, 5)).georeferenced(575000, 6226500, 10)
		f.compression, f.predictor, f.rowsPerStr = compressionDeflate, predictorFloatingPoint, 2
		return f
	}
	with := func(mutate func(f *tiffFixture)) []byte { f := base(); mutate(f); return f.bytes() }
	field := func(tag uint16, fl func(f *tiffFixture) *fixtureField) []byte {
		return with(func(f *tiffFixture) { f.fields = map[uint16]*fixtureField{tag: fl(f)} })
	}
	at := func(v uint32) *uint32 { return &v }
	out := map[string][]byte{
		"empty":            nil,
		"junk":             []byte("<html>not a tiff</html>"),
		"BigTIFF":          []byte("II+\x00\x08\x00\x10\x00\x00\x00\x00\x00\x00\x00\x00\x00"),
		"bad magic":        []byte("II\x2b\x00\x08\x00\x00\x00"),
		"mixed order mark": []byte("IM*\x00\x08\x00\x00\x00"),
		"header only":      []byte("II*\x00\x08\x00\x00\x00"),
		"IFD offset 0":     []byte("II*\x00\x00\x00\x00\x00\x00\x00"),
		"IFD past end":     []byte("II*\x00\xf0\xff\xff\xff"),
		"IFD at max u32":   []byte("MM\x00*\xff\xff\xff\xff"),

		// #686 review N2: bit-reversed bytes or a flipped raster would
		// decode to wrong elevations, so anything but 1 is an error.
		"FillOrder 2":       field(tagFillOrder, func(f *tiffFixture) *fixtureField { return f.shorts(2) }),
		"FillOrder 0":       field(tagFillOrder, func(f *tiffFixture) *fixtureField { return f.shorts(0) }),
		"FillOrder x2":      field(tagFillOrder, func(f *tiffFixture) *fixtureField { return f.shorts(1, 1) }),
		"FillOrder ASCII":   field(tagFillOrder, func(f *tiffFixture) *fixtureField { return asciiField("1") }),
		"Orientation 2":     field(tagOrientation, func(f *tiffFixture) *fixtureField { return f.shorts(2) }),
		"Orientation 4":     field(tagOrientation, func(f *tiffFixture) *fixtureField { return f.shorts(4) }),
		"Orientation 8":     field(tagOrientation, func(f *tiffFixture) *fixtureField { return f.shorts(8) }),
		"Orientation 0":     field(tagOrientation, func(f *tiffFixture) *fixtureField { return f.shorts(0) }),
		"Orientation x2":    field(tagOrientation, func(f *tiffFixture) *fixtureField { return f.shorts(1, 1) }),
		"Orientation empty": field(tagOrientation, func(f *tiffFixture) *fixtureField { return &fixtureField{typ: typeShort} }),

		"SampleFormat uint":   field(tagSampleFormat, func(f *tiffFixture) *fixtureField { return f.shorts(1) }),
		"SampleFormat int":    field(tagSampleFormat, func(f *tiffFixture) *fixtureField { return f.shorts(2) }),
		"SampleFormat absent": with(func(f *tiffFixture) { f.drop = []uint16{tagSampleFormat} }),
		"SampleFormat x2":     field(tagSampleFormat, func(f *tiffFixture) *fixtureField { return f.shorts(3, 3) }),
		"16-bit":              field(tagBitsPerSample, func(f *tiffFixture) *fixtureField { return f.shorts(16) }),
		"64-bit":              field(tagBitsPerSample, func(f *tiffFixture) *fixtureField { return f.shorts(64) }),
		"bits absent":         with(func(f *tiffFixture) { f.drop = []uint16{tagBitsPerSample} }),
		"RGB":                 field(tagSamplesPerPixel, func(f *tiffFixture) *fixtureField { return f.shorts(3) }),
		"tiled":               field(tagTileWidth, func(f *tiffFixture) *fixtureField { return f.longs(256) }),
		"LZW":                 field(tagCompression, func(f *tiffFixture) *fixtureField { return f.shorts(5) }),
		"JPEG":                field(tagCompression, func(f *tiffFixture) *fixtureField { return f.shorts(7) }),
		"predictor 4":         field(tagPredictor, func(f *tiffFixture) *fixtureField { return f.shorts(4) }),
		"width as DOUBLE":     field(tagImageWidth, func(f *tiffFixture) *fixtureField { return f.doublesField(6) }),

		"width 0":       field(tagImageWidth, func(f *tiffFixture) *fixtureField { return f.longs(0) }),
		"height absent": with(func(f *tiffFixture) { f.drop = []uint16{tagImageLength} }),
		"width 513":     field(tagImageWidth, func(f *tiffFixture) *fixtureField { return f.longs(MaxRasterSize + 1) }),
		"height 65535":  field(tagImageLength, func(f *tiffFixture) *fixtureField { return f.shorts(65535) }),
		"width 2^32-1":  field(tagImageWidth, func(f *tiffFixture) *fixtureField { return f.longs(math.MaxUint32) }),
		"absurd both": with(func(f *tiffFixture) {
			f.fields = map[uint16]*fixtureField{tagImageWidth: f.longs(1 << 30), tagImageLength: f.longs(1 << 30), tagRowsPerStrip: f.longs(1 << 30)}
		}),

		"strip offset past end": field(tagStripOffsets, func(f *tiffFixture) *fixtureField { return f.longs(0xfffffff0, 8, 8) }),
		"strip count past end":  field(tagStripByteCounts, func(f *tiffFixture) *fixtureField { return f.longs(math.MaxUint32, 1, 1) }),
		"too few strips":        field(tagStripOffsets, func(f *tiffFixture) *fixtureField { return f.longs(8, 8) }),
		"too many strips":       field(tagStripByteCounts, func(f *tiffFixture) *fixtureField { return f.longs(1, 1, 1, 1) }),
		"strip tags absent":     with(func(f *tiffFixture) { f.drop = []uint16{tagStripOffsets, tagStripByteCounts} }),
		"field data past end": field(tagModelTiepoint, func(f *tiffFixture) *fixtureField {
			return &fixtureField{typ: typeDouble, count: 6, raw: at(0xffffff00)}
		}),
		"field count overflow": field(tagModelPixelScale, func(f *tiffFixture) *fixtureField {
			return &fixtureField{typ: typeDouble, count: math.MaxUint32, raw: at(8)}
		}),

		"inflates past its rows": with(func(f *tiffFixture) {
			f.stripHook = func(i int, b []byte) []byte {
				return deflate(append(inflate(b), 0, 0, 0, 0))
			}
		}),
		"inflates short": with(func(f *tiffFixture) {
			f.stripHook = func(i int, b []byte) []byte { raw := inflate(b); return deflate(raw[:len(raw)-1]) }
		}),
		"zlib checksum": with(func(f *tiffFixture) {
			f.stripHook = func(i int, b []byte) []byte { b[len(b)-1] ^= 0xff; return b }
		}),
		"not zlib": with(func(f *tiffFixture) {
			f.stripHook = func(i int, b []byte) []byte { return []byte("plain bytes, not deflate") }
		}),
		"zlib cut short": with(func(f *tiffFixture) {
			f.stripHook = func(i int, b []byte) []byte { return b[:len(b)/2] }
		}),
		"uncompressed strip short": with(func(f *tiffFixture) {
			f.compression = compressionNone
			f.stripHook = func(i int, b []byte) []byte { return b[:len(b)-1] }
		}),
	}
	// One field wrong on an otherwise valid uncompressed file, so nothing
	// else rejects it first.
	plain := func(mutate func(f *tiffFixture)) []byte {
		f := newFixture(6, 5, varied(6, 5))
		f.rowsPerStr = 2
		mutate(f)
		return f.bytes()
	}
	out["width 0, uncompressed"] = plain(func(f *tiffFixture) { f.fields = map[uint16]*fixtureField{tagImageWidth: f.longs(0)} })
	out["height 0, uncompressed"] = plain(func(f *tiffFixture) { f.fields = map[uint16]*fixtureField{tagImageLength: f.longs(0)} })
	out["one strip too many"] = plain(func(f *tiffFixture) { f.extraStrip = true })
	for _, v := range []uint16{0, 43, 0x2a00} {
		b := plain(func(*tiffFixture) {})
		binary.LittleEndian.PutUint16(b[2:], v)
		out[fmt.Sprintf("version %#x on a valid file", v)] = b
	}

	// A deflate bomb: one strip claiming a 512×512 raster, inflating to
	// four times that from a few KiB.
	bomb := newFixture(1, 1, []float32{0})
	bomb.compression = compressionDeflate
	bomb.fields = map[uint16]*fixtureField{tagImageWidth: bomb.longs(MaxRasterSize), tagImageLength: bomb.longs(MaxRasterSize), tagRowsPerStrip: bomb.longs(MaxRasterSize)}
	bomb.stripHook = func(int, []byte) []byte { return bombStream() }
	out["deflate bomb"] = bomb.bytes()
	// A 512×512 uncompressed claim backed by 8 bytes.
	empty := newFixture(1, 2, []float32{0, 0})
	empty.fields = map[uint16]*fixtureField{tagImageWidth: empty.longs(MaxRasterSize), tagImageLength: empty.longs(MaxRasterSize), tagRowsPerStrip: empty.longs(MaxRasterSize)}
	out["uncompressed 512² from 8 bytes"] = empty.bytes()
	return out
}

// bombStream is 4 MiB of zeros deflated to a few KiB: four times a
// MaxRasterSize² float32 raster.
var bombStream = sync.OnceValue(func() []byte { return deflate(make([]byte, 4<<20)) })

func inflate(b []byte) []byte {
	g, err := decodeRawZlib(b)
	if err != nil {
		panic(err)
	}
	return g
}

// Every malformed input is an error, never a panic.
func TestDecodeGeoTIFFRejectsMalformed(t *testing.T) {
	for name, b := range malformedTIFFs() {
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("%s: panic %v", name, p)
				}
			}()
			if g, err := decodeGeoTIFF(b); err == nil {
				t.Errorf("%s: accepted as %dx%d", name, g.Width, g.Height)
			}
		}()
	}
}

// FillOrder and Orientation set explicitly to their default 1 (the only
// values the decoder reads correctly) decode like a file without them.
func TestDecodeGeoTIFFFillOrderAndOrientationDefault(t *testing.T) {
	plain := newFixture(3, 2, varied(3, 2)).georeferenced(575000, 6226500, 10)
	want, err := decodeGeoTIFF(plain.bytes())
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		f := newFixture(3, 2, varied(3, 2)).georeferenced(575000, 6226500, 10)
		f.order = order
		f.fields = map[uint16]*fixtureField{tagFillOrder: f.shorts(1), tagOrientation: f.shorts(1)}
		g, err := decodeGeoTIFF(f.bytes())
		if err != nil {
			t.Fatalf("%v: explicit FillOrder/Orientation 1: %v", order, err)
		}
		if !slices.Equal(g.Values, want.Values) || g.Georef != want.Georef {
			t.Fatalf("%v: decoded %v %+v, want %v %+v", order, g.Values, g.Georef, want.Values, want.Georef)
		}
	}
}

// Every truncation of a valid file is an error, never a panic.
func TestDecodeGeoTIFFEveryTruncation(t *testing.T) {
	for _, comp := range []uint16{compressionNone, compressionDeflate} {
		f := newFixture(6, 5, varied(6, 5)).georeferenced(575000, 6226500, 10)
		f.compression, f.rowsPerStr, f.noData = comp, 2, "-9999"
		b := f.bytes()
		if _, err := decodeGeoTIFF(b); err != nil {
			t.Fatalf("full file: %v", err)
		}
		for n := 0; n < len(b); n++ {
			if _, err := decodeGeoTIFF(b[:n:n]); err == nil {
				t.Fatalf("compression %d: %d of %d bytes accepted", comp, n, len(b))
			}
		}
	}
}

// The only size read from the file that drives allocation is the raster's,
// and it is checked against MaxRasterSize first: a header that claims more,
// or strips outside the buffer, costs at most 16 KiB, and nothing ever costs
// more than one MaxRasterSize² raster. The IFD's entry count does not drive
// allocation either; see TestDecodeGeoTIFFManyIFDEntriesAllocationIsBounded.
func TestDecodeGeoTIFFAllocationIsBounded(t *testing.T) {
	small := []string{"width 513", "height 65535", "width 2^32-1", "absurd both",
		"strip offset past end", "strip count past end", "field count overflow", "uncompressed 512² from 8 bytes"}
	all := malformedTIFFs()
	for _, name := range small {
		if n := allocBytes(all[name]); n > 16<<10 {
			t.Errorf("%s: %d bytes allocated", name, n)
		}
	}
	bound := uint64(MaxRasterSize*MaxRasterSize*4 + 128<<10)
	for name, b := range all {
		if n := allocBytes(b); n > bound {
			t.Errorf("%s: %d bytes allocated, over one raster", name, n)
		}
	}
}

// #686 review N1: the IFD's entry count comes from the file. A valid file
// with every distinct tag that leaves the decode unchanged (65 530 entries;
// the other six tags change the result) decodes with the same small
// allocation as a plain one.
func TestDecodeGeoTIFFManyIFDEntriesAllocationIsBounded(t *testing.T) {
	f := newFixture(2, 2, []float32{1, 2, 3, 4}).georeferenced(575000, 6226500, 10)
	plain := f.bytes()
	ifdEntries := func(b []byte) int {
		return int(binary.LittleEndian.Uint16(b[binary.LittleEndian.Uint32(b[4:]):]))
	}
	// Add every other tag, except those that change the decode: Predictor,
	// TileWidth, ModelTransformation, GDAL_NODATA, FillOrder 266 and
	// Orientation 274. Tags the file already has are overwritten unchanged.
	f.fields = map[uint16]*fixtureField{}
	skip := map[int]bool{tagPredictor: true, tagTileWidth: true, tagModelTransform: true, tagGDALNoData: true, 266: true, 274: true}
	for tag := 0; tag <= 0xffff; tag++ {
		if !skip[tag] {
			f.fields[uint16(tag)] = f.shorts(7)
		}
	}
	for _, tag := range []uint16{tagImageWidth, tagImageLength, tagBitsPerSample, tagCompression, tagStripOffsets,
		tagSamplesPerPixel, tagRowsPerStrip, tagStripByteCounts, tagSampleFormat, tagModelTiepoint,
		tagModelPixelScale, tagGeoKeyDirectory, 262} {
		delete(f.fields, tag)
	}
	b := f.bytes()
	if n := ifdEntries(b); n != 65530 {
		t.Fatalf("fixture has %d IFD entries", n)
	}
	g, err := decodeGeoTIFF(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !slices.Equal(g.Values, []float32{1, 2, 3, 4}) || g.Georef != (Georef{575000, 6226500, 10, 10}) {
		t.Fatalf("decoded %v %+v", g.Values, g.Georef)
	}
	base := allocBytes(plain)
	if n := allocBytes(b); n > base+4<<10 {
		t.Fatalf("%d bytes allocated for a %d-byte file with 65 530 entries; a plain file takes %d", n, len(b), base)
	}
}

// allocBytes is the heap allocated by one decode, averaged over a few runs.
func allocBytes(b []byte) uint64 {
	const runs = 5
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < runs; i++ {
		decodeGeoTIFF(b)
	}
	runtime.ReadMemStats(&after)
	return (after.TotalAlloc - before.TotalAlloc) / runs
}

// The decoder accepts exactly MaxRasterSize per edge.
func TestDecodeGeoTIFFMaxSize(t *testing.T) {
	f := newFixture(MaxRasterSize, MaxRasterSize, make([]float32, MaxRasterSize*MaxRasterSize))
	f.compression, f.predictor = compressionDeflate, predictorFloatingPoint
	g, err := decodeGeoTIFF(f.bytes())
	if err != nil || g.Width != MaxRasterSize || g.Height != MaxRasterSize {
		t.Fatalf("%dx%d %v", g.Width, g.Height, err)
	}
}

// FuzzDecodeGeoTIFF: no input panics, and an accepted one is always a
// bounded, consistent grid that the sampler can read.
func FuzzDecodeGeoTIFF(f *testing.F) {
	for _, comp := range []uint16{compressionNone, compressionDeflate} {
		for _, pred := range []uint16{0, predictorHorizontal, predictorFloatingPoint} {
			for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
				fx := newFixture(5, 4, varied(5, 4)).georeferenced(575000, 6226500, 10)
				fx.order, fx.compression, fx.predictor, fx.rowsPerStr, fx.noData = order, comp, pred, 3, "-9999"
				f.Add(fx.bytes())
			}
		}
	}
	f.Add(handBuiltBigEndian)
	// No georeferencing: decodes, but must never yield a sample.
	f.Add(newFixture(2, 2, []float32{1, 2, 3, 4}).bytes())
	for _, b := range malformedTIFFs() {
		if len(b) < 4096 {
			f.Add(b)
		}
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		g, err := decodeGeoTIFF(b)
		if err != nil {
			return
		}
		if g.Width < 1 || g.Height < 1 || g.Width > MaxRasterSize || g.Height > MaxRasterSize || len(g.Values) != g.Width*g.Height {
			t.Fatalf("inconsistent grid %dx%d with %d values", g.Width, g.Height, len(g.Values))
		}
		if g.Georef != (Georef{}) && !g.Georef.valid() {
			t.Fatalf("invalid georef %+v", g.Georef)
		}
		// Sample the decoded grid as it is, also without georeferencing
		// (#686 review F2): a sample is either unknown or a finite value,
		// and a grid without a valid Georef has none at all.
		x := g.Extent()
		for _, p := range [][2]float64{
			{(x[0] + x[2]) / 2, (x[1] + x[3]) / 2}, {x[0], x[1]}, {x[2], x[3]}, {0, 0},
		} {
			v, ok := g.Sample(p[0], p[1])
			if ok && (math.IsNaN(v) || math.IsInf(v, 0)) {
				t.Fatalf("Sample(%v, %v) = %v, true", p[0], p[1], v)
			}
			if ok && !g.Georef.valid() {
				t.Fatalf("Sample(%v, %v) = %v, true without georeferencing", p[0], p[1], v)
			}
		}
	})
}

func benchFixture(comp, pred uint16) []byte {
	const n = 180
	f := newFixture(n, n, ramp(n, n, func(c, r int) float32 { return 20 + float32(c)*0.05 - float32(r)*0.03 + float32((c*r)%7)*0.01 }))
	f.georeferenced(575000, 6226500, 10)
	// GDAL's default strip is about 8 KiB: 11 rows of 180 float32.
	f.compression, f.predictor, f.rowsPerStr, f.noData = comp, pred, 11, "-9999"
	return f.bytes()
}

// The plan's default chunk: 180×180 float32 (§1.3, ≤179 px per edge).
func BenchmarkDecodeGeoTIFF180Deflate(b *testing.B) {
	data := benchFixture(compressionDeflate, predictorFloatingPoint)
	b.SetBytes(180 * 180 * 4)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := decodeGeoTIFF(data); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeGeoTIFF180Uncompressed(b *testing.B) {
	data := benchFixture(compressionNone, 0)
	b.SetBytes(180 * 180 * 4)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := decodeGeoTIFF(data); err != nil {
			b.Fatal(err)
		}
	}
}
