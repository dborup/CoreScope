package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dbschema"
)

// Fork adaptation of `Kpa-clawbot/CoreScope#2072`/`#2074` (issue #100): the
// one-off ANALYZE and the #89 route_mask backfill's pending-index build are
// both long holds of the single writer. startPlannerStatsThenRouteMaskBackfill
// runs them one after the other with an ingest-buffer drain in front of each,
// so the ingest pause is never the sum of the two (or of either and the startup
// backlog).

// syncLogBuffer is a log sink that is safe to read while goroutines write.
type syncLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func capturePlannerLog(t *testing.T) *syncLogBuffer {
	t.Helper()
	buf := &syncLogBuffer{}
	orig := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(orig) })
	return buf
}

func shrinkPlannerDrainWait(t *testing.T, timeout time.Duration) {
	t.Helper()
	oldTimeout, oldPoll := plannerStatsDrainTimeout, plannerStatsDrainPoll
	plannerStatsDrainTimeout, plannerStatsDrainPoll = timeout, time.Millisecond
	t.Cleanup(func() { plannerStatsDrainTimeout, plannerStatsDrainPoll = oldTimeout, oldPoll })
}

// plannerStartupStore returns a store holding legacy rows (route_mask NULL,
// so the backfill has real work) and no sqlite_stat1.
func plannerStartupStore(t *testing.T) *Store {
	t.Helper()
	routeMaskShrinkBackfill(t, 2)
	s := routeMaskStore(t, filepath.Join(t.TempDir(), "startup.db"))
	for i := 0; i < 6; i++ {
		routeMaskInsert(t, s, routeMaskObs{firstIngestedFloodRaw, "obs-a", fmt.Sprintf("2026-09-24T10:%02d:00Z", i)})
		routeMaskInsertRaw(t, s, fmt.Sprintf("legacy-%d", i), 1)
	}
	routeMaskMakeLegacy(t, s)
	if hasStat1(t, s) {
		t.Fatal("setup: the store already carries sqlite_stat1")
	}
	return s
}

func plannerIngestPacket(i int) *PacketData {
	return &PacketData{
		RawHex:      "1102a1b2",
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		ObserverID:  "obs-a",
		Hash:        fmt.Sprintf("live-%06d", i),
		RouteType:   1,
		PayloadType: 5,
		ChannelHash: fmt.Sprintf("ch-%d", i%3),
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not finish within 30s (deadlock on the single writer?)", what)
	}
}

func waitAsyncMigrations(t *testing.T, s *Store) {
	t.Helper()
	done := make(chan struct{})
	go func() { s.WaitForAsyncMigrations(); close(done) }()
	waitClosed(t, done, "the route_mask backfill")
}

