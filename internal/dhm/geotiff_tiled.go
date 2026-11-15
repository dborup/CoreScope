package dhm

import (
	"encoding/binary"
	"errors"
	"math"
)

// #738 M1 — the tiled float32 GeoTIFF of a Copernicus DEM GLO-30 tile,
// read the way the foreign fill needs it: the IFDs from one range read of
// the start of the file, then single blocks from range reads of their own.
//
// This reads exactly one more shape than decodeGeoTIFF: classic TIFF
// (either byte order), tiled, one 32-bit IEEE float sample per pixel,
// contiguous planes, no compression or DEFLATE with any predictor
// decodeGeoTIFF undoes, georeferenced by one tiepoint and a pixel scale in
// EPSG:4326 with PixelIsPoint (geoKeys, crsWGS84). The first IFD is the
// full resolution; a second IFD that is a reduced-resolution image of
// half the size is the ½ overview the fill may fall back to (#738
// decision 7). Later IFDs are never read.
//
// As in decodeGeoTIFF, every size comes from the network and is checked
// before use: level edges are at most MaxTiledRasterSize and blocks at
// most MaxTileSize, so a block inflates to at most 4 MiB; every block
// lies inside the file and stores at most MaxBlockBytes; no allocation
// depends on the file beyond the block tables, whose length is bounded by
// the header read itself.

const (
	// MaxTiledRasterSize is the largest level edge in pixels. A GLO-30
	// tile is 2400 × 3600 between 50° and 60° N, and 3600 × 3600 south of
	// 50°.
	MaxTiledRasterSize = 4096
	// MaxTileSize is the largest block edge in pixels: GLO-30 uses 1024,
	// so one block is 4 MiB of float32.
	MaxTileSize = 1024
	// blockSlack is what a stored block may exceed its raw size by:
	// DEFLATE's stored-block overhead is 5 bytes per 64 KiB.
	blockSlack = 8192
	// subfileReducedImage is NewSubfileType bit 0: an overview.
	subfileReducedImage = 1
	// edgeTolerance is how far, in pixels, an edge or a point may lie
	// past a level's extent and still count as on it (#795): half a
	// pixel, the shift between a GLO-30 tile and its PixelIsPoint raster,
	// whose first pixel centre is the tile's north-west corner, plus
	// rounding.
	edgeTolerance = 0.5 + 1e-6
	// sizeTolerance is how far, in pixels, a level's width or height may
	// differ from the box it fits: rounding only.
	sizeTolerance = 1e-6
)

// ErrShortHeader is ParseTiledGeoTIFF's answer when the IFDs continue
// past the bytes given but inside the file: read more of the file's start
// and try again.
var ErrShortHeader = errors.New("GeoTIFF header continues past the bytes read")

// TiledGeoTIFF is the layout of a tiled GeoTIFF in EPSG:4326.
type TiledGeoTIFF struct {
	// Levels holds the full resolution first, then the ½ overview when
	// the file has one.
	Levels    []TiledLevel
	NoData    float32
	HasNoData bool
	order     binary.ByteOrder
}

// TiledLevel is one resolution of a TiledGeoTIFF.
type TiledLevel struct {
	Width, Height         int
	TileWidth, TileHeight int
	// Georef places the level in degrees: OriginE is the longitude of its
	// west edge, OriginN the latitude of its north edge, PixelE and PixelN
	// the pixel size. An overview has the full level's outer extent.
	Georef                 Georef
	compression, predictor uint32
	offsets, counts        []uint32 // per block, row-major
}

