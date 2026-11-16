package packetcard

import (
	"image/color"

	"github.com/meshcore-analyzer/cardkit"
)

// Palette is every colour the packet card uses, under the theme.json and
// customizer keys (customize-v2.js THEME_CSS_MAP) the packet path map
// draws with (packet-path-map.js): the path in accent, observers in
// statusYellow, the first to hear it ringed in statusGreen.
type Palette struct {
	Background, Text, TextMuted, Border color.NRGBA
	Accent, StatusGreen, StatusYellow   color.NRGBA
}

// fields maps each theme/themeDark key to its field, in a fixed order.
func fields(p *Palette) []cardkit.ColorField {
	return []cardkit.ColorField{
		{Key: "background", C: &p.Background}, {Key: "text", C: &p.Text}, {Key: "textMuted", C: &p.TextMuted},
		{Key: "border", C: &p.Border}, {Key: "accent", C: &p.Accent},
		{Key: "statusGreen", C: &p.StatusGreen}, {Key: "statusYellow", C: &p.StatusYellow},
	}
}

// DefaultPalette is the shipped palette, the customizer's default preset.
// TestDefaultPaletteMatchesFrontend pins every value to customize-v2.js.
func DefaultPalette(dark bool) Palette {
	type token struct{ light, dark string }
	vals := []token{
		{"#f4f5f7", "#0f0f23"}, {"#1a1a2e", "#e2e8f0"}, {"#5b6370", "#a8b8cc"},
		{"#e2e5ea", "#334155"}, {"#4a9eff", "#4a9eff"},
		{"#22c55e", "#22c55e"}, {"#eab308", "#eab308"},
	}
	var p Palette
	for i, f := range fields(&p) {
		v := vals[i].light
		if dark {
			v = vals[i].dark
		}
		*f.C, _ = cardkit.ParseCSSColor(v)
	}
	return p
}

// ResolveTheme is the palette the browser shows for one mode: theme and
// themeDark merged as the browser merges them (cardkit.MergeTheme). Each
// known key that parses replaces the shipped default; bad lists the keys
// whose value did not parse, never the values.
func ResolveTheme(theme, themeDark map[string]string, dark bool) (Palette, []string) {
	p := DefaultPalette(dark)
	bad := cardkit.ResolveColors(cardkit.MergeTheme(theme, themeDark, dark), fields(&p))
	return p, bad
}
