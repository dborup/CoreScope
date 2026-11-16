package cardkit

import (
	"image/color"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ColorField ties one theme.json key to the palette field it sets. A card
// package lists its own fields; the order is the card's, and fixed.
type ColorField struct {
	Key string // theme.json key, as customize-v2.js THEME_CSS_MAP names it
	C   *color.NRGBA
}

// ResolveColors overwrites each field whose key is in section and parses;
// a field whose key is absent keeps the value the caller put there (the
// card's shipped default). Keys section has but fields do not are ignored.
// bad lists, sorted, the keys whose value did not parse, so the caller can
// warn once per key. It never returns a value: theme.json is operator input
// and does not belong in a log line.
func ResolveColors(section map[string]string, fields []ColorField) (bad []string) {
	for _, f := range fields {
		v, ok := section[f.Key]
		if !ok {
			continue
		}
		if c, ok := ParseCSSColor(v); ok {
			*f.C = c
		} else {
			bad = append(bad, f.Key)
		}
	}
	sort.Strings(bad)
	return bad
}

// MergeTheme is the theme.json section the browser draws for one mode,
// from the sections theme and themeDark: in dark mode the browser draws
// Object.assign({}, theme, themeDark) (public/app.js reapplyUserThemeVars),
// so a key set only in theme applies in dark mode too, and themeDark wins
// where both have it; light mode reads theme alone. The inputs are not
// modified; in light mode theme itself is returned.
func MergeTheme(theme, themeDark map[string]string, dark bool) map[string]string {
	if !dark {
		return theme
	}
	merged := make(map[string]string, len(theme)+len(themeDark))
	for k, v := range theme {
		merged[k] = v
	}
	for k, v := range themeDark {
		merged[k] = v
	}
	return merged
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
