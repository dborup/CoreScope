package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// decayT0 is an arbitrary fixed clock origin for the decay tests.
var decayT0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

const decayDay = 24 * time.Hour

func newDecayStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "decay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.WaitForAsyncMigrations()
	return s
}

func insertDecayEdge(t *testing.T, s *Store, a, b string, count any, lastSeen string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO neighbor_edges (node_a, node_b, count, last_seen) VALUES (?, ?, ?, ?)`,
		a, b, count, lastSeen); err != nil {
		t.Fatal(err)
	}
}

func decayEdgeCount(t *testing.T, s *Store, a, b string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count FROM neighbor_edges WHERE node_a = ? AND node_b = ?`, a, b).Scan(&n); err != nil {
		t.Fatalf("count %s--%s: %v", a, b, err)
	}
	return n
}

type decayEdgeSnapshot struct {
	count    any
	lastSeen string
}

func decayEdges(t *testing.T, s *Store) map[string]decayEdgeSnapshot {
	t.Helper()
	rows, err := s.db.Query(`SELECT node_a, node_b, count, COALESCE(last_seen, '') FROM neighbor_edges`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := make(map[string]decayEdgeSnapshot)
	for rows.Next() {
		var a, b, ls string
		var c any
		if err := rows.Scan(&a, &b, &c, &ls); err != nil {
			t.Fatal(err)
		}
		out[a+"--"+b] = decayEdgeSnapshot{c, ls}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// mustDecay runs one decay check and fails the test on error.
func mustDecay(t *testing.T, s *Store, now time.Time, halfLifeDays int) neighborEdgeDecay {
	t.Helper()
	d, err := s.DecayNeighborEdgeCounts(now, halfLifeDays)
	if err != nil {
		t.Fatalf("decay at %s: %v", now.Format(time.RFC3339), err)
	}
	return d
}

func decayClock(t *testing.T, s *Store) (int64, bool) {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'neighbor_edge_decay_state'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		return 0, false
	}
	var through int64
	err := s.db.QueryRow(`SELECT decayed_through FROM neighbor_edge_decay_state WHERE id = 1`).Scan(&through)
	if err != nil {
		return 0, false
	}
	return through, true
}

type decayGolden struct {
	HalfLifeDays int `json:"halfLifeDays"`
	Floor        int `json:"floor"`
	Cases        []struct {
		Before int `json:"before"`
		Days   int `json:"days"`
		After  int `json:"after"`
	} `json:"cases"`
}

func loadDecayGolden(t *testing.T) decayGolden {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "test-fixtures", "neighbor-edge-decay-golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g decayGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Cases) == 0 {
		t.Fatal("golden table has no cases")
	}
	return g
}

// The real SQL reproduces the shared golden table (#544). The server's
// consumer test reads the same table, so the two sides agree on what a
// decayed snapshot looks like.
func TestNeighborEdgeDecay_GoldenTable(t *testing.T) {
	g := loadDecayGolden(t)
	if g.Floor != neighborEdgeDecayFloor {
		t.Fatalf("golden floor %d, code floor %d", g.Floor, neighborEdgeDecayFloor)
	}
	for i, c := range g.Cases {
		t.Run(fmt.Sprintf("%d_%dd", c.Before, c.Days), func(t *testing.T) {
			s := newDecayStore(t)
			a, b := fmt.Sprintf("a%03d", i), fmt.Sprintf("b%03d", i)
			insertDecayEdge(t, s, a, b, c.Before, decayT0.Format(time.RFC3339))
			mustDecay(t, s, decayT0, g.HalfLifeDays) // starts the clock
			mustDecay(t, s, decayT0.Add(time.Duration(c.Days)*decayDay), g.HalfLifeDays)
			if got := decayEdgeCount(t, s, a, b); got != c.After {
				t.Fatalf("count %d after %d day(s) = %d, want %d", c.Before, c.Days, got, c.After)
			}
		})
	}
}

// At a short half-life rounding no longer protects small counts, and the
// floor does: counts above it stop at affinityMinObservations (3), counts
// at or below it are never raised or lowered.
func TestNeighborEdgeDecay_FloorAtShortHalfLife(t *testing.T) {
	s := newDecayStore(t)
	want := map[int]int{1: 1, 2: 2, 3: 3, 4: 3, 5: 3, 10: 3, 1000: 3}
	for before := range want {
		insertDecayEdge(t, s, fmt.Sprintf("a%d", before), "b", before, decayT0.Format(time.RFC3339))
	}
	mustDecay(t, s, decayT0, 1)
	mustDecay(t, s, decayT0.Add(30*decayDay), 1)
	for before, after := range want {
		if got := decayEdgeCount(t, s, fmt.Sprintf("a%d", before), "b"); got != after {
			t.Errorf("count %d after 30 days at a 1-day half-life = %d, want %d", before, got, after)
		}
	}
}

// The first check only starts the clock: nothing is decayed until a
// whole day has passed since the feature first ran.
func TestNeighborEdgeDecay_FirstRunOnlyStartsClock(t *testing.T) {
	s := newDecayStore(t)
	insertDecayEdge(t, s, "a", "b", 500, decayT0.Format(time.RFC3339))
	d := mustDecay(t, s, decayT0, 7)
	if d.Steps != 0 || d.Lowered != 0 {
		t.Fatalf("first run decayed: %+v", d)
	}
	if got := decayEdgeCount(t, s, "a", "b"); got != 500 {
		t.Fatalf("count = %d, want 500", got)
	}
	through, ok := decayClock(t, s)
	if !ok || through != decayT0.Unix() {
		t.Fatalf("clock = %d (set=%v), want %d", through, ok, decayT0.Unix())
	}
}

// Only whole days are applied, and the clock advances by whole days, so
// hourly checks neither decay early nor drift.
func TestNeighborEdgeDecay_WholeDaysWithoutDrift(t *testing.T) {
	s := newDecayStore(t)
	insertDecayEdge(t, s, "a", "b", 1000, decayT0.Format(time.RFC3339))
	mustDecay(t, s, decayT0, 7)

	if d := mustDecay(t, s, decayT0.Add(decayDay-time.Second), 7); d.Steps != 0 {
		t.Fatalf("decayed before a whole day: %+v", d)
	}
	if got := decayEdgeCount(t, s, "a", "b"); got != 1000 {
		t.Fatalf("count before a whole day = %d", got)
	}
	if d := mustDecay(t, s, decayT0.Add(36*time.Hour), 7); d.Steps != 1 || d.Lowered != 1 {
		t.Fatalf("at 36h: %+v, want 1 step lowering 1 edge", d)
	}
	if through, _ := decayClock(t, s); through != decayT0.Add(decayDay).Unix() {
		t.Fatalf("clock after 36h = %d, want t0+24h %d (no drift)", through, decayT0.Add(decayDay).Unix())
	}
	if d := mustDecay(t, s, decayT0.Add(2*decayDay), 7); d.Steps != 1 {
		t.Fatalf("at 48h: %+v, want the second step", d)
	}
	if got := decayEdgeCount(t, s, "a", "b"); got != 821 {
		t.Fatalf("count after two days = %d, want 821", got)
	}
}

// Catching up after downtime gives exactly the counts daily checks
// would, so the result does not depend on the restart schedule.
func TestNeighborEdgeDecay_CatchUpEqualsDailyChecks(t *testing.T) {
	counts := []int{1, 10, 11, 12, 37, 100, 999, 123456}
	daily, once := newDecayStore(t), newDecayStore(t)
	for i, c := range counts {
		for _, s := range []*Store{daily, once} {
			insertDecayEdge(t, s, fmt.Sprintf("a%d", i), fmt.Sprintf("b%d", i), c, decayT0.Format(time.RFC3339))
		}
	}
	mustDecay(t, daily, decayT0, 7)
	mustDecay(t, once, decayT0, 7)
	for d := 1; d <= 30; d++ {
		mustDecay(t, daily, decayT0.Add(time.Duration(d)*decayDay), 7)
	}
	if d := mustDecay(t, once, decayT0.Add(30*decayDay+time.Hour), 7); d.Steps != 30 {
		t.Fatalf("catch-up steps = %d, want 30", d.Steps)
	}
	for i := range counts {
		a, b := fmt.Sprintf("a%d", i), fmt.Sprintf("b%d", i)
		if x, y := decayEdgeCount(t, daily, a, b), decayEdgeCount(t, once, a, b); x != y {
			t.Errorf("count %d: daily %d, catch-up %d", counts[i], x, y)
		}
	}
	if through, _ := decayClock(t, once); through != decayT0.Add(30*decayDay).Unix() {
		t.Fatalf("catch-up clock = %d, want t0+30d", through)
	}
}

// A very long gap stops early once every count is at the floor and still
// moves the clock to the last whole day.
func TestNeighborEdgeDecay_LongGapStopsAtFloor(t *testing.T) {
	s := newDecayStore(t)
	insertDecayEdge(t, s, "a", "b", 1<<40, decayT0.Format(time.RFC3339))
	mustDecay(t, s, decayT0, 7)
	start := time.Now()
	d := mustDecay(t, s, decayT0.Add(3650*decayDay), 7)
	if took := time.Since(start); took > 20*time.Second {
		t.Fatalf("10-year catch-up took %s", took)
	}
	if d.Steps != 3650 {
		t.Fatalf("steps = %d, want 3650", d.Steps)
	}
	// At a 7-day half-life rounding stops at 5: round(5 * 0.906) = 5.
	if got := decayEdgeCount(t, s, "a", "b"); got != 5 {
		t.Fatalf("count = %d, want the rounding plateau 5", got)
	}
	if through, _ := decayClock(t, s); through != decayT0.Add(3650*decayDay).Unix() {
		t.Fatalf("clock = %d, want t0+3650d", through)
	}
	if d := mustDecay(t, s, decayT0.Add(3651*decayDay), 7); d.Steps != 1 || d.Lowered != 0 {
		t.Fatalf("step after the plateau: %+v, want 1 step lowering nothing", d)
	}
}

// A clock that moved back by up to one day decays nothing and keeps the
// clock, so the next step still comes one day after the last one.
func TestNeighborEdgeDecay_ClockBackwardsIsNoop(t *testing.T) {
	for _, back := range []time.Duration{time.Second, 12 * time.Hour, decayDay} {
		t.Run(back.String(), func(t *testing.T) {
			s := newDecayStore(t)
			insertDecayEdge(t, s, "a", "b", 500, decayT0.Format(time.RFC3339))
			mustDecay(t, s, decayT0, 7)
			if d := mustDecay(t, s, decayT0.Add(-back), 7); d.Steps != 0 || d.Lowered != 0 || d.ClockReset {
				t.Fatalf("backwards clock decayed or reset: %+v", d)
			}
			if got := decayEdgeCount(t, s, "a", "b"); got != 500 {
				t.Fatalf("count = %d, want 500", got)
			}
			if through, _ := decayClock(t, s); through != decayT0.Unix() {
				t.Fatalf("clock moved to %d", through)
			}
		})
	}
}

// A stored clock more than one day ahead of now (the host clock was once
// far in the future, review F5) is reset to now instead of stalling decay
// until real time catches up. The reset decays nothing; the next step is
// one day later.
func TestNeighborEdgeDecay_ClockFarAheadResets(t *testing.T) {
	s := newDecayStore(t)
	insertDecayEdge(t, s, "a", "b", 500, decayT0.Format(time.RFC3339))
	mustDecay(t, s, decayT0.Add(100*decayDay), 7) // first run starts the clock 100 days ahead
	if d := mustDecay(t, s, decayT0, 7); d.Steps != 0 || d.Lowered != 0 || !d.ClockReset {
		t.Fatalf("corrected clock: %+v, want a reset without decay", d)
	}
	if through, _ := decayClock(t, s); through != decayT0.Unix() {
		t.Fatalf("clock = %d, want reset to %d", through, decayT0.Unix())
	}
	if got := decayEdgeCount(t, s, "a", "b"); got != 500 {
		t.Fatalf("count after reset = %d, want 500", got)
	}
	if d := mustDecay(t, s, decayT0.Add(decayDay), 7); d.Steps != 1 {
		t.Fatalf("one day after the reset: %+v, want one step", d)
	}
	if got := decayEdgeCount(t, s, "a", "b"); got != 453 {
		t.Fatalf("count one day after the reset = %d, want 453", got)
	}
	// Just past the one-day tolerance resets too.
	s2 := newDecayStore(t)
	mustDecay(t, s2, decayT0.Add(decayDay+time.Second), 7)
	if d := mustDecay(t, s2, decayT0, 7); !d.ClockReset {
		t.Fatalf("clock 1 day + 1 s ahead: %+v, want a reset", d)
	}
}

// Disabled decay changes no count and forgets the clock, so enabling it
// again later starts fresh instead of catching up the disabled period.
func TestNeighborEdgeDecay_DisabledKeepsCountsAndForgetsClock(t *testing.T) {
	for _, h := range []int{0, -1} {
		t.Run(fmt.Sprint(h), func(t *testing.T) {
			s := newDecayStore(t)
			insertDecayEdge(t, s, "a", "b", 500, decayT0.Format(time.RFC3339))
			mustDecay(t, s, decayT0, 7)
			if d := mustDecay(t, s, decayT0.Add(30*decayDay), h); d.Steps != 0 || d.Lowered != 0 {
				t.Fatalf("disabled decay ran: %+v", d)
			}
			if got := decayEdgeCount(t, s, "a", "b"); got != 500 {
				t.Fatalf("count = %d, want 500", got)
			}
			if _, ok := decayClock(t, s); ok {
				t.Fatal("disabled decay kept the clock")
			}
			if d := mustDecay(t, s, decayT0.Add(31*decayDay), 7); d.Steps != 0 {
				t.Fatalf("re-enabled decay caught up the disabled period: %+v", d)
			}
			if got := decayEdgeCount(t, s, "a", "b"); got != 500 {
				t.Fatalf("count after re-enable = %d, want 500", got)
			}
		})
	}
}

// Decay changes weights only: every edge, every last_seen and every count
// at or below the floor stays as it was, NULL counts included. The
// builder's watermark (MAX(last_seen)) is therefore untouched and new
// credits add to the decayed count.
func TestNeighborEdgeDecay_KeepsEdgesLastSeenAndWatermark(t *testing.T) {
	s := newDecayStore(t)
	origin, relay, observer := strings.Repeat("aa", 32), strings.Repeat("bb", 32), strings.Repeat("cc", 32)
	for _, key := range []string{origin, relay, observer} {
		if _, err := s.db.Exec(`INSERT INTO nodes (public_key, role) VALUES (?, 'repeater')`, key); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO observers (id) VALUES (?)`, observer); err != nil {
		t.Fatal(err)
	}
	var observerRow int
	if err := s.db.QueryRow(`SELECT rowid FROM observers WHERE id = ?`, observer).Scan(&observerRow); err != nil {
		t.Fatal(err)
	}
	// One transmission per observation: observations dedup on
	// (transmission_id, observer_idx, path_json).
	addObs := func(ts int64) {
		t.Helper()
		res, err := s.db.Exec(`INSERT INTO transmissions
			(raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey)
			VALUES ('', ?, '2026-10-01T12:00:00Z', 1, ?, '{}', ?)`, fmt.Sprintf("decay-%d", ts), payloadADVERT, origin)
		if err != nil {
			t.Fatal(err)
		}
		txID, _ := res.LastInsertId()
		if _, err := s.db.Exec(`INSERT INTO observations
			(transmission_id, observer_idx, direction, path_json, timestamp)
			VALUES (?, ?, 'rx', '["bbbb"]', ?)`, txID, observerRow, ts); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 40; i++ {
		addObs(decayT0.Unix() - 3600 + int64(i))
	}
	if _, err := s.buildAndPersistNeighborEdges(); err != nil {
		t.Fatal(err)
	}
	insertDecayEdge(t, s, "low-a", "low-b", 1, "2026-09-30T00:00:00Z")
	insertDecayEdge(t, s, "floor-a", "floor-b", neighborEdgeDecayFloor, "2026-09-30T00:00:00Z")
	insertDecayEdge(t, s, "null-a", "null-b", nil, "2026-09-29T00:00:00Z")
	before := decayEdges(t, s)
	if len(before) != 5 {
		t.Fatalf("setup: %d edges, want 5", len(before))
	}

	mustDecay(t, s, decayT0, 7)
	mustDecay(t, s, decayT0.Add(14*decayDay), 7)

	after := decayEdges(t, s)
	if len(after) != len(before) {
		t.Fatalf("edge set changed: %d -> %d", len(before), len(after))
	}
	for k, b := range before {
		a, ok := after[k]
		if !ok {
			t.Fatalf("edge %s removed", k)
		}
		if a.lastSeen != b.lastSeen {
			t.Errorf("edge %s last_seen %q -> %q", k, b.lastSeen, a.lastSeen)
		}
	}
	for _, k := range []string{"low-a--low-b", "floor-a--floor-b", "null-a--null-b"} {
		if fmt.Sprint(after[k].count) != fmt.Sprint(before[k].count) {
			t.Errorf("edge %s count %v -> %v, want unchanged", k, before[k].count, after[k].count)
		}
	}
	// 40 credits, two weeks: 40 -> 36 -> 33 -> 30 -> 27 -> 24 -> 22 -> 20 -> 18 -> 16 -> 14 -> 13 -> 12 -> 11 -> 10.
	op := origin + "--" + relay
	if fmt.Sprint(after[op].count) != "10" {
		t.Fatalf("built edge count after 14 days = %v, want 10", after[op].count)
	}

	if n, err := s.buildAndPersistNeighborEdges(); err != nil || n != 0 {
		t.Fatalf("delta scan after decay: %d upserts, err %v; the watermark moved", n, err)
	}
	addObs(decayT0.Unix() + 60)
	if n, err := s.buildAndPersistNeighborEdges(); err != nil || n != 2 {
		t.Fatalf("new observation: %d upserts, err %v; want 2", n, err)
	}
	if got := decayEdgeCount(t, s, origin, relay); got != 11 {
		t.Fatalf("credit after decay: count = %d, want 11", got)
	}
}

// An edge first credited after a step is decayed from the next whole day,
// like every other edge; until then it keeps every credit.
func TestNeighborEdgeDecay_NewEdgeWaitsForNextStep(t *testing.T) {
	s := newDecayStore(t)
	insertDecayEdge(t, s, "old-a", "old-b", 50, decayT0.Format(time.RFC3339))
	mustDecay(t, s, decayT0, 7)
	mustDecay(t, s, decayT0.Add(decayDay), 7)
	if got := decayEdgeCount(t, s, "old-a", "old-b"); got != 45 {
		t.Fatalf("old edge after one day = %d, want 45", got)
	}
	ts := decayT0.Add(decayDay + time.Hour).Format(time.RFC3339)
	edges := make([]edgeRow, 0, 50)
	for i := 0; i < 50; i++ {
		edges = append(edges, canonEdge("new-a", "new-b", ts))
	}
	if err := s.persistNeighborEdges(edges); err != nil {
		t.Fatal(err)
	}
	mustDecay(t, s, decayT0.Add(decayDay+12*time.Hour), 7)
	if got := decayEdgeCount(t, s, "new-a", "new-b"); got != 50 {
		t.Fatalf("new edge decayed before the next whole day: %d", got)
	}
	mustDecay(t, s, decayT0.Add(2*decayDay), 7)
	if got := decayEdgeCount(t, s, "new-a", "new-b"); got != 45 {
		t.Fatalf("new edge after its first step = %d, want 45", got)
	}
	if got := decayEdgeCount(t, s, "old-a", "old-b"); got != 41 {
		t.Fatalf("old edge after two steps = %d, want 41", got)
	}
}

// Retention still decides which edges exist: decay neither deletes an
// edge nor keeps a stale one alive.
func TestNeighborEdgeDecay_RetentionStillPrunes(t *testing.T) {
	s := newDecayStore(t)
	now := time.Now().UTC()
	insertDecayEdge(t, s, "stale-a", "stale-b", 5000, now.Add(-6*decayDay).Format(time.RFC3339))
	insertDecayEdge(t, s, "fresh-a", "fresh-b", 5000, now.Add(-time.Hour).Format(time.RFC3339))
	mustDecay(t, s, now.Add(-3*decayDay), 7)
	mustDecay(t, s, now, 7)
	if got := decayEdgeCount(t, s, "stale-a", "stale-b"); got >= 5000 || got < neighborEdgeDecayFloor {
		t.Fatalf("stale edge count after 3 days = %d", got)
	}
	n, err := s.PruneNeighborEdges(5)
	if err != nil || n != 1 {
		t.Fatalf("prune: %d rows, err %v; want the stale edge only", n, err)
	}
	if _, ok := decayEdges(t, s)["fresh-a--fresh-b"]; !ok {
		t.Fatal("fresh edge pruned")
	}
}

// Decay serialises with the builder through the writer lock: concurrent
// credits and checks lose no credit and race nothing (run with -race).
func TestNeighborEdgeDecay_ConcurrentWithBuilderCredits(t *testing.T) {
	s := newDecayStore(t)
	insertDecayEdge(t, s, "a", "b", 10, decayT0.Format(time.RFC3339))
	mustDecay(t, s, decayT0, 7)
	before := writerStatsAgg.get("neighbor_decay").snapshot().Count

	const writers, credits = 4, 50
	var wg sync.WaitGroup
	errs := make(chan error, writers*credits+30)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < credits; i++ {
				if err := s.persistNeighborEdges([]edgeRow{canonEdge("a", "b", decayT0.Format(time.RFC3339))}); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for d := 1; d <= 30; d++ {
			if _, err := s.DecayNeighborEdgeCounts(decayT0.Add(time.Duration(d)*decayDay), 7); err != nil {
				errs <- err
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	got := decayEdgeCount(t, s, "a", "b")
	if got < neighborEdgeDecayFloor || got > 10+writers*credits {
		t.Fatalf("count = %d, outside [%d, %d]", got, neighborEdgeDecayFloor, 10+writers*credits)
	}
	if through, _ := decayClock(t, s); through != decayT0.Add(30*decayDay).Unix() {
		t.Fatalf("clock = %d, want t0+30d", through)
	}
	if after := writerStatsAgg.get("neighbor_decay").snapshot().Count; after <= before {
		t.Fatal("decay did not run under the instrumented writer lock (component neighbor_decay)")
	}
	// A credit after the concurrent run lands on top of its result.
	if err := s.persistNeighborEdges([]edgeRow{canonEdge("a", "b", decayT0.Format(time.RFC3339))}); err != nil {
		t.Fatal(err)
	}
	if again := decayEdgeCount(t, s, "a", "b"); again != got+1 {
		t.Fatalf("credit after concurrent run: %d -> %d", got, again)
	}
}

func TestStartNeighborEdgeDecay_ChecksAtStartAndStops(t *testing.T) {
	s := newDecayStore(t)
	insertDecayEdge(t, s, "a", "b", 500, decayT0.Format(time.RFC3339))
	stop := s.StartNeighborEdgeDecay(7, time.Hour)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := decayClock(t, s); ok {
			break
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatal("no decay check ran at start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stop did not return")
	}
	stop() // idempotent
}

func TestNeighborEdgeCountHalfLifeDaysOrDefault(t *testing.T) {
	var nilCfg *Config
	for _, tc := range []struct {
		cfg  *Config
		want int
	}{
		{nilCfg, 0}, // opt-in (review F1/F2): unset means disabled
		{&Config{}, 0},
		{&Config{NeighborEdgeCountHalfLifeDays: 3}, 3},
		{&Config{NeighborEdgeCountHalfLifeDays: 30}, 30},
		{&Config{NeighborEdgeCountHalfLifeDays: -1}, 0},
	} {
		if got := tc.cfg.NeighborEdgeCountHalfLifeDaysOrDefault(); got != tc.want {
			set := "nil config"
			if tc.cfg != nil {
				set = fmt.Sprint(tc.cfg.NeighborEdgeCountHalfLifeDays)
			}
			t.Errorf("neighborEdgeCountHalfLifeDays %s: got %d, want %d", set, got, tc.want)
		}
	}
	var cfg Config
	if err := json.Unmarshal([]byte(`{"neighborEdgeCountHalfLifeDays": 14}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.NeighborEdgeCountHalfLifeDaysOrDefault(); got != 14 {
		t.Fatalf("JSON key: got %d, want 14", got)
	}
}

// The ingestor's wiring (main.go) passes the configured half-life to the
// background check: off by default, the configured value otherwise. The
// clock is seeded 25 hours back, so the check at start applies exactly
// one step at whatever half-life it was given (review F4).
func TestStartNeighborEdgeDecayFromConfig(t *testing.T) {
	var nilCfg *Config
	for _, tc := range []struct {
		name         string
		cfg          *Config
		wantHalfLife int
		wantCount    int
		wantClock    bool
	}{
		{"nil config is off", nilCfg, 0, 1000, false},
		{"default is off", &Config{}, 0, 1000, false},
		{"negative is off", &Config{NeighborEdgeCountHalfLifeDays: -1}, 0, 1000, false},
		{"7 days", &Config{NeighborEdgeCountHalfLifeDays: 7}, 7, 906, true},
		{"1 day", &Config{NeighborEdgeCountHalfLifeDays: 1}, 1, 500, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newDecayStore(t)
			insertDecayEdge(t, s, "a", "b", 1000, decayT0.Format(time.RFC3339))
			if err := ensureNeighborEdgeDecayState(s.db); err != nil {
				t.Fatal(err)
			}
			seeded := time.Now().Add(-25 * time.Hour).Unix()
			if _, err := s.db.Exec(`INSERT INTO neighbor_edge_decay_state (id, decayed_through, updated_at) VALUES (1, ?, '')`, seeded); err != nil {
				t.Fatal(err)
			}
			stop, halfLife := startNeighborEdgeDecayFromConfig(s, tc.cfg, time.Hour)
			stop() // returns once the check at start has run
			if halfLife != tc.wantHalfLife {
				t.Errorf("half-life %d, want %d", halfLife, tc.wantHalfLife)
			}
			if got := decayEdgeCount(t, s, "a", "b"); got != tc.wantCount {
				t.Errorf("count %d, want %d", got, tc.wantCount)
			}
			through, ok := decayClock(t, s)
			if ok != tc.wantClock {
				t.Fatalf("clock present = %v, want %v", ok, tc.wantClock)
			}
			if ok && through != seeded+int64(decayDay/time.Second) {
				t.Errorf("clock %d, want one day after the seed", through)
			}
		})
	}
}

// config.example.json is copied as a live config by some deployments, so
// it keeps decay off. It and docs/deployment.md say that decay is opt-in
// and irreversible and point to the replay measurement (review F3).
func TestConfigExampleKeepsNeighborEdgeDecayOff(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.NeighborEdgeCountHalfLifeDaysOrDefault(); got != 0 {
		t.Errorf("config.example.json enables decay (half-life %d days)", got)
	}
	var comments map[string]any
	if err := json.Unmarshal(data, &comments); err != nil {
		t.Fatal(err)
	}
	comment, _ := comments["_comment_neighborEdgeCountHalfLifeDays"].(string)
	docs, err := os.ReadFile(filepath.Join("..", "..", "docs", "deployment.md"))
	if err != nil {
		t.Fatal(err)
	}
	section := string(docs)
	if i := strings.Index(section, "### Neighbour edge count decay"); i >= 0 {
		section = section[i:]
		if j := strings.Index(section[1:], "\n### "); j >= 0 {
			section = section[:j+1]
		}
	} else {
		t.Fatal("docs/deployment.md has no decay section")
	}
	for name, text := range map[string]string{"config.example.json": strings.ToLower(comment), "docs/deployment.md": strings.ToLower(section)} {
		for _, want := range []string{"opt-in", "irreversible", "neighbor-edge-decay-replay.sh"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not say %q about decay", name, want)
			}
		}
	}
}

