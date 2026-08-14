package main

import (
	"bytes"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// captureNeighborGraphRejectLog redirects the global logger to a buffer for
// the life of the test (restored via t.Cleanup, matching this package's
// existing log.SetOutput test convention — see schema_degradation_per_store_test.go)
// and returns a function that extracts just the
// "[neighbor-graph] reject geo-far edge" lines, so incidental log output
// from elsewhere in the build doesn't make callers flaky.
//
// Safe under concurrent log.Printf callers: the standard logger serializes
// each Output() call (format + single Write) behind its own mutex, so a
// plain bytes.Buffer never sees overlapping writes.
func captureNeighborGraphRejectLog(t *testing.T) func() []string {
	t.Helper()
	var buf bytes.Buffer
	prevOut := log.Writer()
	prevFlags := log.Flags()
	prevPrefix := log.Prefix()
	log.SetOutput(&buf)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
		log.SetPrefix(prevPrefix)
	})
	return func() []string {
		var lines []string
		for _, l := range strings.Split(buf.String(), "\n") {
			if strings.Contains(l, "[neighbor-graph] reject geo-far edge") {
				lines = append(lines, l)
			}
		}
		return lines
	}
}

// TestBuildNeighborGraph_RejectsGeoFarEdge — RED test for #1228.
//
// Synthetic advert produces an edge between A (Bay Area) and B (Berlin).
// Distance ≈ 9 100 km, well above any plausible terrestrial LoRa hop.
// The geo-sanity filter must reject the edge at build time so the
// affinity graph cannot self-reinforce a wrong disambiguation.
func TestBuildNeighborGraph_RejectsGeoFarEdge(t *testing.T) {
	nodes := []nodeInfo{
		// A: San Francisco
		{Role: "repeater", PublicKey: "aaaa1111", Name: "A_SF", Lat: 37.77, Lon: -122.41, HasGPS: true},
		// B: Berlin
		{Role: "repeater", PublicKey: "bbbb2222", Name: "B_BE", Lat: 52.52, Lon: 13.40, HasGPS: true},
		// Observer with GPS at SF (won't affect A↔B edge under test)
		{Role: "repeater", PublicKey: "obs00001", Name: "Observer", Lat: 37.77, Lon: -122.41, HasGPS: true},
	}
	// ADVERT originated by A, path=["bbbb"] → builder creates edge A↔B
	// (originator ↔ path[0]). With geo sanity ON this edge must be dropped.
	tx := ngMakeTx(1, 4, ngFromNodeJSON("aaaa1111"), []*StoreObs{
		ngMakeObs("obs00001", `["bbbb"]`, nowStr, ngFloatPtr(-10)),
	})
	store := ngTestStore(nodes, []*StoreTx{tx})
	g := BuildFromStore(store)

	for _, e := range g.AllEdges() {
		if (e.NodeA == "aaaa1111" && e.NodeB == "bbbb2222") ||
			(e.NodeA == "bbbb2222" && e.NodeB == "aaaa1111") {
			t.Fatalf("geo-implausible edge A(SF)↔B(Berlin) was not rejected: %+v", e)
		}
	}
}

// TestBuildNeighborGraph_AcceptsLocalEdge — A↔C within plausible LoRa range
// (both in CA, ~100 km apart) must remain in the graph.
func TestBuildNeighborGraph_AcceptsLocalEdge(t *testing.T) {
	nodes := []nodeInfo{
		{Role: "repeater", PublicKey: "aaaa1111", Name: "A_SF", Lat: 37.77, Lon: -122.41, HasGPS: true},
		{Role: "repeater", PublicKey: "cccc3333", Name: "C_SJ", Lat: 37.34, Lon: -121.89, HasGPS: true},
		{Role: "repeater", PublicKey: "obs00001", Name: "Observer", Lat: 37.77, Lon: -122.41, HasGPS: true},
	}
	tx := ngMakeTx(1, 4, ngFromNodeJSON("aaaa1111"), []*StoreObs{
		ngMakeObs("obs00001", `["cccc"]`, nowStr, ngFloatPtr(-10)),
	})
	store := ngTestStore(nodes, []*StoreTx{tx})
	g := BuildFromStore(store)

	found := false
	for _, e := range g.AllEdges() {
		if (e.NodeA == "aaaa1111" && e.NodeB == "cccc3333") ||
			(e.NodeA == "cccc3333" && e.NodeB == "aaaa1111") {
			found = true
		}
	}
	if !found {
		t.Fatalf("local A↔C edge (~50km) must be accepted")
	}
}

