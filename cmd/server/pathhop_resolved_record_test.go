package main

import (
	"strings"
	"testing"
	"time"
)

// The per-transmission record behind the path-hop dedupe (pathHopResolved)
// holds one hash per resolved relay key of a live transmission. These tests
// pin its lifecycle: bounded by observations, removed on eviction, kept (for
// live transmissions only) across an index rebuild.

var (
	phdRecordKey1 = strings.Repeat("a", 64)
	phdRecordKey2 = strings.Repeat("b", 64)
)

// Raw hops of every transmission built by phdRecordStore (makeTestStore's
// PathJSON), plus the two resolved keys above.
const phdRecordRefsPerTx = 3 + 2

// phdRecordStore builds count non-advert transmissions, the older half past
// the 1 h retention, each indexed under its raw hops and two resolved keys
// through the real byPathHop helpers.
func phdRecordStore(count int) (store *PacketStore, old, young []*StoreTx) {
	now := time.Now().UTC()
	store = makeTestStore(count, now.Add(-30*time.Minute), 0)
	store.byPayloadType[5] = store.byPayloadType[4]
	delete(store.byPayloadType, 4)
	store.byPathHop = make(map[string][]*StoreTx)
	store.useResolvedPathIndex = true
	store.initResolvedPathIndex()
	store.retentionHours = 1
	hopsSeen := make(map[string]bool)
	for i, tx := range store.packets {
		*tx.PayloadType = 5
		if i < count/2 {
			tx.FirstSeen = now.Add(-2 * time.Hour).Format(time.RFC3339)
			old = append(old, tx)
		} else {
			young = append(young, tx)
		}
		addTxToPathHopIndex(store.byPathHop, tx)
		store.indexResolvedPathHops(tx, []string{phdRecordKey1, phdRecordKey2}, hopsSeen)
	}
	return store, old, young
}

func phdEntriesOf(store *PacketStore, tx *StoreTx) int {
	n := 0
	for _, list := range store.byPathHop {
		for _, t := range list {
			if t == tx {
				n++
			}
		}
	}
	return n
}

// The record does not grow with observations, and eviction removes it
// together with the transmission's byPathHop entries.
func TestPathHopResolvedRecordBounded_PathHopDedupe(t *testing.T) {
	store, old, young := phdRecordStore(40)
	hopsSeen := map[string]bool{}
	for _, tx := range store.packets {
		for k := 0; k < 10; k++ {
			store.indexResolvedPathHops(tx, []string{phdRecordKey1, phdRecordKey2}, hopsSeen)
		}
	}
	if got := len(store.pathHopResolved); got != len(old)+len(young) {
		t.Fatalf("record holds %d transmissions, want %d", got, len(old)+len(young))
	}
	for _, tx := range young {
		if got := len(store.pathHopResolved[tx]); got != 2 {
			t.Fatalf("tx %d record holds %d keys after 11 observations, want 2", tx.ID, got)
		}
		if n := phdEntriesOf(store, tx); n != phdRecordRefsPerTx {
			t.Fatalf("tx %d has %d byPathHop entries after 11 observations, want %d", tx.ID, n, phdRecordRefsPerTx)
		}
	}

	store.mu.Lock()
	evicted := store.EvictStale()
	store.mu.Unlock()
	if evicted != len(old) {
		t.Fatalf("evicted %d, want %d", evicted, len(old))
	}
	for _, tx := range old {
		if _, ok := store.pathHopResolved[tx]; ok {
			t.Fatalf("evicted tx %d is still in the indexed-key record", tx.ID)
		}
		if n := phdEntriesOf(store, tx); n != 0 {
			t.Fatalf("evicted tx %d is still in %d byPathHop buckets", tx.ID, n)
		}
	}
	if got := len(store.pathHopResolved); got != len(young) {
		t.Fatalf("record holds %d transmissions after eviction, want %d", got, len(young))
	}
}

// A rebuild keeps the record of live transmissions (their resolved entries
// are carried over), so observations after it still add nothing; it drops
// the record of transmissions that are no longer in the store.
func TestPathHopResolvedRecordAcrossRebuild_PathHopDedupe(t *testing.T) {
	store, _, young := phdRecordStore(40)
	gone := store.packets[0]
	store.packets = store.packets[1:]
	delete(store.byTxID, gone.ID)
	delete(store.byHash, gone.Hash)

	store.mu.Lock()
	store.buildPathHopIndex()
	store.mu.Unlock()

	if _, ok := store.pathHopResolved[gone]; ok {
		t.Fatal("rebuild kept the record of a transmission no longer in the store")
	}
	hopsSeen := map[string]bool{}
	for _, tx := range young {
		if _, ok := store.pathHopResolved[tx]; !ok {
			t.Fatalf("rebuild dropped the record of live tx %d", tx.ID)
		}
		store.indexResolvedPathHops(tx, []string{phdRecordKey1, phdRecordKey2}, hopsSeen)
		if n := phdEntriesOf(store, tx); n != phdRecordRefsPerTx {
			t.Fatalf("tx %d has %d entries after a rebuild and another observation, want %d", tx.ID, n, phdRecordRefsPerTx)
		}
	}
}

func TestCountDistinctNonAdvert_PathHopDedupe(t *testing.T) {
	pt, advert := 2, payloadTypeAdvert
	tx := func(id int) *StoreTx { return &StoreTx{ID: id, PayloadType: &pt} }
	t1, t2, t3 := tx(1), tx(2), tx(3)
	ad := &StoreTx{ID: 4, PayloadType: &advert}
	cases := []struct {
		name string
		list []*StoreTx
		want int
	}{
		{"empty", nil, 0},
		{"ascending", []*StoreTx{t1, t2, t3}, 3},
		{"descending (chunk load order)", []*StoreTx{t3, t2, t1}, 3},
		{"adjacent duplicate", []*StoreTx{t1, t1}, 1},
		{"ascending then duplicate", []*StoreTx{t1, t2, t3, t3}, 3},
		{"interleaved duplicates", []*StoreTx{t1, t2, t1, t3, t2}, 3},
		{"adverts and nils skipped", []*StoreTx{nil, ad, t2, ad, nil, t2}, 1},
	}
	var ids []int
	for _, c := range cases {
		var got int
		got, ids = countDistinctNonAdvert(c.list, ids)
		if got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// When the index being rebuilt holds no resolved entries at all, nothing is
// indexed under any resolved key any more, so the record must not claim
// otherwise: the next observation has to put the transmission back.
func TestPathHopResolvedRecordClearedWithEmptyIndex_PathHopDedupe(t *testing.T) {
	store, _, young := phdRecordStore(40)
	store.byPathHop = make(map[string][]*StoreTx) // nothing carried over

	store.mu.Lock()
	store.buildPathHopIndex()
	store.mu.Unlock()

	if got := len(store.pathHopResolved); got != 0 {
		t.Fatalf("record still holds %d transmissions after a rebuild from an empty index", got)
	}
	hopsSeen := map[string]bool{}
	for _, tx := range young {
		store.indexResolvedPathHops(tx, []string{phdRecordKey1, phdRecordKey2}, hopsSeen)
		if n := phdEntriesOf(store, tx); n != phdRecordRefsPerTx {
			t.Fatalf("tx %d has %d entries after the rebuild and another observation, want %d", tx.ID, n, phdRecordRefsPerTx)
		}
	}
}
