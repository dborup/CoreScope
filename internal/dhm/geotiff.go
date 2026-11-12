package dhm

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
)

// #681 M2 — a minimal reader for the GeoTIFF the DHM WCS returns.
//
// golang.org/x/image/tiff refuses floating-point samples, and the owner
// chose a stdlib-only reader over a new dependency (#681 decision 3). This
// reads exactly one shape: classic TIFF (either byte order), the first IFD,
// one 32-bit IEEE float sample per pixel, strips, no compression or
// DEFLATE, predictor none, horizontal or floating point, and FillOrder and
// Orientation absent or 1. Everything else is an error.
//
// The input comes from the network, so every size is checked before it is
// used: the raster is at most MaxRasterSize per edge before anything is
// allocated, every offset and length is checked against the buffer, and a
// strip that inflates to more than its expected size is an error. No
// allocation depends on a size read from the file except the bounded
// raster itself: the IFD keeps only the fixed set of tags the decoder
// reads, however many entries the file lists.

// errBadTIFF is every decode failure. Its text is fixed, so it never
// carries file content.
var errBadTIFF = errors.New("unsupported or malformed GeoTIFF")

// TIFF tags read by the decoder.
const (
	tagImageWidth      = 256
	tagImageLength     = 257
	tagBitsPerSample   = 258
	tagCompression     = 259
	tagFillOrder       = 266
	tagStripOffsets    = 273
	tagOrientation     = 274
	tagSamplesPerPixel = 277
	tagRowsPerStrip    = 278
	tagStripByteCounts = 279
	tagPredictor       = 317
	tagTileWidth       = 322
	tagSampleFormat    = 339
	tagModelPixelScale = 33550
	tagModelTiepoint   = 33922
	tagModelTransform  = 34264
	tagGeoKeyDirectory = 34735
	tagGDALNoData      = 42113
)

// readTags are the tags readIFD keeps; every other entry is skipped, so
// the field map never grows with the file's entry count (#686 review N1).
var readTags = map[uint16]bool{
	tagImageWidth: true, tagImageLength: true, tagBitsPerSample: true, tagCompression: true,
	tagFillOrder: true, tagOrientation: true,
	tagStripOffsets: true, tagSamplesPerPixel: true, tagRowsPerStrip: true, tagStripByteCounts: true,
	tagPredictor: true, tagTileWidth: true, tagSampleFormat: true, tagModelPixelScale: true,
	tagModelTiepoint: true, tagModelTransform: true, tagGeoKeyDirectory: true, tagGDALNoData: true,
}

// TIFF field types.
const (
	typeASCII  = 2
	typeShort  = 3
	typeLong   = 4
	typeDouble = 12
)

const (
	compressionNone        = 1
	compressionDeflate     = 8     // Adobe
	compressionDeflateOld  = 32946 // PKZIP-style, same zlib stream
	predictorNone          = 1
	predictorHorizontal    = 2
	predictorFloatingPoint = 3
	sampleFormatFloat      = 3

	maxNoDataLen = 64 // GDAL writes e.g. "-9999" or "-3.4028234663852886e+38"

	// GeoKeys.
	geoKeyModelType    = 1024
	geoKeyRasterType   = 1025
	geoKeyProjectedCS  = 3072
	modelTypeProjected = 1
	rasterPixelIsArea  = 1
	rasterPixelIsPoint = 2
	epsgUTM32          = 25832
)

// tiffField is one IFD entry. Its data has been bounds-checked against the
// file: data is the field's value bytes, count elements of type typ (nil
// for a type the decoder never reads).
type tiffField struct {
	typ   uint16
	count uint32
	data  []byte
}

func typeSize(typ uint16) int {
	switch typ {
	case typeASCII:
		return 1
	case typeShort:
		return 2
	case typeLong:
		return 4
	case typeDouble:
		return 8
	}
	return 0 // a type the decoder never reads
}

type tiffReader struct {
	buf    []byte
	order  binary.ByteOrder
	fields map[uint16]tiffField
}

