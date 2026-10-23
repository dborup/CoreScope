package main

import (
	"net/http/httptest"
	"testing"
)

// #523: Regions was trimmed back to the Latest view (#501). The history and
// routes endpoints and the snapshot work behind them were removed for their
// memory and recompute cost, so they must stay unregistered and undocumented.
var removedRegionsEndpoints = []string{"/api/regions/history", "/api/regions/routes"}

func TestRegionsTrimRemovedEndpointsReturn404(t *testing.T) {
	srv, _ := setupTestServer(t)
	router := productionStyleRouter(t, srv)
	for _, path := range removedRegionsEndpoints {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", path+"?window=24h", nil))
		if w.Code != 404 {
			t.Errorf("GET %s: want 404 after #523, got %d", path, w.Code)
		}
	}
	// The kept Latest endpoint is still served.
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/regions/observed?window=24h", nil))
	if w.Code == 404 {
		t.Fatalf("GET /api/regions/observed must remain registered, got 404")
	}
}

func TestRegionsTrimRemovedEndpointsNotInOpenAPI(t *testing.T) {
	descriptions := routeDescriptions()
	for _, path := range removedRegionsEndpoints {
		if _, ok := descriptions["GET "+path]; ok {
			t.Errorf("OpenAPI still documents GET %s after #523", path)
		}
	}
	if _, ok := descriptions["GET /api/regions/observed"]; !ok {
		t.Fatal("OpenAPI must keep documenting GET /api/regions/observed")
	}
}