type decayRecentTraffic struct {
	HalfLifeDays int `json:"halfLifeDays"`
	Days         int `json:"days"`
	Edges        []struct {
		Neighbor string `json:"neighbor"`
		Credits  []struct {
			Day int `json:"day"`
			N   int `json:"n"`
		} `json:"credits"`
		LastSeenHoursAgo int `json:"lastSeenHoursAgo"`
		LifetimeCount    int `json:"lifetimeCount"`
		DecayedCount     int `json:"decayedCount"`
	} `json:"edges"`
}

// Recent traffic can outrank old lifetime credit once decay is on (review
// F1). Both edges are credited day by day through the builder's write
// path; the shared fixture pins the resulting counts, and the server's
// consumer test shows the hop resolver picking A on the lifetime counts
// and B on the decayed ones. With the default configuration decay is off
// and the lifetime counts stay.
func TestNeighborEdgeDecay_RecentTrafficScenario(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "test-fixtures", "neighbor-edge-decay-recent-traffic.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sc decayRecentTraffic
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatal(err)
	}
	if len(sc.Edges) != 2 || sc.Days <= 0 {
		t.Fatalf("scenario: %d edges over %d days", len(sc.Edges), sc.Days)
	}
	simulate := func(halfLifeDays int) *Store {
		s := newDecayStore(t)
		for day := 0; day <= sc.Days; day++ {
			at := decayT0.Add(time.Duration(day) * decayDay)
			var credits []edgeRow
			for _, e := range sc.Edges {
				for _, c := range e.Credits {
					for i := 0; i < c.N && c.Day == day; i++ {
						credits = append(credits, canonEdge(e.Neighbor, "target", at.Add(-time.Hour).Format(time.RFC3339)))
					}
				}
			}
			if len(credits) > 0 {
				if err := s.persistNeighborEdges(credits); err != nil {
					t.Fatal(err)
				}
			}
			mustDecay(t, s, at, halfLifeDays)
		}
		return s
	}
	end := decayT0.Add(time.Duration(sc.Days) * decayDay)
	lifetime := simulate((&Config{}).NeighborEdgeCountHalfLifeDaysOrDefault())
	decayed := simulate(sc.HalfLifeDays)
	for _, e := range sc.Edges {
		a, b := canonEdge(e.Neighbor, "target", "").a, canonEdge(e.Neighbor, "target", "").b
		if got := decayEdgeCount(t, lifetime, a, b); got != e.LifetimeCount {
			t.Errorf("%s with the default config: count %d, want the lifetime %d", e.Neighbor, got, e.LifetimeCount)
		}
		if got := decayEdgeCount(t, decayed, a, b); got != e.DecayedCount {
			t.Errorf("%s at a %d-day half-life: count %d, want %d", e.Neighbor, sc.HalfLifeDays, got, e.DecayedCount)
		}
		for _, s := range []*Store{lifetime, decayed} {
			ls, err := time.Parse(time.RFC3339, decayEdges(t, s)[a+"--"+b].lastSeen)
			if err != nil {
				t.Fatal(err)
			}
			if h := int(end.Sub(ls) / time.Hour); h != e.LastSeenHoursAgo {
				t.Errorf("%s last seen %d h before the end, want %d", e.Neighbor, h, e.LastSeenHoursAgo)
			}
		}
	}
}