// decodeGeoTIFF decodes a DHM GeoTIFF. The returned Grid carries the
// file's georeferencing when it has one; otherwise Georef is the zero
// value and the caller supplies it.
func decodeGeoTIFF(buf []byte) (Grid, error) {
	r := &tiffReader{buf: buf}
	if err := r.readIFD(); err != nil {
		return Grid{}, err
	}
	w, err := r.uint(tagImageWidth, 0, false)
	if err != nil {
		return Grid{}, err
	}
	h, err := r.uint(tagImageLength, 0, false)
	if err != nil {
		return Grid{}, err
	}
	if w < 1 || h < 1 || w > MaxRasterSize || h > MaxRasterSize {
		return Grid{}, errBadTIFF
	}
	for _, want := range []struct {
		tag  uint16
		def  uint32
		only uint32
	}{
		{tagBitsPerSample, 0, 32},
		{tagSampleFormat, 1, sampleFormatFloat},
		{tagSamplesPerPixel, 1, 1},
		// #686 review N2: bit order and row/column order other than the
		// TIFF default would decode to wrong values, so they are errors.
		{tagFillOrder, 1, 1},
		{tagOrientation, 1, 1},
	} {
		v, err := r.uint(want.tag, want.def, true)
		if err != nil || v != want.only {
			return Grid{}, errBadTIFF
		}
		if f, ok := r.fields[want.tag]; ok && f.count != 1 {
			return Grid{}, errBadTIFF
		}
	}
	if _, tiled := r.fields[tagTileWidth]; tiled {
		return Grid{}, errBadTIFF
	}
	compression, err := r.uint(tagCompression, compressionNone, true)
	if err != nil {
		return Grid{}, err
	}
	predictor, err := r.uint(tagPredictor, predictorNone, true)
	if err != nil {
		return Grid{}, err
	}
	if predictor != predictorNone && predictor != predictorHorizontal && predictor != predictorFloatingPoint {
		return Grid{}, errBadTIFF
	}

	g := Grid{Width: int(w), Height: int(h)}
	if g.NoData, g.HasNoData, err = r.noData(); err != nil {
		return Grid{}, err
	}
	if g.Georef, err = r.georef(); err != nil {
		return Grid{}, err
	}
	if g.Values, err = r.strips(g.Width, g.Height, compression, predictor); err != nil {
		return Grid{}, err
	}
	return g, nil
}

// readIFD reads the header and the first IFD. Later IFDs (overviews,
// masks) are ignored.
func (r *tiffReader) readIFD() error {
	b := r.buf
	switch {
	case len(b) >= 8 && b[0] == 'I' && b[1] == 'I':
		r.order = binary.LittleEndian
	case len(b) >= 8 && b[0] == 'M' && b[1] == 'M':
		r.order = binary.BigEndian
	default:
		return errBadTIFF
	}
	if r.order.Uint16(b[2:]) != 42 { // 43 is BigTIFF
		return errBadTIFF
	}
	off := uint64(r.order.Uint32(b[4:]))
	if off < 8 || off+2 > uint64(len(b)) {
		return errBadTIFF
	}
	n := uint64(r.order.Uint16(b[off:]))
	if off+2+n*12 > uint64(len(b)) {
		return errBadTIFF
	}
	r.fields = make(map[uint16]tiffField, len(readTags))
	for i := uint64(0); i < n; i++ {
		e := b[off+2+i*12 : off+2+i*12+12]
		tag, typ, count := r.order.Uint16(e), r.order.Uint16(e[2:]), r.order.Uint32(e[4:])
		if !readTags[tag] {
			continue
		}
		size := typeSize(typ)
		if size == 0 {
			// A type the decoder never reads: kept without data, so a tag
			// it needs fails its type check instead of looking absent.
			r.fields[tag] = tiffField{typ: typ, count: count}
			continue
		}
		total := uint64(count) * uint64(size)
		var data []byte
		if total <= 4 {
			data = e[8 : 8+total]
		} else {
			at := uint64(r.order.Uint32(e[8:]))
			if at+total > uint64(len(b)) {
				return errBadTIFF
			}
			data = b[at : at+total]
		}
		r.fields[tag] = tiffField{typ: typ, count: count, data: data}
	}
	return nil
}

// at returns element i of a SHORT or LONG field.
func (r *tiffReader) at(f tiffField, i int) (uint32, bool) {
	if i < 0 || uint64(i) >= uint64(f.count) {
		return 0, false
	}
	switch f.typ {
	case typeShort:
		return uint32(r.order.Uint16(f.data[2*i:])), true
	case typeLong:
		return r.order.Uint32(f.data[4*i:]), true
	}
	return 0, false
}

