package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// /api/config/client must expose the configured geo_filter box/polygon so
// the frontend can classify domestic vs foreign nodes directly from
// lat/lon, rather than relying on the `foreign` flag alone — that flag
// only reflects nodes whose ADVERT was classified at ingest time, so a
// node whose last-known GPS predates geo_filter being configured (or just
// hasn't re-advertised since) can be geographically outside the filter
// without ever being flagged. See ClientConfigResponse.GeoFilter doc.
func TestConfigClientExposesGeoFilter(t *testing.T) {
	srv, router := setupTestServer(t)
	latMin, latMax, lonMin, lonMax := 53.0, 59.0, 6.0, 15.0
	srv.cfg.GeoFilter = &GeoFilterConfig{
		LatMin: &latMin, LatMax: &latMax, LonMin: &lonMin, LonMax: &lonMax,
	}

	req := httptest.NewRequest("GET", "/api/config/client", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	gf, present := body["geoFilter"].(map[string]interface{})
	if !present {
		t.Fatal("expected geoFilter in /api/config/client response when configured")
	}
	if gf["latMin"] != 53.0 || gf["latMax"] != 59.0 || gf["lonMin"] != 6.0 || gf["lonMax"] != 15.0 {
		t.Errorf("geoFilter bbox mismatch: got %+v", gf)
	}
}

// When no geo_filter is configured, the field must be omitted entirely
// (omitempty), not present-but-null — the frontend treats "field absent"
// as "no geo_filter configured, treat everything as domestic".
func TestConfigClientOmitsGeoFilterWhenUnconfigured(t *testing.T) {
	srv, router := setupTestServer(t)
	srv.cfg.GeoFilter = nil

	req := httptest.NewRequest("GET", "/api/config/client", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if _, present := body["geoFilter"]; present {
		t.Error("expected geoFilter to be omitted from /api/config/client when unconfigured")
	}
}
