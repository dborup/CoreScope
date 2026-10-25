package main

import (
	"context"
	"database/sql"
	"go/ast"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshcore-analyzer/channelregistry"
)

// #583: none of regionObsMu, channelProposalService.approvedMu,
// DB.routeMaskStatusMu or Server.neighborBuildMu may be held across SQL.
// With busy_timeout=5000 (#578) a query can wait up to 5 s, and every HTTP
// handler contending on such a mutex would wait with it.
//
// The tests use the real SQL boundary: setupTestDB has exactly one pooled
// connection. Checking it out makes the next query wait in database/sql
// (visible in Stats().WaitCount), with no sleep and no test double.

// sqlHold583 holds the only pooled connection of a setupTestDB database.
type sqlHold583 struct {
	t    *testing.T
	db   *sql.DB
	conn *sql.Conn
	once sync.Once
}

func holdSQL583(t *testing.T, db *sql.DB) *sqlHold583 {
	t.Helper()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h := &sqlHold583{t: t, db: db, conn: conn}
	t.Cleanup(h.release)
	return h
}

func (h *sqlHold583) release() { h.once.Do(func() { h.conn.Close() }) }

// startInSQL runs fn in a goroutine and returns once fn waits for the held
// connection. done is closed when fn returns.
func (h *sqlHold583) startInSQL(fn func()) (done chan struct{}) {
	h.t.Helper()
	waits := h.db.Stats().WaitCount
	done = make(chan struct{})
	go func() { defer close(done); fn() }()
	deadline := time.Now().Add(3 * time.Second)
	for h.db.Stats().WaitCount == waits {
		select {
		case <-done:
			h.t.Fatal("the call returned without reaching SQL")
		default:
		}
		if time.Now().After(deadline) {
			h.t.Fatal("the call never reached the held SQL pool")
		}
		runtime.Gosched()
	}
	return done
}

// mustFinish583 fails unless done closes while SQL is still held. The bound
// only limits how long a failure takes to report: a call that is not
// blocked returns at once.
func mustFinish583(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s waited for SQL running in another goroutine", what)
	}
}

func run583(fn func()) chan struct{} {
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	return done
}

func lockProbe583(mu sync.Locker) chan struct{} {
	return run583(func() { mu.Lock(); mu.Unlock() })
}

func regionIDs583(m map[string]bool) string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// ─── regionObsMu ───────────────────────────────────────────────────────────────

// A cold region's GetObserverIdsForRegion must not hold regionObsMu: a
// request for a cached region, and the mutex itself, stay available.
func TestRegionObsSQLDoesNotHoldRegionObsMu_583(t *testing.T) {
	db := setupTestDB(t)
	seedTestData(t, db)
	store := NewPacketStore(db, nil)
	if got := regionIDs583(store.resolveRegionObservers("SJC")); got != "obs1" {
		t.Fatalf("warm SJC = %q, want obs1", got)
	}

	h := holdSQL583(t, db.conn)
	var cold map[string]bool
	coldDone := h.startInSQL(func() { cold = store.resolveRegionObservers("SFO") })

	var warm map[string]bool
	mustFinish583(t, run583(func() { warm = store.resolveRegionObservers("SJC") }), "a cached region lookup")
	if got := regionIDs583(warm); got != "obs1" {
		t.Fatalf("cached SJC during the SFO query = %q, want obs1", got)
	}
	mustFinish583(t, lockProbe583(&store.regionObsMu), "regionObsMu")

	h.release()
	mustFinish583(t, coldDone, "the cold lookup after SQL was released")
	if got := regionIDs583(cold); got != "obs2" {
		t.Fatalf("cold SFO = %q, want obs2", got)
	}
}