func countTx(t *testing.T, s *Store, where string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM transmissions WHERE ` + where).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Both one-off startup writers run against live ingest through the real
// IngestBuffer: no deadlock, statistics built, backfill complete, every
// buffered packet written, and ingest still writing afterwards.
func TestPlannerStatsAndRouteMaskBackfillAtStartup_Issue100(t *testing.T) {
	shrinkPlannerDrainWait(t, 10*time.Second)
	s := plannerStartupStore(t)
	defer s.Close()
	if countTx(t, s, "route_mask IS NULL") == 0 {
		t.Fatal("setup: no legacy rows for the backfill")
	}

	buf := NewIngestBuffer(1000)
	buf.Start()
	defer buf.Stop()
	var written, failed atomic.Int64
	submit := func(i int) {
		pd := plannerIngestPacket(i)
		buf.Submit(func() {
			if _, err := s.InsertTransmission(pd); err != nil {
				failed.Add(1)
				return
			}
			written.Add(1)
		})
	}
	// A startup backlog, as the subscription would have buffered it.
	for i := 0; i < 50; i++ {
		submit(i)
	}
	buf.Ready()

	stopFeed := make(chan struct{})
	feedDone := make(chan struct{})
	var fed atomic.Int64
	fed.Store(50)
	go func() {
		defer close(feedDone)
		// Bounded: live traffic is ~130-600 packets a minute; this is far
		// denser, but finite, so the buffer is never the thing under test.
		for i := 50; i < 350; i++ {
			select {
			case <-stopFeed:
				return
			case <-time.After(2 * time.Millisecond):
				submit(i)
				fed.Add(1)
			}
		}
	}()

	scheduled := s.startPlannerStatsThenRouteMaskBackfill(context.Background(), 10000, buf.Pending)
	waitClosed(t, scheduled, "EnsurePlannerStats + scheduling the backfill")
	waitAsyncMigrations(t, s)
	close(stopFeed)
	<-feedDone

	if !hasStat1(t, s) {
		t.Error("sqlite_stat1 absent: EnsurePlannerStats did not run at startup")
	}
	if st, err := s.AsyncMigrationStatus(dbschema.RouteMaskBackfillMigration); err != nil || st != "done" {
		t.Errorf("route_mask backfill status = %q, %v; want done", st, err)
	}
	if n := countTx(t, s, "route_mask IS NULL"); n != 0 {
		t.Errorf("%d rows still without route_mask after the backfill", n)
	}

	// Every packet fed so far must land, and a packet submitted after both
	// startup writers must land too.
	deadline := time.Now().Add(10 * time.Second)
	for written.Load()+failed.Load() < fed.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if failed.Load() > 0 || buf.Dropped() > 0 {
		t.Fatalf("ingest lost packets: failed=%d dropped=%d", failed.Load(), buf.Dropped())
	}
	if got, want := written.Load(), fed.Load(); got != want {
		t.Fatalf("ingest wrote %d of %d packets", got, want)
	}
	after := plannerIngestPacket(999999)
	ok := make(chan struct{})
	buf.Submit(func() {
		if _, err := s.InsertTransmission(after); err == nil {
			close(ok)
		}
	})
	waitClosed(t, ok, "an insert after both startup writers")
	if countTx(t, s, "hash = 'live-999999'") != 1 {
		t.Error("the post-startup packet is not in transmissions")
	}
}

// The order this fork chose: drain, ANALYZE, drain, backfill. Swapping the
// two writers, or dropping either drain, fails here.
func TestPlannerStatsBeforeRouteMaskBackfillWithDrains_Issue100(t *testing.T) {
	shrinkPlannerDrainWait(t, 10*time.Second)
	s := plannerStartupStore(t)
	defer s.Close()
	logs := capturePlannerLog(t)

	// Stand-in for IngestBuffer.Pending: reports a backlog for a few polls,
	// then empty, and logs each time it reports empty so the order is visible.
	var polls atomic.Int64
	pending := func() int {
		n := polls.Add(1)
		if n%4 != 0 {
			return 3
		}
		log.Printf("[test] buffer drained (poll %d)", n)
		return 0
	}

	waitClosed(t, s.startPlannerStatsThenRouteMaskBackfill(context.Background(), 10000, pending), "startup sequence")
	waitAsyncMigrations(t, s)

	out := logs.String()
	marks := []string{
		"[test] buffer drained (poll 4)",
		"no planner statistics",
		"planner statistics built in",
		"[test] buffer drained (poll 8)",
		fmt.Sprintf("%q starting", dbschema.RouteMaskBackfillMigration),
		"[route-mask] pending index ready",
	}
	last := -1
	for _, m := range marks {
		i := strings.Index(out, m)
		if i == -1 {
			t.Fatalf("log line %q missing; got:\n%s", m, out)
		}
		if i < last {
			t.Fatalf("log line %q is out of order; want %q in that order; got:\n%s", m, marks, out)
		}
		last = i
	}
}

// A restart: sqlite_stat1 is already there, so no ANALYZE at startup. The
// sentinel row would be rewritten by any ANALYZE, so it is what proves it.
func TestStartupSkipsAnalyzeWhenStatsExist_Issue100(t *testing.T) {
	shrinkPlannerDrainWait(t, 10*time.Second)
	s := plannerStartupStore(t)
	defer s.Close()
	s.RefreshPlannerStats(10000)
	if _, err := s.db.Exec(`UPDATE sqlite_stat1 SET stat = 'sentinel' WHERE tbl = 'transmissions'`); err != nil {
		t.Fatal(err)
	}
	logs := capturePlannerLog(t)

	waitClosed(t, s.startPlannerStatsThenRouteMaskBackfill(context.Background(), 10000, func() int { return 0 }), "startup sequence")
	waitAsyncMigrations(t, s)

	var sentinels int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_stat1 WHERE stat = 'sentinel'`).Scan(&sentinels); err != nil {
		t.Fatal(err)
	}
	if sentinels == 0 {
		t.Error("the startup ran ANALYZE on a database that already had planner statistics")
	}
	if out := logs.String(); strings.Contains(out, "no planner statistics") || strings.Contains(out, "planner statistics built") {
		t.Errorf("startup logged an ANALYZE on a database with statistics:\n%s", out)
	}
	if st, _ := s.AsyncMigrationStatus(dbschema.RouteMaskBackfillMigration); st != "done" {
		t.Errorf("route_mask backfill status = %q, want done (it must still run on a restart)", st)
	}
}

