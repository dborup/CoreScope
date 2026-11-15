package main

// #724 M5 — the operator side of the customizer values: map.masts.minZoom,
// map.masts.hintDistanceM and map.masts.hintStrongM, bounded like
// map.dhm.profile.rateLimitPerMin (configKnobs.integer), and carried in the
// client block so the customizer starts from the operator's values.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMastsClientSettingsDefaults(t *testing.T) {
	var logs safeLog
	got := resolveMastsClientSettings(MastsConfig{Enabled: true}, logs.printf)
	want := mastsClientSettings{minZoom: 13, hintDistanceM: 500, hintStrongM: 100}
	if got != want {
		t.Fatalf("defaults %+v, want %+v", got, want)
	}
	if logs.String() != "" {
		t.Fatalf("unset values logged: %q", logs.String())
	}
}

func TestMastsClientSettingsBounds(t *testing.T) {
	cases := []struct {
		name string
		cfg  MastsConfig
		want mastsClientSettings
		warn string // "" = no warning
	}{
		{"minZoom low edge", MastsConfig{MinZoom: intp(13)}, mastsClientSettings{13, 500, 100}, ""},
		{"minZoom high edge", MastsConfig{MinZoom: intp(18)}, mastsClientSettings{18, 500, 100}, ""},
		{"minZoom below the gate", MastsConfig{MinZoom: intp(12)}, mastsClientSettings{13, 500, 100}, "map.masts.minZoom"},
		{"minZoom above 18", MastsConfig{MinZoom: intp(19)}, mastsClientSettings{13, 500, 100}, "map.masts.minZoom"},
		{"distance low edge", MastsConfig{HintDistanceM: intp(100)}, mastsClientSettings{13, 100, 100}, ""},
		{"distance high edge", MastsConfig{HintDistanceM: intp(990)}, mastsClientSettings{13, 990, 100}, ""},
		{"distance too short", MastsConfig{HintDistanceM: intp(99)}, mastsClientSettings{13, 500, 100}, "map.masts.hintDistanceM"},
		{"distance past four cells", MastsConfig{HintDistanceM: intp(991)}, mastsClientSettings{13, 500, 100}, "map.masts.hintDistanceM"},
		{"distance negative", MastsConfig{HintDistanceM: intp(-500)}, mastsClientSettings{13, 500, 100}, "map.masts.hintDistanceM"},
		{"strong off", MastsConfig{HintStrongM: intp(0)}, mastsClientSettings{13, 500, 0}, ""},
		{"strong at distance", MastsConfig{HintStrongM: intp(500)}, mastsClientSettings{13, 500, 500}, ""},
		{"strong negative", MastsConfig{HintStrongM: intp(-1)}, mastsClientSettings{13, 500, 100}, "map.masts.hintStrongM"},
		{"strong past distance", MastsConfig{HintStrongM: intp(501)}, mastsClientSettings{13, 500, 100}, "map.masts.hintStrongM"},
		{"strong past a short distance", MastsConfig{HintDistanceM: intp(200), HintStrongM: intp(300)}, mastsClientSettings{13, 200, 100}, "map.masts.hintStrongM"},
		{"strong within a long distance", MastsConfig{HintDistanceM: intp(900), HintStrongM: intp(700)}, mastsClientSettings{13, 900, 700}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var logs safeLog
			got := resolveMastsClientSettings(c.cfg, logs.printf)
			if got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
			out := logs.String()
			if c.warn == "" && out != "" {
				t.Fatalf("unexpected warning %q", out)
			}
			if c.warn != "" && (!strings.Contains(out, c.warn) || !strings.Contains(out, "[masts]")) {
				t.Fatalf("warning %q does not name [masts] %s", out, c.warn)
			}
		})
	}
}

// The hint pins at most four 2 km cells per end (public/masts-layer.js
// PIN_CELL_CAP 8): the distance plus the browser's 10 m pad must stay
// within half a cell.
func TestMastsHintDistanceMaxFitsFourCells(t *testing.T) {
	const cellPadM = 10 // public/masts-hint.js CELL_PAD_M
	if 2*(mastsHintDistanceMax+cellPadM) > 2000 {
		t.Fatalf("hintDistanceM max %d + pad %d spans more than one 2 km cell", mastsHintDistanceMax, cellPadM)
	}
}

func TestMastsClientConfigCarriesSettings(t *testing.T) {
	srv, router := setupTestServer(t)
	cfg := writeMastsConfig(t, `{"map":{"masts":{"enabled":true,"minZoom":15,"hintDistanceM":800,"hintStrongM":50}}}`)
	srv.masts = newMastsProxyFromConfig(cfg)
	raw := clientMastsRaw(t, router)
	if raw == nil {
		t.Fatal("no masts block while enabled")
	}
	var c MastsClientConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if c.MinZoom != 15 || c.HintDistanceM != 800 || c.HintStrongM != 50 {
		t.Fatalf("client block %+v does not carry the operator's values", c)
	}
}

func TestMastsClientConfigOutOfRangeFallsBack(t *testing.T) {
	srv, router := setupTestServer(t)
	cfg := writeMastsConfig(t, `{"map":{"masts":{"enabled":true,"minZoom":3,"hintDistanceM":5000,"hintStrongM":-7}}}`)
	srv.masts = newMastsProxyFromConfig(cfg)
	var c MastsClientConfig
	if err := json.Unmarshal(clientMastsRaw(t, router), &c); err != nil {
		t.Fatal(err)
	}
	if c.MinZoom != mastsMinZoom || c.HintDistanceM != mastsHintDistanceDefault || c.HintStrongM != mastsHintStrongDefault {
		t.Fatalf("out-of-range config reached the client: %+v", c)
	}
}
