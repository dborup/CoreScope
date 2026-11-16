package nodecard

import (
	"image/color"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"

	"github.com/meshcore-analyzer/cardkit"
)

// The shipped palette is pinned to the files the browser takes it from:
// the base keys to the customizer's default preset (customize-v2.js), the
// role colours to the --mc-role-* defaults in style.css :root and to
// WONG_ROLE_DEFAULTS in roles.js.
func TestDefaultPaletteMatchesFrontend(t *testing.T) {
	root := filepath.Join("..", "..", "public")
	read := func(n string) []byte {
		b, err := os.ReadFile(filepath.Join(root, n))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	css, js, roles := read("style.css"), read("customize-v2.js"), read("roles.js")
	preset := regexp.MustCompile(`(?s)\bdefault: \{.*?\btheme: \{(.*?)\},\s*themeDark: \{(.*?)\}`).FindSubmatch(js)
	if preset == nil {
		t.Fatal("default preset not found in customize-v2.js")
	}
	wong := regexp.MustCompile(`(?s)WONG_ROLE_DEFAULTS = \{(.*?)\}`).FindSubmatch(roles)
	if wong == nil {
		t.Fatal("WONG_ROLE_DEFAULTS not found in roles.js")
	}
	parse := func(s string) string {
		c, ok := cardkit.ParseCSSColor(s)
		if !ok {
			t.Fatalf("frontend colour %q does not parse", s)
		}
		return string([]byte{c.R, c.G, c.B, c.A})
	}
	for _, dark := range []bool{false, true} {
		pal := DefaultPalette(dark)
		section := preset[1]
		if dark {
			section = preset[2]
		}
		for _, f := range baseFields(&pal) {
			m := regexp.MustCompile(`\b` + f.Key + `: '([^']+)'`).FindSubmatch(section)
			if m == nil {
				t.Fatalf("key %s not in the default preset", f.Key)
			}
			if parse(string(m[1])) != string([]byte{f.C.R, f.C.G, f.C.B, f.C.A}) {
				t.Errorf("dark=%v %s: card %v, preset %s", dark, f.Key, *f.C, m[1])
			}
		}
		for _, f := range roleFields(&pal) {
			m := regexp.MustCompile(`--mc-role-` + f.Key + `:\s*(#[0-9a-fA-F]+)`).FindSubmatch(css)
			w := regexp.MustCompile(`\b` + f.Key + `:\s*'([^']+)'`).FindSubmatch(wong[1])
			if m == nil || w == nil {
				t.Fatalf("role %s not in style.css or roles.js", f.Key)
			}
			got := string([]byte{f.C.R, f.C.G, f.C.B, f.C.A})
			if parse(string(m[1])) != got || parse(string(w[1])) != got {
				t.Errorf("role %s: card %v, style.css %s, roles.js %s", f.Key, *f.C, m[1], w[1])
			}
		}
	}
}

// theme.json is merged as the browser merges it, nodeColors sets the role
// colours, and bad values are reported by key, never by value.
func TestResolveTheme(t *testing.T) {
	th := loadOperatorTheme(t)
	light, bad := ResolveTheme(th.Theme, th.ThemeDark, th.NodeColors, false)
	if len(bad) != 0 {
		t.Fatalf("bad %v", bad)
	}
	dark, _ := ResolveTheme(th.Theme, th.ThemeDark, th.NodeColors, true)
	want := func(s string) [4]uint8 { c, _ := cardkit.ParseCSSColor(s); return [4]uint8{c.R, c.G, c.B, c.A} }
	for _, c := range []struct {
		name string
		got  [4]uint8
		want string
	}{
		{"light background", rgba(light.Background), "#fef7f2"},
		{"dark background", rgba(dark.Background), "#1a0f08"},
		{"dark statusRed", rgba(dark.StatusRed), "#f87171"},
		{"repeater", rgba(light.Repeater), "#7c3aed"},
		{"dark room", rgba(dark.Room), "#0e7490"},
	} {
		if c.got != want(c.want) {
			t.Errorf("%s: %v, want %s", c.name, c.got, c.want)
		}
	}
	// A key only in theme applies in dark mode too.
	merged, _ := ResolveTheme(map[string]string{"statusRed": "#123456"}, nil, nil, true)
	if rgba(merged.StatusRed) != want("#123456") {
		t.Error("theme-only key not applied in dark mode")
	}
	_, bad = ResolveTheme(map[string]string{"text": "red"}, nil, map[string]string{"sensor": "url(x)", "companion": "nope"}, false)
	if !reflect.DeepEqual(bad, []string{"text", "nodeColors.sensor"}) {
		t.Errorf("bad %v", bad)
	}
}

func rgba(c color.NRGBA) [4]uint8 { return [4]uint8{c.R, c.G, c.B, c.A} }
