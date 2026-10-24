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
//     resolver uses: same edges, same affinity minimum, and a hop that
//     resolved by affinity never resolves to the other candidate;
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

	t.Run("hop resolver never switches candidate", func(t *testing.T) {
		// Every neighbour shares the hop prefix "b0". For each pair, the
		// resolver picks by affinity only with >= 3 credits and a 3x score
		// ratio. One uniform decay step is monotone, so a pair that
		// resolved to one neighbour never resolves to the other after it;
		// at most the ratio drops below 3x and the hop falls through to
		// the next tier, as it would for any two comparably strong edges.
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
