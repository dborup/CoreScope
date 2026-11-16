package profilecard

import (
	"image/color"
	"math"
	"sort"
	"strings"
	"testing"
	"time"
)

// #727 M3: the #755 review findings on the renderer, and the exports the
// server needs (MainVerdict, AltText, ResolveTheme, Options.Pending).

// The browser draws dark mode from Object.assign({}, theme, themeDark)
// (public/app.js reapplyUserThemeVars): a key the operator set only in
// theme still applies in dark mode. The card must merge the same way.
func TestResolveThemeMergesLikeTheBrowser(t *testing.T) {
	light := map[string]string{"losLine": "#112233", "terrainFill": "#445566", "fresnel60": "nope"}
	dark := map[string]string{"terrainFill": "#778899"}

	pal, bad := ResolveTheme(light, dark, true)
	if pal.LosLine != (color.NRGBA{0x11, 0x22, 0x33, 0xff}) {
		t.Errorf("dark: a theme-only key must apply, as Object.assign does: losLine %v", pal.LosLine)
	}
	if pal.TerrainFill != (color.NRGBA{0x77, 0x88, 0x99, 0xff}) {
		t.Errorf("dark: themeDark must win over theme: terrainFill %v", pal.TerrainFill)
	}
	if pal.Fresnel100 != DefaultPalette(true).Fresnel100 {
		t.Errorf("dark: a key in neither section keeps the dark default")
	}
	if len(bad) != 1 || bad[0] != "fresnel60" {
		t.Errorf("dark: bad keys %v", bad)
	}

	pal, _ = ResolveTheme(light, dark, false)
	if pal.TerrainFill != (color.NRGBA{0x44, 0x55, 0x66, 0xff}) {
		t.Errorf("light: themeDark must not apply: terrainFill %v", pal.TerrainFill)
	}
	if pal.Background != DefaultPalette(false).Background {
		t.Errorf("light: default background lost")
	}

	// A bad theme value that themeDark overrides is never used in dark
	// mode, so it is not reported there.
	if _, bad := ResolveTheme(map[string]string{"text": "bad"}, map[string]string{"text": "#fff"}, true); len(bad) != 0 {
		t.Errorf("an overridden bad key reported: %v", bad)
	}
	// The inputs are not modified.
	if light["terrainFill"] != "#445566" || len(dark) != 1 {
		t.Error("ResolveTheme modified its input")
	}
}

// #755 review Low–Med: the surface chip sits on the subtitle's row, so it
// bounds the subtitle, not the title. With the 589 px antenna-below chip
// and names that fit beside the badge, the title keeps its full size.
func TestTitleBoundByTheBadgeOnly(t *testing.T) {
	resp := fxClear()
	resp.Verdict.Surface = &TerrainSurfaceInfo{State: "blocked", Reason: "antennaBelowSurface", Endpoint: "both"}
	names := [2]string{"Søby Bakker", "Ørsted Mast Vestre"}
	_, runs, err := render(resp, names, DefaultPalette(true), Options{})
	if err != nil {
		t.Fatal(err)
	}
	var title, sub, chipRun, badge textRun
	for _, r := range runs {
		switch r.Role {
		case "title":
			title = r
		case "subtitle":
			sub = r
		case "surface-badge":
			chipRun = r
		case "badge":
			badge = r
		}
	}
	if chipRun.Text == "" {
		t.Fatal("precondition: no surface chip")
	}
	if title.Size != titleSize || strings.Contains(title.Text, "…") {
		t.Errorf("title %dpx %q: bounded by the surface chip", title.Size, title.Text)
	}
	if title.X+title.W > badge.X-18-safeMargin {
		t.Errorf("title runs into the badge: ends at %d, badge pill at %d", title.X+title.W, badge.X-18)
	}
	if sub.X+sub.W > chipRun.X-12 {
		t.Errorf("subtitle runs into the surface chip: ends at %d, chip at %d", sub.X+sub.W, chipRun.X-12)
	}
}

