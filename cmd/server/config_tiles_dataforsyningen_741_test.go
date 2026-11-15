package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Issue #741 M1b: the Dataforsyningen Skærmkort styles are opt-in, gated in
// the browser by map.tiles.providers.dataforsyningen.enabled === true and a
// non-blank token. A default naming one while it is off is skipped by the
// browser; the server says so at startup, as it does for Esri (#728).

// The gated-default warning (#728) covers the Dataforsyningen styles too:
// they need enabled === true and a non-empty token, as the browser does.
func TestWarnGatedTileDefaults741(t *testing.T) {
	const want = "map.tiles.providers.dataforsyningen.token"
	cases := []struct {
		name string
		cfg  string
		keys []string // the defaults expected to be reported, in order
	}{
		{"no dataforsyningen block",
			`{"map":{"tiles":{"lightDefault":"dataforsyningen-skaermkort"}}}`, []string{"lightDefault"}},
		{"enabled without a token",
			`{"map":{"tiles":{"darkDefault":"dataforsyningen-skaermkort-graa-dark","providers":{"dataforsyningen":{"enabled":true}}}}}`, []string{"darkDefault"}},
		{"enabled with a blank token",
			`{"map":{"tiles":{"lightDefault":"dataforsyningen-skaermkort-graa","providers":{"dataforsyningen":{"enabled":true,"token":"   "}}}}}`, []string{"lightDefault"}},
		{"enabled with a non-string token",
			`{"map":{"tiles":{"lightDefault":"dataforsyningen-skaermkort","providers":{"dataforsyningen":{"enabled":true,"token":42}}}}}`, []string{"lightDefault"}},
		{"a token but not enabled",
			`{"map":{"tiles":{"lightDefault":"dataforsyningen-skaermkort","providers":{"dataforsyningen":{"token":"t"}}}}}`, []string{"lightDefault"}},
		{"string \"true\" is not the boolean the browser needs",
			`{"map":{"tiles":{"lightDefault":"dataforsyningen-skaermkort","providers":{"dataforsyningen":{"enabled":"true","token":"t"}}}}}`, []string{"lightDefault"}},
		{"both defaults",
			`{"map":{"tiles":{"darkDefault":"dataforsyningen-skaermkort-graa-dark","lightDefault":"dataforsyningen-skaermkort-daempet"}}}`, []string{"darkDefault", "lightDefault"}},
		{"providers.Dataforsyningen (wrong case) does not enable it",
			`{"map":{"tiles":{"lightDefault":"dataforsyningen-skaermkort","providers":{"Dataforsyningen":{"enabled":true,"token":"t"}}}}}`, []string{"lightDefault"}},
		{"dataforsyningen.Token (wrong case) is no token",
			`{"map":{"tiles":{"lightDefault":"dataforsyningen-skaermkort","providers":{"dataforsyningen":{"enabled":true,"Token":"t"}}}}}`, []string{"lightDefault"}},
		{"providers is not an object",
			`{"map":{"tiles":{"lightDefault":"dataforsyningen-skaermkort","providers":"x"}}}`, []string{"lightDefault"}},
		{"providers.dataforsyningen is not an object",
			`{"map":{"tiles":{"lightDefault":"dataforsyningen-skaermkort","providers":{"dataforsyningen":true}}}}`, []string{"lightDefault"}},
		{"legacy top-level mapDarkTileProvider is migrated, then checked",
			`{"mapDarkTileProvider":"dataforsyningen-skaermkort-graa-dark"}`, []string{"darkDefault"}},
		{"enabled with a token",
			`{"map":{"tiles":{"darkDefault":"dataforsyningen-skaermkort-graa-dark","lightDefault":"dataforsyningen-skaermkort","providers":{"dataforsyningen":{"enabled":true,"token":"t"}}}}}`, nil},
		{"enabled, no dataforsyningen default",
			`{"map":{"tiles":{"darkDefault":"osm-dark","providers":{"dataforsyningen":{"enabled":true}}}}}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := esriTileWarnings(t, tc.cfg)
			if len(got) != len(tc.keys) {
				t.Fatalf("got %d warnings, want %d: %q", len(got), len(tc.keys), got)
			}
			for i, k := range tc.keys {
				if !strings.HasPrefix(got[i], "[config] map.tiles."+k+" is ") {
					t.Errorf("warning %d = %q, want it about map.tiles.%s", i, got[i], k)
				}
				for _, s := range []string{"map.tiles.providers.dataforsyningen.enabled", want, "#741"} {
					if !strings.Contains(got[i], s) {
						t.Errorf("warning %d does not name %s: %q", i, s, got[i])
					}
				}
				if strings.Contains(got[i], "esri") {
					t.Errorf("warning %d names the Esri switch: %q", i, got[i])
				}
			}
		})
	}
}

// One provider's switch never silences or adds the other's warning.
func TestWarnGatedTileDefaultsEsriAndDataforsyningen741(t *testing.T) {
	got := esriTileWarnings(t, `{"map":{"tiles":{"darkDefault":"esri-darkgray-labels","lightDefault":"dataforsyningen-skaermkort",
		"providers":{"esri":{"enabled":true}}}}}`)
	if len(got) != 1 || !strings.Contains(got[0], "lightDefault") || !strings.Contains(got[0], "dataforsyningen") {
		t.Fatalf("want only the Dataforsyningen warning, got %q", got)
	}
	got = esriTileWarnings(t, `{"map":{"tiles":{"darkDefault":"esri-darkgray-labels","lightDefault":"dataforsyningen-skaermkort",
		"providers":{"dataforsyningen":{"enabled":true,"token":"t"}}}}}`)
	if len(got) != 1 || !strings.Contains(got[0], "darkDefault") || !strings.Contains(got[0], "providers.esri.enabled") {
		t.Fatalf("want only the Esri warning, got %q", got)
	}
}

// The shipped example keeps the provider off, with an empty token, and
// says that the token is sent to browsers and is not the DHM token.
func TestDataforsyningenExampleConfigIsOff741(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ex struct {
		Map struct {
			Tiles struct {
				Providers struct {
					Comment         string `json:"_comment_dataforsyningen"`
					Dataforsyningen *struct {
						Enabled *bool   `json:"enabled"`
						Token   *string `json:"token"`
					} `json:"dataforsyningen"`
				} `json:"providers"`
			} `json:"tiles"`
		} `json:"map"`
	}
	if err := json.Unmarshal(data, &ex); err != nil {
		t.Fatal(err)
	}
	d := ex.Map.Tiles.Providers.Dataforsyningen
	if d == nil || d.Enabled == nil || *d.Enabled || d.Token == nil || *d.Token != "" {
		t.Fatalf("config.example.json must ship map.tiles.providers.dataforsyningen as {enabled: false, token: \"\"}, got %+v", d)
	}
	c := ex.Map.Tiles.Providers.Comment
	for _, s := range []string{"sent to the browser", "map.dhm.token", "Denmark only", "rotate"} {
		if !strings.Contains(c, s) {
			t.Errorf("_comment_dataforsyningen does not say %q: %s", s, c)
		}
	}
	// The example's own defaults are not gated off.
	for _, w := range esriTileWarnings(t, string(data)) {
		t.Fatalf("the example warns about its own defaults: %s", w)
	}
}