// A failed query (here: no observers table; in production SQLITE_BUSY) is
// not cached: on master it stored nil for 30 s, so the region filter
// silently matched nothing.
func TestRegionObsErrorIsNotCached_583(t *testing.T) {
	db := setupTestDB(t)
	seedTestData(t, db)
	store := NewPacketStore(db, nil)
	mustExec583(t, db, `ALTER TABLE observers RENAME TO observers_away`)
	if got := store.resolveRegionObservers("SJC"); got != nil {
		t.Fatalf("lookup with a failing query = %v, want nil", got)
	}
	mustExec583(t, db, `ALTER TABLE observers_away RENAME TO observers`)
	if got := regionIDs583(store.resolveRegionObservers("SJC")); got != "obs1" {
		t.Fatalf("lookup after the query recovered = %q, want obs1 (the error was cached)", got)
	}
}

// Pin: a successful lookup, empty or not, is still cached for the TTL.
func TestRegionObsSuccessIsCached_583(t *testing.T) {
	db := setupTestDB(t)
	seedTestData(t, db)
	store := NewPacketStore(db, nil)
	if got := store.resolveRegionObservers("ZZZ"); got != nil {
		t.Fatalf("ZZZ = %v, want nil", got)
	}
	store.resolveRegionObservers("SJC")
	mustExec583(t, db, `INSERT INTO observers (id, name, iata) VALUES ('obsZ', 'Z', 'ZZZ'), ('obsS', 'S', 'SJC')`)
	if got := store.resolveRegionObservers("ZZZ"); got != nil {
		t.Fatalf("cached empty ZZZ = %v, want nil until the TTL runs out", got)
	}
	if got := regionIDs583(store.resolveRegionObservers("SJC")); got != "obs1" {
		t.Fatalf("cached SJC = %q, want obs1 until the TTL runs out", got)
	}
	store.regionObsMu.Lock()
	store.regionObsCacheTime = time.Now().Add(-time.Hour)
	store.regionObsMu.Unlock()
	if got := regionIDs583(store.resolveRegionObservers("SJC")); got != "obs1,obsS" {
		t.Fatalf("SJC after the TTL = %q, want obs1,obsS", got)
	}
}

func mustExec583(t *testing.T, db *DB, q string) {
	t.Helper()
	if _, err := db.conn.Exec(q); err != nil {
		t.Fatal(err)
	}
}

// ─── channelProposalService.approvedMu ─────────────────────────────────────────

func approvedNames583(s *approvedSnapshot) string {
	if s == nil {
		return "<nil>"
	}
	names := make([]string, len(s.channels))
	for i, c := range s.channels {
		names[i] = c.Name
	}
	return strings.Join(names, ",")
}

// An expired snapshot is refreshed by one caller; the others are served the
// previous snapshot meanwhile, and a request-status poll (noteDecision)
// does not wait for the refresh either.
func TestApprovedRefreshDoesNotHoldApprovedMu_583(t *testing.T) {
	ps := newProposalServer(t, strongTestKey, enabledConfig(nil), true)
	ps.insert("aaaaaaaaaaaaaaaa", "#A", channelregistry.StatusApproved)
	ctx := context.Background()
	if got := approvedNames583(ps.svc.snapshot(ctx)); got != "#A" {
		t.Fatalf("warm snapshot = %q", got)
	}
	ps.insert("bbbbbbbbbbbbbbbb", "#B", channelregistry.StatusApproved)
	ps.clock = ps.clock.Add(approvedCacheTTL + time.Second)
	// A result that completed before the snapshot was read: polling it
	// takes approvedMu but invalidates nothing.
	reqID := channelregistry.NewID()
	p := &channelregistry.Proposal{ID: "aaaaaaaaaaaaaaaa", Name: "#A", Status: channelregistry.StatusApproved}
	if err := ps.queue.Complete(channelregistry.Result{RequestID: reqID, Status: channelregistry.RequestApproved, Proposal: p, CompletedAt: ps.clock.Add(-time.Hour).UnixMilli()}); err != nil {
		t.Fatal(err)
	}

	h := holdSQL583(t, ps.srv.db.conn)
	var refreshed *approvedSnapshot
	refreshDone := h.startInSQL(func() { refreshed = ps.svc.snapshot(ctx) })

	var stale *approvedSnapshot
	mustFinish583(t, run583(func() { stale = ps.svc.snapshot(ctx) }), "a second snapshot caller")
	if got := approvedNames583(stale); got != "#A" {
		t.Fatalf("snapshot during the refresh = %q, want the previous #A", got)
	}
	var code int
	mustFinish583(t, run583(func() {
		code = ps.do("GET", "/api/channel-proposals/requests/"+reqID, "", "").Code
	}), "GET /api/channel-proposals/requests/{id}")
	if code != 200 {
		t.Fatalf("request status poll = %d", code)
	}
	mustFinish583(t, lockProbe583(&ps.svc.approvedMu), "approvedMu")

	h.release()
	mustFinish583(t, refreshDone, "the refresh after SQL was released")
	if got := approvedNames583(refreshed); got != "#A,#B" {
		t.Fatalf("refreshed snapshot = %q, want #A,#B", got)
	}
	if got := approvedNames583(ps.svc.snapshot(ctx)); got != "#A,#B" {
		t.Fatalf("snapshot after the refresh = %q, want #A,#B (refresh not stored)", got)
	}
}

