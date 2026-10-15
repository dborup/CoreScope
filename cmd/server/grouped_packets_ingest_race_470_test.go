package main

import (
	"fmt"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Issue #470: readers that take a transmission from the store and read its
// observations or header fields after releasing s.mu race with ingest, which
// appends to tx.Observations and rewrites LatestSeen and the header observer
// fields (pickBestObservation) under s.mu.Lock. A torn read of the
// Observations slice header can walk past the backing array and load garbage
// *StoreObs pointers, which the GC reports as "found bad pointer in Go heap".
//
// The tests run the real readers against that mutation and fail under -race
// (CI runs the server suite with -race) while a read is unlocked. The writer
// applies an observation to the tx exactly as IngestNewObservations does, but
// without its SQL read: database/sql's pool mutex, shared with the readers'
// own queries, would order most writes after the readers' unlocked reads and
// hide the overlap from the race detector. Production code cannot rely on
// that ordering.

// applyObservation470 adds obs to tx the way IngestNewObservations does, under
// s.mu.Lock.
func applyObservation470(s *PacketStore, tx *StoreTx, obs *StoreObs) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx.Observations = append(tx.Observations, obs)
	tx.ObservationCount++
	if obs.Timestamp > tx.LatestSeen {
		tx.LatestSeen = obs.Timestamp
	}
	pickBestObservation(tx)
}

// raceIngest470 runs readers goroutines that call read reads times each while
// one writer calls write(1), write(2), ... (yielding in between) until they
// are done and at least minWrites writes ran, so every unlocked read of a
// reader can overlap a write. It returns the number of writes.
func raceIngest470(t *testing.T, readers, reads, minWrites int, read func(reader, i int) error, write func(n int)) int {
	t.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, readers)
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for i := 0; i < reads; i++ {
				if err := read(r, i); err != nil {
					errs <- err
					return
				}
			}
		}(r)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	n := 0
	for {
		select {
		case <-done:
			if n >= minWrites {
				close(errs)
				for err := range errs {
					t.Error(err)
				}
				return n
			}
		default:
		}
		n++
		write(n)
		runtime.Gosched()
	}
}

// QueryGroupedPackets built its page (groupedTxsToPage, storeTxDistinctIatas)
// from the shared grouped cache, and sorted it by LatestSeen, without s.mu.
func TestQueryGroupedPacketsConcurrentIngest470(t *testing.T) {
	const txCount, readers, reads = 50, 4, 150
	iatas := []string{"SJC", "SFO", "OAK"}
	base := time.Now().UTC().Add(-time.Hour)
	stamp := func(i int) string { return base.Add(time.Duration(i) * time.Second).Format(time.RFC3339) }

	store := NewPacketStore(nil, nil)
	txs := make([]*StoreTx, txCount)
	for i := range txs {
		tx := &StoreTx{ID: i + 1, Hash: fmt.Sprintf("470%013d", i+1), FirstSeen: stamp(i), LatestSeen: stamp(i), PathJSON: `["aa"]`}
		tx.Observations = []*StoreObs{{ID: i + 1, TransmissionID: tx.ID, ObserverID: "OBS0", ObserverIATA: iatas[0], PathJSON: `["aa"]`, Timestamp: stamp(i)}}
		tx.ObservationCount = 1
		pickBestObservation(tx)
		txs[i] = tx
		store.packets = append(store.packets, tx)
		store.byHash[tx.Hash] = tx
		store.byTxID[tx.ID] = tx
	}

	ingests := raceIngest470(t, readers, reads, 3*txCount, func(r, i int) error {
		if (i+r)%2 == 0 { // alternate the cold (filter+sort) and cached paths
			store.groupedCacheMu.Lock()
			store.groupedCacheTxs = nil
			store.groupedCacheMu.Unlock()
		}
		if res := store.QueryGroupedPackets(PacketQuery{Limit: txCount}); res.Total != txCount {
			return fmt.Errorf("grouped total = %d, want %d", res.Total, txCount)
		}
		return nil
	}, func(n int) {
		tx := txs[n%txCount]
		applyObservation470(store, tx, &StoreObs{
			ID: txCount + n, TransmissionID: tx.ID, ObserverID: fmt.Sprintf("OBS%d", n%3), ObserverIATA: iatas[n%3],
			PathJSON: fmt.Sprintf(`["bb","%04x"]`, n), Timestamp: stamp(txCount + n),
		})
	})

	store.groupedCacheTxs = nil
	res := store.QueryGroupedPackets(PacketQuery{Limit: txCount})
	observations := 0
	for _, row := range res.Packets {
		observations += row["observation_count"].(int)
		if got := row["distinct_iatas"].([]string); len(got) != len(iatas) {
			t.Errorf("row %v distinct_iatas = %v, want all of %v", row["hash"], got, iatas)
		}
	}
	if observations != txCount+ingests {
		t.Fatalf("grouped rows count %d observations, want %d", observations, txCount+ingests)
	}
}

