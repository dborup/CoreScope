package profilecard

import (
	"bytes"
	"image"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/meshcore-analyzer/buildings"
	"github.com/meshcore-analyzer/cardkit"
	"github.com/meshcore-analyzer/dhm"
)

// #846: a card whose surface was classified with the INSPIRE building
// footprints credits them in the panel's words, colours the surface band
// by class with the panel's tokens, and says which colour is which.

func runsOf(runs []textRun, role string) []textRun {
	var out []textRun
	for _, r := range runs {
		if r.Role == role {
			out = append(out, r)
		}
	}
	return out
}

func renderRuns(t *testing.T, resp TerrainProfileResponse, opt Options) []textRun {
	t.Helper()
	img, runs, err := render(resp, [2]string{"Rådhus Repeater", "Havn Mast"}, DefaultPalette(true), opt)
	if err != nil {
		t.Fatal(err)
	}
	checkCard(t, "card", encodePNG(t, img), runs)
	return runs
}

// joined is the credit as drawn, its lines joined by one space.
func joined(runs []textRun) string {
	var parts []string
	for _, r := range runs {
		parts = append(parts, r.Text)
	}
	return strings.Join(parts, " ")
}

// The credit is the panel's buildings.AttributionDerived exactly when the
// response says the footprints went into it (the server's terrainClasses
// used, on a full or partial classification); every other card keeps the
// DHM credit alone, on its own line, where it always was.
func TestBuildingsCreditOnlyWhenClassified(t *testing.T) {
	unused := fxTown("full")
	unused.Attribution = Credit // classified, but nothing raised: not used
	unavailableButCredited := fxTown("unavailable")
	unavailableButCredited.Attribution = buildings.AttributionDerived
	dtm := fxTown("full")
	dtm.Model = "dtm"

	for _, tc := range []struct {
		name    string
		resp    TerrainProfileResponse
		opt     Options
		credits bool
	}{
		{"full", fxTown("full"), Options{}, true},
		{"partial", fxTown("partial"), Options{}, true},
		{"full, terrain only", dtm, Options{}, true}, // the credit follows the data used, as in the panel
		{"unavailable", fxTown("unavailable"), Options{}, false},
		{"not classified", fxClear(), Options{}, false},
		{"classified, none raised", unused, Options{}, false},
		{"unavailable, credit claimed", unavailableButCredited, Options{}, false},
		{"full, pending", fxTown("full"), Options{Pending: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runs := renderRuns(t, tc.resp, tc.opt)
			credit := runsOf(runs, "attribution")
			if got := buildingsCredited(tc.resp) && !tc.opt.Pending; got != tc.credits && tc.name != "full, pending" {
				t.Errorf("buildingsCredited = %v, want %v", got, tc.credits)
			}
			if !tc.credits {
				if len(credit) != 1 || credit[0].Text != Credit || credit[0].Y != creditBaseline {
					t.Fatalf("want the DHM credit alone at y=%d:\n%s", creditBaseline, formatRuns(runs))
				}
				for _, r := range runs {
					if strings.Contains(r.Text, buildings.DatasetName) || strings.Contains(r.Text, buildings.Source) {
						t.Errorf("unclassified card drew %q", r.Text)
					}
				}
				return
			}
			if got := joined(credit); got != buildings.AttributionDerived {
				t.Fatalf("credit %q, want the panel's %q:\n%s", got, buildings.AttributionDerived, formatRuns(runs))
			}
			for _, r := range credit {
				if r.Size != attributionSize {
					t.Errorf("credit line %q at %dpx, want %d: never shrunk", r.Text, r.Size, attributionSize)
				}
			}
		})
	}
}

// The panel's line is too wide for one line at the credit size, so it
// wraps, whole, onto the two lines a mixed card uses (#779), breaking
// where the footprints' part begins; the summary moves up to make room
// and the site stays on the first credit line.
func TestBuildingsCreditLayout(t *testing.T) {
	fs, err := cardkit.LoadFonts()
	if err != nil {
		t.Fatal(err)
	}
	if fs.Measure(buildings.AttributionDerived, attributionSize, false) <= Width-2*safeMargin {
		t.Fatal("precondition: the buildings credit fits one line, so this test proves nothing")
	}
	runs := renderRuns(t, fxTown("full"), Options{Site: "mesh.example"})
	credit, summary, site := runsOf(runs, "attribution"), runsOf(runs, "summary"), runsOf(runs, "site")
	if len(credit) != 2 || len(summary) != 1 || len(site) != 1 {
		t.Fatalf("want two credit lines, a summary and a site:\n%s", formatRuns(runs))
	}
	if !strings.HasSuffix(credit[0].Text, dhm.DatasetName) || !strings.HasPrefix(credit[1].Text, "og "+buildings.DatasetName) {
		t.Errorf("credit breaks at the wrong place: %q / %q", credit[0].Text, credit[1].Text)
	}
	if credit[1].Y != creditBaseline || credit[0].Y != creditBaseline-creditLeading {
		t.Errorf("credit lines at y=%d, %d; want %d, %d", credit[0].Y, credit[1].Y, creditBaseline-creditLeading, creditBaseline)
	}
	s := summary[0]
	if !(s.Y < credit[0].Y && credit[0].Y-s.Y >= attributionSize && credit[1].Y-credit[0].Y >= attributionSize) {
		t.Errorf("footer lines overlap: summary y=%d, credit y=%d, %d", s.Y, credit[0].Y, credit[1].Y)
	}
	if s.Y-summarySize < footerRule {
		t.Errorf("summary at y=%d crosses the footer rule at %d", s.Y, footerRule)
	}
	if site[0].Y != credit[0].Y || site[0].X < credit[0].X+credit[0].W+16 {
		t.Errorf("site %+v is not right of the first credit line %+v", site[0], credit[0])
	}
}

