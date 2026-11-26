package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// nodePositionCase is one row of testdata/node-position-cases.json, the
// table public/antenna-claims.js hasPosition is tested against as well.
type nodePositionCase struct {
	Name       string   `json:"name"`
	Lat        *float64 `json:"lat"`
	Lon        *float64 `json:"lon"`
	Positioned bool     `json:"positioned"`
}

func loadNodePositionCases(t *testing.T) []nodePositionCase {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "node-position-cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []nodePositionCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) == 0 {
		t.Fatal("no cases")
	}
	return f.Cases
}

// #979: the node page asks /antenna and /station only when hasPosition
// says the server can place the node. This pins the server's side of that
// table: a positioned node is answered (200), every other one gets the
// #743 404, byte for byte the unknown key's; a bad key stays 400. The
// contract itself is unchanged by #979.
func TestNodePosition979_ClientGateMatchesServer(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, detailsOn(nil), nil)
	unknown := pk789("f")
	refStation := env.do("GET", "/api/nodes/"+unknown+"/station", "", "")
	refAntenna := env.do("GET", "/api/nodes/"+unknown+"/antenna", "", "")
	if refStation.Code != http.StatusNotFound || refAntenna.Code != http.StatusNotFound {
		t.Fatalf("unknown key: /station %d, /antenna %d", refStation.Code, refAntenna.Code)
	}
	for i, c := range loadNodePositionCases(t) {
		pk := fmt.Sprintf("%064x", 0x979000+i)
		var lat, lon any
		if c.Lat != nil {
			lat = *c.Lat
		}
		if c.Lon != nil {
			lon = *c.Lon
		}
		if _, err := env.srv.db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count)
			VALUES (?, ?, 'repeater', ?, ?, datetime('now'), datetime('now'), 1)`, pk, "Pos979", lat, lon); err != nil {
			t.Fatal(err)
		}
		env.srv.store.InvalidateNodeCache()
		for _, ep := range []struct {
			path string
			ref  string
		}{{"/station", refStation.Body.String()}, {"/antenna", refAntenna.Body.String()}} {
			w := env.do("GET", "/api/nodes/"+pk+ep.path, "", "")
			switch {
			case c.Positioned && w.Code != http.StatusOK:
				t.Errorf("%s %s: %d %s, want 200", c.Name, ep.path, w.Code, w.Body)
			case !c.Positioned && (w.Code != http.StatusNotFound || w.Body.String() != ep.ref):
				t.Errorf("%s %s: %d %q, want the unknown key's 404 %q", c.Name, ep.path, w.Code, w.Body, ep.ref)
			}
		}
	}
	for _, ep := range []string{"/station", "/antenna"} {
		if w := env.do("GET", "/api/nodes/not-a-key"+ep, "", ""); w.Code != http.StatusBadRequest {
			t.Errorf("bad key %s: %d", ep, w.Code)
		}
	}
}