// /api/nodes/{pk}/paths and the node's hop analytics release s.mu before the
// canonical resolved_path lookup (SQL and lruMu, #1278) and read each
// candidate's tx.Observations after it (hop analytics also its hash,
// first_seen and scope).
func TestNodePathsAndHopAnalyticsConcurrentIngest470(t *testing.T) {
	const txCount, readers, reads = 20, 4, 100

	srv, router := setupTestServer(t)
	txIDs := make([]int, txCount)
	for i := range txIDs {
		txIDs[i] = seedConfirmTx(t, srv, fmt.Sprintf("race470_%02d", i),
			`["aa","bb"]`, `["`+confirmTestTarget+`","eeff00112233aabb"]`)
	}
	store := reloadConfirmStore(t, srv)
	txs := make([]*StoreTx, txCount)
	store.mu.RLock()
	for i, id := range txIDs {
		txs[i] = store.byTxID[id]
	}
	store.mu.RUnlock()
	// The test server's own fixture adds packets through the target too.
	baseline, err := store.GetNodeHopAnalytics(confirmTestTarget, 7)
	if err != nil || len(baseline.Packets) < txCount {
		t.Fatalf("hop analytics before ingest: %v, %+v", err, baseline)
	}

	now := time.Now().UTC()
	raceIngest470(t, readers, reads, txCount, func(r, i int) error {
		if (i+r)%2 == 0 {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("GET", "/api/nodes/"+confirmTestTarget+"/paths", nil))
			if w.Code != 200 {
				return fmt.Errorf("paths: %d %s", w.Code, w.Body.String())
			}
		} else if res, err := store.GetNodeHopAnalytics(confirmTestTarget, 7); err != nil || len(res.Packets) != len(baseline.Packets) {
			return fmt.Errorf("hop analytics: %v, %+v, want %d packets", err, res, len(baseline.Packets))
		}
		return nil
	}, func(n int) {
		tx := txs[n%txCount]
		applyObservation470(store, tx, &StoreObs{
			ID: 100000 + n, TransmissionID: tx.ID, ObserverID: "obs1",
			PathJSON: fmt.Sprintf(`["aa","bb","%04x"]`, n), Timestamp: now.Add(time.Duration(n) * time.Millisecond).Format(time.RFC3339Nano),
		})
	})

	// The new observations carry no resolved_path, so every candidate still
	// resolves through its seeded one.
	res, err := store.GetNodeHopAnalytics(confirmTestTarget, 7)
	if err != nil || len(res.Packets) != len(baseline.Packets) {
		t.Fatalf("hop analytics after ingest: %v, %d packets, want %d", err, len(res.Packets), len(baseline.Packets))
	}
	hashes := map[string]bool{}
	for _, p := range res.Packets {
		hashes[p.Hash] = true
	}
	for i := 0; i < txCount; i++ {
		if h := fmt.Sprintf("race470_%02d", i); !hashes[h] {
			t.Errorf("hop analytics lost %s", h)
		}
	}
}

// txGetParsedPath is called under s.mu.RLock by many readers. It used to fill
// tx.parsedPath lazily, after pickBestObservation emptied the cache on every
// ingested observation, so concurrent readers wrote the same fields.
func TestParsedPathConcurrentReaders470(t *testing.T) {
	const txCount, readers, reads = 20, 4, 200

	store := NewPacketStore(nil, nil)
	txs := make([]*StoreTx, txCount)
	for i := range txs {
		tx := &StoreTx{ID: i + 1, Hash: fmt.Sprintf("470p%012d", i+1)}
		tx.Observations = []*StoreObs{{ID: i + 1, TransmissionID: tx.ID, ObserverID: "OBS0", PathJSON: `["aa"]`}}
		pickBestObservation(tx)
		txs[i] = tx
	}

	raceIngest470(t, readers, reads, txCount, func(r, i int) error {
		store.mu.RLock()
		defer store.mu.RUnlock()
		for _, tx := range txs {
			if got, want := len(txGetParsedPath(tx)), pathLen(tx.PathJSON); got != want {
				return fmt.Errorf("tx %d: %d parsed hops for %s, want %d", tx.ID, got, tx.PathJSON, want)
			}
		}
		return nil
	}, func(n int) {
		// Each observation is the tx's longest path yet, so it becomes the
		// best one and the parsed path changes.
		hops := make([]string, n/txCount+2)
		for i := range hops {
			hops[i] = fmt.Sprintf(`"%02x"`, i)
		}
		tx := txs[n%txCount]
		applyObservation470(store, tx, &StoreObs{ID: 1000 + n, TransmissionID: tx.ID, ObserverID: "OBS1",
			PathJSON: "[" + strings.Join(hops, ",") + "]"})
	})
}
