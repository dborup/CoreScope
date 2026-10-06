package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

func TestEstimatedPositionsStartupHelper(t *testing.T) {
	if os.Getenv("CORESCOPE_ESTIMATED_POSITIONS_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	os.Args = []string{"corescope-server", "-config-dir", os.Getenv("CORESCOPE_ESTIMATED_POSITIONS_TEST_CONFIG")}
	main()
}

func TestEstimatedPositionsInvalidStartup(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"estimatedPositions":{"enabled":"false"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestEstimatedPositionsStartupHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "CORESCOPE_ESTIMATED_POSITIONS_HELPER=1", "CORESCOPE_ESTIMATED_POSITIONS_TEST_CONFIG="+dir, "ENABLE_PPROF=false")
	out, err := cmd.CombinedOutput()
	if err == nil || ctx.Err() != nil {
		t.Fatalf("must reject startup promptly: %v, %s", err, out)
	}
	if !strings.Contains(string(out), "[config] fatal:") || !strings.Contains(string(out), "estimatedPositions.enabled must be a boolean") || strings.Contains(string(out), "panic:") {
		t.Fatalf("expected clear startup rejection, got %s", out)
	}
}

func disabledEstimatedPositionsConfig() *Config {
	enabled := false
	return &Config{EstimatedPositions: &EstimatedPositionsConfig{Enabled: &enabled}}
}

func TestEstimatedPositionsConfig(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      bool
		invalid   bool
	}{
		{"missing", `{}`, true, false},
		{"empty", `{"estimatedPositions":{}}`, true, false},
		{"true", `{"estimatedPositions":{"enabled":true}}`, true, false},
		{"false", `{"estimatedPositions":{"enabled":false}}`, false, false},
		{"string", `{"estimatedPositions":{"enabled":"false"}}`, false, true},
		{"number", `{"estimatedPositions":{"enabled":0}}`, false, true},
		{"null flag", `{"estimatedPositions":{"enabled":null}}`, false, true},
		{"null section", `{"estimatedPositions":null}`, false, true},
		{"wrong section", `{"estimatedPositions":false}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(tc.raw), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(dir)
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "estimatedPositions") {
					t.Fatalf("want explicit config error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			s := NewServer(nil, cfg, nil)
			if s.estimatedPositionsEnabled() != tc.want {
				t.Fatalf("enabled=%v want=%v", s.estimatedPositionsEnabled(), tc.want)
			}
			// Mutating the config after startup must not silently change policy.
			flip := !tc.want
			cfg.EstimatedPositions = &EstimatedPositionsConfig{Enabled: &flip}
			w := httptest.NewRecorder()
			s.handleConfigClient(w, httptest.NewRequest("GET", "/api/config/client", nil))
			var resp struct {
				EstimatedPositions struct {
					Enabled bool `json:"enabled"`
				} `json:"estimatedPositions"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.EstimatedPositions.Enabled != tc.want {
				t.Fatalf("client flag changed without restart: %s", w.Body.String())
			}
		})
	}
}

