package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// Issue #593 (port of upstream PR 2277): a query interrupted partway through
// (lock contention, I/O, a cancelled context) must never be mistaken for "no
// more rows" and cached as if it were a complete, successful read. This file
// covers getAllNodes/getCachedNodesAndPM — the node cache that
// buildNodeInfoMap and computeNodeReach's prefixMap both depend on — which,
// unlike reach_rank.go's scanRankRows/loadDegreeSnapshot, did not check
// rows.Err() after its scan loop and discarded each row's Scan error.
//
// nodesScanFailRows/nodesScanFailConn wrap the real modernc.org/sqlite driver
// (same technique as ping_score_bulk_test.go's countingConn) so the "nodes"
// table query runs for real, but its driver.Rows.Next fails after a chosen
// number of rows — a deterministic stand-in for a connection that drops mid
// scan, rather than one that fails before the query even starts (already
// handled by the existing err != nil checks around s.db.conn.Query).

type nodesScanFailDriver struct{ inner driver.Driver }

func (d *nodesScanFailDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &nodesScanFailConn{Conn: c}, nil
}

type nodesScanFailConn struct{ driver.Conn }

// nodesScanFailAfterRows: -1 disables fault injection; >= 0 makes the
// (n+1)th Next() call on the nodes-table query fail instead of returning a
// row, however many real rows already came back successfully.
var nodesScanFailAfterRows int64 = -1

func setNodesScanFailAfterRows(n int64) { atomic.StoreInt64(&nodesScanFailAfterRows, n) }
func clearNodesScanFailAfterRows()      { atomic.StoreInt64(&nodesScanFailAfterRows, -1) }

// The same two faults for buildNodeInfoMap's observer-enrichment query
// (#593 F3). The match is the exact query text so the fault hits only that
// read: scanReachRows joins the table as "LEFT JOIN observers obs" and
// reach_rank's own roster scan adds a WHERE clause, so both stay healthy
// and the test isolates the enrichment query.
//
//	observersScanFailAfterRows: -1 disables; >= 0 makes the (n+1)th Next() fail.
//	observersQueryFails:         1 makes the query itself fail, before any row.
var (
	observersScanFailAfterRows int64 = -1
	observersQueryFails        int64
)

// observersEnrichQuery must stay identical to buildNodeInfoMap's query text.
const observersEnrichQuery = "SELECT id, name FROM observers"

func setObserversScanFailAfterRows(n int64) { atomic.StoreInt64(&observersScanFailAfterRows, n) }
func setObserversQueryFails()               { atomic.StoreInt64(&observersQueryFails, 1) }
func clearObserversFaults() {
	atomic.StoreInt64(&observersScanFailAfterRows, -1)
	atomic.StoreInt64(&observersQueryFails, 0)
}

func (c *nodesScanFailConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	qc, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	if atomic.LoadInt64(&observersQueryFails) == 1 && query == observersEnrichQuery {
		return nil, fmt.Errorf("injected observers query failure")
	}
	rows, err := qc.QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	if failAfter := atomic.LoadInt64(&nodesScanFailAfterRows); failAfter >= 0 && strings.Contains(query, "FROM nodes") {
		return &nodesScanFailRows{Rows: rows, failAfter: failAfter}, nil
	}
	if failAfter := atomic.LoadInt64(&observersScanFailAfterRows); failAfter >= 0 && query == observersEnrichQuery {
		return &nodesScanFailRows{Rows: rows, failAfter: failAfter}, nil
	}
	return rows, nil
}

type nodesScanFailRows struct {
	driver.Rows
	failAfter int64
	n         int64
}

func (r *nodesScanFailRows) Next(dest []driver.Value) error {
	if r.n >= r.failAfter {
		return fmt.Errorf("injected mid-scan failure after %d rows", r.failAfter)
	}
	err := r.Rows.Next(dest)
	if err == nil {
		r.n++
	}
	return err
}

var registerNodesScanFailDriverOnce sync.Once

func registerNodesScanFailDriver() {
	registerNodesScanFailDriverOnce.Do(func() {
		tmp, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			panic(err)
		}
		realDriver := tmp.Driver()
		tmp.Close()
		sql.Register("sqlite-nodes-scan-fail", &nodesScanFailDriver{inner: realDriver})
	})
}

var nodesScanFailDBNameCounter int64

type nodesScanFailRow struct{ pk, name string }

