package main

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

type observerCreditCase struct {
	Name                  string `json:"name"`
	Role                  string `json:"role"`
	HeardAgoSec           *int64 `json:"heardAgoSec"`
	ObserverAgoSec        *int64 `json:"observerAgoSec"`
	ObserverIDUpper       bool   `json:"observerIdUpper"`
	ObserverIDTrim        int    `json:"observerIdTrim"`
	ObserverOnlineMinutes int    `json:"observerOnlineMinutes"`
	ObserverRoster        string `json:"observerRoster"`
	Status                string `json:"status"`
}

// observerID is the id the case's observer row is stored under.
func (c observerCreditCase) observerID(key string) string {
	id := key[:64-c.ObserverIDTrim]
	if c.ObserverIDUpper {
		id = strings.ToUpper(id)
	}
	return id
}

// bucket maps the fixture's client status onto the server's bucket name.
func (c observerCreditCase) bucket() string {
	return map[string]string{"healthy": "active", "degraded": "degraded", "silent": "silent"}[c.Status]
}

func (fx observerCreditFixture) thresholds(c observerCreditCase) HealthThresholds {
	ht := HealthThresholds{
		InfraDegradedHours: fx.Thresholds.InfraDegradedHours, InfraSilentHours: fx.Thresholds.InfraSilentHours,
		NodeDegradedHours: fx.Thresholds.NodeDegradedHours, NodeSilentHours: fx.Thresholds.NodeSilentHours,
		ObserverOnlineMinutes: fx.Thresholds.ObserverOnlineMinutes,
	}
	if c.ObserverOnlineMinutes > 0 {
		ht.ObserverOnlineMinutes = c.ObserverOnlineMinutes
	}
	return ht
}

type observerCreditFixture struct {
	Thresholds struct {
		InfraDegradedHours    float64 `json:"infraDegradedHours"`
		InfraSilentHours      float64 `json:"infraSilentHours"`
		NodeDegradedHours     float64 `json:"nodeDegradedHours"`
		NodeSilentHours       float64 `json:"nodeSilentHours"`
		ObserverOnlineMinutes int     `json:"observerOnlineMinutes"`
	} `json:"thresholds"`
	Cases []observerCreditCase `json:"cases"`
}