// ParseTiledGeoTIFF reads the layout of a tiled EPSG:4326 GeoTIFF of size
// bytes from prefix, the first bytes of the file. It reads no block.
func ParseTiledGeoTIFF(prefix []byte, size int64) (*TiledGeoTIFF, error) {
	if size < int64(len(prefix)) {
		return nil, errBadTIFF
	}
	r := &tiffReader{buf: prefix, size: uint64(size), crs: crsWGS84}
	first, err := r.readHeader()
	if err != nil {
		return nil, err
	}
	next, err := r.readIFD(first)
	if err != nil {
		return nil, err
	}
	if st, err := r.uint(tagNewSubfileType, 0, true); err != nil || st != 0 {
		return nil, errBadTIFF
	}
	g, err := r.georef()
	if err != nil {
		return nil, err
	}
	if g == (Georef{}) {
		return nil, errBadTIFF // the fill cannot place a raster without it
	}
	full, err := r.tiledLevel(g)
	if err != nil {
		return nil, err
	}
	t := &TiledGeoTIFF{Levels: []TiledLevel{full}, order: r.order}
	if t.NoData, t.HasNoData, err = r.noData(); err != nil {
		return nil, err
	}
	if next == 0 {
		return t, nil
	}
	if _, err := r.readIFD(next); err != nil {
		return nil, err
	}
	st, err := r.uint(tagNewSubfileType, 0, true)
	if err != nil {
		return nil, errBadTIFF
	}
	if st != subfileReducedImage {
		return t, nil // a mask or another image: no overview
	}
	w, h := (full.Width+1)/2, (full.Height+1)/2
	half, err := r.tiledLevel(Georef{
		OriginE: g.OriginE, OriginN: g.OriginN,
		PixelE: g.PixelE * float64(full.Width) / float64(w),
		PixelN: g.PixelN * float64(full.Height) / float64(h),
	})
	if err != nil {
		return nil, err
	}
	if half.Width != w || half.Height != h {
		return nil, errBadTIFF
	}
	t.Levels = append(t.Levels, half)
	return t, nil
}

// tiledLevel reads the level in r.fields, placed by g.
func (r *tiffReader) tiledLevel(g Georef) (TiledLevel, error) {
	if err := r.sampleShape(); err != nil {
		return TiledLevel{}, err
	}
	compression, predictor, err := r.coding()
	if err != nil {
		return TiledLevel{}, err
	}
	if pc, err := r.uint(tagPlanarConfig, 1, true); err != nil || pc != 1 {
		return TiledLevel{}, errBadTIFF
	}
	for _, tag := range []uint16{tagStripOffsets, tagStripByteCounts} {
		if _, ok := r.fields[tag]; ok {
			return TiledLevel{}, errBadTIFF
		}
	}
	var dims [4]uint32
	for i, tag := range []uint16{tagImageWidth, tagImageLength, tagTileWidth, tagTileLength} {
		if dims[i], err = r.uint(tag, 0, false); err != nil {
			return TiledLevel{}, err
		}
	}
	w, h, tw, th := dims[0], dims[1], dims[2], dims[3]
	// The TIFF spec requires tile edges in multiples of 16.
	if w < 1 || h < 1 || w > MaxTiledRasterSize || h > MaxTiledRasterSize ||
		tw < 16 || th < 16 || tw > MaxTileSize || th > MaxTileSize || tw%16 != 0 || th%16 != 0 || !g.valid() {
		return TiledLevel{}, errBadTIFF
	}
	l := TiledLevel{Width: int(w), Height: int(h), TileWidth: int(tw), TileHeight: int(th),
		Georef: g, compression: compression, predictor: predictor}
	n := l.BlocksAcross() * l.blocksDown()
	offsets, okO := r.fields[tagTileOffsets]
	counts, okC := r.fields[tagTileByteCounts]
	if !okO || !okC || offsets.count != uint32(n) || counts.count != uint32(n) {
		return TiledLevel{}, errBadTIFF
	}
	l.offsets, l.counts = make([]uint32, n), make([]uint32, n)
	raw := uint64(tw) * uint64(th) * 4
	for i := 0; i < n; i++ {
		off, ok1 := r.at(offsets, i)
		c, ok2 := r.at(counts, i)
		if !ok1 || !ok2 || off < 8 || c == 0 || uint64(c) > uint64(l.MaxBlockBytes()) ||
			uint64(off)+uint64(c) > r.size || compression == compressionNone && uint64(c) < raw {
			return TiledLevel{}, errBadTIFF
		}
		l.offsets[i], l.counts[i] = off, c
	}
	return l, nil
}

// Fits reports whether the full resolution covers the box west, south,
// east, north in degrees: its width and height the box's, and each edge
// within half a pixel of the box's (#795). A GLO-30 tile's raster is its
// 1° square shifted half a pixel north-west; a raster of another place or
// another size does not fit.
func (t *TiledGeoTIFF) Fits(west, south, east, north float64) bool {
	l := &t.Levels[0]
	g := l.Georef
	w, h := float64(l.Width)*g.PixelE, float64(l.Height)*g.PixelN
	return math.Abs(w-(east-west)) <= sizeTolerance*g.PixelE &&
		math.Abs(h-(north-south)) <= sizeTolerance*g.PixelN &&
		math.Abs(g.OriginE-west) <= edgeTolerance*g.PixelE &&
		math.Abs(g.OriginN-north) <= edgeTolerance*g.PixelN
}

// BlocksAcross is the number of block columns.
func (l *TiledLevel) BlocksAcross() int { return (l.Width + l.TileWidth - 1) / l.TileWidth }

