package main

import (
	"sort"
	"sync"
	"testing"
	"time"
)

// Follow-ups from #542 (issue #548) for loadCanonicalResolvedPathsAt.

// lruOrderSnapshot returns lruOrder under the LRU lock.
func lruOrderSnapshot(s *PacketStore) []int {
	s.lruMu.RLock()
	defer s.lruMu.RUnlock()
	return append([]int(nil), s.lruOrder...)
}

// N3: one cold request inserts in ascending id order, chunk after chunk, for
// both the primary pass and the fallback pass. Removing either sort.Ints makes
// the order follow map iteration and fails here.
func TestCanonicalPaths548ChunkInsertOrderAscending(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		name := "primary"
		if fallback {
			name = "fallback"
		}
		t.Run(name, func(t *testing.T) {
			s := batch2224Store(t, false)
			const n = 1200 // three chunks, below lruMaxSize
			snapshots := seed2224(t, s, n, fallback)
			got := s.loadCanonicalResolvedPathsAt(snapshots, s.lruNow())
			if len(got) != n {
				t.Fatalf("got %d paths, want %d", len(got), n)
			}
			order := lruOrderSnapshot(s)
			if len(order) != n {
				t.Fatalf("lruOrder has %d entries, want %d", len(order), n)
			}
			if !sort.IntsAreSorted(order) {
				t.Fatalf("insert order is not ascending by observation id: %v ...", order[:10])
			}
		})
	}
}

// N1: when one request inserts past lruMaxSize, the intended survivors are the
// highest observation ids (ascending insert, FIFO eviction drops the lowest).
func TestCanonicalPaths548EvictionOrderPastCapacity(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		name := "primary"
		if fallback {
			name = "fallback"
		}
		t.Run(name, func(t *testing.T) {
			s := batch2224Store(t, false)
			const n = lruMaxSize + 50
			snapshots := seed2224(t, s, n, fallback)
			// The cached observation is the primary (odd id 2*tx-1) or, on
			// the fallback pass, its sibling (even id 2*tx).
			cachedID := func(tx int) int {
				if fallback {
					return 2 * tx
				}
				return 2*tx - 1
			}
			s.loadCanonicalResolvedPathsAt(snapshots, s.lruNow())
			s.lruMu.RLock()
			defer s.lruMu.RUnlock()
			if len(s.apiResolvedPathLRU) != lruMaxSize {
				t.Fatalf("LRU holds %d entries, want %d", len(s.apiResolvedPathLRU), lruMaxSize)
			}
			for tx := 1; tx <= 50; tx++ {
				if _, ok := s.apiResolvedPathLRU[cachedID(tx)]; ok {
					t.Fatalf("lowest id %d survived; eviction order is not ascending", cachedID(tx))
				}
			}
			for tx := n - 49; tx <= n; tx++ {
				if _, ok := s.apiResolvedPathLRU[cachedID(tx)]; !ok {
					t.Fatalf("highest id %d was evicted", cachedID(tx))
				}
			}
		})
	}
}

// N2: an absent row and a NULL resolved_path are not cached. The sibling that
// supplies the fallback is.
func TestCanonicalPaths548AbsentAndNullNotCached(t *testing.T) {
	s := batch2224Store(t, false)
	for _, row := range []struct {
		id, tx int
		path   string
		rp     interface{}
	}{
		{10, 1, `["aa","bb"]`, nil},      // longest, NULL resolved_path
		{11, 1, `["cc"]`, `["sibling"]`}, // shorter sibling supplies fallback
		{20, 2, `["aa"]`, `["present"]`}, // plain hit
	} {
		if _, err := s.db.conn.Exec(`INSERT INTO observations VALUES (?,?,?,?)`, row.id, row.tx, row.path, row.rp); err != nil {
			t.Fatal(err)
		}
	}
	snapshots := map[int][]*StoreObs{
		1: {{ID: 10, PathJSON: `["aa","bb"]`}, {ID: 11, PathJSON: `["cc"]`}},
		2: {{ID: 20, PathJSON: `["aa"]`}},
		3: {{ID: 30, PathJSON: `["aa"]`}}, // row 30 does not exist
	}
	got := s.loadCanonicalResolvedPathsAt(snapshots, s.lruNow())
	if len(got[1]) != 1 || *got[1][0] != "sibling" || len(got[2]) != 1 || *got[2][0] != "present" {
		t.Fatalf("unexpected paths: %+v", got)
	}
	if _, ok := got[3]; ok {
		t.Fatal("absent row produced a path")
	}
	s.lruMu.RLock()
	defer s.lruMu.RUnlock()
	for _, id := range []int{10, 30} {
		if _, ok := s.apiResolvedPathLRU[id]; ok {
			t.Fatalf("observation %d (NULL/absent) entered the LRU", id)
		}
	}
	for _, id := range []int{11, 20} {
		if _, ok := s.apiResolvedPathLRU[id]; !ok {
			t.Fatalf("observation %d should be cached", id)
		}
	}
}

