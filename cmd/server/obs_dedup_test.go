package main

import (
	"fmt"
	"strings"
	"testing"
)

// obsDedupAdd adds an observation through the dedup the store uses and
// reports whether it was kept.
func obsDedupAdd(tx *StoreTx, id int, observerID, pathJSON string) bool {
	dup, known := tx.lookupObservation(observerID, pathJSON)
	if dup {
		return false
	}
	tx.addObservation(&StoreObs{ID: id, ObserverID: observerID, PathJSON: pathJSON}, known)
	return true
}

// TestObsDedupCorrectness verifies that the dedup produces correct results:
// no duplicate observations (same observerID + pathJSON) on a single
// transmission, on either side of obsIndexThreshold.
func TestObsDedupCorrectness(t *testing.T) {
	for _, n := range []int{5, obsIndexThreshold + 5} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			tx := &StoreTx{ID: 1, Hash: "abc123"}
			for i := 0; i < n; i++ {
				if !obsDedupAdd(tx, i, fmt.Sprintf("obs-%d", i), fmt.Sprintf(`["path-%d"]`, i)) {
					t.Fatalf("observation %d should not be a duplicate", i)
				}
			}
			if tx.ObservationCount != n || int(tx.UniqueObserverCount) != n {
				t.Fatalf("expected %d observations by %d observers, got %d/%d", n, n, tx.ObservationCount, tx.UniqueObserverCount)
			}
			// Try to add duplicates of each — all should be rejected
			for i := 0; i < n; i++ {
				if obsDedupAdd(tx, 1000+i, fmt.Sprintf("obs-%d", i), fmt.Sprintf(`["path-%d"]`, i)) {
					t.Fatalf("observation %d should be detected as duplicate", i)
				}
			}
			// Same observer, different path — should NOT be a duplicate
			if dup, known := tx.lookupObservation("obs-0", `["different-path"]`); dup || !known {
				t.Fatalf("different path: dup=%v known=%v, want false/true", dup, known)
			}
			// Different observer, same path — should NOT be a duplicate
			if dup, known := tx.lookupObservation("obs-new", `["path-0"]`); dup || known {
				t.Fatalf("different observer: dup=%v known=%v, want false/false", dup, known)
			}
		})
	}
}

// TestObsDedupNilMapSafety ensures a transmission without maps (every
// transmission below obsIndexThreshold) deduplicates correctly.
func TestObsDedupNilMapSafety(t *testing.T) {
	tx := &StoreTx{ID: 1, Hash: "abc"}
	if dup, _ := tx.lookupObservation("obs1", "path1"); dup {
		t.Fatal("should not be duplicate on an empty transmission")
	}
	obsDedupAdd(tx, 1, "obs1", "path1")
	if dup, _ := tx.lookupObservation("obs1", "path1"); !dup {
		t.Fatal("should be duplicate after insert")
	}
	if tx.obsKeys != nil || tx.observerSet != nil {
		t.Fatal("a transmission with one observation allocated its dedup maps")
	}
}

// obsDedupBenchTx is a transmission with k observations. realistic: 48
// distinct 64-hex observers with short paths; adversarial: one observer on
// k paths of equal length that differ only at the end, the worst case for
// the scan below obsIndexThreshold.
func obsDedupBenchTx(k int, adversarial bool) (tx *StoreTx, missObserver, missPath string) {
	tx = &StoreTx{ID: 1}
	for i := 0; i < k; i++ {
		o, p := obs411ObserverID(i%48), fmt.Sprintf(`["%02x","%02x"]`, i/48, i%7)
		if adversarial {
			o, p = obs411ObserverID(0), fmt.Sprintf(`["aa","bb","cc","%04d"]`, i)
		}
		obsDedupAdd(tx, i, o, p)
	}
	if adversarial {
		return tx, strings.Clone(obs411ObserverID(0)), `["aa","bb","cc","zzzz"]`
	}
	return tx, strings.Clone(obs411ObserverID(1)), `["ff","ff"]`
}

// BenchmarkObsDedup is a duplicate check that misses (the common case on
// ingest) on a transmission with k observations; obsIndexThreshold is
// chosen from these numbers (#411). "concat" is the pre-#411 check, a
// map lookup with an allocated "observerID|pathJSON" key, for reference.
func BenchmarkObsDedup(b *testing.B) {
	for _, adversarial := range []bool{false, true} {
		for _, k := range []int{4, 8, 16, 24, 31, 32, 64, 128, 600} {
			tx, o, p := obsDedupBenchTx(k, adversarial)
			name := fmt.Sprintf("adversarial=%v/k=%d", adversarial, k)
			b.Run(name, func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					tx.lookupObservation(o, p)
				}
			})
			old := make(map[string]bool, k)
			for _, x := range tx.Observations {
				old[x.ObserverID+"|"+x.PathJSON] = true
			}
			b.Run(name+"/concat", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					_ = old[o+"|"+p]
				}
			})
		}
	}
}
