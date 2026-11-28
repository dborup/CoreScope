package main

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"testing"
	"time"
)

// Exercise the real SQL boundary: setupTestDB has exactly one connection.
// Keeping it checked out makes the next query wait in database/sql rather
// than relying on a sleep or on a test double which never executes the query.
func TestStoreSQLDoesNotHoldPacketLock412(t *testing.T) {
	for _, name := range []string{"hash", "transmission", "observation", "observations", "packets", "expanded", "multi-node", "grouped", "node-filter", "region-cold", "region-expired", "area-cold", "area-expired", "nodes-cold", "nodes-expired", "health", "bulk-health", "subpaths", "channels", "grouped-region", "grouped-area", "channel-list-region", "encrypted-channels-region", "analytics-channels-region", "analytics-rf-region", "subpaths-windowed-region"} {
		t.Run(name, func(t *testing.T) {
			srv, _ := setupTestServer(t)
			txID := seedConfirmTx(t, srv, "sql_unlocked_412", `["aa","bb"]`, `["aabbccdd11223344","eeff00112233aabb"]`)
			store := reloadConfirmStore(t, srv)
			resetLRU(store)
			store.mu.RLock()
			obsID := store.byTxID[txID].Observations[0].ID
			store.mu.RUnlock()
			var request func()
			switch name {
			case "hash":
				request = func() { store.GetPacketByHash("sql_unlocked_412") }
			case "transmission":
				request = func() { store.GetTransmissionByID(txID) }
			case "observation":
				request = func() { store.GetPacketByID(obsID) }
			case "observations":
				request = func() { store.GetObservationsForHash("sql_unlocked_412") }
			case "packets":
				request = func() { store.QueryPackets(PacketQuery{Limit: 1}) }
			case "expanded":
				request = func() { store.QueryPackets(PacketQuery{Limit: 1, ExpandObservations: true}) }
			case "multi-node":
				request = func() { store.QueryMultiNodePackets([]string{confirmTestTarget}, 1, 0, "DESC", "", "") }
			case "grouped":
				request = func() { store.QueryGroupedPackets(PacketQuery{Limit: 1}) }
			case "node-filter":
				request = func() { store.QueryPackets(PacketQuery{Node: "TestRepeater", Limit: 1}) }
			case "region-cold", "region-expired":
				if name == "region-expired" {
					store.resolveRegionObservers("SJC")
					store.regionObsCacheTime = time.Now().Add(-time.Hour)
				}
				request = func() { store.QueryPackets(PacketQuery{Region: "SJC", Limit: 1}) }
			case "area-cold", "area-expired":
				store.config = &Config{Areas: map[string]AreaEntry{"TEST": {Label: "Test", Polygon: [][2]float64{{0, 0}, {1, 0}, {1, 1}, {0, 1}}}}}
				if name == "area-expired" {
					store.resolveAreaNodes("TEST")
					store.areaNodeCacheTimes["TEST"] = time.Now().Add(-time.Hour)
				}
				request = func() { store.QueryPackets(PacketQuery{Area: "TEST", Limit: 1}) }
			case "nodes-cold", "nodes-expired":
				store.cacheMu.Lock()
				if name == "nodes-cold" {
					store.nodeCache = nil
				}
				store.nodeCacheTime = time.Now().Add(-time.Hour)
				store.cacheMu.Unlock()
				request = func() { store.computeAnalyticsTopology("", "", TimeWindow{}) }
			case "health":
				request = func() { store.GetNodeHealth(confirmTestTarget, time.Now()) }
			case "bulk-health":
				request = func() { store.GetBulkHealth(10, "", "") }
			case "subpaths":
				request = func() { store.computeAnalyticsSubpaths("SJC", 2, 8, 10) }
			case "channels":
				request = func() { store.GetChannelMessages("public", 10, 0, "SJC") }
			// #597 (upstream 2247): every other region/area caller resolves
			// its set before s.mu, as QueryPackets does above.
			case "grouped-region":
				request = func() { store.QueryGroupedPackets(PacketQuery{Region: "SJC", Limit: 1}) }
			case "grouped-area":
				store.config = &Config{Areas: map[string]AreaEntry{"TEST": {Label: "Test", Polygon: [][2]float64{{0, 0}, {1, 0}, {1, 1}, {0, 1}}}}}
				request = func() { store.QueryGroupedPackets(PacketQuery{Area: "TEST", Limit: 1}) }
			case "channel-list-region":
				request = func() { store.GetChannels("SJC") }
			case "encrypted-channels-region":
				request = func() { store.GetEncryptedChannels("SJC") }
			case "analytics-channels-region":
				request = func() { store.computeAnalyticsChannels("SJC", "", TimeWindow{}) }
			case "analytics-rf-region":
				request = func() { store.computeAnalyticsRF("SJC", "", TimeWindow{}) }
			case "subpaths-windowed-region":
				request = func() { store.computeAnalyticsSubpathsWindowed("SJC", 2, 8, 10, TimeWindow{}) }
			}
			connection, err := srv.db.conn.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			// Release SQL before joining the request even when an assertion fails.
			done := make(chan struct{})
			defer func() {
				connection.Close()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("request did not finish after releasing SQL")
				}
			}()
			waitCount := srv.db.conn.Stats().WaitCount
			go func() { defer close(done); request() }()
			deadline := time.Now().Add(3 * time.Second)
			for srv.db.conn.Stats().WaitCount == waitCount {
				select {
				case <-done:
					t.Fatal("request never attempted SQL")
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("request never reached the occupied SQL pool")
				}
				runtime.Gosched()
			}
			writer := make(chan struct{})
			go func() { store.mu.Lock(); store.mu.Unlock(); close(writer) }()
			select {
			case <-writer:
			case <-time.After(time.Second):
				t.Error("SQL wait held PacketStore.mu and blocked an unrelated writer")
			}
		})
	}
}

