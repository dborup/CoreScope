package profilecard

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"math"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// nasty is a list of responses no well-behaved server sends, but which the
// renderer must still turn into a full-size card with the credit on it.
func nasty() map[string]TerrainProfileResponse {
	nan := math.NaN()
	inf := math.Inf(1)
	out := map[string]TerrainProfileResponse{}

	out["zero value"] = TerrainProfileResponse{}
	out["nil samples"] = func() TerrainProfileResponse { r := fxClear(); r.Samples = nil; return r }()
	out["empty samples"] = func() TerrainProfileResponse { r := fxClear(); r.Samples = []TerrainSample{}; return r }()
	out["one sample"] = func() TerrainProfileResponse { r := fxClear(); r.Samples = r.Samples[:1]; return r }()
	out["zero distance"] = func() TerrainProfileResponse { r := fxClear(); r.DistanceM = 0; return r }()
	out["negative distance"] = func() TerrainProfileResponse { r := fxClear(); r.DistanceM = -5; return r }()
	out["NaN distance"] = func() TerrainProfileResponse { r := fxClear(); r.DistanceM = nan; return r }()
	out["Inf distance"] = func() TerrainProfileResponse { r := fxClear(); r.DistanceM = inf; return r }()
	out["NaN elevations"] = func() TerrainProfileResponse {
		r := fxMarginal()
		for i := range r.Samples {
			if i%3 == 0 {
				r.Samples[i].DTM = ptr(nan)
				r.Samples[i].DSM = ptr(inf)
			}
		}
		r.A.GroundM = ptr(nan)
		return r
	}()
	out["all NaN"] = func() TerrainProfileResponse {
		r := fxClear()
		for i := range r.Samples {
			r.Samples[i].DTM, r.Samples[i].DSM = ptr(nan), ptr(nan)
		}
		return r
	}()
	out["huge elevations"] = func() TerrainProfileResponse {
		r := fxClear()
		r.Samples[5].DTM = ptr(1e308)
		r.Samples[6].DTM = ptr(-1e308)
		r.A.AntennaM = 1e308
		return r
	}()
	out["NaN physics"] = func() TerrainProfileResponse {
		r := fxMarginal()
		r.Physics = TerrainPhysics{FreqMHz: nan, LambdaM: nan, K: nan, F1MaxM: inf}
		r.Verdict.Criterion = nan
		r.A.AntennaM = nan
		return r
	}()
	out["nil verdict numbers"] = func() TerrainProfileResponse {
		r := fxBlocked()
		r.Verdict.TerrainClearance = TerrainClearance{}
		return r
	}()
	out["NaN verdict numbers"] = func() TerrainProfileResponse {
		r := fxBlocked()
		r.Verdict.WorstAtM, r.Verdict.WorstClearanceM, r.Verdict.WorstF1Fraction = ptr(nan), ptr(inf), ptr(nan)
		return r
	}()
	out["unknown verdict word"] = func() TerrainProfileResponse { r := fxClear(); r.Verdict.State = "<b>great</b>"; return r }()
	out["both without surface"] = func() TerrainProfileResponse { r := fxClear(); r.Verdict.Surface = nil; return r }()
	out["dtm with dsm samples"] = func() TerrainProfileResponse { r := fxClear(); r.Model = "dtm"; return r }()
	out["unsorted samples"] = func() TerrainProfileResponse {
		r := fxClear()
		r.Samples[10], r.Samples[500] = r.Samples[500], r.Samples[10]
		r.Samples[0].D = 1e9
		r.Samples[1].D = -1e9
		r.Samples[2].D = nan
		return r
	}()
	out["4096 identical d"] = func() TerrainProfileResponse {
		r := fxClear()
		r.Samples = make([]TerrainSample, 4096)
		for i := range r.Samples {
			r.Samples[i] = TerrainSample{D: 5, DTM: ptr(float64(i % 50))}
		}
		return r
	}()
	return out
}

