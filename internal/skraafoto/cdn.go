package skraafoto

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/draw"
	"image/jpeg"
	"sort"

	"github.com/meshcore-analyzer/dhm"
)

const (
	// PreviewMaxPx is the long edge of a preview (#737 Q8).
	PreviewMaxPx = 1024

	// HeaderBytes is the first read of a photo: the owner's check found
	// all seven IFDs of a real photo in its first 64 KiB. The live smoke
	// check (#803) reads the same range.
	HeaderBytes = 64 << 10
	// maxHeaderBytes is a header after its one extension.
	maxHeaderBytes = 1 << 20
	// MaxPreviewTiles, MaxPreviewPixels and MaxPreviewBytes bound one
	// preview: the tiles decoded, the level's pixels and every byte read
	// from the CDN, header included (#737 §4.3). IFD4 of a real photo is
	// 6 tiles and 1.1 MP.
	MaxPreviewTiles  = 16
	MaxPreviewPixels = 4 << 20
	MaxPreviewBytes  = 4 << 20
	// MaxPreviewOutput caps the JPEG served.
	MaxPreviewOutput = 400 << 10
	// spanGap is the most unread bytes between two tiles that one range
	// read still covers: GDAL writes a level's tiles back to back, with
	// a 4-byte leader and trailer, so a level is one read.
	spanGap = 64 << 10
	// previewQuality is the JPEG quality of a preview.
	previewQuality = 80
)

// CDN reads previews of photos on the image CDN. It holds no token: the
// CDN needs none, and the token lives only in STAC.
type CDN struct {
	lim limiter
}

// NewCDN returns a CDN client on o.
func NewCDN(o Options) *CDN { return &CDN{lim: newLimiter(o)} }

// Requests is the number of upstream requests sent.
func (c *CDN) Requests() int64 { return c.lim.sent.Load() }

// readRange reads r of path, a checked CDN path, holding a slot.
func (c *CDN) readRange(ctx context.Context, path string, r dhm.Range) ([]byte, int64, error) {
	target, err := CDNURL(path)
	if err != nil {
		return nil, 0, err
	}
	var body []byte
	var total int64
	err = c.lim.do(ctx, func(ctx context.Context) error {
		var err error
		body, total, err = dhm.RangeRead(ctx, c.lim.http, target, r, &c.lim.sent)
		return err
	})
	return body, total, err
}

// Preview returns the JPEG preview of the photo at path, a CDN path
// (CDNPath): at most PreviewMaxPx on the long edge, from the smallest
// overview that is at least that, or the whole photo when it is smaller.
// It reads the header (one range read, extended once when the IFDs
// continue) and that overview's tiles, merged into as few range reads as
// their layout allows; nothing else. A photo without a level inside the
// MaxPreview bounds is refused after the header, so full resolution is
// never read.
func (c *CDN) Preview(ctx context.Context, path string) ([]byte, error) {
	if !cdnPathRe.MatchString(path) {
		return nil, dhm.ErrBadRequest
	}
	layout, size, used, err := c.header(ctx, path)
	if err != nil {
		return nil, err
	}
	l, err := previewLevel(layout)
	if err != nil {
		return nil, err
	}
	tiles, err := c.tiles(ctx, path, l, size, MaxPreviewBytes-used)
	if err != nil {
		return nil, err
	}
	canvas := image.NewRGBA(image.Rect(0, 0, l.Width, l.Height))
	for i, raw := range tiles {
		if raw == nil {
			continue // sparse: left black
		}
		if err := drawTile(canvas, l, i, raw); err != nil {
			return nil, err
		}
	}
	w, h := previewSize(l.Width, l.Height)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, downscale(canvas, w, h), &jpeg.Options{Quality: previewQuality}); err != nil {
		return nil, dhm.FetchError{Reason: "encode"}
	}
	if buf.Len() > c.lim.opts.MaxOutput {
		return nil, dhm.FetchError{Reason: "oversize preview"}
	}
	return buf.Bytes(), nil
}

