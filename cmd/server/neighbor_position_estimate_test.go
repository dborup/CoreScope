package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

// A heavily observed remote edge must not determine the reference location
// when two other positioned neighbors agree with each other.
func TestNeighborPositionEstimate_RemoteTrafficDoesNotAnchor(t *testing.T) {
	for _, remoteCount := range []int{100, 10000} {
		t.Run(fmt.Sprint(remoteCount), func(t *testing.T) {
			db := setupPacketPathCountingDB(t)
			defer db.Close()
			for _, q := range []string{
				`INSERT INTO nodes VALUES ('local1', 'Local1', 'repeater', 55.00, 9.40)`,
				`INSERT INTO nodes VALUES ('local2', 'Local2', 'repeater', 55.05, 9.45)`,
				`INSERT INTO nodes VALUES ('remote', 'Remote', 'repeater', 48.00, 11.50)`,
				`INSERT INTO neighbor_edges (node_a,node_b,count) VALUES ('target','local1',60), ('target','local2',60)`,
			} {
				if _, err := db.conn.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.conn.Exec(`INSERT INTO neighbor_edges (node_a,node_b,count) VALUES ('target','remote',?)`, remoteCount); err != nil {
				t.Fatal(err)
			}
			_, lat, lon, count, _, ok := db.nearestPositionedNeighbor("target", EstimateMaxEdgeKm)
			if !ok || count != 2 || haversineKm(lat, lon, 55.025, 9.425) > 1 {
				t.Fatalf("remote count %d: got ok=%v, contributors=%d, location=(%v,%v); want coherent local pair", remoteCount, ok, count, lat, lon)
			}
		})
	}
}

func TestNeighborPositionEstimate_EvidenceAndFreshness(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	near := []neighborPositionCandidate{
		{Pubkey: "a", Lat: 55, Lon: 9.4, Count: 60, LastSeen: now},
		{Pubkey: "b", Lat: 55.05, Lon: 9.45, Count: 60, LastSeen: now.Add(-time.Hour)},
	}
	t.Run("competing clusters abstain", func(t *testing.T) {
		cs := append(append([]neighborPositionCandidate(nil), near...),
			neighborPositionCandidate{Pubkey: "c", Lat: 48, Lon: 11.5, Count: 10000, LastSeen: now},
			neighborPositionCandidate{Pubkey: "d", Lat: 48.05, Lon: 11.55, Count: 10000, LastSeen: now})
		r := estimateNeighborPosition(cs, 30, now)
		if r.Estimate.Status != "ambiguous" || r.LegacyOK || r.Estimate.Lat != nil {
			t.Fatalf("must abstain, got %+v", r)
		}
	})
	t.Run("old traffic loses to fresh cluster", func(t *testing.T) {
		cs := append(append([]neighborPositionCandidate(nil), near...),
			neighborPositionCandidate{Pubkey: "c", Lat: 48, Lon: 11.5, Count: 10000, LastSeen: now.Add(-30 * 24 * time.Hour)},
			neighborPositionCandidate{Pubkey: "d", Lat: 48.05, Lon: 11.55, Count: 10000, LastSeen: now.Add(-30 * 24 * time.Hour)})
		r := estimateNeighborPosition(cs, 30, now)
		if r.Estimate.Status != "estimated" || r.Estimate.ContributorCount != 2 || *r.Estimate.Lat < 54 {
			t.Fatalf("want recent pair, got %+v", r)
		}
		if r.Estimate.NewestSeen != now.Format(time.RFC3339) || r.Estimate.OldestSeen != now.Add(-time.Hour).Format(time.RFC3339) {
			t.Fatalf("freshness bounds: %+v", r.Estimate)
		}
	})
	t.Run("single is only a legacy proxy", func(t *testing.T) {
		r := estimateNeighborPosition(near[:1], 30, now)
		if r.Estimate.Status != "insufficient" || r.Estimate.Lat != nil || r.Estimate.ContributorCount != 1 || !r.LegacyOK {
			t.Fatalf("got %+v", r)
		}
	})
	t.Run("duplicate is not a second neighbor", func(t *testing.T) {
		r := estimateNeighborPosition([]neighborPositionCandidate{near[0], near[0]}, 30, now)
		if r.Estimate.Status != "insufficient" || r.Estimate.ContributorCount != 1 {
			t.Fatalf("got %+v", r)
		}
	})
	t.Run("zero weight is not supporting evidence", func(t *testing.T) {
		cs := append([]neighborPositionCandidate(nil), near...)
		cs[1].LastSeen = time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
		r := estimateNeighborPosition(cs, 30, now)
		if r.Estimate.Status != "insufficient" {
			t.Fatalf("zero weight must not satisfy min2: %+v", r)
		}
	})
	t.Run("negligible stale partner is not supporting evidence", func(t *testing.T) {
		cs := append([]neighborPositionCandidate(nil), near...)
		cs[1].LastSeen = now.Add(-30 * 24 * time.Hour)
		r := estimateNeighborPosition(cs, 30, now)
		if r.Estimate.Status != "insufficient" || r.Estimate.ContributorCount != 1 {
			t.Fatalf("stale partner must not upgrade a singleton: %+v", r)
		}
	})
	t.Run("input permutation deterministic", func(t *testing.T) {
		a := estimateNeighborPosition(near, 30, now)
		b := estimateNeighborPosition([]neighborPositionCandidate{near[1], near[0]}, 30, now)
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("different output: %+v vs %+v", a, b)
		}
	})
	t.Run("bad and future timestamps unknown", func(t *testing.T) {
		cs := append([]neighborPositionCandidate(nil), near...)
		cs[0].LastSeen = parseTimestamp("not a timestamp")
		cs[1].LastSeen = now.Add(24 * time.Hour)
		r := estimateNeighborPosition(cs, 30, now)
		if r.Estimate.UnknownFreshnessCount != 2 || r.Estimate.NewestSeen != "" || r.Estimate.Status != "estimated" {
			t.Fatalf("got %+v", r.Estimate)
		}
	})
}

