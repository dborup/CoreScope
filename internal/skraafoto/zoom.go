package skraafoto

import (
	"context"
	"errors"
	"image"
	"math"

	"github.com/meshcore-analyzer/dhm"
)

// #737 M3b — a zoomed crop around a point of the preview. The preview is
// cut from IFD4 of a real photo (1284 px, about 1.1 m/px); a crop is a
// box of at most PreviewMaxPx cut 1:1 from IFD2 (5136 px, about 0.3
// m/px) around the point, read as the preview is: the header, then only
// the tiles under the box, by range reads within the preview's byte
// budget.

const (
	// ZoomFactor picks the crop level: the smallest level whose long edge
	// is at least ZoomFactor times the preview level's, or the largest
	// when none is.
	ZoomFactor = 4
	// MaxZoomTiles bounds the tiles one crop decodes: a box of
	// PreviewMaxPx on 512 px tiles touches at most 3 × 3. A photo whose
	// box could touch more is refused.
	MaxZoomTiles = 9
)

// ErrOutsideFrame is a point outside the preview, or not a number.
var ErrOutsideFrame = errors.New("point outside the preview")

var errNoZoomLevel = dhm.FetchError{Reason: "no zoom level"}

// ZoomFrame is how a photo zooms: the preview its points are given in
// (PreviewWidth × PreviewHeight, as Preview serves it) and the level its
// crops are cut from. It is small and plain, so a server can keep it per
// photo and find a crop's box without reading the photo again.
type ZoomFrame struct {
	PreviewWidth  int `json:"pw"`
	PreviewHeight int `json:"ph"`
	// Level is the IFD index of the crop level, Width × Height its size
	// and TileWidth × TileHeight its tiles.
	Level      int `json:"level"`
	Width      int `json:"w"`
	Height     int `json:"h"`
	TileWidth  int `json:"tw"`
	TileHeight int `json:"th"`
}

// CropBox is a crop in the crop level's pixels: its origin and size. The
// crop is served 1:1, so a crop pixel is a level pixel less the origin.
type CropBox struct{ X, Y, W, H int }

func (f ZoomFrame) valid() bool {
	return f.PreviewWidth > 0 && f.PreviewHeight > 0 && f.Width > 0 && f.Height > 0 && f.TileWidth > 0 && f.TileHeight > 0
}

// toLevel is preview point (x, y) in the crop level's pixels: the plain
// ratio of the sizes, as for PreviewFrame.
func (f ZoomFrame) toLevel(x, y float64) (float64, float64) {
	return x * float64(f.Width) / float64(f.PreviewWidth), y * float64(f.Height) / float64(f.PreviewHeight)
}

// Box is the crop around preview point (x, y), its edges included:
// PreviewMaxPx on the long edge in the level's aspect (the whole level
// when it is smaller), centred on the point with its origin snapped to
// half a tile, then moved inside the level. Points that snap alike share
// a box, which is what a crop is cached by; the point stays within a
// quarter tile of the centre, or nearer the edge it was moved from.
func (f ZoomFrame) Box(x, y float64) (CropBox, error) {
	if !f.valid() {
		return CropBox{}, dhm.ErrBadRequest
	}
	if !(x >= 0 && y >= 0 && x <= float64(f.PreviewWidth) && y <= float64(f.PreviewHeight)) {
		return CropBox{}, ErrOutsideFrame
	}
	w, h := previewSize(f.Width, f.Height)
	lx, ly := f.toLevel(x, y)
	return CropBox{X: snapInside(lx, w, f.Width, f.TileWidth/2), Y: snapInside(ly, h, f.Height, f.TileHeight/2), W: w, H: h}, nil
}

// snapInside is the origin of a run of size centred on c, snapped to
// grid and moved inside [0, limit).
func snapInside(c float64, size, limit, grid int) int {
	g := max(grid, 1)
	v := int(math.Round((c-float64(size)/2)/float64(g))) * g
	return min(max(v, 0), limit-size)
}

// Marker is preview point (x, y) in the pixels of crop b.
func (f ZoomFrame) Marker(b CropBox, x, y float64) Pixel {
	lx, ly := f.toLevel(x, y)
	return Pixel{X: lx - float64(b.X), Y: ly - float64(b.Y)}
}

