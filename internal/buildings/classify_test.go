package buildings

import (
	"errors"
	"math"
	"math/rand"
	"testing"

	"github.com/meshcore-analyzer/dhm"
)

// profileAlong samples the corridor every stepM metres from A to B, the
// way dhm.PlanProfile does: n evenly spaced samples, both ends included.
// DTM is groundM everywhere and DSM groundM + above(i).
func profileAlong(c Corridor, stepM, groundM float64, above func(i int) float64) Samples {
	n := int(math.Round(c.LengthM()/stepM)) + 1
	s := Samples{E: make([]float64, n), N: make([]float64, n), DTM: make([]float32, n), DSM: make([]float32, n),
		StepM: c.LengthM() / float64(n-1)}
	for i := range s.E {
		t := float64(i) / float64(n-1)
		s.E[i], s.N[i] = c.a[0]+(c.b[0]-c.a[0])*t, c.a[1]+(c.b[1]-c.a[1])*t
		s.DTM[i] = float32(groundM)
		s.DSM[i] = float32(groundM + above(i))
	}
	return s
}

func constant(m float64) func(int) float64 { return func(int) float64 { return m } }

func mustClassify(t *testing.T, c Corridor, s Samples, pages []*Page) []Class {
	t.Helper()
	got, err := Classify(c, s, pages, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// rect is a footprint given in the corridor's track frame: s0..s1 along,
// t0..t1 across (left positive), as a closed EPSG:25832 ring.
func rect(c Corridor, s0, s1, t0, t1 float64) dhm.MultiPolygon {
	ring := make([][2]float64, 0, 5)
	for _, p := range [][2]float64{{s0, t0}, {s1, t0}, {s1, t1}, {s0, t1}, {s0, t0}} {
		e, n := c.point(p[0], p[1])
		ring = append(ring, [2]float64{e, n})
	}
	return dhm.MultiPolygon{{ring}}
}

// TestClassifyDokk1 is the #729 reference case: a path from west of Dokk1
// eastwards, 498 m at a 6 m step, with the real footprints of the
// fixture. At 138 m the path is inside Dokk1, whose roof stands 28 m over
// the ground: a building.
func TestClassifyDokk1(t *testing.T) {
	page, err := Parse(readFixture(t, "dokk1_cell.json"))
	if err != nil {
		t.Fatal(err)
	}
	c := mustCorridor(t, [2]float64{575424 - 138, 6223838}, [2]float64{575424 + 360, 6223838})
	s := profileAlong(c, 6, 3, func(i int) float64 {
		if in, _ := dhm.Locate(page.Footprints[dokk1Index], 575424-138+6*float64(i), 6223838); in {
			return 28
		}
		return 0
	})
	got := mustClassify(t, c, s, []*Page{&page})
	if s.E[23] != 575424 || got[23] != Building || s.DSM[23]-s.DTM[23] != 28 {
		t.Fatalf("sample at 138 m (+%v m): %q, want building", s.DSM[23]-s.DTM[23], got[23])
	}
	buildings := 0
	for i, cl := range got {
		switch {
		case !s.Elevated(i, DefaultOptions().ThresholdM) && cl != Unclassified:
			t.Errorf("sample %d at ground level classified %q", i, cl)
		case cl == Vegetation:
			t.Errorf("sample %d inside Dokk1 read as vegetation", i)
		case cl == Building:
			buildings++
		}
	}
	if buildings < 20 { // Dokk1 is ~140 m across at this northing
		t.Fatalf("%d building samples", buildings)
	}
}

// TestClassifyForest: elevated samples with no footprint near them are
// vegetation (or other), on an empty but complete answer.
func TestClassifyForest(t *testing.T) {
	c := mustCorridor(t, [2]float64{545000, 6300000}, [2]float64{550000, 6300000})
	s := profileAlong(c, 10, 60, constant(18))
	for i, cl := range mustClassify(t, c, s, []*Page{{}}) {
		if cl != Vegetation {
			t.Fatalf("sample %d: %q, want vegetation", i, cl)
		}
	}
}

// oneSample is a 1 km E–W corridor and its sample 50 (500 m), elevated
// 10 m, at a 10 m step: a 5 m half-window.
func oneSample(t *testing.T) (Corridor, Samples) {
	c := mustCorridor(t, [2]float64{545000, 6300000}, [2]float64{546000, 6300000})
	return c, profileAlong(c, 10, 60, constant(10))
}

// TestClassifyWindowFraction: a sample is a building when at least
// MinBuildingFraction of its 5 × 5 window lies inside a footprint. Each
// case is a footprint relative to sample 50 at s = 500 m, h = 5 m.
func TestClassifyWindowFraction(t *testing.T) {
	c, s := oneSample(t)
	const at, h = 500.0, 5.0
	row := rect(c, at-30, at+30, 0.75*h, 30)            // the five points at t = h
	p6 := rect(c, at+0.75*h, at+1.25*h, 0.25*h, 0.75*h) // the point (h, h/2)
	p7 := rect(c, at+0.25*h, at+0.75*h, 0.25*h, 0.75*h) // the point (h/2, h/2)
	for name, fps := range map[string]struct {
		fps  []dhm.MultiPolygon
		want Class
	}{
		// f = 0.25 of 25 points is 6.25: six are not enough, seven are.
		"six points (24 %)":   {[]dhm.MultiPolygon{row, p6}, Vegetation},
		"seven points (28 %)": {[]dhm.MultiPolygon{row, p6, p7}, Building},
	} {
		if got := mustClassify(t, c, s, []*Page{{Footprints: fps.fps}})[50]; got != fps.want {
			t.Errorf("%s: %q, want %q", name, got, fps.want)
		}
	}
	for name, tc := range map[string]struct {
		fp   dhm.MultiPolygon
		want Class
	}{
		// About 10 % of the window's area, 4 of 25 points.
		"forest edge, a 10 % corner": {rect(c, at+0.4*h, at+30, 0.4*h, 30), Vegetation},
		// One row of five: 20 %.
		"one row (20 %)": {rect(c, at-30, at+30, 0.75*h, 30), Vegetation},
		// Two rows of ten: 40 %.
		"two rows (40 %)": {rect(c, at-30, at+30, 0.25*h, 30), Building},
		// The building's edge runs along the track through the sample: the
		// left half of the window is inside.
		"edge along the track": {rect(c, at-30, at+30, 0, 30), Building},
		// The building's gable crosses the track at the sample.
		"edge across the track":  {rect(c, at, at+30, -30, 30), Building},
		"inside":                 {rect(c, at-30, at+30, -30, 30), Building},
		"just beyond the window": {rect(c, at+h+0.01, at+30, -30, 30), Vegetation},
	} {
		page := &Page{Footprints: []dhm.MultiPolygon{tc.fp}}
		if got := mustClassify(t, c, s, []*Page{page})[50]; got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

// TestClassifyCourtyard: a sample in a building's courtyard (a hole) is
// not inside the building.
func TestClassifyCourtyard(t *testing.T) {
	c, s := oneSample(t)
	outer := rect(c, 450, 550, -50, 50)[0][0]
	hole := rect(c, 480, 520, -20, 20)[0][0]
	page := &Page{Footprints: []dhm.MultiPolygon{{{outer, hole}}}}
	got := mustClassify(t, c, s, []*Page{page})
	if got[50] != Vegetation || got[46] != Building {
		t.Fatalf("courtyard %q, roof %q", got[50], got[46])
	}
}

// TestClassifyThreshold: only a sample more than ThresholdM above the
// terrain is classified, and only when both models have a value.
func TestClassifyThreshold(t *testing.T) {
	c, s := oneSample(t)
	page := &Page{Footprints: []dhm.MultiPolygon{rect(c, 0, 1000, -30, 30)}}
	thr := DefaultOptions().ThresholdM
	nan := float32(math.NaN())
	for name, tc := range map[string]struct {
		dtm, dsm float32
		want     Class
	}{
		"exactly T":     {60, 60 + float32(thr), Unclassified},
		"below T":       {60, 62.4, Unclassified},
		"just above T":  {60, 62.6, Building},
		"below terrain": {60, 55, Unclassified},
		"no DSM":        {60, nan, Unclassified},
		"no DTM":        {nan, 70, Unclassified},
		"neither":       {nan, nan, Unclassified},
	} {
		s.DTM[50], s.DSM[50] = tc.dtm, tc.dsm
		if got := mustClassify(t, c, s, []*Page{page})[50]; got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

// TestClassifyUnusablePages: a segment that was not fetched (nil), failed
// (nil) or came back truncated leaves its samples unclassified; the other
// segments are classified.
func TestClassifyUnusablePages(t *testing.T) {
	c := mustCorridor(t, [2]float64{550000, 6223000}, [2]float64{575000, 6223000}) // 3 segments
	s := profileAlong(c, 25, 20, constant(12))
	full := []dhm.MultiPolygon{rect(c, 0, c.LengthM(), -30, 30)}
	for name, pages := range map[string][]*Page{
		"middle truncated": {{Footprints: full}, {Footprints: full, Truncated: true}, {Footprints: full}},
		"middle missing":   {{Footprints: full}, nil, {Footprints: full}},
	} {
		got := mustClassify(t, c, s, pages)
		for i, cl := range got {
			sv, tv := c.track(s.E[i], s.N[i])
			want := Building
			if c.segmentOf(sv, tv) == 1 {
				want = Unclassified
			}
			if cl != want {
				t.Fatalf("%s: sample %d (segment %d): %q, want %q", name, i, c.segmentOf(sv, tv), cl, want)
			}
		}
	}
	// A page that skipped a few features is still used; one that skipped
	// many is not (TestClassifyHighSkipShareIsUnusable).
	var few []dhm.MultiPolygon
	for len(few) < 19 {
		few = append(few, full[0])
	}
	got := mustClassify(t, c, s, []*Page{{Footprints: few, Skipped: 1}, {Footprints: full}, {Footprints: full}})
	if got[0] != Building {
		t.Fatalf("page with 1 of 20 features skipped unused: %q", got[0])
	}
}

// TestClassifyOffTrack: a sample farther off the line than trackSlackM
// could have a window the segment's query does not cover; it stays
// unclassified.
func TestClassifyOffTrack(t *testing.T) {
	c, s := oneSample(t)
	page := &Page{Footprints: []dhm.MultiPolygon{rect(c, 0, 1000, -30, 30)}}
	s.N[50] += 1
	s.E[0] -= 1 // before A
	got := mustClassify(t, c, s, []*Page{page})
	if got[50] != Unclassified || got[0] != Unclassified || got[51] != Building {
		t.Fatalf("off the track %q, before A %q, on it %q", got[50], got[0], got[51])
	}
}

// TestClassifyInsideTheSlack: a sample up to trackSlackM off the line is
// classified with its whole window, so the index keeps a footprint that
// only the slack brings within reach. Here the sample is 0.4 m left of the
// line; a footprint from t = h + 0.2 holds the window's outer row (5
// points), and a small one two more points: 7 of 25, a building, but only
// if the first is indexed.
func TestClassifyInsideTheSlack(t *testing.T) {
	c, s := oneSample(t)
	const at, h, off = 500.0, 5.0, 0.4
	s.E[50], s.N[50] = c.point(at, off)
	fps := []dhm.MultiPolygon{
		rect(c, at-30, at+30, h+0.2, 30),
		rect(c, at-0.5, at+3, off+0.4*h, off+0.6*h),
	}
	if got := mustClassify(t, c, s, []*Page{{Footprints: fps}})[50]; got != Building {
		t.Fatalf("sample %.1f m off the line: %q, want building", off, got)
	}
	// The same past B: the last sample moved 0.4 m beyond the end.
	s.E[100], s.N[100] = c.point(c.LengthM()+off, 0)
	fps = []dhm.MultiPolygon{
		rect(c, c.LengthM()+h+0.2, c.LengthM()+60, -30, 30), // the column at s + h
		rect(c, c.LengthM()-30, c.LengthM()+60, 0.75*h, 30), // the row at t = h
	}
	// Column (5) + row (5) − the shared corner = 9 points: a building, but
	// only if the footprint starting past s1 + h is indexed.
	if got := mustClassify(t, c, s, []*Page{{Footprints: fps}})[100]; got != Building {
		t.Fatalf("sample %.1f m past B: %q, want building", off, got)
	}
}

// TestClassifyWindowFollowsTheStep: the window is ± half the step,
// clamped to MinHalfWindowM..MaxHalfWindowM, so a coarse profile never
// looks past what its query covers.
func TestClassifyWindowFollowsTheStep(t *testing.T) {
	c := mustCorridor(t, [2]float64{545000, 6300000}, [2]float64{546000, 6300000})
	// A footprint beside the track, t in [6, 30]: two rows of a 12.5 m
	// window (40 %), nothing of a 5 m one.
	fp := []dhm.MultiPolygon{rect(c, 0, 1000, 6, 30)}
	for _, tc := range []struct {
		stepM float64
		want  Class
	}{
		{10, Vegetation}, // h = 5
		{25, Building},   // h = 12.5
		{100, Building},  // h clamped to 12.5
	} {
		s := profileAlong(c, tc.stepM, 0, constant(10))
		if got := mustClassify(t, c, s, []*Page{{Footprints: fp}})[1]; got != tc.want {
			t.Errorf("step %v: %q, want %q", tc.stepM, got, tc.want)
		}
	}
	// Past the widest window: the clamp keeps it out at any step.
	fp = []dhm.MultiPolygon{rect(c, 0, 1000, MaxHalfWindowM+0.2, 30)}
	s := profileAlong(c, 100, 0, constant(10))
	if got := mustClassify(t, c, s, []*Page{{Footprints: fp}})[1]; got != Vegetation {
		t.Fatalf("a footprint %.1f m off counted at step 100: %q", MaxHalfWindowM+0.2, got)
	}
	// And a fine step still gets MinHalfWindowM: a footprint 0.9 m off
	// holds two rows of a 2 m window, none of a 0.5 m one.
	fp = []dhm.MultiPolygon{rect(c, 0, 1000, 0.9, 30)}
	s = profileAlong(c, 1, 0, constant(10))
	if got := mustClassify(t, c, s, []*Page{{Footprints: fp}})[10]; got != Building {
		t.Fatalf("1 m step: %q, want building", got)
	}
}

// TestClassifyRejectsBadInput: the samples, the pages and the options
// must fit the corridor.
func TestClassifyRejectsBadInput(t *testing.T) {
	c, s := oneSample(t)
	pages := []*Page{{}}
	nan := math.NaN()
	for name, tc := range map[string]struct {
		c     Corridor
		s     func(Samples) Samples
		pages []*Page
		o     func(Options) Options
	}{
		"zero corridor":   {c: Corridor{}, pages: []*Page{}},
		"short N":         {c: c, s: func(s Samples) Samples { s.N = s.N[1:]; return s }, pages: pages},
		"short DTM":       {c: c, s: func(s Samples) Samples { s.DTM = s.DTM[1:]; return s }, pages: pages},
		"short DSM":       {c: c, s: func(s Samples) Samples { s.DSM = nil; return s }, pages: pages},
		"step 0":          {c: c, s: func(s Samples) Samples { s.StepM = 0; return s }, pages: pages},
		"step NaN":        {c: c, s: func(s Samples) Samples { s.StepM = nan; return s }, pages: pages},
		"step +Inf":       {c: c, s: func(s Samples) Samples { s.StepM = math.Inf(1); return s }, pages: pages},
		"no page":         {c: c, pages: nil},
		"two pages":       {c: c, pages: []*Page{{}, {}}},
		"threshold NaN":   {c: c, pages: pages, o: func(o Options) Options { o.ThresholdM = nan; return o }},
		"threshold < 0":   {c: c, pages: pages, o: func(o Options) Options { o.ThresholdM = -1; return o }},
		"fraction 0":      {c: c, pages: pages, o: func(o Options) Options { o.MinBuildingFraction = 0; return o }},
		"fraction over 1": {c: c, pages: pages, o: func(o Options) Options { o.MinBuildingFraction = 1.01; return o }},
		"fraction NaN":    {c: c, pages: pages, o: func(o Options) Options { o.MinBuildingFraction = nan; return o }},
	} {
		ss, o := s, DefaultOptions()
		if tc.s != nil {
			ss = tc.s(ss)
		}
		if tc.o != nil {
			o = tc.o(o)
		}
		if _, err := Classify(tc.c, ss, tc.pages, o); !errors.Is(err, ErrBadInput) {
			t.Errorf("%s: err %v, want ErrBadInput", name, err)
		}
	}
	if o := DefaultOptions(); o.ThresholdM != 2.5 || o.MinBuildingFraction != 0.25 {
		t.Fatalf("defaults %+v", o)
	}
}

// TestNeeded: a segment is requested only when one of its samples is
// elevated; a path with nothing elevated costs no request.
func TestNeeded(t *testing.T) {
	c := mustCorridor(t, [2]float64{550000, 6223000}, [2]float64{575000, 6223000}) // 3 segments
	flat := profileAlong(c, 25, 20, constant(0))
	got, err := c.Needed(flat, DefaultOptions())
	if err != nil || len(got) != 3 || got[0] || got[1] || got[2] {
		t.Fatalf("flat path: %v, %v", got, err)
	}
	s := profileAlong(c, 25, 20, func(i int) float64 {
		if i == 900 { // 22.5 km: the last segment
			return 2.6
		}
		return 2.5 // exactly T is not elevated
	})
	s.DSM[10] = float32(math.NaN())
	got, _ = c.Needed(s, DefaultOptions())
	if got[0] || got[1] || !got[2] {
		t.Fatalf("needed %v, want only the last", got)
	}
	s.N[5] += 1 // off the track: never classified, so never requested
	s.DSM[5] = 99
	if got, _ = c.Needed(s, DefaultOptions()); got[0] {
		t.Fatal("off-track sample requested its segment")
	}
	if _, err := c.Needed(Samples{E: []float64{1}}, DefaultOptions()); !errors.Is(err, ErrBadInput) {
		t.Fatalf("bad samples: %v", err)
	}
}

// TestClassifyIncidenceCap: a page whose footprints would make the index
// work exceed maxIncidences (a broken or hostile answer: here one polygon
// of 30,000 vertices running the whole segment) leaves its segment
// unclassified instead of making the classification slow.
func TestClassifyIncidenceCap(t *testing.T) {
	c := mustCorridor(t, [2]float64{545000, 6300000}, [2]float64{555000, 6300000})
	s := profileAlong(c, 25, 0, constant(10))
	ring := make([][2]float64, 0, 30001)
	for i := 0; i < 15000; i++ {
		e, n := c.point(10000*float64(i)/14999, -10)
		ring = append(ring, [2]float64{e, n})
	}
	for i := 14999; i >= 0; i-- {
		e, n := c.point(10000*float64(i)/14999, 10)
		ring = append(ring, [2]float64{e, n})
	}
	ring = append(ring, ring[0])
	page := &Page{Footprints: []dhm.MultiPolygon{{{ring}}}}
	if got := mustClassify(t, c, s, []*Page{page}); got[200] != Unclassified {
		t.Fatalf("over the cap: %q, want unclassified", got[200])
	}
	// The same building with few vertices is classified.
	small := &Page{Footprints: []dhm.MultiPolygon{rect(c, 0, 10000, -10, 10)}}
	if got := mustClassify(t, c, s, []*Page{small}); got[200] != Building {
		t.Fatalf("under the cap: %q", got[200])
	}
}

// classifyNaive is the reference the index must agree with: every window
// point against every footprint of its sample's segment, no index, no
// early exit. It is the O(samples × footprints) "before" of the
// benchmarks.
func classifyNaive(c Corridor, s Samples, pages []*Page, o Options) []Class {
	out := make([]Class, len(s.E))
	h := math.Max(MinHalfWindowM, math.Min(MaxHalfWindowM, s.StepM/2))
	need := int(math.Ceil(o.MinBuildingFraction*float64(windowPoints) - 1e-9))
	for i := range out {
		if !(float64(s.DSM[i])-float64(s.DTM[i]) > o.ThresholdM) {
			continue
		}
		sv, tv := c.track(s.E[i], s.N[i])
		k := c.segmentOf(sv, tv)
		if k < 0 || pages[k] == nil || pages[k].Truncated {
			continue
		}
		inside := 0
		for _, a := range windowOffsets {
			for _, b := range windowOffsets {
				e, n := c.point(sv+a*h, tv+b*h)
				for _, fp := range pages[k].Footprints {
					if in, _ := dhm.Locate(fp, e, n); in {
						inside++
						break
					}
				}
			}
		}
		out[i] = Vegetation
		if inside >= need {
			out[i] = Building
		}
	}
	return out
}

// randomScene is a corridor of up to 30 km with footprints scattered
// along and around it (rotated quadrilaterals, some with a hole, some
// spanning several buckets), elevations that come and go, and samples at
// the corridor's step.
func randomScene(rng *rand.Rand) (Corridor, Samples, []*Page) {
	for {
		// At most 1,500 samples, so the naive reference stays quick.
		step := []float64{2, 10, 17, 25, 60}[rng.Intn(5)]
		a := [2]float64{500000 + rng.Float64()*300000, 6100000 + rng.Float64()*250000}
		ang, l := rng.Float64()*2*math.Pi, 50+rng.Float64()*math.Min(30000, 1500*step)
		b := [2]float64{a[0] + l*math.Cos(ang), a[1] + l*math.Sin(ang)}
		c, err := NewCorridor(a, b, AllowedRadiiM[rng.Intn(len(AllowedRadiiM))], 1000+rng.Intn(9001))
		if err != nil {
			continue
		}
		s := profileAlong(c, step, 30, func(int) float64 {
			if rng.Intn(3) == 0 {
				return 0
			}
			return 1 + rng.Float64()*30
		})
		pages := make([]*Page, len(c.Segments()))
		for k := range pages {
			if rng.Intn(8) == 0 {
				continue // failed
			}
			s0, s1 := c.span(k)
			p := &Page{Truncated: rng.Intn(10) == 0}
			for j := rng.Intn(40); j > 0; j-- {
				cs, ct := s0+rng.Float64()*(s1-s0), (rng.Float64()-0.5)*50
				w, d, rot := 3+rng.Float64()*80, 3+rng.Float64()*30, rng.Float64()*math.Pi
				quad := func(sc float64) [][2]float64 {
					ring := make([][2]float64, 0, 5)
					for _, q := range [][2]float64{{-1, -1}, {1, -1}, {1.2, 1}, {-1, 1}, {-1, -1}} {
						x, y := q[0]*w/2*sc, q[1]*d/2*sc
						e, n := c.point(cs+x*math.Cos(rot)-y*math.Sin(rot), ct+x*math.Sin(rot)+y*math.Cos(rot))
						ring = append(ring, [2]float64{e, n})
					}
					return ring
				}
				poly := [][][2]float64{quad(1)}
				if rng.Intn(4) == 0 {
					poly = append(poly, quad(0.4))
				}
				p.Footprints = append(p.Footprints, dhm.MultiPolygon{poly})
			}
			pages[k] = p
		}
		return c, s, pages
	}
}

// TestClassifyMatchesNaive: on random scenes the bucket index answers
// exactly what testing every footprint answers.
func TestClassifyMatchesNaive(t *testing.T) {
	rng := rand.New(rand.NewSource(730))
	samples, classified := 0, 0
	for scene := 0; scene < 100; scene++ {
		c, s, pages := randomScene(rng)
		got := mustClassify(t, c, s, pages)
		want := classifyNaive(c, s, pages, DefaultOptions())
		for i := range got {
			if got[i] != want[i] {
				sv, tv := c.track(s.E[i], s.N[i])
				t.Fatalf("scene %d sample %d (s %.2f, t %.2f): index %q, naive %q", scene, i, sv, tv, got[i], want[i])
			}
			if got[i] != Unclassified {
				classified++
			}
		}
		samples += len(got)
	}
	if classified < samples/4 {
		t.Fatalf("only %d of %d samples classified: the scenes test too little", classified, samples)
	}
}
