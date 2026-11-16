package main

// #737 M3b — GET /api/skraafoto/preview/{collection}/{item}?zoom=1&x=&y=:
// a crop around point (x, y) of the photo's preview (the pixels M3a's
// search gives), cut 1:1 from a finer overview (IFD2 of a real photo,
// about 0.3 m/px against the preview's 1.1) by internal/skraafoto. At
// most 1024 px on the long edge, a JPEG like the preview. The point in
// the crop's pixels is in X-CoreScope-Skraafoto-Marker ("x,y"), and the
// crop's box in the preview's pixels in X-CoreScope-Skraafoto-Crop
// ("x0,y0,x1,y1"), so a viewer can draw the crosshair and the line of
// sight on it.
//
// A crop is a preview for fair use and caching: the same per-client and
// daily budgets, admission and coalescing, the same bounded cache. It is
// cached by the photo, the crop level and the box, whose origin is on a
// half-tile grid of the level, so nearby points share one crop. How a
// photo zooms (skraafoto.ZoomFrame) is cached per photo, which is what
// lets a request find its box, and a point outside the preview be
// refused, without reading the photo again.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/skraafoto"
)

const (
	skraafotoMarkerHeader = "X-CoreScope-Skraafoto-Marker"
	skraafotoCropHeader   = "X-CoreScope-Skraafoto-Crop"
)

// skraafotoImageKey is an image of the preview cache: a photo's preview
// (zoom false) or a crop of it.
type skraafotoImageKey struct {
	photo skraafotoPhotoKey
	zoom  bool
	level int
	box   skraafoto.CropBox
}

// skraafotoCropKey is the key of crop b of photo, which zooms as f.
func skraafotoCropKey(photo skraafotoPhotoKey, f skraafoto.ZoomFrame, b skraafoto.CropBox) skraafotoImageKey {
	return skraafotoImageKey{photo: photo, zoom: true, level: f.Level, box: b}
}

// parseSkraafotoZoom accepts zoom=1, x and y, each exactly once, x and y
// plain decimals of at most PreviewMaxPx: the preview pixel to zoom on.
// Whether it is inside the photo's preview is the frame's to say.
func parseSkraafotoZoom(raw string) (x, y float64, err error) {
	q, err := url.ParseQuery(raw)
	if err != nil || strings.Contains("&"+raw+"&", "&&") {
		return 0, 0, errSkraafotoRequest
	}
	v, err := dhm.ExactQuery(q, map[string]string{"zoom": "1"}, "x", "y")
	if err != nil {
		return 0, 0, errSkraafotoRequest
	}
	x, okX := parseTerrainNumber(v["x"], terrainNumberRe)
	y, okY := parseTerrainNumber(v["y"], terrainNumberRe)
	if !okX || !okY || x > skraafoto.PreviewMaxPx || y > skraafoto.PreviewMaxPx {
		return 0, 0, errSkraafotoRequest
	}
	return x, y, nil
}

// zoomFrame is how photo zooms, when cached.
func (p *skraafotoProxy) zoomFrame(photo skraafotoPhotoKey) (skraafoto.ZoomFrame, bool) {
	var f skraafoto.ZoomFrame
	data, ok := p.zooms.Get(photo, p.now())
	return f, ok && json.Unmarshal(data, &f) == nil
}

func (s *Server) handleSkraafotoZoom(w http.ResponseWriter, r *http.Request, photo skraafotoPhotoKey) {
	p := s.skraafoto
	x, y, err := parseSkraafotoZoom(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, errSkraafotoRequest.Error())
		return
	}
	num := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	// Concurrent requests of one box share the upstream call; while the
	// box is unknown, those of one point.
	flight := "z " + photo.collection + " " + photo.id + " " + num(x) + " " + num(y)
	if f, ok := p.zoomFrame(photo); ok {
		b, err := f.Box(x, y)
		if err != nil {
			writeError(w, http.StatusBadRequest, errSkraafotoRequest.Error())
			return
		}
		if data, ok := p.previews.Get(skraafotoCropKey(photo, f, b), p.now()); ok {
			writeSkraafotoCrop(w, data, x, y, "hit")
			return
		}
		flight = "zb " + photo.collection + " " + photo.id + " " + strconv.Itoa(f.Level) + " " + strconv.Itoa(b.X) + " " + strconv.Itoa(b.Y)
	}
	data, err := p.cold(r, p.previewLimiter, flight,
		func(ctx context.Context) ([]byte, error) { return p.zoom(ctx, photo, x, y) })
	if err != nil {
		p.writeFetchError(w, r, p.previewLimiter, "preview", err)
		return
	}
	writeSkraafotoCrop(w, data, x, y, "miss")
}

