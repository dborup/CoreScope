package dhm

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"math/rand"
	"sync"
	"testing"
)

// #692 — the DHM WCS answers 0.0, not nodata, outside Denmark. On the sea
// that is the true height, Danish sea or not; on Swedish, German, Polish
// or Norwegian land it is not. The profile therefore counts a sample as
// covered inside the raster extent unless it lies on the embedded foreign
// land polygon. These pin the polygon, its index and the gate in the
// planner.

// Reference places. The windows in the path tests below are where the
// real coast or border lies along each path, with the polygon's accuracy
// (Natural Earth 1:10m, simplified at 150 m) on top.
var (
	copenhagen  = LatLon{55.6761, 12.5683}
	malmo       = LatLon{55.6050, 13.0038}
	helsingor   = LatLon{56.0361, 12.6136}
	helsingborg = LatLon{56.0500, 12.7200} // inland of the 1:10m coast
	aabenraa    = LatLon{55.0443, 9.4174}
	flensburg   = LatLon{54.7836, 9.4321}
	tonder      = LatLon{54.9330, 8.8660}
	niebull     = LatLon{54.7880, 8.8290}
	ronne       = LatLon{55.1009, 14.7067}
	ystad       = LatLon{55.4400, 13.8300}
	dueodde     = LatLon{54.9900, 15.0750}
	pobierowo   = LatLon{54.0600, 14.9300} // Polish coast south of Bornholm
	nyborg      = LatLon{55.3127, 10.7897}
	korsor      = LatLon{55.3297, 11.1393}
	grenaa      = LatLon{56.4158, 10.8783}
	anholt      = LatLon{56.7167, 11.5167}
	hundested   = LatLon{55.9650, 11.8500}
	laeso       = LatLon{57.2667, 11.0167}
	gilleleje   = LatLon{56.1210, 12.3100}
	hesselo     = LatLon{56.1980, 11.7100}
	sjOdde      = LatLon{55.9740, 11.2960}
	himmelbjerg = LatLon{56.1050, 9.6830}
	silkeborg   = LatLon{56.1697, 9.5451}
)

func coversLL(p LatLon) bool {
	e, n := ToUTM32(p.Lat, p.Lon)
	return Covers(e, n)
}

func TestCoversPlaces(t *testing.T) {
	for _, c := range []struct {
		name string
		p    LatLon
		want bool
	}{
		// Danish land, islands included (Natural Earth lacks Hesselø and
		// Ertholmene; they are covered because nothing foreign is there).
		{"Copenhagen", copenhagen, true},
		{"Padborg", LatLon{54.8256, 9.3640}, true},
		{"Tønder", tonder, true},
		{"Saltholm", LatLon{55.6400, 12.7600}, true},
		{"Hesselø", hesselo, true},
		{"Christiansø", LatLon{55.3205, 15.1872}, true},
		{"Anholt", anholt, true},
		{"Læsø", laeso, true},
		{"Ærø", LatLon{54.8800, 10.4100}, true},
		{"Fanø", LatLon{55.4200, 8.4000}, true},
		{"Rømø", LatLon{55.1500, 8.5300}, true},
		{"Rønne", ronne, true},
		{"Gedser", LatLon{54.5747, 11.9269}, true},
		// The sea, Danish or foreign.
		{"Øresund east of Saltholm", LatLon{55.6600, 12.8600}, true},
		{"Øresund off Landskrona", LatLon{55.8700, 12.7700}, true},
		{"Baltic between Møn and Rügen", LatLon{54.8500, 13.6000}, true},
		{"Baltic between Bornholm and Skåne", LatLon{55.3500, 14.3000}, true},
		{"Kattegat off Varberg", LatLon{57.1000, 11.9000}, true},
		{"Skagerrak off Hirtshals", LatLon{57.7000, 10.0000}, true},
		// Foreign land.
		{"Malmö", malmo, false},
		{"Helsingborg", helsingborg, false},
		{"Ven (Swedish, mid-Øresund)", LatLon{55.9070, 12.6950}, false},
		{"Falsterbo", LatLon{55.4000, 12.8300}, false},
		{"Ystad", ystad, false},
		{"Varberg", LatLon{57.1000, 12.2500}, false},
		{"Flensburg", flensburg, false},
		{"Niebüll", niebull, false},
		{"Sylt, Westerland", LatLon{54.9070, 8.3200}, false},
		{"Rügen, Bergen", LatLon{54.4170, 13.4330}, false},
		// Outside the raster extent: no data at all.
		{"Hamburg", LatLon{53.5500, 9.9900}, false},
		{"Kołobrzeg", LatLon{54.1760, 15.5830}, false},
		{"North Sea, 60 km off Hvide Sande", LatLon{56.0000, 7.1500}, false},
	} {
		if got := coversLL(c.p); got != c.want {
			t.Errorf("%s: Covers = %v, want %v", c.name, got, c.want)
		}
	}
	for _, p := range [][2]float64{{math.NaN(), 6200000}, {600000, math.NaN()}, {math.Inf(1), 6200000}, {600000, math.Inf(-1)}} {
		if Covers(p[0], p[1]) {
			t.Errorf("Covers(%v, %v) = true", p[0], p[1])
		}
	}
}