// #755 review M19: the title shrinks in 4 px steps, 40 → 36 → 32 → 28.
func TestTitleShrinkSteps(t *testing.T) {
	r := &renderer{fs: mustFonts(t)}
	a, b := "Søby Bakker Nordre", "Ørsted Mast Vestre"
	full := a + " → " + b
	for _, c := range []struct{ fitsAt, want int }{{40, 40}, {38, 36}, {36, 36}, {34, 32}, {30, 28}, {28, 28}} {
		maxW := r.fs.Measure(full, c.fitsAt, true)
		size, got := r.fitTitle(a, b, maxW)
		if size != c.want || got != full {
			t.Errorf("room for %dpx: got %dpx %q, want %dpx and the whole title", c.fitsAt, size, got, c.want)
		}
	}
	// Below 28 px the size stays and the names are shortened instead.
	size, got := r.fitTitle(a, b, r.fs.Measure(full, 24, true))
	if size != minTitleSize || !strings.Contains(got, "…") {
		t.Errorf("no room at 28px: %dpx %q", size, got)
	}
}

// #755 review M5/M6: an infinite elevation is no data, never a reason to
// drop the whole plot. A response cannot carry ±Inf through JSON, but
// Render takes a Go value and must not depend on that.
func TestInfiniteElevationsKeepThePlot(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*TerrainSample)
	}{
		{"+Inf DSM", func(s *TerrainSample) { s.DSM = ptr(math.Inf(1)) }},
		{"-Inf DSM", func(s *TerrainSample) { s.DSM = ptr(math.Inf(-1)) }},
		{"+Inf DTM", func(s *TerrainSample) { s.DTM = ptr(math.Inf(1)) }},
	} {
		resp := fxClear()
		for _, i := range []int{3, 4, 500, 501} {
			tc.edit(&resp.Samples[i])
		}
		// The ends too: an infinite endpoint ground, and with none, an
		// infinite first sample, fall back to the nearest finite one.
		resp.A.GroundM = ptr(math.Inf(1))
		resp.B.GroundM = nil
		resp.Samples[len(resp.Samples)-1].DTM = ptr(math.Inf(-1))
		g, ok := geometry(resp)
		if !ok {
			t.Errorf("%s: the plot was dropped", tc.name)
			continue
		}
		if !finite(g.zMin) || !finite(g.zMax) || g.span <= 0 || g.span > 1000 {
			t.Errorf("%s: vertical domain %v..%v", tc.name, g.zMin, g.zMax)
		}
		_, runs, err := render(resp, [2]string{"A", "B"}, DefaultPalette(true), Options{})
		if err != nil {
			t.Fatal(err)
		}
		if got := runText(runs, "message"); got != "" {
			t.Errorf("%s: empty-plot message %q", tc.name, got)
		}
	}
}

// #755 review M10b: a profile with a gap never shows a verdict, whatever
// the response's verdict says.
func TestPartialCoverageNeverShowsTheMainVerdict(t *testing.T) {
	for _, state := range []string{"clear", "fresnel100", "fresnel60", "blocked"} {
		resp := fxPartial()
		resp.Verdict.State = state
		_, runs, err := render(resp, [2]string{"A", "B"}, DefaultPalette(true), Options{})
		if err != nil {
			t.Fatal(err)
		}
		if got := runText(runs, "badge"); got != "No verdict" {
			t.Errorf("partial + %s: badge %q", state, got)
		}
		if v := MainVerdict(resp); v.Known || v.Word != "No verdict" {
			t.Errorf("partial + %s: MainVerdict %+v", state, v)
		}
	}
}

// #755 review M18: the host line is sanitised like a name: no controls,
// no bidi overrides, collapsed whitespace.
func TestSiteIsSanitised(t *testing.T) {
	_, runs, err := render(fxClear(), [2]string{"A", "B"}, DefaultPalette(true), Options{Site: " mesh‮.\x07example \n  dk "})
	if err != nil {
		t.Fatal(err)
	}
	if got := runText(runs, "site"); got != "mesh.example dk" {
		t.Errorf("site %q", got)
	}
}

