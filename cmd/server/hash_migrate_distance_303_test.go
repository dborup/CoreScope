package main

import (
	"fmt"
	"testing"
	"time"
)

// Issue #303, follow-up to the review of #297.
//
//  1. A merge that moves first_seen *and* changes the path was not covered:
//     the XOR mutant `pathChanged != moved` in finishHashMerge survived every
//     test, and so did skipping pickBestObservation for a moved survivor.
//  2. The distance records carry a copy of the transmission's content hash. An
//     in-memory rehash that neither moved first_seen nor changed the path left
//     the old hash in distHops/distPaths until the next full build, and
//     /api/analytics/distance served it in topPaths[].hash.
//  3. Before the lazy distance build has run, the migration still appends
//     records for merge survivors into the unbuilt index. That is harmless
//     because buildDistanceIndex replaces both slices wholesale; the test
//     below pins it.

// hm303SetupSingle seeds one stale-hash transmission (id 70) observed over
// path, with the distance index built before the migration runs. It returns
// the store and the hash the migration must give the transmission. The row has
// a raw packet of its own, so the migration rehashes it without a collision:
// neither first_seen nor the path moves.
func hm303SetupSingle(t *testing.T, resolvedIndex bool, at, path string) (*PacketStore, string) {
	t.Helper()
	d := hm215Create(t)
	raw := hm215Raw(11)
	d.tx(t, 70, raw, "stale-303-70", at, 4, hm215Advert(acct113PK(150)))
	d.observation(t, 70, 1, path, "", at)
	s := hm215OpenIdx(t, d, resolvedIndex)
	hm288SeedGPSNodes(s)
	s.mu.Lock()
	s.buildDistanceIndex()
	s.mu.Unlock()
	hops, paths := hm288Records(s, 70)
	if len(hops) == 0 || len(paths) != 1 {
		t.Fatalf("setup: tx 70 has %d hop and %d path records before the migration, want some and 1", len(hops), len(paths))
	}
	if hops[0].Hash != "stale-303-70" || paths[0].Hash != "stale-303-70" {
		t.Fatalf("setup: distance records carry hop hash %q and path hash %q, want the stale %q", hops[0].Hash, paths[0].Hash, "stale-303-70")
	}
	return s, ComputeContentHash(raw)
}

// hm303TopPathHashes returns the hashes of topPaths in the distance analytics.
func hm303TopPathHashes(t *testing.T, s *PacketStore) []string {
	t.Helper()
	res := s.GetAnalyticsDistance("", "")
	top, ok := res["topPaths"].([]map[string]interface{})
	if !ok {
		t.Fatalf("topPaths is %T, want []map[string]interface{}", res["topPaths"])
	}
	out := make([]string, 0, len(top))
	for _, p := range top {
		h, _ := p["hash"].(string)
		out = append(out, h)
	}
	return out
}

// Point 1: the merge moves first_seen and changes the path in one go. Both
// halves of the condition in finishHashMerge fire, so neither an XOR of them
// nor a skipped pickBestObservation may pass.
func TestHashMigrate_DistanceIndexFollowsFirstSeenAndPathChange_303(t *testing.T) {
	late := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Hour).Add(29 * time.Minute)
	early := late.Add(-2 * time.Hour)
	lateAt, earlyAt := late.Format(time.RFC3339), early.Format(time.RFC3339)
	const shortPath, longPath = `["05","06"]`, `["05","06","07"]`
	wantHash := ComputeContentHash(hm215Raw(9)) // the raw packet hm288Setup uses
	for _, resolvedIndex := range []bool{true, false} {
		for _, batch := range []int{1, 100} {
			t.Run(fmt.Sprintf("resolvedIndex=%v/batch%d", resolvedIndex, batch), func(t *testing.T) {
				// Survivor 60 is the later row with the short path; duplicate 61
				// is earlier and has the longer one.
				s := hm288Setup(t, resolvedIndex, lateAt, earlyAt, shortPath, longPath)
				beforeHops, beforePaths := hm288Records(s, 60)
				hm215Migrate(s, batch)

				s.mu.RLock()
				w := s.byTxID[60]
				gone := s.byTxID[61] == nil
				s.mu.RUnlock()
				if w == nil || !gone {
					t.Fatalf("merge did not happen: survivor present=%v, duplicate gone=%v", w != nil, gone)
				}
				if w.FirstSeen != earlyAt || w.PathJSON != longPath {
					t.Fatalf("survivor first_seen=%s path=%s, want %s and %s (both must move)", w.FirstSeen, w.PathJSON, earlyAt, longPath)
				}
				hops, paths := hm288Records(s, 60)
				if len(hops) != len(beforeHops)+1 || len(paths) != 1 {
					t.Fatalf("survivor has %d hop and %d path records, want %d and 1", len(hops), len(paths), len(beforeHops)+1)
				}
				for i, r := range hops {
					if r.Timestamp != earlyAt || r.HourBucket != earlyAt[:13] {
						t.Errorf("hop record %d: time %s, hour bucket %s; want %s and %s", i, r.Timestamp, r.HourBucket, earlyAt, earlyAt[:13])
					}
					if r.Hash != wantHash {
						t.Errorf("hop record %d: hash %s, want the rehashed %s", i, r.Hash, wantHash)
					}
				}
				if paths[0].Timestamp != earlyAt {
					t.Errorf("path record: time %s, want %s", paths[0].Timestamp, earlyAt)
				}
				if paths[0].HopCount != beforePaths[0].HopCount+1 {
					t.Errorf("path record: %d hops, want %d", paths[0].HopCount, beforePaths[0].HopCount+1)
				}
				if paths[0].Hash != wantHash {
					t.Errorf("path record: hash %s, want the rehashed %s", paths[0].Hash, wantHash)
				}
				if h, p := hm288Records(s, 61); len(h) != 0 || len(p) != 0 {
					t.Errorf("the merged-away duplicate still has %d hop and %d path records", len(h), len(p))
				}
			})
		}
	}
}