// TestBuildNeighborGraph_AcceptsEdgeWhenNoGPS — if either endpoint lacks GPS,
// we have no signal to reject; the edge is accepted.
func TestBuildNeighborGraph_AcceptsEdgeWhenNoGPS(t *testing.T) {
	nodes := []nodeInfo{
		// A has GPS (Berlin)
		{Role: "repeater", PublicKey: "aaaa1111", Name: "A", Lat: 52.52, Lon: 13.40, HasGPS: true},
		// D has no GPS
		{Role: "repeater", PublicKey: "dddd4444", Name: "D"}, // HasGPS = false
		{Role: "repeater", PublicKey: "obs00001", Name: "Observer"},
	}
	tx := ngMakeTx(1, 4, ngFromNodeJSON("aaaa1111"), []*StoreObs{
		ngMakeObs("obs00001", `["dddd"]`, nowStr, nil),
	})
	store := ngTestStore(nodes, []*StoreTx{tx})
	g := BuildFromStore(store)

	found := false
	for _, e := range g.AllEdges() {
		if (e.NodeA == "aaaa1111" && e.NodeB == "dddd4444") ||
			(e.NodeA == "dddd4444" && e.NodeB == "aaaa1111") {
			found = true
		}
	}
	if !found {
		t.Fatalf("A(GPS)↔D(no-GPS) edge must be accepted (no signal to reject)")
	}
}

// TestBuildNeighborGraph_RejectedCounterIncrements — every dropped edge bumps
// the atomic counter surfaced by /api/analytics/neighbor-graph stats.
func TestBuildNeighborGraph_RejectedCounterIncrements(t *testing.T) {
	nodes := []nodeInfo{
		{Role: "repeater", PublicKey: "aaaa1111", Name: "A_SF", Lat: 37.77, Lon: -122.41, HasGPS: true},
		{Role: "repeater", PublicKey: "bbbb2222", Name: "B_BE", Lat: 52.52, Lon: 13.40, HasGPS: true},
		{Role: "repeater", PublicKey: "obs00001", Name: "Observer", Lat: 37.77, Lon: -122.41, HasGPS: true},
	}
	// Two adverts each producing the far A↔B edge attempt → counter ≥ 2.
	txs := []*StoreTx{
		ngMakeTx(1, 4, ngFromNodeJSON("aaaa1111"), []*StoreObs{
			ngMakeObs("obs00001", `["bbbb"]`, nowStr, nil),
		}),
		ngMakeTx(2, 4, ngFromNodeJSON("aaaa1111"), []*StoreObs{
			ngMakeObs("obs00001", `["bbbb"]`, nowStr, nil),
		}),
	}
	store := ngTestStore(nodes, txs)
	g := BuildFromStore(store)
	got := atomic.LoadUint64(&g.RejectedEdgesGeoFar)
	if got < 2 {
		t.Fatalf("RejectedEdgesGeoFar = %d, want >= 2", got)
	}
}

