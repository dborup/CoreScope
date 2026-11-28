package dhmtest

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
)

// PNG is a real, deterministic w × h RGBA PNG shaped like a DHM hillshade
// tile: smooth grey relief with a little per-pixel noise, fully opaque.
// Different seeds give different bytes, so fakes can tell layers apart.
// At 256 × 256 it encodes to roughly the 50–60 KB of a real tile (#695).
func PNG(w, h int, seed uint32) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	s := seed*2654435761 + 1
	fs := float64(seed%97) / 7
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			s ^= s << 13
			s ^= s >> 17
			s ^= s << 5
			relief := math.Sin(float64(x)/23+fs)*math.Cos(float64(y)/31-fs) +
				0.5*math.Sin(float64(x+y)/9+fs)
			g := uint8(128 + 60*relief + float64(int(s%13)-6))
			img.SetNRGBA(x, y, color.NRGBA{g, g, g, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}