// Point 2a: a rehash with no collision at all. Nothing moves, so nothing used
// to refresh the records, and they kept the old content hash.
func TestHashMigrate_DistanceIndexHashAfterNoMoveRehash_303(t *testing.T) {
	at := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Hour).Add(29 * time.Minute).Format(time.RFC3339)
	const path = `["05","06"]`
	for _, resolvedIndex := range []bool{true, false} {
		for _, batch := range []int{1, 100} {
			t.Run(fmt.Sprintf("resolvedIndex=%v/batch%d", resolvedIndex, batch), func(t *testing.T) {
				s, wantHash := hm303SetupSingle(t, resolvedIndex, at, path)
				// Warm the TTL cache with the pre-migration hash: the refresh
				// must invalidate it, or the API keeps serving the old one.
				if got := hm303TopPathHashes(t, s); len(got) != 1 || got[0] != "stale-303-70" {
					t.Fatalf("setup: topPaths hashes %v, want [stale-303-70]", got)
				}
				hm215Migrate(s, batch)

				s.mu.RLock()
				w := s.byTxID[70]
				s.mu.RUnlock()
				if w == nil {
					t.Fatal("tx 70 is gone")
				}
				if w.Hash != wantHash {
					t.Fatalf("tx 70 hash %s, want the rehashed %s", w.Hash, wantHash)
				}
				if w.FirstSeen != at || w.PathJSON != path {
					t.Fatalf("tx 70 first_seen=%s path=%s, want the unchanged %s and %s", w.FirstSeen, w.PathJSON, at, path)
				}
				hops, paths := hm288Records(s, 70)
				if len(hops) == 0 || len(paths) != 1 {
					t.Fatalf("tx 70 has %d hop and %d path records after the migration, want some and 1", len(hops), len(paths))
				}
				for i, r := range hops {
					if r.Hash != wantHash {
						t.Errorf("hop record %d: hash %s, want the rehashed %s", i, r.Hash, wantHash)
					}
				}
				if paths[0].Hash != wantHash {
					t.Errorf("path record: hash %s, want the rehashed %s", paths[0].Hash, wantHash)
				}
				if got := hm303TopPathHashes(t, s); len(got) != 1 || got[0] != wantHash {
					t.Errorf("topPaths hashes %v, want [%s]", got, wantHash)
				}
			})
		}
	}
}

