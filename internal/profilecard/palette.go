package profilecard

import (
	"image/color"

	"github.com/meshcore-analyzer/cardkit"
)

// Palette is every colour the card uses. The keys are the ones theme.json
// and the customizer already use (customize-v2.js THEME_CSS_MAP), so the
// card and the panel are drawn from one palette.
type Palette struct {
	Background, Text, TextMuted, Border   color.NRGBA
	StatusGreen, StatusYellow, StatusRed  color.NRGBA
	TerrainFill, TerrainClutter, LosLine  color.NRGBA
	Fresnel100, Fresnel60, ProfileBlocked color.NRGBA
	// The surface band's classes (#846): inside a building footprint, and
	// raised with none (trees/other), as the panel draws them (#797).
	TerrainBuilding, TerrainVegetation color.NRGBA
}

// paletteFields maps each theme.json key to its field in p, in a fixed
// order.
func paletteFields(p *Palette) []cardkit.ColorField {
	return []cardkit.ColorField{
		{Key: "background", C: &p.Background}, {Key: "text", C: &p.Text}, {Key: "textMuted", C: &p.TextMuted}, {Key: "border", C: &p.Border},
		{Key: "statusGreen", C: &p.StatusGreen}, {Key: "statusYellow", C: &p.StatusYellow}, {Key: "statusRed", C: &p.StatusRed},
		{Key: "terrainFill", C: &p.TerrainFill}, {Key: "terrainClutter", C: &p.TerrainClutter}, {Key: "losLine", C: &p.LosLine},
		{Key: "fresnel100", C: &p.Fresnel100}, {Key: "fresnel60", C: &p.Fresnel60}, {Key: "profileBlocked", C: &p.ProfileBlocked},
		{Key: "terrainBuilding", C: &p.TerrainBuilding}, {Key: "terrainVegetation", C: &p.TerrainVegetation},
	}
}

// DefaultPalette is the shipped palette: what a deployment without
// theme.json shows in the browser. These are not a second source of
// truth — TestDefaultPaletteMatchesFrontend pins each value to
// public/style.css (the terrain keys) and to the customizer's default
// preset in public/customize-v2.js (the rest), so a colour changed there
// and not here fails the build.
func DefaultPalette(dark bool) Palette {
	type token struct{ key, light, dark string }
	tokens := []token{
		{"background", "#f4f5f7", "#0f0f23"},
		{"text", "#1a1a2e", "#e2e8f0"},
		{"textMuted", "#5b6370", "#a8b8cc"},
		{"border", "#e2e5ea", "#334155"},
		{"statusGreen", "#22c55e", "#22c55e"},
		{"statusYellow", "#eab308", "#eab308"},
		{"statusRed", "#ef4444", "#ef4444"},
		{"terrainFill", "#9a8a73", "#5d5444"},
		{"terrainClutter", "rgba(84, 124, 92, 0.55)", "rgba(122, 170, 132, 0.45)"},
		{"losLine", "#1d4ed8", "#60a5fa"},
		{"fresnel100", "#6d28d9", "#c4b5fd"},
		{"fresnel60", "#a78bfa", "#8b5cf6"},
		{"profileBlocked", "#b91c1c", "#f87171"},
		{"terrainBuilding", "#5e1f0c", "#e6cdb0"},
		{"terrainVegetation", "#5a9466", "#337f4c"},
	}
	var p Palette
	fields := paletteFields(&p)
	for i, t := range tokens {
		v := t.light
		if dark {
			v = t.dark
		}
		*fields[i].C, _ = cardkit.ParseCSSColor(v)
	}
	return p
}

// ResolvePalette is the palette for one theme.json section ("theme" or
// "themeDark"): each known key that parses replaces the shipped default of
// the same section; keys it does not know are ignored. bad is as for
// cardkit.ResolveColors.
func ResolvePalette(section map[string]string, dark bool) (p Palette, bad []string) {
	p = DefaultPalette(dark)
	// Two statements: in "return p, f(&p)" Go does not specify whether p
	// is read before or after f writes through &p.
	bad = cardkit.ResolveColors(section, paletteFields(&p))
	return p, bad
}

// ResolveTheme is the palette the browser shows for one mode, from the
// theme.json-style sections theme and themeDark, merged as the browser
// merges them (cardkit.MergeTheme). bad is as for ResolvePalette, for the
// merged section: a bad value that themeDark overrides is never used, so
// it is not reported.
func ResolveTheme(theme, themeDark map[string]string, dark bool) (Palette, []string) {
	return ResolvePalette(cardkit.MergeTheme(theme, themeDark, dark), dark)
}
