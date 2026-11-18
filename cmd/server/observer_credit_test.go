package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

type observerCreditCase struct {
	Name                  string `json:"name"`
	Role                  string `json:"role"`
	HeardAgoSec           *int64 `json:"heardAgoSec"`
	ObserverAgoSec        *int64 `json:"observerAgoSec"`
	ObserverIDUpper       bool   `json:"observerIdUpper"`
	ObserverIDTrim        int    `json:"observerIdTrim"`
	ObserverOnlineMinutes int    `json:"observerOnlineMinutes"`
	Status                string `json:"status"`
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
			ht := HealthThresholds{
				InfraDegradedHours: fx.Thresholds.InfraDegradedHours, InfraSilentHours: fx.Thresholds.InfraSilentHours,
				NodeDegradedHours: fx.Thresholds.NodeDegradedHours, NodeSilentHours: fx.Thresholds.NodeSilentHours,
				ObserverOnlineMinutes: fx.Thresholds.ObserverOnlineMinutes,
			}
			if c.ObserverOnlineMinutes > 0 {
				ht.ObserverOnlineMinutes = c.ObserverOnlineMinutes
			}
			obs := ObserverReports{}
			if c.ObserverAgoSec != nil {
				id := key[:64-c.ObserverIDTrim]
				if c.ObserverIDUpper {
					id = strings.ToUpper(id)
				}
				obs.add(id, ago(*c.ObserverAgoSec))
			}
			var heard sql.NullString
			if c.HeardAgoSec != nil {
				heard = validStr(ago(*c.HeardAgoSec))
			}
			got := classifyNodeHealth(c.Role, key, heard, now.UnixMilli(), ht, obs)
			want := map[string]string{"healthy": "active", "degraded": "degraded", "silent": "silent"}[c.Status]
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
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
	res, err := db.GetNetworkStatus(ht)
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

// Observers load once per computation, not once per node.
func TestLoadObserverReportsReadsRosterOnce(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	seedHealthObserver(t, db, strings.Repeat("AB", 32), time.Now().UTC().Format(time.RFC3339))
	obs, err := db.LoadObserverReports()
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 {
		t.Fatalf("got %d observers, want 1", len(obs))
	}
}
