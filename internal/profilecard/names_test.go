package profilecard

import (
	"strings"
	"testing"
)

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
		{strings.Repeat("a", 100), strings.Repeat("a", maxNameRunes-1) + "…"},
		{strings.Repeat("ø", 100), strings.Repeat("ø", maxNameRunes-1) + "…"},
		{"\xff\xfe broken utf8", "broken utf8"},
	}
	for _, c := range cases {
		if got := fonts.cardName(c.in); got != c.want {
			t.Errorf("cardName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEndpointLabel(t *testing.T) {
	fonts := mustFonts(t)
	node := TerrainEndpoint{Kind: "node", Lat: 55.123456, Lon: 10.654321}
	point := TerrainEndpoint{Kind: "point", Lat: 55.123456, Lon: -3.5}
	cases := []struct {
		end  TerrainEndpoint
		name string
		i    int
		hide bool
		want string
	}{
		{node, "Søby Bakker", 0, false, "Søby Bakker"},
		{node, "Søby Bakker", 1, true, "Node B"}, // names off: never the name
		{node, "东京", 0, false, "Node A"},
		{node, "", 1, false, "Node B"},
		// A map point has no name, whatever the caller passes.
		{point, "Ignored", 0, false, "55.1235, -3.5000"},
		{point, "", 1, true, "55.1235, -3.5000"},
		// Kind missing or odd: treated as a node, never as a name leak.
		{TerrainEndpoint{}, "", 0, false, "Node A"},
	}
	for _, c := range cases {
		if got := fonts.endpointLabel(c.end, c.name, c.i, c.hide); got != c.want {
			t.Errorf("endpointLabel(%+v, %q, %d, %v) = %q, want %q", c.end, c.name, c.i, c.hide, got, c.want)
		}
	}
}

func mustFonts(t testing.TB) *fontSet {
	t.Helper()
	f, err := loadFonts()
	if err != nil {
		t.Fatal(err)
	}
	return f
}