func checkCard(t *testing.T, label string, b []byte, runs []textRun) {
	t.Helper()
	cfg, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("%s: not a PNG: %v", label, err)
	}
	if cfg.Width != Width || cfg.Height != Height {
		t.Errorf("%s: %dx%d, want %dx%d", label, cfg.Width, cfg.Height, Width, Height)
	}
	if runs == nil {
		return
	}
	attributed := false
	for _, r := range runs {
		if r.Role == "attribution" && r.Text == Credit && r.Size == attributionSize {
			attributed = true
		}
		// Safe area: nothing within safeMargin px of an edge.
		if r.X < safeMargin || r.X+r.W > Width-safeMargin || r.Y > Height-safeMargin || r.Y-r.Size < safeMargin/2 {
			t.Errorf("%s: %s run %q at x=%d..%d y=%d leaves the safe area", label, r.Role, r.Text, r.X, r.X+r.W, r.Y)
		}
	}
	if !attributed {
		t.Errorf("%s: the DHM credit is missing or shrunk: %s", label, formatRuns(runs))
	}
}

func TestRenderNeverPanicsAndAlwaysCredits(t *testing.T) {
	names := [][2]string{{"", ""}, {"Søby Bakker", "东京"},
		{strings.Repeat("Very Long Node Name ", 30), strings.Repeat("Ø", 400)}, {"\x00\n\r\t", " ‮"}}
	// Each response once, cycling through the awkward names and both
	// palettes: the palette and the names cannot make geometry panic, and
	// every combination costs ~0.2 s under -race.
	labels := make([]string, 0, len(nasty()))
	cases := nasty()
	for label := range cases {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for i, label := range labels {
		img, runs, err := render(cases[label], names[i%len(names)], DefaultPalette(i%2 == 0), Options{Site: strings.Repeat("x", 300)})
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		checkCard(t, label, encodePNG(t, img), runs)
	}
	for _, nm := range names {
		_, runs, err := render(fxClear(), nm, DefaultPalette(false), Options{})
		if err != nil {
			t.Fatal(err)
		}
		checkCard(t, "names", encodePNG(t, mustRender(t, fxClear(), nm)), runs)
	}
	// The exported entry point, too.
	for label, resp := range nasty() {
		b, err := Render(resp, [2]string{}, Palette{}, Options{})
		if err != nil {
			t.Fatalf("%s: Render: %v", label, err)
		}
		checkCard(t, label, b, nil)
	}
}

// The credit wins over the title: with names too long for the header, the
// title shrinks and is clipped; the attribution stays whole at full size.
func TestLongNamesShrinkTitleNotCredit(t *testing.T) {
	long := strings.Repeat("Wide Name ", 20)
	_, runs, err := render(fxClear(), [2]string{long, long}, DefaultPalette(true), Options{Site: "a.very.long.host.name.example"})
	if err != nil {
		t.Fatal(err)
	}
	var title textRun
	for _, r := range runs {
		if r.Role == "title" {
			title = r
		}
	}
	if title.Size >= titleSize {
		t.Errorf("title kept %dpx with names that cannot fit", title.Size)
	}
	if !strings.Contains(title.Text, "…") {
		t.Errorf("title %q was not shortened", title.Text)
	}
	checkCard(t, "long names", encodePNG(t, mustRender(t, fxClear(), [2]string{long, long})), runs)
}

func mustRender(t testing.TB, resp TerrainProfileResponse, names [2]string) *image.RGBA {
	t.Helper()
	img, _, err := render(resp, names, DefaultPalette(true), Options{})
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// Render is a pure function: the same input gives the same bytes, also
// from many goroutines at once. The concurrent renders run first, on
// different names, so any state shared between calls (a glyph cache, a
// font buffer) would be written concurrently and trip -race (CI) or the
// runtime's map check; the sequential renders afterwards must match them.
func TestRenderIsPure(t *testing.T) {
	resp := fxMarginal()
	names := func(i int) [2]string {
		return [2]string{fmt.Sprintf("Ridge %c%c", 'A'+i, 'k'+i), fmt.Sprintf("Valley %c", 'Q'+i)}
	}
	var wg sync.WaitGroup
	got := make([][]byte, 8)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], _ = Render(resp, names(i), DefaultPalette(i%2 == 0), Options{})
		}(i)
	}
	wg.Wait()
	for i, b := range got {
		want, err := Render(resp, names(i), DefaultPalette(i%2 == 0), Options{})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b, want) {
			t.Fatalf("concurrent render %d differs from the same render run alone", i)
		}
	}
	// And the input is not modified.
	if again := fxMarginal(); len(again.Samples) != len(resp.Samples) || *again.Samples[7].DTM != *resp.Samples[7].DTM {
		t.Fatal("Render modified its input")
	}
}

