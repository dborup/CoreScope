package skraafoto

import (
	"math"
	"testing"
)

// #839 finding 11 — the crop's two mappings agree. The viewer puts the
// crosshair at the Marker header and moves the line of sight into the crop
// by the InPreview box (public/skraafoto.js toCrop: (x − x0) · W / (x1 −
// x0)). Were they different, the crosshair and the line would drift apart
// in the crop only. They are one affine map, so the gap between them in a
// crop is the overview's gap times Width/PreviewWidth: the rc19 tester's
// 10.9 px × 6.9 ≈ 75 px is a scaled real offset, not a coordinate error.
func TestZoom839_MarkerAndCropBoxAreOneMap(t *testing.T) {
	frames := []ZoomFrame{
		realZoom,
		{PreviewWidth: 1024, PreviewHeight: 765, Level: 1, Width: 7092, Height: 5296, TileWidth: 512, TileHeight: 512},
		{PreviewWidth: 1024, PreviewHeight: 683, Level: 0, Width: 1500, Height: 1000, TileWidth: 512, TileHeight: 512},
	}
	pts := [][2]float64{{0, 0}, {300.5, 400.2}, {512, 349.5}, {1023.9, 1.25}, {17, 600}}
	for _, f := range frames {
		scale := float64(f.Height) / float64(f.PreviewHeight) // a vertical gap
		for _, p := range pts {
			if p[1] > float64(f.PreviewHeight) {
				continue
			}
			b, err := f.Box(p[0], p[1])
			if err != nil {
				t.Fatal(err)
			}
			lo, hi := f.InPreview(b)
			toCrop := func(x, y float64) Pixel {
				return Pixel{X: (x - lo.X) * float64(b.W) / (hi.X - lo.X), Y: (y - lo.Y) * float64(b.H) / (hi.Y - lo.Y)}
			}
			m, c := f.Marker(b, p[0], p[1]), toCrop(p[0], p[1])
			if math.Abs(m.X-c.X) > 1e-9 || math.Abs(m.Y-c.Y) > 1e-9 {
				t.Errorf("%+v at %v: marker %+v, the line's map %+v", f, p, m, c)
			}
			// A point 10.9 px above, the line's: 10.9 × scale apart in the crop.
			up := toCrop(p[0], p[1]-10.9)
			if got := math.Hypot(up.X-m.X, up.Y-m.Y); math.Abs(got-10.9*scale) > 1e-6 {
				t.Errorf("%+v at %v: %.4f px apart in the crop, want %.4f", f, p, got, 10.9*scale)
			}
		}
	}
}