// newNodesScanFailDB builds an isolated DB (own driver registration, like
// setupPacketPathCountingDB) seeded with the given node rows, on the
// fault-injecting driver above. rows defaults to 3 arbitrary nodes when nil.
func newNodesScanFailDB(t *testing.T, rows []nodesScanFailRow) *DB {
	t.Helper()
	registerNodesScanFailDriver()
	clearNodesScanFailAfterRows()
	clearObserversFaults()
	t.Cleanup(clearNodesScanFailAfterRows)
	t.Cleanup(clearObserversFaults)
	name := fmt.Sprintf("nodesfail%d", atomic.AddInt64(&nodesScanFailDBNameCounter, 1))
	conn, err := sql.Open("sqlite-nodes-scan-fail", "file:"+name+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { conn.Close() })
	stmts := []string{
		`CREATE TABLE nodes (public_key TEXT, name TEXT, role TEXT, lat REAL, lon REAL, last_seen TEXT, first_seen TEXT, advert_count INTEGER)`,
	}
	for _, s := range stmts {
		if _, err := conn.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if rows == nil {
		rows = []nodesScanFailRow{{pk64("aa"), "A"}, {pk64("bb"), "B"}, {pk64("cc"), "C"}}
	}
	for _, r := range rows {
		if _, err := conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count) VALUES (?, ?, 'repeater', 50.0, 5.0, '2026-06-01T00:00:00Z', '2026-06-01T00:00:00Z', 1)`, r.pk, r.name); err != nil {
			t.Fatal(err)
		}
	}
	db := &DB{conn: conn}
	db.isV3Flag.forceTrue()
	return db
}

// TestGetAllNodes_MidScanFailureDoesNotTruncate is the direct, store-level
// reproduction: a connection that fails on its 2nd row (of 3) must make
// getAllNodes report failure, never a silently truncated 1-row list.
func TestGetAllNodes_MidScanFailureDoesNotTruncate(t *testing.T) {
	db := newNodesScanFailDB(t, nil)
	store := &PacketStore{db: db}

	// Baseline: no injected failure, all 3 rows come back.
	nodes, ok := store.getAllNodes()
	if !ok || len(nodes) != 3 {
		t.Fatalf("baseline: ok=%v len=%d, want ok=true len=3", ok, len(nodes))
	}

	setNodesScanFailAfterRows(1) // 1 good row, then the connection drops
	nodes, ok = store.getAllNodes()
	if ok {
		t.Fatalf("mid-scan failure after 1/3 rows: ok=true with %d nodes, want ok=false (a truncated read must not be reported as a complete one)", len(nodes))
	}
	if nodes != nil {
		t.Fatalf("mid-scan failure: got %d nodes, want nil on failure", len(nodes))
	}
}

// TestGetCachedNodesAndPM_MidScanFailureKeepsStaleCompleteSnapshot is the
// caching-layer reproduction: once a complete snapshot is cached, a refresh
// that fails partway through must not replace it with a partial or empty
// one — every consumer (buildNodeInfoMap, Reach's reliableTokens) would
// otherwise silently lose nodes for the rest of the 30s TTL.
func TestGetCachedNodesAndPM_MidScanFailureKeepsStaleCompleteSnapshot(t *testing.T) {
	db := newNodesScanFailDB(t, nil)
	store := &PacketStore{db: db}

	nodes, pm := store.getCachedNodesAndPM()
	if len(nodes) != 3 || pm == nil {
		t.Fatalf("warm-up: got %d nodes, pm=%v, want 3 nodes and a non-nil pm", len(nodes), pm)
	}

	// Expire the cache's TTL and make the next scan fail after 1 of 3 rows.
	store.cacheMu.Lock()
	store.nodeCacheTime = time.Now().Add(-time.Minute)
	store.cacheMu.Unlock()
	setNodesScanFailAfterRows(1)

	nodes2, pm2 := store.getCachedNodesAndPM()
	if len(nodes2) != 3 || pm2 == nil {
		t.Fatalf("after a failed refresh: got %d nodes, pm=%v, want the stale complete snapshot (3 nodes) preserved", len(nodes2), pm2)
	}

	// Recovery: once the connection is healthy again, the next refresh
	// (after the TTL, which a failed refresh deliberately does not bump)
	// replaces the snapshot normally.
	clearNodesScanFailAfterRows()
	store.cacheMu.Lock()
	store.nodeCacheTime = time.Now().Add(-time.Minute)
	store.cacheMu.Unlock()
	nodes3, pm3 := store.getCachedNodesAndPM()
	if len(nodes3) != 3 || pm3 == nil {
		t.Fatalf("after recovery: got %d nodes, pm=%v, want 3", len(nodes3), pm3)
	}
}

// TestNodeReach_NodeCacheRefreshFailureDoesNotCacheADegradedReport is the
// end-to-end reproduction through the Reach HTTP handler: a node-cache
// refresh that fails mid-scan while a Reach request is building its (5
// minute TTL) cached response must not leave that response's attribution
// silently empty — the fix in getCachedNodesAndPM keeps serving the prior
// complete node snapshot, so Reach's own scan (unaffected by this failure)
// still resolves reliable tokens and links correctly.
func TestNodeReach_NodeCacheRefreshFailureDoesNotCacheADegradedReport(t *testing.T) {
	reachDB, target := newReachIntegrationDB(t, `["AABB","01FA","CCDD"]`)
	defer reachDB.conn.Close()

	// A second, fault-injectable connection backs the *shared* node cache
	// (getAllNodes/getCachedNodesAndPM) that computeNodeReach's prefixMap
	// comes from; its pubkeys must match newReachIntegrationDB's target (N)
	// and its two neighbours (A, B) so reliableTokens/attributeDirections
	// can resolve the hop tokens the Reach scan reads from reachDB.
	nodesDB := newNodesScanFailDB(t, []nodesScanFailRow{
		{target, "N"}, {pk64("aabb"), "A"}, {pk64("ccdd"), "B"},
	})
	cfg := &Config{}
	store := newTestStoreWithDB(t, reachDB, cfg)
	store.db = nodesDB // getAllNodes/getCachedNodesAndPM read from the fault-injectable DB
	srv := &Server{store: store, db: reachDB, cfg: cfg, perfStats: NewPerfStats()}
	resetReachState(t, srv)

	// Warm both caches with a healthy request.
	warm := serveReach(srv, "/api/nodes/"+target+"/reach?days=30")
	if warm.Code != http.StatusOK {
		t.Fatalf("warm-up: status=%d body=%s", warm.Code, warm.Body.String())
	}

	// Force the shared node-cache's next refresh to fail mid-scan, expire
	// its TTL, and clear the Reach response cache so the next request is a
	// genuinely cold compute that must consult the (now-failing) node cache.
	store.cacheMu.Lock()
	store.nodeCacheTime = time.Now().Add(-time.Minute)
	store.cacheMu.Unlock()
	setNodesScanFailAfterRows(1)
	clearReachCache(t, srv)

	rr := serveReach(srv, "/api/nodes/"+target+"/reach?days=30")
	if rr.Code != http.StatusOK {
		t.Fatalf("after node-cache refresh failure: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp NodeReachResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v (%s)", err, rr.Body.String())
	}
	// The target's own reliable path-hash token still resolves and both
	// neighbours are still attributed (they come from the stale-but-complete
	// node cache preserved by the fix) — the report is not silently
	// degraded to zero links/observers by the node-cache refresh failure.
	if !contains(resp.ReliableTokens, "01FA") || len(resp.Links) != 2 {
		t.Fatalf("reach report degraded after a node-cache refresh failure: tokens=%v links=%d, want 01FA token and 2 links", resp.ReliableTokens, len(resp.Links))
	}
}

// newReachFailDriverDB builds the full newReachIntegrationDB schema on the
// fault-injecting driver, so one connection backs both the shared node cache
// (getAllNodes) and the Reach request path (scanReachRows, the observer
// enrichment). #593 F2/F3.
func newReachFailDriverDB(t *testing.T, obsPath string) (*DB, string) {
	t.Helper()
	registerNodesScanFailDriver()
	clearNodesScanFailAfterRows()
	clearObserversFaults()
	t.Cleanup(clearNodesScanFailAfterRows)
	t.Cleanup(clearObserversFaults)
	name := fmt.Sprintf("reachfail%d", atomic.AddInt64(&nodesScanFailDBNameCounter, 1))
	conn, err := sql.Open("sqlite-nodes-scan-fail", "file:"+name+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { conn.Close() })
	return seedReachIntegrationDB(t, conn, obsPath)
}

// reachCacheLen reports how many entries the Reach response cache holds, so a
// test can assert a failed build cached nothing at all (rather than only that
// this one request returned an error).
func reachCacheLen(srv *Server) int {
	srv.reach.cacheMu.Lock()
	defer srv.reach.cacheMu.Unlock()
	return len(srv.reach.cache)
}

// TestGetAllNodes_MalformedRowIsSkippedNotFatal — review round 2, F1. A row
// the scan cannot decode (nodes.public_key is a bare TEXT PRIMARY KEY, so
// SQLite permits one NULL; a non-numeric lat/lon is the same class) is a
// malformed row, not an interrupted read. It must be skipped, logged and
// counted the way scanReachRows already discards malformed rows — never turn
// into a permanent ok=false that disables the shared node cache for every
// caller for the lifetime of the process.
func TestGetAllNodes_MalformedRowIsSkippedNotFatal(t *testing.T) {
	db := newNodesScanFailDB(t, nil)
	if _, err := db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count) VALUES (NULL, 'bad', 'repeater', 50.0, 5.0, '2026-06-01T00:00:00Z', '2026-06-01T00:00:00Z', 1)`); err != nil {
		t.Fatal(err)
	}
	store := &PacketStore{db: db}

	nodes, ok := store.getAllNodes()
	if !ok {
		t.Fatalf("one NULL public_key row made getAllNodes report ok=false; a malformed row must be skipped, not fail the whole read")
	}
	if len(nodes) != 3 {
		t.Fatalf("got %d nodes, want the 3 well-formed rows (the NULL-pubkey row skipped)", len(nodes))
	}

	// ...and the cache is actually published, so later callers are cache
	// hits instead of re-running the full nodes SELECT on every request.
	cached, pm := store.getCachedNodesAndPM()
	if len(cached) != 3 || pm == nil {
		t.Fatalf("getCachedNodesAndPM: got %d nodes pm=%v, want 3 nodes and a non-nil pm", len(cached), pm)
	}
	store.cacheMu.Lock()
	published := store.nodeCache != nil && store.nodePM != nil
	store.cacheMu.Unlock()
	if !published {
		t.Fatalf("node cache was not published after a load containing one malformed row — every caller would re-query forever")
	}
	// The well-formed repeaters are resolvable through the published prefix
	// map, i.e. the snapshot is usable and not just non-empty.
	if got := uniqueResolve(pm, "aa"); got != pk64("aa") {
		t.Fatalf("uniqueResolve(\"aa\")=%q, want %q", got, pk64("aa"))
	}
}

