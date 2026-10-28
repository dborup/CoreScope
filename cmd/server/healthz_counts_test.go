package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type healthzCountsResponse struct {
	Ready     bool `json:"ready"`
	LoadedTx  int  `json:"loadedTx"`
	LoadedObs int  `json:"loadedObs"`
}

func isClosedForTest(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func readHealthzCounts(t testing.TB, store *PacketStore) healthzCountsResponse {
	t.Helper()
	w := httptest.NewRecorder()
	(&Server{store: store}).handleHealthz(w, httptest.NewRequest("GET", "/api/healthz", nil))
	var got healthzCountsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode healthz: %v", err)
	}
	if w.Code != http.StatusOK || !got.Ready {
		t.Fatalf("healthz: code=%d body=%s", w.Code, w.Body.String())
	}
	return got
}

// assertHealthzCounts checks the retained scan, the maintained counter and
// the handler output all agree.
func assertHealthzCounts(t testing.TB, store *PacketStore, wantTx, wantObs int) {
	t.Helper()
	if store != nil {
		store.mu.RLock()
		txCount, obsCount, counter := len(store.packets), 0, store.totalObs
		for _, tx := range store.packets {
			obsCount += len(tx.Observations)
		}
		store.mu.RUnlock()
		if txCount != wantTx || obsCount != wantObs || counter != obsCount {
			t.Fatalf("retained=%d/%d counter=%d, want %d/%d", txCount, obsCount, counter, wantTx, wantObs)
		}
	}
	got := readHealthzCounts(t, store)
	if got.LoadedTx != wantTx || got.LoadedObs != wantObs {
		t.Fatalf("healthz=%d/%d, want %d/%d", got.LoadedTx, got.LoadedObs, wantTx, wantObs)
	}
}

func healthzReady(t testing.TB) {
	readiness.Store(1)
	t.Cleanup(func() { readiness.Store(0) })
	t.Setenv("CORESCOPE_INGESTOR_STATS", filepath.Join(t.TempDir(), "absent.json"))
}

// Red before the port: after startup the handler walked every retained
// transmission. A sentinel counter proves it now reads the maintained value.
func TestHealthzReadsCounterAfterStartup(t *testing.T) {
	healthzReady(t)
	db := setupTestDB(t)
	t.Cleanup(func() { db.Close() })
	seedTestData(t, db)
	store := NewPacketStore(db, &PacketStoreConfig{RetentionHours: 72})
	if err := store.RunStartupLoad(10); err != nil {
		t.Fatal(err)
	}
	assertHealthzCounts(t, store, 3, 4)
	store.mu.Lock()
	store.totalObs = 12345
	store.mu.Unlock()
	if got := readHealthzCounts(t, store); got.LoadedObs != 12345 {
		t.Fatalf("after startup healthz scanned the slice: loadedObs=%d, want counter 12345", got.LoadedObs)
	}
}

func TestHealthzCountsLifecycle(t *testing.T) {
	healthzReady(t)
	assertHealthzCounts(t, nil, 0, 0)
	for _, loader := range []string{"Load", "LoadChunked", "loadChunk", "RunStartupLoad"} {
		t.Run(loader, func(t *testing.T) {
			db := setupTestDB(t)
			t.Cleanup(func() { db.Close() })
			seedTestData(t, db)
			now := time.Now().UTC()
			mustExec(t, db, `INSERT INTO transmissions(raw_hex,hash,first_seen) VALUES ('AA','empty',?)`, now.Add(-24*time.Hour).Format(time.RFC3339))
			mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,path_json,timestamp) VALUES (1,1,'["aa","bb"]',?)`, now.Unix())
			store := NewPacketStore(db, &PacketStoreConfig{RetentionHours: 72})
			assertHealthzCounts(t, store, 0, 0)
			var err error
			switch loader {
			case "Load":
				err = store.Load()
			case "LoadChunked":
				err = store.LoadChunked(1)
			case "loadChunk":
				err = store.loadChunk(now.Add(-72*time.Hour), now.Add(time.Hour))
			case "RunStartupLoad":
				store.hotStartupHours = 2
				store.OnChunkLoaded(func(_, _ int) {
					if isClosedForTest(store.StartupLoadDone()) {
						t.Error("startup signal closed during hot load")
					}
					assertHealthzCounts(t, store, 1, 2)
				})
				store.bgLoaderEntryHook = func() { assertHealthzCounts(t, store, 1, 2) }
				err = store.RunStartupLoad(1)
				if !isClosedForTest(store.StartupLoadDone()) {
					t.Error("startup signal did not close after background fill")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			assertHealthzCounts(t, store, 4, 4)

			mustExec(t, db, `INSERT INTO transmissions(raw_hex,hash,first_seen) VALUES ('BB','live',?)`, now.Format(time.RFC3339))
			mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,path_json,timestamp) VALUES (5,1,'[]',?),(5,1,'[]',?)`, now.Unix(), now.Unix())
			store.IngestNewFromDB(4, 10)
			store.IngestNewFromDB(4, 10) // replay must not add observations
			assertHealthzCounts(t, store, 5, 5)
		})
	}
}

