package skraafoto

import (
	"image"
	"math"
)

// downscale returns src scaled to w × h by area averaging: each output
// pixel is the mean of the source pixels under it, each weighted by how
// much of it is covered. Separable, horizontal first. previewLevel holds
// the factor below 2, where this is what a box filter should do. src is
// returned as is when w × h is its size. Alpha is opaque.
func downscale(src *image.RGBA, w, h int) *image.RGBA {
	sw, sh := src.Rect.Dx(), src.Rect.Dy()
	if w == sw && h == sh {
		return src
	}
	cx, cy := boxWeights(sw, w), boxWeights(sh, h)
	tmp := make([]float32, 3*w*sh) // rows of src height, columns of w
	for y := 0; y < sh; y++ {
		row := src.Pix[y*src.Stride:]
		out := tmp[3*w*y:]
		for x, c := range cx {
			var r, g, b float32
			for k, wt := range c.w {
				p := row[4*(c.start+k):]
				r += wt * float32(p[0])
				g += wt * float32(p[1])
				b += wt * float32(p[2])
			}
			out[3*x], out[3*x+1], out[3*x+2] = r, g, b
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y, c := range cy {
		out := dst.Pix[y*dst.Stride:]
		for x := 0; x < w; x++ {
			var r, g, b float32
			for k, wt := range c.w {
				p := tmp[3*(w*(c.start+k)+x):]
				r += wt * p[0]
				g += wt * p[1]
				b += wt * p[2]
			}
			out[4*x], out[4*x+1], out[4*x+2], out[4*x+3] = to8(r), to8(g), to8(b), 255
		}
	}
	return dst
}

// contrib is the source run under one output pixel and its weights,
// which sum to 1.
type contrib struct {
	start int
	w     []float32
}

// boxWeights maps n source pixels onto m ≤ n output pixels.
func boxWeights(n, m int) []contrib {
	scale := float64(n) / float64(m)
	out := make([]contrib, m)
	for i := range out {
		lo, hi := float64(i)*scale, float64(i+1)*scale
		s, e := int(lo), min(n, int(math.Ceil(hi)))
		c := contrib{start: s, w: make([]float32, 0, e-s)}
		for j := s; j < e; j++ {
			c.w = append(c.w, float32((math.Min(hi, float64(j+1))-math.Max(lo, float64(j)))/scale))
		}
		out[i] = c
	}
	return out
}

func to8(v float32) uint8 {
	switch {
	case v <= 0:
		return 0
	case v >= 255:
		return 255
	}
	return uint8(v + 0.5)
}