func loadObserverCreditFixture(t *testing.T) observerCreditFixture {
	t.Helper()
	raw, err := os.ReadFile("../../test-fixtures/observer-credit-parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx observerCreditFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	return fx
}

// TestObserverCreditParityWithClient runs the same cases the client test
// (test-issue-918-observer-credit-parity.js) runs against public/roles.js.
func TestObserverCreditParityWithClient(t *testing.T) {
	fx := loadObserverCreditFixture(t)
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	key := strings.Repeat("ab", 32)
	ago := func(sec int64) string { return now.Add(-time.Duration(sec) * time.Second).Format(time.RFC3339) }
	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			obs := ObserverReports{}
			// Like the client, the helper only sees served observers; the
			// roster filter itself is covered by the endpoint test below.
			if c.ObserverAgoSec != nil && c.ObserverRoster == "" {
				obs.add(c.observerID(key), ago(*c.ObserverAgoSec))
			}
			var heard sql.NullString
			if c.HeardAgoSec != nil {
				heard = validStr(ago(*c.HeardAgoSec))
			}
			got := classifyNodeHealth(c.Role, key, heard, now.UnixMilli(), fx.thresholds(c), obs)
			if want := c.bucket(); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

// TestObserverCreditParityThroughEndpoints runs every shared case through the
// real server: the case's node and observer row go into a fresh database, then
//   - /api/observers (what the client indexes) serves the observer exactly
//     when the case has no observerRoster exclusion, and
//   - /api/nodes/network-status and /api/analytics/areas both put the node in
//     the case's bucket.
//
// With the JS test giving the client the same served roster, this pins that
// the server reads the client's roster, not every observers row (#921 F1),
// and that both handlers pass it on (#921 F2).
func TestObserverCreditParityThroughEndpoints(t *testing.T) {
	fx := loadObserverCreditFixture(t)
	key := strings.Repeat("ab", 32)
	f := func(v float64) *float64 { return &v }
	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			db := setupTestDB(t)
			defer db.Close()
			ht := fx.thresholds(c)
			cfg := &Config{
				Port:             3000,
				HealthThresholds: &ht,
				Areas:            map[string]AreaEntry{"ODE": {Label: "Odense by", LatMin: f(55.32), LatMax: f(55.45), LonMin: f(10.3), LonMax: f(10.5)}},
			}
			obsID := c.observerID(key)
			if c.ObserverRoster == "blacklisted" {
				cfg.ObserverBlacklist = []string{strings.ToLower(obsID)}
			}
			srv := NewServer(db, cfg, NewHub())
			router := mux.NewRouter()
			srv.RegisterRoutes(router)

			// Millisecond timestamps, taken just before the requests, keep
			// the 1 s margins of the boundary cases intact.
			now := time.Now().UTC()
			ago := func(sec int64) string {
				return now.Add(-time.Duration(sec) * time.Second).Format("2006-01-02T15:04:05.000Z07:00")
			}
			var heard interface{}
			if c.HeardAgoSec != nil {
				heard = ago(*c.HeardAgoSec)
			}
			if _, err := db.conn.Exec("INSERT INTO nodes (public_key, name, role, last_seen, lat, lon) VALUES (?, 'n', ?, ?, 55.40, 10.40)", key, c.Role, heard); err != nil {
				t.Fatal(err)
			}
			if c.ObserverAgoSec != nil {
				inactive := 0
				if c.ObserverRoster == "inactive" {
					inactive = 1
				}
				if _, err := db.conn.Exec("INSERT INTO observers (id, name, last_seen, inactive) VALUES (?, 'obs', ?, ?)", obsID, ago(*c.ObserverAgoSec), inactive); err != nil {
					t.Fatal(err)
				}
			}

			get := func(path string, v interface{}) {
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

			var roster ObserverListResponse
			get("/api/observers", &roster)
			wantServed := c.ObserverAgoSec != nil && c.ObserverRoster == ""
			if served := len(roster.Observers) == 1 && roster.Observers[0].ID == obsID; served != wantServed || len(roster.Observers) > 1 {
				t.Fatalf("/api/observers served %+v, want observer served=%v", roster.Observers, wantServed)
			}

			want := c.bucket()
			var status struct {
				Active   int `json:"active"`
				Degraded int `json:"degraded"`
				Silent   int `json:"silent"`
			}
			get("/api/nodes/network-status", &status)
			gotStatus := map[string]int{"active": status.Active, "degraded": status.Degraded, "silent": status.Silent}
			if gotStatus[want] != 1 || status.Active+status.Degraded+status.Silent != 1 {
				t.Errorf("network-status %+v, want the node in %q", status, want)
			}

			var areas AreaAnalyticsResponse
			get("/api/analytics/areas", &areas)
			if len(areas.Density) != 1 {
				t.Fatalf("areas density = %+v, want one ODE entry", areas.Density)
			}
			d := areas.Density[0]
			got := map[string]int{"active": d.Active, "degraded": d.Degraded, "silent": d.Silent}
			if d.Total != 1 || got[want] != 1 {
				t.Errorf("areas ODE = %+v, want the node in %q", d, want)
			}
		})
	}
}

func seedHealthNode(t *testing.T, db *DB, pk, role, lastSeen string) {
	t.Helper()
	if _, err := db.conn.Exec("INSERT INTO nodes (public_key, name, role, last_seen, lat, lon) VALUES (?, ?, ?, ?, 55.40, 10.40)", pk, pk[:6], role, lastSeen); err != nil {
		t.Fatal(err)
	}
}