func TestNeighborPositionEstimate_CoordinateSafety(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for _, c := range []neighborPositionCandidate{
		{Lat: math.NaN(), Lon: 9}, {Lat: 55, Lon: math.Inf(1)}, {Lat: 91, Lon: 9}, {Lat: 55, Lon: 181}, {Lat: 0, Lon: 0},
	} {
		if r := estimateNeighborPosition([]neighborPositionCandidate{c}, 30, now); r.Estimate.Status != "unavailable" || r.LegacyOK {
			t.Fatalf("invalid coordinate accepted: %+v", r)
		}
	}
	cs := []neighborPositionCandidate{{Pubkey: "a", Lat: 0, Lon: 179.95, Count: math.Inf(1)}, {Pubkey: "b", Lat: 0, Lon: -179.95, Count: math.NaN()}}
	r := estimateNeighborPosition(cs, 30, now)
	if r.Estimate.Status != "estimated" || math.IsNaN(*r.Estimate.Lat) || math.Abs(*r.Estimate.Lon) < 179 {
		t.Fatalf("antimeridian/finite count safety: %+v", r)
	}
}

func BenchmarkNeighborPositionEstimate_MaxCandidates(b *testing.B) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	cs := make([]neighborPositionCandidate, 20)
	for i := range cs {
		cs[i] = neighborPositionCandidate{Pubkey: fmt.Sprint(i), Lat: 55 + float64(i)*0.002, Lon: 9.4, Count: 10000, LastSeen: now.Add(-time.Duration(i) * time.Hour)}
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		estimateNeighborPosition(cs, 30, now)
	}
}

func TestNodeDetail_NeighborEstimateAbstention(t *testing.T) {
	for _, status := range []string{"insufficient", "ambiguous"} {
		t.Run(status, func(t *testing.T) {
			srv, router := setupTestServer(t)
			for _, q := range []string{
				`INSERT INTO nodes (public_key,name,role) VALUES ('target','Target','repeater')`,
				`INSERT INTO nodes (public_key,name,lat,lon) VALUES ('a','A',55,9.4)`,
				`INSERT INTO neighbor_edges (node_a,node_b,count) VALUES ('target','a',60)`,
			} {
				if _, err := srv.db.conn.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			if status == "ambiguous" {
				for _, q := range []string{
					`INSERT INTO nodes (public_key,name,lat,lon) VALUES ('b','B',55.01,9.41),('c','C',48,11.5),('d','D',48.01,11.51)`,
					`INSERT INTO neighbor_edges (node_a,node_b,count) VALUES ('target','b',60),('target','c',10000),('target','d',10000)`,
				} {
					if _, err := srv.db.conn.Exec(q); err != nil {
						t.Fatal(err)
					}
				}
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("GET", "/api/nodes/target", nil))
			if w.Code != 200 {
				t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
			}
			var body struct {
				Node map[string]json.RawMessage `json:"node"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			var estimate NeighborPositionEstimate
			if err := json.Unmarshal(body.Node["neighbor_estimate"], &estimate); err != nil {
				t.Fatal(err)
			}
			if estimate.Status != status || estimate.Lat != nil || estimate.Lon != nil {
				t.Fatalf("unexpected metadata %+v", estimate)
			}
			for _, key := range []string{"estimated_lat", "estimated_lon", "estimated_distance_km", "estimated_contributor_count"} {
				if _, exists := body.Node[key]; exists {
					t.Errorf("unsupported estimate leaked %s", key)
				}
			}
		})
	}
}

func TestNeighborPositionEstimate_TargetPositionDoesNotInfluenceEstimate(t *testing.T) {
	db := setupPacketPathCountingDB(t)
	defer db.Close()
	for _, q := range []string{
		`INSERT INTO nodes VALUES ('target','Target','repeater',48,11.5),('a','A','repeater',55,9.4),('b','B','repeater',55.01,9.41)`,
		`INSERT INTO neighbor_edges (node_a,node_b,count,last_seen) VALUES ('target','a',60,'2026-10-03T11:00:00Z'),('target','b',60,'2026-10-02T11:00:00Z')`,
	} {
		if _, err := db.conn.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	before := db.neighborPositionEstimate("target", 30, now)
	if _, err := db.conn.Exec(`UPDATE nodes SET lat=NULL,lon=NULL WHERE public_key='target'`); err != nil {
		t.Fatal(err)
	}
	after := db.neighborPositionEstimate("target", 30, now)
	if !reflect.DeepEqual(before, after) || after.Estimate.Status != "estimated" {
		t.Fatalf("target GPS affected estimate: %+v vs %+v", before, after)
	}
	bulk := make(map[string]neighborEstimate)
	if err := db.nearestPositionedNeighborsChunk([]string{"target"}, 30, bulk, now); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after.Legacy, bulk["target"]) {
		t.Fatalf("freshness single/bulk mismatch: %+v vs %+v", after.Legacy, bulk["target"])
	}
}

func TestNeighborPositionEstimate_CandidateBound(t *testing.T) {
	cs := make([]neighborPositionCandidate, 100)
	for i := range cs {
		cs[i] = neighborPositionCandidate{Pubkey: fmt.Sprint(i), Lat: 55 + float64(i)*0.0001, Lon: 9.4, Count: 1}
	}
	r := estimateNeighborPosition(cs, 30, time.Now())
	if r.Estimate.CandidateCount != 20 || r.Estimate.ContributorCount != 20 {
		t.Fatalf("expected bounded20, got %+v", r)
	}
}