// analysis_limit < 0 turns off both the startup build and the routine
// refresh, and nothing else: the backfill still runs.
func TestNegativeAnalysisLimitDisablesBothStartupAndRefresh_Issue100(t *testing.T) {
	shrinkPlannerDrainWait(t, 10*time.Second)
	s := plannerStartupStore(t)
	defer s.Close()
	logs := capturePlannerLog(t)

	scheduled := s.startPlannerStatsThenRouteMaskBackfill(context.Background(), -1, func() int { return 0 })
	stop, enabled := s.schedulePlannerStatsRefresh(-1, scheduled, time.Millisecond, time.Millisecond)
	defer stop()
	if enabled {
		t.Error("schedulePlannerStatsRefresh reported the refresh enabled at analysis_limit=-1")
	}
	waitClosed(t, scheduled, "startup sequence")
	waitAsyncMigrations(t, s)
	time.Sleep(50 * time.Millisecond) // many refresh intervals, had it been scheduled

	if hasStat1(t, s) {
		t.Error("sqlite_stat1 built although analysis_limit < 0")
	}
	if out := logs.String(); strings.Contains(out, "no planner statistics") {
		t.Errorf("warned about an ANALYZE that is disabled:\n%s", out)
	}
	if st, _ := s.AsyncMigrationStatus(dbschema.RouteMaskBackfillMigration); st != "done" {
		t.Errorf("route_mask backfill status = %q, want done: disabling ANALYZE must not disable the backfill", st)
	}
}

// The routine refresh starts only once the startup sequence has handed over,
// then runs on its interval.
func TestPlannerStatsRefreshWaitsForStartupThenTicks_Issue100(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	logs := capturePlannerLog(t)

	after := make(chan struct{})
	stop, enabled := s.schedulePlannerStatsRefresh(10000, after, time.Millisecond, 5*time.Millisecond)
	defer stop()
	if !enabled {
		t.Fatal("refresh not scheduled at a positive limit")
	}
	time.Sleep(30 * time.Millisecond)
	if hasStat1(t, s) {
		t.Fatal("the routine refresh ran before the startup sequence handed over")
	}
	close(after)
	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(logs.String(), "[analyze] planner statistics") < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := strings.Count(logs.String(), "[analyze] planner statistics"); n < 2 {
		t.Fatalf("want the staggered refresh and at least one tick, got %d refresh lines:\n%s", n, logs.String())
	}
}

// A drain that never happens (ingest faster than the writer) must not hold
// the backfill back forever.
func TestStartupDrainWaitIsBounded_Issue100(t *testing.T) {
	shrinkPlannerDrainWait(t, 20*time.Millisecond)
	s := plannerStartupStore(t)
	defer s.Close()

	waitClosed(t, s.startPlannerStatsThenRouteMaskBackfill(context.Background(), 10000, func() int { return 1 }), "startup sequence with a buffer that never drains")
	waitAsyncMigrations(t, s)
	if st, _ := s.AsyncMigrationStatus(dbschema.RouteMaskBackfillMigration); st != "done" {
		t.Errorf("route_mask backfill status = %q, want done", st)
	}
}

