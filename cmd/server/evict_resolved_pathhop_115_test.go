package main

import (
	"fmt"
	"io"
	"log"
	"sync"
	"testing"
	"time"
)

// Issue #115: eviction must remove a transmission from every byPathHop
// bucket it is in. indexResolvedPathHops puts it under resolved full-pubkey
// keys as well as its raw wire hops, and several observations of one
// transmission legitimately add it to the same resolved key more than once.
// removeTxFromPathHopIndex derived its keys from the raw path only and
// removed one occurrence, so evicted transmissions stayed reachable (relay
// counts, transported scopes, retained memory) until the next full rebuild.

const (
	evict115PK1  = "a3f19c2b7d4e5081aa22bb33cc44dd55ee66ff778899000112233445566778899"
	evict115PK2  = "b7002211ffeeddccbbaa99887766554433221100aabbccddeeff001122334455"
	evict115Only = "c0ffee00112233445566778899aabbccddeeff00112233445566778899aabbcc" // only old txs
)

// evict115Store builds count transmissions (raw path aa,bb,cc), half of them
// older than the 24h retention, indexed through the real byPathHop helpers:
// raw hops, and resolved hops from two observations (duplicates).
func evict115Store(t testing.TB, count int, resolvedIndex bool) (*PacketStore, []*StoreTx, []*StoreTx) {
	t.Helper()
	now := time.Now().UTC()
	store := makeTestStore(count, now.Add(-48*time.Hour), 0)
	store.byPathHop = make(map[string][]*StoreTx)
	store.retentionHours = 24
	store.useResolvedPathIndex = resolvedIndex
	if resolvedIndex {
		store.initResolvedPathIndex()
	}
	var old, young []*StoreTx
	hopsSeen := map[string]bool{}
	for i, tx := range store.packets {
		if i >= count/2 {
			tx.FirstSeen = now.Add(-time.Hour).Format(time.RFC3339)
			young = append(young, tx)
		} else {
			old = append(old, tx)
		}
		addTxToPathHopIndex(store.byPathHop, tx)
		pks := []string{evict115PK1, evict115PK2}
		if i < count/2 {
			pks = append(pks, evict115Only)
		}
		for obs := 0; obs < 2; obs++ { // two observations -> duplicate entries
			store.indexResolvedPathHops(tx, pks, hopsSeen)
		}
	}
	return store, old, young
}

func countIn(idx map[string][]*StoreTx, tx *StoreTx) int {
	n := 0
	for _, list := range idx {
		for _, t := range list {
			if t == tx {
				n++
			}
		}
	}
	return n
}

func assertEvictedGone115(t *testing.T, store *PacketStore, old, young []*StoreTx, wantYoungRefs int) {
	t.Helper()
	assertEvictedGone115Partial(t, store, old, young, wantYoungRefs)
	if _, ok := store.byPathHop[evict115Only]; ok {
		t.Errorf("bucket %q held only evicted transmissions but was not deleted", evict115Only[:8])
	}
}

// assertEvictedGone115Partial is assertEvictedGone115 for a pass that
// evicted only some of the transmissions sharing evict115Only.
func assertEvictedGone115Partial(t *testing.T, store *PacketStore, old, young []*StoreTx, wantYoungRefs int) {
	t.Helper()
	for _, tx := range old {
		if n := countIn(store.byPathHop, tx); n != 0 {
			t.Fatalf("evicted tx %d is still in byPathHop %d times", tx.ID, n)
		}
	}
	for _, tx := range young {
		if n := countIn(store.byPathHop, tx); n != wantYoungRefs {
			t.Fatalf("surviving tx %d has %d byPathHop entries, want %d", tx.ID, n, wantYoungRefs)
		}
	}
	// no discarded slot of any bucket's backing array still points at an
	// evicted transmission (it would keep it alive for the GC)
	evicted := map[*StoreTx]bool{}
	for _, tx := range old {
		evicted[tx] = true
	}
	for key, list := range store.byPathHop {
		for i, tx := range list[:cap(list)] {
			if i >= len(list) && evicted[tx] {
				t.Fatalf("bucket %q keeps evicted tx %d in its backing array (slot %d, len %d)", key, tx.ID, i, len(list))
			}
		}
	}
}

// Survivors: 3 raw keys once each, 2 resolved keys twice each.
const young115Refs = 3 + 2*2

func TestEvictRemovesRawAndResolvedPathHops_115(t *testing.T) {
	for _, mode := range []bool{true, false} {
		t.Run(fmt.Sprintf("useResolvedPathIndex=%v", mode), func(t *testing.T) {
			store, old, young := evict115Store(t, 40, mode)
			if n := countIn(store.byPathHop, old[0]); n != 3+3*2 {
				t.Fatalf("fixture: old tx indexed %d times, want %d", n, 3+3*2)
			}
			if got := store.EvictStale(); got != len(old) {
				t.Fatalf("evicted %d, want %d", got, len(old))
			}
			assertEvictedGone115(t, store, old, young, young115Refs)
		})
	}
}

func TestRunEvictionRemovesResolvedPathHops_115(t *testing.T) {
	store, old, young := evict115Store(t, 40, true)
	if got := store.RunEviction(); got != len(old) {
		t.Fatalf("evicted %d, want %d", got, len(old))
	}
	assertEvictedGone115(t, store, old, young, young115Refs)
}

