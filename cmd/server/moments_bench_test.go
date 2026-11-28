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

// tickCase is one detector pass at size n, built fresh so the two sizes of a
// scaling check share nothing.
type tickCase struct {
	name string
	// build returns the closure that performs one tick at size n.
	build func(tb testing.TB, n int) func()
	// cap is the best-of-rounds wall-clock budget at momentsScaleBig for a
	// plain (non-race) binary, ~10x the idle cost measured for this pass.
	cap time.Duration
}

// budget is the case's cap for this binary. -race (and coverage) instrument
// every memory access, which makes these loops several times slower, so the
// race binary gets its own multiple rather than one number shared by both.
func (c tickCase) budget() time.Duration {
	if raceBuild {
		return c.cap * momentsRaceFactor
	}
	return c.cap
}

func tickCases() []tickCase {
	return []tickCase{
		{"node tick", func(tb testing.TB, n int) func() {
			h, rows := nodeTickHarness(tb, n)
			return func() { nodeTickOnce(h, rows) }
		}, momentsCapNode},
		{"record scan", func(tb testing.TB, n int) func() {
			h, g := recordGraph(tb, n, 2*n)
			return func() { h.d.onGraph(g) }
		}, momentsCapRecord},
		{"busiest pass", func(tb testing.TB, n int) func() {
			h := newMomentsHarness(tb, nil)
			rows := make([]momentNodeRow, n)
			info := make(map[string]RepeaterRelayInfo, n)
			for i := range rows {
				rows[i] = nodeRow(momentPK(i), "repeater", momentsT0, momentsT0.Add(-2000*time.Hour))
				info[momentPK(i)] = RepeaterRelayInfo{relayCountRecent: i}
			}
			h.tick(rows...)
			snap := &repeaterRelaySnapshot{info: info}
			return func() { h.d.onRelay(snap) }
		}, momentsCapBusiest},
	}
}

// Scaling bounds. A linear tick scales ~4x from n to 4n and an O(n²) one
// ~16x; momentsMaxScale sits between them with room for noise.
//
// The absolute caps are per pass, because the passes differ 3x in cost and a
// cap shared by all of them is loose for the cheap ones: a linear tick made
// 85x slower still fit under the old shared 1 s. Each cap is ~10x the idle
// best-of-9 cost at momentsScaleBig on a plain binary (node tick 4.7 ms,
// record scan 10 ms, busiest pass 3.4 ms), so a ~10x constant-factor
// slowdown fails and ordinary runner noise does not. Under -race the cost
// rises to roughly 25x (record scan 250-320 ms idle) and a loaded runner
// reached 1.7 s on a single attempt, so the race binary gets
// momentsRaceFactor x the cap, which leaves headroom for load.
const (
	momentsScaleSmall = 5000
	momentsScaleBig   = 20000
	momentsMaxScale   = 9.0
	momentsCapNode    = 50 * time.Millisecond
	momentsCapRecord  = 100 * time.Millisecond
	momentsCapBusiest = 40 * time.Millisecond
	momentsRaceFactor = 30
	momentsMaxAllocs  = 64 // per tick; measured 1-2, independent of n
)

// scaleRatio returns best(big)/best(small) with the two sizes sampled
// interleaved, so a load spike hits both and mostly cancels. Each size keeps
// its minimum over rounds; the minimum is the least load-sensitive statistic.
func scaleRatio(small, big func(), rounds int) (ratio float64, bigBest time.Duration) {
	sb, bb := time.Duration(1<<63-1), time.Duration(1<<63-1)
	for i := 0; i < rounds; i++ {
		s := time.Now()
		small()
		if d := time.Since(s); d < sb {
			sb = d
		}
		s = time.Now()
		big()
		if d := time.Since(s); d < bb {
			bb = d
		}
	}
	if sb <= 0 {
		sb = 1
	}
	return float64(bb) / float64(sb), bb
}

// TestMomentsTickCostBounded_881 proves a tick does bounded, linear work
// without a runner-dependent wall-clock budget (#926): allocations per tick
// are deterministic, and time is judged by how it scales from n to 4n (a
// ratio, so load cancels) plus a per-pass absolute cap (#931). The
// benchmarks above report the real timings.
func TestMomentsTickCostBounded_881(t *testing.T) {
	for _, c := range tickCases() {
		t.Run(c.name, func(t *testing.T) {
			small, big := c.build(t, momentsScaleSmall), c.build(t, momentsScaleBig)
			if a := testing.AllocsPerRun(5, big); a > momentsMaxAllocs {
				t.Fatalf("%d allocs per tick at n=%d, max %d (per-row allocation?)", int(a), momentsScaleBig, momentsMaxAllocs)
			}
			// Retry the noisy timing check: a real regression fails every
			// attempt, a load spike does not.
			var ratio float64
			var bb time.Duration
			defer func() {
				t.Logf("best %v of budget %v at n=%d (%.1fx headroom), ratio %.1fx", bb, c.budget(), momentsScaleBig, float64(c.budget())/float64(bb), ratio)
			}()
			for attempt := 0; attempt < 3; attempt++ {
				ratio, bb = scaleRatio(small, big, 9)
				if ratio <= momentsMaxScale && bb <= c.budget() {
					return
				}
			}
			t.Fatalf("tick scales %.1fx for %dx the rows (max %.1fx), best %v at n=%d (cap %v)",
				ratio, momentsScaleBig/momentsScaleSmall, momentsMaxScale, bb, momentsScaleBig, c.budget())
		})
	}
}