func TestNodeCacheInvalidationWinsOverSQL412(t *testing.T) {
	db := setupTestDB(t)
	seedTestData(t, db)
	store := NewPacketStore(db, nil)
	connection, err := db.conn.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	defer func() {
		connection.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("cache load did not finish")
		}
	}()
	waits := db.conn.Stats().WaitCount
	go func() { defer close(done); store.getCachedNodesAndPM() }()
	deadline := time.Now().Add(3 * time.Second)
	for db.conn.Stats().WaitCount == waits {
		if time.Now().After(deadline) {
			t.Fatal("node cache did not reach SQL")
		}
		runtime.Gosched()
	}
	store.InvalidateNodeCache()
	connection.Close()
	<-done
	store.cacheMu.RLock()
	if store.nodeCache != nil || store.nodePM != nil || !store.nodeCacheTime.IsZero() {
		t.Error("invalidated SQL snapshot was republished")
	}
	store.cacheMu.RUnlock()
	if nodes, pm := store.getCachedNodesAndPM(); len(nodes) == 0 || pm == nil {
		t.Fatal("next request did not rebuild invalidated cache")
	}
}

func TestPacketDetailResolvedPathZeroAndNegativeIDs412(t *testing.T) {
	srv, _ := setupTestServer(t)
	for _, id := range []int{0, -5} {
		hash := fmt.Sprintf("edge_id_%d", id)
		if _, err := srv.db.conn.Exec(`INSERT INTO transmissions(id,raw_hex,hash,first_seen,route_type,payload_type,decoded_json) VALUES (?,'FF01',?,'2026-01-01T00:00:00Z',1,4,'{}')`, id, hash); err != nil {
			t.Fatal(err)
		}
		if _, err := srv.db.conn.Exec(`INSERT INTO observations(transmission_id,observer_idx,path_json,timestamp,resolved_path) VALUES (?,1,'["aa","bb"]',1767225600,'["aabbccdd11223344","eeff00112233aabb"]')`, id); err != nil {
			t.Fatal(err)
		}
	}
	store := reloadConfirmStore(t, srv)
	for _, id := range []int{0, -5} {
		m := store.GetTransmissionByID(id)
		if m == nil {
			t.Fatalf("missing tx %d", id)
		}
		rp, _ := m["resolved_path"].([]*string)
		if len(rp) != 2 || rp[0] == nil || *rp[0] != confirmTestTarget {
			t.Errorf("tx %d resolved_path = %#v", id, m["resolved_path"])
		}
	}
}

func TestDistanceIndexUsesCapturedNodeSnapshot412(t *testing.T) {
	late := time.Now().UTC().Add(-72 * time.Hour)
	early := late.Add(-time.Hour)
	s := hm288Setup(t, true, late.Format(time.RFC3339), early.Format(time.RFC3339), `["05","06"]`, `["05","06"]`)
	metadata := s.snapshotPathNodes()
	s.mu.RLock()
	wantHops, wantPaths := len(s.distHops), len(s.distPaths)
	s.mu.RUnlock()
	if wantHops == 0 || wantPaths == 0 {
		t.Fatal("fixture has no distance records")
	}
	// This is the precise warm -> invalidation -> lock interleaving. The
	// helper must use the captured value, not re-read an empty global cache.
	s.InvalidateNodeCache()
	s.mu.Lock()
	s.buildDistanceIndex(metadata)
	gotHops, gotPaths := len(s.distHops), len(s.distPaths)
	s.mu.Unlock()
	if gotHops != wantHops || gotPaths != wantPaths {
		t.Fatalf("invalidating cache lost distance facts: hops %d->%d paths %d->%d", wantHops, gotHops, wantPaths, gotPaths)
	}
}