// TestGetCachedNodesAndPM_ColdStartFailureReportsNotOKWithUsableEmptyPM —
// review round 2, F2/F4. On a cold start (nothing cached yet) a failed scan
// has no complete snapshot to fall back on. Two things must hold: the
// fallback *prefixMap is non-nil and usable (every caller dereferences it),
// and the failure is reported to callers that would otherwise cache a result
// built on an empty map.
func TestGetCachedNodesAndPM_ColdStartFailureReportsNotOKWithUsableEmptyPM(t *testing.T) {
	db := newNodesScanFailDB(t, nil)
	store := &PacketStore{db: db}
	setNodesScanFailAfterRows(0) // fails before any row, with nothing cached yet

	nodes, pm, ok := store.getCachedNodesAndPMOK()
	if ok {
		t.Fatalf("cold-start scan failure: ok=true, want false (there is no complete snapshot to serve)")
	}
	if len(nodes) != 0 {
		t.Fatalf("cold-start scan failure: got %d nodes, want none", len(nodes))
	}
	if pm == nil {
		t.Fatalf("cold-start scan failure returned a nil *prefixMap; callers dereference it unconditionally")
	}
	// Usable, not merely non-nil: these are the two calls Reach makes.
	if got := uniqueResolve(pm, "aa"); got != "" {
		t.Fatalf("uniqueResolve on the empty fallback map = %q, want \"\"", got)
	}
	if got := pm.without(map[string]bool{pk64("aa"): true}); got == nil {
		t.Fatalf("pm.without returned nil on the empty fallback map")
	}

	// The ok-dropping wrapper keeps its old shape for the many callers that
	// cannot act on a failure.
	nodes2, pm2 := store.getCachedNodesAndPM()
	if len(nodes2) != 0 || pm2 == nil {
		t.Fatalf("getCachedNodesAndPM wrapper: got %d nodes pm=%v, want 0 nodes and a non-nil pm", len(nodes2), pm2)
	}
}

