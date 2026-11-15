package dhmtest

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"math"
)

// #738 — synthetic tiles shaped like Copernicus DEM GLO-30, for the
// foreign-fill tests and benchmarks. Nothing here touches the network.

// COGEntry is one IFD entry of a COG, its value already in little-endian
// byte order. COGOptions.Edit may change, add or drop entries to build
// malformed files.
type COGEntry struct {
	Tag, Type uint16
	Count     uint32
	Data      []byte
	// Raw, when set, is written into the entry's 4-byte value slot
	// verbatim, so a test can point a field anywhere.
	Raw *uint32
}

// COGOptions shape a COG. The zero value of each optional field is the
// GLO-30 layout (#738 research §3.2): float32, DEFLATE, predictor 3,
// EPSG:4326, PixelIsPoint, IFDs before the data.
type COGOptions struct {
	Width, Height int // full resolution
	Tile          int // tile edge; 0: 1024
	// Lon0, Lat0 is the centre of pixel (0, 0), the tiepoint of a
	// PixelIsPoint raster; PixelLon, PixelLat the pixel size in degrees.
	Lon0, Lat0, PixelLon, PixelLat float64
	// Value is the elevation at a pixel centre.
	Value func(lon, lat float64) float32
	// Overview adds the ½ overview as a second IFD, with each pixel the
	// Value at its own centre.
	Overview bool
	// Uncompressed stores the blocks raw, without a predictor.
	Uncompressed bool
	// GeoKeys replaces the GeoKeyDirectory; nil: the GLO-30 keys.
	GeoKeys []uint16
	// Edit rewrites the entries of IFD level (0: full resolution, 1: the
	// overview) before the file is laid out.
	Edit func(level int, entries []COGEntry) []COGEntry
	// Block rewrites the stored bytes of a block after compression.
	Block func(level, i int, b []byte) []byte
	// Pad adds this many zero bytes between the IFDs and the first block,
	// to push data past a header read.
	Pad int
}

// GLO30GeoKeys are the GeoKeys of a GLO-30 tile: geographic model,
// PixelIsPoint, EPSG:4326.
func GLO30GeoKeys() []uint16 {
	return []uint16{1, 1, 0, 3, 1024, 0, 1, 2, 1025, 0, 1, 2, 2048, 0, 1, 4326}
}

// GLO30Tile is a COG of one 1°×1° GLO-30 tile between 50° and 60° N,
// whose south-west corner is lat, lon: 2400 × 3600 pixels of 1.5″ × 1″,
// tiled 1024², with the ½ overview.
func GLO30Tile(lat, lon int, value func(lon, lat float64) float32) COGOptions {
	return COGOptions{
		Width: 2400, Height: 3600,
		Lon0: float64(lon), Lat0: float64(lat + 1),
		PixelLon: 1.0 / 2400, PixelLat: 1.0 / 3600,
		Value: value, Overview: true,
	}
}

// COGBlocks is the number of blocks of each level of o.
func COGBlocks(o COGOptions) []int {
	t := tileEdge(o)
	out := []int{blocks(o.Width, t) * blocks(o.Height, t)}
	if o.Overview {
		out = append(out, blocks((o.Width+1)/2, t)*blocks((o.Height+1)/2, t))
	}
	return out
}

func tileEdge(o COGOptions) int {
	if o.Tile == 0 {
		return 1024
	}
	return o.Tile
}

func blocks(n, t int) int { return (n + t - 1) / t }

// cogLevel is one resolution level being written.
type cogLevel struct {
	w, h           int
	lon0, lat0     float64 // centre of pixel (0, 0)
	dLon, dLat     float64
	blocks         [][]byte
	entries        []COGEntry
	offIdx, cntIdx int // entry indices of TileOffsets and TileByteCounts
}

