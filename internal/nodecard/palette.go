package nodecard

import (
	"image/color"

	"github.com/meshcore-analyzer/cardkit"
)

// Palette is every colour the node card uses. The base keys are the
// theme.json/customizer keys the terrain card uses too (customize-v2.js
// THEME_CSS_MAP); the role colours are the ROLE_COLORS keys, which
// theme.json carries in its own nodeColors section.
type Palette struct {
	Background, Text, TextMuted, Border, StatusRed color.NRGBA
	Repeater, Room, Sensor                         color.NRGBA
}

// baseFields maps each theme/themeDark key to its field, in a fixed order.
func baseFields(p *Palette) []cardkit.ColorField {
	return []cardkit.ColorField{
		{Key: "background", C: &p.Background}, {Key: "text", C: &p.Text}, {Key: "textMuted", C: &p.TextMuted},
		{Key: "border", C: &p.Border}, {Key: "statusRed", C: &p.StatusRed},
	}
}

// roleFields maps each nodeColors key to its field. Only the carded roles
// have one: the card never draws a companion or observer colour.
func roleFields(p *Palette) []cardkit.ColorField {
	return []cardkit.ColorField{{Key: "repeater", C: &p.Repeater}, {Key: "room", C: &p.Room}, {Key: "sensor", C: &p.Sensor}}
}

// DefaultPalette is the shipped palette, what a deployment without
// theme.json shows: the base keys from the customizer's default preset,
// the role colours from the --mc-role-* defaults in style.css :root (the
// same in both modes). TestDefaultPaletteMatchesFrontend pins every value
// to those files.
func DefaultPalette(dark bool) Palette {
	type token struct{ key, light, dark string }
	base := []token{
		{"background", "#f4f5f7", "#0f0f23"},
		{"text", "#1a1a2e", "#e2e8f0"},
		{"textMuted", "#5b6370", "#a8b8cc"},
		{"border", "#e2e5ea", "#334155"},
		{"statusRed", "#ef4444", "#ef4444"},
	}
	roles := []string{"#D55E00", "#009E73", "#F0E442"}
	var p Palette
	for i, f := range baseFields(&p) {
		v := base[i].light
		if dark {
			v = base[i].dark
		}
		*f.C, _ = cardkit.ParseCSSColor(v)
	}
	for i, f := range roleFields(&p) {
		*f.C, _ = cardkit.ParseCSSColor(roles[i])
	}
	return p
}

// ResolveTheme is the palette the browser shows for one mode: theme and
// themeDark merged as the browser merges them (cardkit.MergeTheme), and the
// role colours from nodeColors. Each known key that parses replaces the
// shipped default; unknown keys are ignored. bad lists the keys whose value
// did not parse, the nodeColors ones as "nodeColors.<key>"; never values.
func ResolveTheme(theme, themeDark, nodeColors map[string]string, dark bool) (Palette, []string) {
	p := DefaultPalette(dark)
	bad := cardkit.ResolveColors(cardkit.MergeTheme(theme, themeDark, dark), baseFields(&p))
	for _, k := range cardkit.ResolveColors(nodeColors, roleFields(&p)) {
		bad = append(bad, "nodeColors."+k)
	}
	return p, bad
}

// roleColor is the badge and accent colour for a carded role.
func (p Palette) roleColor(role string) color.NRGBA {
	switch role {
	case "repeater":
		return p.Repeater
	case "room":
		return p.Room
	}
	return p.Sensor
}
