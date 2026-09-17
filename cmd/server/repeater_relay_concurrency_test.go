package main

import (
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Bulk relay info runs concurrently with ingest and eviction, which compacts
// byNode/byPathHop slices in place. Every bulk result must be the exact
// single-path result of one store state the reader could have observed:
// a candidate from a later state must never be credited (or deduplicated)
// as some other transmission. Run with -race.
func TestRepeaterRelayInfoMap_ConcurrentIngestAndEviction(t *testing.T) {
	relayA := "a1b2c3" + strings.Repeat("11", 29)
	relayB := "b7c8d9" + strings.Repeat("22", 29)
	hop := "d4e5f6" + strings.Repeat("33", 29)
	keys := []string{relayA, relayB, hop}
	db, store := activityTestStore(t, keys...)
	defer db.conn.Close()

	// Timestamps sit hours away from the 1h/24h windows, so results do not
	// depend on when each reader samples time.Now.
	base := time.Now().UTC().Add(-20 * time.Hour).Truncate(time.Second)
	const initialTx, rounds, txPerRound, keepTx = 200, 60, 20, 40
	nextID := 1
	resolved := map[int][]string{}
	hopsSeen := map[string]bool{}
	addLocked := func() {
		id := nextID
		nextID++
		ts := base.Add(time.Duration(id) * time.Second).Format(time.RFC3339)
		// Relay identity comes only from a shorter non-display observation;
		// alternate transmissions credit different relays.
		relay, alt := relayA, `["A1"]`
		if id%2 == 1 {
			relay, alt = relayB, `["B7C8"]`
		}
		tx := storeObservedTx(store, id, 2, ts, `{"type":"TXT_MSG"}`, alt, `["D4E5F6","D4E5F6","D4E5F6"]`)
		resolved[id] = []string{relay, hop}
		store.indexResolvedPathHops(tx, resolved[id], hopsSeen)
	}
	// oracle[round] is the single-path relay info for every key in the store
	// state published as round, computed under the writer's lock.
	var oracleMu sync.Mutex
	var oracle []map[string]RepeaterRelayInfo
	round := 0
	publishLocked := func() {
		want := make(map[string]RepeaterRelayInfo, len(keys))
		for _, key := range keys {
			want[key] = computeRelayInfoFromEntries(store.collectRelayEntriesLocked(key), 24)
		}
		oracleMu.Lock()
		oracle = append(oracle, want)
		oracleMu.Unlock()
	}
	store.mu.Lock()
	for i := 0; i < initialTx; i++ {
		addLocked()
	}
	publishLocked()
	store.mu.Unlock()

	readRound := func() int {
		store.mu.RLock()
		defer store.mu.RUnlock()
		return round
	}
	matchesSomeRound := func(got map[string]RepeaterRelayInfo, from, to int) bool {
		oracleMu.Lock()
		defer oracleMu.Unlock()
		for r := from; r <= to; r++ {
			same := true
			for _, key := range keys {
				if !reflect.DeepEqual(got[key], oracle[r][key]) {
					same = false
					break
				}
			}
			if same {
				return true
			}
		}
		return false
	}

	const readers = 3
	var wg sync.WaitGroup
	ready := make(chan struct{}, readers)
	stop := make(chan struct{})
	failures := make(chan string, readers)
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			ready <- struct{}{}
			for first := true; ; first = false {
				select {
				case <-stop:
					if !first {
						return
					}
				default:
				}
				from := readRound()
				if r == readers-1 {
					// Health shares the candidate buckets; exercised for races.
					_ = store.GetBulkHealth(10, "", "")
					continue
				}
				got := store.computeRepeaterRelayInfoMap(24)
				if to := readRound(); !matchesSomeRound(got, from, to) {
					failures <- "bulk relay info matches no store state between rounds"
					return
				}
			}
		}(r)
	}
	for r := 0; r < readers; r++ {
		<-ready
	}

	for i := 0; i < rounds; i++ {
		store.mu.Lock()
		for j := 0; j < txPerRound; j++ {
			addLocked()
		}
		// Evict all but the newest keepTx transmissions (1s apart; the cutoff
		// sits mid-second so clock progress during the round cannot move it).
		keepFrom := base.Add(time.Duration(nextID-keepTx)*time.Second - 500*time.Millisecond)
		store.retentionHours = time.Since(keepFrom).Hours()
		store.EvictStaleWithRP(resolved)
		round++
		publishLocked()
		store.mu.Unlock()
	}
	close(stop)
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Fatal(failure)
	}

	// Final synchronization point: no writer is active.
	got := store.computeRepeaterRelayInfoMap(24)
	if !matchesSomeRound(got, round, round) {
		t.Fatalf("final bulk relay info differs from single path: %+v", got)
	}
	// Each transmission credits exactly one relay plus the shared hop.
	// (Eviction leaves resolved full-key path-hop entries in place, so counts
	// can exceed len(store.packets); that policy is out of scope here.)
	final := oracle[round]
	if final[relayA].RelayCount24h == 0 || final[relayB].RelayCount24h == 0 || final[relayA].RelayCount24h+final[relayB].RelayCount24h != final[hop].RelayCount24h {
		t.Fatalf("fixture relay evidence inconsistent: %+v", final)
	}
}