// uint returns the first value of a SHORT or LONG tag, or def when the tag
// is absent and optional.
func (r *tiffReader) uint(tag uint16, def uint32, optional bool) (uint32, error) {
	f, ok := r.fields[tag]
	if !ok {
		if optional {
			return def, nil
		}
		return 0, errBadTIFF
	}
	v, ok := r.at(f, 0)
	if !ok {
		return 0, errBadTIFF
	}
	return v, nil
}

// doubles returns a DOUBLE tag that has exactly n values.
func (r *tiffReader) doubles(tag uint16, n int) ([]float64, bool) {
	f, ok := r.fields[tag]
	if !ok || f.typ != typeDouble || f.count != uint32(n) {
		return nil, false
	}
	out := make([]float64, n)
	for i := range out {
		out[i] = math.Float64frombits(r.order.Uint64(f.data[8*i:]))
	}
	return out, true
}

// noData reads GDAL_NODATA, an ASCII number.
func (r *tiffReader) noData() (float32, bool, error) {
	f, ok := r.fields[tagGDALNoData]
	if !ok {
		return 0, false, nil
	}
	if f.typ != typeASCII || f.count > maxNoDataLen {
		return 0, false, errBadTIFF
	}
	s := strings.TrimSpace(strings.TrimRight(string(f.data), "\x00"))
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false, errBadTIFF
	}
	return float32(v), true, nil
}

// georef reads ModelTiepoint + ModelPixelScale and the GeoKeys that change
// their meaning. Absent georeferencing is the zero Georef; a rotated
// (ModelTransformation) or non-UTM32 raster is an error.
func (r *tiffReader) georef() (Georef, error) {
	if _, ok := r.fields[tagModelTransform]; ok {
		return Georef{}, errBadTIFF
	}
	_, tiePresent := r.fields[tagModelTiepoint]
	_, scalePresent := r.fields[tagModelPixelScale]
	if !tiePresent && !scalePresent {
		return Georef{}, nil
	}
	// One tiepoint and one scale: several tiepoints would mean a warped
	// raster.
	tie, hasTie := r.doubles(tagModelTiepoint, 6)
	scale, hasScale := r.doubles(tagModelPixelScale, 3)
	if !hasTie || !hasScale {
		return Georef{}, errBadTIFF
	}
	pointOrigin, err := r.geoKeys()
	if err != nil {
		return Georef{}, err
	}
	g := Georef{PixelE: scale[0], PixelN: scale[1]}
	// Tiepoint (I, J, K) → (X, Y, Z): raster position I, J sits at X, Y.
	g.OriginE = tie[3] - tie[0]*g.PixelE
	g.OriginN = tie[4] + tie[1]*g.PixelN
	if pointOrigin {
		// PixelIsPoint: raster position (0, 0) is the centre of the first
		// pixel, not its corner.
		g.OriginE -= g.PixelE / 2
		g.OriginN += g.PixelN / 2
	}
	if !g.valid() {
		return Georef{}, errBadTIFF
	}
	return g, nil
}

// geoKeys checks the GeoKeyDirectory, when present: the model must be
// projected and EPSG:25832, and the raster type area or point. It reports
// whether the raster is PixelIsPoint.
func (r *tiffReader) geoKeys() (bool, error) {
	f, ok := r.fields[tagGeoKeyDirectory]
	if !ok {
		return false, nil
	}
	if f.typ != typeShort || f.count < 4 {
		return false, errBadTIFF
	}
	nKeys, _ := r.at(f, 3)
	if uint64(f.count) < 4+4*uint64(nKeys) {
		return false, errBadTIFF
	}
	point := false
	for k := 0; k < int(nKeys); k++ {
		id, _ := r.at(f, 4+4*k)
		loc, _ := r.at(f, 5+4*k)
		val, _ := r.at(f, 7+4*k)
		inline := loc == 0
		switch id {
		case geoKeyModelType:
			if !inline || val != modelTypeProjected {
				return false, errBadTIFF
			}
		case geoKeyProjectedCS:
			if !inline || val != epsgUTM32 {
				return false, errBadTIFF
			}
		case geoKeyRasterType:
			if !inline || (val != rasterPixelIsArea && val != rasterPixelIsPoint) {
				return false, errBadTIFF
			}
			point = val == rasterPixelIsPoint
		}
	}
	return point, nil
}