// Point 2b: the same staleness through a merge whose survivor is already the
// earliest row and keeps its path, so neither half of the #297 condition fires
// while the hash still changes.
func TestHashMigrate_DistanceIndexHashAfterMergeWithoutMove_303(t *testing.T) {
	early := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Hour).Add(29 * time.Minute)
	earlyAt, lateAt := early.Format(time.RFC3339), early.Add(2*time.Hour).Format(time.RFC3339)
	const path = `["05","06"]`
	wantHash := ComputeContentHash(hm215Raw(9))
	for _, resolvedIndex := range []bool{true, false} {
		t.Run(fmt.Sprintf("resolvedIndex=%v", resolvedIndex), func(t *testing.T) {
			// Survivor 60 is the earliest and both rows share the path.
			s := hm288Setup(t, resolvedIndex, earlyAt, lateAt, path, path)
			hm215Migrate(s, 100)

			s.mu.RLock()
			w := s.byTxID[60]
			gone := s.byTxID[61] == nil
			s.mu.RUnlock()
			if w == nil || !gone {
				t.Fatalf("merge did not happen: survivor present=%v, duplicate gone=%v", w != nil, gone)
			}
			if w.FirstSeen != earlyAt || w.PathJSON != path {
				t.Fatalf("survivor first_seen=%s path=%s, want the unchanged %s and %s", w.FirstSeen, w.PathJSON, earlyAt, path)
			}
			hops, paths := hm288Records(s, 60)
			if len(hops) == 0 || len(paths) != 1 {
				t.Fatalf("survivor has %d hop and %d path records, want some and 1", len(hops), len(paths))
			}
			for i, r := range hops {
				if r.Hash != wantHash {
					t.Errorf("hop record %d: hash %s, want the rehashed %s", i, r.Hash, wantHash)
				}
			}
			if paths[0].Hash != wantHash {
				t.Errorf("path record: hash %s, want the rehashed %s", paths[0].Hash, wantHash)
			}
		})
	}
}

// Point 3: with the lazy distance build still pending, the migration appends
// records for its survivors into the empty index. That is harmless only
// because buildDistanceIndex replaces both slices wholesale instead of adding
// to them: this pins that no transmission ends up with duplicate records.
func TestHashMigrate_DistanceRecordsBeforeLazyBuildAreReplaced_303(t *testing.T) {
	early := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Hour).Add(29 * time.Minute)
	earlyAt, lateAt := early.Format(time.RFC3339), early.Add(2*time.Hour).Format(time.RFC3339)
	const shortPath, longPath = `["05","06"]`, `["05","06","07"]`

	d := hm215Create(t)
	raw := hm215Raw(9)
	sender := hm215Advert(acct113PK(150))
	d.tx(t, 60, raw, "stale-303-60", earlyAt, 4, sender)
	d.tx(t, 61, raw, "stale-303-61", lateAt, 4, sender)
	d.observation(t, 60, 1, shortPath, "", earlyAt)
	d.observation(t, 61, 2, longPath, "", lateAt)
	s := hm215OpenIdx(t, d, true)
	hm288SeedGPSNodes(s)

	// No buildDistanceIndex: this is the unbuilt index the handler answers
	// 202 for.
	s.mu.RLock()
	unbuilt := len(s.distHops) + len(s.distPaths)
	s.mu.RUnlock()
	if unbuilt != 0 {
		t.Fatalf("setup: the distance index already holds %d records", unbuilt)
	}

	hm215Migrate(s, 100)

	// The survivor's path changed, so the migration recomputed its records
	// into the unbuilt index.
	appendedHops, appendedPaths := hm288Records(s, 60)
	if len(appendedHops) == 0 || len(appendedPaths) != 1 {
		t.Fatalf("precondition: the migration left %d hop and %d path records for the survivor in the unbuilt index, want some and 1",
			len(appendedHops), len(appendedPaths))
	}

	s.mu.Lock()
	s.buildDistanceIndex()
	s.mu.Unlock()

	// One path record per transmission and no repeated hop, i.e. the build
	// replaced what the migration appended rather than adding to it.
	s.mu.RLock()
	pathsPerTx := make(map[int]int)
	for _, r := range s.distPaths {
		if r.tx != nil {
			pathsPerTx[r.tx.ID]++
		}
	}
	hopsPerKey := make(map[string]int)
	for _, r := range s.distHops {
		if r.tx != nil {
			hopsPerKey[fmt.Sprintf("%d|%s|%s", r.tx.ID, r.FromPk, r.ToPk)]++
		}
	}
	total := len(s.distPaths)
	s.mu.RUnlock()

	if total == 0 {
		t.Fatal("the rebuilt index holds no path records")
	}
	for id, n := range pathsPerTx {
		if n != 1 {
			t.Errorf("tx %d has %d path records after the rebuild, want 1", id, n)
		}
	}
	for key, n := range hopsPerKey {
		if n != 1 {
			t.Errorf("hop %s appears %d times after the rebuild, want once", key, n)
		}
	}
	if p := pathsPerTx[61]; p != 0 {
		t.Errorf("the merged-away duplicate has %d path records after the rebuild", p)
	}
}
