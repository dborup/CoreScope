package main

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dhm"
)

// #734 M2 — follow-ups carried from the #774 review. Fake upstreams only.

// F2 (#774 review): under the default map.dhm.layer the terrain id is the
// #680 hillshade, and its admission refusal is logged as before #734, as a
// "hillshade tile", for the id and for the legacy alias alike. The surface
// and contour labels are pinned by TestDHM734_AdmissionFullSurfaceIsBusyAndRefunded.
func TestDHM734M2_TerrainRefusalKeepsTheHillshadeLabel(t *testing.T) {
	const client = "198.51.100.37:1000"
	up, release, _ := heldUpstream(t)
	logs := &dhmLogSink{}
	p := newTestDHMProxy(t, up, logs, func(o *dhmProxyOptions) {
		o.tileMaxInFlight = 1
		o.client.Timeout = 5 * time.Second
	})
	held := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, overlayRequest(overlayTile("surface", 16, 0), client))
		held <- w
	}()
	eventually744(t, "the surface tile upstream", func() bool { return up.hits.Load() == 1 })
	for i, layers := range []string{"terrain", terrainLayer} {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, overlayRequest(overlayTile(layers, 16, i+1), client))
		assertBusyTile(t, w)
	}
	close(release)
	assertMissTile(t, <-held)
	const want = "[dhm] hillshade tile refused: admission full, no upstream request\n"
	if l := logs.String(); strings.Count(l, want) != 2 || strings.Contains(l, "terrain") {
		t.Fatalf("terrain refusals logged as:\n%s", l)
	}
}

// #734 M2 budget, from test-issue-734-dhm-overlay-budget-e2e.js and the
// ceilings in tests/dhm-tile-budget.js (1920x1080, HTTP cache off,
// terrain + contours from z15 in Aarhus): a cold first view is 90 tiles
// (45 per overlay, measured); the normal session's ceiling is 660 tiles in
// 22.5 s at 2.5 s per view, 2288/min with the 1.3 headroom; the aggressive
// z15→z19→z15 session's ceiling is 1692 as one burst. These are geometry
// at a fixed pace, not wall-clock rates, so they hold on every runner
// (#798 review F1).
func TestDHM734M2_BudgetDefaultsFitTwoOverlays(t *testing.T) {
	const (
		coldView2        = 90   // tiles, both overlays
		normalNeedPerMin = 2288 // normal session ceiling at the user pace, x1.3
		aggressive       = 1692 // aggressive session ceiling, one burst
		// T9 on staging: the slowest cold DHM tile, z15-z19 (#734).
		maxTileLatency = 350 * time.Millisecond
	)
	if dhmTileMaxInFlight < coldView2 {
		t.Errorf("admission %d slots: a cold two-overlay view of %d tiles would be refused in part (busy holes)",
			dhmTileMaxInFlight, coldView2)
	}
	// An admitted tile waits for one of Concurrency upstream slots, and
	// Timeout bounds that wait plus the request (internal/dhm Client.fetch):
	// a full queue must drain inside it at the slowest measured latency.
	opts := dhm.DefaultClientOptions()
	if drain := time.Duration(dhmTileMaxInFlight/opts.Concurrency) * maxTileLatency; drain >= opts.Timeout {
		t.Errorf("admission %d / concurrency %d drains in %v at %v a tile: not inside the %v timeout",
			dhmTileMaxInFlight, opts.Concurrency, drain, maxTileLatency, opts.Timeout)
	}
	if opts.Concurrency != 4 {
		t.Errorf("upstream concurrency %d: #734 M2 keeps the fair-use 4", opts.Concurrency)
	}
	if dhmTileRateLimitDefault < normalNeedPerMin || dhmTileRateLimitDefault < aggressive {
		t.Errorf("per-client budget %d/min: under the normal two-overlay need (%d/min) or the aggressive burst (%d)",
			dhmTileRateLimitDefault, normalNeedPerMin, aggressive)
	}
	// No more than needed: the budget still holds a tile-walking script.
	if dhmTileRateLimitDefault > 2400 || dhmTileMaxInFlight > 96 {
		t.Errorf("budget %d/min, admission %d: raised beyond the measured need", dhmTileRateLimitDefault, dhmTileMaxInFlight)
	}
	if dhmTileRateLimitMax < 10*dhmTileRateLimitDefault {
		t.Errorf("tileRateLimitPerMin max %d no longer leaves room above the default %d", dhmTileRateLimitMax, dhmTileRateLimitDefault)
	}
}

// The OpenAPI 429 descriptions state the shipped default.
func TestDHM734M2_OpenAPIStatesTheTileBudget(t *testing.T) {
	want := "default " + strconv.Itoa(dhmTileRateLimitDefault) + "/min"
	for _, route := range []string{"GET /api/map/dhm/wms", "GET " + dhmOrthoRoute} {
		meta, ok := routeDescriptions()[route]
		if !ok {
			t.Fatalf("%s: not documented", route)
		}
		found := false
		for _, e := range meta.Errors {
			if e.Status == "429" && strings.Contains(e.Description, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no 429 description with %q", route, want)
		}
	}
}