// strips decodes all strips into a w×h raster.
func (r *tiffReader) strips(w, h int, compression, predictor uint32) ([]float32, error) {
	if compression != compressionNone && compression != compressionDeflate && compression != compressionDeflateOld {
		return nil, errBadTIFF
	}
	rps, err := r.uint(tagRowsPerStrip, uint32(h), true)
	if err != nil {
		return nil, err
	}
	if rps == 0 || rps > uint32(h) {
		rps = uint32(h)
	}
	nStrips := (h + int(rps) - 1) / int(rps)
	offsets, okO := r.fields[tagStripOffsets]
	counts, okC := r.fields[tagStripByteCounts]
	if !okO || !okC || offsets.count != uint32(nStrips) || counts.count != uint32(nStrips) {
		return nil, errBadTIFF
	}

	// Every strip must lie inside the buffer, and an uncompressed one must
	// hold its rows, before the raster is allocated.
	rowBytes := w * 4
	for i := 0; i < nStrips; i++ {
		if _, err := r.strip(offsets, counts, i); err != nil {
			return nil, err
		}
		rows := min(int(rps), h-i*int(rps))
		if c, _ := r.at(counts, i); compression == compressionNone && uint64(c) < uint64(rows*rowBytes) {
			return nil, errBadTIFF
		}
	}

	out := make([]float32, w*h)
	raw := make([]byte, rowBytes)
	var tmp []byte
	if predictor == predictorFloatingPoint {
		tmp = make([]byte, rowBytes)
	}
	var zr io.ReadCloser
	for i := 0; i < nStrips; i++ {
		src, _ := r.strip(offsets, counts, i)
		rows := min(int(rps), h-i*int(rps))
		var in io.Reader = bytes.NewReader(src)
		if compression != compressionNone {
			if zr, err = resetZlib(zr, src); err != nil {
				return nil, errBadTIFF
			}
			in = zr
		}
		for y := i * int(rps); y < i*int(rps)+rows; y++ {
			if _, err := io.ReadFull(in, raw); err != nil {
				return nil, errBadTIFF
			}
			r.decodeRow(out[y*w:(y+1)*w], raw, predictor, tmp)
		}
		if compression != compressionNone {
			// The stream must end exactly here, with a valid checksum:
			// a strip never inflates past its rows.
			var extra [1]byte
			if k, err := zr.Read(extra[:]); k != 0 || err != io.EOF {
				return nil, errBadTIFF
			}
		}
	}
	return out, nil
}

// strip returns the stored bytes of strip i, checked against the buffer.
func (r *tiffReader) strip(offsets, counts tiffField, i int) ([]byte, error) {
	off, ok1 := r.at(offsets, i)
	n, ok2 := r.at(counts, i)
	if !ok1 || !ok2 || uint64(off)+uint64(n) > uint64(len(r.buf)) {
		return nil, errBadTIFF
	}
	return r.buf[off : uint64(off)+uint64(n)], nil
}

// resetZlib reuses one inflater across strips.
func resetZlib(zr io.ReadCloser, src []byte) (io.ReadCloser, error) {
	if zr == nil {
		return zlib.NewReader(bytes.NewReader(src))
	}
	return zr, zr.(zlib.Resetter).Reset(bytes.NewReader(src), nil)
}

// decodeRow undoes the predictor on one row of raw bytes and stores the
// floats. tmp is a row-sized scratch buffer for the floating-point
// predictor.
func (r *tiffReader) decodeRow(dst []float32, src []byte, predictor uint32, tmp []byte) {
	w := len(dst)
	switch predictor {
	case predictorHorizontal:
		// Differences of the 32-bit words, in file byte order (libtiff
		// horAcc32).
		var prev uint32
		for i := range dst {
			prev += r.order.Uint32(src[4*i:])
			dst[i] = math.Float32frombits(prev)
		}
	case predictorFloatingPoint:
		// libtiff fpAcc: undo a byte-wise difference over the whole row,
		// then regather each float from four byte planes stored most
		// significant first, whatever the file byte order.
		var acc byte
		for i, b := range src {
			acc += b
			tmp[i] = acc
		}
		for i := range dst {
			bits := uint32(tmp[i])<<24 | uint32(tmp[w+i])<<16 | uint32(tmp[2*w+i])<<8 | uint32(tmp[3*w+i])
			dst[i] = math.Float32frombits(bits)
		}
	default:
		for i := range dst {
			dst[i] = math.Float32frombits(r.order.Uint32(src[4*i:]))
		}
	}
}