// A decision that completes while a refresh is reading may not be in what
// it read: noteDecision makes that refresh's result uncached, so the next
// caller reads again, as it would have when the read ran under approvedMu.
func TestApprovedRefreshOverlappingDecisionIsNotCached_583(t *testing.T) {
	ps := newProposalServer(t, strongTestKey, enabledConfig(nil), true)
	ps.insert("aaaaaaaaaaaaaaaa", "#A", channelregistry.StatusApproved)
	ctx := context.Background()
	ps.svc.snapshot(ctx)
	ps.clock = ps.clock.Add(approvedCacheTTL + time.Second)

	h := holdSQL583(t, ps.srv.db.conn)
	refreshDone := h.startInSQL(func() { ps.svc.snapshot(ctx) })
	// Completed after the refresh started reading (the clock is frozen, so
	// one millisecond later is after it).
	decision := channelregistry.RequestStatus{Status: channelregistry.RequestRevoked, CompletedAt: ps.clock.Add(time.Millisecond).UnixMilli()}
	mustFinish583(t, run583(func() { ps.svc.noteDecision(decision) }), "noteDecision")
	h.release()
	mustFinish583(t, refreshDone, "the refresh after SQL was released")

	ps.svc.approvedMu.Lock()
	cached := ps.svc.approved
	ps.svc.approvedMu.Unlock()
	if cached != nil {
		t.Fatalf("a refresh that overlapped a newer decision was cached: %q", approvedNames583(cached))
	}
}

// Pin: concurrent callers with no snapshot at all share one refresh and
// all get its result (nothing stale to serve).
func TestApprovedColdCallersShareTheRefresh_583(t *testing.T) {
	ps := newProposalServer(t, strongTestKey, enabledConfig(nil), true)
	ps.insert("aaaaaaaaaaaaaaaa", "#A", channelregistry.StatusApproved)
	ctx := context.Background()
	h := holdSQL583(t, ps.srv.db.conn)
	var first, second *approvedSnapshot
	firstDone := h.startInSQL(func() { first = ps.svc.snapshot(ctx) })
	secondDone := run583(func() { second = ps.svc.snapshot(ctx) })
	h.release()
	mustFinish583(t, firstDone, "the first cold caller")
	mustFinish583(t, secondDone, "the second cold caller")
	if approvedNames583(first) != "#A" || approvedNames583(second) != "#A" {
		t.Fatalf("cold callers got %q and %q, want #A both", approvedNames583(first), approvedNames583(second))
	}
}

// ─── DB.routeMaskStatusMu (/api/healthz) ───────────────────────────────────────

