package cardkit

import (
	"image"
	"image/color"
	"math"
	"testing"
)

func newCanvas(w, h int, bg color.NRGBA) *Canvas {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	cv := &Canvas{Img: img}
	cv.FillRect(img.Rect, bg)
	return cv
}

func TestFxClampsAndRounds(t *testing.T) {
	const lim = int32(1<<20) * FracOne
	cases := []struct {
		in   float64
		want int32
	}{
		{0, 0}, {1, FracOne}, {-1, -FracOne}, {0.5, FracOne / 2},
		{1.0 / FracOne, 1}, {0.4 / FracOne, 0},
		{math.NaN(), -lim}, {math.Inf(1), lim}, {math.Inf(-1), -lim}, {1e30, lim}, {-1e30, -lim},
	}
	for _, c := range cases {
		if got := Fx(c.in); got != c.want {
			t.Errorf("Fx(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestBlend(t *testing.T) {
	white := color.NRGBA{255, 255, 255, 255}
	cv := newCanvas(4, 4, white)
	black := color.NRGBA{0, 0, 0, 255}

	cv.Blend(1, 1, black, 255)
	if got := cv.Img.RGBAAt(1, 1); got != (color.RGBA{0, 0, 0, 255}) {
		t.Errorf("full coverage = %v", got)
	}
	cv.Blend(2, 2, black, 0)
	cv.Blend(-1, 0, black, 255) // out of bounds: ignored, no panic
	cv.Blend(4, 0, black, 255)
	if got := cv.Img.RGBAAt(2, 2); got != (color.RGBA{255, 255, 255, 255}) {
		t.Errorf("zero coverage changed the pixel: %v", got)
	}
	// Half coverage of half-alpha black on white: a quarter of the way.
	cv.Blend(3, 3, color.NRGBA{0, 0, 0, 128}, 128)
	if got := cv.Img.RGBAAt(3, 3); got.R != 191 || got.A != 255 {
		t.Errorf("partial = %v, want R 191, opaque", got)
	}
	// Coverage above 255 is clamped, not wrapped.
	cv.Blend(0, 3, black, 1000)
	if got := cv.Img.RGBAAt(0, 3); got.R != 0 {
		t.Errorf("coverage 1000 = %v, want full", got)
	}
}

func TestVSpanFractionalEnds(t *testing.T) {
	white, black := color.NRGBA{255, 255, 255, 255}, color.NRGBA{0, 0, 0, 255}
	cv := newCanvas(1, 6, white)
	// From y 1.5 to 3.25, given in reverse order.
	cv.VSpan(0, Fx(3.25), Fx(1.5), 0, 6, black)
	// Ends are integer coverage: 0.5 px is 127/255, 0.25 px is 63/255.
	want := []uint8{255, 128, 0, 192, 255, 255}
	for y, w := range want {
		if got := cv.Img.RGBAAt(0, y).R; got != w {
			t.Errorf("y=%d: R=%d, want %d", y, got, w)
		}
	}
	// Clipped entirely: nothing drawn.
	cv2 := newCanvas(1, 6, white)
	cv2.VSpan(0, Fx(0), Fx(6), 2, 2, black)
	for y := 0; y < 6; y++ {
		if cv2.Img.RGBAAt(0, y).R != 255 {
			t.Fatalf("empty clip drew at y=%d", y)
		}
	}
}

func TestDashedLines(t *testing.T) {
	white, black := color.NRGBA{255, 255, 255, 255}, color.NRGBA{0, 0, 0, 255}
	cv := newCanvas(10, 10, white)
	cv.HLine(0, 10, 0, 2, 3, black) // on 2, off 3
	var got []bool
	for x := 0; x < 10; x++ {
		got = append(got, cv.Img.RGBAAt(x, 0).R == 0)
	}
	want := []bool{true, true, false, false, false, true, true, false, false, false}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hline dash pattern = %v, want %v", got, want)
		}
	}
	cv.VLine(5, 2, 2, 6, 1, 0, black) // solid, 2 px wide
	for y := 2; y < 6; y++ {
		if cv.Img.RGBAAt(5, y).R != 0 || cv.Img.RGBAAt(6, y).R != 0 || cv.Img.RGBAAt(7, y).R != 255 {
			t.Fatalf("vline row %d wrong", y)
		}
	}
}

func TestDiscAndPillAreSymmetricAndBounded(t *testing.T) {
	white, black := color.NRGBA{255, 255, 255, 255}, color.NRGBA{0, 0, 0, 255}
	cv := newCanvas(21, 21, white)
	cv.Disc(10, 10, 6, black)
	for y := 0; y < 21; y++ {
		for x := 0; x < 21; x++ {
			a, b := cv.Img.RGBAAt(x, y), cv.Img.RGBAAt(20-x, 20-y)
			if a != b {
				t.Fatalf("disc not point-symmetric at (%d,%d): %v vs %v", x, y, a, b)
			}
		}
	}
	if cv.Img.RGBAAt(10, 10).R != 0 || cv.Img.RGBAAt(10, 2).R != 255 {
		t.Error("disc centre not filled or edge overdrawn")
	}

	cv = newCanvas(40, 20, white)
	r := image.Rect(5, 5, 35, 15)
	cv.Pill(r, black)
	for y := 0; y < 20; y++ {
		for x := 0; x < 40; x++ {
			if !(image.Point{x, y}.In(r)) && cv.Img.RGBAAt(x, y).R != 255 {
				t.Fatalf("pill drew outside its rectangle at (%d,%d)", x, y)
			}
		}
	}
	if cv.Img.RGBAAt(20, 10).R != 0 {
		t.Error("pill middle not filled")
	}
	if c := cv.Img.RGBAAt(5, 5).R; c == 0 {
		t.Error("pill corner is square")
	}
}

func TestReadableOnAndOpaque(t *testing.T) {
	dark, light := color.NRGBA{20, 20, 30, 255}, color.NRGBA{240, 240, 240, 255}
	if got := ReadableOn(color.NRGBA{250, 220, 0, 255}, light, dark); got != dark {
		t.Errorf("on yellow: %v, want the dark text", got)
	}
	if got := ReadableOn(color.NRGBA{30, 60, 160, 255}, dark, light); got != light {
		t.Errorf("on blue: %v, want the light text", got)
	}
	if got := Opaque(color.NRGBA{1, 2, 3, 0}); got != (color.NRGBA{1, 2, 3, 255}) {
		t.Errorf("Opaque = %v", got)
	}
}

func TestEncodePNGRoundTrip(t *testing.T) {
	cv := newCanvas(8, 4, color.NRGBA{10, 20, 30, 255})
	b, err := EncodePNG(cv.Img)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 8 || string(b[1:4]) != "PNG" {
		t.Fatalf("not a PNG: % x", b[:min(8, len(b))])
	}
}
