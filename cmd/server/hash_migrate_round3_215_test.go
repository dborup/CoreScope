package main

import (
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// Round 3 of #215 (review of round 2: NEW-1, NEW-3, NEW-4).

func hm215TextMsg(dest, src string) string {
	return fmt.Sprintf(`{"type":"TXT_MSG","destPubKey":%q,"srcPubKey":%q}`, dest, src)
}

// hm215AssertOrdered fails when s.packets, a byPayloadType list or a byNode list
// is not in first_seen order (eviction cuts from the head of each).
func hm215AssertOrdered(t *testing.T, s *PacketStore) {
	t.Helper()
	check := func(name string, list []*StoreTx) {
		for i := 1; i < len(list); i++ {
			if list[i-1].FirstSeen > list[i].FirstSeen {
				t.Errorf("%s is not in first_seen order at %d: tx %d (%s) before tx %d (%s)",
					name, i, list[i-1].ID, list[i-1].FirstSeen, list[i].ID, list[i].FirstSeen)
				return
			}
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	check("s.packets", s.packets)
	for pt, list := range s.byPayloadType {
		check(fmt.Sprintf("byPayloadType[%d]", pt), list)
	}
	for pk, list := range s.byNode {
		check(fmt.Sprintf("byNode[%s...]", pk[:4]), list)
	}
}

func hm215IDs(list []*StoreTx) []int {
	ids := make([]int, len(list))
	for i, tx := range list {
		ids[i] = tx.ID
	}
	return ids
}

// A transmission is also indexed under the destPubKey and srcPubKey of its
// decoded JSON (indexByNode), not only pubKey: the rename and the removal of a
// merged-away duplicate must find those keys too. Every other fixture here uses
// ADVERTs, which carry pubKey only (#215 review, NEW-3, mutant M2).
func TestHashMigrate_RekeysDestAndSrcPubkeys_215(t *testing.T) {
	const textMsg = 2
	for _, resolvedIndex := range []bool{true, false} {
		for _, batch := range []int{1, 2, 100} {
			t.Run(fmt.Sprintf("resolvedIndex=%v/batch%d", resolvedIndex, batch), func(t *testing.T) {
				base := hm215Create(t)
				base.ballast(t)
				want := hm215Snap(hm215OpenIdx(t, base, resolvedIndex))

				d := hm215Create(t)
				d.ballast(t)
				old := hm215Time(-72 * time.Hour)
				older := hm215Time(-72*time.Hour + time.Second)
				// A colliding pair and a single, none of them an ADVERT.
				d.tx(t, 80, hm215Raw(80), "stale-t-80", old, textMsg, hm215TextMsg(acct113PK(150), acct113PK(151)))
				d.tx(t, 81, hm215Raw(80), "stale-t-81", older, textMsg, hm215TextMsg(acct113PK(150), acct113PK(151)))
				d.tx(t, 82, hm215Raw(82), "stale-t-82", old, textMsg, hm215TextMsg(acct113PK(152), acct113PK(153)))
				s := hm215OpenIdx(t, d, resolvedIndex)
				hm215Migrate(s, batch)

				s.mu.RLock()
				pairHash, singleHash := ComputeContentHash(hm215Raw(80)), ComputeContentHash(hm215Raw(82))
				for _, pk := range []string{acct113PK(150), acct113PK(151)} {
					if got := s.nodeHashes[pk]; len(got) != 1 || !got[pairHash] {
						t.Errorf("nodeHashes[%s...] = %v, want exactly the new hash of the merged pair", pk[:4], got)
					}
					if list := s.byNode[pk]; len(list) != 1 || list[0].ID != 80 {
						t.Errorf("byNode[%s...] = %v, want only the survivor 80", pk[:4], hm215IDs(list))
					}
				}
				for _, pk := range []string{acct113PK(152), acct113PK(153)} {
					if got := s.nodeHashes[pk]; len(got) != 1 || !got[singleHash] {
						t.Errorf("nodeHashes[%s...] = %v, want exactly the new hash of the single", pk[:4], got)
					}
				}
				s.mu.RUnlock()
				acct113Check(t, s, "after the migration")

				s.mu.Lock()
				s.retentionHours = 24
				s.mu.Unlock()
				if n := s.RunEviction(); n != 2 {
					t.Fatalf("evicted %d transmissions, want the survivor of the pair and the single (2)", n)
				}
				acct113Check(t, s, "after evicting the migrated rows")
				if got := hm215Snap(s); !reflect.DeepEqual(got, want) {
					t.Errorf("store after migrate+evict differs from one that never had the rows:\n got  %+v\n want %+v", got, want)
				}
			})
		}
	}
}

// A survivor that takes an earlier first_seen from its duplicate moves in every
// list that is ordered by first_seen: s.packets, byPayloadType and the byNode
// lists it is in. The order is asserted, not just the membership (#215 review,
// NEW-3, mutants M6 and M7).
func TestHashMigrate_FirstSeenMoveKeepsEveryListOrdered_215(t *testing.T) {
	for _, resolvedIndex := range []bool{true, false} {
		t.Run(fmt.Sprintf("resolvedIndex=%v", resolvedIndex), func(t *testing.T) {
			d := hm215Create(t)
			d.ballast(t)
			t0 := time.Now().UTC().Add(-72 * time.Hour)
			at := func(s int) string { return t0.Add(time.Duration(s) * time.Second).Format(time.RFC3339) }
			pk := acct113PK(210)
			// 50 survives with the LATER first_seen; its duplicate 51 is the earliest
			// of all; 52 is a different packet of the same node, between the two.
			d.tx(t, 50, hm215Raw(7), "stale-o-50", at(30), 4, hm215Advert(pk))
			d.tx(t, 51, hm215Raw(7), "stale-o-51", at(10), 4, hm215Advert(pk))
			d.tx(t, 52, hm215Raw(8), "stale-o-52", at(20), 4, hm215Advert(pk))
			s := hm215OpenIdx(t, d, resolvedIndex)

			s.mu.RLock()
			if got, want := hm215IDs(s.byNode[pk]), []int{51, 52, 50}; !reflect.DeepEqual(got, want) {
				s.mu.RUnlock()
				t.Fatalf("setup: byNode[%s...] = %v before the migration, want load order %v", pk[:4], got, want)
			}
			s.mu.RUnlock()
			hm215Migrate(s, 100)

			s.mu.RLock()
			ballast := 1000
			if got, want := hm215IDs(s.packets), []int{50, 52, ballast}; !reflect.DeepEqual(got, want) {
				t.Errorf("s.packets = %v, want %v", got, want)
			}
			if got, want := hm215IDs(s.byPayloadType[4]), []int{50, 52, ballast}; !reflect.DeepEqual(got, want) {
				t.Errorf("byPayloadType[ADVERT] = %v, want %v", got, want)
			}
			if got, want := hm215IDs(s.byNode[pk]), []int{50, 52}; !reflect.DeepEqual(got, want) {
				t.Errorf("byNode[%s...] = %v, want %v", pk[:4], got, want)
			}
			s.mu.RUnlock()
			hm215AssertOrdered(t, s)
		})
	}
}

// A merge fills a survivor's empty scope from the duplicate. The ingestor's
// COALESCE fills a NULL scope_name only and keeps "" (transport-scoped, region
// unknown); StoreTx.ScopeName cannot tell the two apart and has no spare byte
// for a flag (it stays 320 bytes), so in memory "" is filled. The result is the
// same for a NULL survivor, the usual one, and differs for a "" survivor with a
// duplicate that matched a region: the DB keeps "" and memory shows the region
// until the reload the deploy plan requires (#215 review, NEW-4). This pins both
// halves of that documented difference, so changing either is a decision.
func TestHashMigrate_ScopeFillAgreesWithTheIngestorForNull_215(t *testing.T) {
	d := hm215Create(t)
	if _, err := d.conn.Exec(`ALTER TABLE transmissions ADD COLUMN scope_name TEXT`); err != nil {
		t.Fatal(err)
	}
	d.ballast(t)
	t0 := time.Now().UTC().Add(-72 * time.Hour)
	at := func(s int) string { return t0.Add(time.Duration(s) * time.Second).Format(time.RFC3339) }
	pair := func(survivor, dup, key int, sScope, dScope interface{}) {
		raw := hm215Raw(key)
		d.tx(t, survivor, raw, fmt.Sprintf("stale-c-%d", survivor), at(survivor), 4, hm215Advert(acct113PK(key)))
		d.tx(t, dup, raw, fmt.Sprintf("stale-c-%d", dup), at(dup), 4, hm215Advert(acct113PK(key)))
		if _, err := d.conn.Exec(`UPDATE transmissions SET scope_name = ? WHERE id = ?`, sScope, survivor); err != nil {
			t.Fatal(err)
		}
		if _, err := d.conn.Exec(`UPDATE transmissions SET scope_name = ? WHERE id = ?`, dScope, dup); err != nil {
			t.Fatal(err)
		}
	}
	pair(70, 71, 70, nil, "#x") // NULL survivor: the ingestor fills it too
	pair(72, 73, 72, nil, "")   // NULL survivor, "" duplicate: "" in both
	pair(74, 75, 74, "#k", "#z")
	pair(76, 77, 76, nil, nil)
	pair(78, 79, 78, "", "#w") // the documented difference: the DB keeps ""
	s := hm215Open(t, d)
	hm215Migrate(s, 100)

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range []struct {
		id   int
		want string
		why  string
	}{
		{70, "#x", "NULL is filled with the duplicate's value, as the ingestor does"},
		{72, "", `NULL filled with ""`},
		{74, "#k", "its own value stays"},
		{76, "", "NULL and NULL"},
		{78, "#w", `a "" survivor is filled in memory (the DB keeps ""), see the comment above`},
	} {
		tx := s.byTxID[c.id]
		if tx == nil {
			t.Fatalf("survivor %d is gone", c.id)
		}
		if tx.ScopeName != c.want {
			t.Errorf("survivor %d scope = %q, want %q: %s", c.id, tx.ScopeName, c.want, c.why)
		}
	}
}

// NEW-1 (measurement only): the migration reports the longest time one batch
// held the write lock, so a staging run can read it off the log.
func TestHashMigrate_LogsMaxWriteLockHold_215(t *testing.T) {
	re := regexp.MustCompile(`\[hash-migrate\] .*max write-lock hold ([0-9]+\.[0-9]) ms over ([0-9]+) batches`)
	for _, c := range []struct{ batch, want int }{{1, 8}, {100, 1}} {
		d := hm215Create(t)
		d.ballast(t)
		d.stale(t, 2, 3, 2) // 8 stale rows
		s := hm215Open(t, d)
		out := hm215Migrate(s, c.batch)
		m := re.FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("batch %d: no max write-lock hold in the log:\n%s", c.batch, out)
		}
		if n, _ := strconv.Atoi(m[2]); n != c.want {
			t.Errorf("batch %d: %d batches in the log, want %d (one per batch that had something to apply)", c.batch, n, c.want)
		}
	}
	// A store with nothing stale applies nothing and logs nothing.
	d := hm215Create(t)
	d.ballast(t)
	if out := hm215Migrate(hm215Open(t, d), 10); re.MatchString(out) {
		t.Errorf("nothing was stale, but the log says: %s", out)
	}
}

func TestLockHold_TracksTheMaximumAndTheCount_215(t *testing.T) {
	var h lockHold
	for _, d := range []time.Duration{3 * time.Millisecond, 9 * time.Millisecond, 5 * time.Millisecond} {
		h.record(d)
	}
	if h.max != 9*time.Millisecond || h.batches != 3 || h.total != 17*time.Millisecond {
		t.Errorf("lockHold = %+v, want max 9ms, total 17ms over 3 batches", h)
	}
}
