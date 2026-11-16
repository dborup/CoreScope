package profilecard

import (
	"strings"
	"testing"
)

// #839 finding 4: the verdict's worst point is the sample with the
// smallest Fresnel ratio (dhm.Link.Assess), not the one with the least
// clearance. On Padborg → Flensburg the two differ: −11.3 m at 5786.7 m
// is the worst Fresnel fraction (−1.267), while the least clearance on
// the path is −22.8 m at 3937.7 m. The card and og:image:alt must not
// call the first one "worst clearance".
func TestWording839_WorstPointIsTheTightestFresnelPoint(t *testing.T) {
	resp := fxBlocked()
	wc, at, f := -11.3, 5786.7, -1.267
	resp.Verdict.WorstClearanceM, resp.Verdict.WorstAtM, resp.Verdict.WorstF1Fraction = &wc, &at, &f

	alt := AltText(resp, [2]string{"Padborg", "Flensburg"}, Options{})
	if want := " Tightest point for the Fresnel zone: clearance " + fmtMetres(wc) + " at " + fmtMetres(at) + " along the path."; !strings.Contains(alt, want) {
		t.Errorf("og:image:alt %q\nlacks %q", alt, want)
	}
	_, runs, err := render(resp, [2]string{"Padborg", "Flensburg"}, DefaultPalette(false), Options{})
	if err != nil {
		t.Fatal(err)
	}
	summary := runText(runs, "summary")
	if want := "Tightest Fresnel point: clearance " + fmtMetres(wc) + " at " + fmtMetres(at); !strings.Contains(summary, want) {
		t.Errorf("summary %q\nlacks %q", summary, want)
	}
	for _, s := range []string{alt, summary} {
		if strings.Contains(strings.ToLower(s), "worst clearance") {
			t.Errorf("%q still calls the Fresnel point the worst clearance", s)
		}
	}
}