// N4: two transmissions whose longest observation has the same id must both
// be answered. A plain obsID->txID map keeps only the last one.
func TestCanonicalPaths548SharedLongestObservation(t *testing.T) {
	s := batch2224Store(t, false)
	if _, err := s.db.conn.Exec(`INSERT INTO observations VALUES (5,1,'["aa"]','["shared"]')`); err != nil {
		t.Fatal(err)
	}
	snapshots := map[int][]*StoreObs{
		1: {{ID: 5, PathJSON: `["aa"]`}},
		2: {{ID: 5, PathJSON: `["aa"]`}},
	}
	got := s.loadCanonicalResolvedPathsAt(snapshots, s.lruNow())
	for _, tx := range []int{1, 2} {
		if len(got[tx]) != 1 || *got[tx][0] != "shared" {
			t.Fatalf("tx %d lost its path: %+v", tx, got)
		}
	}
}

// N5: lruMu is taken once per chunk, not once per id. The clock hook runs
// under the write lock inside lruPut; at the second put it starts a reader
// and holds the lock long enough for it to queue. The reader then sees the
// whole chunk if the chunk is written under one lock hold, and only the first
// two entries if the lock is released between ids.
func TestCanonicalPaths548LRULockedOncePerChunk(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		name := "primary"
		if fallback {
			name = "fallback"
		}
		t.Run(name, func(t *testing.T) {
			s := batch2224Store(t, false)
			const n = 6
			snapshots := seed2224(t, s, n, fallback)
			var calls int
			var wg sync.WaitGroup
			seen := -1
			s.lruClock = func() time.Time {
				calls++
				if calls == 2 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						s.lruMu.RLock()
						seen = len(s.apiResolvedPathLRU)
						s.lruMu.RUnlock()
					}()
					time.Sleep(50 * time.Millisecond)
				}
				return lruEpoch
			}
			s.loadCanonicalResolvedPathsAt(snapshots, 0)
			wg.Wait()
			if calls != n {
				t.Fatalf("lruPut ran %d times, want %d", calls, n)
			}
			if seen != n {
				t.Fatalf("reader saw %d entries mid-chunk, want all %d (lock released between ids)", seen, n)
			}
		})
	}
}

// N3 (fallback pass): when the primaries are cached as empty, the fallback
// list is built in map iteration order, so only sort.Ints(txIDs) makes the
// fallback chunks and their LRU inserts ascending.
func TestCanonicalPaths548FallbackListSortedFromCachedEmpty(t *testing.T) {
	s := batch2224Store(t, false)
	const n = 1200
	snapshots := seed2224(t, s, n, true)
	s.lruMu.Lock()
	for tx := 1; tx <= n; tx++ {
		s.lruPut(2*tx-1, nil) // primary cached as an empty path
	}
	s.lruMu.Unlock()
	got := s.loadCanonicalResolvedPathsAt(snapshots, s.lruNow())
	if len(got) != n {
		t.Fatalf("got %d paths, want %d", len(got), n)
	}
	order := lruOrderSnapshot(s)
	if len(order) != 2*n {
		t.Fatalf("lruOrder has %d entries, want %d", len(order), 2*n)
	}
	if tail := order[n:]; !sort.IntsAreSorted(tail) {
		t.Fatalf("fallback inserts are not ascending: %v ...", tail[:10])
	}
}

// BenchmarkCanonicalPaths548LRUContention measures the N5 change under reader
// pressure on lruMu: each lruMu write acquisition has to wait for in-flight
// readers, so per-id locking pays that once per id and per-chunk locking once
// per chunk.
func BenchmarkCanonicalPaths548LRUContention(b *testing.B) {
	s := batch2224Store(b, false)
	snapshots := seed2224(b, s, 5000, false)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s.lruMu.RLock()
				_, _ = s.lruGet(1, 0)
				s.lruMu.RUnlock()
			}
		}()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resetLRU(s)
		batch2224Sink = s.loadCanonicalResolvedPathsAt(snapshots, s.lruNow())
	}
	b.StopTimer()
	close(stop)
	wg.Wait()
	batch2224Sink = nil
}