// The renderer walks plot columns, not samples: a 3200-sample response
// costs what a 1024-sample one does. Timed as the fastest of several runs
// so scheduler noise cannot fail it; the bound is loose on purpose — the
// failure it guards against is work per sample, which costs multiples.
func TestRenderTimeIndependentOfSampleCount(t *testing.T) {
	small := build(spec{distM: 40000, n: 1024, terrain: []float64{40, 70, 95, 60, 30, 18, 45, 110},
		surface: []float64{45, 80, 99, 66, 35, 28, 49, 112}, antA: 30, antB: 30, maxReturned: 1024})
	large := build(spec{distM: 40000, n: 3200, terrain: []float64{40, 70, 95, 60, 30, 18, 45, 110},
		surface: []float64{45, 80, 99, 66, 35, 28, 49, 112}, antA: 30, antB: 30, maxReturned: 3200})
	if len(small.Samples) != 1024 || len(large.Samples) != 3200 {
		t.Fatalf("fixtures have %d and %d samples", len(small.Samples), len(large.Samples))
	}
	fastest := func(resp TerrainProfileResponse) time.Duration {
		best := time.Duration(math.MaxInt64)
		for i := 0; i < 9; i++ {
			start := time.Now()
			if _, _, err := render(resp, [2]string{"A", "B"}, DefaultPalette(true), Options{}); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d < best {
				best = d
			}
		}
		return best
	}
	fastest(small) // warm up
	// The median round of geometryRounds, as TestGeometryLinearInSamples
	// does, so one burst of load on a shared runner cannot fail it (#759
	// review finding 4).
	type round struct{ s, l time.Duration }
	rounds := make([]round, geometryRounds)
	for i := range rounds {
		rounds[i] = round{fastest(small), fastest(large)}
	}
	over := func(r round) time.Duration { return r.l - (r.s*3/2 + time.Millisecond) }
	sort.Slice(rounds, func(i, j int) bool { return over(rounds[i]) < over(rounds[j]) })
	s, l := rounds[len(rounds)/2].s, rounds[len(rounds)/2].l
	t.Logf("draw: 1024 samples %v, 3200 samples %v (median of %d rounds)", s, l, len(rounds))
	if l > s*3/2+time.Millisecond {
		t.Errorf("3200 samples took %v against %v for 1024: the renderer is doing work per sample", l, s)
	}
}

// Every case renders inside the per-card budget. The plan's target is
// 25 ms (draw and PNG encode); the test fails at budgetLimit, twice that,
// so a loaded shared CI runner cannot fail it while a renderer that does
// real extra work still does (#759 review finding 4). A case over the
// target but under the limit is logged, not failed. Skipped under -race,
// which slows everything ~10× and is not what production runs.
func TestRenderWithinBudget(t *testing.T) {
	if raceEnabled {
		t.Skip("timing is meaningless under -race")
	}
	var times []time.Duration
	for _, c := range goldenCases() {
		best := time.Duration(math.MaxInt64)
		for i := 0; i < 7; i++ {
			start := time.Now()
			if _, err := Render(c.resp, c.names, DefaultPalette(true), c.opt); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d < best {
				best = d
			}
		}
		times = append(times, best)
		switch {
		case best > budgetLimit:
			t.Errorf("%s: %v per card, limit %v (target %v)", c.name, best, budgetLimit, budgetTarget)
		case best > budgetTarget:
			t.Logf("%s: %v per card, over the %v target (limit %v)", c.name, best, budgetTarget, budgetLimit)
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	t.Logf("per card: fastest %v, slowest %v (target %v, limit %v)", times[0], times[len(times)-1], budgetTarget, budgetLimit)
}

// budgetTarget is the plan's per-card budget; budgetLimit is what the
// test enforces: twice the target, the headroom a shared runner needs.
const (
	budgetTarget = 25 * time.Millisecond
	budgetLimit  = 2 * budgetTarget
)

// #759 review finding 4: the enforced bound keeps 2× headroom over the
// plan's 25 ms target, which stays what the logs measure against.
func TestRenderBudgetHasHeadroom(t *testing.T) {
	if budgetTarget != 25*time.Millisecond || budgetLimit != 2*budgetTarget {
		t.Fatalf("target %v, limit %v; want 25ms and twice that", budgetTarget, budgetLimit)
	}
}
