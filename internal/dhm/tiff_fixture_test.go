package dhm

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"io"
	"math"
	"sort"
)

// A tiny TIFF writer for in-test golden fixtures. It writes what GDAL's
// GTiff driver writes for a single-band float32 raster: header, strip
// data, then one IFD with its out-of-line values. Fields can be replaced
// or added to build malformed files.

type fixtureField struct {
	typ   uint16
	count uint32
	data  []byte // already in the fixture's byte order; nil = count×size zero bytes
	// raw sets the 4-byte value/offset slot verbatim instead of laying
	// data out, to point a field anywhere.
	raw *uint32
}

type tiffFixture struct {
	order       binary.ByteOrder
	width       int
	height      int
	values      []float32
	compression uint16 // 1 or 8
	predictor   uint16 // 0: tag omitted
	rowsPerStr  int    // 0: the whole image in one strip
	noData      string // "": tag omitted
	// tiepoint + scale; nil: no georeferencing
	tie, scale []float64
	geoKeys    []uint16 // GeoKeyDirectory; nil: omitted
	fields     map[uint16]*fixtureField
	drop       []uint16 // tags to omit
	// stripHook rewrites the stored bytes of strip i (after compression).
	stripHook func(i int, b []byte) []byte
	// extraStrip lists one strip more than the image has.
	extraStrip bool
}

func newFixture(w, h int, vals []float32) *tiffFixture {
	return &tiffFixture{order: binary.LittleEndian, width: w, height: h, values: vals, compression: 1}
}

func ramp(w, h int, f func(col, row int) float32) []float32 {
	v := make([]float32, w*h)
	for row := 0; row < h; row++ {
		for col := 0; col < w; col++ {
			v[row*w+col] = f(col, row)
		}
	}
	return v
}

func (f *tiffFixture) shorts(vs ...uint16) *fixtureField {
	b := make([]byte, 2*len(vs))
	for i, v := range vs {
		f.order.PutUint16(b[2*i:], v)
	}
	return &fixtureField{typ: typeShort, count: uint32(len(vs)), data: b}
}

func (f *tiffFixture) longs(vs ...uint32) *fixtureField {
	b := make([]byte, 4*len(vs))
	for i, v := range vs {
		f.order.PutUint32(b[4*i:], v)
	}
	return &fixtureField{typ: typeLong, count: uint32(len(vs)), data: b}
}

func (f *tiffFixture) doublesField(vs ...float64) *fixtureField {
	b := make([]byte, 8*len(vs))
	for i, v := range vs {
		f.order.PutUint64(b[8*i:], math.Float64bits(v))
	}
	return &fixtureField{typ: typeDouble, count: uint32(len(vs)), data: b}
}

func asciiField(s string) *fixtureField {
	b := append([]byte(s), 0)
	return &fixtureField{typ: typeASCII, count: uint32(len(b)), data: b}
}

// encodeRow applies the predictor to one row of floats and returns its
// stored bytes.
func (f *tiffFixture) encodeRow(vals []float32) []byte {
	w := len(vals)
	out := make([]byte, 4*w)
	switch f.predictor {
	case predictorHorizontal:
		var prev uint32
		for i, v := range vals {
			bits := math.Float32bits(v)
			f.order.PutUint32(out[4*i:], bits-prev)
			prev = bits
		}
	case predictorFloatingPoint:
		// libtiff fpDiff: byte planes, most significant first, then a
		// byte-wise difference over the row (from the end backwards).
		for i, v := range vals {
			bits := math.Float32bits(v)
			out[i] = byte(bits >> 24)
			out[w+i] = byte(bits >> 16)
			out[2*w+i] = byte(bits >> 8)
			out[3*w+i] = byte(bits)
		}
		for i := len(out) - 1; i > 0; i-- {
			out[i] -= out[i-1]
		}
	default:
		for i, v := range vals {
			f.order.PutUint32(out[4*i:], math.Float32bits(v))
		}
	}
	return out
}

func deflate(b []byte) []byte {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}

func decodeRawZlib(b []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(zr)
}

