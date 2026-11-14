package profilecard

import (
	"image/color"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Palette is every colour the card uses. The keys are the ones theme.json
// and the customizer already use (customize-v2.js THEME_CSS_MAP), so the
// card and the panel are drawn from one palette.
type Palette struct {
	Background, Text, TextMuted, Border   color.NRGBA
	StatusGreen, StatusYellow, StatusRed  color.NRGBA
	TerrainFill, TerrainClutter, LosLine  color.NRGBA
	Fresnel100, Fresnel60, ProfileBlocked color.NRGBA
}

type paletteField struct {
	key string // theme.json key
	c   *color.NRGBA
}

// paletteFields maps each theme.json key to its field in p, in a fixed
// order.
func paletteFields(p *Palette) []paletteField {
	return []paletteField{
		{"background", &p.Background}, {"text", &p.Text}, {"textMuted", &p.TextMuted}, {"border", &p.Border},
		{"statusGreen", &p.StatusGreen}, {"statusYellow", &p.StatusYellow}, {"statusRed", &p.StatusRed},
		{"terrainFill", &p.TerrainFill}, {"terrainClutter", &p.TerrainClutter}, {"losLine", &p.LosLine},
		{"fresnel100", &p.Fresnel100}, {"fresnel60", &p.Fresnel60}, {"profileBlocked", &p.ProfileBlocked},
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
	}
	var p Palette
	fields := paletteFields(&p)
	for i, t := range tokens {
		v := t.light
		if dark {
			v = t.dark
		}
		*fields[i].c, _ = ParseCSSColor(v)
	}
	return p
}

// ResolvePalette is the palette for one theme.json section ("theme" or
// "themeDark"): each known key that parses replaces the shipped default of
// the same section; keys it does not know are ignored. bad lists, sorted,
// the keys whose value did not parse, so the caller can warn once per key.
// It never returns a value: theme.json is operator input and does not
// belong in a log line.
func ResolvePalette(section map[string]string, dark bool) (p Palette, bad []string) {
	p = DefaultPalette(dark)
	for _, f := range paletteFields(&p) {
		v, ok := section[f.key]
		if !ok {
			continue
		}
		if c, ok := ParseCSSColor(v); ok {
			*f.c = c
		} else {
			bad = append(bad, f.key)
		}
	}
	sort.Strings(bad)
	return p, bad
}

var (
	hexColorRe = regexp.MustCompile(`^#([0-9a-fA-F]{3,4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`)
	rgbRe      = regexp.MustCompile(`^(?i:rgb)\(\s*(\d{1,3})\s*,\s*(\d{1,3})\s*,\s*(\d{1,3})\s*\)$`)
	rgbaRe     = regexp.MustCompile(`^(?i:rgba)\(\s*(\d{1,3})\s*,\s*(\d{1,3})\s*,\s*(\d{1,3})\s*,\s*([01]|0?\.\d{1,6}|1\.0{1,6})\s*\)$`)
)

// ParseCSSColor reads the CSS colour forms theme.json and style.css use for
// these keys: #rgb, #rgba, #rrggbb, #rrggbbaa, rgb(r, g, b) and
// rgba(r, g, b, a) with integer channels 0–255 and a plain-decimal alpha
// 0–1. Anything else (names, var(), hsl(), exponents, percentages) is
// rejected rather than guessed at.
func ParseCSSColor(s string) (color.NRGBA, bool) {
	s = strings.TrimSpace(s)
	if len(s) > 64 {
		return color.NRGBA{}, false
	}
	if hexColorRe.MatchString(s) {
		h := s[1:]
		if len(h) <= 4 {
			var b strings.Builder
			for _, c := range h {
				b.WriteRune(c)
				b.WriteRune(c)
			}
			h = b.String()
		}
		if len(h) == 6 {
			h += "ff"
		}
		v, err := strconv.ParseUint(h, 16, 32)
		if err != nil {
			return color.NRGBA{}, false
		}
		return color.NRGBA{uint8(v >> 24), uint8(v >> 16), uint8(v >> 8), uint8(v)}, true
	}
	m := rgbRe.FindStringSubmatch(s)
	alpha := uint8(255)
	if m == nil {
		m = rgbaRe.FindStringSubmatch(s)
		if m == nil {
			return color.NRGBA{}, false
		}
		a, err := strconv.ParseFloat(m[4], 64)
		if err != nil || a < 0 || a > 1 {
			return color.NRGBA{}, false
		}
		alpha = uint8(math.Floor(float64(a*255) + 0.5))
	}
	var ch [3]uint8
	for i := range ch {
		v, err := strconv.Atoi(m[i+1])
		if err != nil || v > 255 {
			return color.NRGBA{}, false
		}
		ch[i] = uint8(v)
	}
	return color.NRGBA{ch[0], ch[1], ch[2], alpha}, true
}
