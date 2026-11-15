package dhmtest

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math"
)

// JPEG is a real, deterministic w × h baseline JPEG shaped like an
// orthophoto tile (#728): fields and roofs in a few colours with per-pixel
// texture, fully opaque. Different seeds give different bytes. At
// 256 × 256 it encodes to some 15–40 KB, the #728 plan's estimate for
// 10 cm imagery of built-up areas at z18–20 (real sizes are measured on
// staging).
func JPEG(w, h int, seed uint32) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	s := seed*2654435761 + 1
	fs := float64(seed%97) / 7
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			s ^= s << 13
			s ^= s >> 17
			s ^= s << 5
			field := math.Sin(float64(x)/19+fs) * math.Cos(float64(y)/27-fs)
			n := float64(int(s%71) - 35)
			img.SetRGBA(x, y, color.RGBA{
				R: clamp8(110 + 50*field + n),
				G: clamp8(130 + 40*field + n),
				B: clamp8(90 - 30*field + n),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 75}); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func clamp8(v float64) uint8 {
	return uint8(math.Max(0, math.Min(255, v)))
}
