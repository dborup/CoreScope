// Package dhmtest builds DHM-shaped rasters for tests outside package dhm:
// the fake WCS upstreams in cmd/server and the profile benchmarks. Nothing
// in it touches the network.
package dhmtest

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"math"
	"strconv"
)

// GeoTIFF is a little-endian, single-strip, 32-bit float TIFF of w × h
// values without georeferencing, which dhm.WCSSource places on the
// requested bbox. A non-nil noData adds the GDAL_NODATA tag; deflate
// compresses the strip the way DHM does.
func GeoTIFF(w, h int, values []float32, noData *float32, deflate bool) []byte {
	raw := make([]byte, 4*len(values))
	for i, v := range values {
		binary.LittleEndian.PutUint32(raw[4*i:], math.Float32bits(v))
	}
	compression := uint16(1)
	if deflate {
		var b bytes.Buffer
		zw := zlib.NewWriter(&b)
		zw.Write(raw)
		zw.Close()
		raw, compression = b.Bytes(), 8
	}
	type entry struct {
		tag, typ uint16
		count    uint32
		value    uint32 // inline value or offset
	}
	var nd []byte
	if noData != nil {
		nd = append([]byte(strconv.FormatFloat(float64(*noData), 'g', -1, 32)), 0)
	}
	entries := []entry{
		{256, 4, 1, uint32(w)},
		{257, 4, 1, uint32(h)},
		{258, 3, 1, 32},
		{259, 3, 1, uint32(compression)},
		{273, 4, 1, 0}, // strip offset, set below
		{277, 3, 1, 1},
		{278, 4, 1, uint32(h)},
		{279, 4, 1, uint32(len(raw))},
		{339, 3, 1, 3},
	}
	if nd != nil {
		entries = append(entries, entry{42113, 2, uint32(len(nd)), 0})
	}
	ifdLen := 2 + 12*len(entries) + 4
	ndOff := 8 + ifdLen
	stripOff := ndOff
	if len(nd) > 4 { // does not fit the entry's 4-byte slot
		stripOff += len(nd)
	}
	var out bytes.Buffer
	out.WriteString("II*\x00")
	binary.Write(&out, binary.LittleEndian, uint32(8))
	binary.Write(&out, binary.LittleEndian, uint16(len(entries)))
	for _, e := range entries {
		switch {
		case e.tag == 273:
			e.value = uint32(stripOff)
		case e.tag == 42113 && len(nd) > 4:
			e.value = uint32(ndOff)
		case e.tag == 42113:
			var v [4]byte
			copy(v[:], nd)
			e.value = binary.LittleEndian.Uint32(v[:])
		}
		binary.Write(&out, binary.LittleEndian, e.tag)
		binary.Write(&out, binary.LittleEndian, e.typ)
		binary.Write(&out, binary.LittleEndian, e.count)
		if e.typ == 3 { // SHORT: left-justified in the slot
			binary.Write(&out, binary.LittleEndian, uint16(e.value))
			out.Write([]byte{0, 0})
		} else {
			binary.Write(&out, binary.LittleEndian, e.value)
		}
	}
	binary.Write(&out, binary.LittleEndian, uint32(0))
	if len(nd) > 4 {
		out.Write(nd)
	}
	out.Write(raw)
	return out.Bytes()
}
