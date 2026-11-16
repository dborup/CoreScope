package packetcard

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"

	"github.com/meshcore-analyzer/cardkit"
)

// The shipped palette is the customizer's default preset (customize-v2.js).
func TestDefaultPaletteMatchesFrontend(t *testing.T) {
	js, err := os.ReadFile(filepath.Join("..", "..", "public", "customize-v2.js"))
	if err != nil {
		t.Fatal(err)
	}
	preset := regexp.MustCompile(`(?s)\bdefault: \{.*?\btheme: \{(.*?)\},\s*themeDark: \{(.*?)\}`).FindSubmatch(js)
	if preset == nil {
		t.Fatal("default preset not found in customize-v2.js")
	}
	for _, dark := range []bool{false, true} {
		pal := DefaultPalette(dark)
		section := preset[1]
		if dark {
			section = preset[2]
		}
		for _, f := range fields(&pal) {
			m := regexp.MustCompile(`\b` + f.Key + `: '([^']+)'`).FindSubmatch(section)
			if m == nil {
				t.Fatalf("key %s not in the default preset", f.Key)
			}
			c, ok := cardkit.ParseCSSColor(string(m[1]))
			if !ok || c != *f.C {
				t.Errorf("dark=%v %s: card %v, preset %s", dark, f.Key, *f.C, m[1])
			}
		}
	}
}

// theme.json is merged as the browser merges it, and bad values are
// reported by key, never by value.
func TestResolveTheme(t *testing.T) {
	th := loadOperatorTheme(t)
	light, bad := ResolveTheme(th.Theme, th.ThemeDark, false)
	if len(bad) != 0 {
		t.Fatalf("bad %v", bad)
	}
	dark, _ := ResolveTheme(th.Theme, th.ThemeDark, true)
	want := func(s string) [4]uint8 { c, _ := cardkit.ParseCSSColor(s); return [4]uint8{c.R, c.G, c.B, c.A} }
	rgba := func(c interface{ RGBA() (r, g, b, a uint32) }) [4]uint8 {
		r, g, b, a := c.RGBA()
		return [4]uint8{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)}
	}
	for _, c := range []struct {
		name string
		got  [4]uint8
		want string
	}{
		{"light accent", rgba(light.Accent), "#c2410c"},
		{"dark accent", rgba(dark.Accent), "#fb923c"},
		{"dark statusGreen (theme only)", rgba(dark.StatusGreen), "#15803d"},
		{"light statusYellow", rgba(light.StatusYellow), "#a16207"},
	} {
		if c.got != want(c.want) {
			t.Errorf("%s: %v, want %s", c.name, c.got, c.want)
		}
	}
	_, bad = ResolveTheme(map[string]string{"accent": "red", "nope": "#fff"}, map[string]string{"statusGreen": "url(x)"}, true)
	if !reflect.DeepEqual(bad, []string{"accent", "statusGreen"}) {
		t.Errorf("bad %v", bad)
	}
}
