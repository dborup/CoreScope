package cardkit

import (
	"image/color"
	"strings"
	"testing"
)

func mustFonts(t testing.TB) *Fonts {
	t.Helper()
	f, err := LoadFonts()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestCardName(t *testing.T) {
	fonts := mustFonts(t)
	cases := []struct{ in, want string }{
		{"Søby Bakker", "Søby Bakker"},
		{"Ørsted Mast — Æblehaven", "Ørsted Mast — Æblehaven"},
		{"Łódź · Straße · Žižkov", "Łódź · Straße · Žižkov"},
		{"Москва α", "Москва α"}, // Cyrillic and Greek are in the Go fonts
		{"东京中继站", ""},            // CJK: all .notdef → fall back
		{"🛰🛰🛰 ab", ""},           // mostly unmappable → fall back
		{"Mast 🛰", "Mast"},       // one stray glyph is dropped, not boxed
		{"Relay 🛰 North", "Relay North"},
		{"  spaced \t\n  out  ", "spaced out"},
		{"ctl\x00\x07\u0085x\u2028y\u202ez", "ctl x yz"},
		{"", ""},
		{"   ", ""},
		{strings.Repeat("a", 100), strings.Repeat("a", MaxNameRunes-1) + "…"},
		{strings.Repeat("ø", 100), strings.Repeat("ø", MaxNameRunes-1) + "…"},
		{"\xff\xfe broken utf8", "broken utf8"},
	}
	for _, c := range cases {
		if got := fonts.DrawableName(c.in); got != c.want {
			t.Errorf("DrawableName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// #755 review Low: a name in NFD (as macOS file names and some keyboards
// produce it) keeps its diacritics: it is composed before the glyph check,
// which would otherwise drop the combining marks.
func TestCardNameNFC(t *testing.T) {
	fonts := mustFonts(t)
	for in, want := range map[string]string{
		"Åbenrå":        "Åbenrå",
		"Kobenhavn Vést": "Kobenhavn Vést",
		"Mälmö Hub":     "Mälmö Hub",
		"Åbenrå":          "Åbenrå", // already NFC: unchanged
	} {
		if got := fonts.DrawableName(in); got != want {
			t.Errorf("DrawableName(%q) = %q, want %q", in, got, want)
		}
	}
}

// #755 review M8: the fallback threshold is "more than one rune in five
// unmappable", exactly.
func TestCardNameFallbackThreshold(t *testing.T) {
	fonts := mustFonts(t)
	for in, want := range map[string]string{
		"abc🛰":       "",         // 1 of 4: fallback
		"abcd🛰":      "abcd",     // 1 of 5: kept, the stray glyph dropped
		"abcdefgh🛰🛰": "abcdefgh", // 2 of 10: kept
		"abcdefg🛰🛰":  "",         // 2 of 9: fallback
		"ab cd 🛰":    "ab cd",    // spaces do not count: 1 of 5
		"a b c 🛰":    "",         // 1 of 4
	} {
		if got := fonts.DrawableName(in); got != want {
			t.Errorf("DrawableName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMeasureFitDraw(t *testing.T) {
	fonts := mustFonts(t)
	s := "Søby Bakker → Ørsted Mast"
	w := fonts.Measure(s, 24, true)
	if w <= 0 || fonts.Measure(s, 24, false) >= w {
		t.Fatalf("bold %d px should be wider than regular", w)
	}
	if got := fonts.Fit(s, 24, true, w); got != s {
		t.Errorf("Fit at its own width = %q", got)
	}
	got := fonts.Fit(s, 24, true, w/2)
	if !strings.HasSuffix(got, "…") || fonts.Measure(got, 24, true) > w/2 {
		t.Errorf("Fit at half width = %q (%d px)", got, fonts.Measure(got, 24, true))
	}
	if got := fonts.Fit(s, 24, true, 2); got != "" {
		t.Errorf("Fit with no room = %q, want empty", got)
	}
	if !fonts.Has(true, 'ø') || fonts.Has(true, '东') {
		t.Error("Has: Go fonts cover ø and not CJK")
	}

	cv := newCanvas(400, 60, color.NRGBA{255, 255, 255, 255})
	drawn := fonts.Draw(cv, s, 10, 40, 24, true, color.NRGBA{0, 0, 0, 255})
	if drawn != w {
		t.Errorf("Draw returned %d px, Measure says %d", drawn, w)
	}
	ink := 0
	for y := 0; y < 60; y++ {
		for x := 0; x < 400; x++ {
			if cv.Img.RGBAAt(x, y).R < 128 {
				ink++
				if x < 10 || x > 10+w+2 {
					t.Fatalf("ink at x=%d, outside the drawn run 10..%d", x, 10+w)
				}
			}
		}
	}
	if ink == 0 {
		t.Fatal("Draw painted nothing")
	}
}
