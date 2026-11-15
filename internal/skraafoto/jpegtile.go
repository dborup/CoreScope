package skraafoto

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"

	"github.com/meshcore-analyzer/dhm"
)

// A TIFF stores each JPEG tile as an abbreviated stream (SOI … EOI
// without tables) and the shared tables once, in JPEGTables (TIFF
// Technical Note 2). decodeTile splices them into one stream for
// image/jpeg.
//
// The colours need one more step. image/jpeg reads three components as
// YCbCr unless an Adobe APP14 marker says otherwise, and refuses four
// components without one. A photo stored as RGB (PhotometricInterpretation
// 2) — three samples, or RGB plus a fourth band as the Skråfoto photos
// are — is raw RGB(+band) in the stream, which libjpeg marks with an Adobe
// marker of transform 0 when it writes one. So for RGB the splice puts
// such a marker first; a marker in the tile's own stream comes later and
// wins. image/jpeg then gives an RGBA image for three components, and for
// four an Adobe-inverted CMYK one, which cmykToRGB turns back.

// adobeMarker is an APP14 Adobe segment with transform 0.
var adobeMarker = []byte{0xFF, 0xEE, 0, 14, 'A', 'd', 'o', 'b', 'e', 0, 100, 0, 0, 0, 0, 0}

// decodeTile decodes one stored tile of l to a TileWidth × TileHeight
// image: *image.RGBA or *image.YCbCr for three samples, an Adobe-inverted
// *image.CMYK for four.
func decodeTile(l *dhm.ImageLevel, raw []byte) (image.Image, error) {
	if len(raw) < 4 || raw[0] != 0xFF || raw[1] != 0xD8 {
		return nil, errBadTile
	}
	t := l.JPEGTables
	if t != nil && (len(t) < 4 || t[0] != 0xFF || t[1] != 0xD8 || t[len(t)-2] != 0xFF || t[len(t)-1] != 0xD9) {
		return nil, errBadTile
	}
	s := make([]byte, 0, len(adobeMarker)+len(t)+len(raw))
	s = append(s, 0xFF, 0xD8)
	if l.Photometric == 2 {
		s = append(s, adobeMarker...)
	}
	if t != nil {
		s = append(s, t[2:len(t)-2]...)
	}
	s = append(s, raw[2:]...)
	// The frame must be the tile, checked before Decode: image/jpeg
	// allocates the whole frame its SOF declares, up to 65535² from a
	// few hundred bytes, and cmykToRGB indexes a full tile. Go refuses a
	// second SOF, so Decode's bounds are these.
	if cfg, err := jpeg.DecodeConfig(bytes.NewReader(s)); err != nil || cfg.Width != l.TileWidth || cfg.Height != l.TileHeight {
		return nil, errBadTile
	}
	img, err := jpeg.Decode(bytes.NewReader(s))
	if err != nil {
		return nil, errBadTile
	}
	if m, ok := img.(*image.CMYK); ok {
		if l.Samples != 4 {
			return nil, errBadTile
		}
		if t, _ := adobeTransform(raw); t != 0 {
			// The tile's own marker says YCbCrK, which GDAL never writes
			// for RGB: refused rather than shown in wrong colours.
			return nil, errBadTile
		}
		return m, nil
	} else if l.Samples == 4 {
		return nil, errBadTile
	}
	return img, nil
}

// adobeTransform is the transform of the first Adobe APP14 marker before
// the scan of a JPEG stream.
func adobeTransform(b []byte) (byte, bool) {
	for p := 2; p+4 <= len(b) && b[p] == 0xFF; {
		m := b[p+1]
		if m == 0xDA || m == 0xD9 {
			break
		}
		n := int(binary.BigEndian.Uint16(b[p+2:]))
		if n < 2 || p+2+n > len(b) {
			break
		}
		if m == 0xEE && n >= 14 && string(b[p+4:p+9]) == "Adobe" {
			return b[p+2+n-1], true
		}
		p += 2 + n
	}
	return 0, false
}

// cmykToRGB draws the RGB of an Adobe-inverted 4-component tile onto
// canvas in r, which starts at the tile's origin; the fourth band is
// dropped.
func cmykToRGB(canvas *image.RGBA, r image.Rectangle, m *image.CMYK) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		src := m.Pix[(y-r.Min.Y)*m.Stride:]
		dst := canvas.Pix[canvas.PixOffset(r.Min.X, y):]
		for x := 0; x < r.Dx(); x++ {
			dst[4*x] = 255 - src[4*x]
			dst[4*x+1] = 255 - src[4*x+1]
			dst[4*x+2] = 255 - src[4*x+2]
			dst[4*x+3] = 255
		}
	}
}