func (f *tiffFixture) bytes() []byte {
	rps := f.rowsPerStr
	if rps <= 0 || rps > f.height {
		rps = f.height
	}
	var strips [][]byte
	for y := 0; y < f.height; y += rps {
		var raw []byte
		for r := y; r < y+rps && r < f.height; r++ {
			raw = append(raw, f.encodeRow(f.values[r*f.width:(r+1)*f.width])...)
		}
		if f.compression == compressionDeflate {
			raw = deflate(raw)
		}
		if f.stripHook != nil {
			raw = f.stripHook(len(strips), raw)
		}
		strips = append(strips, raw)
	}

	var out bytes.Buffer
	if f.order == binary.LittleEndian {
		out.WriteString("II*\x00")
	} else {
		out.WriteString("MM\x00*")
	}
	out.Write(make([]byte, 4)) // IFD offset, patched below
	offsets := make([]uint32, len(strips))
	counts := make([]uint32, len(strips))
	for i, s := range strips {
		offsets[i] = uint32(out.Len())
		counts[i] = uint32(len(s))
		out.Write(s)
	}

	if f.extraStrip {
		offsets = append(offsets, offsets[len(offsets)-1])
		counts = append(counts, counts[len(counts)-1])
	}
	fields := map[uint16]*fixtureField{
		tagImageWidth:      f.longs(uint32(f.width)),
		tagImageLength:     f.longs(uint32(f.height)),
		tagBitsPerSample:   f.shorts(32),
		tagCompression:     f.shorts(f.compression),
		262:                f.shorts(1), // PhotometricInterpretation: BlackIsZero
		tagStripOffsets:    f.longs(offsets...),
		tagSamplesPerPixel: f.shorts(1),
		tagRowsPerStrip:    f.longs(uint32(rps)),
		tagStripByteCounts: f.longs(counts...),
		tagSampleFormat:    f.shorts(sampleFormatFloat),
	}
	if f.predictor != 0 {
		fields[tagPredictor] = f.shorts(f.predictor)
	}
	if f.noData != "" {
		fields[tagGDALNoData] = asciiField(f.noData)
	}
	if f.tie != nil {
		fields[tagModelTiepoint] = f.doublesField(f.tie...)
		fields[tagModelPixelScale] = f.doublesField(f.scale...)
	}
	if f.geoKeys != nil {
		fields[tagGeoKeyDirectory] = f.shorts(f.geoKeys...)
	}
	for tag, v := range f.fields {
		fields[tag] = v
	}
	for _, tag := range f.drop {
		delete(fields, tag)
	}
	tags := make([]int, 0, len(fields))
	for tag := range fields {
		tags = append(tags, int(tag))
	}
	sort.Ints(tags)

	if out.Len()%2 == 1 {
		out.WriteByte(0)
	}
	ifd := uint32(out.Len())
	f.order.PutUint32(out.Bytes()[4:], ifd)
	extra := ifd + 2 + uint32(12*len(tags)) + 4
	var entries, tail bytes.Buffer
	for _, tag := range tags {
		fl := fields[uint16(tag)]
		data := fl.data
		if data == nil && fl.raw == nil {
			data = make([]byte, int(fl.count)*max(typeSize(fl.typ), 1))
		}
		e := make([]byte, 12)
		f.order.PutUint16(e, uint16(tag))
		f.order.PutUint16(e[2:], fl.typ)
		f.order.PutUint32(e[4:], fl.count)
		switch {
		case fl.raw != nil:
			f.order.PutUint32(e[8:], *fl.raw)
		case len(data) <= 4:
			copy(e[8:], data)
		default:
			f.order.PutUint32(e[8:], extra+uint32(tail.Len()))
			tail.Write(data)
			if tail.Len()%2 == 1 {
				tail.WriteByte(0)
			}
		}
		entries.Write(e)
	}
	var n [2]byte
	f.order.PutUint16(n[:], uint16(len(tags)))
	out.Write(n[:])
	out.Write(entries.Bytes())
	out.Write(make([]byte, 4)) // no next IFD
	out.Write(tail.Bytes())
	return out.Bytes()
}

// utm32GeoKeys is the GeoKeyDirectory GDAL writes for EPSG:25832:
// projected model, PixelIsArea (or point), ProjectedCSType 25832.
func utm32GeoKeys(rasterType uint16) []uint16 {
	return []uint16{
		1, 1, 0, 3,
		geoKeyModelType, 0, 1, modelTypeProjected,
		geoKeyRasterType, 0, 1, rasterType,
		geoKeyProjectedCS, 0, 1, epsgUTM32,
	}
}

// georeferenced sets the tiepoint and scale for a grid whose outer
// north-west corner is (e, n) with square pixels of size px, the way GDAL
// writes it for PixelIsArea.
func (f *tiffFixture) georeferenced(e, n, px float64) *tiffFixture {
	f.tie = []float64{0, 0, 0, e, n, 0}
	f.scale = []float64{px, px, 0}
	f.geoKeys = utm32GeoKeys(rasterPixelIsArea)
	return f
}