// A classified profile is never mixed (the server classifies only a path
// wholly inside DHM), but a card of one still draws every credit whole:
// three credit lines, and the summary, which is not an obligation, goes.
func TestClassifiedAndMixedKeepsEveryCredit(t *testing.T) {
	r := fxTown("full")
	r.Coverage.Mixed = true
	runs := renderRuns(t, r, Options{Site: "mesh.example"})
	credit, fill := runsOf(runs, "attribution"), runsOf(runs, "attributionFill")
	if joined(credit) != buildings.AttributionDerived || len(fill) != 1 || fill[0].Text != FillCredit {
		t.Fatalf("credits not whole:\n%s", formatRuns(runs))
	}
	lines := append(credit, fill...)
	for i := 1; i < len(lines); i++ {
		if lines[i].Y-lines[i-1].Y < attributionSize {
			t.Errorf("credit lines %d and %d overlap: y=%d, %d", i-1, i, lines[i-1].Y, lines[i].Y)
		}
	}
	if top := lines[0].Y - attributionSize; top < footerRule {
		t.Errorf("first credit line reaches y=%d, above the footer rule at %d", top, footerRule)
	}
	if lines[len(lines)-1].Y != creditBaseline {
		t.Errorf("last credit line at y=%d, want %d", lines[len(lines)-1].Y, creditBaseline)
	}
	for _, s := range runsOf(runs, "summary") {
		if s.Y > lines[0].Y-attributionSize || s.Y-summarySize < footerRule {
			t.Errorf("summary at y=%d overlaps the credits or the footer rule", s.Y)
		}
	}
}

// The two class colours are the panel's tokens, under the customizer's
// keys (customize-v2.js THEME_CSS_MAP), resolved like every other card
// colour; TestDefaultPaletteMatchesFrontend pins their defaults to
// --terrain-building and --terrain-vegetation in style.css.
func TestClassPaletteMapping(t *testing.T) {
	var p Palette
	keys := map[string]bool{}
	for _, f := range paletteFields(&p) {
		keys[f.Key] = true
	}
	if !keys["terrainBuilding"] || !keys["terrainVegetation"] {
		t.Fatalf("palette keys %v lack terrainBuilding/terrainVegetation", keys)
	}
	js, err := os.ReadFile(filepath.Join("..", "..", "public", "customize-v2.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"terrainBuilding: '--terrain-building'", "terrainVegetation: '--terrain-vegetation'"} {
		if !bytes.Contains(js, []byte(m)) {
			t.Errorf("customize-v2.js does not map %s", m)
		}
	}
	for _, dark := range []bool{true, false} {
		pal, bad := ResolvePalette(map[string]string{"terrainBuilding": "#102030", "terrainVegetation": "rgba(1, 2, 3, 0.5)"}, dark)
		if len(bad) != 0 || pal.TerrainBuilding != (cardkit.Opaque(pal.TerrainBuilding)) || pal.TerrainBuilding.R != 0x10 || pal.TerrainVegetation.A != 128 {
			t.Errorf("dark=%v: theme keys not taken: %+v %+v %v", dark, pal.TerrainBuilding, pal.TerrainVegetation, bad)
		}
		def := DefaultPalette(dark)
		if def.TerrainBuilding == def.TerrainVegetation || def.TerrainBuilding.A != 255 || def.TerrainVegetation.A != 255 {
			t.Errorf("dark=%v: defaults %+v / %+v", dark, def.TerrainBuilding, def.TerrainVegetation)
		}
	}
}

// bandPixel is the colour just above the terrain in column c, inside the
// surface band and below every Fresnel line on these fixtures.
func bandPixel(t *testing.T, img *image.RGBA, g geom, c int) [4]uint8 {
	t.Helper()
	y := int(g.y(g.terr[c]+2) >> cardkit.FracBits)
	if top := int(g.y(g.surf[c]) >> cardkit.FracBits); y <= top+1 {
		t.Fatalf("column %d: no band above the terrain", c)
	}
	o := img.PixOffset(plotX+c, y)
	return [4]uint8{img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3]}
}

