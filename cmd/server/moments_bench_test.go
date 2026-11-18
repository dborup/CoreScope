package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// #881 M1 perf proof (AGENTS.md rule 0). Each detector runs on an existing
// timer; these measure one tick at realistic and 5x sizes, and the cost the
// busy window adds to the repeater-relay snapshot. The per-packet path is
// untouched (TestMomentsNotOnPacketPath_881).

// nodeTickHarness: n known repeaters, each tick reporting all n with a
// new last_seen (the worst case: every node changed since the last tick).
func nodeTickHarness(tb testing.TB, n int) (*momentsHarness, []momentNodeRow) {
	h := newMomentsHarness(tb, nil)
	old := momentsT0.Add(-90 * 24 * time.Hour)
	rows := make([]momentNodeRow, n)
	for i := range rows {
		rows[i] = nodeRow(momentPK(i), "repeater", momentsT0, old)
	}
	h.tick(rows...)
	return h, rows
}

func nodeTickOnce(h *momentsHarness, rows []momentNodeRow) {
	h.now = h.now.Add(15 * time.Second)
	ls := h.now.Format(time.RFC3339)
	for i := range rows {
		rows[i].LastSeen = ls
	}
	h.rows = rows
	h.d.tickNodes(context.Background())
}

func BenchmarkMomentsNodeTick(b *testing.B) {
	for _, n := range []int{2000, 10000} {
		b.Run(fmt.Sprintf("changed=%d", n), func(b *testing.B) {
			h, rows := nodeTickHarness(b, n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				nodeTickOnce(h, rows)
			}
		})
	}
}

// recordGraph: edges over nodes with GPS, a few hundred km apart at most.
func recordGraph(tb testing.TB, nodes, edges int) (*momentsHarness, *NeighborGraph) {
	h := newMomentsHarness(tb, nil)
	rows := make([]momentNodeRow, nodes)
	for i := range rows {
		rows[i] = linkRow(momentPK(i), "repeater", 54.5+float64(i%100)*0.03, 8+float64(i/100)*0.05)
	}
	h.tick(rows...)
	g := NewNeighborGraph()
	for i := 0; i < edges; i++ {
		a, b := momentPK(i%nodes), momentPK((i*7919+1)%nodes)
		if a == b {
			b = momentPK((i + 1) % nodes)
		}
		g.edges[makeEdgeKey(a, b)] = linkEdge(a, b, 3, momentsT0)
	}
	h.d.onGraph(g) // silent seed
	return h, g
}

func BenchmarkMomentsRecordScan(b *testing.B) {
	for _, c := range []struct{ nodes, edges int }{{2000, 5000}, {10000, 20000}} {
		b.Run(fmt.Sprintf("edges=%d", c.edges), func(b *testing.B) {
			h, g := recordGraph(b, c.nodes, c.edges)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				h.d.onGraph(g)
			}
		})
	}
}

// BenchmarkMomentsDeltaQuery: the real SQL delta on a file DB through the
// mode=ro handle, 20K nodes of which 2K changed since the watermark.
func BenchmarkMomentsDeltaQuery(b *testing.B) {
	path := filepath.Join(b.TempDir(), "m.db")
	rw, err := sql.Open("sqlite", path)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := rw.Exec(ingestorNodesSchema); err != nil {
		b.Fatal(err)
	}
	tx, _ := rw.Begin()
	for i := 0; i < 20000; i++ {
		ls := momentsT0.Add(-time.Duration(i) * time.Second).Format(time.RFC3339)
		if _, err := tx.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen) VALUES (?, 'n', 'repeater', 55, 12, ?, '2026-01-01T00:00:00Z')`, momentPK(i), ls); err != nil {
			b.Fatal(err)
		}
		if i%10 == 0 {
			tx.Exec(`INSERT INTO inactive_nodes (public_key, last_seen) VALUES (?, '2026-01-01T00:00:00Z')`, momentPK(i))
		}
	}
	tx.Commit()
	rw.Close()
	db, err := OpenDB(path)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	srv := &Server{db: db, cfg: &Config{}}
	since := momentsT0.Add(-2000 * time.Second)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := srv.readMomentNodes(context.Background(), since, false)
		// 2000 changed + the row at the watermark second (re-read on purpose).
		if err != nil || len(rows) != 2001 {
			b.Fatalf("%d rows, %v", len(rows), err)
		}
	}
}

// BenchmarkComputeRepeaterRelaySnapshot: the #76/#632 fixture (2,000
// repeaters, 30K transmissions, 150K observations) with the moments busy
// window off (master behaviour) and on.
func BenchmarkComputeRepeaterRelaySnapshot(b *testing.B) {
	store, _ := relayBench632Store(b)
	for _, w := range []time.Duration{0, 10 * time.Minute} {
		b.Run(fmt.Sprintf("busyWindow=%s", w), func(b *testing.B) {
			store.relayRecentWindow.Store(int64(w))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				store.computeRepeaterRelaySnapshot(24)
			}
		})
	}
}

// bestOf returns the fastest of n runs of f.
func bestOf(n int, f func()) time.Duration {
	best := time.Duration(1<<63 - 1)
	for i := 0; i < n; i++ {
		start := time.Now()
		f()
		if d := time.Since(start); d < best {
			best = d
		}
	}
	return best
}

// TestMomentsTickCostBounded_881 enforces the tick budgets at 5x the
// expected scale. The bounds are an order of magnitude above the measured
// cost (see the PR) so a slow CI runner or -race does not flake them, yet an
// O(n²) slip at these sizes (10^8 steps) would blow straight through.
func TestMomentsTickCostBounded_881(t *testing.T) {
	h, rows := nodeTickHarness(t, 10000)
	if d := bestOf(5, func() { nodeTickOnce(h, rows) }); d > 100*time.Millisecond {
		t.Fatalf("node tick over 10K changed rows took %v, budget 100ms", d)
	}
	rh, g := recordGraph(t, 10000, 20000)
	if d := bestOf(5, func() { rh.d.onGraph(g) }); d > 100*time.Millisecond {
		t.Fatalf("record scan over 20K edges took %v, budget 100ms", d)
	}
	bh := newMomentsHarness(t, nil)
	brows := make([]momentNodeRow, 10000)
	counts := make(map[string]int, 10000)
	for i := range brows {
		brows[i] = nodeRow(momentPK(i), "repeater", momentsT0, momentsT0.Add(-2000*time.Hour))
		counts[momentPK(i)] = i
	}
	bh.tick(brows...)
	info := make(map[string]RepeaterRelayInfo, len(counts))
	for k, c := range counts {
		info[k] = RepeaterRelayInfo{relayCountRecent: c}
	}
	snap := &repeaterRelaySnapshot{info: info}
	if d := bestOf(5, func() { bh.d.onRelay(snap) }); d > 100*time.Millisecond {
		t.Fatalf("busiest pass over 10K repeaters took %v, budget 100ms", d)
	}
}