func TestHealthzCountsEviction(t *testing.T) {
	healthzReady(t)
	db := setupTestDB(t)
	t.Cleanup(func() { db.Close() })
	seedTestData(t, db)
	store := NewPacketStore(db, &PacketStoreConfig{RetentionHours: 72})
	if err := store.RunStartupLoad(10); err != nil {
		t.Fatal(err)
	}
	store.mu.RLock()
	before := len(store.packets)
	store.mu.RUnlock()
	store.mu.Lock()
	store.retentionHours = 0.0001
	evicted := store.EvictStale()
	store.mu.Unlock()
	if evicted == 0 || evicted > before {
		t.Fatalf("evicted %d of %d", evicted, before)
	}
	got := readHealthzCounts(t, store)
	store.mu.RLock()
	want := 0
	for _, tx := range store.packets {
		want += len(tx.Observations)
	}
	store.mu.RUnlock()
	if got.LoadedTx != before-evicted || got.LoadedObs != want {
		t.Fatalf("after eviction healthz=%d/%d, want %d/%d", got.LoadedTx, got.LoadedObs, before-evicted, want)
	}
}

func TestHealthzCountsFailedStartup(t *testing.T) {
	healthzReady(t)
	db := setupTestDB(t)
	t.Cleanup(func() { db.Close() })
	seedTestData(t, db)
	store := NewPacketStore(db, &PacketStoreConfig{RetentionHours: 72, HotStartupHours: 2})
	store.bgLoaderEntryHook = func() {
		assertHealthzCounts(t, store, 1, 2)
		db.Close()
	}
	_ = store.RunStartupLoad(1)
	if !isClosedForTest(store.StartupLoadDone()) {
		t.Fatal("failed startup must publish its terminal signal")
	}
	assertHealthzCounts(t, store, 1, 2)
}

func TestHealthzCountsConcurrentIngest(t *testing.T) {
	healthzReady(t)
	db := setupTestDB(t)
	t.Cleanup(func() { db.Close() })
	mustExec(t, db, `INSERT INTO observers(id) VALUES ('obs1'),('obs2')`)
	store := NewPacketStore(db, &PacketStoreConfig{RetentionHours: 24})
	if err := store.RunStartupLoad(10); err != nil {
		t.Fatal(err)
	}
	var readers sync.WaitGroup
	var samples atomic.Int64
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				got := readHealthzCounts(t, store)
				if got.LoadedObs != 2*got.LoadedTx {
					t.Errorf("torn snapshot: %d transmissions, %d observations", got.LoadedTx, got.LoadedObs)
					return
				}
				samples.Add(1)
			}
		}()
	}
	now := time.Now().UTC()
	for id := 1; id <= 60; id++ {
		mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen) VALUES (?,'AA',?,?)`, id, fmt.Sprintf("live-%d", id), now.Format(time.RFC3339))
		mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,path_json,timestamp) VALUES (?,1,'[]',?),(?,2,'[]',?)`, id, now.Unix(), id, now.Unix())
		store.IngestNewFromDB(id-1, 10)
	}
	close(stop)
	readers.Wait()
	assertHealthzCounts(t, store, 60, 120)
	if samples.Load() == 0 {
		t.Fatal("readers never sampled")
	}
}

