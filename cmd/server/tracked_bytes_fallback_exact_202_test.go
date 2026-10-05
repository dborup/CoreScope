package main

import (
	"fmt"
	"testing"
	"time"
)

// Issue #202, leak 2, continued: the amount charged and credited is exact, not
// just non-zero (tracked_bytes_fallback_202_test.go).

// The fallback's charge is exactly the record eviction later reads, per
// transmission, and every recorded relay is indexed under the transmission.
func TestFallbackRelays_ChargeIsExactlyTheRecord_202(t *testing.T) {
	for _, sc := range leak202Scenarios {
		t.Run(sc.name, func(t *testing.T) {
			with, without := sc.build(t, true), sc.build(t, false)
			with.mu.RLock()
			defer with.mu.RUnlock()

			var want int64
			records := 0
			for tx, pks := range with.fallbackByNode {
				records++
				want += fallbackRelayBytes(len(pks))
				seen := map[string]bool{}
				for _, pk := range pks {
					if seen[pk] {
						t.Fatalf("tx %d is recorded twice under %s", tx.ID, pk)
					}
					seen[pk] = true
					if !with.nodeHashes[pk][tx.Hash] || countTxInLocked(with.byNode[pk], tx) != 1 {
						t.Fatalf("tx %d is recorded under %s but byNode/nodeHashes do not hold it once", tx.ID, pk)
					}
				}
			}
			if records == 0 {
				t.Fatal("setup: no fallback records")
			}
			if got := with.trackedBytes - without.trackedBytes; got != want {
				t.Errorf("the fallback added %d bytes to trackedBytes, its records account for %d", got, want)
			}
			sum, stale := acct113Sum(with)
			if stale > 0 || with.trackedBytes != sum {
				t.Errorf("trackedBytes = %d, live charges sum to %d (%d stale)", with.trackedBytes, sum, stale)
			}
		})
	}
}

// Evicting one half keeps the other half exactly charged, with the fallback's
// records of the evicted half gone; evicting the rest empties the store.
func TestFallbackRelays_PartialEvictionStaysExact_202(t *testing.T) {
	s := leak202Scenarios[2].build(t, true) // BackgroundFill: 100 old, 100 recent
	s.mu.Lock()
	s.retentionHours = 24
	s.mu.Unlock()
	acct113Check(t, s, "before eviction")
	if n := s.RunEviction(); n != 100 {
		t.Fatalf("evicted %d transmissions, want the 100 old ones", n)
	}
	acct113Check(t, s, "after evicting the old half")
	s.mu.RLock()
	live := map[*StoreTx]bool{}
	for _, tx := range s.packets {
		live[tx] = true
	}
	for tx := range s.fallbackByNode {
		if !live[tx] {
			t.Fatalf("a fallback record of evicted tx %d is still held", tx.ID)
		}
	}
	if len(s.fallbackByNode) == 0 || s.trackedBytes <= 0 {
		t.Fatalf("setup: %d records and trackedBytes %d with half of the store live", len(s.fallbackByNode), s.trackedBytes)
	}
	s.mu.RUnlock()

	s.mu.Lock()
	for _, tx := range s.packets {
		tx.FirstSeen = time.Now().UTC().Add(-100 * time.Hour).Format(time.RFC3339)
	}
	s.mu.Unlock()
	if n := s.RunEviction(); n != 100 {
		t.Fatalf("second pass evicted %d, want 100", n)
	}
	if s.trackedBytes != 0 || len(s.fallbackByNode) != 0 || len(s.byNode) != 0 || len(s.nodeHashes) != 0 {
		t.Fatalf("emptied store: trackedBytes=%d records=%d byNode=%d nodeHashes=%d, want all 0",
			s.trackedBytes, len(s.fallbackByNode), len(s.byNode), len(s.nodeHashes))
	}
}

// One call to indexObservationRelayHops charges each new (relay, tx) pair once.
// Repeating it, or a relay the tx is already indexed under, charges nothing.
func TestIndexObservationRelayHops_FallbackChargesOncePerRelay_202(t *testing.T) {
	s := leak202Scenarios[0].build(t, false) // Load: no fallback relays yet
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := s.packets[0]
	// Resolve against the full node set, so the paths below find their nodes.
	var nodes []nodeInfo
	for k := 0; k < 256; k++ {
		nodes = append(nodes, nodeInfo{PublicKey: acct113PK(k), Role: "repeater"})
	}
	pm := buildPrefixMap(nodes)

	var fresh []int // relays tx is not indexed under yet
	for k := 0; k < 256 && len(fresh) < 3; k++ {
		if !s.nodeHashes[acct113PK(k)][tx.Hash] {
			fresh = append(fresh, k)
		}
	}
	path := func(ks ...int) string {
		j := "["
		for i, k := range ks {
			if i > 0 {
				j += ","
			}
			j += fmt.Sprintf(`"%02x"`, k)
		}
		return j + "]"
	}
	hopsSeen := map[string]bool{}
	index := func(pathJSON string) int64 {
		before := s.trackedBytes
		s.indexObservationRelayHops(tx, persistedRelayPath{}, pathJSON, "", pm, hopsSeen, nil)
		return s.trackedBytes - before
	}

	if d, want := index(path(fresh[0], fresh[1])), fallbackRelayBytes(2); d != want {
		t.Fatalf("two new relays charged %d, want %d (a record and two relays)", d, want)
	}
	if d := index(path(fresh[0], fresh[1])); d != 0 {
		t.Errorf("the same relays again charged %d, want 0", d)
	}
	if d, want := index(path(fresh[1], fresh[2])), fallbackRelayBytes(3)-fallbackRelayBytes(2); d != want {
		t.Errorf("one more relay charged %d, want %d (one relay, no second record)", d, want)
	}
	if n := len(s.fallbackByNode[tx]); n != 3 {
		t.Errorf("record holds %d relays, want 3", n)
	}

	// A relay the tx is already indexed under (a decoded pubkey, say) is not
	// the fallback's: no charge, no record.
	other := s.packets[1]
	var held int
	for k := 0; k < 256; k++ {
		if !s.nodeHashes[acct113PK(k)][other.Hash] {
			held = k
			break
		}
	}
	s.addToByNode(other, acct113PK(held))
	before := s.trackedBytes
	s.indexObservationRelayHops(other, persistedRelayPath{}, path(held), "", pm, hopsSeen, nil)
	if d := s.trackedBytes - before; d != 0 || len(s.fallbackByNode[other]) != 0 {
		t.Errorf("a relay already indexed charged %d and recorded %d, want 0 and 0", d, len(s.fallbackByNode[other]))
	}
}
