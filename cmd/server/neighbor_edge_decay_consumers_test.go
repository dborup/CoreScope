package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// The ingestor decays neighbor_edges.count (#544); the server only reads
// it. These tests feed the server's readers of the persisted table a
// snapshot before and after decay, built from the shared golden table that
// cmd/ingestor's decay SQL is tested against:
//   - the PacketStore graph (loadNeighborEdgesFromDB), which the hop
//     resolver uses: same edges and the same affinity minimum. On these
//     same-age pairs one uniform step cannot flip a resolution, but that
//     does not hold in general: when credits and decay interleave, a
//     newer edge can outrank an older one (TestNeighborEdgeDecay_
//     RecentTrafficFlipsAffinity, review F1);
//   - reach degree (loadValidDegrees), which only counts edges;
//   - the count-weighted estimated position (nearestPositionedNeighbor).
// The neighbour list and neighbour graph APIs are built from in-memory
// packets (BuildFromStoreWithOptions) and never read the table.

type neighborDecayGolden struct {
	Cases []struct {
		Before int `json:"before"`
		Days   int `json:"days"`
		After  int `json:"after"`
	} `json:"cases"`
}

func loadNeighborDecayGolden(t *testing.T) neighborDecayGolden {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "test-fixtures", "neighbor-edge-decay-golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g neighborDecayGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Cases) == 0 {
		t.Fatal("golden table has no cases")
	}
	return g
}

var decayTarget = pk64("7a7a")

func decayNeighbor(i int) string { return pk64(fmt.Sprintf("b%03x", i)) }