// firstGapM is the distance to the first sample the plan does not cover,
// or -1 when every sample is covered.
func firstGapM(p *Plan) float64 {
	for i, in := range p.inside {
		if !in {
			return float64(i) * p.StepM
		}
	}
	return -1
}

func TestPlanGapStartsAtTheForeignCoast(t *testing.T) {
	for _, c := range []struct {
		name   string
		a, b   LatLon
		lo, hi float64 // km along the path where the gap must start
	}{
		// All of the Øresund is covered; the gap starts on the Swedish
		// shore, about 1.4 km before Malmö centre.
		{"Copenhagen→Malmö", copenhagen, malmo, 26, 28},
		{"Helsingør→Helsingborg", helsingor, helsingborg, 4.5, 6.3},
		// The land border meets Flensburg Fjord near 54.81°N here.
		{"Aabenraa→Flensburg", aabenraa, flensburg, 24, 27.5},
		// Land border at Rudbøl/Møgeltønder, about 54.905°N.
		{"Tønder→Niebüll", tonder, niebull, 2, 4.5},
		// The Baltic is covered all the way to the Swedish coast at Ystad.
		{"Rønne→Ystad", ronne, ystad, 63.5, 66.5},
		// The Polish coast (54.06°N) lies south of the raster extent
		// (northing 6048000, about 54.5°N here), so on this path the sea is
		// covered up to the extent edge, 62.6 km out, and the gap is the
		// extent's. No Polish land is inside the extent.
		{"Dueodde→Pobierowo", dueodde, pobierowo, 61.5, 63.5},
	} {
		p := PlanProfile(c.a, c.b, defaultPlan())
		gap := firstGapM(&p) / 1000
		if gap < c.lo || gap > c.hi {
			t.Errorf("%s: first gap at %.2f km, want %v–%v km", c.name, gap, c.lo, c.hi)
		}
		// Once out, the path stays out: no foreign sample is covered.
		for i := int(gap*1000/p.StepM) + 1; i < len(p.E); i++ {
			if p.inside[i] {
				t.Errorf("%s: sample %d (%.2f km) beyond the border is covered", c.name, i, float64(i)*p.StepM/1000)
				break
			}
		}
		if !p.inside[0] || p.inside[len(p.E)-1] {
			t.Errorf("%s: start covered %v, end covered %v", c.name, p.inside[0], p.inside[len(p.E)-1])
		}
	}
}

// Paths over Danish land and any sea with no foreign land on them are
// covered end to end, however far from a Danish coast (#692 review F1:
// Anholt→Hundested and Læsø→Gilleleje cross the open Kattegat).
func TestPlanDanishPathsStayFull(t *testing.T) {
	for _, c := range []struct {
		name string
		a, b LatLon
	}{
		{"Himmelbjerget→Silkeborg", himmelbjerg, silkeborg},
		{"Great Belt, Nyborg→Korsør", nyborg, korsor},
		{"Kattegat, Grenaa→Anholt", grenaa, anholt},
		{"Kattegat, Anholt→Hundested", anholt, hundested},
		{"Kattegat, Læsø→Gilleleje", laeso, gilleleje},
		{"Kattegat, Hesselø→Sjællands Odde", hesselo, sjOdde},
		{"Kattegat, Hesselø→Gilleleje", hesselo, gilleleje},
	} {
		p := PlanProfile(c.a, c.b, defaultPlan())
		if p.Covered() != len(p.E) {
			t.Errorf("%s: covered %d of %d, first gap %.0f m", c.name, p.Covered(), len(p.E), firstGapM(&p))
		}
	}
}