// InPreview is crop b in the preview's pixels: its top-left and
// bottom-right corners. A client maps anything it has in the preview
// (the line of sight) into the crop by it.
func (f ZoomFrame) InPreview(b CropBox) (Pixel, Pixel) {
	sx, sy := float64(f.PreviewWidth)/float64(f.Width), float64(f.PreviewHeight)/float64(f.Height)
	return Pixel{X: float64(b.X) * sx, Y: float64(b.Y) * sy}, Pixel{X: float64(b.X+b.W) * sx, Y: float64(b.Y+b.H) * sy}
}

// tilesIn is the number of tiles under b.
func (f ZoomFrame) tilesIn(b CropBox) int {
	across := (b.X+b.W-1)/f.TileWidth - b.X/f.TileWidth + 1
	down := (b.Y+b.H-1)/f.TileHeight - b.Y/f.TileHeight + 1
	return across * down
}

// maxTiles is the most tiles any box of f can touch.
func (f ZoomFrame) maxTiles() int {
	w, h := previewSize(f.Width, f.Height)
	across := min((w+f.TileWidth-2)/f.TileWidth+1, (f.Width+f.TileWidth-1)/f.TileWidth)
	down := min((h+f.TileHeight-2)/f.TileHeight+1, (f.Height+f.TileHeight-1)/f.TileHeight)
	return across * down
}

// fits reports whether b is a box Box can give for f: inside the level,
// the size of a crop, within the tile budget.
func (f ZoomFrame) fits(b CropBox) bool {
	w, h := previewSize(f.Width, f.Height)
	return f.valid() && b.W == w && b.H == h && b.X >= 0 && b.Y >= 0 && b.X+b.W <= f.Width && b.Y+b.H <= f.Height &&
		f.tilesIn(b) <= MaxZoomTiles
}

// Zoom is a photo opened for crops: its frame, and its layout for
// reading them.
type Zoom struct {
	Frame ZoomFrame
	path  string
	level *dhm.ImageLevel
	size  int64
	used  int // header bytes read, charged to every crop's budget
}

// Zoom opens the photo at path, a CDN path (CDNPath), for crops: one
// header read (extended once, as for Preview). The frame's preview is the
// one Preview cuts, so a photo without a preview has no zoom either; one
// whose crops could touch more than MaxZoomTiles tiles is refused.
func (c *CDN) Zoom(ctx context.Context, path string) (*Zoom, error) {
	if !cdnPathRe.MatchString(path) {
		return nil, dhm.ErrBadRequest
	}
	layout, size, used, err := c.header(ctx, path)
	if err != nil {
		return nil, err
	}
	pl, err := previewLevel(layout)
	if err != nil {
		return nil, err
	}
	want := ZoomFactor * max(pl.Width, pl.Height)
	i := 0
	for j, l := range layout.Levels {
		if max(l.Width, l.Height) >= want {
			i = j
		}
	}
	l := &layout.Levels[i]
	pw, ph := previewSize(pl.Width, pl.Height)
	f := ZoomFrame{PreviewWidth: pw, PreviewHeight: ph, Level: i, Width: l.Width, Height: l.Height,
		TileWidth: l.TileWidth, TileHeight: l.TileHeight}
	if f.maxTiles() > MaxZoomTiles {
		return nil, errNoZoomLevel
	}
	return &Zoom{Frame: f, path: path, level: l, size: size, used: used}, nil
}

// Crop is box b of z (from z.Frame.Box) as a JPEG of b's size: the tiles
// under it, merged into as few range reads as their layout allows,
// within the preview's byte budget with z's header counted. Any other box
// is ErrBadRequest, before a read.
func (c *CDN) Crop(ctx context.Context, z *Zoom, b CropBox) ([]byte, error) {
	if z == nil || !z.Frame.fits(b) {
		return nil, dhm.ErrBadRequest
	}
	l := z.level
	var tiles []int
	for ty := b.Y / l.TileHeight; ty <= (b.Y+b.H-1)/l.TileHeight; ty++ {
		for tx := b.X / l.TileWidth; tx <= (b.X+b.W-1)/l.TileWidth; tx++ {
			tiles = append(tiles, ty*l.TilesAcross()+tx)
		}
	}
	canvas, err := c.draw(ctx, z.path, l, tiles, image.Rect(b.X, b.Y, b.X+b.W, b.Y+b.H), z.size, MaxPreviewBytes-z.used)
	if err != nil {
		return nil, err
	}
	return c.encode(canvas)
}