// decayDistribution summarises edge counts for the report logs.
func decayDistribution(counts []int) string {
	if len(counts) == 0 {
		return "n=0"
	}
	c := append([]int(nil), counts...)
	sort.Ints(c)
	sum := 0
	for _, v := range c {
		sum += v
	}
	pct := func(p float64) int { return c[int(p*float64(len(c)-1)+0.5)] }
	return fmt.Sprintf("n=%d sum=%d p50=%d p90=%d p99=%d max=%d", len(c), sum, pct(0.5), pct(0.9), pct(0.99), c[len(c)-1])
}

func decayCounts(t *testing.T, s *Store) []int {
	t.Helper()
	var out []int
	for _, e := range decayEdges(t, s) {
		if n, ok := e.count.(int64); ok {
			out = append(out, int(n))
		}
	}
	return out
}

// Synthetic repair measurement (#544). Edges near a TX-publishing bridge
// were credited twice per reception before #540; clean edges once. After
// 90 days of that history, 60 days of RX-only credits follow (the #540
// filter), credited through the builder's own write path, with and
// without decay. Without decay the inflation never leaves; with the 7-day
// half-life it halves every week.
func TestNeighborEdgeDecay_RepairsSyntheticTXInflation(t *testing.T) {
	const historyDays, days = 90, 60
	rates := []int{1, 3, 10, 30, 100} // credits per edge per day
	decayed, control := newDecayStore(t), newDecayStore(t)
	ts := decayT0.Format(time.RFC3339)
	for _, s := range []*Store{decayed, control} {
		for _, r := range rates {
			insertDecayEdge(t, s, fmt.Sprintf("clean-%03d", r), "z", r*historyDays, ts)
			insertDecayEdge(t, s, fmt.Sprintf("inflated-%03d", r), "z", 2*r*historyDays, ts)
		}
	}
	mustDecay(t, decayed, decayT0, 7)
	mustDecay(t, control, decayT0, -1)

	ratio := func(s *Store, r int) float64 {
		return float64(decayEdgeCount(t, s, fmt.Sprintf("inflated-%03d", r), "z")) /
			float64(decayEdgeCount(t, s, fmt.Sprintf("clean-%03d", r), "z"))
	}
	report := []string{"day | rate/day | no decay: clean / inflated (ratio) | decay: clean / inflated (ratio)"}
	logDay := func(d int) {
		for _, r := range rates {
			report = append(report, fmt.Sprintf("%3d | %3d | %d / %d (%.2f) | %d / %d (%.2f)", d, r,
				decayEdgeCount(t, control, fmt.Sprintf("clean-%03d", r), "z"), decayEdgeCount(t, control, fmt.Sprintf("inflated-%03d", r), "z"), ratio(control, r),
				decayEdgeCount(t, decayed, fmt.Sprintf("clean-%03d", r), "z"), decayEdgeCount(t, decayed, fmt.Sprintf("inflated-%03d", r), "z"), ratio(decayed, r)))
		}
	}
	logDay(0)
	for d := 1; d <= days; d++ {
		at := decayT0.Add(time.Duration(d) * decayDay)
		var credits []edgeRow
		for _, r := range rates {
			for i := 0; i < r; i++ {
				credits = append(credits,
					canonEdge(fmt.Sprintf("clean-%03d", r), "z", at.Add(-time.Hour).Format(time.RFC3339)),
					canonEdge(fmt.Sprintf("inflated-%03d", r), "z", at.Add(-time.Hour).Format(time.RFC3339)))
			}
		}
		for _, s := range []*Store{decayed, control} {
			if err := s.persistNeighborEdges(credits); err != nil {
				t.Fatal(err)
			}
		}
		mustDecay(t, decayed, at, 7)
		mustDecay(t, control, at, -1)
		if d == 7 || d == 14 || d == 30 || d == 60 {
			logDay(d)
		}
	}
	t.Log("\n" + strings.Join(report, "\n"))
	for _, r := range rates {
		if got := ratio(control, r); got < 1.5 {
			t.Errorf("rate %d without decay: inflation ratio %.2f, want it to persist (>= 1.5)", r, got)
		}
		if got := ratio(decayed, r); got > 1.05 {
			t.Errorf("rate %d with decay: inflation ratio %.2f after %d days, want <= 1.05", r, got, days)
		}
		// Steady state: a fixed point of c = round((c + r) f) lies within
		// 0.5/(1-f) of the exact r f/(1-f), about 9.6 r +- 5.3, plus what
		// is left of the history after 60 days (0.5^(60/7) of it).
		f := math.Pow(0.5, 1.0/7)
		clean := decayEdgeCount(t, decayed, fmt.Sprintf("clean-%03d", r), "z")
		left := float64(r*historyDays) * math.Pow(0.5, float64(days)/7)
		if want, tol := float64(r)*f/(1-f), 0.5/(1-f)+1+left; math.Abs(float64(clean)-want) > tol {
			t.Errorf("rate %d steady count %d, want %.1f +- %.1f", r, clean, want, tol)
		}
	}
}