// A point exactly on the foreign coast is on foreign land (the polygon is
// closed), so it is not covered; a metre off the edge, one side is covered
// and the other is not.
func TestCoversOnTheCoast(t *testing.T) {
	b := foreignLand()
	ext := RasterExtent
	checked := 0
	for i := 0; i < len(b.segs); i += 5 {
		s := b.segs[i]
		me, mn := (s.e0+s.e1)/2, (s.n0+s.n1)/2
		// Clipped edges of the polygon lie outside the extent.
		if !(me > ext[0]+10 && me < ext[2]-10 && mn > ext[1]+10 && mn < ext[3]-10) {
			continue
		}
		if Covers(s.e0, s.n0) || Covers(me, mn) {
			t.Errorf("coast point (%v, %v) or (%v, %v) is covered", s.e0, s.n0, me, mn)
		}
		l := math.Hypot(s.e1-s.e0, s.n1-s.n0)
		ne, nn := -(s.n1-s.n0)/l, (s.e1-s.e0)/l // unit normal
		if Covers(me+ne, mn+nn) == Covers(me-ne, mn-nn) {
			t.Errorf("edge (%v,%v)–(%v,%v): both sides 1 m off cover alike", s.e0, s.n0, s.e1, s.n1)
		}
		checked++
	}
	if checked < 50 {
		t.Fatalf("only %d edges checked", checked)
	}
}

// The cell index answers exactly as a plain even-odd test over every
// edge, including near edges and in the bbox margins.
func TestForeignLandIndexMatchesBruteForce(t *testing.T) {
	b := foreignLand()
	rng := rand.New(rand.NewSource(692))
	w, h := b.maxE-b.minE, b.maxN-b.minN
	for i := 0; i < 200000; i++ {
		e := b.minE - 1000 + rng.Float64()*(w+2000)
		n := b.minN - 1000 + rng.Float64()*(h+2000)
		if got, want := b.has(e, n), b.bruteForce(e, n); got != want {
			t.Fatalf("(%v, %v): index %v, brute force %v", e, n, got, want)
		}
	}
	// Near every vertex, where the edge cells are.
	for _, s := range b.segs {
		for k := 0; k < 8; k++ {
			e, n := s.e0+rng.Float64()*400-200, s.n0+rng.Float64()*400-200
			if got, want := b.has(e, n), b.bruteForce(e, n); got != want {
				t.Fatalf("(%v, %v) near a vertex: index %v, brute force %v", e, n, got, want)
			}
		}
	}
}

// #692 review N5: cellRange widens each edge's cells by the onSegment
// tolerance. Here the square's east edge lies 0.5 mm west of the cell
// boundary at e = 1000, so a point 0.4 mm east of the boundary is on the
// edge (0.9 mm off) but in the next cell. Without the widening that cell
// is not an edge cell, and its centre answers "outside". A tiny second
// square keeps the point inside the bbox, which is checked first. The
// mirror case tests the low side.
func TestBoundaryCellRangeEps(t *testing.T) {
	east, err := parseBoundary("ring 0,0 999.9995,0 999.9995,5000 0,5000\nring 5000,0 5001,0 5001,1 5000,1\n")
	if err != nil {
		t.Fatal(err)
	}
	if !east.has(1000.0004, 2500) {
		t.Error("point 0.9 mm east of the east edge, in the next cell, is not on the edge")
	}
	if east.has(1000.002, 2500) {
		t.Error("point 2.5 mm east of the east edge is inside")
	}
	// The low side: a tiny square at the origin fixes the grid, and the
	// second square's west edge lies 0.5 mm east of the cell boundary.
	west, err := parseBoundary("ring 0,0 1,0 1,1 0,1\nring 1000.0005,2000 3000,2000 3000,4000 1000.0005,4000\n")
	if err != nil {
		t.Fatal(err)
	}
	if !west.has(999.9996, 3000) {
		t.Error("point 0.9 mm west of the west edge, in the previous cell, is not on the edge")
	}
	if west.has(999.998, 3000) {
		t.Error("point 2.5 mm west of the west edge is inside")
	}
}

