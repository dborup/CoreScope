package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// #740 M3c: GET /api/analytics/dagi-unpositioned is the count /api/nodes
// reports beside a DAGI-filtered list (the seed's two visible nodes with no
// usable position; the blacklisted and the hidden-name one are left out).
func TestDagiAnalyticsUnpositioned(t *testing.T) {
	_, router := newDagiServer(t, dagiSeedConfig(), "memory")
	var got DagiFilterInfo
	dagiGetJSON(t, router, "/api/analytics/dagi-unpositioned", &got)
	var nodes dagiNodesPage
	dagiGetJSON(t, router, "/api/nodes?limit=100&kommune=0751", &nodes)
	if nodes.Dagi == nil || got.UnpositionedHidden != nodes.Dagi.UnpositionedHidden || got.UnpositionedHidden != 2 {
		t.Errorf("dagi-unpositioned %+v, /api/nodes %+v, want 2 for both", got, nodes.Dagi)
	}
	// The count does not depend on a selection, so params change nothing.
	var withParam DagiFilterInfo
	dagiGetJSON(t, router, "/api/analytics/dagi-unpositioned?kommune=9999", &withParam)
	if withParam != got {
		t.Errorf("with a param: %+v, without: %+v", withParam, got)
	}
}

func TestDagiAnalyticsUnpositionedOff(t *testing.T) {
	_, router := newDagiServer(t, dagiOff(), "memory")
	if w := dagiGet(t, router, "/api/analytics/dagi-unpositioned"); w.Code != 404 || !strings.Contains(w.Body.String(), "DAGI is off") {
		t.Errorf("DAGI off: %d %s", w.Code, w.Body.String())
	}
}

// The count comes from the membership snapshot, not from a DB query per
// request: once the snapshot is warm, the request is answered with the DB
// closed.
func TestDagiAnalyticsUnpositionedNoDBQuery(t *testing.T) {
	srv, router := newDagiServer(t, dagiSeedConfig(), "memory")
	var warm DagiFilterInfo
	dagiGetJSON(t, router, "/api/analytics/dagi-unpositioned", &warm)
	srv.db.conn.Close()
	var cold DagiFilterInfo
	dagiGetJSON(t, router, "/api/analytics/dagi-unpositioned", &cold)
	if cold != warm || cold.UnpositionedHidden != 2 {
		t.Errorf("with the DB closed: %+v, warm %+v", cold, warm)
	}
}

// dagiAnalyticsPerfServer is dagiPerfServer (2K nodes, 30K adverts) with one
// observation per advert, so RF has signal values to aggregate.
func dagiAnalyticsPerfServer(t testing.TB) *Server {
	t.Helper()
	srv := dagiPerfServer(t)
	st := srv.store
	for i, tx := range st.packets {
		snr, rssi := float64(i%20)-10, float64(-120+i%60)
		obs := &StoreObs{ID: i + 1, TransmissionID: tx.ID, ObserverID: "obs" + fmt.Sprint(i%8), SNR: &snr, RSSI: &rssi,
			PathJSON: "[]", Timestamp: tx.FirstSeen}
		tx.Observations = append(tx.Observations, obs)
		tx.ObservationCount = 1
		st.byTxID[tx.ID] = tx
		st.byObserver[obs.ObserverID] = append(st.byObserver[obs.ObserverID], obs)
		st.totalObs++
	}
	return srv
}

// dagiAnalyticsRecomputes are the store computations behind the Analytics
// tabs M3c sends a DAGI selection to, called directly so no result cache
// answers.
var dagiAnalyticsRecomputes = []struct {
	name string
	run  func(s *PacketStore, scope string)
}{
	{"rf", func(s *PacketStore, scope string) { s.computeAnalyticsRF("", scope, TimeWindow{}) }},
	{"topology", func(s *PacketStore, scope string) { s.computeAnalyticsTopology("", scope, TimeWindow{}) }},
	{"hash-sizes", func(s *PacketStore, scope string) { s.computeAnalyticsHashSizes("", scope) }},
}

// dagiAnalyticsScopes are the scope keys the handlers compute with: the
// configured area, and kommune 0751 as nodeScopeKey builds it.
func dagiAnalyticsScopes(t testing.TB, srv *Server) map[string]string {
	idx := srv.dagiIndex()
	if idx == nil {
		t.Fatal("no DAGI index")
	}
	key, ok := idx.layers[DagiKommune].filterKey("0751")
	if !ok {
		t.Fatal("0751 unknown")
	}
	var sel dagiSelection
	sel[DagiKommune] = key
	return map[string]string{"area=AAR": "AAR", "kommune=0751": nodeScopeKey("", sel)}
}

// Perf (AGENTS.md rule 0): an Analytics recompute with a DAGI selection on.
func BenchmarkDagiAnalyticsRecompute30K(b *testing.B) {
	srv := dagiAnalyticsPerfServer(b)
	scopes := dagiAnalyticsScopes(b, srv)
	for _, rc := range dagiAnalyticsRecomputes {
		for _, q := range []string{"area=AAR", "kommune=0751"} {
			b.Run(rc.name+"/"+q, func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					rc.run(srv.store, scopes[q])
				}
			})
		}
	}
}

// TestDagiAnalyticsRecomputeNoSlowerThanArea enforces it: per computation,
// the best of 5 runs with kommune=0751 within 1.5× the best of 5 with the
// configured area (plus 2 ms for timer noise). The DAGI scope is one set
// lookup per packet sender, as the area is; a point-in-polygon test per
// packet (30K × ~2 µs) would fail it.
func TestDagiAnalyticsRecomputeNoSlowerThanArea(t *testing.T) {
	if testing.Short() {
		t.Skip("perf")
	}
	srv := dagiAnalyticsPerfServer(t)
	scopes := dagiAnalyticsScopes(t, srv)
	// Both scopes must keep a real share of the 2K nodes, or the timings
	// compare nothing.
	for q, scope := range scopes {
		if n := len(srv.store.resolveAreaNodes(scope)); n < 50 || n > 1000 {
			t.Fatalf("%s matches %d of 2000 nodes", q, n)
		}
	}
	for _, rc := range dagiAnalyticsRecomputes {
		best := func(scope string) time.Duration {
			var min time.Duration
			for i := 0; i < 6; i++ {
				start := time.Now()
				rc.run(srv.store, scope)
				d := time.Since(start)
				if i > 0 && (min == 0 || d < min) { // the first run warms the caches
					min = d
				}
			}
			return min
		}
		area, kommune := best(scopes["area=AAR"]), best(scopes["kommune=0751"])
		t.Logf("%s 30K: area %v, kommune %v", rc.name, area, kommune)
		if kommune > area*3/2+2*time.Millisecond {
			t.Errorf("%s: kommune %v vs area %v", rc.name, kommune, area)
		}
	}
}