// COG writes the file.
func COG(o COGOptions) []byte {
	t := tileEdge(o)
	levels := []*cogLevel{{w: o.Width, h: o.Height, lon0: o.Lon0, lat0: o.Lat0, dLon: o.PixelLon, dLat: o.PixelLat}}
	if o.Overview {
		w1, h1 := (o.Width+1)/2, (o.Height+1)/2
		// The overview has the same outer extent, at its own pixel size.
		dLon, dLat := o.PixelLon*float64(o.Width)/float64(w1), o.PixelLat*float64(o.Height)/float64(h1)
		levels = append(levels, &cogLevel{w: w1, h: h1,
			lon0: o.Lon0 - o.PixelLon/2 + dLon/2, lat0: o.Lat0 + o.PixelLat/2 - dLat/2, dLon: dLon, dLat: dLat})
	}
	for li, l := range levels {
		for by := 0; by < blocks(l.h, t); by++ {
			for bx := 0; bx < blocks(l.w, t); bx++ {
				b := encodeBlock(o, l, bx, by, t)
				if o.Block != nil {
					b = o.Block(li, len(l.blocks), b)
				}
				l.blocks = append(l.blocks, b)
			}
		}
		l.entries = levelEntries(o, l, li, t)
		if o.Edit != nil {
			l.entries = o.Edit(li, l.entries)
		}
		l.offIdx, l.cntIdx = -1, -1
		for i, e := range l.entries {
			switch e.Tag {
			case 324:
				l.offIdx = i
			case 325:
				l.cntIdx = i
			}
		}
	}

	// Layout: header, then per level its IFD and out-of-line values, then
	// the blocks of the last level first, as GDAL writes a COG.
	pos := 8
	ifdAt := make([]int, len(levels))
	extraAt := make([][]int, len(levels))
	for li, l := range levels {
		ifdAt[li] = pos
		pos += 2 + 12*len(l.entries) + 4
		extraAt[li] = make([]int, len(l.entries))
		for i, e := range l.entries {
			if len(e.Data) > 4 && e.Raw == nil {
				extraAt[li][i] = pos
				pos += len(e.Data)
				pos += pos & 1 // word alignment
			}
		}
	}
	pos += o.Pad
	blockAt := make([][]int, len(levels))
	for li := len(levels) - 1; li >= 0; li-- {
		for _, b := range levels[li].blocks {
			blockAt[li] = append(blockAt[li], pos)
			pos += len(b)
		}
	}
	// Now the block offsets are known: fill TileOffsets in place.
	for li, l := range levels {
		if l.offIdx >= 0 && l.entries[l.offIdx].Raw == nil {
			d := l.entries[l.offIdx].Data
			for i := 0; i < len(d)/4 && i < len(blockAt[li]); i++ {
				binary.LittleEndian.PutUint32(d[4*i:], uint32(blockAt[li][i]))
			}
		}
	}

	out := make([]byte, pos)
	copy(out, "II*\x00")
	binary.LittleEndian.PutUint32(out[4:], uint32(ifdAt[0]))
	for li, l := range levels {
		p := ifdAt[li]
		binary.LittleEndian.PutUint16(out[p:], uint16(len(l.entries)))
		p += 2
		for i, e := range l.entries {
			binary.LittleEndian.PutUint16(out[p:], e.Tag)
			binary.LittleEndian.PutUint16(out[p+2:], e.Type)
			binary.LittleEndian.PutUint32(out[p+4:], e.Count)
			switch {
			case e.Raw != nil:
				binary.LittleEndian.PutUint32(out[p+8:], *e.Raw)
			case len(e.Data) > 4:
				binary.LittleEndian.PutUint32(out[p+8:], uint32(extraAt[li][i]))
				copy(out[extraAt[li][i]:], e.Data)
			default:
				copy(out[p+8:p+12], e.Data)
			}
			p += 12
		}
		if li+1 < len(levels) {
			binary.LittleEndian.PutUint32(out[p:], uint32(ifdAt[li+1]))
		}
	}
	for li, l := range levels {
		for i, b := range l.blocks {
			copy(out[blockAt[li][i]:], b)
		}
	}
	return out
}

