package dhmtest

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
)

// #737 — synthetic JPEG-tiled image COGs shaped like the Skråfoto
// oblique photos (owner token checks, 2026-11-15): 512 × 512 tiles,
// compression 7 (JPEG) with the tables in JPEGTables, 4 samples (RGB plus
// a fourth band), and a pyramid of reduced-resolution IFDs. Nothing here
// touches the network.

// SkraafotoLevels is the pyramid of a real oblique photo: 20544 × 14016
// at full resolution and six overviews down to 321 px.
var SkraafotoLevels = [][2]int{
	{20544, 14016}, {10272, 7008}, {5136, 3504}, {2568, 1752},
	{1284, 876}, {642, 438}, {321, 219},
}

// ImageCOGOptions shape a JPEG-tiled image COG.
type ImageCOGOptions struct {
	// Levels are the width and height of each IFD, full resolution first.
	Levels [][2]int
	// Tile is the tile edge; 0: 512.
	Tile int
	// Samples is 3 or 4; 0: 4.
	Samples int
	// Photometric is the PhotometricInterpretation; 0: 2 (RGB). With 3
	// samples, 6 (YCbCr) stores tiles as Go's encoder writes them.
	Photometric int
	// Encode reports whether a level's tiles are real JPEG. The tiles of
	// any other level are 16 filler bytes: a reader that decodes them
	// fails, and one that only reads the chosen level never notices.
	Encode func(level int) bool
	// Color is the pixel at (x, y) of a level: R, G, B and the fourth
	// band. nil: Gradient. RGB and 4-sample tiles are flat per 8 × 8
	// block, the colour of the block's top-left pixel, so they decode
	// exactly (DCJPEG); YCbCr tiles are Go's lossy encoding of every
	// pixel.
	Color func(level, x, y int) [4]uint8
	// Mask inserts a transparency mask IFD (NewSubfileType 4) after this
	// level index; 0: none.
	Mask int
	// Edit rewrites the entries of IFD level before the file is laid out.
	Edit func(level int, entries []COGEntry) []COGEntry
	// Block rewrites the stored bytes of a tile.
	Block func(level, i int, b []byte) []byte
	// Pad adds zero bytes between the IFDs and the first tile.
	Pad int
}

// Gradient is the default colour: red across, green down, blue fixed per
// level, so a test can tell where a pixel came from.
func Gradient(level, x, y int) [4]uint8 {
	return [4]uint8{uint8(x * 255 / 2048), uint8(y * 255 / 2048), uint8(40 + 20*level), 255}
}

// ImageCOG writes the file.
func ImageCOG(o ImageCOGOptions) []byte {
	t := o.Tile
	if t == 0 {
		t = 512
	}
	spp := o.Samples
	if spp == 0 {
		spp = 4
	}
	photometric := o.Photometric
	if photometric == 0 {
		photometric = 2
	}
	col := o.Color
	if col == nil {
		col = Gradient
	}
	var out []TIFFLevel
	for li, wh := range o.Levels {
		w, h := wh[0], wh[1]
		across, down := blocks(w, t), blocks(h, t)
		var tables []byte
		tiles := make([][]byte, 0, across*down)
		for by := 0; by < down; by++ {
			for bx := 0; bx < across; bx++ {
				b := []byte("not-a-jpeg-tile!")
				if o.Encode != nil && o.Encode(li) {
					px := func(x, y int) [4]uint8 { return col(li, bx*t+x, by*t+y) }
					if photometric == 6 {
						tables, b = GoJPEGTile(t, t, px)
					} else {
						tables, b = DCJPEG(t, t, spp, px)
					}
				}
				if o.Block != nil {
					b = o.Block(li, len(tiles), b)
				}
				tiles = append(tiles, b)
			}
		}
		if tables == nil {
			tables, _ = DCJPEG(16, 16, spp, func(int, int) [4]uint8 { return [4]uint8{} })
		}
		es := imageEntries(li, w, h, t, spp, photometric, tiles, tables)
		if o.Edit != nil {
			es = o.Edit(li, es)
		}
		out = append(out, TIFFLevel{Entries: es, Blocks: tiles})
		if o.Mask > 0 && li == o.Mask {
			// A 1-bit mask as GDAL writes one: its own tiles, never read.
			m := imageEntries(li, w, h, t, 1, 4, tiles, nil)
			m[0] = longEntry(254, 4)
			out = append(out, TIFFLevel{Entries: m, Blocks: tiles})
		}
	}
	return WriteTIFF(out, o.Pad)
}

func imageEntries(li, w, h, t, spp, photometric int, tiles [][]byte, tables []byte) []COGEntry {
	counts := make([]uint32, len(tiles))
	for i, b := range tiles {
		counts[i] = uint32(len(b))
	}
	bits := make([]uint16, spp)
	for i := range bits {
		bits[i] = 8
	}
	subfile := uint32(0)
	if li > 0 {
		subfile = 1
	}
	es := []COGEntry{
		longEntry(254, subfile),
		longEntry(256, uint32(w)), longEntry(257, uint32(h)),
		shortsEntry(258, bits...), shortEntry(259, 7), shortEntry(262, uint16(photometric)),
		shortEntry(277, uint16(spp)), shortEntry(284, 1),
		shortEntry(322, uint16(t)), shortEntry(323, uint16(t)),
		{Tag: 324, Type: 4, Count: uint32(len(tiles)), Data: make([]byte, 4*len(tiles))},
		longsEntry(325, counts...),
	}
	if spp == 4 {
		es = append(es, shortEntry(338, 0)) // ExtraSamples: unspecified
	}
	if tables != nil {
		es = append(es, COGEntry{Tag: 347, Type: 7, Count: uint32(len(tables)), Data: tables})
	}
	return es
}

