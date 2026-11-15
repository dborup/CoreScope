package copdem

import (
	"context"
	"math"
	"testing"

	"github.com/meshcore-analyzer/dhm/dhmtest"
)

// #795 — a COG is only used for the tile it was asked for: its
// georeference must place it on that 1° square. Codex review round 4,
// finding 2: a valid COG of another location, served under a listed
// tile's path, was sampled, every candidate clamped to its edge.

// A valid GLO-30 COG of N53E008, served for every path, fails the fill:
// no value, and no header cached. Adapted from Codex's
// TestCodexR4WrongTileGeoreferenceRejected (118 finite samples before).
func TestFillRefusesForeignTileGeoreference795(t *testing.T) {
	b := newBucket(t)
	// Real GLO-30 dimensions, blocks and overview; CRS, ranges and
	// encoding are all valid. Only the place is wrong.
	wrong := dhmtest.COG(dhmtest.GLO30Tile(53, 8, func(float64, float64) float32 { return 1234 }))
	b.Generate = func(string) []byte { return wrong }
	c := newTestClient(t, b)
	p := plan(copenhagen, malmo)
	r, err := c.Fill(context.Background(), &p)
	if failure(err) != "wrong georeference" || r.Values != nil || r.Filled != 0 {
		finite := 0
		for _, v := range r.Values {
			if !math.IsNaN(float64(v)) {
				finite++
			}
		}
		t.Fatalf("fill of a foreign tile: %v, %d finite samples, filled %d", err, finite, r.Filled)
	}
	if c.headers.Len() != 0 {
		t.Fatalf("%d foreign headers cached", c.headers.Len())
	}
}

// The extent must be the tile's square: each edge within half a pixel,
// which a PixelIsPoint tile whose first pixel centre is the tile's
// north-west corner (GLO-30) needs, and the size exactly the square's.
func TestHeaderGeoreferenceMustFitTile795(t *testing.T) {
	const w, h = 40, 24
	px, py := 1.0/w, 1.0/h
	lon, lat := float64(cph.Lon), float64(cph.Lat+1) // north-west corner
	for _, c := range []struct {
		name string
		edit func(*dhmtest.COGOptions)
		ok   bool
	}{
		// Lon0, Lat0 is the first pixel's centre: the raster's edges are
		// half a pixel west and north of it.
		{"GLO-30: edges half a pixel north-west", func(*dhmtest.COGOptions) {}, true},
		{"edges on the tile", func(o *dhmtest.COGOptions) { o.Lon0, o.Lat0 = lon+px/2, lat-py/2 }, true},
		{"edges half a pixel south-east", func(o *dhmtest.COGOptions) { o.Lon0, o.Lat0 = lon+px, lat-py }, true},
		{"edges 0.6 pixel east", func(o *dhmtest.COGOptions) { o.Lon0 = lon + 1.1*px }, false},
		{"edges 0.6 pixel south", func(o *dhmtest.COGOptions) { o.Lat0 = lat - 1.1*py }, false},
		{"edges 0.6 pixel west", func(o *dhmtest.COGOptions) { o.Lon0 = lon - 0.1*px }, false},
		{"edges 0.6 pixel north", func(o *dhmtest.COGOptions) { o.Lat0 = lat + 0.1*py }, false},
		{"the tile east of it", func(o *dhmtest.COGOptions) { o.Lon0 = lon + 1 }, false},
		{"N53E008", func(o *dhmtest.COGOptions) { o.Lon0, o.Lat0 = 8, 54 }, false},
		// Within half a pixel at every edge, but not the square's size.
		{"pixels 1% wide", func(o *dhmtest.COGOptions) { o.PixelLon = 1.01 * px }, false},
		{"pixels 1% tall", func(o *dhmtest.COGOptions) { o.PixelLat = 1.01 * py }, false},
		{"one column more", func(o *dhmtest.COGOptions) { o.Width = w + 1 }, false},
		{"one row fewer", func(o *dhmtest.COGOptions) { o.Height = h - 1 }, false},
		{"two degrees wide", func(o *dhmtest.COGOptions) { o.PixelLon = 2 * px }, false},
	} {
		o := smallCOG(cph)
		o.Overview = true
		c.edit(&o)
		b := newBucket(t)
		b.Put(cph.Path(), dhmtest.COG(o))
		cl := newTestClient(t, b)
		_, err := cl.header(context.Background(), cph)
		switch {
		case c.ok && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case !c.ok && failure(err) != "wrong georeference":
			t.Errorf("%s: %v, want wrong georeference", c.name, err)
		case !c.ok && cl.headers.Len() != 0:
			t.Errorf("%s: cached", c.name)
		}
	}
}
