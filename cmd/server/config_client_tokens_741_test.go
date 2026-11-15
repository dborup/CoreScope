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
		// The base-map token reaches the browser only while the browser
		// would use it: enabled is the JSON true and the token is non-blank,
		// both under exact-case keys, as map-tile-providers.js reads them.
		{"gate off: enabled false", `"tiles": {"providers": {"dataforsyningen": {"enabled": false, "token": "` + baseMapTestToken + `"}}}`, "", false},
		{"gate off: enabled is the string true", `"tiles": {"providers": {"dataforsyningen": {"enabled": "true", "token": "` + baseMapTestToken + `"}}}`, "", false},
		{"gate off: enabled is 1", `"tiles": {"providers": {"dataforsyningen": {"enabled": 1, "token": "` + baseMapTestToken + `"}}}`, "", false},
		{"gate off: enabled missing", `"tiles": {"providers": {"dataforsyningen": {"token": "` + baseMapTestToken + `"}}}`, "", false},
		{"gate off: token is not a string", `"tiles": {"providers": {"dataforsyningen": {"enabled": true, "token": ["` + baseMapTestToken + `"]}}}`, "", false},
		// A case variant of the provider block, or of its keys, is not read
		// by the browser, so the gate is off and its token stays home.
		{"providers.Dataforsyningen case variant", dhmBlock + `, "tiles": {"providers": {"Dataforsyningen": {"enabled": true, "token": "` + baseMapTestToken + `"}}}`, "", false},
		{"dataforsyningen.Token case variant", dhmBlock + `, "tiles": {"providers": {"dataforsyningen": {"enabled": true, "Token": "` + baseMapTestToken + `"}}}`, "", false},
		{"dataforsyningen.Enabled case variant", dhmBlock + `, "tiles": {"providers": {"dataforsyningen": {"Enabled": true, "token": "` + baseMapTestToken + `"}}}`, "", false},
		{"DATAFORSYNINGEN.TOKEN case variant", dhmBlock + `, "tiles": {"providers": {"DATAFORSYNINGEN": {"ENABLED": true, "TOKEN": "` + baseMapTestToken + `"}}}`, "", false},
		// Malformed map.dhm blocks leave the base-map gate on.
		{"map.dhm is the token string itself", `"dhm": "` + dhmTestToken + `", "tiles": {"providers": {` + base + `}}`, "", true},
		{"map.dhm is an array", `"dhm": ["` + dhmTestToken + `"], "tiles": {"providers": {` + base + `}}`, "", true},
		{"map.dhm is a number", `"dhm": 5, "tiles": {"providers": {` + base + `}}`, "", true},
		// Malformed tile blocks: the browser reads no gate from them.
		{"providers is not an object", dhmBlock + `, "tiles": {"providers": "` + baseMapTestToken + `"}`, "", false},
		{"providers.dataforsyningen is not an object", dhmBlock + `, "tiles": {"providers": {"dataforsyningen": "` + baseMapTestToken + `"}}`, "", false},
		{"tiles is not an object", dhmBlock + `, "tiles": "` + baseMapTestToken + `"`, "", false},
		// The base-map token in the legacy top-level tiles block reaches the
		// client both as itself and through the map.tiles migration, and
		// only with the gate on.
		{"legacy top-level tiles block", dhmBlock, `"tiles": {"providers": {` + base + `}}`, true},
		{"legacy top-level tiles block, gate off", dhmBlock, `"tiles": {"providers": {"dataforsyningen": {"enabled": false, "token": "` + baseMapTestToken + `"}}}`, false},
		{"legacy top-level tiles block, gate off, not migrated", dhmBlock + `, "tiles": {"providers": {"osm": {"enabled": false}}}`, `"tiles": {"providers": {"dataforsyningen": {"token": "` + baseMapTestToken + `"}}}`, false},
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