// Measurement on a copy of the e2e fixture (#544): decay lowers counts and
// nothing else. Every edge and last_seen stays, so readers that only need
// the edge (reach degree, seenViaPackets, area bridges, the ingestor's
// path resolver) see the same graph, and the hop resolver's minimum of 3
// credits selects the same edges after 1, 7 and 30 days.
func TestNeighborEdgeDecay_E2EFixtureKeepsConsumerThresholds(t *testing.T) {
	src, err := os.Open(filepath.Join("..", "..", "test-fixtures", "e2e-fixture.db"))
	if err != nil {
		t.Skipf("e2e fixture not available: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "fixture.db")
	dst, err := os.Create(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	src.Close()
	dst.Close()
	s, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.WaitForAsyncMigrations()

	before := decayEdges(t, s)
	if len(before) == 0 {
		t.Fatal("fixture has no neighbor_edges")
	}
	above := 0
	for _, e := range before {
		if n, _ := e.count.(int64); n > neighborEdgeDecayFloor {
			above++
		}
	}
	if above == 0 {
		t.Fatal("fixture has no count above the floor; the measurement would be vacuous")
	}
	selected := func(edges map[string]decayEdgeSnapshot, min int64) map[string]bool {
		out := make(map[string]bool)
		for k, e := range edges {
			if n, _ := e.count.(int64); n >= min {
				out[k] = true
			}
		}
		return out
	}
	t.Logf("day 0: %s (%d above the floor)", decayDistribution(decayCounts(t, s)), above)
	mustDecay(t, s, decayT0, 7)
	for _, day := range []int{1, 7, 30} {
		mustDecay(t, s, decayT0.Add(time.Duration(day)*decayDay), 7)
		after := decayEdges(t, s)
		t.Logf("day %d: %s", day, decayDistribution(decayCounts(t, s)))
		if len(after) != len(before) {
			t.Fatalf("day %d: %d edges, want %d", day, len(after), len(before))
		}
		for _, min := range []int64{1, neighborEdgeDecayFloor} {
			b, a := selected(before, min), selected(after, min)
			if len(a) != len(b) {
				t.Errorf("day %d: %d edges with count >= %d, want %d", day, len(a), min, len(b))
			}
			for k := range b {
				if !a[k] {
					t.Errorf("day %d: edge %s fell below %d", day, k, min)
				}
			}
		}
		for k, e := range before {
			if after[k].lastSeen != e.lastSeen {
				t.Errorf("day %d: edge %s last_seen changed", day, k)
			}
		}
	}
}

// BenchmarkNeighborEdgeDecayStep measures one daily step, the write-lock
// hold the ingestor pays once a day, on 100k edges with counts spread over
// 1..5000 (about 99% above the floor, the worst case for rows written).
func BenchmarkNeighborEdgeDecayStep(b *testing.B) {
	s, err := OpenStore(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	s.WaitForAsyncMigrations()
	const edges = 100000
	ts := decayT0.Format(time.RFC3339)
	seed := func() {
		if _, err := s.db.Exec(`DELETE FROM neighbor_edges`); err != nil {
			b.Fatal(err)
		}
		tx, err := s.db.Begin()
		if err != nil {
			b.Fatal(err)
		}
		stmt, err := tx.Prepare(`INSERT INTO neighbor_edges (node_a, node_b, count, last_seen) VALUES (?, ?, ?, ?)`)
		if err != nil {
			b.Fatal(err)
		}
		for i := 0; i < edges; i++ {
			if _, err := stmt.Exec(fmt.Sprintf("%064x", i), fmt.Sprintf("%064x", i+edges), 1+(i*7919)%5000, ts); err != nil {
				b.Fatal(err)
			}
		}
		stmt.Close()
		if err := tx.Commit(); err != nil {
			b.Fatal(err)
		}
	}
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		seed()
		if _, err := s.db.Exec(`DELETE FROM neighbor_edge_decay_state`); err != nil && !strings.Contains(err.Error(), "no such table") {
			b.Fatal(err)
		}
		if _, err := s.DecayNeighborEdgeCounts(decayT0, 7); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		d, err := s.DecayNeighborEdgeCounts(decayT0.Add(decayDay), 7)
		if err != nil || d.Steps != 1 || d.Lowered < edges*98/100 {
			b.Fatalf("step: %+v, err %v", d, err)
		}
	}
}