// TestBuildNeighborGraph_GeoFarRejectLoggedOnce — regression test for a
// production incident on meshview.dk: upsertEdge is called once per
// observation, so a single geo-implausible pair re-evaluated across many
// observations produced one duplicate "reject geo-far edge" log line per
// observation (22k duplicate lines for 7 unique edges in one rebuild),
// contributing to CPU spikes and hard resets on a 2-vCPU host. The counter
// must still count every rejection, but the dedup-tracking map must record
// each unique edge pair at most once per build.
func TestBuildNeighborGraph_GeoFarRejectLoggedOnce(t *testing.T) {
	nodes := []nodeInfo{
		{Role: "repeater", PublicKey: "aaaa1111", Name: "A_SF", Lat: 37.77, Lon: -122.41, HasGPS: true},
		{Role: "repeater", PublicKey: "bbbb2222", Name: "B_BE", Lat: 52.52, Lon: 13.40, HasGPS: true},
		{Role: "repeater", PublicKey: "obs00001", Name: "Observer", Lat: 37.77, Lon: -122.41, HasGPS: true},
	}
	// Ten adverts each re-triggering the same far A↔B edge attempt, mimicking
	// many observations of the same implausible pair within one rebuild.
	txs := make([]*StoreTx, 0, 10)
	for i := 0; i < 10; i++ {
		txs = append(txs, ngMakeTx(i+1, 4, ngFromNodeJSON("aaaa1111"), []*StoreObs{
			ngMakeObs("obs00001", `["bbbb"]`, nowStr, nil),
		}))
	}
	store := ngTestStore(nodes, txs)

	rejectLines := captureNeighborGraphRejectLog(t)
	g := BuildFromStore(store)
	lines := rejectLines()

	// The production symptom was duplicate LOG LINES (22k of them for 7
	// unique edges on meshview.dk), not merely the size of an internal
	// tracking map — assert on the actual captured output first.
	if len(lines) != 2 {
		t.Fatalf("got %d '[neighbor-graph] reject geo-far edge' log lines, want exactly 2 (one per unique edge pair, not one per observation): %q", len(lines), lines)
	}

	// Each advert attempts two edges — originator↔path[0] (A↔B) and
	// observer↔path[last] (obs↔B, also far since the observer sits at A's
	// location). Both pubkeys in these fixtures are exactly 8 chars, so
	// piiTruncPubkey is a no-op and the log line contains them verbatim —
	// the pair may appear in either order since upsertEdge logs its
	// original call-argument order, not the normalized edgeKey order.
	abCount, obsBCount := 0, 0
	for _, l := range lines {
		if strings.Contains(l, "aaaa1111↔bbbb2222") || strings.Contains(l, "bbbb2222↔aaaa1111") {
			abCount++
		}
		if strings.Contains(l, "obs00001↔bbbb2222") || strings.Contains(l, "bbbb2222↔obs00001") {
			obsBCount++
		}
	}
	if abCount != 1 {
		t.Fatalf("A↔B geo-far reject line appeared %d times, want exactly 1: %q", abCount, lines)
	}
	if obsBCount != 1 {
		t.Fatalf("observer↔B geo-far reject line appeared %d times, want exactly 1: %q", obsBCount, lines)
	}
	if abCount == 10 || obsBCount == 10 {
		t.Fatalf("edge logged once per observation (10) instead of once per unique pair — dedup regressed (ab=%d, obsB=%d)", abCount, obsBCount)
	}

	// Every rejection must still be counted, even though only 2 of the 20
	// produced a log line — RejectedEdgesGeoFar semantics are unchanged.
	if got := atomic.LoadUint64(&g.RejectedEdgesGeoFar); got < 20 {
		t.Fatalf("RejectedEdgesGeoFar = %d, want >= 20 (every observation still counted)", got)
	}

	// Secondary/internal assertion corroborating the log-output proof above.
	// Not sufficient on its own: a bug that moved the log.Printf outside the
	// `if !alreadyLogged` guard would still pass this line while
	// re-introducing the log-spam incident.
	g.mu.RLock()
	loggedCount := len(g.loggedGeoFarRejects)
	g.mu.RUnlock()
	if loggedCount != 2 {
		t.Fatalf("loggedGeoFarRejects has %d entries, want exactly 2 (one per unique edge pair, not per observation)", loggedCount)
	}
}

// TestBuildNeighborGraph_GeoFarRejectLoggedOnce_Concurrent — concurrency
// coverage for the same dedup path: many goroutines rejecting the SAME edge
// pair simultaneously must not race on loggedGeoFarRejects (run under
// `go test -race`), must still produce exactly one log line for that edge,
// and RejectedEdgesGeoFar must count every individual call.
//
// Builds a minimal NeighborGraph directly and calls the real upsertEdge
// entry point concurrently, rather than going through BuildFromStore, so
// the only possible reject-log lines are the ones under test.
func TestBuildNeighborGraph_GeoFarRejectLoggedOnce_Concurrent(t *testing.T) {
	g := NewNeighborGraph()
	g.maxEdgeKm = DefaultMaxEdgeKm
	g.nodeGeo = map[string]nodeGeoInfo{
		"aaaa1111": {Lat: 37.77, Lon: -122.41, HasGPS: true},
		"bbbb2222": {Lat: 52.52, Lon: 13.40, HasGPS: true},
	}

	rejectLines := captureNeighborGraphRejectLog(t)

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			g.upsertEdge("aaaa1111", "bbbb2222", "", "obs00001", nil, time.Now())
		}()
	}
	wg.Wait()

	if got := atomic.LoadUint64(&g.RejectedEdgesGeoFar); got != n {
		t.Fatalf("RejectedEdgesGeoFar = %d, want exactly %d (every concurrent call must still be counted)", got, n)
	}

	lines := rejectLines()
	if len(lines) != 1 {
		t.Fatalf("got %d '[neighbor-graph] reject geo-far edge' log lines from %d concurrent calls on the same edge, want exactly 1: %q", len(lines), n, lines)
	}
}