// Both tokens come from the same Dataforsyningen account, so the likeliest
// leak is the operator pasting the DHM token as the base-map token too.
// The client-visible map must then lose that value, never ship it.
func TestClientConfigDropsReusedDHMToken741(t *testing.T) {
	dhmBlock := `"dhm": {"enabled": true, "token": "` + dhmTestToken + `", "layer": "terrain"}`
	cases := []struct {
		name, doc string
	}{
		{"reused as the base-map token",
			`{"map": {` + dhmBlock + `, "tiles": {"providers": {"dataforsyningen": {"enabled": true, "token": "` + dhmTestToken + `"}}}}}`},
		{"reused with whitespace around it",
			`{"map": {` + dhmBlock + `, "tiles": {"providers": {"dataforsyningen": {"enabled": true, "token": "  ` + dhmTestToken + ` "}}}}}`},
		{"reused under a case variant of the key",
			`{"map": {` + dhmBlock + `, "tiles": {"providers": {"DATAFORSYNINGEN": {"TOKEN": "` + dhmTestToken + `"}}}}}`},
		{"reused as another provider's token",
			`{"map": {` + dhmBlock + `, "tiles": {"providers": {"stamen": {"enabled": true, "token": "` + dhmTestToken + `"}}}}}`},
		{"inside a legacy tile URL",
			`{"map": {` + dhmBlock + `, "tiles": {"darkUrl": "https://api.dataforsyningen.dk/x/{z}/{x}/{y}?token=` + dhmTestToken + `"}}}`},
		{"inside an array",
			`{"map": {` + dhmBlock + `, "tiles": {"notes": ["a", "` + dhmTestToken + `"]}}}`},
		{"in the legacy top-level tiles block",
			`{"map": {` + dhmBlock + `}, "tiles": {"providers": {"dataforsyningen": {"enabled": true, "token": "` + dhmTestToken + `"}}}}`},
		{"in a legacy top-level block next to map.tiles",
			`{"map": {` + dhmBlock + `, "tiles": {"darkDefault": "osm-dark"}}, "tiles": {"providers": {"dataforsyningen": {"token": "` + dhmTestToken + `"}}}}`},
		// map.tiles has its own providers, so the legacy block is not
		// migrated into it and is shipped on its own.
		{"in a legacy top-level block that is not migrated",
			`{"map": {` + dhmBlock + `, "tiles": {"providers": {"osm": {"enabled": false}}}}, "tiles": {"providers": {"dataforsyningen": {"token": "` + dhmTestToken + `"}}}}`},
		// The legacy top-level mapDarkTileProvider is shipped on its own as
		// well as migrated into map.tiles.darkDefault.
		{"in the legacy top-level mapDarkTileProvider",
			`{"map": {` + dhmBlock + `}, "mapDarkTileProvider": "` + dhmTestToken + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, cfg := clientConfigFor(t, tc.doc)
			if strings.Contains(body, dhmTestToken) {
				t.Fatalf("the reused DHM token is in /api/config/client: %s", body)
			}
			if cfg.DHM.Token != dhmTestToken || !cfg.DHM.Active() {
				t.Fatalf("the DHM proxy must keep its own token: %+v", cfg.DHM)
			}
		})
	}
	// The rest of the block is untouched.
	body, _ := clientConfigFor(t, cases[0].doc)
	if !strings.Contains(body, `"dataforsyningen":{"enabled":true}`) {
		t.Fatalf("only the reused value is dropped: %s", body)
	}
}

// The drop is logged, naming the key and never the value.
func TestClientConfigReusedDHMTokenIsLogged741(t *testing.T) {
	dir := t.TempDir()
	writeConfigFile(t, dir, "config.json", `{"map": {"dhm": {"enabled": true, "token": "`+dhmTestToken+`"},
		"tiles": {"providers": {"dataforsyningen": {"enabled": true, "token": "`+dhmTestToken+`"}}}}}`)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	_, err := LoadConfig(dir)
	log.SetOutput(os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "map.tiles.providers.dataforsyningen.token") || !strings.Contains(out, "map.dhm.token") {
		t.Fatalf("the drop is not logged with the key it removed:\n%s", out)
	}
	if strings.Contains(out, dhmTestToken) {
		t.Fatalf("the log line prints the token:\n%s", out)
	}
	buf.Reset()
	writeConfigFile(t, dir, "config.json", `{"map": {"dhm": {"enabled": true, "token": "`+dhmTestToken+`"},
		"tiles": {"providers": {"dataforsyningen": {"enabled": true, "token": "`+baseMapTestToken+`"}}}}}`)
	log.SetOutput(&buf)
	_, err = LoadConfig(dir)
	log.SetOutput(os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "map.dhm.token") {
		t.Fatalf("logged a reuse with two different tokens:\n%s", buf.String())
	}
}

// With the gate on, only the exact-case block keeps its token: a case
// variant next to it is dropped, and the on block is shipped untouched.
func TestClientConfigShipsOnlyTheGatedBaseMapToken741(t *testing.T) {
	const variantToken = "basemap-741-case-variant"
	body, _ := clientConfigFor(t, `{"map": {"tiles": {"providers": {
		"dataforsyningen": {"enabled": true, "token": "`+baseMapTestToken+`"},
		"Dataforsyningen": {"enabled": true, "token": "`+variantToken+`"}}}}}`)
	if !strings.Contains(body, `"dataforsyningen":{"enabled":true,"token":"`+baseMapTestToken+`"}`) {
		t.Fatalf("the gated block is not shipped as configured: %s", body)
	}
	if strings.Contains(body, variantToken) {
		t.Fatalf("a case-variant block's token is shipped: %s", body)
	}
}

// Dropping a gated-off token is logged by key, never by value, and the
// shipped example (enabled false, token "") drops nothing worth a line.
func TestClientConfigUngatedBaseMapTokenIsLogged741(t *testing.T) {
	load := func(doc string) string {
		dir := t.TempDir()
		writeConfigFile(t, dir, "config.json", doc)
		var buf bytes.Buffer
		log.SetOutput(&buf)
		_, err := LoadConfig(dir)
		log.SetOutput(os.Stderr)
		if err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	out := load(`{"map": {"tiles": {"providers": {"dataforsyningen": {"enabled": false, "token": "` + baseMapTestToken + `"}}}}}`)
	if !strings.Contains(out, "map.tiles.providers.dataforsyningen.token") {
		t.Fatalf("the drop is not logged with the key it removed:\n%s", out)
	}
	if strings.Contains(out, baseMapTestToken) {
		t.Fatalf("the log line prints the token:\n%s", out)
	}
	if out := load(`{"map": {"tiles": {"providers": {"dataforsyningen": {"enabled": false, "token": ""}}}}}`); strings.Contains(out, "dataforsyningen") {
		t.Fatalf("the shipped example logs a drop:\n%s", out)
	}
}