func seedHealthObserver(t *testing.T, db *DB, id, lastSeen string) {
	t.Helper()
	if _, err := db.conn.Exec("INSERT INTO observers (id, name, last_seen) VALUES (?, ?, ?)", id, "obs", lastSeen); err != nil {
		t.Fatal(err)
	}
}

// A companion silent for 3 days that is also an observer reporting 5 minutes
// ago is Active in the network status (#918), like the client's #902.
func TestGetNetworkStatusCreditsReportingObserver(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	now := time.Now().UTC()
	stale := now.Add(-72 * time.Hour).Format(time.RFC3339)
	pkObs := strings.Repeat("cd", 32)
	pkPlain := strings.Repeat("ef", 32)
	seedHealthNode(t, db, pkObs, "companion", stale)
	seedHealthNode(t, db, pkPlain, "companion", stale)
	seedHealthObserver(t, db, strings.ToUpper(pkObs), now.Add(-5*time.Minute).Format(time.RFC3339))

	ht := HealthThresholds{InfraDegradedHours: 2, InfraSilentHours: 48, NodeDegradedHours: 1, NodeSilentHours: 24, ObserverOnlineMinutes: 60}
	res, err := db.GetNetworkStatus(ht, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res["active"] != 1 || res["silent"] != 1 || res["total"] != 2 {
		t.Fatalf("got active=%v silent=%v total=%v, want 1/1/2", res["active"], res["silent"], res["total"])
	}
}

// The same credit applies to every area a node belongs to.
func TestComputeAreaDensityCreditsReportingObserver(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	areas := map[string]AreaEntry{
		"ODE": {Label: "Odense by", LatMin: f(55.32), LatMax: f(55.45), LonMin: f(10.3), LonMax: f(10.5)},
		"DK":  {Label: "Danmark", LatMin: f(54.5), LatMax: f(57.8), LonMin: f(8.0), LonMax: f(15.2)},
	}
	ht := HealthThresholds{InfraDegradedHours: 2, InfraSilentHours: 48, NodeDegradedHours: 1, NodeSilentHours: 24, ObserverOnlineMinutes: 60}
	now := time.Now().UTC()
	pkObs := strings.Repeat("cd", 32)
	stale := validStr(now.Add(-72 * time.Hour).Format(time.RFC3339))
	nodes := []areaAnalyticsNode{
		{PublicKey: pkObs, Role: "repeater", LastSeen: stale, Lat: 55.40, Lon: 10.40},
		{PublicKey: strings.Repeat("ef", 32), Role: "repeater", LastSeen: stale, Lat: 55.41, Lon: 10.41},
	}
	obs := ObserverReports{}
	obs.add(strings.ToUpper(pkObs), now.Add(-5*time.Minute).Format(time.RFC3339))

	for _, d := range computeAreaDensity(nodes, areas, ht, obs) {
		if d.Total != 2 || d.Active != 1 || d.Silent != 1 {
			t.Errorf("%s = %+v, want Total=2 Active=1 Silent=1", d.AreaKey, d)
		}
	}
}

// The credit roster is the /api/observers roster: soft-deleted rows and
// blacklisted ids (any case) are not loaded (#921 F1).
func TestLoadObserverReportsUsesListedRoster(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	listed, gone, banned := strings.Repeat("AB", 32), strings.Repeat("CD", 32), strings.Repeat("EF", 32)
	for _, id := range []string{listed, gone, banned} {
		seedHealthObserver(t, db, id, now)
	}
	if _, err := db.conn.Exec("UPDATE observers SET inactive = 1 WHERE id = ?", gone); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{ObserverBlacklist: []string{strings.ToLower(banned)}}
	obs, err := db.LoadObserverReports(cfg.IsObserverBlacklisted)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := obs[strings.ToLower(listed)]; !ok || len(obs) != 1 {
		t.Fatalf("got %v, want only %s", obs, listed)
	}
	if all, _ := db.LoadObserverReports(nil); len(all) != 2 {
		t.Fatalf("nil blacklist: got %d observers, want 2 (inactive row still dropped)", len(all))
	}
}
