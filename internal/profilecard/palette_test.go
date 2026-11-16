package profilecard

import (
	"image/color"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"

	"github.com/meshcore-analyzer/cardkit"
)

// The shipped palette is not a second copy of the colours: it is pinned to
// the two files the browser takes them from. The six terrain keys come from
// public/style.css (:root for light, every dark block for dark), the base
// keys from the default preset in public/customize-v2.js, which is what a
// deployment without theme.json shows.
func TestDefaultPaletteMatchesFrontend(t *testing.T) {
	root := filepath.Join("..", "..", "public")
	css, err := os.ReadFile(filepath.Join(root, "style.css"))
	if err != nil {
		t.Fatal(err)
	}
	js, err := os.ReadFile(filepath.Join(root, "customize-v2.js"))
	if err != nil {
		t.Fatal(err)
	}
	terrainVars := map[string]string{
		"terrainFill": "--terrain-fill", "terrainClutter": "--terrain-clutter", "losLine": "--los-line",
		"fresnel100": "--fresnel-100", "fresnel60": "--fresnel-60", "profileBlocked": "--profile-blocked",
	}
	preset := regexp.MustCompile(`(?s)\bdefault: \{.*?\btheme: \{(.*?)\},\s*themeDark: \{(.*?)\}`).FindSubmatch(js)
	if preset == nil {
		t.Fatal("default preset not found in customize-v2.js")
	}
	presetValue := func(section []byte, key string) string {
		m := regexp.MustCompile(`\b` + key + `: '([^']+)'`).FindSubmatch(section)
		if m == nil {
			t.Fatalf("key %s not in the default preset", key)
		}
		return string(m[1])
	}
	for _, dark := range []bool{false, true} {
		pal := DefaultPalette(dark)
		for _, pf := range paletteFields(&pal) {
			key := pf.Key
			var want string
			if v, ok := terrainVars[key]; ok {
				all := regexp.MustCompile(regexp.QuoteMeta(v)+`:\s*(#[0-9a-fA-F]+|rgba?\([^)]*\))\s*;`).FindAllSubmatch(css, -1)
				if len(all) < 2 {
					t.Fatalf("%s: expected a light and at least one dark value in style.css", v)
				}
				want = string(all[0][1])
				if dark {
					want = string(all[1][1])
					for _, m := range all[2:] {
						if string(m[1]) != want {
							t.Fatalf("%s: dark blocks disagree in style.css: %s vs %s", v, m[1], want)
						}
					}
				}
			} else {
				section := preset[1]
				if dark {
					section = preset[2]
				}
				want = presetValue(section, key)
			}
			wantC, ok := cardkit.ParseCSSColor(want)
			if !ok {
				t.Fatalf("%s: frontend value %q does not parse", key, want)
			}
			got := *pf.C
			if got != wantC {
				t.Errorf("dark=%v %s = %v; frontend has %s", dark, key, got, want)
			}
		}
	}
}

func TestResolvePalette(t *testing.T) {
	th := loadOperatorTheme(t)

	pal, bad := ResolvePalette(th.ThemeDark, true)
	if want := []string{"profileBlocked"}; !reflect.DeepEqual(bad, want) {
		t.Errorf("bad keys = %v, want %v", bad, want)
	}
	if pal.LosLine != (color.NRGBA{0xfb, 0x92, 0x3c, 0xff}) {
		t.Errorf("losLine not taken from the theme: %v", pal.LosLine)
	}
	if pal.TerrainClutter != (color.NRGBA{74, 222, 128, 89}) {
		t.Errorf("terrainClutter rgba not taken from the theme: %v", pal.TerrainClutter)
	}
	// Invalid → the shipped dark default, not the light one, not black.
	if pal.ProfileBlocked != DefaultPalette(true).ProfileBlocked {
		t.Errorf("invalid profileBlocked = %v, want the dark default", pal.ProfileBlocked)
	}
	// A key the theme leaves out keeps the default of the same section.
	if pal.StatusGreen != DefaultPalette(true).StatusGreen {
		t.Errorf("statusGreen = %v, want the dark default", pal.StatusGreen)
	}

	light, bad := ResolvePalette(th.Theme, false)
	if want := []string{"profileBlocked"}; !reflect.DeepEqual(bad, want) {
		t.Errorf("light bad keys = %v, want %v", bad, want)
	}
	if light.Background != (color.NRGBA{0xfe, 0xf7, 0xf2, 0xff}) {
		t.Errorf("light background = %v", light.Background)
	}
	if light.ProfileBlocked != DefaultPalette(false).ProfileBlocked {
		t.Errorf("invalid light profileBlocked = %v, want the light default", light.ProfileBlocked)
	}

	if got, bad := ResolvePalette(nil, true); got != DefaultPalette(true) || len(bad) != 0 {
		t.Errorf("nil section = %v, %v; want the dark default, no bad keys", got, bad)
	}
	if got, _ := ResolvePalette(nil, false); got != DefaultPalette(false) {
		t.Errorf("nil light section = %v; want the light default", got)
	}
	if DefaultPalette(true) == DefaultPalette(false) {
		t.Error("dark and light defaults are identical")
	}

	// Bad keys come back sorted and name only the key, never the value.
	_, bad = ResolvePalette(map[string]string{"text": "<script>", "border": "nope", "accent": "ignored"}, true)
	if want := []string{"border", "text"}; !reflect.DeepEqual(bad, want) {
		t.Errorf("bad keys = %v, want %v", bad, want)
	}
}