// TestNodeReach_ColdStartNodeCacheFailureIsA500AndNotCached — review round 2,
// F2. With nothing cached yet, a failed nodes scan leaves Reach with an empty
// prefix map. If the target is *also* an observer, buildNodeInfoMap still
// finds it through the observer enrichment, so the build "succeeds" and a
// report with zero reliable tokens and zero links gets cached for the full
// 5-minute TTL — surviving the DB's recovery. The failure must reach the
// handler instead: 500, nothing cached.
func TestNodeReach_ColdStartNodeCacheFailureIsA500AndNotCached(t *testing.T) {
	reachDB, target := newReachFailDriverDB(t, `["AABB","01FA","CCDD"]`)
	// The target is an observer as well as a node — the case where the
	// empty-node-cache failure stays invisible to the Reach build.
	if _, err := reachDB.conn.Exec(`INSERT INTO observers (id, name) VALUES (?, 'N-obs')`, target); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{}
	store := newTestStoreWithDB(t, reachDB, cfg)
	srv := &Server{store: store, db: reachDB, cfg: cfg, perfStats: NewPerfStats()}
	resetReachState(t, srv)

	setNodesScanFailAfterRows(0) // cold start: the very first nodes scan fails

	rr := serveReach(srv, "/api/nodes/"+target+"/reach?days=30")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("cold-start node-cache failure: status=%d body=%s, want 500 (a report built on an empty node cache must not be served or cached)", rr.Code, rr.Body.String())
	}
	if n := reachCacheLen(srv); n != 0 {
		t.Fatalf("reach cache holds %d entries after a failed build, want 0", n)
	}

	// Recovery on the very next request, with no TTL to wait out.
	clearNodesScanFailAfterRows()
	ok := serveReach(srv, "/api/nodes/"+target+"/reach?days=30")
	if ok.Code != http.StatusOK {
		t.Fatalf("after recovery: status=%d body=%s, want 200", ok.Code, ok.Body.String())
	}
	var resp NodeReachResponse
	if err := json.Unmarshal(ok.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v (%s)", err, ok.Body.String())
	}
	if !contains(resp.ReliableTokens, "01FA") || len(resp.Links) != 2 {
		t.Fatalf("after recovery: tokens=%v links=%d, want the 01FA token and 2 links", resp.ReliableTokens, len(resp.Links))
	}
}

