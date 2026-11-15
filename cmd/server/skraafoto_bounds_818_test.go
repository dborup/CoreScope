package main

import (
	"encoding/json"
	"testing"

	"github.com/meshcore-analyzer/dhm"
)

// #818: the client's skraafoto block carries the coverage the node action
// is limited to, dhm.Coverage as Leaflet bounds, like the other Danish
// layers' blocks.
func TestSkraafoto818_ClientConfigBounds(t *testing.T) {
	e := newSkraafotoEnv(t, SkraafotoConfig{})
	w := e.get("/api/config/client")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	var cc struct {
		Skraafoto *struct {
			Bounds *[2][2]float64 `json:"bounds"`
		} `json:"skraafoto"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &cc); err != nil || cc.Skraafoto == nil {
		t.Fatalf("%v %s", err, w.Body.String())
	}
	if cc.Skraafoto.Bounds == nil {
		t.Fatalf("no bounds in the skraafoto block: %s", w.Body.String())
	}
	want := [2][2]float64{{54.4265, 7.99125}, {57.778, 15.5995}}
	if *cc.Skraafoto.Bounds != want || want != dhmCoverageBounds() {
		t.Fatalf("bounds %v, want %v (dhm.Coverage)", *cc.Skraafoto.Bounds, want)
	}
}

// The bounds hold Denmark's extreme points, each one the search covers
// (dhm.Covers): hiding the action outside the box hides no Danish photo.
func TestSkraafoto818_BoundsHoldDenmark(t *testing.T) {
	b := dhmCoverageBounds()
	for _, p := range []struct {
		name     string
		lat, lon float64
	}{
		{"Grenen (north)", 57.7445, 10.6380},
		{"Gedser Odde (south)", 54.5600, 11.9660},
		{"Blåvandshuk (west)", 55.5580, 8.0790},
		{"Ertholmene (east)", 55.3210, 15.1890},
		{"Aarhus", 56.15674, 10.21076},
	} {
		if !dhm.Covers(dhm.ToUTM32(p.lat, p.lon)) {
			t.Fatalf("%s %.4f,%.4f: the search does not cover it", p.name, p.lat, p.lon)
		}
		if p.lat < b[0][0] || p.lat > b[1][0] || p.lon < b[0][1] || p.lon > b[1][1] {
			t.Fatalf("%s %.4f,%.4f is outside the bounds %v", p.name, p.lat, p.lon, b)
		}
	}
}
