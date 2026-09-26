package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// PR #93 review: route_mask_changes notifies running servers of a new route
// bit on an existing transmission. It matters most when the observation that
// brings the bit only upserts an existing row (same observer and path): the
// row keeps its id, so a server polling new observation ids never sees it.
// One row per actual mask change, written in the same transaction as the
// mask update and the observation.

type routeMaskChange struct {
	txID      int64
	mask      int64
	createdAt int64
}

func routeMaskChanges(t *testing.T, s *Store) []routeMaskChange {
	t.Helper()
	rows, err := s.db.Query(`SELECT transmission_id, route_mask, created_at FROM route_mask_changes ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []routeMaskChange
	for rows.Next() {
		var c routeMaskChange
		if err := rows.Scan(&c.txID, &c.mask, &c.createdAt); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func routeMaskTxID(t *testing.T, s *Store, hash string) int64 {
	t.Helper()
	var id int64
	if err := s.db.QueryRow(`SELECT id FROM transmissions WHERE hash = ?`, hash).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// Same observer, same path, new route: the observation row keeps its id, the
// mask gains the bit, and exactly one change row carries the full new mask.
func TestRouteMaskChange_SameRowUpsertWritesOneEvent(t *testing.T) {
	for _, tc := range []struct {
		name        string
		first, next routeMaskObs
		want        int64
	}{
		{"zero-hop then 0-hop transport flood",
			routeMaskObs{firstIngestedZeroHopRaw, "obs-a", routeMaskT0},
			routeMaskObs{routeMaskFlood0HopRaw, "obs-a", routeMaskT5}, 0b1001},
		{"0-hop transport flood then zero-hop",
			routeMaskObs{routeMaskFlood0HopRaw, "obs-a", routeMaskT0},
			routeMaskObs{firstIngestedZeroHopRaw, "obs-a", routeMaskT5}, 0b1001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := routeMaskStore(t, filepath.Join(t.TempDir(), "upsert.db"))
			defer s.Close()
			hash := routeMaskInsert(t, s, tc.first)
			txID := routeMaskTxID(t, s, hash)
			var obsID int64
			s.db.QueryRow(`SELECT id FROM observations WHERE transmission_id = ?`, txID).Scan(&obsID)
			before := time.Now().Unix()
			routeMaskInsert(t, s, tc.next)

			var n, id int64
			s.db.QueryRow(`SELECT COUNT(*), MAX(id) FROM observations WHERE transmission_id = ?`, txID).Scan(&n, &id)
			if n != 1 || id != obsID {
				t.Fatalf("observations = %d (max id %d), want the one row %d upserted in place", n, id, obsID)
			}
			if mask, _ := routeMaskOf(t, s, hash); !mask.Valid || mask.Int64 != tc.want {
				t.Fatalf("route_mask = %v, want %04b", mask, tc.want)
			}
			got := routeMaskChanges(t, s)
			if len(got) != 1 || got[0].txID != txID || got[0].mask != tc.want || got[0].createdAt < before {
				t.Fatalf("change rows = %+v, want one {tx %d, mask %04b, created_at >= %d}", got, txID, tc.want, before)
			}
		})
	}
}

// Only actual mask changes are logged: a repeated route, a new transmission
// (the server reads its full mask when it loads the new row) and a legacy
// NULL mask (the backfill owns it) write nothing.
func TestRouteMaskChange_OnlyActualChangesAreLogged(t *testing.T) {
	s := routeMaskStore(t, filepath.Join(t.TempDir(), "only.db"))
	defer s.Close()
	flood := routeMaskObs{firstIngestedFloodRaw, "obs-a", routeMaskT0}
	hash := routeMaskInsert(t, s, flood) // new transmission
	if got := routeMaskChanges(t, s); len(got) != 0 {
		t.Fatalf("new transmission logged %+v", got)
	}
	routeMaskInsert(t, s, flood)                                                     // redelivery
	routeMaskInsert(t, s, routeMaskObs{firstIngestedFloodRaw, "obs-b", routeMaskT5}) // same route, other observer
	if got := routeMaskChanges(t, s); len(got) != 0 {
		t.Fatalf("repeated route logged %+v", got)
	}
	if mask, _ := routeMaskOf(t, s, hash); mask.Int64 != 0b0010 {
		t.Fatalf("route_mask = %v, want 0010", mask)
	}

	if _, err := s.db.Exec(`UPDATE transmissions SET route_mask = NULL`); err != nil {
		t.Fatal(err)
	}
	routeMaskInsert(t, s, routeMaskObs{firstIngestedZeroHopRaw, "obs-b", routeMaskT5})
	if got := routeMaskChanges(t, s); len(got) != 0 {
		t.Fatalf("legacy NULL row logged %+v", got)
	}
}

// Several new bits, through both a new observation row and an upsert: one
// row per change, each with the mask as it was after that change.
func TestRouteMaskChange_EachNewBitIsLogged(t *testing.T) {
	s := routeMaskStore(t, filepath.Join(t.TempDir(), "bits.db"))
	defer s.Close()
	hash := routeMaskInsert(t, s, routeMaskObs{firstIngestedFloodRaw, "obs-a", routeMaskT0}) // 0010
	routeMaskInsert(t, s, routeMaskObs{firstIngestedZeroHopRaw, "obs-b", routeMaskT5})       // new row: 1010
	routeMaskInsert(t, s, routeMaskObs{routeMaskFlood0HopRaw, "obs-b", routeMaskT0})         // upsert: 1011
	routeMaskInsert(t, s, routeMaskObs{routeMaskFlood0HopRaw, "obs-b", routeMaskT0})         // repeat: nothing
	txID := routeMaskTxID(t, s, hash)
	got := routeMaskChanges(t, s)
	if len(got) != 2 || got[0].txID != txID || got[0].mask != 0b1010 || got[1].txID != txID || got[1].mask != 0b1011 {
		t.Fatalf("change rows = %+v, want masks 1010 then 1011 for tx %d", got, txID)
	}
}

// The backfill fills NULL masks without logging: running servers pick those
// up through their own refresh of rows they loaded with an unknown mask.
func TestRouteMaskChange_BackfillWritesNoEvents(t *testing.T) {
	s := routeMaskLegacyFixture(t, filepath.Join(t.TempDir(), "backfill.db"), 6)
	defer s.Close()
	if _, err := s.db.Exec(`DELETE FROM route_mask_changes`); err != nil {
		t.Fatal(err)
	}
	s.StartRouteMaskBackfill(context.Background())
	s.WaitForAsyncMigrations()
	if got := routeMaskChanges(t, s); len(got) != 0 {
		t.Fatalf("backfill logged %d change rows", len(got))
	}
}

// Retention deletes a pruned transmission's change rows in the same batch
// and keeps those of transmissions that still exist.
func TestPruneOldPackets_DeletesChangeRowsWithTheirTransmissions(t *testing.T) {
	s := routeMaskStore(t, filepath.Join(t.TempDir(), "prune.db"))
	defer s.Close()
	old := time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339)
	fresh := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	oldHash := routeMaskInsert(t, s, routeMaskObs{firstIngestedFloodRaw, "obs-a", old})
	routeMaskInsert(t, s, routeMaskObs{firstIngestedZeroHopRaw, "obs-b", old})
	payload := fmt.Sprintf("%064x", 7) + firstIngestedAdvertPayload[64:]
	freshHash := routeMaskInsert(t, s, routeMaskObs{"11" + "02" + "a1b2" + payload, "obs-a", fresh})
	routeMaskInsert(t, s, routeMaskObs{"13" + "00000000" + "00" + payload, "obs-b", fresh})
	oldID, freshID := routeMaskTxID(t, s, oldHash), routeMaskTxID(t, s, freshHash)
	if got := routeMaskChanges(t, s); len(got) != 2 {
		t.Fatalf("setup: %+v", got)
	}
	if _, err := s.PruneOldPackets(7); err != nil {
		t.Fatal(err)
	}
	got := routeMaskChanges(t, s)
	if len(got) != 1 || got[0].txID != freshID {
		t.Fatalf("after prune: %+v, want only the row of tx %d (tx %d was pruned)", got, freshID, oldID)
	}
}

// Change rows whose transmission was removed by any other path are pruned in
// bounded batches; rows of existing transmissions are never touched, and a
// second pass changes nothing.
func TestPruneOrphanRouteMaskChanges(t *testing.T) {
	s := routeMaskStore(t, filepath.Join(t.TempDir(), "orphan.db"))
	defer s.Close()
	hash := routeMaskInsert(t, s, routeMaskObs{firstIngestedFloodRaw, "obs-a", routeMaskT0})
	routeMaskInsert(t, s, routeMaskObs{firstIngestedZeroHopRaw, "obs-b", routeMaskT5})
	keep := routeMaskTxID(t, s, hash)
	for i := 0; i < 5; i++ {
		if _, err := s.db.Exec(`INSERT INTO route_mask_changes (transmission_id, route_mask, created_at) VALUES (?, 2, 1)`, 900000+i); err != nil {
			t.Fatal(err)
		}
	}
	old := routeMaskOrphanPruneBatch
	routeMaskOrphanPruneBatch = 2
	defer func() { routeMaskOrphanPruneBatch = old }()
	ResetWriterStatsForTest()
	n, err := s.PruneOrphanRouteMaskChanges()
	if err != nil || n != 5 {
		t.Fatalf("PruneOrphanRouteMaskChanges = %d, %v; want 5 over several batches", n, err)
	}
	// Six rows, examined two ids per writer transaction (the last one finds
	// nothing left): the work of one transaction is bounded by the batch, not
	// by how far the next orphan is.
	if got := s.WriterStatsSnapshot()["prune_route_mask_changes"].Count; got != 4 {
		t.Fatalf("orphan prune ran %d writer transactions, want 4 bounded batches", got)
	}
	got := routeMaskChanges(t, s)
	if len(got) != 1 || got[0].txID != keep {
		t.Fatalf("after orphan prune: %+v, want only the row of tx %d", got, keep)
	}
	if n, err := s.PruneOrphanRouteMaskChanges(); err != nil || n != 0 {
		t.Fatalf("second pass = %d, %v; want 0", n, err)
	}
}

// The transaction logs a change only when the OR actually changed the mask:
// called with a bit the mask already has, it writes the observation and no
// change row.
func TestInsertObservationWithRouteBit_NoChangeRowWhenTheBitIsSet(t *testing.T) {
	s := routeMaskStore(t, filepath.Join(t.TempDir(), "noop.db"))
	defer s.Close()
	hash := routeMaskInsert(t, s, routeMaskObs{firstIngestedFloodRaw, "obs-a", routeMaskT0}) // mask 0010
	txID := routeMaskTxID(t, s, hash)
	var obsIdx int64
	s.db.QueryRow(`SELECT rowid FROM observers WHERE id = 'obs-b'`).Scan(&obsIdx)
	writerMu.Lock()
	err := s.insertObservationWithRouteBit(txID, 0b0010, []interface{}{
		txID, obsIdx, nil, nil, nil, nil, `[]`, int64(1790244000), nil, nil,
	})
	writerMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM observations WHERE transmission_id = ?`, txID).Scan(&n)
	if n != 2 {
		t.Fatalf("observations = %d, want 2 (the call still writes its observation)", n)
	}
	if got := routeMaskChanges(t, s); len(got) != 0 {
		t.Fatalf("change rows = %+v, want none: the mask did not change", got)
	}
}