// An expired route-mask status is refreshed by one caller; the others are
// served the previous status meanwhile.
func TestRouteMaskStatusRefreshDoesNotHoldMutex_583(t *testing.T) {
	db := rmIndexedDB(t)
	routeMaskStatusTTL = time.Hour
	if st := db.routeMaskBackfillStatus(); st.Remaining == nil || *st.Remaining != 1 {
		t.Fatalf("warm status = %+v, want remaining=1", st)
	}
	rmExec(t, db, `UPDATE transmissions SET route_mask = 6 WHERE route_mask IS NULL`)
	expireRouteMaskStatus583(db)

	h := holdSQL583(t, db.conn)
	var refreshed RouteMaskBackfillStatus
	refreshDone := h.startInSQL(func() { refreshed = db.routeMaskBackfillStatus() })

	var stale RouteMaskBackfillStatus
	mustFinish583(t, run583(func() { stale = db.routeMaskBackfillStatus() }), "a second status caller")
	if stale.Remaining == nil || *stale.Remaining != 1 {
		t.Fatalf("status during the refresh = %+v, want the previous remaining=1", stale)
	}
	mustFinish583(t, lockProbe583(&db.routeMaskStatusMu), "routeMaskStatusMu")

	h.release()
	mustFinish583(t, refreshDone, "the refresh after SQL was released")
	if refreshed.Status != "complete" {
		t.Fatalf("refreshed status = %+v, want complete", refreshed)
	}
	if st := db.routeMaskBackfillStatus(); st.Status != "complete" {
		t.Fatalf("status after the refresh = %+v, want complete (refresh not stored)", st)
	}
}

// A failed status query keeps the last good status and is not cached: on
// master it cached "pending" for the TTL.
func TestRouteMaskStatusErrorIsNotCached_583(t *testing.T) {
	db := rmIndexedDB(t)
	routeMaskStatusTTL = time.Hour
	good := db.routeMaskBackfillStatus()
	if good.Remaining == nil || *good.Remaining != 1 {
		t.Fatalf("warm status = %+v, want remaining=1", good)
	}
	expireRouteMaskStatus583(db)
	rmExec(t, db, `ALTER TABLE transmissions RENAME TO transmissions_away`)
	if st := db.routeMaskBackfillStatus(); st.Status != good.Status || st.Remaining == nil || *st.Remaining != 1 {
		t.Fatalf("status with a failing query = %+v, want the last good %+v", st, good)
	}
	rmExec(t, db, `ALTER TABLE transmissions_away RENAME TO transmissions`)
	rmExec(t, db, `UPDATE transmissions SET route_mask = 6 WHERE route_mask IS NULL`)
	if st := db.routeMaskBackfillStatus(); st.Status != "complete" {
		t.Fatalf("status after the query recovered = %+v, want complete (the error was cached)", st)
	}
}

// With no status yet, a failed query reports pending, uncached.
func TestRouteMaskStatusColdErrorIsPendingUncached_583(t *testing.T) {
	db := rmIndexedDB(t)
	routeMaskStatusTTL = time.Hour
	rmExec(t, db, `ALTER TABLE transmissions RENAME TO transmissions_away`)
	if st := db.routeMaskBackfillStatus(); st.Status != "pending" || st.Remaining != nil {
		t.Fatalf("cold status with a failing query = %+v, want pending", st)
	}
	rmExec(t, db, `ALTER TABLE transmissions_away RENAME TO transmissions`)
	if st := db.routeMaskBackfillStatus(); st.Remaining == nil || *st.Remaining != 1 {
		t.Fatalf("status after the query recovered = %+v, want remaining=1", st)
	}
}

func expireRouteMaskStatus583(db *DB) {
	db.routeMaskStatusMu.Lock()
	db.routeMaskStatusExp = time.Now().Add(-time.Second)
	db.routeMaskStatusMu.Unlock()
}

// ─── Server.neighborBuildMu (cold path) ────────────────────────────────────────

