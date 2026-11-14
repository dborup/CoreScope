package heritage

import (
	"math"
	"testing"
)

func fixtureZones(t *testing.T) []Zone {
	t.Helper()
	page, err := Parse(readFixture(t, "zones_two.json"))
	if err != nil {
		t.Fatal(err)
	}
	return page.Zones
}

func near(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if math.IsNaN(got) || math.Abs(got-want) > tol {
		t.Fatalf("%s = %.3f, want %.3f ±%g", what, got, want, tol)
	}
}

// The fixture holds two synthetic zones shaped like the two the research
// measured (#723): a ~223 × 221 m "Rundhøj" zone centred on
// (526400, 6179600) and a ~272 × 222 m "Vandfyldt grav" zone centred on
// (526900, 6179600), each a 48-gon.
func TestNearestInsideAZone(t *testing.T) {
	r := Nearest(fixtureZones(t), 526400, 6179600, 100)
	if r.Zone == nil || r.Zone.Type != "Rundhøj" || !r.Inside || r.DistanceM != 0 || r.Count != 1 {
		t.Fatalf("%+v", r)
	}
	// Just inside the edge.
	r = Nearest(fixtureZones(t), 526400+110, 6179600, 100)
	if r.Zone == nil || !r.Inside {
		t.Fatalf("edge point %+v", r)
	}
}

func TestNearestOutsideWithinRadius(t *testing.T) {
	// 40 m east of the Rundhøj zone's eastern vertex (526511.5).
	r := Nearest(fixtureZones(t), 526551.5, 6179600, 100)
	if r.Zone == nil || r.Zone.Type != "Rundhøj" || r.Inside || r.Count != 1 {
		t.Fatalf("%+v", r)
	}
	near(t, "distance", r.DistanceM, 40, 0.01)
	// Halfway between the zones (526650): 138.5 m from Rundhøj's edge and
	// 114 m from the grave's (526764): nothing within 100 m, both within 150.
	if r := Nearest(fixtureZones(t), 526650, 6179600, 100); r.Zone != nil || r.Count != 0 {
		t.Fatalf("nothing within 100 m, got %+v", r)
	}
	r = Nearest(fixtureZones(t), 526650, 6179600, 150)
	if r.Zone == nil || r.Zone.Type != "Vandfyldt grav" || r.Count != 2 {
		t.Fatalf("nearest of two %+v", r)
	}
	near(t, "distance", r.DistanceM, 114, 0.01)
}

func TestNearestNothing(t *testing.T) {
	if r := Nearest(nil, 526400, 6179600, 100); r.Zone != nil || r.Count != 0 || r.Inside {
		t.Fatalf("%+v", r)
	}
}

// Overlapping zones: the point lies in both; the one it is deepest inside
// wins. For buffer zones of equal width that is the nearest monument.
func TestNearestOverlapPrefersTheDeeperZone(t *testing.T) {
	a := Zone{Type: "a", SystemNo: "1", Polygons: [][][][2]float64{{rect(0, 0, 200, 200)}}}
	b := Zone{Type: "b", SystemNo: "2", Polygons: [][][][2]float64{{rect(150, 0, 350, 200)}}}
	// x=170: 30 m inside a (to its x=200 edge), 20 m inside b.
	r := Nearest([]Zone{b, a}, 170, 100, 50)
	if r.Zone == nil || r.Zone.Type != "a" || !r.Inside || r.Count != 2 {
		t.Fatalf("%+v", r)
	}
	// x=175: 25 m inside both: the tie goes to the lower system number.
	r = Nearest([]Zone{b, a}, 175, 100, 50)
	if r.Zone.Type != "a" {
		t.Fatalf("tie %+v", r)
	}
}

// A tie goes to the lower system number, compared as a number when both
// are digit strings (#762 review finding 5): "9" before "10", in either
// input order, while non-numeric ones compare as text.
func TestNearestTieOrdersSystemNumbersNumerically(t *testing.T) {
	a := Zone{Type: "nine", SystemNo: "9", Polygons: [][][][2]float64{{rect(0, 0, 100, 100)}}}
	b := Zone{Type: "ten", SystemNo: "10", Polygons: [][][][2]float64{{rect(0, 0, 100, 100)}}}
	for _, zs := range [][]Zone{{a, b}, {b, a}} {
		if r := Nearest(zs, 50, 50, 10); r.Zone == nil || r.Zone.Type != "nine" || r.Count != 2 {
			t.Fatalf("%s/%s: %+v", zs[0].SystemNo, zs[1].SystemNo, r)
		}
	}
	if !lessID("9", "10") || lessID("10", "9") || lessID("9", "10a") || !lessID("A10", "A9") {
		t.Fatal("lessID order")
	}
}

// A hole is outside the zone: its edge is the distance.
func TestNearestHoleIsOutside(t *testing.T) {
	z := Zone{Type: "ring", Polygons: [][][][2]float64{{rect(0, 0, 300, 300), rect(100, 100, 200, 200)}}}
	r := Nearest([]Zone{z}, 150, 150, 100)
	if r.Zone == nil || r.Inside {
		t.Fatalf("in the hole: %+v", r)
	}
	near(t, "distance", r.DistanceM, 50, 1e-9)
	if r := Nearest([]Zone{z}, 50, 150, 100); !r.Inside {
		t.Fatalf("in the ring: %+v", r)
	}
}

// A multipolygon is inside if any of its polygons is.
func TestNearestMultiPolygon(t *testing.T) {
	z := Zone{Type: "multi", Polygons: [][][][2]float64{{rect(0, 0, 10, 10)}, {rect(100, 0, 110, 10)}}}
	if r := Nearest([]Zone{z}, 105, 5, 1); !r.Inside {
		t.Fatalf("%+v", r)
	}
	r := Nearest([]Zone{z}, 50, 5, 100)
	near(t, "distance", r.DistanceM, 40, 1e-9)
}

func rect(x0, y0, x1, y1 float64) [][2]float64 {
	return [][2]float64{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}, {x0, y0}}
}

// BenchmarkNearestFullPage is the worst case one request computes: a full
// page of MaxFeatures zones, each a 48-gon like the fixture's.
func BenchmarkNearestFullPage(b *testing.B) {
	zones := make([]Zone, MaxFeatures)
	for i := range zones {
		ring := make([][2]float64, 49)
		for k := 0; k < 48; k++ {
			a := 2 * math.Pi * float64(k) / 48
			ring[k] = [2]float64{526000 + float64(i%10)*60 + 110*math.Cos(a), 6179000 + float64(i/10)*60 + 110*math.Sin(a)}
		}
		ring[48] = ring[0]
		zones[i] = Zone{Polygons: [][][][2]float64{{ring}}}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Nearest(zones, 526300, 6179300, 250)
	}
}