func nrgba(c interface{ RGBA() (r, g, b, a uint32) }) [4]uint8 {
	r, g, b, a := c.RGBA()
	return [4]uint8{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)}
}

// column is the plot column whose centre is d metres along resp's path.
func column(resp TerrainProfileResponse, d float64) int { return int(d / resp.DistanceM * plotW) }

func TestSurfaceBandColouredByClass(t *testing.T) {
	for _, dark := range []bool{true, false} {
		pal := DefaultPalette(dark)
		draw := func(resp TerrainProfileResponse) (*image.RGBA, geom) {
			img, _, err := render(resp, [2]string{"A", "B"}, pal, Options{})
			if err != nil {
				t.Fatal(err)
			}
			g, ok := geometry(resp)
			if !ok {
				t.Fatal("no geometry")
			}
			return img, g
		}
		full, g := draw(fxTown("full"))
		partial, _ := draw(fxTown("partial"))
		none, _ := draw(fxTown("unavailable"))
		resp := fxTown("full")
		building, trees, latePath := column(resp, 1180), column(resp, 900), column(resp, 1775)
		wantB, wantV := nrgba(pal.TerrainBuilding), nrgba(pal.TerrainVegetation)
		if got := bandPixel(t, full, g, building); got != wantB {
			t.Errorf("dark=%v: building column %d is %v, want --terrain-building %v", dark, building, got, wantB)
		}
		if got := bandPixel(t, full, g, trees); got != wantV {
			t.Errorf("dark=%v: trees column %d is %v, want --terrain-vegetation %v", dark, trees, got, wantV)
		}
		if got := bandPixel(t, full, g, latePath); got != wantV {
			t.Errorf("dark=%v: trees at 1775 m are %v on a full classification, want %v", dark, got, wantV)
		}
		// Partial: where nothing was told apart the band keeps the clutter
		// colour, exactly as an unclassified card draws it.
		if got, want := bandPixel(t, partial, g, latePath), bandPixel(t, none, g, latePath); got != want || got == wantB || got == wantV {
			t.Errorf("dark=%v: unclassified stretch of a partial card is %v, want the clutter colour %v", dark, got, want)
		}
		if got := bandPixel(t, partial, g, building); got != wantB {
			t.Errorf("dark=%v: classified stretch of a partial card is %v, want %v", dark, got, wantB)
		}
		// No classes: every pixel as it was drawn before (#846 keeps the
		// unclassified card byte for byte; the goldens pin the rest).
		bare := fxTown("unavailable")
		bare.Classification = nil
		before, _ := draw(bare)
		if !bytes.Equal(before.Pix, none.Pix) {
			t.Errorf("dark=%v: an unavailable classification changed the picture", dark)
		}
	}
}

// columnClasses is the Go twin of the panel's bandRuns rule (#730 M3):
// where a class meets a sample without one, the class claims the slope
// between them; two classes meet halfway between their samples.
func TestColumnClassesFollowThePanelRule(t *testing.T) {
	smp := func(d float64, cls string) TerrainSample {
		return TerrainSample{D: d, DTM: ptr(10), DSM: ptr(20), Cls: cls}
	}
	resp := TerrainProfileResponse{Model: "both", DistanceM: 1080, Coverage: TerrainCoverage{State: "full"},
		Classification: &TerrainClassification{State: "full"},
		Samples: []TerrainSample{smp(0, ""), smp(100, "b"), smp(200, "b"), smp(300, ""), smp(400, ""),
			smp(500, "v"), smp(600, "b"), smp(700, ""), smp(1080, "")}}
	g := geom{D: 1080}
	got := columnClasses(resp, g)
	if len(got) != plotW {
		t.Fatalf("%d columns, want %d", len(got), plotW)
	}
	// One column per metre here: column c is centred on c + 0.5 m.
	for _, tc := range []struct {
		d    int
		want byte
	}{
		{0, 'b'}, {50, 'b'}, {150, 'b'}, {250, 'b'}, {299, 'b'}, // b claims the slope to 0 and to 300
		{300, 0}, {350, 0}, {399, 0}, // between two samples without a class
		{400, 'v'}, {450, 'v'}, {549, 'v'}, // v claims the slope from 400
		{550, 'b'}, {650, 'b'}, {699, 'b'}, // v and b meet halfway, b claims to 700
		{700, 0}, {1000, 0},
	} {
		if got[tc.d] != tc.want {
			t.Errorf("column at %d m: %q, want %q", tc.d, got[tc.d], tc.want)
		}
	}
	// Classes are drawn only on a full or partial classification of a
	// fully covered model=both profile, and only "b" and "v" count.
	for name, edit := range map[string]func(*TerrainProfileResponse){
		"terrain only":      func(r *TerrainProfileResponse) { r.Model = "dtm" },
		"gap":               func(r *TerrainProfileResponse) { r.Coverage.State = "partial" },
		"unavailable":       func(r *TerrainProfileResponse) { r.Classification.State = "unavailable" },
		"no classification": func(r *TerrainProfileResponse) { r.Classification = nil },
		"unknown state":     func(r *TerrainProfileResponse) { r.Classification.State = "FULL" },
		"no class in the set": func(r *TerrainProfileResponse) {
			for i := range r.Samples {
				r.Samples[i].Cls = "x"
			}
		},
	} {
		r := resp
		r.Samples = append([]TerrainSample(nil), resp.Samples...)
		c := *resp.Classification
		r.Classification = &c
		edit(&r)
		if got := columnClasses(r, g); got != nil {
			t.Errorf("%s: classes drawn", name)
		}
	}
	partial := resp
	partial.Classification = &TerrainClassification{State: "partial"}
	if columnClasses(partial, g) == nil {
		t.Error("partial classification: no classes drawn")
	}
}

