package dhm

import (
	"math"
	"math/rand"
	"sync"
	"testing"
)

// #692 — the DHM WCS answers 0.0, not nodata, outside Denmark, so the
// profile gates every sample with the embedded Danish boundary polygon
// (land plus territorial waters). These pin the polygon, its index and
// the gate in the planner.

// Reference places. The windows in the path tests below are where the
// real boundary lies along each path, with the polygon's ≈ 1 km accuracy
// (Natural Earth 1:10m, simplified at 150 m) on top.
var (
	copenhagen    = LatLon{55.6761, 12.5683}
	malmo         = LatLon{55.6050, 13.0038}
	aabenraa      = LatLon{55.0443, 9.4174}
	flensburg     = LatLon{54.7836, 9.4321}
	tonder        = LatLon{54.9330, 8.8660}
	niebull       = LatLon{54.7880, 8.8290}
	dueodde       = LatLon{54.9900, 15.0750}
	pobierowo     = LatLon{54.0600, 14.9300} // Polish coast south of Bornholm
	kolobrzeg     = LatLon{54.1760, 15.5830}
	nyborg        = LatLon{55.3127, 10.7897}
	korsor        = LatLon{55.3297, 11.1393}
	grenaa        = LatLon{56.4158, 10.8783}
	anholt        = LatLon{56.7167, 11.5167}
	sjOdde        = LatLon{55.9740, 11.2960}
	ebeltoft      = LatLon{56.1944, 10.6797}
	laeso         = LatLon{57.2667, 11.0167}
	frederikshavn = LatLon{57.4407, 10.5366}
	himmelbjerget = LatLon{56.1050, 9.6830}
	silkeborg     = LatLon{56.1697, 9.5451}
)

func inDenmarkLL(p LatLon) bool {
	e, n := ToUTM32(p.Lat, p.Lon)
	return InDenmark(e, n)
}