func levelEntries(o COGOptions, l *cogLevel, li, t int) []COGEntry {
	n := len(l.blocks)
	counts := make([]uint32, n)
	for i, b := range l.blocks {
		counts[i] = uint32(len(b))
	}
	compression, predictor := uint16(8), uint16(3)
	if o.Uncompressed {
		compression, predictor = 1, 1
	}
	var es []COGEntry
	if li > 0 {
		es = append(es, longEntry(254, 1)) // NewSubfileType: reduced resolution
	}
	es = append(es,
		shortEntry(256, uint16(l.w)), shortEntry(257, uint16(l.h)),
		shortEntry(258, 32), shortEntry(259, compression), shortEntry(262, 1),
		shortEntry(277, 1), shortEntry(284, 1), shortEntry(317, predictor),
		shortEntry(322, uint16(t)), shortEntry(323, uint16(t)),
		COGEntry{Tag: 324, Type: 4, Count: uint32(n), Data: make([]byte, 4*n)},
		longsEntry(325, counts...),
		shortEntry(339, 3),
	)
	if li == 0 {
		keys := o.GeoKeys
		if keys == nil {
			keys = GLO30GeoKeys()
		}
		es = append(es,
			doublesEntry(33550, o.PixelLon, o.PixelLat, 0),
			doublesEntry(33922, 0, 0, 0, o.Lon0, o.Lat0, 0),
			shortsEntry(34735, keys...),
		)
	}
	return es
}

// encodeBlock is block (bx, by) of level l: t × t values, padded past the
// raster edge with zeros, as GDAL pads, then predicted and compressed.
func encodeBlock(o COGOptions, l *cogLevel, bx, by, t int) []byte {
	raw := make([]byte, 4*t*t)
	row := make([]byte, 4*t)
	for y := 0; y < t; y++ {
		for x := 0; x < t; x++ {
			col, r := bx*t+x, by*t+y
			var v float32
			if col < l.w && r < l.h && o.Value != nil {
				v = o.Value(l.lon0+float64(col)*l.dLon, l.lat0-float64(r)*l.dLat)
			}
			binary.LittleEndian.PutUint32(row[4*x:], math.Float32bits(v))
		}
		if !o.Uncompressed {
			floatPredict(row, t)
		}
		copy(raw[4*t*y:], row)
	}
	if o.Uncompressed {
		return raw
	}
	var b bytes.Buffer
	zw := zlib.NewWriter(&b)
	zw.Write(raw)
	zw.Close()
	return b.Bytes()
}

// floatPredict applies libtiff's floating-point predictor (fpDiff) to one
// row of w little-endian float32: byte planes most significant first, then
// byte-wise differences.
func floatPredict(row []byte, w int) {
	planes := make([]byte, len(row))
	for i := 0; i < w; i++ {
		v := binary.LittleEndian.Uint32(row[4*i:])
		planes[i] = byte(v >> 24)
		planes[w+i] = byte(v >> 16)
		planes[2*w+i] = byte(v >> 8)
		planes[3*w+i] = byte(v)
	}
	for i := len(planes) - 1; i > 0; i-- {
		planes[i] -= planes[i-1]
	}
	copy(row, planes)
}

func shortEntry(tag, v uint16) COGEntry { return shortsEntry(tag, v) }

func shortsEntry(tag uint16, vs ...uint16) COGEntry {
	b := make([]byte, 2*len(vs))
	for i, v := range vs {
		binary.LittleEndian.PutUint16(b[2*i:], v)
	}
	return COGEntry{Tag: tag, Type: 3, Count: uint32(len(vs)), Data: b}
}

func longEntry(tag uint16, v uint32) COGEntry { return longsEntry(tag, v) }

func longsEntry(tag uint16, vs ...uint32) COGEntry {
	b := make([]byte, 4*len(vs))
	for i, v := range vs {
		binary.LittleEndian.PutUint32(b[4*i:], v)
	}
	return COGEntry{Tag: tag, Type: 4, Count: uint32(len(vs)), Data: b}
}

func doublesEntry(tag uint16, vs ...float64) COGEntry {
	b := make([]byte, 8*len(vs))
	for i, v := range vs {
		binary.LittleEndian.PutUint64(b[8*i:], math.Float64bits(v))
	}
	return COGEntry{Tag: tag, Type: 12, Count: uint32(len(vs)), Data: b}
}

// ShortsEntry, LongsEntry and DoublesEntry build replacement entries for
// COGOptions.Edit.
func ShortsEntry(tag uint16, vs ...uint16) COGEntry   { return shortsEntry(tag, vs...) }
func LongsEntry(tag uint16, vs ...uint32) COGEntry    { return longsEntry(tag, vs...) }
func DoublesEntry(tag uint16, vs ...float64) COGEntry { return doublesEntry(tag, vs...) }