// DCJPEG encodes a w × h baseline JPEG of n components (1-4) whose every
// 8 × 8 block is flat: the colour px gives at the block's top-left pixel.
// Only DC coefficients are coded, with quantiser 1, so each block decodes
// to its exact colour. It returns the tables (SOI, DQT, DHT, EOI) apart
// from the abbreviated image stream (SOI, SOF0, SOS, data, EOI), as a
// TIFF stores them. The stream carries no APP14 marker: a 4-component
// stream is what GDAL writes for RGB plus a fourth band.
func DCJPEG(w, h, n int, px func(x, y int) [4]uint8) (tables, stream []byte) {
	var tb bytes.Buffer
	tb.Write([]byte{0xFF, 0xD8})
	// DQT: table 0, all ones.
	tb.Write([]byte{0xFF, 0xDB, 0, 67, 0})
	tb.Write(bytes.Repeat([]byte{1}, 64))
	// DHT: the standard luminance DC table, and an AC table whose only
	// code, "0", is EOB.
	tb.Write([]byte{0xFF, 0xC4, 0, 31, 0x00})
	tb.Write(dcBits[:])
	for v := 0; v < 12; v++ {
		tb.WriteByte(byte(v))
	}
	tb.Write([]byte{0xFF, 0xC4, 0, 20, 0x10, 1})
	tb.Write(make([]byte, 15))
	tb.WriteByte(0x00)
	tb.Write([]byte{0xFF, 0xD9})

	var sb bytes.Buffer
	sb.Write([]byte{0xFF, 0xD8})
	sof := []byte{0xFF, 0xC0, 0, byte(8 + 3*n), 8, byte(h >> 8), byte(h), byte(w >> 8), byte(w), byte(n)}
	for c := 0; c < n; c++ {
		sof = append(sof, byte(c+1), 0x11, 0)
	}
	sb.Write(sof)
	sos := []byte{0xFF, 0xDA, 0, byte(6 + 2*n), byte(n)}
	for c := 0; c < n; c++ {
		sos = append(sos, byte(c+1), 0x00)
	}
	sb.Write(append(sos, 0, 63, 0))
	bw := &bitWriter{w: &sb}
	pred := make([]int, n)
	for by := 0; by < (h+7)/8; by++ {
		for bx := 0; bx < (w+7)/8; bx++ {
			p := px(min(bx*8, w-1), min(by*8, h-1))
			for c := 0; c < n; c++ {
				dc := 8 * (int(p[c]) - 128)
				diff := dc - pred[c]
				pred[c] = dc
				cat, bits := 0, diff
				for a := abs(diff); a > 0; a >>= 1 {
					cat++
				}
				if diff < 0 {
					bits = diff + (1 << cat) - 1
				}
				code, l := dcCode(cat)
				bw.put(code, l)
				bw.put(uint32(bits), cat)
				bw.put(0, 1) // EOB
			}
		}
	}
	bw.flush()
	sb.Write([]byte{0xFF, 0xD9})
	return tb.Bytes(), sb.Bytes()
}

// dcBits is the code-length count of the standard luminance DC table
// (ITU T.81 Table K.3).
var dcBits = [16]byte{0, 1, 5, 1, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0, 0, 0}

// dcCode is the canonical code of DC category cat in that table.
func dcCode(cat int) (uint32, int) {
	code, n := uint32(0), 0
	for l := 1; l <= 16; l++ {
		for i := 0; i < int(dcBits[l-1]); i++ {
			if n == cat {
				return code, l
			}
			code++
			n++
		}
		code <<= 1
	}
	panic("dhmtest: DC category out of range")
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// bitWriter writes entropy-coded bits with 0xFF byte stuffing.
type bitWriter struct {
	w    *bytes.Buffer
	acc  uint32
	nacc int
}

func (b *bitWriter) put(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		b.acc = b.acc<<1 | (v>>uint(i))&1
		b.nacc++
		if b.nacc == 8 {
			b.byte(byte(b.acc))
			b.acc, b.nacc = 0, 0
		}
	}
}

func (b *bitWriter) byte(c byte) {
	b.w.WriteByte(c)
	if c == 0xFF {
		b.w.WriteByte(0)
	}
}

// flush pads the last byte with ones.
func (b *bitWriter) flush() {
	for b.nacc != 0 {
		b.put(1, 1)
	}
}

// GoJPEGTile is a w × h tile encoded by Go's image/jpeg (YCbCr 4:2:0,
// quality 75), every pixel from px, split into the tables (DQT and DHT)
// and the abbreviated stream like DCJPEG.
func GoJPEGTile(w, h int, px func(x, y int) [4]uint8) (tables, stream []byte) {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			p := px(x, y)
			img.SetRGBA(x, y, color.RGBA{p[0], p[1], p[2], 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 75}); err != nil {
		panic(err)
	}
	return SplitJPEG(buf.Bytes())
}

// SplitJPEG moves the DQT and DHT segments of a full JPEG into a tables
// stream, as a TIFF's JPEGTables holds them.
func SplitJPEG(full []byte) (tables, stream []byte) {
	tables = []byte{0xFF, 0xD8}
	stream = []byte{0xFF, 0xD8}
	p := 2
	for p+4 <= len(full) {
		m := full[p+1]
		if m == 0xDA { // SOS: the rest is the scan
			stream = append(stream, full[p:]...)
			break
		}
		n := int(binary.BigEndian.Uint16(full[p+2:]))
		seg := full[p : p+2+n]
		if m == 0xDB || m == 0xC4 {
			tables = append(tables, seg...)
		} else {
			stream = append(stream, seg...)
		}
		p += 2 + n
	}
	return append(tables, 0xFF, 0xD9), stream
}
