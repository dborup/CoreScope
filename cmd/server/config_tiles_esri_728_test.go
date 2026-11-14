package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
)

// Issue #728: every esri-* tile style is opt-in behind
// map.tiles.providers.esri.enabled. A config that still names one as its
// dark/light default keeps a working map (the browser falls back), but the
// operator must be told at startup.

func esriTileWarnings(t *testing.T, cfgJSON string) []string {
	t.Helper()
	dir := t.TempDir()
	writeConfigFile(t, dir, "config.json", cfgJSON)
	log.SetOutput(&bytes.Buffer{}) // LoadConfig's own lines are checked below
	cfg, err := LoadConfig(dir)
	log.SetOutput(os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	cfg.warnGatedTileDefaults(func(f string, a ...interface{}) { got = append(got, fmt.Sprintf(f, a...)) })
	return got
}

func TestWarnGatedTileDefaults728(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
		want []string // one substring per expected line, in order
	}{
		{"esri dark default, no esri block",
			`{"map":{"tiles":{"darkDefault":"esri-darkgray-labels"}}}`,
			[]string{`map.tiles.darkDefault is "esri-darkgray-labels"`}},
		{"esri light default, esri disabled",
			`{"map":{"tiles":{"lightDefault":"esri-imagery","providers":{"esri":{"enabled":false}}}}}`,
			[]string{`map.tiles.lightDefault is "esri-imagery"`}},
		{"both defaults esri, esri block without enabled",
			`{"map":{"tiles":{"darkDefault":"esri-imagery-dark","lightDefault":"esri-imagery","providers":{"esri":{}}}}}`,
			[]string{`map.tiles.darkDefault is "esri-imagery-dark"`, `map.tiles.lightDefault is "esri-imagery"`}},
		{"string \"true\" is not the boolean the browser needs",
			`{"map":{"tiles":{"darkDefault":"esri-darkgray-labels","providers":{"esri":{"enabled":"true"}}}}}`,
			[]string{`map.tiles.darkDefault is "esri-darkgray-labels"`}},
		{"legacy top-level mapDarkTileProvider is migrated, then checked",
			`{"mapDarkTileProvider":"esri-darkgray-labels"}`,
			[]string{`map.tiles.darkDefault is "esri-darkgray-labels"`}},
		{"esri enabled",
			`{"map":{"tiles":{"darkDefault":"esri-darkgray-labels","lightDefault":"esri-imagery","providers":{"esri":{"enabled":true}}}}}`,
			nil},
		{"esri enabled, no esri default",
			`{"map":{"tiles":{"darkDefault":"osm-dark","providers":{"esri":{"enabled":true}}}}}`,
			nil},
		{"non-esri defaults, esri disabled",
			`{"map":{"tiles":{"darkDefault":"osm-dark","lightDefault":"opentopomap"}}}`,
			nil},
		{"no tiles block",
			`{"port":8080}`,
			nil},
		// The browser reads these keys exactly, so a case variant does not
		// switch Esri on there and must not silence the warning here.
		{"providers.Esri (wrong case) does not enable esri",
			`{"map":{"tiles":{"darkDefault":"esri-darkgray-labels","providers":{"Esri":{"enabled":true}}}}}`,
			[]string{`map.tiles.darkDefault is "esri-darkgray-labels"`}},
		{"esri.Enabled (wrong case) does not enable esri",
			`{"map":{"tiles":{"darkDefault":"esri-darkgray-labels","providers":{"esri":{"Enabled":true}}}}}`,
			[]string{`map.tiles.darkDefault is "esri-darkgray-labels"`}},
		{"ESRI.ENABLED (wrong case) does not enable esri",
			`{"map":{"tiles":{"darkDefault":"esri-darkgray-labels","providers":{"ESRI":{"ENABLED":true}}}}}`,
			[]string{`map.tiles.darkDefault is "esri-darkgray-labels"`}},
		{"DarkDefault (wrong case) is ignored by the browser, so not reported",
			`{"map":{"tiles":{"DarkDefault":"esri-darkgray-labels"}}}`,
			nil},
		// A malformed neighbour must not hide the warning.
		{"providers is not an object",
			`{"map":{"tiles":{"darkDefault":"esri-darkgray-labels","providers":"x"}}}`,
			[]string{`map.tiles.darkDefault is "esri-darkgray-labels"`}},
		{"providers.esri is not an object",
			`{"map":{"tiles":{"darkDefault":"esri-darkgray-labels","providers":{"esri":true}}}}`,
			[]string{`map.tiles.darkDefault is "esri-darkgray-labels"`}},
		{"lightDefault is not a string",
			`{"map":{"tiles":{"darkDefault":"esri-darkgray-labels","lightDefault":5}}}`,
			[]string{`map.tiles.darkDefault is "esri-darkgray-labels"`}},
		{"darkDefault is not a string",
			`{"map":{"tiles":{"darkDefault":5,"lightDefault":"esri-imagery"}}}`,
			[]string{`map.tiles.lightDefault is "esri-imagery"`}},
		{"tiles is not an object",
			`{"map":{"tiles":"esri-darkgray-labels"}}`,
			nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := esriTileWarnings(t, tc.cfg)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d warnings, want %d: %q", len(got), len(tc.want), got)
			}
			for i, w := range tc.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("warning %d = %q, want it to contain %q", i, got[i], w)
				}
				if !strings.Contains(got[i], "map.tiles.providers.esri.enabled") {
					t.Errorf("warning %d does not name the switch: %q", i, got[i])
				}
			}
		})
	}
}

// LoadConfig itself must run the check, not only expose it.
func TestLoadConfigWarnsGatedEsriDefault728(t *testing.T) {
	load := func(body string) string {
		dir := t.TempDir()
		writeConfigFile(t, dir, "config.json", body)
		var buf bytes.Buffer
		log.SetOutput(&buf)
		_, err := LoadConfig(dir)
		log.SetOutput(os.Stderr)
		if err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	if out := load(`{"map":{"tiles":{"darkDefault":"esri-darkgray-labels"}}}`); !strings.Contains(out, `map.tiles.darkDefault is "esri-darkgray-labels"`) {
		t.Fatalf("LoadConfig did not log the Esri opt-in warning; log was:\n%s", out)
	}
	if out := load(`{"map":{"tiles":{"darkDefault":"esri-darkgray-labels","providers":{"esri":{"enabled":true}}}}}`); strings.Contains(out, "#728") {
		t.Fatalf("warned although Esri is enabled:\n%s", out)
	}
	// The legacy key only reaches map.tiles.darkDefault through
	// migrateDeprecatedConfig, so this fails if LoadConfig runs the check
	// before the migration.
	if out := load(`{"mapDarkTileProvider":"esri-darkgray-labels"}`); !strings.Contains(out, `map.tiles.darkDefault is "esri-darkgray-labels"`) {
		t.Fatalf("LoadConfig did not warn about a legacy mapDarkTileProvider Esri default; log was:\n%s", out)
	}
}
