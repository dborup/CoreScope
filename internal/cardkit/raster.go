package cardkit

import (
	"image"
	"image/color"
	"math"
)

// Canvas draws onto an opaque RGBA image with integer coverage arithmetic
// only, so the same input gives the same pixels on every architecture
// (see TestNoFusedMultiplyAdd). Vertical positions are fixed point with
// FracBits fractional bits; coverage is 0..255.
type Canvas struct {
	Img *image.RGBA

	// Polyline's per-pixel coverage, allocated on first use and cleared
	// after every call, and the pixels it touched.
	mask    []uint8
	touched []int32
}

const (
	FracBits = 8
	FracOne  = 1 << FracBits
)

// Fx converts a pixel coordinate to fixed point, clamped far outside the
// card so NaN and huge values cannot overflow. NaN maps to the clamp floor
// and is clipped away by every caller.
func Fx(v float64) int32 {
	const lim = 1 << 20
	if !(v > -lim) {
		return -lim * FracOne
	}
	if v > lim {
		return lim * FracOne
	}
	return int32(math.Round(float64(v * FracOne)))
}

// Blend paints c over the pixel at (x, y) with coverage cov (0..255),
// scaled by c's own alpha. Out-of-bounds pixels are ignored.
func (cv *Canvas) Blend(x, y int, c color.NRGBA, cov uint32) {
	if !(image.Point{x, y}.In(cv.Img.Rect)) || cov == 0 {
		return
	}
	if cov > 255 {
		cov = 255
	}
	a := uint32(c.A) * cov // 0..65025
	if a == 0 {
		return
	}
	const full = 255 * 255
	i := cv.Img.PixOffset(x, y)
	p := cv.Img.Pix[i : i+4 : i+4]
	inv := full - a
	p[0] = uint8((uint32(c.R)*a + uint32(p[0])*inv + full/2) / full)
	p[1] = uint8((uint32(c.G)*a + uint32(p[1])*inv + full/2) / full)
	p[2] = uint8((uint32(c.B)*a + uint32(p[2])*inv + full/2) / full)
	p[3] = 255
}

// FillRect paints r with c at full coverage.
func (cv *Canvas) FillRect(r image.Rectangle, c color.NRGBA) {
	r = r.Intersect(cv.Img.Rect)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			cv.Blend(x, y, c, 255)
		}
	}
}

// VSpan paints column x from y0 to y1 (fixed point, either order),
// clipped to [clipTop, clipBot) in whole pixels, with exact fractional
// coverage on the two end pixels.
func (cv *Canvas) VSpan(x int, y0, y1 int32, clipTop, clipBot int, c color.NRGBA) {
	if y0 > y1 {
		y0, y1 = y1, y0
	}
	top, bot := int32(clipTop)*FracOne, int32(clipBot)*FracOne
	if y0 < top {
		y0 = top
	}
	if y1 > bot {
		y1 = bot
	}
	if y1 <= y0 {
		return
	}
	for py := y0 >> FracBits; py<<FracBits < y1; py++ {
		lo, hi := py<<FracBits, (py+1)<<FracBits
		if lo < y0 {
			lo = y0
		}
		if hi > y1 {
			hi = y1
		}
		cv.Blend(x, int(py), c, uint32(hi-lo)*255/FracOne)
	}
}

// HLine paints a 1 px horizontal line from x0 to x1 at row y,
// on for on px and off for off px (off = 0: solid).
func (cv *Canvas) HLine(x0, x1, y, on, off int, c color.NRGBA) {
	for x := x0; x < x1; x++ {
		if off == 0 || (x-x0)%(on+off) < on {
			cv.Blend(x, y, c, 255)
		}
	}
}

// VLine is the vertical twin of HLine, w px wide.
func (cv *Canvas) VLine(x, w, y0, y1, on, off int, c color.NRGBA) {
	for y := y0; y < y1; y++ {
		if off == 0 || (y-y0)%(on+off) < on {
			for i := 0; i < w; i++ {
				cv.Blend(x+i, y, c, 255)
			}
		}
	}
}

// coverage8 counts, out of 16, the 4×4 subsamples of pixel (x, y) inside
// the circle with centre (cx8, cy8) and radius r8, all in eighths of a
// pixel. Integer only.
func coverage8(x, y, cx8, cy8, r8 int) uint32 {
	n := uint32(0)
	for sy := 0; sy < 4; sy++ {
		for sx := 0; sx < 4; sx++ {
			dx := 8*x + 2*sx + 1 - cx8
			dy := 8*y + 2*sy + 1 - cy8
			if dx*dx+dy*dy <= r8*r8 {
				n++
			}
		}
	}
	return n
}

// Disc paints a filled circle centred on the middle of pixel (cx, cy).
func (cv *Canvas) Disc(cx, cy, r int, c color.NRGBA) {
	for y := cy - r - 1; y <= cy+r+1; y++ {
		for x := cx - r - 1; x <= cx+r+1; x++ {
			cv.Blend(x, y, c, coverage8(x, y, 8*cx+4, 8*cy+4, 8*r)*255/16)
		}
	}
}

// Pill paints a rectangle with fully rounded left and right ends.
func (cv *Canvas) Pill(r image.Rectangle, c color.NRGBA) {
	h := r.Dy()
	rad := h / 2
	cv.FillRect(image.Rect(r.Min.X+rad, r.Min.Y, r.Max.X-rad, r.Max.Y), c)
	cy8 := 8*r.Min.Y + 4*h
	for _, capX := range [][3]int{{r.Min.X, r.Min.X + rad, 8 * (r.Min.X + rad)}, {r.Max.X - rad, r.Max.X, 8 * (r.Max.X - rad)}} {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := capX[0]; x < capX[1]; x++ {
				cv.Blend(x, y, c, coverage8(x, y, capX[2], cy8, 4*h)*255/16)
			}
		}
	}
}

// luminance is a cheap integer relative luminance (0..255·1000), enough to
// pick the more readable of two text colours on a badge.
func luminance(c color.NRGBA) int {
	return 2126*int(c.R) + 7152*int(c.G) + 722*int(c.B)
}

// ReadableOn picks whichever of a and b differs more in luminance from bg.
func ReadableOn(bg, a, b color.NRGBA) color.NRGBA {
	l := luminance(bg)
	da, db := luminance(a)-l, luminance(b)-l
	if da < 0 {
		da = -da
	}
	if db < 0 {
		db = -db
	}
	if db > da {
		return b
	}
	return a
}

// Opaque is c with full alpha: the card is never transparent.
func Opaque(c color.NRGBA) color.NRGBA {
	c.A = 255
	return c
}