// Shutdown during the startup ANALYZE: the backfill is not started on a
// cancelled context (it resumes on the next start, as before). The stand-in
// for IngestBuffer.Pending cancels once the statistics exist, i.e. between
// the two writers, which is the window the ctx check after the second drain
// covers.
func TestStartupSequenceStopsOnCancel_Issue100(t *testing.T) {
	shrinkPlannerDrainWait(t, time.Hour)
	s := plannerStartupStore(t)
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pending := func() int {
		var n int
		s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'sqlite_stat1'`).Scan(&n)
		if n == 0 {
			return 0 // first drain: nothing buffered, go on to ANALYZE
		}
		cancel() // shutdown arrives while the ANALYZE's backlog drains
		return 1
	}
	waitClosed(t, s.startPlannerStatsThenRouteMaskBackfill(ctx, 10000, pending), "startup sequence after cancel")
	waitAsyncMigrations(t, s)
	if !hasStat1(t, s) {
		t.Fatal("setup: the ANALYZE did not run before the cancel")
	}
	if st, err := s.AsyncMigrationStatus(dbschema.RouteMaskBackfillMigration); err != sql.ErrNoRows {
		t.Errorf("the backfill was scheduled after shutdown was requested (status %q, err %v)", st, err)
	}
}

// The routine refresh must rewrite statistics that already exist; that is its
// whole job after the first start. ANALYZE does. PRAGMA optimize, which
// upstream's first draft used, would leave them alone here: it only
// re-analyzes a table whose row count has moved a lot. (Upstream's
// TestRefreshPlannerStatsWritesStatistics_Issue2058 guards the same mistake
// on SQLite 3.45.1, where optimize wrote nothing on a fresh database; the
// SQLite 3.46.0 bundled with modernc.org/sqlite here does analyze a
// never-analyzed table from optimize, so that test alone no longer catches it.)
func TestRefreshPlannerStatsRewritesExistingStats_Issue100(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	for i := 0; i < 20; i++ {
		s.InsertTransmission(plannerIngestPacket(i))
	}
	if !s.RefreshPlannerStats(10000) {
		t.Fatal("first refresh failed")
	}
	if _, err := s.db.Exec(`UPDATE sqlite_stat1 SET stat = 'sentinel' WHERE tbl = 'transmissions'`); err != nil {
		t.Fatal(err)
	}
	if !s.RefreshPlannerStats(10000) {
		t.Fatal("second refresh failed")
	}
	var sentinels int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_stat1 WHERE stat = 'sentinel'`).Scan(&sentinels); err != nil {
		t.Fatal(err)
	}
	if sentinels != 0 {
		t.Errorf("%d stale sqlite_stat1 rows survived a refresh; the refresh does not re-analyze", sentinels)
	}
}

// hasPlannerStats gates a write, so its error direction is pinned: a failing
// query reads as "no statistics" (one unneeded ANALYZE), never as "has
// statistics" (a database left without them).
func TestHasPlannerStatsErrorReadsAsAbsent_Issue100(t *testing.T) {
	s := newTestStore(t)
	s.RefreshPlannerStats(10000)
	if !s.hasPlannerStats() {
		t.Fatal("hasPlannerStats false right after a refresh")
	}
	s.WaitForAsyncMigrations()
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	if s.hasPlannerStats() {
		t.Error("hasPlannerStats read a failed query as \"statistics present\"")
	}
}

// Upstream's EnsurePlannerStats logged its "building them now, ingest will
// pause" warning even at a negative limit, where nothing is built.
func TestEnsurePlannerStatsDisabledIsSilent_Issue100(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	logs := capturePlannerLog(t)
	if s.EnsurePlannerStats(-1) {
		t.Error("EnsurePlannerStats(-1) reported a build")
	}
	if out := logs.String(); strings.Contains(out, "no planner statistics") {
		t.Errorf("warned about an ANALYZE that is disabled:\n%s", out)
	}
}
