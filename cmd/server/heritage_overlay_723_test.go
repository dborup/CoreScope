package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gorilla/mux"
)

// #723 M1 — the protected ancient monuments overlay. The browser fetches
// the tiles straight from the Slots- og Kulturstyrelsen WMS (no token, open
// CORS), so the server's whole part is the opt-in `heritage` block of
// /api/config/client. Nothing here contacts the real service.

// clientHeritageRaw returns the raw `heritage` block of /api/config/client,
// or nil when the key is absent.
func clientHeritageRaw(t *testing.T, router *mux.Router) json.RawMessage {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/config/client", nil))
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body["heritage"]
}

func TestHeritageClientConfig_AbsentUnlessEnabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  HeritageConfig
	}{
		{"no map.heritage", HeritageConfig{}},
		{"enabled:false", HeritageConfig{Enabled: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, router := setupTestServer(t)
			srv.cfg.Heritage = tc.cfg
			if raw := clientHeritageRaw(t, router); raw != nil {
				t.Fatalf("heritage block offered while disabled: %s", raw)
			}
		})
	}
}

func TestHeritageClientConfig_EnabledBlock(t *testing.T) {
	srv, router := setupTestServer(t)
	srv.cfg.Heritage = HeritageConfig{Enabled: true}
	raw := clientHeritageRaw(t, router)
	if raw == nil {
		t.Fatal("no heritage block while map.heritage.enabled is true")
	}

	// Exactly these keys: the block carries what the overlay needs and
	// nothing an operator could put there.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(keys))
	for k := range keys {
		got = append(got, k)
	}
	sort.Strings(got)
	want := []string{"attribution", "bounds", "enabled", "layers", "maxZoom", "minZoom", "url"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("heritage block keys %v, want %v", got, want)
	}

	var b HeritageClientConfig
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	if !b.Enabled {
		t.Error("enabled is false")
	}
	// Both public map sites ultimately use this GeoServer; only the
	// /ffgeoserver/public/ prefix answers over HTTPS without a redirect to
	// http:// (#723 research, risks).
	if b.URL != "https://www.kulturarv.dk/ffgeoserver/public/wms" {
		t.Errorf("url %q", b.URL)
	}
	// Zones under the monument points, in one GetMap.
	if b.Layers != "fundogfortidsminder_areal_beskyttelse,fundogfortidsminder_punkt_fredet" {
		t.Errorf("layers %q", b.Layers)
	}
	// MaxScaleDenominator 250000: z8-z11 come back as the same blank PNG,
	// z12 is the first tile with content.
	if b.MinZoom != 12 {
		t.Errorf("minZoom %d, want 12", b.MinZoom)
	}
	if b.MaxZoom != 19 {
		t.Errorf("maxZoom %d, want 19 (the Map and Live maximum)", b.MaxZoom)
	}
	// Union of both layers' EX_GeographicBoundingBox, rounded outward.
	wantBounds := [2][2]float64{{54.4646, 8.0899}, {57.7289, 15.5689}}
	if b.Bounds != wantBounds {
		t.Errorf("bounds %v, want %v", b.Bounds, wantBounds)
	}
	if b.Attribution != "© Slots- og Kulturstyrelsen" {
		t.Errorf("attribution %q", b.Attribution)
	}
	// Terms of use, not a CC licence: no CC mark next to this credit.
	if strings.Contains(b.Attribution, "CC") {
		t.Errorf("attribution carries a CC mark: %q", b.Attribution)
	}
}

// The bounds must contain every advertised layer's own bounding box, or
// Leaflet would never request tiles at the edges of the data.
func TestHeritageBoundsCoverBothLayers(t *testing.T) {
	b := heritageClientConfig().Bounds
	// EX_GeographicBoundingBox from GetCapabilities (WMS 1.3.0, 2026-11-14).
	layers := map[string][4]float64{ // south, west, north, east
		"fundogfortidsminder_areal_beskyttelse": {54.464637446896354, 8.091679528115318, 57.58290466005352, 15.545393634081856},
		"fundogfortidsminder_punkt_fredet":      {54.46611008561668, 8.089944163493827, 57.72888334459091, 15.56885485731753},
	}
	for name, l := range layers {
		if b[0][0] > l[0] || b[0][1] > l[1] || b[1][0] < l[2] || b[1][1] < l[3] {
			t.Errorf("bounds %v do not cover %s %v", b, name, l)
		}
	}
}

func writeHeritageConfig(t *testing.T, cfgJSON string) *Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// map.heritage is lifted out of the free-form map block like map.dhm, so it
// reaches the browser only as the typed top-level block.
func TestHeritageConfig_LoadedAndStripped(t *testing.T) {
	cfg := writeHeritageConfig(t, `{"map":{"tiles":{"darkDefault":"osm-dark"},"heritage":{"enabled":true}}}`)
	if !cfg.Heritage.Enabled {
		t.Fatal("map.heritage.enabled not loaded")
	}
	if _, ok := cfg.Map["heritage"]; ok {
		t.Fatal("map.heritage left in the client-visible map block")
	}
	if _, ok := cfg.Map["tiles"]; !ok {
		t.Fatal("map.tiles lost while extracting heritage")
	}
	srv, router := setupTestServer(t)
	srv.cfg.Map = cfg.Map
	srv.cfg.Heritage = cfg.Heritage
	if clientHeritageRaw(t, router) == nil {
		t.Fatal("loaded map.heritage not offered")
	}
}

func TestHeritageConfig_MalformedIsOff(t *testing.T) {
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	cfg := writeHeritageConfig(t, `{"map":{"heritage":{"enabled":"yes"}}}`)
	log.SetOutput(os.Stderr)
	if cfg.Heritage.Enabled {
		t.Fatal("malformed map.heritage enabled the overlay")
	}
	if _, ok := cfg.Map["heritage"]; ok {
		t.Fatal("malformed map.heritage left in the map block")
	}
	if !strings.Contains(logBuf.String(), "map.heritage is malformed") {
		t.Fatalf("no malformed warning: %q", logBuf.String())
	}
}

// Same rule as map.dhm (#682 F1): only the documented spelling enables it,
// and a case variant is stripped with a warning.
func TestHeritageConfig_CaseVariantIsStrippedNotEnabled(t *testing.T) {
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	cfg := writeHeritageConfig(t, `{"map":{"Heritage":{"enabled":true}}}`)
	log.SetOutput(os.Stderr)
	if cfg.Heritage.Enabled {
		t.Fatal("map.Heritage enabled the overlay")
	}
	for k := range cfg.Map {
		if strings.EqualFold(k, "heritage") {
			t.Fatalf("map.%s left in the map block", k)
		}
	}
	if !strings.Contains(logBuf.String(), "map.Heritage") {
		t.Fatalf("no warning naming map.Heritage: %q", logBuf.String())
	}
}

// The shipped example documents map.heritage disabled.
func TestHeritageConfig_ExampleIsOffAndStripped(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Map map[string]json.RawMessage `json:"map"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Map["heritage"]; !ok {
		t.Fatal("config.example.json does not document map.heritage")
	}
	cfg := writeHeritageConfig(t, string(data))
	if cfg.Heritage.Enabled {
		t.Fatal("example config enables the heritage overlay")
	}
	if _, ok := cfg.Map["heritage"]; ok {
		t.Fatal("example map.heritage left in the client-visible map block")
	}
}
