package main

import (
	"bytes"
	"encoding/json"
	"image/color"
	"reflect"
	"testing"

	"github.com/meshcore-analyzer/dhm"
)

// #860 — the colour-by-elevation ramp tuned to Danish land. The server
// tells the page the ramp's id; the page sends it as the tile's `styles`,
// so a new ramp is a new tile URL that no browser or shared cache holds
// (tiles are `public, max-age=86400`). No test here reaches the network.

// The client config's elevation entry carries the ramp: its stops for the
// legend and its id for the tile URL. No other entry gets a style.
func TestDHM860_ClientConfigNamesTheRamp(t *testing.T) {
	var stops []int
	for _, s := range dhm.ElevationRamp {
		stops = append(stops, s.M)
	}
	for _, e := range dhmClientConfig().Overlays {
		if e.ID != "elevation" {
			if e.Style != "" {
				t.Errorf("%s: style %q", e.ID, e.Style)
			}
			continue
		}
		if e.Style != dhm.ElevationStyle {
			t.Errorf("style %q, want %q", e.Style, dhm.ElevationStyle)
		}
		if !reflect.DeepEqual(e.LegendM, stops) {
			t.Errorf("legendM %v, want %v", e.LegendM, stops)
		}
	}
	raw, _ := json.Marshal(dhmClientConfig())
	if !bytes.Contains(raw, []byte(`"style":"`+dhm.ElevationStyle+`"`)) {
		t.Errorf("no style on the wire: %s", raw)
	}
}

// The tile URL with the ramp's id is served. The empty `styles` of a page
// loaded before #860 is the same tile, from the same cache entry, for one
// release. Another id is refused before any upstream call.
func TestDHM860_TileURLNamesTheRamp(t *testing.T) {
	up := newWCSUpstream(t, 60, nil)
	logs := &dhmLogSink{}
	p := newElevationProxy(t, up, logs, nil)
	q := overlayTile("elevation", 12, 0)
	q.Set("styles", dhm.ElevationStyle)
	w := doTile(p, q)
	if w.Code != 200 || w.Header().Get("X-CoreScope-Tile") != "miss" {
		t.Fatalf("ramp id: status %d %s", w.Code, w.Header().Get("X-CoreScope-Tile"))
	}
	// 60 m on the #860 ramp: 10/15 of the way from 50 m to 65 m.
	if got, want := elevationColour(t, w), (color.NRGBA{0xef, 0x7d, 0x2a, 255}); got != want {
		t.Fatalf("centre %v, want %v", got, want)
	}
	first := w.Body.Bytes()
	q.Set("styles", "")
	w = doTile(p, q)
	if w.Code != 200 || w.Header().Get("X-CoreScope-Tile") != "hit" || !bytes.Equal(w.Body.Bytes(), first) {
		t.Fatalf("empty styles: status %d %s, same tile %v", w.Code, w.Header().Get("X-CoreScope-Tile"), bytes.Equal(w.Body.Bytes(), first))
	}
	calls := up.hits.Load()
	for _, s := range []string{"ramp-00000000", "default", dhm.ElevationStyle + "x"} {
		q.Set("styles", s)
		if w := doTile(p, q); w.Code != 400 {
			t.Errorf("styles %q: status %d", s, w.Code)
		}
	}
	q = overlayTile("terrain", 12, 0)
	q.Set("styles", dhm.ElevationStyle)
	if w := doTile(p, q); w.Code != 400 {
		t.Errorf("terrain with the ramp id: status %d", w.Code)
	}
	if up.hits.Load() != calls {
		t.Errorf("a refused tile went upstream: %d calls, want %d", up.hits.Load(), calls)
	}
	assertNoToken(t, "elevation #860", w, logs)
}
