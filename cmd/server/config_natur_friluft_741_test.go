package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Issue #741 M2: Natur- og Friluftskort is one more Dataforsyningen style in
// public/map-tile-providers.js, on the Skærmkort gate and token (#805). The
// server's half of that gate (warnGatedTileDefaults, dropUngatedTileTokens,
// dropDHMTokenFromTiles) is keyed on the provider and on the style-id
// prefix, not on the styles, so M2 adds no server code. These tests pin
// that the new style really is covered by it.

const naturFriluftStyle = "dataforsyningen-natur-friluft"

// dataforsyningenStyleIDs returns the ids of the BASE_STYLES entries the
// browser registers under provider 'dataforsyningen'.
func dataforsyningenStyleIDs(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "public", "map-tile-providers.js"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`'([a-z0-9-]+)':\s*\{\s*provider:\s*'dataforsyningen'`)
	var ids []string
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		ids = append(ids, m[1])
	}
	sort.Strings(ids)
	return ids
}

// Every style the browser gates on map.tiles.providers.dataforsyningen,
// Natur included, carries the prefix the server's gate reads, so a default
// naming one while the gate is off is reported, and not while it is on.
func TestDataforsyningenStylesMatchServerGate741(t *testing.T) {
	ids := dataforsyningenStyleIDs(t)
	if i := sort.SearchStrings(ids, naturFriluftStyle); i >= len(ids) || ids[i] != naturFriluftStyle {
		t.Fatalf("map-tile-providers.js registers no %s style: %q", naturFriluftStyle, ids)
	}
	var gate *gatedTileProvider
	for i := range gatedTileProviders {
		if gatedTileProviders[i].key == "dataforsyningen" {
			gate = &gatedTileProviders[i]
		}
	}
	if gate == nil || !gate.needsToken {
		t.Fatalf("no token-gated dataforsyningen provider on the server: %+v", gatedTileProviders)
	}
	for _, id := range ids {
		if !strings.HasPrefix(id, gate.prefix) {
			t.Errorf("style %s does not start with the server's gate prefix %q", id, gate.prefix)
		}
		off := esriTileWarnings(t, `{"map":{"tiles":{"lightDefault":"`+id+`","providers":{"dataforsyningen":{"enabled":true}}}}}`)
		if len(off) != 1 || !strings.Contains(off[0], `"`+id+`"`) || !strings.Contains(off[0], "map.tiles.providers.dataforsyningen.token") {
			t.Errorf("%s as a gated-off default: want one warning naming it and the token switch, got %q", id, off)
		}
		on := esriTileWarnings(t, `{"map":{"tiles":{"lightDefault":"`+id+`","providers":{"dataforsyningen":{"enabled":true,"token":"t"}}}}}`)
		if len(on) != 0 {
			t.Errorf("%s as a default with the gate on: want no warning, got %q", id, on)
		}
	}
}

// The #805 config shapes, with Natur as the configured default: the DHM
// token never ships, the base-map token ships only with the gate on (the
// one token both maps use), and the server warns exactly when it does not.
func TestClientConfigNaturFriluftSharesTheBaseMapGate741(t *testing.T) {
	dhmBlock := `"dhm": {"enabled": true, "token": "` + dhmTestToken + `", "layer": "terrain"}, `
	def := `"lightDefault": "` + naturFriluftStyle + `", `
	block := func(body string) string { return `"tiles": {` + def + `"providers": {` + body + `}}` }
	cases := []struct {
		name     string
		mapJSON  string
		topLevel string
		on       bool
	}{
		{"on", dhmBlock + block(`"dataforsyningen": {"enabled": true, "token": "`+baseMapTestToken+`"}`), "", true},
		{"enabled false", dhmBlock + block(`"dataforsyningen": {"enabled": false, "token": "`+baseMapTestToken+`"}`), "", false},
		{"enabled is the string true", dhmBlock + block(`"dataforsyningen": {"enabled": "true", "token": "`+baseMapTestToken+`"}`), "", false},
		{"no token", dhmBlock + block(`"dataforsyningen": {"enabled": true}`), "", false},
		{"blank token", dhmBlock + block(`"dataforsyningen": {"enabled": true, "token": "  "}`), "", false},
		{"token is not a string", dhmBlock + block(`"dataforsyningen": {"enabled": true, "token": ["`+baseMapTestToken+`"]}`), "", false},
		{"providers.Dataforsyningen case variant", dhmBlock + block(`"Dataforsyningen": {"enabled": true, "token": "`+baseMapTestToken+`"}`), "", false},
		{"DATAFORSYNINGEN.TOKEN case variant", dhmBlock + block(`"DATAFORSYNINGEN": {"ENABLED": true, "TOKEN": "`+baseMapTestToken+`"}`), "", false},
		{"providers.dataforsyningen is not an object", dhmBlock + block(`"dataforsyningen": "`+baseMapTestToken+`"`), "", false},
		// The legacy top-level block counts only as far as it migrates into
		// map.tiles, which the browser reads.
		{"legacy top-level providers, migrated", dhmBlock + `"tiles": {` + strings.TrimSuffix(def, ", ") + `}`,
			`"tiles": {"providers": {"dataforsyningen": {"enabled": true, "token": "` + baseMapTestToken + `"}}}`, true},
		{"legacy top-level tiles, all migrated", strings.TrimSuffix(dhmBlock, ", "),
			`"tiles": {` + def + `"providers": {"dataforsyningen": {"enabled": true, "token": "` + baseMapTestToken + `"}}}`, true},
		{"legacy top-level providers, not migrated", dhmBlock + block(`"esri": {"enabled": true}`),
			`"tiles": {"providers": {"dataforsyningen": {"enabled": true, "token": "` + baseMapTestToken + `"}}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := `{"map": {` + tc.mapJSON + `}`
			if tc.topLevel != "" {
				doc += `, ` + tc.topLevel
			}
			doc += `}`
			body, _ := clientConfigFor(t, doc)
			if strings.Contains(body, dhmTestToken) {
				t.Fatalf("the DHM token is in /api/config/client: %s", body)
			}
			if got := strings.Contains(body, baseMapTestToken); got != tc.on {
				t.Fatalf("base-map token shipped = %v, want %v: %s", got, tc.on, body)
			}
			if !strings.Contains(body, `"lightDefault":"`+naturFriluftStyle+`"`) {
				t.Fatalf("the Natur default is not shipped as configured: %s", body)
			}
			warned := esriTileWarnings(t, doc)
			if tc.on != (len(warned) == 0) {
				t.Fatalf("gate on = %v, but warnings = %q", tc.on, warned)
			}
		})
	}
}

// The DHM token pasted as the base-map token stays home with Natur as the
// default too: the value is dropped, the DHM proxy keeps it.
func TestClientConfigNaturFriluftDropsReusedDHMToken741(t *testing.T) {
	body, cfg := clientConfigFor(t, `{"map": {"dhm": {"enabled": true, "token": "`+dhmTestToken+`"},
		"tiles": {"lightDefault": "`+naturFriluftStyle+`", "providers": {"dataforsyningen": {"enabled": true, "token": "`+dhmTestToken+`"}}}}}`)
	if strings.Contains(body, dhmTestToken) {
		t.Fatalf("the reused DHM token is in /api/config/client: %s", body)
	}
	if cfg.DHM.Token != dhmTestToken || !cfg.DHM.Active() {
		t.Fatalf("the DHM proxy must keep its own token: %+v", cfg.DHM)
	}
}