func (l *TiledLevel) blocksDown() int { return (l.Height + l.TileHeight - 1) / l.TileHeight }

// NumBlocks is the number of blocks, row-major.
func (l *TiledLevel) NumBlocks() int { return len(l.offsets) }

// BlockRange is the byte range of block i in the file: its offset and
// length. i must be in [0, NumBlocks).
func (l *TiledLevel) BlockRange(i int) (off, n int64) {
	return int64(l.offsets[i]), int64(l.counts[i])
}

// MaxBlockBytes is the most bytes a block of this level may store.
func (l *TiledLevel) MaxBlockBytes() int { return l.TileWidth*l.TileHeight*4 + blockSlack }

// blockGrid is block i's pixels inside the raster, placed, without values.
func (l *TiledLevel) blockGrid(i int) Grid {
	bx, by := i%l.BlocksAcross(), i/l.BlocksAcross()
	g := l.Georef
	return Grid{
		Width:  min(l.TileWidth, l.Width-bx*l.TileWidth),
		Height: min(l.TileHeight, l.Height-by*l.TileHeight),
		Georef: Georef{
			OriginE: g.OriginE + float64(bx*l.TileWidth)*g.PixelE,
			OriginN: g.OriginN - float64(by*l.TileHeight)*g.PixelN,
			PixelE:  g.PixelE, PixelN: g.PixelN,
		},
	}
}

// Locate returns the block whose pixels hold (lon, lat) — the pixel whose
// cell contains it, the point first clamped into the raster — and the
// point clamped into that block's extent, where its Grid always samples.
// So past the outermost pixel centres the edge pixels are held, as
// Grid.Sample holds them, and sampling never needs two blocks: within half
// a pixel of an internal block edge the block's own edge pixel is held
// instead of interpolating across (one pixel, about 30 m, every 1024 on
// GLO-30). NaN is treated as the west or north edge.
func (l *TiledLevel) Locate(lon, lat float64) (block int, blon, blat float64) {
	g := l.Georef
	col := clampIndex((lon-g.OriginE)/g.PixelE, l.Width)
	row := clampIndex((g.OriginN-lat)/g.PixelN, l.Height)
	block = (row/l.TileHeight)*l.BlocksAcross() + col/l.TileWidth
	bg := l.blockGrid(block)
	x := bg.Extent()
	return block, clampTo(lon, x[0], x[2]), clampTo(lat, x[1], x[3])
}

// clampIndex is floor(v) clamped to [0, n−1]; NaN is 0.
func clampIndex(v float64, n int) int {
	switch {
	case !(v >= 0):
		return 0
	case v >= float64(n):
		return n - 1
	}
	return int(v)
}

// clampTo is v clamped to [lo, hi]; NaN is lo.
func clampTo(v, lo, hi float64) float64 {
	switch {
	case !(v >= lo):
		return lo
	case v > hi:
		return hi
	}
	return v
}

// DecodeBlock decodes block i of the given level from raw, exactly its
// BlockRange bytes, into a Grid of the block's pixels inside the raster:
// an edge block's padding is cropped. The Grid is in degrees, like the
// level, and carries the file's nodata. buf is used for the values when it
// holds at least one tile, and the Grid then aliases it; otherwise one
// tile is allocated.
func (t *TiledGeoTIFF) DecodeBlock(level, i int, raw []byte, buf []float32) (Grid, error) {
	if level < 0 || level >= len(t.Levels) {
		return Grid{}, errBadTIFF
	}
	l := &t.Levels[level]
	if i < 0 || i >= l.NumBlocks() || uint64(len(raw)) != uint64(l.counts[i]) {
		return Grid{}, errBadTIFF
	}
	n := l.TileWidth * l.TileHeight
	if cap(buf) < n {
		buf = make([]float32, n)
	}
	buf = buf[:n]
	d := newRowDecoder(&tiffReader{order: t.order}, l.compression, l.predictor, l.TileWidth)
	if err := d.decode(raw, buf); err != nil {
		return Grid{}, err
	}
	g := l.blockGrid(i)
	if g.Width < l.TileWidth {
		// Crop the padding: row y moves forward from y·TileWidth to
		// y·Width; copy handles the overlap.
		for y := 1; y < g.Height; y++ {
			copy(buf[y*g.Width:(y+1)*g.Width], buf[y*l.TileWidth:])
		}
	}
	g.Values = buf[:g.Width*g.Height]
	g.NoData, g.HasNoData = t.NoData, t.HasNoData
	return g, nil
}