// MainVerdict is what the badge says, without the "Terrain: " prefix the
// badge adds when the surface model is also drawn: the server puts the
// same word in og:title.
func TestMainVerdictAgreesWithTheBadge(t *testing.T) {
	for name, resp := range map[string]TerrainProfileResponse{
		"clear": fxClear(), "marginal": fxMarginal(), "blocked": fxBlocked(),
		"partial": fxPartial(), "none": fxNone(), "dtm": fxDTM(),
	} {
		_, runs, err := render(resp, [2]string{"A", "B"}, DefaultPalette(true), Options{})
		if err != nil {
			t.Fatal(err)
		}
		v := MainVerdict(resp)
		badge := runText(runs, "badge")
		if badge != v.Word && badge != "Terrain: "+v.Word {
			t.Errorf("%s: badge %q, MainVerdict %+v", name, badge, v)
		}
		if v.Detail == "" {
			t.Errorf("%s: no detail", name)
		}
	}
	if v := MainVerdict(fxMarginal()); !v.Known || v.Word != "Marginal" {
		t.Errorf("marginal: %+v", v)
	}
}

// AltText is the Go twin of public/terrain-profile.js chartTitle, with the
// card's own endpoint labels.
func TestAltText(t *testing.T) {
	resp := fxMarginal()
	got := AltText(resp, [2]string{"Søby Bakker", "东京"}, Options{}, altMaxRunes)
	want := "Terrain profile from Søby Bakker to Node B, 14.6 km. Terrain: Marginal. Line of sight is clear, but less than 60 % of the first Fresnel zone is."
	if !strings.HasPrefix(got, want) {
		t.Errorf("alt %q\nwant prefix %q", got, want)
	}
	if !strings.Contains(got, " Tightest point for the Fresnel zone: clearance ") || !strings.Contains(got, " along the path.") {
		t.Errorf("alt %q lacks the worst point", got)
	}
	if got := AltText(resp, [2]string{"Søby Bakker", "X"}, Options{HideNames: true}, altMaxRunes); !strings.HasPrefix(got, "Terrain profile from Node A to Node B,") {
		t.Errorf("names off: %q", got)
	}
	if got := AltText(fxPartial(), [2]string{"A", "B"}, Options{}, altMaxRunes); strings.Contains(got, "Tightest point") || !strings.Contains(got, "No verdict.") {
		t.Errorf("partial: %q", got)
	}
	if got := AltText(fxClear(), [2]string{"A", "B"}, Options{Pending: true}, altMaxRunes); !strings.Contains(got, pendingMessage) || strings.Contains(got, "Clear") {
		t.Errorf("pending: %q", got)
	}
}