func TestPacketDetailSnapshotSurvivesConcurrentMutation412(t *testing.T) {
	srv, _ := setupTestServer(t)
	txID := seedConfirmTx(t, srv, "stable_412", `["aa","bb"]`, `["aabbccdd11223344","eeff00112233aabb"]`)
	store := reloadConfirmStore(t, srv)
	resetLRU(store)
	connection, err := srv.db.conn.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	defer func() {
		connection.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("detail did not finish")
		}
	}()
	waits := srv.db.conn.Stats().WaitCount
	var result struct {
		hash         string
		route        int
		observations int
	}
	go func() {
		defer close(done)
		m := store.GetTransmissionByID(txID)
		result.hash, _ = m["hash"].(string)
		result.route, _ = m["route_type"].(int)
		result.observations = len(m["observations"].([]map[string]interface{}))
	}()
	deadline := time.Now().Add(3 * time.Second)
	for srv.db.conn.Stats().WaitCount == waits {
		if time.Now().After(deadline) {
			t.Fatal("detail did not reach SQL")
		}
		runtime.Gosched()
	}
	// Mirror the hash migration and an append while the response's SQL is
	// blocked. A response owns its original snapshot, not mutable tx fields.
	mutated := make(chan struct{})
	go func() {
		store.mu.Lock()
		tx := store.byTxID[txID]
		tx.Hash = "migrated_412"
		route := 2
		tx.RouteType = &route
		tx.Observations = append(tx.Observations, &StoreObs{ID: 999, TransmissionID: txID, PathJSON: `["cc"]`})
		store.mu.Unlock()
		close(mutated)
	}()
	select {
	case <-mutated:
	case <-time.After(time.Second):
		t.Fatal("detail blocks concurrent mutation")
	}
	connection.Close()
	<-done
	if result.hash != "stable_412" || result.route != 1 || result.observations != 1 {
		t.Fatalf("mixed snapshot: %+v", result)
	}
}

// The benchmark measures locked snapshot overhead and allocations without a
// DB delay. Convoy safety itself is enforced by the deterministic test above.
func BenchmarkPacketDetailSnapshot412(b *testing.B) {
	for _, n := range []int{30000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			s := NewPacketStore(nil, nil)
			for i := 1; i <= n; i++ {
				tx := &StoreTx{ID: i, Hash: fmt.Sprintf("%016x", i), FirstSeen: "2026-01-01T00:00:00Z", Observations: []*StoreObs{{ID: i, TransmissionID: i, PathJSON: `["aa","bb"]`}}}
				txGetParsedPath(tx)
				s.byHash[tx.Hash] = tx
				s.byTxID[i] = tx
				s.packets = append(s.packets, tx)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s.GetTransmissionByID(1)
			}
		})
	}
}

// Real database/sql pool contention with a controlled 20 ms connection hold.
// The writer applies the same observation/header mutation as live ingest
// (applyObservation470), not merely a mutex probe. Both versions wait for the
// same SQL hold overall; writer-us metrics isolate the user-visible convoy.
func BenchmarkPacketSQLWriterDelay412(b *testing.B) {
	db := setupTestDB(b)
	s := NewPacketStore(db, nil)
	for i := 1; i <= 30000; i++ {
		tx := &StoreTx{ID: i, Hash: fmt.Sprintf("%016x", i), FirstSeen: "2026-01-01T00:00:00Z", Observations: []*StoreObs{{ID: i, TransmissionID: i, PathJSON: `["aa","bb"]`}}}
		txGetParsedPath(tx)
		s.byHash[tx.Hash], s.byTxID[i] = tx, tx
		s.packets = append(s.packets, tx)
	}
	latencies := make([]float64, 0, b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resetLRU(s)
		connection, err := db.conn.Conn(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		waits := db.conn.Stats().WaitCount
		done := make(chan struct{})
		go func() { s.GetTransmissionByID(1); close(done) }()
		deadline := time.Now().Add(3 * time.Second)
		for db.conn.Stats().WaitCount == waits {
			if time.Now().After(deadline) {
				connection.Close()
				b.Fatal("reader did not reach SQL")
			}
			runtime.Gosched()
		}
		timer := time.AfterFunc(20*time.Millisecond, func() { connection.Close() })
		written := make(chan struct{})
		started := time.Now()
		go func() {
			applyObservation470(s, s.byTxID[1], &StoreObs{ID: 30001 + i, TransmissionID: 1, PathJSON: `["aa","bb"]`, Timestamp: "2026-01-01T00:00:01Z"})
			close(written)
		}()
		<-written
		latencies = append(latencies, float64(time.Since(started).Nanoseconds())/1000)
		<-done
		timer.Stop()
	}
	b.StopTimer()
	sort.Float64s(latencies)
	if len(latencies) > 0 {
		b.ReportMetric(latencies[len(latencies)/2], "writer-p50-us")
		b.ReportMetric(latencies[(len(latencies)-1)*95/100], "writer-p95-us")
	}
}