func TestMemoryEvictionRemovesResolvedPathHops_115(t *testing.T) {
	store, _, _ := evict115Store(t, 40, true)
	store.retentionHours = 0
	store.maxMemoryMB = 1
	store.trackedBytes = 2 * 1048576 // over the high watermark: the 25% cap applies
	before := append([]*StoreTx(nil), store.packets...)
	n := store.EvictStale()
	if n == 0 {
		t.Fatal("fixture: memory eviction evicted nothing")
	}
	assertEvictedGone115Partial(t, store, before[:n], nil, 0)
	for _, tx := range before[n : len(before)/2] { // older half not evicted: all 9 refs
		if got := countIn(store.byPathHop, tx); got != 3+3*2 {
			t.Fatalf("surviving tx %d has %d entries, want 9", tx.ID, got)
		}
	}
}

// A rebuild after eviction must not bring an evicted transmission back
// (buildPathHopIndex retains resolved keys from the previous index).
func TestRebuildAfterEvictionDoesNotResurrect_115(t *testing.T) {
	store, old, young := evict115Store(t, 40, true)
	store.EvictStale()
	store.buildPathHopIndex()
	for _, tx := range old {
		if n := countIn(store.byPathHop, tx); n != 0 {
			t.Fatalf("rebuild resurrected evicted tx %d (%d entries)", tx.ID, n)
		}
	}
	for _, tx := range young {
		if countIn(store.byPathHop, tx) == 0 {
			t.Fatalf("rebuild lost surviving tx %d", tx.ID)
		}
	}
}

func TestEvictionInvalidatesRelayStatsCache_115(t *testing.T) {
	store, _, _ := evict115Store(t, 40, true)
	store.relayStatsCache = map[string]RepeaterNodeStats{evict115PK1: {}}
	store.EvictStale()
	if store.relayStatsCache != nil {
		t.Fatal("relay stats cache survived an eviction that changed byPathHop")
	}
}

// Relay readers take the read lock and materialize what they need under it;
// eviction compacts buckets under the write lock. Run both concurrently under
// -race, then check the index.
func TestRelayStatsConcurrentWithEviction_115(t *testing.T) {
	store, old, young := evict115Store(t, 400, true)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					store.GetRepeaterNodeStatsBatch([]string{evict115PK1, evict115PK2, evict115Only}, 72)
				}
			}
		}()
	}
	store.RunEviction()
	close(stop)
	wg.Wait()
	assertEvictedGone115(t, store, old, young, young115Refs)
}

// BenchmarkEvictPathHops_115 evicts a realistic batch (1% of the store,
// the oldest) from a store of n transmissions with 3 raw 1-byte hops and
// 2 resolved hops each (two observations, so 4 resolved entries).
func BenchmarkEvictPathHops_115(b *testing.B) {
	prev := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(prev)
	for _, n := range []int{20000, 100000} {
		b.Run(fmt.Sprintf("txs=%d", n), func(b *testing.B) {
			batch := n / 100
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				store := evict115BenchStore(n)
				evictTo := store.packets[batch].FirstSeen
				store.retentionHours = time.Since(mustParseRFC3339(evictTo)).Hours()
				b.StartTimer()
				store.EvictStale()
			}
		})
	}
}

func mustParseRFC3339(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// evict115BenchStore: transmissions one second apart, 3 raw hops drawn
// from 256 one-byte prefixes, 2 resolved keys drawn from 2000 relays.
func evict115BenchStore(n int) *PacketStore {
	start := time.Now().UTC().Add(-time.Duration(n+10) * time.Second)
	store := makeTestStore(0, start, 0)
	store.byPathHop = make(map[string][]*StoreTx)
	hopsSeen := map[string]bool{}
	for i := 0; i < n; i++ {
		h1, h2, h3 := i%256, (i*7+3)%256, (i*13+5)%256
		tx := &StoreTx{
			ID:        i + 1,
			Hash:      fmt.Sprintf("bench%07d", i),
			FirstSeen: start.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
			PathJSON:  fmt.Sprintf(`["%02x","%02x","%02x"]`, h1, h2, h3),
		}
		store.packets = append(store.packets, tx)
		store.byHash[tx.Hash] = tx
		store.byTxID[tx.ID] = tx
		addTxToPathHopIndex(store.byPathHop, tx)
		pks := []string{fmt.Sprintf("%064x", i%2000), fmt.Sprintf("%064x", 5000+(i*31)%2000)}
		store.indexResolvedPathHops(tx, pks, hopsSeen)
		store.indexResolvedPathHops(tx, pks, hopsSeen)
	}
	return store
}

// Background chunks load older transmissions after newer ones, so an evicted
// transmission is often NOT at the front of a raw bucket. Removing it must
// not leave its pointer in the bucket's backing array.
func TestEvictDoesNotRetainPointerInRawBucketTail_115(t *testing.T) {
	now := time.Now().UTC()
	store := makeTestStore(2, now.Add(-time.Hour), 0)
	store.byPathHop = make(map[string][]*StoreTx)
	store.retentionHours = 24
	young, old := store.packets[1], store.packets[0]
	old.FirstSeen = now.Add(-48 * time.Hour).Format(time.RFC3339)
	addTxToPathHopIndex(store.byPathHop, young) // indexed first
	addTxToPathHopIndex(store.byPathHop, old)   // e.g. from a later, older chunk
	if got := store.EvictStale(); got != 1 {
		t.Fatalf("evicted %d, want 1", got)
	}
	assertEvictedGone115(t, store, []*StoreTx{old}, []*StoreTx{young}, 3)
}