// header reads and parses the photo's layout, and returns it, the file's
// size and the bytes read.
func (c *CDN) header(ctx context.Context, path string) (*dhm.TiledImage, int64, int, error) {
	buf, size, err := c.readRange(ctx, path, dhm.Range{From: 0, To: HeaderBytes - 1, MaxBody: HeaderBytes})
	if err != nil {
		return nil, 0, 0, err
	}
	layout, err := dhm.ParseTiledImage(buf, size)
	if errors.Is(err, dhm.ErrShortHeader) && len(buf) == HeaderBytes {
		more, _, err2 := c.readRange(ctx, path, dhm.Range{From: HeaderBytes, To: min(maxHeaderBytes, size) - 1,
			Size: size, MaxBody: maxHeaderBytes - HeaderBytes})
		if err2 != nil {
			return nil, 0, 0, err2
		}
		buf = append(buf, more...)
		layout, err = dhm.ParseTiledImage(buf, size)
	}
	switch {
	case errors.Is(err, dhm.ErrShortHeader):
		return nil, 0, 0, dhm.FetchError{Reason: "header too large"}
	case err != nil:
		return nil, 0, 0, dhm.FetchError{Reason: "invalid header"}
	}
	return layout, size, len(buf), nil
}

var errNoLevel = dhm.FetchError{Reason: "no preview level"}

// previewLevel is the smallest level whose long edge is at least
// PreviewMaxPx, or the largest when none is; it must hold within the
// preview bounds, and be at most twice the preview, so a scaled preview
// never reads more than four times its pixels.
func previewLevel(t *dhm.TiledImage) (*dhm.ImageLevel, error) {
	best := 0
	for i := range t.Levels {
		if max(t.Levels[i].Width, t.Levels[i].Height) >= PreviewMaxPx {
			best = i
		}
	}
	l := &t.Levels[best]
	if max(l.Width, l.Height) > 2*PreviewMaxPx || l.NumTiles() > MaxPreviewTiles || l.Width*l.Height > MaxPreviewPixels {
		return nil, errNoLevel
	}
	return l, nil
}

// span is one range read covering tiles.
type span struct {
	from, to int64 // inclusive
	tiles    []int
}

// tiles reads every stored tile of l, in spans of tiles less than
// spanGap apart, within budget bytes, and returns them by index; a
// sparse tile is nil.
func (c *CDN) tiles(ctx context.Context, path string, l *dhm.ImageLevel, size int64, budget int) ([][]byte, error) {
	order := make([]int, 0, l.NumTiles())
	for i := 0; i < l.NumTiles(); i++ {
		if _, n := l.TileRange(i); n > 0 {
			order = append(order, i)
		}
	}
	sort.Slice(order, func(a, b int) bool {
		oa, _ := l.TileRange(order[a])
		ob, _ := l.TileRange(order[b])
		return oa < ob
	})
	var spans []span
	for _, i := range order {
		off, n := l.TileRange(i)
		if k := len(spans) - 1; k >= 0 && off-spans[k].to-1 <= spanGap && off+n-1-spans[k].from < int64(budget) {
			spans[k].to = max(spans[k].to, off+n-1)
			spans[k].tiles = append(spans[k].tiles, i)
			continue
		}
		spans = append(spans, span{from: off, to: off + n - 1, tiles: []int{i}})
	}
	var total int64
	for _, s := range spans {
		total += s.to - s.from + 1
	}
	if total > int64(budget) {
		return nil, dhm.FetchError{Reason: "preview too large"}
	}
	out := make([][]byte, l.NumTiles())
	for _, s := range spans {
		body, _, err := c.readRange(ctx, path, dhm.Range{From: s.from, To: s.to, Size: size, MaxBody: int(s.to - s.from + 1)})
		if err != nil {
			return nil, err
		}
		for _, i := range s.tiles {
			off, n := l.TileRange(i)
			out[i] = body[off-s.from : off-s.from+n]
		}
	}
	return out, nil
}

// previewSize is w × h scaled to PreviewMaxPx on the long edge, or as is
// when smaller.
func previewSize(w, h int) (int, int) {
	long := max(w, h)
	if long <= PreviewMaxPx {
		return w, h
	}
	scale := func(v int) int { return max(1, (v*PreviewMaxPx+long/2)/long) }
	return scale(w), scale(h)
}

var errBadTile = dhm.FetchError{Reason: "bad tile"}

// drawTile decodes tile i of l from raw and draws its pixels inside the
// level onto canvas.
func drawTile(canvas *image.RGBA, l *dhm.ImageLevel, i int, raw []byte) error {
	img, err := decodeTile(l, raw)
	if err != nil {
		return err
	}
	x0, y0 := (i%l.TilesAcross())*l.TileWidth, (i/l.TilesAcross())*l.TileHeight
	r := image.Rect(x0, y0, x0+l.TileWidth, y0+l.TileHeight).Intersect(canvas.Rect)
	switch m := img.(type) {
	case *image.CMYK:
		cmykToRGB(canvas, r, m)
	case *image.YCbCr, *image.RGBA:
		draw.Draw(canvas, r, m, image.Point{}, draw.Src)
	default:
		return errBadTile
	}
	return nil
}
