package cardkit

import (
	"image"
	"image/color"
	"testing"
)

func fxPt(x, y float64) image.Point { return image.Point{int(Fx(x)), int(Fx(y))} }

// A 2 px line along y = 10.5 covers row 10 fully and half of rows 9 and
// 11 (two of the four subsample rows), and nothing beyond; the round caps
// stop it within 1 px of its ends.
func TestPolylineCoverage(t *testing.T) {
	white, black := color.NRGBA{255, 255, 255, 255}, color.NRGBA{0, 0, 0, 255}
	cv := newCanvas(40, 20, white)
	cv.Polyline([]image.Point{fxPt(10, 10.5), fxPt(30, 10.5)}, Fx(2), false, black)
	for y, want := range map[int]uint8{8: 255, 9: 128, 10: 0, 11: 128, 12: 255} {
		if got := cv.Img.RGBAAt(20, y).R; got != want {
			t.Errorf("row %d at x=20: R %d, want %d", y, got, want)
		}
	}
	if got := cv.Img.RGBAAt(31, 10).R; got != 255 {
		t.Errorf("past the cap: R %d, want untouched", got)
	}
	if got := cv.Img.RGBAAt(9, 10).R; got == 255 {
		t.Error("the round cap draws nothing before the start")
	}
}

// Each pixel is painted once, with the largest coverage any segment gives
// it: a translucent line is no darker at a joint than along a segment.
func TestPolylineJointsNotDarker(t *testing.T) {
	white := color.NRGBA{255, 255, 255, 255}
	half := color.NRGBA{0, 0, 0, 128}
	cv := newCanvas(60, 60, white)
	cv.Polyline([]image.Point{fxPt(10.5, 10.5), fxPt(30.5, 10.5), fxPt(30.5, 40.5)}, Fx(3), false, half)
	mid, joint := cv.Img.RGBAAt(20, 10), cv.Img.RGBAAt(30, 10)
	if mid != joint {
		t.Errorf("joint %v differs from mid-segment %v", joint, mid)
	}
	if mid.R == 255 || mid.R == 0 {
		t.Errorf("mid-segment %v: want translucent", mid)
	}
}

// closed draws the edge from the last point back to the first.
func TestPolylineClosed(t *testing.T) {
	white, black := color.NRGBA{255, 255, 255, 255}, color.NRGBA{0, 0, 0, 255}
	pts := []image.Point{fxPt(5.5, 5.5), fxPt(25.5, 5.5), fxPt(25.5, 25.5)}
	open, closed := newCanvas(32, 32, white), newCanvas(32, 32, white)
	open.Polyline(pts, Fx(1), false, black)
	closed.Polyline(pts, Fx(1), true, black)
	if got := open.Img.RGBAAt(15, 15).R; got != 255 {
		t.Errorf("open: diagonal pixel R %d, want untouched", got)
	}
	if got := closed.Img.RGBAAt(15, 15).R; got == 255 {
		t.Error("closed: no edge back to the start")
	}
}

// A segment with an end beyond ±LineLimit px is skipped (callers clip);
// the rest of the polyline is still drawn, and nothing overflows.
func TestPolylineSkipsOutOfLimitSegments(t *testing.T) {
	white, black := color.NRGBA{255, 255, 255, 255}, color.NRGBA{0, 0, 0, 255}
	cv := newCanvas(40, 20, white)
	far := image.Point{int(Fx(LineLimit + 1)), int(Fx(10.5))}
	cv.Polyline([]image.Point{far, fxPt(5, 10.5), fxPt(35, 10.5)}, Fx(2), false, black)
	if got := cv.Img.RGBAAt(20, 10).R; got != 0 {
		t.Errorf("in-limit segment: R %d, want drawn", got)
	}
	if got := cv.Img.RGBAAt(38, 10).R; got != 255 {
		t.Errorf("out-of-limit segment drawn: R %d", got)
	}
	cv.Polyline([]image.Point{{1 << 30, 1 << 30}, {-1 << 30, -1 << 30}}, Fx(2), true, black)
	cv.Polyline(nil, Fx(2), true, black)
	cv.Polyline([]image.Point{fxPt(1, 1), fxPt(2, 2)}, 0, false, black)
}

// One point is a dot of the line's width; a line off the canvas is
// clipped without panicking.
func TestPolylineDotAndClip(t *testing.T) {
	white, black := color.NRGBA{255, 255, 255, 255}, color.NRGBA{0, 0, 0, 255}
	cv := newCanvas(20, 20, white)
	cv.Polyline([]image.Point{fxPt(10.5, 10.5)}, Fx(4), false, black)
	if got := cv.Img.RGBAAt(10, 10).R; got != 0 {
		t.Errorf("dot centre R %d", got)
	}
	if got := cv.Img.RGBAAt(14, 10).R; got != 255 {
		t.Errorf("dot wider than its width: R %d", got)
	}
	cv.Polyline([]image.Point{fxPt(-50, -50), fxPt(70, 70)}, Fx(2), false, black)
	if got := cv.Img.RGBAAt(5, 5).R; got != 0 {
		t.Errorf("clipped diagonal R %d, want drawn", got)
	}
}