// Before the final merge of a background chunk, a live observation poll can
// bump totalObs for a packet that is not yet in store.packets. The handler
// must keep counting the slice until StartupLoadDone, or it over-reports.
func TestHealthzCountsExcludeUnpublishedObservations(t *testing.T) {
	healthzReady(t)
	db := setupTestDB(t)
	t.Cleanup(func() { db.Close() })
	now := time.Now().UTC()
	hot, cold := now.Add(-30*time.Minute), now.Add(-48*time.Hour)
	mustExec(t, db, `INSERT INTO observers(id) VALUES ('obs1'),('obs2')`)
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen) VALUES (1,'AA','hot',?),(2,'BB','cold',?)`, hot.Format(time.RFC3339), cold.Format(time.RFC3339))
	mustExec(t, db, `INSERT INTO observations(id,transmission_id,observer_idx,path_json,timestamp) VALUES (1,1,1,'[]',?),(2,2,1,'[]',?)`, hot.Unix(), cold.Unix())
	store := NewPacketStore(db, &PacketStoreConfig{RetentionHours: 72, HotStartupHours: 2})
	if err := store.LoadChunked(1); err != nil {
		t.Fatal(err)
	}
	assertHealthzCounts(t, store, 1, 1)

	chunk := NewPacketStore(db, nil)
	if err := chunk.loadChunk(now.Add(-72*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	localObs, localBytes := chunk.totalObs, chunk.trackedBytes
	pending := chunk.packets[0]

	// loadChunk's pre-merge state: indexes visible, packets/counter not.
	store.mu.Lock()
	store.byHash[pending.Hash] = pending
	store.byTxID[pending.ID] = pending
	for id, obs := range chunk.byObsID {
		store.byObsID[id] = obs
		store.byObserver[obs.ObserverID] = append(store.byObserver[obs.ObserverID], obs)
	}
	store.mu.Unlock()
	mustExec(t, db, `INSERT INTO observations(id,transmission_id,observer_idx,path_json,timestamp) VALUES (3,2,2,'[]',?)`, now.Unix())
	if got := store.IngestNewObservations(2, 10); len(got) != 1 {
		t.Fatalf("late observation was not ingested: %d broadcasts", len(got))
	}
	if len(store.packets) != 1 || len(pending.Observations) != 2 || store.totalObs != 2 {
		t.Fatalf("fixture: packets=%d pending obs=%d counter=%d", len(store.packets), len(pending.Observations), store.totalObs)
	}
	if isClosedForTest(store.StartupLoadDone()) {
		t.Fatal("startup must still be open")
	}
	if got := readHealthzCounts(t, store); got.LoadedTx != 1 || got.LoadedObs != 1 {
		t.Errorf("during background publication: healthz=%d/%d, want 1/1", got.LoadedTx, got.LoadedObs)
	}

	store.mu.Lock()
	store.packets = mergeByFirstSeen(store.packets, chunk.packets)
	store.totalObs += localObs
	store.trackedBytes += localBytes
	store.mu.Unlock()
	store.signalStartupLoadDone()
	assertHealthzCounts(t, store, 2, 3)
}

// Benchmark the real handler with SQLite-loaded stores at 1K/30K/100K.
func BenchmarkHealthzCounts(b *testing.B) {
	healthzReady(b)
	for _, n := range []int{1000, 30000, 100000} {
		b.Run(fmt.Sprintf("tx=%d", n), func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "healthz.db")
			createTestDBWithObs(b, path, n)
			db, err := OpenDB(path)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { db.Close() })
			store := NewPacketStore(db, nil)
			if err := store.RunStartupLoad(10000); err != nil {
				b.Fatal(err)
			}
			assertHealthzCounts(b, store, n, 3*n)
			srv := &Server{store: store}
			req := httptest.NewRequest("GET", "/api/healthz", nil)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				w := httptest.NewRecorder()
				srv.handleHealthz(w, req)
				if w.Code != http.StatusOK {
					b.Fatal(w.Code)
				}
			}
		})
	}
}
