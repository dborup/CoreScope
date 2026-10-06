package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNodeDetail_NeighborAreaVisibility(t *testing.T) {
	for _, hide := range []string{"node-blacklist", "observer-blacklist", "observer-alias", "lookup-error"} {
		t.Run(hide, func(t *testing.T) {
			srv, router := setupTestServer(t)
			for _, q := range []string{
				`INSERT INTO nodes(public_key,name,role,lat,lon) VALUES ('target','Target','repeater',NULL,NULL),('a','A','repeater',55,9.4),('b','B','repeater',55.1,9.5),('c','C','repeater',55,9.6)`,
				`INSERT INTO neighbor_edges(node_a,node_b,count) VALUES ('target','a',60),('target','b',60),('target','c',60)`,
			} {
				if _, err := srv.db.conn.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			switch hide {
			case "node-blacklist":
				srv.cfg.NodeBlacklist = []string{"c"}
			case "observer-blacklist":
				srv.cfg.ObserverBlacklist = []string{"c"}
			case "observer-alias":
				srv.cfg.SetHiddenNamePrefixes([]string{"SECRET"})
				if _, err := srv.db.conn.Exec(`INSERT INTO observers(id,name) VALUES ('C','SECRET alias')`); err != nil {
					t.Fatal(err)
				}
			case "lookup-error":
				srv.cfg.SetHiddenNamePrefixes([]string{"SECRET"})
				if _, err := srv.db.conn.Exec(`DROP TABLE observers`); err != nil {
					t.Fatal(err)
				}
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("GET", "/api/nodes/target", nil))
			if w.Code != 200 {
				t.Fatalf("HTTP %d", w.Code)
			}
			var response struct {
				Node struct {
					Estimate NeighborPositionEstimate `json:"neighbor_estimate"`
				} `json:"node"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			e := response.Node.Estimate
			if hide == "lookup-error" {
				if e.Area != nil || e.Status != "unavailable" {
					t.Fatalf("lookup failure must abstain: %+v", e)
				}
				return
			}
			if e.Area == nil || e.Area.Kind != "line" || e.ContributorCount != 2 {
				t.Fatalf("hidden contributor not excluded: %+v", e)
			}
			for _, p := range e.Area.Vertices {
				if p.Lon == 9.6 {
					t.Fatal("hidden GPS exposed")
				}
			}
		})
	}
}

func TestNeighborEvidenceAreaGeometry(t *testing.T) {
	now := time.Now()
	cs := []neighborPositionCandidate{{Pubkey: "a", Lat: 55, Lon: 9, LastSeen: now}, {Pubkey: "b", Lat: 55.1, Lon: 9.1, LastSeen: now}, {Pubkey: "c", Lat: 55, Lon: 9.2, LastSeen: now}, {Pubkey: "remote", Lat: 48, Lon: 11, LastSeen: now.Add(-60 * 24 * time.Hour)}}
	r := estimateNeighborPosition(cs, 30, now)
	if r.Estimate.Area == nil || r.Estimate.Area.Kind != "polygon" || len(r.Estimate.Area.Vertices) != 3 {
		t.Fatalf("want supported triangle only: %+v", r.Estimate.Area)
	}
	for _, p := range r.Estimate.Area.Vertices {
		if p.Lat < 54 {
			t.Fatal("remote candidate leaked into area")
		}
	}
	r = estimateNeighborPosition(cs[:2], 30, now)
	if r.Estimate.Area.Kind != "line" {
		t.Fatal("two contributors must be a line, not invented area")
	}
	cs[1].Lat, cs[1].Lon = cs[0].Lat, cs[0].Lon
	r = estimateNeighborPosition(cs[:2], 30, now)
	if r.Estimate.Area != nil {
		t.Fatal("coincident contributors must not invent geometry")
	}
	r = estimateNeighborPosition(cs[:1], 30, now)
	if r.Estimate.Area != nil {
		t.Fatal("singleton must abstain")
	}
}

func TestNeighborEvidenceAreaAntimeridian(t *testing.T) {
	now := time.Now()
	r := estimateNeighborPosition([]neighborPositionCandidate{{Pubkey: "a", Lat: 10, Lon: 179.95, LastSeen: now}, {Pubkey: "b", Lat: 10.05, Lon: -179.95, LastSeen: now}, {Pubkey: "c", Lat: 10.1, Lon: 179.96, LastSeen: now}}, 30, now)
	if r.Estimate.Area == nil || r.Estimate.Area.Kind != "polygon" {
		t.Fatal("missing dateline polygon")
	}
	for _, p := range r.Estimate.Area.Vertices {
		if p.Lon < -180 || p.Lon > 180 || (p.Lon > -179 && p.Lon < 179) {
			t.Fatalf("invalid dateline vertex %+v", p)
		}
	}
}

func TestNeighborEvidenceAreaAbstentionsAndBound(t *testing.T) {
	now := time.Now()
	cs := []neighborPositionCandidate{{Pubkey: "a", Lat: 55, Lon: 9, LastSeen: now}, {Pubkey: "b", Lat: 55.1, Lon: 9.1, LastSeen: now}, {Pubkey: "c", Lat: 48, Lon: 11, LastSeen: now}, {Pubkey: "d", Lat: 48.1, Lon: 11.1, LastSeen: now}}
	if e := estimateNeighborPosition(cs, 30, now).Estimate; e.Status != "ambiguous" || e.Area != nil {
		t.Fatalf("ambiguous geometry leaked: %+v", e)
	}
	ps := make([]NeighborEvidenceVertex, 21)
	if neighborEvidenceArea(ps) != nil {
		t.Fatal("oversized geometry must abstain")
	}
	ps = []NeighborEvidenceVertex{{Lat: 55, Lon: 9}, {Lat: 55.1, Lon: 9.1}, {Lat: 55.2, Lon: 9.2}}
	if a := neighborEvidenceArea(ps); a == nil || a.Kind != "line" {
		t.Fatalf("collinear must have no fabricated area: %+v", a)
	}
}