// TestNodeReach_ObserverEnrichmentInterruptedIsA500AndNotCached — review
// round 2, F3. buildNodeInfoMap's follow-up "SELECT id, name FROM observers"
// is the closest local analogue of the upstream reachNodeScopes root cause: a
// second query in the same build whose error turns into empty strings. Both
// an outright Query failure and a mid-iteration failure must stop the Reach
// build, not cache a report with observer names silently blanked for 5
// minutes.
func TestNodeReach_ObserverEnrichmentInterruptedIsA500AndNotCached(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inject func()
	}{
		{"query fails", setObserversQueryFails},
		{"fails mid-scan", func() { setObserversScanFailAfterRows(0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reachDB, target := newReachFailDriverDB(t, `["AABB","01FA","CCDD"]`)
			if _, err := reachDB.conn.Exec(`INSERT INTO observers (id, name) VALUES (?, 'OBS-NAMED')`, pk64("eeff")); err != nil {
				t.Fatal(err)
			}
			cfg := &Config{}
			store := newTestStoreWithDB(t, reachDB, cfg)
			srv := &Server{store: store, db: reachDB, cfg: cfg, perfStats: NewPerfStats()}
			resetReachState(t, srv)

			tc.inject()
			rr := serveReach(srv, "/api/nodes/"+target+"/reach?days=30")
			if rr.Code != http.StatusInternalServerError {
				t.Fatalf("interrupted observer enrichment: status=%d body=%s, want 500", rr.Code, rr.Body.String())
			}
			if n := reachCacheLen(srv); n != 0 {
				t.Fatalf("reach cache holds %d entries after a failed build, want 0", n)
			}

			clearObserversFaults()
			ok := serveReach(srv, "/api/nodes/"+target+"/reach?days=30")
			if ok.Code != http.StatusOK {
				t.Fatalf("after recovery: status=%d body=%s, want 200", ok.Code, ok.Body.String())
			}
		})
	}
}
