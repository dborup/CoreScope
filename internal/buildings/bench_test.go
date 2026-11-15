package buildings

import (
	"encoding/json"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/meshcore-analyzer/dhm"
)

// The #730 M1b perf proof. The research measured an 80 km profile at
// r = 15 m with 1,214 buildings (~1.5 MB of GeoJSON over 8 segments) and
// the densest 10 km (Aarhus C) at 493; the profile has at most 3,200
// samples (map.dhm.profile.maxSamples). Targets from the research: parse
// the 80 km corridor in ≤ 30 ms, classify 3,200 samples against 1,214
// footprints in ≤ 5 ms.

// realProperties are the 25 attributes of one real feature of the Dokk1
// sample, so a synthetic page decodes like the service's: 1,238 bytes
// per feature on average with the geometry, as the research's 1.22 KB.
const realProperties = `{"ogc_fid":3116426,"beginlifespanversion":"2017-07-13T11:43:20Z","buildingnature":null,"conditionofconstruction":"functional","currentuse":null,"dateofconstruction":null,"dateofdemolition":null,"dateofrenovation":null,"endlifespanversion":null,"externalreferenceinfosystem":"https://datafordeler.dk","externalreferenceinfosystemname":"Datafordeler,Bygnings- og Boligregisteret (BBR)","externalreferencereference":"b738fed9-754c-4e56-b557-bacd1fbf2945","heightaboveground":null,"horizontalgeometryreference":"roofEdge","horizontalgeorefaccuracy":"0.10","horizontalgeorefaccuracy_uom":"m","inspireid":"1060529569","name":null,"numberofbuildingsunits":null,"numberofdwellings":null,"numberoffloorsaboveground":null,"referencegeometry":1,"vericalgeometryreference":"generalRoofEdge","verticalgeorefaccuracy":"0.15","verticalgeorefaccuracy_uom":"m"}`

// bench80km is the 80 km reference: A at Dokk1, B 80 km due west (eight
// 10 km segments), 3,200 samples, every one elevated by 10 m so every
// window is computed (the worst case), and 1,214 footprints, copies of the
// 25 real Dokk1 shapes centred at random points within ±15 m of the line.
type bench80km struct {
	c       Corridor
	s       Samples
	pages   []*Page
	bodies  [][]byte // each segment's page as the service would send it
	byteLen int
}

func newBench80km(tb testing.TB) bench80km {
	tb.Helper()
	fixture, err := Parse(readFixture(tb, "dokk1_cell.json"))
	if err != nil {
		tb.Fatal(err)
	}
	c := mustCorridor(tb, dokk1, [2]float64{dokk1[0] - 80000, dokk1[1]})
	s := profileAlong(c, 80000.0/3199, 40, constant(10))
	if len(s.E) != 3200 {
		tb.Fatalf("%d samples", len(s.E))
	}
	segs := c.Segments()
	b := bench80km{c: c, s: s, pages: make([]*Page, len(segs))}
	feats := make([][]string, len(segs))
	for k := range b.pages {
		b.pages[k] = &Page{}
	}
	rng := rand.New(rand.NewSource(1214))
	for j := 0; j < 1214; j++ {
		src := fixture.Footprints[j%len(fixture.Footprints)]
		cs, ct := rng.Float64()*c.LengthM(), (rng.Float64()-0.5)*30
		fp := centredAt(c, src, cs, ct)
		k := c.segmentOf(cs, 0)
		b.pages[k].Footprints = append(b.pages[k].Footprints, fp)
		feats[k] = append(feats[k], geoJSONFeature(j, fp))
	}
	for k := range segs {
		body := []byte(`{"type":"FeatureCollection","features":[` + strings.Join(feats[k], ",") +
			`],"numberMatched":` + strconv.Itoa(len(feats[k])) + `,"numberReturned":` + strconv.Itoa(len(feats[k])) + `}`)
		b.bodies = append(b.bodies, body)
		b.byteLen += len(body)
	}
	return b
}

// centredAt is fp moved so its bbox centre is at (cs, ct) in c's frame.
func centredAt(c Corridor, fp dhm.MultiPolygon, cs, ct float64) dhm.MultiPolygon {
	minE, minN, maxE, maxN := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, v := range fp[0][0] {
		minE, maxE = math.Min(minE, v[0]), math.Max(maxE, v[0])
		minN, maxN = math.Min(minN, v[1]), math.Max(maxN, v[1])
	}
	e, n := c.point(cs, ct)
	de, dn := e-(minE+maxE)/2, n-(minN+maxN)/2
	out := make(dhm.MultiPolygon, len(fp))
	for i, poly := range fp {
		out[i] = make([][][2]float64, len(poly))
		for j, ring := range poly {
			out[i][j] = make([][2]float64, len(ring))
			for k, v := range ring {
				out[i][j][k] = [2]float64{math.Round((v[0]+de)*100) / 100, math.Round((v[1]+dn)*100) / 100}
			}
		}
	}
	return out
}

func geoJSONFeature(id int, fp dhm.MultiPolygon) string {
	coords, _ := json.Marshal(fp)
	return `{"type":"Feature","id":"building.` + strconv.Itoa(id) + `","geometry":{"type":"MultiPolygon","coordinates":` +
		string(coords) + `},"geometry_name":"geometry","properties":` + realProperties + `}`
}

