package dhm

// #737 M1 — the layout of a JPEG-tiled image COG, the Skråfoto oblique
// photos (internal/skraafoto): the IFD pyramid from one range read of
// the start of the file, so a preview reads the tiles of one overview and
// nothing else.
//
// This reads one shape, with the IFD reader of geotiff.go: classic TIFF
// (either byte order), tiled, compression 7 (JPEG, tables in JPEGTables),
// 8 bits per sample, 3 samples (RGB or YCbCr) or 4 (RGB plus a fourth
// band), contiguous planes. The first IFD is the full resolution; each
// later reduced-resolution IFD (NewSubfileType 1) is an overview, and
// any other IFD, such as a transparency mask, is skipped. At most
// MaxImageIFDs IFDs are walked. Georeferencing is not read: a preview is
// a picture.
//
// As everywhere in this reader, every size comes from the network and is
// checked before use: level and tile edges are bounded, every tile lies
// inside the file and stores at most MaxTileBytes, and the tables are at
// most MaxJPEGTables. The only allocations are the tile tables, whose
// length is bounded by the bytes read.

const (
	// MaxImageEdge is the largest level edge in pixels. A full-resolution
	// oblique photo is 20544 × 14016.
	MaxImageEdge = 1 << 16
	// MaxImageTile is the largest tile edge; the photos use 512.
	MaxImageTile = 1024
	// MaxImageIFDs bounds the IFDs walked; a photo has 7.
	MaxImageIFDs = 16
	// MaxJPEGTables bounds the JPEGTables of a level.
	MaxJPEGTables = 64 << 10

	compressionJPEG    = 7
	photometricRGB     = 2
	photometricYCbCr   = 6
	tagPhotometric     = 262
	tagJPEGTables      = 347
	typeUndefined      = 7
	subfileTransparent = 4 // NewSubfileType bit 2: a mask
)

// imageReadTags are the tags ParseTiledImage keeps.
var imageReadTags = map[uint16]bool{
	tagNewSubfileType: true, tagImageWidth: true, tagImageLength: true, tagBitsPerSample: true,
	tagCompression: true, tagPhotometric: true, tagSamplesPerPixel: true, tagPlanarConfig: true,
	tagStripOffsets: true, tagStripByteCounts: true, tagTileWidth: true, tagTileLength: true,
	tagTileOffsets: true, tagTileByteCounts: true, tagJPEGTables: true,
}

// TiledImage is the layout of a JPEG-tiled image COG.
type TiledImage struct {
	// Levels holds the full resolution first, then the overviews, each
	// smaller than the one before.
	Levels []ImageLevel
}

// ImageLevel is one resolution of a TiledImage.
type ImageLevel struct {
	Width, Height         int
	TileWidth, TileHeight int
	// Samples is 3 or 4; Photometric is 2 (RGB) or 6 (YCbCr, 3 samples).
	Samples, Photometric int
	// JPEGTables are the quantisation and Huffman tables every tile's
	// abbreviated stream uses: a JPEG stream from SOI to EOI. nil: the
	// tiles carry their own.
	JPEGTables      []byte
	offsets, counts []uint32 // per tile, row-major
}

// TilesAcross is the number of tile columns.
func (l *ImageLevel) TilesAcross() int { return (l.Width + l.TileWidth - 1) / l.TileWidth }

// TilesDown is the number of tile rows.
func (l *ImageLevel) TilesDown() int { return (l.Height + l.TileHeight - 1) / l.TileHeight }

// NumTiles is the number of tiles, row-major.
func (l *ImageLevel) NumTiles() int { return len(l.offsets) }

// TileRange is the byte range of tile i in the file: its offset and
// length. A sparse tile, which GDAL writes for an empty area, is 0, 0. i
// must be in [0, NumTiles).
func (l *ImageLevel) TileRange(i int) (off, n int64) {
	return int64(l.offsets[i]), int64(l.counts[i])
}

// MaxTileBytes is the most bytes a tile of this level may store: its raw
// size and some slack. JPEG at any quality stores less.
func (l *ImageLevel) MaxTileBytes() int {
	return l.TileWidth*l.TileHeight*l.Samples + blockSlack
}

