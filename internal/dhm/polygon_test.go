package dhm

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// testWindow is the plausible EPSG:25832 box around Denmark that
// internal/heritage uses, wide enough for every zone near a cell extent.
var testWindow = [4]float64{100000, 5900000, 1100000, 6600000}

func reader(maxVertices int) *PolygonReader {
	return &PolygonReader{Window: testWindow, MaxVertices: maxVertices}
}

// square is a 100 m × 100 m ring, closed, counter-clockwise.
const square = `[[526000,6179000],[526100,6179000],[526100,6179100],[526000,6179100],[526000,6179000]]`

// TestReadPolygonAndMultiPolygon: both geometry types decode to the same
// MultiPolygon shape, a Polygon as a MultiPolygon of one, and the vertex
// budget is charged across every Read of the one reader.
func TestReadPolygonAndMultiPolygon(t *testing.T) {
	r := reader(100)
	got, err := r.Read("Polygon", json.RawMessage(`[`+square+`]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0]) != 1 || len(got[0][0]) != 5 {
		t.Fatalf("Polygon shape %v", got)
	}
	if got[0][0][1] != [2]float64{526100, 6179000} {
		t.Fatalf("easting/northing order: %v", got[0][0][1])
	}
	if r.Vertices() != 5 {
		t.Fatalf("vertices %d, want 5", r.Vertices())
	}
	got, err = r.Read("MultiPolygon", json.RawMessage(`[[`+square+`],[`+square+`]]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || len(got[0]) != 1 || len(got[1][0]) != 5 {
		t.Fatalf("MultiPolygon shape %v", got)
	}
	if r.Vertices() != 15 {
		t.Fatalf("vertices %d across both reads, want 15", r.Vertices())
	}
}

// TestReadThirdOrdinateIgnored: a position may carry a height, which is
// dropped rather than refused.
func TestReadThirdOrdinateIgnored(t *testing.T) {
	got, err := reader(100).Read("Polygon",
		json.RawMessage(`[[[526000,6179000,12.5],[526100,6179000,0],[526100,6179100,0],[526000,6179000,0]]]`))
	if err != nil {
		t.Fatal(err)
	}
	if got[0][0][0] != [2]float64{526000, 6179000} {
		t.Fatalf("height kept: %v", got[0][0][0])
	}
}

// TestReadRejects: every off-shape geometry is ErrGeometry (or an
// encoding/json error for coordinates that are not numbers), so a caller
// refuses the response instead of measuring nonsense as metres.
func TestReadRejects(t *testing.T) {
	for name, tc := range map[string]struct {
		typ, coords string
	}{
		"point geometry":        {"Point", `[526000,6179000]`},
		"linestring geometry":   {"LineString", `[[526000,6179000],[526100,6179100]]`},
		"empty geometry type":   {"", `[` + square + `]`},
		"geometrycollection":    {"GeometryCollection", `[]`},
		"polygon without rings": {"Polygon", `[]`},
		"multipolygon empty":    {"MultiPolygon", `[[]]`},
		"ring of three":         {"Polygon", `[[[526000,6179000],[526100,6179000],[526000,6179000]]]`},
		"one-value position":    {"Polygon", `[[[526000],[526100,6179000],[526100,6179100],[526000,6179000]]]`},
		"empty position":        {"Polygon", `[[[],[526100,6179000],[526100,6179100],[526000,6179000]]]`},
		"string coordinate":     {"Polygon", `[[["526000",6179000],[526100,6179000],[526100,6179100],[526000,6179000]]]`},
		"null coordinates":      {"Polygon", `null`},
		"lat/lon degrees":       {"Polygon", `[[[9.42,55.75],[9.43,55.75],[9.43,55.76],[9.42,55.75]]]`},
		"swapped axes":          {"Polygon", `[[[6179000,526000],[6179000,526100],[6179100,526100],[6179000,526000]]]`},
		"easting below window":  {"Polygon", `[[[99999.9,6179000],[526100,6179000],[526100,6179100],[99999.9,6179000]]]`},
		"easting above window":  {"Polygon", `[[[1100000.1,6179000],[526100,6179000],[526100,6179100],[1100000.1,6179000]]]`},
		"northing below window": {"Polygon", `[[[526000,5899999.9],[526100,6179000],[526100,6179100],[526000,5899999.9]]]`},
		"northing above window": {"Polygon", `[[[526000,6600000.1],[526100,6179000],[526100,6179100],[526000,6600000.1]]]`},
		"ring is a number":      {"Polygon", `[7]`},
	} {
		if _, err := reader(1000).Read(tc.typ, json.RawMessage(tc.coords)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestReadWindowEdgesInclusive: the window bounds themselves are inside it,
// so a feature touching the extent is not refused.
func TestReadWindowEdgesInclusive(t *testing.T) {
	coords := `[[[100000,5900000],[1100000,5900000],[1100000,6600000],[100000,5900000]]]`
	if _, err := reader(10).Read("Polygon", json.RawMessage(coords)); err != nil {
		t.Fatalf("window corners refused: %v", err)
	}
}

// TestReadNaNAndInf: NaN and ±Inf are not JSON numbers and a finite literal
// too large for a float64 is an encoding/json error, so no position that
// passes the window is ever non-finite. Locate's callers may therefore
// assume finite geometry.
func TestReadNaNAndInf(t *testing.T) {
	for name, coords := range map[string]string{
		"NaN literal":       `[[[NaN,6179000],[526100,6179000],[526100,6179100],[526000,6179000]]]`,
		"Infinity literal":  `[[[Infinity,6179000],[526100,6179000],[526100,6179100],[526000,6179000]]]`,
		"-Infinity literal": `[[[-Infinity,6179000],[526100,6179000],[526100,6179100],[526000,6179000]]]`,
		"quoted NaN":        `[[["NaN",6179000],[526100,6179000],[526100,6179100],[526000,6179000]]]`,
		"overflow easting":  `[[[1e400,6179000],[526100,6179000],[526100,6179100],[526000,6179000]]]`,
		"overflow northing": `[[[526000,-1e400],[526100,6179000],[526100,6179100],[526000,6179000]]]`,
	} {
		got, err := reader(1000).Read("Polygon", json.RawMessage(coords))
		if err == nil {
			t.Errorf("%s: accepted as %v", name, got)
		}
	}
	// And a finite document really does stay finite.
	got, err := reader(10).Read("Polygon", json.RawMessage(`[`+square+`]`))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got[0][0] {
		if math.IsNaN(p[0]) || math.IsInf(p[0], 0) || math.IsNaN(p[1]) || math.IsInf(p[1], 0) {
			t.Fatalf("non-finite position %v", p)
		}
	}
}

// TestReadVertexCap: the budget is the whole document's, counted inclusively
// (exactly MaxVertices is allowed, one more is refused), and it is charged
// even by the geometry that overruns it, so splitting the vertices over
// several features or rings does not evade it.
func TestReadVertexCap(t *testing.T) {
	ring := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = `[526000,6179000]`
		}
		return `[[` + strings.Join(parts, ",") + `]]`
	}
	if _, err := reader(8).Read("Polygon", json.RawMessage(ring(8))); err != nil {
		t.Fatalf("exactly MaxVertices refused: %v", err)
	}
	if _, err := reader(8).Read("Polygon", json.RawMessage(ring(9))); err == nil {
		t.Fatal("MaxVertices+1 accepted")
	}
	// Spread over two features of one reader.
	r := reader(8)
	if _, err := r.Read("Polygon", json.RawMessage(ring(5))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read("Polygon", json.RawMessage(ring(4))); err == nil {
		t.Fatal("budget not shared across reads")
	}
	// Spread over two rings of one MultiPolygon.
	if _, err := reader(8).Read("MultiPolygon", json.RawMessage(`[`+ring(5)+`,`+ring(4)+`]`)); err == nil {
		t.Fatal("budget not shared across polygons")
	}
	if r := reader(0); func() bool { _, err := r.Read("Polygon", json.RawMessage(ring(4))); return err == nil }() {
		t.Fatal("a zero budget accepted a ring")
	}
}

// outer is a 100 m square from (0,0) local, in the window; hole is the
// middle 20 m of it.
var holed = MultiPolygon{{
	{{526000, 6179000}, {526100, 6179000}, {526100, 6179100}, {526000, 6179100}, {526000, 6179000}},
	{{526040, 6179040}, {526060, 6179040}, {526060, 6179060}, {526040, 6179060}, {526040, 6179040}},
}}

// TestLocateHoles: a point in a hole is outside the polygon, and the hole's
// own edge is the nearest edge for it.
func TestLocateHoles(t *testing.T) {
	for name, tc := range map[string]struct {
		e, n       float64
		wantInside bool
		wantEdge   float64
	}{
		"centre of hole":         {526050, 6179050, false, 10},
		"just in the hole":       {526041, 6179050, false, 1},
		"just outside hole":      {526039, 6179050, true, 1},
		"between hole and outer": {526020, 6179050, true, 20},
		"outer centre line":      {526005, 6179050, true, 5},
		"outside everything":     {525990, 6179050, false, 10},
	} {
		inside, edge := Locate(holed, tc.e, tc.n)
		if inside != tc.wantInside || math.Abs(edge-tc.wantEdge) > 1e-9 {
			t.Errorf("%s: inside %v edge %.3f, want %v %.3f", name, inside, edge, tc.wantInside, tc.wantEdge)
		}
	}
}

// TestLocateMultiPolygon: inside is the union of the polygons, and the
// distance is to the nearest edge of any of them. A hole in one polygon does
// not punch through another polygon that covers it.
func TestLocateMultiPolygon(t *testing.T) {
	far := MultiPolygon{
		holed[0],
		{{{526200, 6179000}, {526300, 6179000}, {526300, 6179100}, {526200, 6179100}, {526200, 6179000}}},
	}
	if inside, edge := Locate(far, 526250, 6179050); !inside || math.Abs(edge-50) > 1e-9 {
		t.Errorf("second polygon: inside %v edge %.3f", inside, edge)
	}
	if inside, edge := Locate(far, 526150, 6179050); inside || math.Abs(edge-50) > 1e-9 {
		t.Errorf("between the polygons: inside %v edge %.3f, want false 50", inside, edge)
	}
	// The first polygon holds the point and the second does not: inside is
	// the union, not whichever polygon came last.
	if inside, edge := Locate(far, 526020, 6179050); !inside || math.Abs(edge-20) > 1e-9 {
		t.Errorf("first polygon: inside %v edge %.3f, want true 20", inside, edge)
	}
	// A second polygon covering the first's hole: the union holds the point.
	cover := MultiPolygon{
		holed[0],
		{{{526030, 6179030}, {526070, 6179030}, {526070, 6179070}, {526030, 6179070}, {526030, 6179030}}},
	}
	if inside, _ := Locate(cover, 526050, 6179050); !inside {
		t.Error("a hole covered by another polygon still excluded the point")
	}
	if inside, edge := Locate(nil, 526050, 6179050); inside || !math.IsInf(edge, 1) {
		t.Errorf("no geometry: inside %v edge %v, want false +Inf", inside, edge)
	}
	if inside, edge := Locate(MultiPolygon{{}}, 526050, 6179050); inside || !math.IsInf(edge, 1) {
		t.Errorf("polygon without rings: inside %v edge %v, want false +Inf", inside, edge)
	}
}

// TestLocateUnclosedRing: a ring whose last position does not repeat its
// first is closed implicitly, so it gives the same answer as the closed one.
func TestLocateUnclosedRing(t *testing.T) {
	closed := MultiPolygon{{{{526000, 6179000}, {526100, 6179000}, {526100, 6179100}, {526000, 6179100}, {526000, 6179000}}}}
	open := MultiPolygon{{{{526000, 6179000}, {526100, 6179000}, {526100, 6179100}, {526000, 6179100}}}}
	for _, p := range [][2]float64{{526050, 6179050}, {525990, 6179050}, {526050, 6179110}, {526000, 6179050}} {
		ci, ce := Locate(closed, p[0], p[1])
		oi, oe := Locate(open, p[0], p[1])
		if ci != oi || math.Abs(ce-oe) > 1e-9 {
			t.Errorf("%v: closed (%v, %.3f) != implicitly closed (%v, %.3f)", p, ci, ce, oi, oe)
		}
	}
}

// TestLocateTieBreaks: the crossing-number rule is half-open in northing, so
// a point level with a vertex or lying on a horizontal edge is counted once,
// not zero or twice, whatever position the ring starts at. A point exactly
// on an edge is at distance 0, and whether it reads as inside is the
// documented half-open consequence, not luck: the same point gives the same
// answer for every rotation of the ring.
func TestLocateTieBreaks(t *testing.T) {
	// A chevron: a square with a V notched into its top edge, so northing
	// 6179050 is level with both upper vertices and the ray through it meets
	// a vertex, two sloped edges and a vertical edge.
	ring := [][2]float64{
		{526000, 6179000}, {526100, 6179000}, {526100, 6179050},
		{526050, 6179020}, {526000, 6179050},
	}
	rotate := func(r [][2]float64, k int) [][2]float64 {
		out := make([][2]float64, len(r))
		for i := range r {
			out[i] = r[(i+k)%len(r)]
		}
		return out
	}
	for name, p := range map[string][2]float64{
		"level with two vertices": {526075, 6179050},
		"on a vertex":             {526100, 6179050},
		"on the bottom edge":      {526050, 6179000},
		"on a vertical edge":      {526100, 6179025},
		"inside":                  {526050, 6179010},
		"above everything":        {526050, 6179060},
	} {
		var wantInside bool
		var wantEdge float64
		for k := range ring {
			inside, edge := Locate(MultiPolygon{{rotate(ring, k)}}, p[0], p[1])
			if k == 0 {
				wantInside, wantEdge = inside, edge
				continue
			}
			if inside != wantInside || math.Abs(edge-wantEdge) > 1e-9 {
				t.Errorf("%s: rotation %d gave (%v, %.3f), rotation 0 gave (%v, %.3f)",
					name, k, inside, edge, wantInside, wantEdge)
			}
		}
	}
	// An L whose step is a horizontal edge at northing 6179050. The edge is
	// never counted (both ends are level with the ray, and the slope division
	// is skipped), so the two points straddling it disagree, as a closed
	// region requires. Counting it once or twice would make them agree.
	step := [][2]float64{
		{526000, 6179000}, {526100, 6179000}, {526100, 6179050},
		{526050, 6179050}, {526050, 6179100}, {526000, 6179100},
	}
	below, _ := Locate(MultiPolygon{{step}}, 526075, 6179049)
	above, _ := Locate(MultiPolygon{{step}}, 526075, 6179051)
	if !below || above {
		t.Fatalf("across a horizontal edge: below %v above %v, want true false", below, above)
	}
	// The rule is half-open upwards in northing: an edge counts when its
	// upper end is strictly above the ray. So the step's bottom edge reads
	// inside and its horizontal step edge, with the interior below it, reads
	// outside. Counting from the lower end instead would swap both.
	if on, edge := Locate(MultiPolygon{{step}}, 526075, 6179050); on || edge != 0 {
		t.Fatalf("on the step edge: inside %v edge %v, want false 0", on, edge)
	}
	if on, _ := Locate(MultiPolygon{{step}}, 526050, 6179000); !on {
		t.Fatal("on the bottom edge: outside, want inside")
	}
	// And half-open westwards in easting: a crossing exactly at the point
	// does not count, so a square's west edge reads inside and its east edge
	// outside. Either way the edge distance is 0, so a caller that reports
	// "inside or within r" cannot see the difference.
	sq := MultiPolygon{{{{526000, 6179000}, {526100, 6179000}, {526100, 6179100}, {526000, 6179100}, {526000, 6179000}}}}
	if west, edge := Locate(sq, 526000, 6179050); !west || edge != 0 {
		t.Fatalf("on the west edge: inside %v edge %v, want true 0", west, edge)
	}
	if east, edge := Locate(sq, 526100, 6179050); east || edge != 0 {
		t.Fatalf("on the east edge: inside %v edge %v, want false 0", east, edge)
	}
	// On an edge the distance is 0 either way.
	for name, p := range map[string][2]float64{
		"on the bottom edge": {526050, 6179000},
		"on a vertex":        {526000, 6179000},
		"on a sloped edge":   {526025, 6179035},
	} {
		if _, edge := Locate(MultiPolygon{{ring}}, p[0], p[1]); edge != 0 {
			t.Errorf("%s: edge distance %.6f, want 0", name, edge)
		}
	}
}

// TestLocateDegenerateRing: a ring of repeated positions has zero-length
// segments, which must not divide by zero; the distance is to the point.
func TestLocateDegenerateRing(t *testing.T) {
	ring := [][2]float64{{526000, 6179000}, {526000, 6179000}, {526000, 6179000}, {526000, 6179000}}
	inside, edge := Locate(MultiPolygon{{ring}}, 526003, 6179004)
	// math.IsNaN explicitly: math.Abs(NaN) > tol is false, so a division by
	// the zero segment length would slip past the comparison alone.
	if inside || math.IsNaN(edge) || math.Abs(edge-5) > 1e-9 {
		t.Fatalf("inside %v edge %v, want false 5", inside, edge)
	}
}

// TestLocateNonFinitePoint: geometry is always finite (TestReadNaNAndInf),
// but the queried point comes from a request. A NaN point is never inside
// and its distance is NaN rather than a small number that would read as a
// near miss.
func TestLocateNonFinitePoint(t *testing.T) {
	nan := math.NaN()
	for name, p := range map[string][2]float64{
		"NaN easting":  {nan, 6179050},
		"NaN northing": {526050, nan},
		"+Inf easting": {math.Inf(1), 6179050},
		"-Inf easting": {math.Inf(-1), 6179050},
	} {
		inside, edge := Locate(holed, p[0], p[1])
		if inside {
			t.Errorf("%s: inside", name)
		}
		if !math.IsNaN(edge) && !math.IsInf(edge, 1) {
			t.Errorf("%s: edge %v, want NaN or +Inf", name, edge)
		}
	}
}