// newDecaySnapshotDB stores one edge from decayTarget to a positioned
// neighbour per golden case, with the before or after count. last_seen
// ages spread over 0-100 hours, inside the default 5-day edge retention,
// so the affinity score's time decay is exercised too.
func newDecaySnapshotDB(t *testing.T, g neighborDecayGolden, after bool) *DB {
	t.Helper()
	db := setupTestDB(t)
	t.Cleanup(func() { db.Close() })
	if _, err := db.conn.Exec(`CREATE TABLE IF NOT EXISTS neighbor_edges (node_a TEXT NOT NULL, node_b TEXT NOT NULL, count INTEGER DEFAULT 1, last_seen TEXT, PRIMARY KEY (node_a, node_b))`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := db.conn.Exec(`INSERT INTO nodes (public_key, name, role) VALUES (?, 'Target', 'repeater')`, decayTarget); err != nil {
		t.Fatal(err)
	}
	for i, c := range g.Cases {
		n := decayNeighbor(i)
		if _, err := db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES (?, ?, 'repeater', ?, 9.4)`,
			n, fmt.Sprintf("N%d", i), 55.0+0.005*float64(i)); err != nil {
			t.Fatal(err)
		}
		count := c.Before
		if after {
			count = c.After
		}
		a, b := decayTarget, n
		if a > b {
			a, b = b, a
		}
		if _, err := db.conn.Exec(`INSERT INTO neighbor_edges (node_a, node_b, count, last_seen) VALUES (?, ?, ?, ?)`,
			a, b, count, now.Add(-time.Duration(i%6)*20*time.Hour).Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestNeighborEdgeDecay_SnapshotConsumers(t *testing.T) {
	g := loadNeighborDecayGolden(t)
	before, after := newDecaySnapshotDB(t, g, false), newDecaySnapshotDB(t, g, true)
	graphBefore, graphAfter := loadNeighborEdgesFromDB(before.conn), loadNeighborEdgesFromDB(after.conn)

	t.Run("snapshot graph edges", func(t *testing.T) {
		set := func(gr *NeighborGraph) string {
			var out []string
			for _, e := range gr.Neighbors(decayTarget) {
				out = append(out, e.NodeA+"--"+e.NodeB)
			}
			sort.Strings(out)
			return strings.Join(out, ",")
		}
		if b, a := set(graphBefore), set(graphAfter); a != b || len(graphBefore.Neighbors(decayTarget)) != len(g.Cases) {
			t.Errorf("snapshot graph neighbours differ after decay (before %d)", len(graphBefore.Neighbors(decayTarget)))
		}
	})

	t.Run("hop resolver on same-age pairs after uniform decay", func(t *testing.T) {
		// Every neighbour shares the hop prefix "b0". For each pair, the
		// resolver picks by affinity only with >= 3 credits and a 3x score
		// ratio. This only covers two edges of the same age that both went
		// through the same decay steps without new credits in between:
		// decay is monotone there, so a resolution can fall through to the
		// next tier but not move to the other neighbour. With new credits
		// between steps it can (TestNeighborEdgeDecay_RecentTrafficFlipsAffinity).
		var nodes []nodeInfo
		for i := range g.Cases {
			nodes = append(nodes, nodeInfo{PublicKey: decayNeighbor(i), Name: fmt.Sprintf("N%d", i), Role: "repeater"})
		}
		resolve := func(gr *NeighborGraph, i, j int) string {
			pm := buildPrefixMap([]nodeInfo{nodes[i], nodes[j]})
			r, method, _ := pm.resolveWithContext("b0", []string{decayTarget}, gr)
			if r == nil || method != "neighbor_affinity" {
				return ""
			}
			return r.PublicKey
		}
		pairs, kept, lost, gained := 0, 0, 0, 0
		for i, ci := range g.Cases {
			for j, cj := range g.Cases {
				if j <= i || ci.Days != 1 || cj.Days != 1 || i%6 != j%6 {
					continue
				}
				pairs++
				b, a := resolve(graphBefore, i, j), resolve(graphAfter, i, j)
				switch {
				case b != "" && a != "" && a != b:
					t.Errorf("counts %d/%d: resolved to %s before decay, %s after", ci.Before, cj.Before, b[:4], a[:4])
				case b != "" && a == b:
					kept++
				case b != "" && a == "":
					lost++
				case b == "" && a != "":
					gained++
				}
				if ci.Before == ci.After && cj.Before == cj.After && a != b {
					t.Errorf("counts %d/%d unchanged by decay but the resolution changed", ci.Before, cj.Before)
				}
			}
		}
		if pairs == 0 || kept == 0 {
			t.Fatalf("setup: %d pairs, %d affinity resolutions kept; the check would be vacuous", pairs, kept)
		}
		t.Logf("%d same-age candidate pairs: %d affinity resolutions kept, %d fell through, %d new", pairs, kept, lost, gained)
	})

	t.Run("affinity minimum observations", func(t *testing.T) {
		eligible := func(gr *NeighborGraph) int {
			n := 0
			for _, e := range gr.AllEdges() {
				if e.Count >= affinityMinObservations {
					n++
				}
			}
			return n
		}
		if b, a := eligible(graphBefore), eligible(graphAfter); a != b {
			t.Errorf("affinity-eligible edges: %d after decay, %d before", a, b)
		}
	})

	t.Run("reach degree", func(t *testing.T) {
		ctx := context.Background()
		b, err := loadValidDegrees(ctx, before.conn)
		if err != nil {
			t.Fatal(err)
		}
		a, err := loadValidDegrees(ctx, after.conn)
		if err != nil {
			t.Fatal(err)
		}
		if b[decayTarget] != len(g.Cases) {
			t.Fatalf("setup: target degree %d, want %d", b[decayTarget], len(g.Cases))
		}
		if len(a) != len(b) {
			t.Fatalf("degree map size %d after decay, %d before", len(a), len(b))
		}
		for pk, d := range b {
			if a[pk] != d {
				t.Errorf("degree of %s: %d after decay, %d before", pk[:8], a[pk], d)
			}
		}
	})

	t.Run("map estimated position", func(t *testing.T) {
		_, latB, lonB, nB, _, okB := before.nearestPositionedNeighbor(decayTarget, EstimateMaxEdgeKm)
		_, latA, lonA, nA, _, okA := after.nearestPositionedNeighbor(decayTarget, EstimateMaxEdgeKm)
		if !okB || !okA {
			t.Fatalf("estimate ok before=%v after=%v", okB, okA)
		}
		if nA != nB {
			t.Errorf("estimate contributors: %d after decay, %d before", nA, nB)
		}
		// The weights move the centroid within the neighbours' own spread.
		if d := haversineKm(latB, lonB, latA, lonA); d > EstimateMaxEdgeKm {
			t.Errorf("estimate moved %.1f km", d)
		}
	})
}

type persistedEdge struct {
	a, b  string
	count int
	age   time.Duration // last_seen before now
}

// persistedNeighborGraph stores edges in a neighbor_edges table and loads
// them the way the server does (loadNeighborEdgesFromDB).
func persistedNeighborGraph(t *testing.T, edges []persistedEdge) *NeighborGraph {
	t.Helper()
	db := setupTestDB(t)
	defer db.Close()
	if _, err := db.conn.Exec(`CREATE TABLE IF NOT EXISTS neighbor_edges (node_a TEXT NOT NULL, node_b TEXT NOT NULL, count INTEGER DEFAULT 1, last_seen TEXT, PRIMARY KEY (node_a, node_b))`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, e := range edges {
		a, b := e.a, e.b
		if a > b {
			a, b = b, a
		}
		if _, err := db.conn.Exec(`INSERT INTO neighbor_edges (node_a, node_b, count, last_seen) VALUES (?, ?, ?, ?)`,
			a, b, e.count, now.Add(-e.age).Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	return loadNeighborEdgesFromDB(db.conn)
}

type decayRecentTraffic struct {
	Edges []struct {
		Neighbor         string `json:"neighbor"`
		LastSeenHoursAgo int    `json:"lastSeenHoursAgo"`
		LifetimeCount    int    `json:"lifetimeCount"`
		DecayedCount     int    `json:"decayedCount"`
	} `json:"edges"`
	ResolvedLifetime string `json:"resolvedLifetime"`
	ResolvedDecayed  string `json:"resolvedDecayed"`
}

// With decay enabled, affinity follows recent traffic, so an established
// edge can lose a hop to a newer one (review F1). The counts come from the
// ingestor's simulation of the shared scenario (the real write path and
// the real decay): A has 400 old credits and one recent one, B a burst of
// 20 over the last two days. On the lifetime counts (decay off, the
// default) the hop resolves to A by affinity; on the decayed counts it
// resolves to B, also by affinity.
func TestNeighborEdgeDecay_RecentTrafficFlipsAffinity(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "test-fixtures", "neighbor-edge-decay-recent-traffic.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sc decayRecentTraffic
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatal(err)
	}
	if len(sc.Edges) != 2 || sc.ResolvedLifetime == sc.ResolvedDecayed {
		t.Fatalf("scenario: %d edges, %s -> %s", len(sc.Edges), sc.ResolvedLifetime, sc.ResolvedDecayed)
	}
	target := pk64("7a7a")
	pubkey := map[string]string{}
	var nodes []nodeInfo
	for i, e := range sc.Edges {
		pubkey[e.Neighbor] = pk64(fmt.Sprintf("b0%02x", i+1)) // shared hop prefix "b0"
		nodes = append(nodes, nodeInfo{PublicKey: pubkey[e.Neighbor], Name: e.Neighbor, Role: "repeater"})
	}
	pm := buildPrefixMap(nodes)
	resolve := func(decayed bool) (string, string) {
		var edges []persistedEdge
		for _, e := range sc.Edges {
			count := e.LifetimeCount
			if decayed {
				count = e.DecayedCount
			}
			edges = append(edges, persistedEdge{target, pubkey[e.Neighbor], count, time.Duration(e.LastSeenHoursAgo) * time.Hour})
		}
		r, method, _ := pm.resolveWithContext("b0", []string{target}, persistedNeighborGraph(t, edges))
		if r == nil {
			return "", method
		}
		return r.Name, method
	}
	for _, tc := range []struct {
		name    string
		decayed bool
		want    string
	}{
		{"lifetime counts", false, sc.ResolvedLifetime},
		{"decayed counts", true, sc.ResolvedDecayed},
	} {
		if got, method := resolve(tc.decayed); got != tc.want || method != "neighbor_affinity" {
			t.Errorf("%s: resolved to %q via %s, want %q via neighbor_affinity", tc.name, got, method, tc.want)
		}
	}
}
