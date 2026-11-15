package dhm

import (
	"math"
	"os"
	"testing"
)

// #795 — the tiled reader can tell whether a raster is on the 1° square
// the fill asked for (Fits).

// The real N55E012 header fits N55E012 and nothing next to it.
func TestTiledGeoTIFFFitsRealGLO30Header795(t *testing.T) {
	b, err := os.ReadFile("testdata/glo30-n55e012-ifds.bin")
	if err != nil {
		t.Fatal(err)
	}
	tt, err := ParseTiledGeoTIFF(b, 15746303)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name                     string
		west, south, east, north float64
		want                     bool
	}{
		{"N55E012", 12, 55, 13, 56, true},
		{"N55E013", 13, 55, 14, 56, false},
		{"N54E012", 12, 54, 13, 55, false},
		{"N53E008", 8, 53, 9, 54, false},
		{"one pixel east", 12 + 1.0/2400, 55, 13 + 1.0/2400, 56, false},
		{"one pixel south", 12, 55 - 1.0/3600, 13, 56 - 1.0/3600, false},
		{"two degrees wide", 12, 55, 14, 56, false},
		{"NaN", math.NaN(), 55, 13, 56, false},
	} {
		if got := tt.Fits(c.west, c.south, c.east, c.north); got != c.want {
			t.Errorf("%s: Fits = %v, want %v", c.name, got, c.want)
		}
	}
}

// The tolerance is half a pixel at an edge, in either direction, and
// rounding in the size. smallCOG's raster has its first pixel centre on
// 12° E 56° N, so its edges are half a pixel west and north of that.
func TestTiledGeoTIFFFitsTolerance795(t *testing.T) {
	tt, _ := parseCOG(t, smallCOG())
	const px, py = 0.01, 0.005
	g := tt.Levels[0].Georef
	for _, c := range []struct {
		name   string
		ex, ey float64 // the raster's edges east and north of the box's, in pixels
		want   bool
	}{
		{"on the box", 0, 0, true},
		{"half a pixel north-west (GLO-30)", -0.5, 0.5, true},
		{"half a pixel south-east", 0.5, -0.5, true},
		{"0.6 pixel east", 0.6, 0, false},
		{"0.6 pixel west", -0.6, 0, false},
		{"0.6 pixel north", 0, 0.6, false},
		{"0.6 pixel south", 0, -0.6, false},
	} {
		w, n := g.OriginE-c.ex*px, g.OriginN-c.ey*py
		if got := tt.Fits(w, n-24*py, w+40*px, n); got != c.want {
			t.Errorf("%s: Fits = %v, want %v", c.name, got, c.want)
		}
	}
	// Within half a pixel at every edge, but a box of another size.
	if tt.Fits(g.OriginE, g.OriginN-24*py, g.OriginE+40.4*px, g.OriginN) || tt.Fits(g.OriginE, g.OriginN-23.6*py, g.OriginE+40*px, g.OriginN) {
		t.Error("a box 0.4 pixel wider or shorter fits")
	}
}
