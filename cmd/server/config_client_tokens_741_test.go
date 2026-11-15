package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// Issue #741 M1b: the Dataforsyningen Skærmkort base maps are fetched by
// the browser with their own token, map.tiles.providers.dataforsyningen.token.
// That token is client-visible on purpose; map.dhm.token is not and must
// never reach /api/config/client. Both come from the same Dataforsyningen
// account, so these tests pin the separation for every shape a config can
// take, including the ones an operator reaches by mistake.

const baseMapTestToken = "basemap-741-client-visible"

// clientConfigFor loads cfgJSON the way the server does and returns the
// /api/config/client body.
func clientConfigFor(t *testing.T, cfgJSON string) (string, *Config) {
	t.Helper()
	dir := t.TempDir()
	writeConfigFile(t, dir, "config.json", cfgJSON)
	log.SetOutput(&bytes.Buffer{})
	cfg, err := LoadConfig(dir)
	log.SetOutput(os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	srv, router := setupTestServer(t)
	srv.cfg = cfg
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/config/client", nil))
	if w.Code != 200 {
		t.Fatalf("/api/config/client: %d %s", w.Code, w.Body.String())
	}
	return w.Body.String(), cfg
}

func TestClientConfigTokenSeparation741(t *testing.T) {
	dhmBlock := `"dhm": {"enabled": true, "token": "` + dhmTestToken + `", "layer": "terrain"}`
	base := `"dataforsyningen": {"enabled": true, "token": "` + baseMapTestToken + `"}`
	cases := []struct {
		name     string
		mapJSON  string // the body of "map": {…}
		topLevel string // extra top-level members, legacy keys
		wantBase bool   // the base-map token is shipped
	}{
		{"both tokens, documented keys", dhmBlock + `, "tiles": {"providers": {` + base + `}}`, "", true},
		{"base-map token only", `"tiles": {"providers": {` + base + `}}`, "", true},
		{"DHM token only", dhmBlock, "", false},
		// Case variants of map.dhm are removed, not shipped (liftMapBlock).
		{"map.DHM case variant", `"DHM": {"enabled": true, "token": "` + dhmTestToken + `"}, "tiles": {"providers": {` + base + `}}`, "", true},
		{"map.Dhm case variant next to map.dhm", dhmBlock + `, "Dhm": {"token": "` + dhmTestToken + `"}, "tiles": {"providers": {` + base + `}}`, "", true},
		// A case variant of the provider block is shipped as it is (the
		// browser ignores it), and it still never carries the DHM token.
		{"providers.Dataforsyningen case variant", dhmBlock + `, "tiles": {"providers": {"Dataforsyningen": {"enabled": true, "token": "` + baseMapTestToken + `"}}}`, "", true},
		{"dataforsyningen.Token case variant", dhmBlock + `, "tiles": {"providers": {"dataforsyningen": {"enabled": true, "Token": "` + baseMapTestToken + `"}}}`, "", true},
		{"DATAFORSYNINGEN.TOKEN case variant", dhmBlock + `, "tiles": {"providers": {"DATAFORSYNINGEN": {"ENABLED": true, "TOKEN": "` + baseMapTestToken + `"}}}`, "", true},
		// Malformed blocks.
		{"map.dhm is the token string itself", `"dhm": "` + dhmTestToken + `", "tiles": {"providers": {` + base + `}}`, "", true},
		{"map.dhm is an array", `"dhm": ["` + dhmTestToken + `"], "tiles": {"providers": {` + base + `}}`, "", true},
		{"map.dhm is a number", `"dhm": 5, "tiles": {"providers": {` + base + `}}`, "", true},
		{"providers is not an object", dhmBlock + `, "tiles": {"providers": "` + baseMapTestToken + `"}`, "", true},
		{"providers.dataforsyningen is not an object", dhmBlock + `, "tiles": {"providers": {"dataforsyningen": "` + baseMapTestToken + `"}}`, "", true},
		{"tiles is not an object", dhmBlock + `, "tiles": "` + baseMapTestToken + `"`, "", true},
		// The base-map token in the legacy top-level tiles block reaches the
		// client both as itself and through the map.tiles migration.
		{"legacy top-level tiles block", dhmBlock, `"tiles": {"providers": {` + base + `}}`, true},
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
			if got := strings.Contains(body, baseMapTestToken); got != tc.wantBase {
				t.Fatalf("base-map token shipped = %v, want %v: %s", got, tc.wantBase, body)
			}
		})
	}
}

// The browser reads the documented spelling only; the client config must
// carry the base-map token there, where map-tile-providers.js looks for it.
func TestClientConfigShipsBaseMapTokenWhereTheBrowserReadsIt741(t *testing.T) {
	body, _ := clientConfigFor(t, `{"map": {"dhm": {"enabled": true, "token": "`+dhmTestToken+`"},
		"tiles": {"providers": {"dataforsyningen": {"enabled": true, "token": "`+baseMapTestToken+`"}}}}}`)
	var got struct {
		Map struct {
			Tiles struct {
				Providers struct {
					Dataforsyningen struct {
						Enabled *bool   `json:"enabled"`
						Token   *string `json:"token"`
					} `json:"dataforsyningen"`
				} `json:"providers"`
			} `json:"tiles"`
		} `json:"map"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	d := got.Map.Tiles.Providers.Dataforsyningen
	if d.Enabled == nil || !*d.Enabled || d.Token == nil || *d.Token != baseMapTestToken {
		t.Fatalf("map.tiles.providers.dataforsyningen not shipped as configured: %s", body)
	}
}