// #692 review N3: the polygon itself is pinned. It is byte-reproducible;
// after a deliberate regeneration (see scripts/gen-dhm-foreign-land.py)
// update this hash and the windows of the path tests above if the coast
// moved.
const foreignLandSHA256 = "c2ef95888155813210cac7f5e00c82138f785bebaf6e85db4ebe8a899bd84cb1"

func TestForeignLandIsPinned(t *testing.T) {
	sum := sha256.Sum256([]byte(foreignLandText))
	if got := hex.EncodeToString(sum[:]); got != foreignLandSHA256 {
		t.Fatalf("foreign_land.txt sha256 %s, pinned %s: regenerate with scripts/gen-dhm-foreign-land.py and update the pin", got, foreignLandSHA256)
	}
}

// The embedded polygon and its index stay bounded.
func TestForeignLandIsBounded(t *testing.T) {
	if n := len(foreignLandText); n > 64<<10 {
		t.Fatalf("embedded polygon is %d bytes", n)
	}
	b := foreignLand()
	if len(b.segs) < 100 || len(b.segs) > 5000 {
		t.Fatalf("%d edges", len(b.segs))
	}
	if len(b.cells) != b.nx*b.ny || len(b.cells) > 1<<20 {
		t.Fatalf("%d cells (%d × %d)", len(b.cells), b.nx, b.ny)
	}
	edge := 0
	for _, c := range b.cells {
		if c == cellEdge {
			edge++
		}
	}
	// Most of the grid resolves without a point-in-polygon test.
	if frac := float64(edge) / float64(len(b.cells)); frac > 0.1 {
		t.Fatalf("%.1f %% of the cells are edge cells", 100*frac)
	}
}

func TestParseBoundaryRejectsJunk(t *testing.T) {
	for _, s := range []string{
		"",
		"# only a comment\n",
		"ring 1,2 3,4\n",
		"ring 1,2 3,4 x,6\n",
		"ring 1,2 3,4 5\n",
		"polygon 1,2 3,4 5,6\n",
	} {
		if _, err := parseBoundary(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	if _, err := parseBoundary("ring 0,0 10,0 0,10\n"); err != nil {
		t.Fatal(err)
	}
}

// ── Perf proof (AGENTS.md rule 0) ─────────────────────────────────────────

// borderPlan is a 3200-sample, 80 km path that leaves Denmark: from
// Copenhagen east across the Øresund into Skåne, so the gate meets
// interior, edge and exterior cells.
func borderPlan(b *testing.B) Plan {
	p := PlanProfile(copenhagen, toward(copenhagen, 80000, 80), defaultPlan())
	if len(p.E) != 3200 {
		b.Fatalf("%d samples", len(p.E))
	}
	return p
}

func BenchmarkCovers3200(b *testing.B) {
	p := borderPlan(b)
	foreignLand()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := range p.E {
			Covers(p.E[j], p.N[j])
		}
	}
}

// The same 3200 samples through the plain even-odd test over every edge,
// for comparison with the indexed gate.
func BenchmarkForeignLandBruteForce3200(b *testing.B) {
	p := borderPlan(b)
	bd := foreignLand()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := range p.E {
			bd.bruteForce(p.E[j], p.N[j])
		}
	}
}

// The whole planner for 3200 samples, gate included.
func BenchmarkPlanProfile3200(b *testing.B) {
	foreignLand()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		PlanProfile(copenhagen, toward(copenhagen, 80000, 80), defaultPlan())
	}
}

// Parsing the embedded polygon and building its cell index (once per
// process).
func BenchmarkForeignLandLoad(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := parseBoundary(foreignLandText); err != nil {
			b.Fatal(err)
		}
	}
}

var allSegs = sync.OnceValue(func() []int32 {
	all := make([]int32, len(foreignLand().segs))
	for i := range all {
		all[i] = int32(i)
	}
	return all
})

// bruteForce is contains over every edge of the foreign land polygon,
// without the index: the reference for the index test and the benchmark
// baseline.
func (b *boundary) bruteForce(e, n float64) bool {
	return b.contains(e, n, allSegs())
}