// The first graph build reads the node list from SQL (getCachedNodesAndPM).
// That read must happen before neighborBuildMu: a background rebuild or
// another cold request would otherwise wait behind the query.
func TestNeighborColdBuildSQLOutsideBuildMu_583(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.store.InvalidateNodeCache()

	h := holdSQL583(t, srv.db.conn)
	var g *NeighborGraph
	coldDone := h.startInSQL(func() { g = srv.getNeighborGraph() })
	mustFinish583(t, lockProbe583(&srv.neighborBuildMu), "neighborBuildMu (cold getNeighborGraph)")

	h.release()
	mustFinish583(t, coldDone, "the cold build after SQL was released")
	if g == nil || srv.neighborGraph.Load() != g {
		t.Fatal("the cold build did not publish its graph")
	}
}

// Same for the background rebuild: its node read is outside the mutex.
func TestNeighborRebuildSQLOutsideBuildMu_583(t *testing.T) {
	srv, _ := setupTestServer(t)
	old := NewNeighborGraph()
	srv.neighborGraph.Store(old)
	srv.store.InvalidateNodeCache()

	h := holdSQL583(t, srv.db.conn)
	rebuildDone := h.startInSQL(srv.rebuildNeighborGraph)
	mustFinish583(t, lockProbe583(&srv.neighborBuildMu), "neighborBuildMu (rebuildNeighborGraph)")

	h.release()
	mustFinish583(t, rebuildDone, "the rebuild after SQL was released")
	if got := srv.neighborGraph.Load(); got == old || got == nil {
		t.Fatal("the rebuild did not publish a new graph")
	}
}

// ─── Static guard ──────────────────────────────────────────────────────────────

// isNonStoreMu583 matches the #583 mutexes the #412 scanner can follow:
// their SQL is reached through package functions and DB methods.
// channelProposalService.approvedMu queries through the channelregistry
// package, which the scanner does not resolve; the runtime tests above
// cover it.
func isNonStoreMu583(x ast.Expr, recv string) bool {
	switch lockScanExpr(x) {
	case "s.regionObsMu":
		return recv == "PacketStore"
	case "db.routeMaskStatusMu":
		return recv == "DB"
	case "s.neighborBuildMu":
		return recv == "Server"
	}
	return false
}

func TestNoSQLUnderNonStoreMutexes_583(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	srcs := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		srcs[f] = string(b)
	}
	hits, regions := findSQLUnderLock(t, srcs, isNonStoreMu583)
	// Four on master (one per lock site), seven since #583 split them.
	if regions < 4 {
		t.Fatalf("found only %d sections of the #583 mutexes; the scan no longer recognises the locking code", regions)
	}
	for _, h := range hits {
		t.Errorf("SQL under a #583 mutex: %s", h)
	}
}

// The guard must flag a query under one of these mutexes and ignore one
// after its unlock (the #412 scanner used to recognise only an unlock of a
// lower-case "...mu", so a camel-case mutex's section ran to the block end).
func TestNoSQLUnderNonStoreMutexesDetects_583(t *testing.T) {
	src := `package main
type PacketStore struct{}
type DB struct{}
func (db *DB) GetThing() {}
func (s *PacketStore) Under() {
	s.regionObsMu.Lock()
	s.db.GetThing()
	s.regionObsMu.Unlock()
}
func (s *PacketStore) After() {
	s.regionObsMu.Lock()
	x := 1
	s.regionObsMu.Unlock()
	s.db.GetThing()
	_ = x
}
`
	hits, regions := findSQLUnderLock(t, map[string]string{"x.go": src}, isNonStoreMu583)
	got := strings.Join(hits, "\n")
	if regions != 2 || !strings.Contains(got, "PacketStore.Under calls DB.GetThing") {
		t.Errorf("scan missed the query under regionObsMu (%d sections); hits:\n%s", regions, got)
	}
	if strings.Contains(got, "PacketStore.After") {
		t.Errorf("scan flagged a query after the unlock; hits:\n%s", got)
	}
}