func TestInDenmarkPlaces(t *testing.T) {
	for _, c := range []struct {
		name string
		p    LatLon
		want bool
	}{
		{"Copenhagen", copenhagen, true},
		{"Saltholm (Øresund, Danish side)", LatLon{55.6400, 12.7600}, true},
		{"Helsingør", LatLon{56.0361, 12.6136}, true},
		{"Padborg", LatLon{54.8256, 9.3640}, true},
		{"Great Belt mid-strait", LatLon{55.3200, 10.9600}, true},
		{"Kattegat, Sjællands Odde–Ebeltoft midpoint", LatLon{56.0842, 10.9879}, true},
		{"Anholt", anholt, true},
		{"Læsø", laeso, true},
		{"Skagen", LatLon{57.7209, 10.5839}, true},
		{"Gedser", LatLon{54.5747, 11.9269}, true},
		{"Rønne", LatLon{55.1009, 14.7067}, true},
		{"Malmö", malmo, false},
		{"Helsingborg", LatLon{56.0465, 12.6945}, false},
		{"Ven (Swedish island in Øresund)", LatLon{55.9070, 12.6900}, false},
		{"Flensburg", flensburg, false},
		{"Niebüll", niebull, false},
		{"Rostock", LatLon{54.0887, 12.1405}, false},
		{"Hamburg", LatLon{53.5500, 9.9900}, false},
		{"Kołobrzeg", kolobrzeg, false},
		{"Gothenburg", LatLon{57.7089, 11.9746}, false},
		{"North Sea, 60 km off Hvide Sande", LatLon{56.0000, 7.1500}, false},
	} {
		if got := inDenmarkLL(c.p); got != c.want {
			t.Errorf("%s: InDenmark = %v, want %v", c.name, got, c.want)
		}
	}
	for _, p := range [][2]float64{{math.NaN(), 6200000}, {600000, math.NaN()}, {math.Inf(1), 6200000}, {600000, math.Inf(-1)}} {
		if InDenmark(p[0], p[1]) {
			t.Errorf("InDenmark(%v, %v) = true", p[0], p[1])
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

func TestPlanGapStartsAtTheBorder(t *testing.T) {
	for _, c := range []struct {
		name   string
		a, b   LatLon
		lo, hi float64 // km along the path where the gap must start
	}{
		// The DK–SE boundary in Øresund runs east of Saltholm, about 12.79°E.
		{"Copenhagen→Malmö", copenhagen, malmo, 13.5, 18},
		// The land border meets Flensburg Fjord near 54.815°N here.
		{"Aabenraa→Flensburg", aabenraa, flensburg, 24, 27.5},
		// Land border at Rudbøl/Møgeltønder, about 54.905°N.
		{"Tønder→Niebüll", tonder, niebull, 2, 4.5},
		// Bornholm's 12 nm (22.2 km) territorial sea, from its southern tip.
		// (Toward Kołobrzeg, the RasterExtent edge at 15.2°E comes first.)
		{"Dueodde→Pobierowo", dueodde, pobierowo, 21, 24},
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

func TestPlanDanishPathsStayFull(t *testing.T) {
	for _, c := range []struct {
		name string
		a, b LatLon
	}{
		{"Himmelbjerget→Silkeborg", himmelbjerget, silkeborg},
		{"Great Belt, Nyborg→Korsør", nyborg, korsor},
		{"Kattegat, Grenaa→Anholt", grenaa, anholt},
		{"Kattegat, Sjællands Odde→Ebeltoft", sjOdde, ebeltoft},
		{"Kattegat, Læsø→Frederikshavn", laeso, frederikshavn},
	} {
		p := PlanProfile(c.a, c.b, defaultPlan())
		if p.Covered() != len(p.E) {
			t.Errorf("%s: covered %d of %d, first gap %.0f m", c.name, p.Covered(), len(p.E), firstGapM(&p))
		}
	}
}

// A point exactly on the boundary is inside (the polygon is closed); a
// metre off the edge, one side is inside and the other is not.
func TestInDenmarkOnTheBoundary(t *testing.T) {
	b := denmarkBoundary()
	checked := 0
	for i := 0; i < len(b.segs); i += 7 {
		s := b.segs[i]
		if !InDenmark(s.e0, s.n0) {
			t.Errorf("vertex (%v, %v) is not inside", s.e0, s.n0)
		}
		me, mn := (s.e0+s.e1)/2, (s.n0+s.n1)/2
		if !InDenmark(me, mn) {
			t.Errorf("edge midpoint (%v, %v) is not inside", me, mn)
		}
		l := math.Hypot(s.e1-s.e0, s.n1-s.n0)
		ne, nn := -(s.n1-s.n0)/l, (s.e1-s.e0)/l // unit normal
		left, right := InDenmark(me+ne, mn+nn), InDenmark(me-ne, mn-nn)
		if left == right {
			t.Errorf("edge (%v,%v)–(%v,%v): both sides 1 m off are inside=%v", s.e0, s.n0, s.e1, s.n1, left)
		}
		checked++
	}
	if checked < 50 {
		t.Fatalf("only %d edges checked", checked)
	}
}

// The cell index answers exactly as a plain even-odd test over every
// edge, including near edges and in the bbox margins.
func TestInDenmarkIndexMatchesBruteForce(t *testing.T) {
	b := denmarkBoundary()
	rng := rand.New(rand.NewSource(692))
	w, h := b.maxE-b.minE, b.maxN-b.minN
	for i := 0; i < 200000; i++ {
		e := b.minE - 1000 + rng.Float64()*(w+2000)
		n := b.minN - 1000 + rng.Float64()*(h+2000)
		if got, want := InDenmark(e, n), b.bruteForce(e, n); got != want {
			t.Fatalf("(%v, %v): index %v, brute force %v", e, n, got, want)
		}
	}
	// Near every vertex, where the edge cells are.
	for _, s := range b.segs {
		for k := 0; k < 8; k++ {
			e, n := s.e0+rng.Float64()*400-200, s.n0+rng.Float64()*400-200
			if got, want := InDenmark(e, n), b.bruteForce(e, n); got != want {
				t.Fatalf("(%v, %v) near a vertex: index %v, brute force %v", e, n, got, want)
			}
		}
	}
}

// The embedded polygon and its index stay bounded.
func TestDenmarkBoundaryIsBounded(t *testing.T) {
	if n := len(denmarkBoundaryText); n > 64<<10 {
		t.Fatalf("embedded polygon is %d bytes", n)
	}
	b := denmarkBoundary()
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

func BenchmarkInDenmark3200(b *testing.B) {
	p := borderPlan(b)
	denmarkBoundary()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := range p.E {
			InDenmark(p.E[j], p.N[j])
		}
	}
}

// The same 3200 samples through the plain even-odd test over every edge,
// for comparison with the indexed gate.
func BenchmarkInDenmarkBruteForce3200(b *testing.B) {
	p := borderPlan(b)
	bd := denmarkBoundary()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := range p.E {
			bd.bruteForce(p.E[j], p.N[j])
		}
	}
}

// The whole planner for 3200 samples, gate included.
func BenchmarkPlanProfile3200(b *testing.B) {
	denmarkBoundary()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		PlanProfile(copenhagen, toward(copenhagen, 80000, 80), defaultPlan())
	}
}

// Parsing the embedded polygon and building its cell index (once per
// process).
func BenchmarkDenmarkBoundaryLoad(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := parseBoundary(denmarkBoundaryText); err != nil {
			b.Fatal(err)
		}
	}
}

var allSegs = sync.OnceValue(func() []int32 {
	all := make([]int32, len(denmarkBoundary().segs))
	for i := range all {
		all[i] = int32(i)
	}
	return all
})

// bruteForce is contains over every edge, without the index: the
// reference for the index test and the benchmark baseline.
func (b *boundary) bruteForce(e, n float64) bool {
	return b.contains(e, n, allSegs())
}
