package cardkit

import (
	"image"
	"image/color"
	"math"
)

// LineLimit bounds Polyline's coordinates, in whole pixels either side of
// the origin: a segment with an end further out is skipped, which keeps
// the integer arithmetic below far from overflowing. A caller whose lines
// can run far off the card clips them to it first.
const LineLimit = 4096

// maxLineWidth caps a Polyline's width (64 px), so a bad width cannot
// turn one call into a full-canvas fill.
const maxLineWidth = 64 * FracOne

// Polyline paints the line through pts (fixed point) w wide (fixed
// point), with round caps and joins; closed adds the edge from the last
// point back to the first, for an outline. One point is a dot w across.
//
// Coverage is counted on each pixel's 4×4 subsample grid in integer
// arithmetic only, so the pixels are the same on every architecture.
// Each pixel is painted once, with the largest coverage any segment gives
// it, so a translucent line is no darker where its segments meet.
func (cv *Canvas) Polyline(pts []image.Point, w int32, closed bool, c color.NRGBA) {
	if len(pts) == 0 || w <= 0 {
		return
	}
	if w > maxLineWidth {
		w = maxLineWidth
	}
	b := cv.Img.Rect
	if n := b.Dx() * b.Dy(); len(cv.mask) != n {
		cv.mask = make([]uint8, n)
	}
	cv.touched = cv.touched[:0]
	if len(pts) == 1 {
		cv.capsule(pts[0], pts[0], int64(w)/2)
	}
	for i := 0; i+1 < len(pts); i++ {
		cv.capsule(pts[i], pts[i+1], int64(w)/2)
	}
	if closed && len(pts) > 2 {
		cv.capsule(pts[len(pts)-1], pts[0], int64(w)/2)
	}
	for _, i := range cv.touched {
		x, y := b.Min.X+int(i)%b.Dx(), b.Min.Y+int(i)/b.Dx()
		cv.Blend(x, y, c, uint32(cv.mask[i])*255/16)
		cv.mask[i] = 0
	}
}

// capsule records, in cv.mask, the coverage of every pixel by the set of
// points within r (fixed point) of the segment a–b.
func (cv *Canvas) capsule(a, b image.Point, r int64) {
	const lim = int64(LineLimit) * FracOne
	ax, ay, bx, by := int64(a.X), int64(a.Y), int64(b.X), int64(b.Y)
	for _, v := range [...]int64{ax, ay, bx, by} {
		if v < -lim || v > lim {
			return
		}
	}
	dx, dy := bx-ax, by-ay
	len2 := dx*dx + dy*dy
	// The segment's length, rounded down: an exact integer below 2^53, so
	// the square root is correctly rounded and the same everywhere.
	rl := r * int64(math.Sqrt(float64(len2)))
	r2 := r * r
	inside := func(sx, sy int64) bool {
		qx, qy := sx-ax, sy-ay
		dot := qx*dx + qy*dy
		switch {
		case dot <= 0:
			return qx*qx+qy*qy <= r2
		case dot >= len2:
			ex, ey := sx-bx, sy-by
			return ex*ex+ey*ey <= r2
		}
		cross := qx*dy - qy*dx
		if cross < 0 {
			cross = -cross
		}
		return cross <= rl
	}
	rect := cv.Img.Rect
	yLo, yHi := min(ay, by), max(ay, by)
	py0 := max(int((yLo-r)>>FracBits), rect.Min.Y)
	py1 := min(int((yHi+r)>>FracBits), rect.Max.Y-1)
	for py := py0; py <= py1; py++ {
		// The centre line is within r of this pixel row only where its y
		// is within r of the row; those x, widened by r, bound the scan.
		lo, hi := min(ax, bx), max(ax, bx)
		if dy != 0 {
			ys := max(int64(py)*FracOne-r, yLo)
			ye := min(int64(py+1)*FracOne+r, yHi)
			xs, xe := ax+dx*(ys-ay)/dy, ax+dx*(ye-ay)/dy
			lo, hi = min(xs, xe), max(xs, xe)
		}
		px0 := max(int((lo-r)>>FracBits), rect.Min.X)
		px1 := min(int((hi+r)>>FracBits), rect.Max.X-1)
		for px := px0; px <= px1; px++ {
			n := uint8(0)
			for sy := int64(0); sy < 4; sy++ {
				y := int64(py)*FracOne + (2*sy+1)*FracOne/8
				for sx := int64(0); sx < 4; sx++ {
					if inside(int64(px)*FracOne+(2*sx+1)*FracOne/8, y) {
						n++
					}
				}
			}
			if n == 0 {
				continue
			}
			i := (py-rect.Min.Y)*rect.Dx() + (px - rect.Min.X)
			if cv.mask[i] == 0 {
				cv.touched = append(cv.touched, int32(i))
			}
			if n > cv.mask[i] {
				cv.mask[i] = n
			}
		}
	}
}