// ParseTiledImage reads the layout of a JPEG-tiled image COG of size bytes
// from prefix, the first bytes of the file. It reads no tile. When the
// IFDs continue past prefix but inside the file it returns
// ErrShortHeader: read more of the start and try again.
func ParseTiledImage(prefix []byte, size int64) (*TiledImage, error) {
	if size < int64(len(prefix)) {
		return nil, errBadTIFF
	}
	r := &tiffReader{buf: prefix, size: uint64(size), image: true}
	off, err := r.readHeader()
	if err != nil {
		return nil, err
	}
	img := &TiledImage{}
	seen := make(map[uint64]bool, MaxImageIFDs)
	for walked := 0; off != 0 && walked < MaxImageIFDs; walked++ {
		if seen[off] {
			return nil, errBadTIFF // an IFD chain that loops
		}
		seen[off] = true
		next, err := r.readIFD(off)
		if err != nil {
			return nil, err
		}
		st, err := r.uint(tagNewSubfileType, 0, true)
		if err != nil {
			return nil, errBadTIFF
		}
		switch {
		case walked == 0 && st != 0:
			return nil, errBadTIFF // the first IFD must be the full image
		case walked > 0 && st != subfileReducedImage:
			// A mask (subfileTransparent) or another image: not a level.
		default:
			l, err := r.imageLevel()
			if err != nil {
				return nil, err
			}
			if n := len(img.Levels); n > 0 {
				p := img.Levels[n-1]
				if l.Width > p.Width || l.Height > p.Height || l.Width*l.Height >= p.Width*p.Height {
					return nil, errBadTIFF
				}
			}
			img.Levels = append(img.Levels, l)
		}
		off = next
	}
	return img, nil
}

// imageLevel reads the level in r.fields.
func (r *tiffReader) imageLevel() (ImageLevel, error) {
	var v [7]uint32
	for i, tag := range []uint16{tagImageWidth, tagImageLength, tagTileWidth, tagTileLength, tagCompression, tagSamplesPerPixel, tagPhotometric} {
		var err error
		if v[i], err = r.uint(tag, 0, false); err != nil {
			return ImageLevel{}, err
		}
	}
	w, h, tw, th, compression, spp, photometric := v[0], v[1], v[2], v[3], v[4], v[5], v[6]
	if w < 1 || h < 1 || w > MaxImageEdge || h > MaxImageEdge ||
		tw < 16 || th < 16 || tw > MaxImageTile || th > MaxImageTile || tw%16 != 0 || th%16 != 0 ||
		compression != compressionJPEG || (spp != 3 && spp != 4) ||
		(photometric != photometricRGB && !(photometric == photometricYCbCr && spp == 3)) {
		return ImageLevel{}, errBadTIFF
	}
	if pc, err := r.uint(tagPlanarConfig, 1, true); err != nil || pc != 1 {
		return ImageLevel{}, errBadTIFF
	}
	bits, ok := r.fields[tagBitsPerSample]
	if !ok || bits.count != spp {
		return ImageLevel{}, errBadTIFF
	}
	for i := 0; i < int(spp); i++ {
		if b, ok := r.at(bits, i); !ok || b != 8 {
			return ImageLevel{}, errBadTIFF
		}
	}
	for _, tag := range []uint16{tagStripOffsets, tagStripByteCounts} {
		if _, ok := r.fields[tag]; ok {
			return ImageLevel{}, errBadTIFF
		}
	}
	l := ImageLevel{Width: int(w), Height: int(h), TileWidth: int(tw), TileHeight: int(th),
		Samples: int(spp), Photometric: int(photometric)}
	if f, ok := r.fields[tagJPEGTables]; ok {
		if f.typ != typeUndefined || f.count > MaxJPEGTables {
			return ImageLevel{}, errBadTIFF
		}
		l.JPEGTables = append([]byte(nil), f.data...)
	}
	n := l.TilesAcross() * l.TilesDown()
	offsets, okO := r.fields[tagTileOffsets]
	counts, okC := r.fields[tagTileByteCounts]
	if !okO || !okC || offsets.count != uint32(n) || counts.count != uint32(n) {
		return ImageLevel{}, errBadTIFF
	}
	l.offsets, l.counts = make([]uint32, n), make([]uint32, n)
	for i := 0; i < n; i++ {
		off, ok1 := r.at(offsets, i)
		c, ok2 := r.at(counts, i)
		sparse := off == 0 && c == 0
		if !ok1 || !ok2 || !sparse && (off < 8 || c == 0 || uint64(c) > uint64(l.MaxTileBytes()) || uint64(off)+uint64(c) > r.size) {
			return ImageLevel{}, errBadTIFF
		}
		l.offsets[i], l.counts[i] = off, c
	}
	return l, nil
}