// The legend names both colours, and on a partial classification the
// clutter colour that is left, in the panel's words; it sits in the axis
// row between the "0" and the middle distance label, and is left out
// where no class is drawn.
func TestClassLegend(t *testing.T) {
	js, err := os.ReadFile(filepath.Join("..", "..", "public", "terrain-profile.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{legendBuilding, legendVegetation, legendNone} {
		if !regexp.MustCompile(`(legend: |CLASS_NONE_LEGEND = )'` + regexp.QuoteMeta(w) + `'`).Match(js) {
			t.Errorf("legend word %q is not the panel's (public/terrain-profile.js)", w)
		}
	}
	for _, tc := range []struct {
		state string
		want  []string
	}{
		{"full", []string{legendBuilding, legendVegetation}},
		{"partial", []string{legendBuilding, legendVegetation, legendNone}},
		{"unavailable", nil},
	} {
		runs := renderRuns(t, fxTown(tc.state), Options{})
		legend := runsOf(runs, "legend")
		var got []string
		for _, r := range legend {
			got = append(got, r.Text)
		}
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("%s: legend %q, want %q", tc.state, got, tc.want)
		}
		axis := runsOf(runs, "axis-x")
		for _, r := range legend {
			if r.Y != axisBaseline || r.X < axis[0].X+axis[0].W+16 || r.X+r.W > axis[1].X-16 {
				t.Errorf("%s: legend %q at x=%d..%d y=%d, outside the axis row gap %d..%d", tc.state, r.Text, r.X, r.X+r.W, r.Y, axis[0].X+axis[0].W, axis[1].X)
			}
		}
	}
	if runs := renderRuns(t, fxClear(), Options{}); len(runsOf(runs, "legend")) != 0 {
		t.Error("an unclassified card drew a legend")
	}
}

// og:image:alt names the footprints' dataset and source when the card
// credits them, from internal/buildings' own parts.
func TestAltCredit(t *testing.T) {
	for _, st := range []string{"full", "partial"} {
		got := AltCredit(fxTown(st), Options{})
		if !strings.Contains(got, buildings.DatasetName) || !strings.Contains(got, buildings.Source) || !strings.Contains(got, dhm.LicenseName) {
			t.Errorf("%s: alt credit %q lacks the dataset, source or licence", st, got)
		}
		if AltCredit(fxTown(st), Options{Pending: true}) != "" {
			t.Errorf("%s: a pending card credits the footprints in its alt", st)
		}
	}
	for name, r := range map[string]TerrainProfileResponse{"unavailable": fxTown("unavailable"), "clear": fxClear()} {
		if got := AltCredit(r, Options{}); got != "" {
			t.Errorf("%s: alt credit %q on an unclassified card", name, got)
		}
	}
}

// A classified card stays inside the #755 budget: one more pass over the
// columns, no allocation per sample beyond the class slice, and a PNG
// well under the server's 128 KiB per cached card.
func TestClassifiedCardSize(t *testing.T) {
	for _, st := range []string{"full", "partial"} {
		for _, dark := range []bool{true, false} {
			b, err := Render(fxTown(st), [2]string{"Rådhus Repeater", "Havn Mast"}, DefaultPalette(dark), Options{Site: "mesh.example"})
			if err != nil {
				t.Fatal(err)
			}
			if len(b) > 128<<10 {
				t.Errorf("%s dark=%v: %d bytes, over the 128 KiB a cached card may take", st, dark, len(b))
			}
		}
	}
}