// TestBench80kmIsRealistic: the synthetic corridor has the research's
// size, its pages parse back to the footprints, and the bucket index
// classifies its real-shaped footprints exactly like the naive scan.
func TestBench80kmIsRealistic(t *testing.T) {
	b := newBench80km(t)
	if b.byteLen < 1300000 || b.byteLen > 1700000 {
		t.Fatalf("%d bytes over 8 pages, want ~1.5 MB", b.byteLen)
	}
	for k, body := range b.bodies {
		p, err := Parse(body)
		if err != nil || len(p.Footprints) != len(b.pages[k].Footprints) || p.Truncated {
			t.Fatalf("page %d: %d footprints, %v", k, len(p.Footprints), err)
		}
	}
	got := mustClassify(t, b.c, b.s, b.pages)
	// The naive scan takes ~2 s for all 3,200 samples; the first segment's
	// 400 are enough to check the real shapes.
	first := b.s
	first.E, first.N, first.DTM, first.DSM = first.E[:400], first.N[:400], first.DTM[:400], first.DSM[:400]
	want := classifyNaive(b.c, first, b.pages, DefaultOptions())
	buildings := 0
	for i := range got {
		if i < len(want) && got[i] != want[i] {
			t.Fatalf("sample %d: index %q, naive %q", i, got[i], want[i])
		}
		if got[i] == Building {
			buildings++
		}
	}
	t.Logf("80 km: %d bytes, %d of %d samples buildings", b.byteLen, buildings, len(got))
	if buildings == 0 || buildings == len(got) {
		t.Fatalf("%d buildings: the benchmark measures nothing", buildings)
	}
}

// BenchmarkParseCorridor80km parses the eight pages of the 80 km
// corridor, ~1.5 MB and 1,214 footprints. Target ≤ 30 ms.
func BenchmarkParseCorridor80km(b *testing.B) {
	bb := newBench80km(b)
	b.SetBytes(int64(bb.byteLen))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, body := range bb.bodies {
			if _, err := Parse(body); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkParseFullPage parses one page of MaxFeatures real-shaped
// footprints (~1.2 MB), the most one request returns.
func BenchmarkParseFullPage(b *testing.B) {
	fixture, err := Parse(readFixture(b, "dokk1_cell.json"))
	if err != nil {
		b.Fatal(err)
	}
	c := mustCorridor(b, dokk1, [2]float64{dokk1[0] - 10000, dokk1[1]})
	feats := make([]string, MaxFeatures)
	for j := range feats {
		feats[j] = geoJSONFeature(j, centredAt(c, fixture.Footprints[j%len(fixture.Footprints)], float64(j)*10, 0))
	}
	body := []byte(`{"type":"FeatureCollection","features":[` + strings.Join(feats, ",") + `],"numberMatched":1000,"numberReturned":1000}`)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Parse(body); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkClassify80km classifies 3,200 samples, all elevated, against
// 1,214 footprints with the bucket index. Target ≤ 5 ms.
func BenchmarkClassify80km(b *testing.B) {
	bb := newBench80km(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Classify(bb.c, bb.s, bb.pages, DefaultOptions()); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkClassify80kmNaive is the same without the index: every window
// point against every footprint of its segment. It is the "before".
func BenchmarkClassify80kmNaive(b *testing.B) {
	bb := newBench80km(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		classifyNaive(bb.c, bb.s, bb.pages, DefaultOptions())
	}
}

// BenchmarkClassifyAtIncidenceCap is the worst a single 10 km segment can
// cost: polygons packed to exactly maxIncidences, all in the two 25 m
// buckets the windows around 5 km look at, each a thin diagonal sliver
// whose box covers those windows but whose area holds none of their
// points. Every window point there passes every box and tests every edge.
func BenchmarkClassifyAtIncidenceCap(b *testing.B) {
	c := mustCorridor(b, dokk1, [2]float64{dokk1[0] - 10000, dokk1[1]})
	s := profileAlong(c, 25, 40, constant(10))
	const vertices = 250 // 124 along the diagonal, back 5 cm off it, closed
	page := &Page{}
	for incidences := 0; incidences+vertices*2 <= maxIncidences; incidences += vertices * 2 {
		ring := make([][2]float64, 0, vertices)
		for _, off := range []float64{0, 0.05} {
			for v := 0; v < (vertices-1)/2; v++ {
				f := float64(v) / float64((vertices-1)/2-1)
				if off != 0 {
					f = 1 - f
				}
				e, n := c.point(4988.01+24*f+off, -13+26*f)
				ring = append(ring, [2]float64{e, n})
			}
		}
		ring = append(ring, ring[0])
		page.Footprints = append(page.Footprints, dhm.MultiPolygon{{ring}})
	}
	got := mustClassifyB(b, c, s, []*Page{page})
	if got[200] != Vegetation {
		b.Fatalf("sample at 5 km: %q, want vegetation (was the index refused?)", got[200])
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Classify(c, s, []*Page{page}, DefaultOptions()); err != nil {
			b.Fatal(err)
		}
	}
}

func mustClassifyB(b *testing.B, c Corridor, s Samples, pages []*Page) []Class {
	got, err := Classify(c, s, pages, DefaultOptions())
	if err != nil {
		b.Fatal(err)
	}
	return got
}
