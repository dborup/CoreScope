package main

import (
	"fmt"
	"testing"
	"time"
)

// Issue #288: after an in-memory merge the distance index was refreshed only
// for survivors whose best path changed. A survivor that took an earlier
// first_seen from its duplicate kept distance records with the old time and
// hour bucket. Both changes must refresh it.

// hm288SeedGPSNodes replaces the store's node cache with the sender and the two
// relays of hm288Path, all with GPS a few km apart, so a transmission over that
// path yields distance records.
func hm288SeedGPSNodes(s *PacketStore) {
	nodes := []nodeInfo{
		{PublicKey: acct113PK(150), Name: "sender", Role: "repeater", Lat: 55.00, Lon: 12.00, HasGPS: true},
		{PublicKey: acct113PK(5), Name: "relay5", Role: "repeater", Lat: 55.02, Lon: 12.00, HasGPS: true},
		{PublicKey: acct113PK(6), Name: "relay6", Role: "repeater", Lat: 55.04, Lon: 12.00, HasGPS: true},
		{PublicKey: acct113PK(7), Name: "relay7", Role: "repeater", Lat: 55.06, Lon: 12.00, HasGPS: true},
	}
	s.cacheMu.Lock()
	s.nodeCache = nodes
	s.nodePM = buildPrefixMap(nodes)
	s.nodeCacheTime = time.Now()
	s.cacheMu.Unlock()
}

// hm288Records returns the distance hop records and path records of tx.
func hm288Records(s *PacketStore, txID int) (hops []distHopRecord, paths []distPathRecord) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.distHops {
		if r.tx != nil && r.tx.ID == txID {
			hops = append(hops, r)
		}
	}
	for _, r := range s.distPaths {
		if r.tx != nil && r.tx.ID == txID {
			paths = append(paths, r)
		}
	}
	return hops, paths
}

// hm288Setup builds a colliding pair: survivor 60 (lowest id) and duplicate 61,
// first_seen survivorAt and dupAt, observed over survivorPath and dupPath. The
// distance index is built before the migration runs.
func hm288Setup(t *testing.T, resolvedIndex bool, survivorAt, dupAt, survivorPath, dupPath string) *PacketStore {
	t.Helper()
	d := hm215Create(t)
	raw := hm215Raw(9)
	sender := hm215Advert(acct113PK(150))
	d.tx(t, 60, raw, "stale-288-60", survivorAt, 4, sender)
	d.tx(t, 61, raw, "stale-288-61", dupAt, 4, sender)
	d.observation(t, 60, 1, survivorPath, "", survivorAt)
	d.observation(t, 61, 2, dupPath, "", dupAt)
	s := hm215OpenIdx(t, d, resolvedIndex)
	hm288SeedGPSNodes(s)
	s.mu.Lock()
	s.buildDistanceIndex()
	s.mu.Unlock()
	for _, id := range []int{60, 61} {
		if hops, paths := hm288Records(s, id); len(hops) == 0 || len(paths) != 1 {
			t.Fatalf("setup: tx %d has %d hop and %d path records before the migration, want some and 1", id, len(hops), len(paths))
		}
	}
	return s
}

func TestHashMigrate_DistanceIndexFollowsFirstSeenMove_288(t *testing.T) {
	// 13:29 and 11:29 of the same day, as in the issue: a different hour bucket.
	late := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Hour).Add(29 * time.Minute)
	early := late.Add(-2 * time.Hour)
	lateAt, earlyAt := late.Format(time.RFC3339), early.Format(time.RFC3339)
	const path = `["05","06"]`
	for _, resolvedIndex := range []bool{true, false} {
		for _, batch := range []int{1, 100} {
			t.Run(fmt.Sprintf("resolvedIndex=%v/batch%d", resolvedIndex, batch), func(t *testing.T) {
				// Same path on both rows: the merge moves first_seen only.
				s := hm288Setup(t, resolvedIndex, lateAt, earlyAt, path, path)
				beforeHops, _ := hm288Records(s, 60)
				if beforeHops[0].Timestamp != lateAt {
					t.Fatalf("setup: survivor's distance time is %s before the migration, want %s", beforeHops[0].Timestamp, lateAt)
				}
				hm215Migrate(s, batch)

				s.mu.RLock()
				w := s.byTxID[60]
				gone := s.byTxID[61] == nil
				s.mu.RUnlock()
				if w == nil || !gone {
					t.Fatalf("merge did not happen: survivor present=%v, duplicate gone=%v", w != nil, gone)
				}
				if w.FirstSeen != earlyAt || w.PathJSON != path {
					t.Fatalf("survivor first_seen=%s path=%s, want %s and the unchanged %s", w.FirstSeen, w.PathJSON, earlyAt, path)
				}
				hops, paths := hm288Records(s, 60)
				if len(hops) != len(beforeHops) || len(paths) != 1 {
					t.Fatalf("survivor has %d hop and %d path records, want %d and 1", len(hops), len(paths), len(beforeHops))
				}
				for i, r := range hops {
					if r.Timestamp != earlyAt || r.HourBucket != earlyAt[:13] {
						t.Errorf("hop record %d: time %s, hour bucket %s; want %s and %s", i, r.Timestamp, r.HourBucket, earlyAt, earlyAt[:13])
					}
				}
				if paths[0].Timestamp != earlyAt {
					t.Errorf("path record: time %s, want %s", paths[0].Timestamp, earlyAt)
				}
				if h, p := hm288Records(s, 61); len(h) != 0 || len(p) != 0 {
					t.Errorf("the merged-away duplicate still has %d hop and %d path records", len(h), len(p))
				}
			})
		}
	}
}

// The path-change refresh still happens when first_seen does not move: the
// duplicate is later and has the longer path, which becomes the survivor's.
func TestHashMigrate_DistanceIndexFollowsPathChange_288(t *testing.T) {
	early := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Hour).Add(29 * time.Minute)
	earlyAt, lateAt := early.Format(time.RFC3339), early.Add(2*time.Hour).Format(time.RFC3339)
	const shortPath, longPath = `["05","06"]`, `["05","06","07"]`
	for _, resolvedIndex := range []bool{true, false} {
		t.Run(fmt.Sprintf("resolvedIndex=%v", resolvedIndex), func(t *testing.T) {
			s := hm288Setup(t, resolvedIndex, earlyAt, lateAt, shortPath, longPath)
			_, beforePaths := hm288Records(s, 60)
			hm215Migrate(s, 100)

			s.mu.RLock()
			w := s.byTxID[60]
			s.mu.RUnlock()
			if w == nil || w.FirstSeen != earlyAt || w.PathJSON != longPath {
				t.Fatalf("survivor = %+v, want first_seen %s kept and path %s", w, earlyAt, longPath)
			}
			_, paths := hm288Records(s, 60)
			if len(paths) != 1 || paths[0].HopCount != beforePaths[0].HopCount+1 {
				t.Fatalf("survivor's path records = %+v, want one with %d hops (was %d)", paths, beforePaths[0].HopCount+1, beforePaths[0].HopCount)
			}
			if paths[0].Timestamp != earlyAt {
				t.Errorf("path record time %s, want %s", paths[0].Timestamp, earlyAt)
			}
		})
	}
}