// A pending card (the server's degraded card: no elevation yet) states
// that honestly instead of "no data for this path", and shows no verdict.
func TestPendingCard(t *testing.T) {
	_, runs, err := render(fxClear(), [2]string{"Søby Bakker", "Ørsted Mast"}, DefaultPalette(true), Options{Pending: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := runText(runs, "message"); got != pendingMessage {
		t.Errorf("message %q", got)
	}
	if got := runText(runs, "badge"); got != "No verdict" {
		t.Errorf("badge %q", got)
	}
	if got := runText(runs, "summary"); strings.Contains(got, "Tightest Fresnel point") || strings.Contains(got, "No elevation") {
		t.Errorf("summary %q", got)
	}
	if got := runText(runs, "title"); got != "Søby Bakker → Ørsted Mast" {
		t.Errorf("title %q", got)
	}
	if got := runText(runs, "attribution"); got == "" {
		t.Error("the credit is missing from a pending card")
	}
}

// #755 review Low: the sample-count test's +1 ms slack was about the whole
// draw, so an O(samples×columns) resampler survived it. geometry is the
// only part of a render that sees the samples; it is timed alone, as the
// fastest of many runs, against a ratio with no absolute slack. The ratio
// is the median of geometryRounds rounds, each timing both sizes back to
// back, so a burst of load on a shared runner skews a round, not the
// verdict (#759 review finding 4).
func TestGeometryLinearInSamples(t *testing.T) {
	if raceEnabled {
		t.Skip("timing is meaningless under -race")
	}
	mk := func(n int) TerrainProfileResponse {
		return build(spec{distM: 40000, n: n, terrain: []float64{40, 70, 95, 60, 30, 18, 45, 110},
			surface: []float64{45, 80, 99, 66, 35, 28, 49, 112}, antA: 30, antB: 30, maxReturned: n})
	}
	small, large := mk(1024), mk(3200)
	fastest := func(resp TerrainProfileResponse) time.Duration {
		best := time.Duration(math.MaxInt64)
		for i := 0; i < 41; i++ {
			start := time.Now()
			if _, ok := geometry(resp); !ok {
				t.Fatal("no geometry")
			}
			if d := time.Since(start); d < best {
				best = d
			}
		}
		return best
	}
	fastest(large)
	ratios := make([]float64, geometryRounds)
	for i := range ratios {
		s, l := fastest(small), fastest(large)
		ratios[i] = float64(l) / float64(s)
		t.Logf("geometry round %d: 1024 samples %v, 3200 samples %v (%.2fx)", i, s, l, ratios[i])
	}
	sort.Float64s(ratios)
	median := ratios[len(ratios)/2]
	t.Logf("geometry: median ratio %.2fx over %d rounds", median, len(ratios))
	if median > 2 {
		t.Errorf("3200 samples took a median %.2fx the time of 1024: work per sample and column", median)
	}
}

// geometryRounds is odd, so the median is one round's ratio.
const geometryRounds = 5

// #759 review RM6: a title too long for its row stops safeMargin short of
// the badge pill, not at it. Names of narrow glyphs over a range of
// lengths make some cut land within a few px of the bound.
func TestLongTitleKeepsTheGapBeforeTheBadge(t *testing.T) {
	cut := 0
	for n := 36; n <= 70; n++ {
		names := [2]string{"Il" + strings.Repeat("i", n), "Il" + strings.Repeat("l", n)}
		_, runs, err := render(fxClear(), names, DefaultPalette(true), Options{})
		if err != nil {
			t.Fatal(err)
		}
		var title, badge textRun
		for _, r := range runs {
			switch r.Role {
			case "title":
				title = r
			case "badge":
				badge = r
			}
		}
		if title.Size < titleSize || strings.Contains(title.Text, "…") {
			cut++
		}
		pill := badge.X - 18
		if end := title.X + title.W; end > pill-safeMargin {
			t.Errorf("%dpx %q ends at %d, within %d px of the badge pill at %d", title.Size, title.Text, end, pill-end, pill)
		}
	}
	if cut == 0 {
		t.Fatal("precondition: no title was shrunk or shortened")
	}
}

// #839 §8: a distance along the path is the panel's fmtDistance — whole
// metres under 1 km, two decimals to 10 km, one from there — never
// "8141.4 m".
func TestFmtDistance(t *testing.T) {
	for v, want := range map[float64]string{
		0: "0 m", 12.4: "12 m", 999.4: "999 m", 999.6: "1.00 km", 1000: "1.00 km",
		3912.3: "3.91 km", 8141.4: "8.14 km", 9994.9: "9.99 km", 9999.9: "10.0 km",
		10000: "10.0 km", 11149.99: "11.1 km", 11150: "11.2 km", 80000: "80.0 km",
	} {
		if got := fmtDistance(v); got != want {
			t.Errorf("fmtDistance(%v) = %q, want %q", v, got, want)
		}
	}
	if got := FmtDistance(8141.4); got != "8.14 km" {
		t.Errorf("FmtDistance(8141.4) = %q", got)
	}
}
