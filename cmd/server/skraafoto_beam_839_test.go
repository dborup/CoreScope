package main

// #839 finding 11 — in the zoomed crop the crosshair sits visibly off the
// line of sight. Not a projection bug: the crosshair is the point on the
// ground (skraafotoProject, at the DTM height) and the line is drawn
// between the antennas, ~20 m higher there, so in an oblique photo the two
// are apart by that height's image. These tests show it on the real Dokk1
// cameras, and pin the beam point the viewer joins them with: the line's
// own point above the crosshair.

import (
	"math"
	"testing"

	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/skraafoto"
)

// pxDist is the distance between two preview pixels.
func pxDist(a SkraafotoPixel, b skraafoto.Pixel) float64 { return math.Hypot(a.X-b.X, a.Y-b.Y) }

// polylineDist is the distance from q to the polyline path.
func polylineDist(q SkraafotoPixel, path []SkraafotoPixel) float64 {
	best := math.Inf(1)
	for i := 1; i < len(path); i++ {
		a, b := path[i-1], path[i]
		dx, dy := b.X-a.X, b.Y-a.Y
		t := 0.0
		if l2 := dx*dx + dy*dy; l2 > 0 {
			t = math.Max(0, math.Min(1, ((q.X-a.X)*dx+(q.Y-a.Y)*dy)/l2))
		}
		best = math.Min(best, math.Hypot(q.X-(a.X+t*dx), q.Y-(a.Y+t*dy)))
	}
	return best
}

// A chart point halfway along a 2 km path through Dokk1, on flat ground
// 3 m high, antennas 20 m: the crosshair is off the drawn line in every
// oblique photo, the line's point above it (the beam) is on the line, and
// the beam is exactly the camera's image of the point raised to the
// line's height there (straight between the antennas, lowered by the
// earth's bulge).
func TestSkraafoto839_BeamIsTheLineAboveThePoint(t *testing.T) {
	elev := newFakeElevation(t, flatElev(3))
	e := newSkraafotoM3aEnv(t, elev, dhm.ProfileConfig{})
	const aLat, aLon, bLat, bLon = 56.15370, 10.22300, 56.15370, 10.20490
	resp := decodeSkraafotoSearch(t, e.search(skraafotoDokk1+"&path=56.15370,10.22300,56.15370,10.20490&ha=20&hb=20"))
	pe, pn := dhm.ToUTM32(skraafotoDokk1Lat, skraafotoDokk1Lon)
	ae, an := dhm.ToUTM32(aLat, aLon)
	be, bn := dhm.ToUTM32(bLat, bLon)
	tt := ((pe-ae)*(be-ae) + (pn-an)*(bn-an)) / ((be-ae)*(be-ae) + (bn-an)*(bn-an))
	fe, fn := ae+(be-ae)*tt, an+(bn-an)*tt
	if off := math.Hypot(pe-fe, pn-fn); off > 1 {
		t.Fatalf("the fixture point is %.2f m off the line", off)
	}
	d := dhm.GroundDistance(dhm.LatLon{Lat: aLat, Lon: aLon}, dhm.LatLon{Lat: bLat, Lon: bLon})
	h := 3 + 20 - dhm.EarthBulge(tt*d, (1-tt)*d, dhm.DefaultKFactor)
	feats := skraafotoFixtureFeatures(t)
	for _, ph := range resp.Photos {
		if ph.Direction == "nadir" {
			continue // straight down, a height has (almost) no image
		}
		c := skraafotoFixtureCamera(t, feats[ph.Direction][0], ph.Direction)
		if ph.Point == nil || !ph.InFrame || len(ph.Path) < 2 {
			t.Fatalf("%s: %+v", ph.Direction, ph)
		}
		if ph.Beam == nil {
			t.Fatalf("%s: no beam point", ph.Direction)
		}
		want, _ := c.At(skraafoto.Ground{E: fe, N: fn, H: h})
		if !nearPx(*ph.Beam, want, 0.06) {
			t.Errorf("%s: beam %+v, want the line's point above the crosshair %+v", ph.Direction, *ph.Beam, want)
		}
		if got := polylineDist(*ph.Beam, ph.Path); got > 0.3 {
			t.Errorf("%s: the beam is %.2f px off the drawn line", ph.Direction, got)
		}
		// What the tester saw: the crosshair is the ground, the drop to
		// the line the image of ~20 m of height, not a projection error.
		ground, _ := c.At(skraafoto.Ground{E: pe, N: pn, H: 3})
		if !nearPx(*ph.Point, ground, 0.06) {
			t.Fatalf("%s: point %+v, ground %+v", ph.Direction, *ph.Point, ground)
		}
		drop, gap := pxDist(*ph.Point, want), polylineDist(*ph.Point, ph.Path)
		if drop < 5 {
			t.Errorf("%s: the drop is only %.2f px", ph.Direction, drop)
		}
		// From the side (north, south: across this east–west line, the
		// viewer's auto direction) the crosshair is visibly off the line;
		// looking along it (east, west) the offset runs along the line.
		if (ph.Direction == "north" || ph.Direction == "south") && gap < 5 {
			t.Errorf("%s: the crosshair is only %.2f px off the line; the fixture does not show the offset", ph.Direction, gap)
		}
		t.Logf("%s: crosshair %.1f px off the line; drop %.1f px", ph.Direction, gap, drop)
	}
}

// No beam without a line, nor for a point far off it (a node's own
// photos, or a point the line passes by): there is nothing above it.
func TestSkraafoto839_NoBeamOffTheLine(t *testing.T) {
	e := newSkraafotoM3aEnv(t, newFakeElevation(t, flatElev(3)), dhm.ProfileConfig{})
	for _, ph := range decodeSkraafotoSearch(t, e.search(skraafotoDokk1)).Photos {
		if ph.Beam != nil {
			t.Fatalf("%s: a beam without a line: %+v", ph.Direction, *ph.Beam)
		}
	}
	// A north–south line about 300 m east of Dokk1.
	for _, ph := range decodeSkraafotoSearch(t, e.search(skraafotoDokk1+"&path=56.14500,10.21880,56.16500,10.21880&ha=20&hb=20")).Photos {
		if len(ph.Path) == 0 {
			continue
		}
		if ph.Beam != nil {
			t.Fatalf("%s: a beam 300 m off the line: %+v", ph.Direction, *ph.Beam)
		}
	}
}