// zoom runs a cold crop: where the photo is, its header (how it zooms,
// cached), then, unless another request has meanwhile cached the box,
// the box's tiles.
func (p *skraafotoProxy) zoom(ctx context.Context, photo skraafotoPhotoKey, x, y float64) ([]byte, error) {
	path, err := p.photoPath(ctx, photo)
	if err != nil {
		return nil, err
	}
	z, err := p.cdn.Zoom(ctx, path)
	if err != nil {
		return nil, err
	}
	p.zooms.Put(photo, mustMarshal(z.Frame), p.now())
	b, err := z.Frame.Box(x, y)
	if err != nil {
		return nil, err
	}
	key := skraafotoCropKey(photo, z.Frame, b)
	if data, ok := p.previews.Get(key, p.now()); ok {
		return data, nil
	}
	jpg, err := p.cdn.Crop(ctx, z, b)
	if err != nil {
		return nil, err
	}
	data := encodeSkraafotoCrop(z.Frame, b, jpg)
	p.previews.Put(key, data, p.now())
	return data, nil
}

// A cached crop is the frame and box it was cut by, each field a
// big-endian uint32, then the JPEG: what a hit needs to place its own
// point.
const skraafotoCropHead = 11 * 4

func encodeSkraafotoCrop(f skraafoto.ZoomFrame, b skraafoto.CropBox, jpg []byte) []byte {
	out := make([]byte, 0, skraafotoCropHead+len(jpg))
	for _, v := range []int{f.PreviewWidth, f.PreviewHeight, f.Level, f.Width, f.Height, f.TileWidth, f.TileHeight, b.X, b.Y, b.W, b.H} {
		out = binary.BigEndian.AppendUint32(out, uint32(v))
	}
	return append(out, jpg...)
}

func decodeSkraafotoCrop(data []byte) (skraafoto.ZoomFrame, skraafoto.CropBox, []byte, bool) {
	if len(data) < skraafotoCropHead {
		return skraafoto.ZoomFrame{}, skraafoto.CropBox{}, nil, false
	}
	var v [11]int
	for i := range v {
		v[i] = int(binary.BigEndian.Uint32(data[4*i:]))
	}
	f := skraafoto.ZoomFrame{PreviewWidth: v[0], PreviewHeight: v[1], Level: v[2], Width: v[3], Height: v[4], TileWidth: v[5], TileHeight: v[6]}
	return f, skraafoto.CropBox{X: v[7], Y: v[8], W: v[9], H: v[10]}, data[skraafotoCropHead:], true
}

// writeSkraafotoCrop serves a cached crop with point (x, y) placed in it.
func writeSkraafotoCrop(w http.ResponseWriter, data []byte, x, y float64, state string) {
	f, b, jpg, ok := decodeSkraafotoCrop(data)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	m := f.Marker(b, x, y)
	lo, hi := f.InPreview(b)
	h := w.Header()
	h.Set(skraafotoMarkerHeader, skraafotoNumbers(m.X, m.Y))
	h.Set(skraafotoCropHeader, skraafotoNumbers(lo.X, lo.Y, hi.X, hi.Y))
	writeSkraafotoJPEG(w, jpg, state)
}

// skraafotoNumbers is vs to two decimals, comma-separated.
func skraafotoNumbers(vs ...float64) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = strconv.FormatFloat(v, 'f', 2, 64)
	}
	return strings.Join(parts, ",")
}