func TestEstimatedPositionsPathsSkipNeighborQueries(t *testing.T) {
	db := setupPacketPathCountingDB(t)
	defer db.Close()
	_, err := db.conn.Exec(`
		INSERT INTO nodes(public_key,name,role,lat,lon) VALUES ('anchor','GPS','repeater',55.5,9.5),('ghost','No GPS','repeater',NULL,NULL),('observer','Observer','repeater',NULL,NULL);
		INSERT INTO observers(id,name) VALUES ('observer','Observer');
		INSERT INTO neighbor_edges(node_a,node_b,count) VALUES ('anchor','ghost',10),('anchor','observer',10);
		INSERT INTO transmissions(id,raw_hex,hash,first_seen) VALUES(1,'AA','estimatepolicy','2026-01-01T00:00:00Z');
		INSERT INTO observations(transmission_id,observer_idx,path_json,resolved_path,timestamp) VALUES(1,1,'["aa","bb"]','["ghost","anchor"]',1736935200);`)
	if err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{true, false, true} {
		resetBulkTestQueryLog()
		single, err := db.getPacketPath("estimatepolicy", 50, enabled)
		if err != nil {
			t.Fatal(err)
		}
		bulk, err := db.getPacketPathsBulk([]string{"estimatepolicy"}, 50, enabled)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(single, bulk["estimatepolicy"]) {
			t.Fatal("single/bulk diverged")
		}
		b := single.Branches[0]
		if len(b.Points) != 2 || b.Points[0].PublicKey != "ghost" || b.Observer.PublicKey != "observer" {
			t.Fatal("route identities lost")
		}
		if b.Points[1].Lat == nil || *b.Points[1].Lat != 55.5 {
			t.Fatal("reported GPS lost")
		}
		if (b.Points[0].Lat != nil) != enabled || b.Points[0].Approx != enabled || (b.Observer.Lat != nil) != enabled {
			t.Fatalf("unexpected estimate policy %+v", b)
		}
		queries := 0
		for _, q := range bulkTestQueryLog() {
			if strings.Contains(q.sql, "neighbor_edges") {
				queries++
			}
		}
		if enabled && queries == 0 {
			t.Fatal("fixture failed to exercise estimator")
		}
		if !enabled && queries != 0 {
			t.Fatalf("disabled performed %d neighbor queries", queries)
		}
	}
}

