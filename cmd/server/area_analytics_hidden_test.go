package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// Tests for #1005: /api/analytics/areas must apply the same identity rule as
// the node list (identityHidden) to every count and list it returns, and must
// not serve a cached result across a blacklist or hidden-prefix change.

const (
	hid1005Visible   = "aa00000000000000000000000000000000000000000000000000000000000001"
	hid1005Black     = "bb00000000000000000000000000000000000000000000000000000000000002"
	hid1005Prefix    = "cc00000000000000000000000000000000000000000000000000000000000003"
	hid1005ObsBlack  = "dd00000000000000000000000000000000000000000000000000000000000004"
	hid1005ObsName   = "ee00000000000000000000000000000000000000000000000000000000000005"
	hid1005VisibleB  = "aa00000000000000000000000000000000000000000000000000000000000006"
	hid1005Unpos     = "aa00000000000000000000000000000000000000000000000000000000000007"
	hid1005UnposHid  = "bb00000000000000000000000000000000000000000000000000000000000008"
	hid1005UnposOnly = "aa00000000000000000000000000000000000000000000000000000000000009"
)

func areaHiddenServer(t *testing.T, cfg *Config) (*Server, *mux.Router) {
	t.Helper()
	db := setupTestDB(t)
	srv := NewServer(db, cfg, NewHub())
	store := NewPacketStore(db, nil)
	if err := store.Load(); err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if !store.WaitIndexesReady(5 * time.Second) {
		t.Fatalf("background indexes never became ready")
	}
	store.config = cfg
	srv.store = store
	router := mux.NewRouter()
	srv.RegisterRoutes(router)
	return srv, router
}

func areaHiddenCfg() *Config {
	f := func(v float64) *float64 { return &v }
	cfg := &Config{Port: 3000}
	cfg.Areas = map[string]AreaEntry{
		"ODE": {Label: "Odense by", LatMin: f(55.32), LatMax: f(55.45), LonMin: f(10.3), LonMax: f(10.5)},
		"AAR": {Label: "Aarhus", LatMin: f(56.10), LatMax: f(56.20), LonMin: f(10.1), LonMax: f(10.3)},
	}
	return cfg
}