func TestEstimatedPositionsNodePreservesGPS(t *testing.T) {
	on, _ := setupTestServer(t)
	_, err := on.db.conn.Exec(`INSERT INTO nodes(public_key,name,role,lat,lon) VALUES('policyghost','No GPS','repeater',NULL,NULL); INSERT INTO neighbor_edges(node_a,node_b,count) VALUES('policyghost','aabbccdd11223344',10)`)
	if err != nil {
		t.Fatal(err)
	}
	off := NewServer(on.db, disabledEstimatedPositionsConfig(), nil)
	for _, s := range []*Server{on, off, on} {
		for _, key := range []string{"policyghost", "aabbccdd11223344"} {
			w := httptest.NewRecorder()
			r := mux.SetURLVars(httptest.NewRequest("GET", "/api/nodes/"+key, nil), map[string]string{"pubkey": key})
			s.handleNodeDetail(w, r)
			if w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			var resp struct {
				Node map[string]interface{} `json:"node"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if key == "policyghost" && (resp.Node["estimated_lat"] != nil) != s.estimatedPositionsEnabled() {
				t.Fatalf("estimate policy wrong: %s", w.Body.String())
			}
			if key != "policyghost" && resp.Node["lat"] == nil {
				t.Fatal("real GPS removed")
			}
			if !s.estimatedPositionsEnabled() {
				for k := range resp.Node {
					if strings.HasPrefix(k, "estimated_") {
						t.Fatalf("leaked %s", k)
					}
				}
			}
		}
	}
}

func TestEstimatedPositionsAnalyticsDisabled(t *testing.T) {
	s := NewServer(nil, disabledEstimatedPositionsConfig(), nil)
	// A nil DB would panic if node detail's estimator wrapper did any work.
	if _, _, _, _, _, ok := s.estimateNodePosition("unpositioned"); ok {
		t.Fatal("disabled node estimate returned a position")
	}
	// A pre-existing cache must never bypass the disabled response.
	s.gpsSanityCache = &GPSSanityResponse{Evaluated: 1, Nodes: []SuspiciousGPSNode{{ClusterLat: 55, ClusterLon: 10}}}
	s.gpsSanityCachedAt = time.Now()
	w := httptest.NewRecorder()
	s.handleGPSSanity(w, httptest.NewRequest("GET", "/api/analytics/gps-sanity", nil))
	if strings.TrimSpace(w.Body.String()) != `{"estimatedPositionsEnabled":false}` {
		t.Fatalf("disabled GPS-sanity: %s", w.Body.String())
	}
	s.areaAnalyticsCache = &AreaAnalyticsResponse{EstimatedNodes: []EstimatedAreaNode{{Lat: 55, Lon: 10}}}
	s.areaAnalyticsCachedAt = time.Now()
	w = httptest.NewRecorder()
	s.handleAreaAnalytics(w, httptest.NewRequest("GET", "/api/analytics/areas", nil))
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["estimatedPositionsEnabled"] != false {
		t.Fatalf("missing disabled status: %s", w.Body.String())
	}
	for _, field := range []string{"estimatedNodes", "positionGaps", "unpositionedNoNeighborFix"} {
		if _, ok := resp[field]; ok {
			t.Fatalf("uncomputed %s emitted", field)
		}
	}
}

func TestEstimatedPositionsArchivedPathImmutable(t *testing.T) {
	on, _ := setupPingScoresFixture(t)
	snap := testPingScorePathArchive(t, on)
	archive := snap.pathArchives["allTime.farthestPing"]
	archive.Path.Branches[0].Points[0].Approx = true
	archive.Path.Branches[0].Points[0].ApproxNeighborCount = 2
	snap.pathArchives["allTime.farthestPing"] = archive
	before := mustJSON(t, snap.pathArchives)
	off := NewServer(on.db, disabledEstimatedPositionsConfig(), nil)
	off.pingScores.Store(snap)
	for _, s := range []*Server{off, on} {
		w := httptest.NewRecorder()
		r := mux.SetURLVars(httptest.NewRequest("GET", "/api/ping-scores/archive01/path?record=allTime.farthestPing", nil), map[string]string{"hash": "archive01"})
		s.handlePingScorePath(w, r)
		var resp PingScorePathResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || resp.Path == nil {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		point := resp.Path.Branches[0].Points[0]
		if point.PublicKey != "RELAY" || (point.Lat != nil) != s.estimatedPositionsEnabled() || point.Approx != s.estimatedPositionsEnabled() {
			t.Fatalf("bad archived point %+v", point)
		}
		if resp.Path.Branches[0].Observer.Lat == nil || resp.Path.Branches[0].DistanceFromFirstKm == nil {
			t.Fatal("real GPS distance removed")
		}
	}
	if mustJSON(t, snap.pathArchives) != before {
		t.Fatal("mutated shared archive")
	}
}

func TestEstimatedPositionsAreasSkipQueriesAndPreserveDensity(t *testing.T) {
	db := setupPacketPathCountingDB(t)
	defer db.Close()
	_, err := db.conn.Exec(`ALTER TABLE nodes ADD COLUMN last_seen TEXT;
		INSERT INTO nodes(public_key,name,role,lat,lon) VALUES ('anchor','GPS','repeater',55.5,9.5),('ghost','No GPS','repeater',NULL,NULL);
		INSERT INTO neighbor_edges(node_a,node_b,count) VALUES ('anchor','ghost',10);`)
	if err != nil {
		t.Fatal(err)
	}
	value := func(f float64) *float64 { return &f }
	areas := map[string]AreaEntry{"test": {Label: "Test area", LatMin: value(55), LatMax: value(56), LonMin: value(9), LonMax: value(10)}}
	for _, enabled := range []bool{true, false} {
		cfg := &Config{Areas: areas, EstimatedPositions: &EstimatedPositionsConfig{Enabled: &enabled}}
		s := NewServer(db, cfg, nil)
		resetBulkTestQueryLog()
		w := httptest.NewRecorder()
		s.handleAreaAnalytics(w, httptest.NewRequest("GET", "/api/analytics/areas", nil))
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		var resp struct {
			Density           []AreaDensity       `json:"density"`
			UnpositionedTotal int                 `json:"unpositionedTotal"`
			EstimatedNodes    []EstimatedAreaNode `json:"estimatedNodes"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Density) != 1 || resp.Density[0].Total != 1 || resp.UnpositionedTotal != 1 {
			t.Fatalf("real metrics lost: %s", w.Body.String())
		}
		if (len(resp.EstimatedNodes) > 0) != enabled {
			t.Fatalf("bad estimate policy: %s", w.Body.String())
		}
		queries := 0
		for _, q := range bulkTestQueryLog() {
			if strings.Contains(q.sql, "neighbor_edges") {
				queries++
			}
		}
		if enabled && queries == 0 {
			t.Fatal("on fixture not exercised")
		}
		if !enabled && queries != 0 {
			t.Fatal("disabled areas queried neighbor estimates")
		}
		resetBulkTestQueryLog()
		w = httptest.NewRecorder()
		s.handleAreaAnalytics(w, httptest.NewRequest("GET", "/api/analytics/areas", nil))
		if len(bulkTestQueryLog()) != 0 {
			t.Fatal("area cache lost")
		}
	}
}

func TestEstimatedPositionsArchivedApproxObserverDistance(t *testing.T) {
	on, _ := setupPingScoresFixture(t)
	snap := testPingScorePathArchive(t, on)
	archive := snap.pathArchives["allTime.farthestPing"]
	archive.Path.Branches[0].Observer.Approx = true
	archive.Path.Branches[0].Observer.ApproxNeighborCount = 2
	snap.pathArchives["allTime.farthestPing"] = archive
	before := mustJSON(t, snap.pathArchives)
	off := NewServer(on.db, disabledEstimatedPositionsConfig(), nil)
	off.pingScores.Store(snap)
	w := httptest.NewRecorder()
	r := mux.SetURLVars(httptest.NewRequest("GET", "/api/ping-scores/archive01/path?record=allTime.farthestPing", nil), map[string]string{"hash": "archive01"})
	off.handlePingScorePath(w, r)
	var resp PingScorePathResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || resp.Path == nil {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	branch := resp.Path.Branches[0]
	if branch.Observer.PublicKey != "PINGOBSB" || branch.Observer.Lat != nil || branch.Observer.Approx || branch.DistanceFromFirstKm != nil {
		t.Fatalf("approx endpoint leaked: %+v", branch)
	}
	if resp.Path.First.Observer.Lat == nil || branch.Points[0].Lat == nil {
		t.Fatal("real coordinates lost")
	}
	if mustJSON(t, snap.pathArchives) != before {
		t.Fatal("source archive mutated")
	}
}

// Ping metrics deliberately use only non-approximate observer endpoints;
// changing position policy must not change cached/history score records.
func TestEstimatedPositionsPingMetricsIndependent(t *testing.T) {
	s, _ := setupPingScoresFixture(t)
	txID := seedPingTrigger(t, s, "policyping", "#test", "Sender", "2026-01-15T10:00:00Z")
	seedPingObservation(t, s, txID, "pingobsa", 9, `[]`, `[]`, 1736935200)
	seedPingObservation(t, s, txID, "pingobsb", 9, `[]`, `[]`, 1736935201)
	_, err := s.db.conn.Exec(`INSERT INTO nodes(public_key,name,role,lat,lon) VALUES ('policyobserver','No GPS','repeater',NULL,NULL);
		INSERT INTO observers(id,name) VALUES ('policyobserver','No GPS');
		INSERT INTO neighbor_edges(node_a,node_b,count) VALUES ('policyobserver','pingobsa',10);`)
	if err != nil {
		t.Fatal(err)
	}
	// This ping already has real endpoints. Add an unpositioned
	// observer and relay to that same transmission, backed by a neighbor.
	triggers, err := s.db.fetchPingTriggers()
	if err != nil || len(triggers) == 0 {
		t.Fatalf("fixture triggers: %v", err)
	}
	trigger := triggers[0]
	_, err = s.db.conn.Exec(`INSERT INTO observations(transmission_id,observer_idx,path_json,resolved_path,timestamp)
		VALUES (?,(SELECT rowid FROM observers WHERE id='policyobserver'),'["aa"]','["policyobserver"]',1736935210)`, trigger.txID)
	if err != nil {
		t.Fatal(err)
	}
	var scores []*PingScore
	for _, enabled := range []bool{true, false} {
		path, err := s.db.getPacketPath(trigger.hash, 50, enabled)
		if err != nil {
			t.Fatal(err)
		}
		scores = append(scores, s.buildPingScoreFromPath(trigger, path))
		for _, b := range path.Branches {
			if b.Observer != nil && b.Observer.Approx && b.DistanceFromFirstKm != nil {
				t.Fatal("approx endpoint credited as GPS distance")
			}
		}
	}
	if mustJSON(t, scores[0]) != mustJSON(t, scores[1]) {
		t.Fatalf("position policy changed real Ping metrics: %s vs %s", mustJSON(t, scores[0]), mustJSON(t, scores[1]))
	}
}