func areaHiddenExec(t *testing.T, s *Server, q string, args ...interface{}) {
	t.Helper()
	if _, err := s.db.conn.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func areaHiddenNode(t *testing.T, s *Server, pk, name string, lat, lon interface{}) {
	t.Helper()
	areaHiddenExec(t, s, "INSERT INTO nodes (public_key, name, role, last_seen, lat, lon) VALUES (?, ?, 'repeater', ?, ?, ?)",
		pk, name, time.Now().UTC().Format(time.RFC3339), lat, lon)
}

func areaHiddenGet(t *testing.T, router *mux.Router, path string, v interface{}) {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	if w.Code != 200 {
		t.Fatalf("%s: status=%d body=%s", path, w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func areaDensityByKey(resp AreaAnalyticsResponse) map[string]AreaDensity {
	out := make(map[string]AreaDensity, len(resp.Density))
	for _, d := range resp.Density {
		out[d.AreaKey] = d
	}
	return out
}

// TestAreaAnalytics1005_DensityMatchesNodeList is the #1002 §7.1 reproduction:
// one visible, one blacklisted and one hidden-prefix node in one area. The
// density total must equal /api/nodes?area= total for every area.
func TestAreaAnalytics1005_DensityMatchesNodeList(t *testing.T) {
	cfg := areaHiddenCfg()
	cfg.NodeBlacklist = []string{hid1005Black}
	cfg.HiddenNamePrefixes = []string{"🚫"}
	s, router := areaHiddenServer(t, cfg)
	areaHiddenNode(t, s, hid1005Visible, "Visible", 55.40, 10.40)
	areaHiddenNode(t, s, hid1005Black, "Blacklisted", 55.41, 10.41)
	areaHiddenNode(t, s, hid1005Prefix, "🚫Hidden", 55.42, 10.42)
	areaHiddenNode(t, s, hid1005VisibleB, "VisibleB", 56.15, 10.20)

	var resp AreaAnalyticsResponse
	areaHiddenGet(t, router, "/api/analytics/areas", &resp)
	density := areaDensityByKey(resp)
	for _, key := range []string{"ODE", "AAR"} {
		var nodes NodeListResponse
		areaHiddenGet(t, router, "/api/nodes?area="+key, &nodes)
		if density[key].Total != nodes.Total {
			t.Errorf("area %s: density total %d, /api/nodes total %d", key, density[key].Total, nodes.Total)
		}
		if density[key].Total != 1 {
			t.Errorf("area %s: density total %d, want 1", key, density[key].Total)
		}
	}
	if got := density["ODE"].RoleCounts["repeater"]; got != 1 {
		t.Errorf("ODE repeater role count = %d, want 1", got)
	}
	if got := density["ODE"].Active; got != 1 {
		t.Errorf("ODE active = %d, want 1", got)
	}
}

// TestAreaAnalytics1005_ObserverSideFollowsNodeList pins what already worked:
// a node whose key is observer-blacklisted, or whose observer row has a
// hidden name, is listed by /api/nodes, so the Areas tab counts it too (the
// tab must not show fewer or more nodes than the node list). The observer
// blacklist still withholds observer credit (TestObserverCreditParity...).
func TestAreaAnalytics1005_ObserverSideFollowsNodeList(t *testing.T) {
	cfg := areaHiddenCfg()
	cfg.ObserverBlacklist = []string{hid1005ObsBlack}
	cfg.HiddenNamePrefixes = []string{"🚫"}
	s, router := areaHiddenServer(t, cfg)
	now := time.Now().UTC().Format(time.RFC3339)
	areaHiddenNode(t, s, hid1005Visible, "Visible", 55.40, 10.40)
	areaHiddenNode(t, s, hid1005ObsBlack, "ObsBlack", 55.41, 10.41)
	areaHiddenNode(t, s, hid1005ObsName, "PlainNodeName", 55.42, 10.42)
	areaHiddenExec(t, s, "INSERT INTO observers (id, name, last_seen) VALUES (?, ?, ?)", hid1005ObsName, "🚫Observer", now)

	var resp AreaAnalyticsResponse
	areaHiddenGet(t, router, "/api/analytics/areas", &resp)
	var nodes NodeListResponse
	areaHiddenGet(t, router, "/api/nodes?area=ODE", &nodes)
	if d := areaDensityByKey(resp)["ODE"]; d.Total != nodes.Total || d.Total != 3 {
		t.Errorf("ODE density total %d, /api/nodes total %d, want both 3", d.Total, nodes.Total)
	}
}

// TestAreaAnalytics1005_BridgeNodesSkipHidden: a hidden node is never listed
// as a bridge node, and its edges do not credit the visible node it links to.
func TestAreaAnalytics1005_BridgeNodesSkipHidden(t *testing.T) {
	cfg := areaHiddenCfg()
	cfg.NodeBlacklist = []string{hid1005Black}
	cfg.HiddenNamePrefixes = []string{"🚫"}
	s, router := areaHiddenServer(t, cfg)
	areaHiddenNode(t, s, hid1005Visible, "Visible", 55.40, 10.40)   // ODE
	areaHiddenNode(t, s, hid1005VisibleB, "VisibleB", 56.15, 10.20) // AAR
	areaHiddenNode(t, s, hid1005Black, "Blacklisted", 56.16, 10.21) // AAR
	areaHiddenNode(t, s, hid1005Prefix, "🚫Hidden", 56.17, 10.22)    // AAR

	g := NewNeighborGraph()
	now := time.Now()
	snr := 5.0
	g.upsertEdge(hid1005Visible, hid1005VisibleB, "aa", "obs", &snr, now)
	g.upsertEdge(hid1005Visible, hid1005Black, "bb", "obs", &snr, now)
	g.upsertEdge(hid1005Visible, hid1005Prefix, "cc", "obs", &snr, now)
	s.store.graph.Store(g)

	var resp AreaAnalyticsResponse
	areaHiddenGet(t, router, "/api/analytics/areas", &resp)
	byPK := map[string]AreaBridgeNode{}
	for _, b := range resp.BridgeNodes {
		byPK[b.PublicKey] = b
	}
	for _, pk := range []string{hid1005Black, hid1005Prefix} {
		if _, ok := byPK[pk]; ok {
			t.Errorf("hidden node %s listed as a bridge node: %+v", pk, resp.BridgeNodes)
		}
	}
	v, ok := byPK[hid1005Visible]
	if !ok {
		t.Fatalf("visible bridge node missing: %+v", resp.BridgeNodes)
	}
	if v.EdgeCount != 1 || v.OtherAreaCount != 1 {
		t.Errorf("visible bridge = %+v, want EdgeCount=1 OtherAreaCount=1 (hidden edges must not count)", v)
	}
	if _, ok := byPK[hid1005VisibleB]; !ok {
		t.Errorf("visible peer missing from bridge nodes: %+v", resp.BridgeNodes)
	}
}

// TestAreaAnalytics1005_PositionGapsSkipHidden: a hidden unpositioned node is
// not estimated, listed or counted, and a hidden positioned neighbour does
// not contribute to a visible node's estimate (it would leak its position).
func TestAreaAnalytics1005_PositionGapsSkipHidden(t *testing.T) {
	cfg := areaHiddenCfg()
	cfg.NodeBlacklist = []string{hid1005Black, hid1005UnposHid}
	s, router := areaHiddenServer(t, cfg)
	areaHiddenNode(t, s, hid1005Visible, "Visible", 55.40, 10.40)   // ODE, real fix
	areaHiddenNode(t, s, hid1005Black, "Blacklisted", 55.43, 10.45) // ODE, hidden real fix
	areaHiddenNode(t, s, hid1005Unpos, "Unpos", nil, nil)
	areaHiddenNode(t, s, hid1005UnposHid, "UnposHidden", nil, nil)
	areaHiddenNode(t, s, hid1005UnposOnly, "UnposOnlyHiddenNeighbour", nil, nil)
	areaHiddenExec(t, s, "INSERT INTO neighbor_edges (node_a, node_b, count) VALUES (?, ?, 5)", hid1005Unpos, hid1005Visible)
	areaHiddenExec(t, s, "INSERT INTO neighbor_edges (node_a, node_b, count) VALUES (?, ?, 50)", hid1005Unpos, hid1005Black)
	areaHiddenExec(t, s, "INSERT INTO neighbor_edges (node_a, node_b, count) VALUES (?, ?, 5)", hid1005UnposHid, hid1005Visible)
	areaHiddenExec(t, s, "INSERT INTO neighbor_edges (node_a, node_b, count) VALUES (?, ?, 5)", hid1005UnposOnly, hid1005Black)

	var resp AreaAnalyticsResponse
	areaHiddenGet(t, router, "/api/analytics/areas", &resp)
	if resp.UnpositionedTotal != 2 {
		t.Errorf("unpositionedTotal = %d, want 2 (hidden unpositioned node left out)", resp.UnpositionedTotal)
	}
	if resp.UnpositionedNoNeighborFix != 1 {
		t.Errorf("unpositionedNoNeighborFix = %d, want 1 (the node whose only neighbour is hidden)", resp.UnpositionedNoNeighborFix)
	}
	if len(resp.PositionGaps) != 1 || resp.PositionGaps[0].RealFix != 1 || resp.PositionGaps[0].Approximated != 1 {
		t.Errorf("positionGaps = %+v, want ODE RealFix=1 Approximated=1", resp.PositionGaps)
	}
	if len(resp.EstimatedNodes) != 1 {
		t.Fatalf("estimatedNodes = %+v, want only the visible unpositioned node", resp.EstimatedNodes)
	}
	en := resp.EstimatedNodes[0]
	if en.PublicKey != hid1005Unpos {
		t.Errorf("estimatedNodes[0] = %+v, want %s", en, hid1005Unpos)
	}
	if en.ContributorCount != 1 || en.Lat != 55.40 || en.Lon != 10.40 {
		t.Errorf("estimate = %+v, want only the visible neighbour (55.40/10.40, 1 contributor)", en)
	}
}

// TestAreaAnalytics1005_CacheFollowsVisibilityChange: within the 30 s TTL a
// blacklist change or a hidden-prefix change must still take effect on the
// very next request.
func TestAreaAnalytics1005_CacheFollowsVisibilityChange(t *testing.T) {
	s, router := areaHiddenServer(t, areaHiddenCfg())
	areaHiddenNode(t, s, hid1005Visible, "Visible", 55.40, 10.40)
	areaHiddenNode(t, s, hid1005Black, "Blacklisted", 55.41, 10.41)
	areaHiddenNode(t, s, hid1005Prefix, "🚫Hidden", 55.42, 10.42)

	total := func() int {
		var resp AreaAnalyticsResponse
		areaHiddenGet(t, router, "/api/analytics/areas", &resp)
		return areaDensityByKey(resp)["ODE"].Total
	}
	if got := total(); got != 3 {
		t.Fatalf("before any hiding: total %d, want 3", got)
	}
	s.cfg.SetNodeBlacklist([]string{hid1005Black})
	if got := total(); got != 2 {
		t.Errorf("after blacklist change: total %d, want 2 (stale cache served)", got)
	}
	s.cfg.SetHiddenNamePrefixes([]string{"🚫"})
	if got := total(); got != 1 {
		t.Errorf("after hidden-prefix change: total %d, want 1 (stale cache served)", got)
	}
	s.cfg.SetNodeBlacklist(nil)
	if got := total(); got != 2 {
		t.Errorf("after un-blacklisting: total %d, want 2", got)
	}
}

// TestAreaAnalytics1005_RegressionAlreadyWorking pins behaviour that was right
// before #1005 and must stay so: with nothing hidden every node counts, nodes
// outside every area count nowhere, an unchanged config is served from the
// 30 s cache.
func TestAreaAnalytics1005_RegressionAlreadyWorking(t *testing.T) {
	s, router := areaHiddenServer(t, areaHiddenCfg())
	areaHiddenNode(t, s, hid1005Visible, "Visible", 55.40, 10.40)
	areaHiddenNode(t, s, hid1005Prefix, "🚫NotHiddenWithoutPrefixConfig", 55.42, 10.42)
	areaHiddenNode(t, s, hid1005VisibleB, "Outside", 40.0, -3.0)
	// Move the generations off zero so a cache that forgets to record them
	// (zero value) is caught as a miss below.
	s.cfg.SetNodeBlacklist(nil)
	s.cfg.SetHiddenNamePrefixes(nil)

	var resp AreaAnalyticsResponse
	areaHiddenGet(t, router, "/api/analytics/areas", &resp)
	density := areaDensityByKey(resp)
	if len(density) != 1 || density["ODE"].Total != 2 {
		t.Fatalf("density = %+v, want ODE total 2 only", resp.Density)
	}

	// Cache hit: a node added within the TTL with no visibility change is not
	// seen yet (the cache is still used, not bypassed on every request).
	areaHiddenNode(t, s, hid1005Black, "Late", 55.41, 10.41)
	areaHiddenGet(t, router, "/api/analytics/areas", &resp)
	if got := areaDensityByKey(resp)["ODE"].Total; got != 2 {
		t.Errorf("unchanged config within TTL: total %d, want cached 2", got)
	}
}
