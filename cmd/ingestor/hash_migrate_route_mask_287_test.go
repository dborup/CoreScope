package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// Issue #287: a NULL route_mask means "not computed yet", not "no routes". When
// a merge folds a loser into a survivor and only one side has a computed mask,
// the result must keep every bit either side stored (a bit a surviving
// observation can no longer rebuild, because idx_observations_dedup dropped its
// frame during the move, must not be lost) and OR in the bits the #89 backfill
// would recompute from the merged observations. The merge does this inline, in
// the same transaction, so the result is non-NULL: the backfill then leaves the
// row alone and the route_mask_changes row tells running servers.

// A DIRECT frame header (low two bits = 2) and a FLOOD frame header (low two
// bits = 1).
const (
	hm287DirectFrame = "12" + "00000000"
	hm287FloodFrame  = "11" + "00000000"
	hm287DirectBit   = int64(1) << 2 // route type 2 (DIRECT)
	hm287FloodBit    = int64(1) << 1 // route type 1 (FLOOD)
)

// hm287InsObs writes one observation with a raw_hex header. observer and path
// decide whether the loser's copy survives being re-parented onto the survivor:
// a distinct observer or path keeps it, a matching pair makes idx_observations_dedup
// drop it.
func hm287InsObs(t *testing.T, db *sql.DB, txID, observer int, path, rawHex string) {
	t.Helper()
	hm215Exec(t, db, `INSERT INTO observations (transmission_id, observer_idx, direction, path_json, timestamp, raw_hex)
		VALUES (?, ?, 'RX', ?, 1767225600, ?)`, txID, observer, path, rawHex)
}

func hm287Observers(t *testing.T, db *sql.DB) (o1, o2 int) {
	t.Helper()
	for _, o := range []string{"o1", "o2"} {
		hm215Exec(t, db, `INSERT OR IGNORE INTO observers (id, name, first_seen, last_seen) VALUES (?, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, o, o)
	}
	return hm215Count(t, db, `SELECT rowid FROM observers WHERE id = 'o1'`),
		hm215Count(t, db, `SELECT rowid FROM observers WHERE id = 'o2'`)
}

// Survivor computed DIRECT + loser still NULL but heard as FLOOD, both
// observations surviving the merge: the merge must recompute the union inline
// (DIRECT|FLOOD), keep it non-NULL, and log a route_mask_changes row so a
// running server learns the grown mask. Kills the mutant that stops logging the
// merge's change row (which leaves a running server stuck on DIRECT until it
// restarts, #287 finding 3).
func TestContentHashMigration_MergeRecomputesUnionWhenLoserUnknown_287(t *testing.T) {
	s := hm215Reopen(t, filepath.Join(t.TempDir(), "rm287-loser.db"), func(db *sql.DB) {
		o1, o2 := hm287Observers(t, db)
		// Survivor (lowest id): DIRECT, already computed.
		hm215Exec(t, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, last_seen, route_type, payload_type, decoded_json, route_mask)
			VALUES (70, ?, 'stale-rm-70', '2026-01-01T00:00:00Z', 1, 2, 4, '{}', ?)`, hm215Raw(7), hm287DirectBit)
		// Loser: route_mask NULL (not computed), but heard as FLOOD.
		hm215Exec(t, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, last_seen, route_type, payload_type, decoded_json, route_mask)
			VALUES (71, ?, 'stale-rm-71', '2026-01-01T00:00:00Z', 1, 1, 4, '{}', NULL)`, hm215Raw(7))
		hm287InsObs(t, db, 70, o1, `["aa"]`, hm287DirectFrame)
		hm287InsObs(t, db, 71, o2, `["bb"]`, hm287FloodFrame)
	})

	want := hm287DirectBit | hm287FloodBit
	var merged sql.NullInt64
	if err := s.db.QueryRow(`SELECT route_mask FROM transmissions WHERE id = 70`).Scan(&merged); err != nil {
		t.Fatalf("survivor row: %v", err)
	}
	if !merged.Valid || merged.Int64 != want {
		t.Fatalf("merged route_mask = %v, want DIRECT|FLOOD = %04b recomputed inline", merged, want)
	}
	// A running server that holds the survivor must learn the grown mask.
	if got := hm215Count(t, s.db, `SELECT COALESCE(MAX(route_mask), -1) FROM route_mask_changes WHERE transmission_id = 70`); int64(got) != want {
		t.Fatalf("route_mask_changes announces mask %d for the survivor, want %d", got, want)
	}
	if n := hm215Count(t, s.db, `SELECT COUNT(*) FROM transmissions WHERE id = 71`); n != 0 {
		t.Fatalf("loser 71 was not deleted")
	}

	// The mask is already known, so the backfill leaves it untouched.
	if err := s.backfillTxRouteMask(context.Background(), s.db); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	var final sql.NullInt64
	if err := s.db.QueryRow(`SELECT route_mask FROM transmissions WHERE id = 70`).Scan(&final); err != nil {
		t.Fatalf("survivor row after backfill: %v", err)
	}
	if !final.Valid || final.Int64 != want {
		t.Fatalf("route_mask after backfill = %v, want DIRECT|FLOOD = %04b unchanged", final, want)
	}
}

// The reverse direction: survivor still NULL (heard FLOOD), loser already
// computed DIRECT and holding the current hash, both observations surviving.
// The merge must recompute the union inline from the survivor's own route_type
// and observations too, not just copy the loser's mask. Kills the mutant that
// recomputes only when the loser is the NULL side and otherwise defers the
// survivor-NULL case back to the backfill (#287 finding 2).
func TestContentHashMigration_MergeRecomputesUnionWhenSurvivorUnknown_287(t *testing.T) {
	s := hm215Reopen(t, filepath.Join(t.TempDir(), "rm287-survivor.db"), func(db *sql.DB) {
		o1, o2 := hm287Observers(t, db)
		// Survivor (lowest id): route_mask NULL (not computed), heard as FLOOD.
		hm215Exec(t, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, last_seen, route_type, payload_type, decoded_json, route_mask)
			VALUES (72, ?, 'stale-rm-72', '2026-01-01T00:00:00Z', 1, 1, 4, '{}', NULL)`, hm215Raw(9))
		// Loser: DIRECT, already computed, holding the current hash.
		hm215Exec(t, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, last_seen, route_type, payload_type, decoded_json, route_mask)
			VALUES (73, ?, ?, '2026-01-01T00:00:00Z', 1, 2, 4, '{}', ?)`, hm215Raw(9), ComputeContentHash(hm215Raw(9)), hm287DirectBit)
		hm287InsObs(t, db, 72, o1, `["aa"]`, hm287FloodFrame)
		hm287InsObs(t, db, 73, o2, `["bb"]`, hm287DirectFrame)
	})

	// Survivor is id 72 (lowest), holding the current hash after the merge.
	want := hm287DirectBit | hm287FloodBit
	var merged sql.NullInt64
	if err := s.db.QueryRow(`SELECT route_mask FROM transmissions WHERE id = 72`).Scan(&merged); err != nil {
		t.Fatalf("survivor row: %v", err)
	}
	if !merged.Valid || merged.Int64 != want {
		t.Fatalf("merged route_mask = %v, want DIRECT|FLOOD = %04b recomputed inline", merged, want)
	}
	if n := hm215Count(t, s.db, `SELECT COUNT(*) FROM transmissions WHERE id = 73`); n != 0 {
		t.Fatalf("loser 73 was not deleted")
	}
}

// Finding 1 (blocking): a bit the computed side stored from a frame that no
// surviving observation can rebuild must not be lost. The survivor is NULL
// (heard FLOOD); the loser holds a computed DIRECT mask whose only DIRECT frame
// is an observation that idx_observations_dedup drops when it is re-parented
// (same observer and path as the survivor's FLOOD observation), and whose
// route_type is FLOOD, not DIRECT. Deferring to the backfill (what the NULL-only
// fix did) would recompute FLOOD alone and lose the stored DIRECT bit for good.
// Kills the mutant that recomputes from route_type and observations but drops
// the stored masks.
func TestContentHashMigration_MergeKeepsStoredBitsOfDedupDroppedObservation_287(t *testing.T) {
	s := hm215Reopen(t, filepath.Join(t.TempDir(), "rm287-dedup.db"), func(db *sql.DB) {
		o1, _ := hm287Observers(t, db)
		// Survivor (lowest id): NULL, route_type FLOOD, one FLOOD observation.
		hm215Exec(t, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, last_seen, route_type, payload_type, decoded_json, route_mask)
			VALUES (90, ?, 'stale-rm-90', '2026-01-01T00:00:00Z', 1, 1, 4, '{}', NULL)`, hm215Raw(6))
		// Loser: computed DIRECT mask, but route_type FLOOD, and its only DIRECT
		// evidence is an observation that dedups against the survivor's.
		hm215Exec(t, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, last_seen, route_type, payload_type, decoded_json, route_mask)
			VALUES (91, ?, 'stale-rm-91', '2026-01-01T00:00:00Z', 1, 1, 4, '{}', ?)`, hm215Raw(6), hm287DirectBit)
		hm287InsObs(t, db, 90, o1, `["aa"]`, hm287FloodFrame)
		hm287InsObs(t, db, 91, o1, `["aa"]`, hm287DirectFrame) // dropped: same observer and path
	})

	// The loser's DIRECT observation was dropped, so only the stored DIRECT bit
	// carries it. The merged mask must still be DIRECT|FLOOD.
	want := hm287DirectBit | hm287FloodBit
	var merged sql.NullInt64
	if err := s.db.QueryRow(`SELECT route_mask FROM transmissions WHERE id = 90`).Scan(&merged); err != nil {
		t.Fatalf("survivor row: %v", err)
	}
	if !merged.Valid || merged.Int64 != want {
		t.Fatalf("merged route_mask = %v, want DIRECT|FLOOD = %04b (stored DIRECT bit kept)", merged, want)
	}
	// Only the survivor's FLOOD observation survives the dedup.
	if n := hm215Count(t, s.db, `SELECT COUNT(*) FROM observations WHERE transmission_id = 90`); n != 1 {
		t.Fatalf("survivor holds %d observations, want 1 (the loser's DIRECT one is dropped)", n)
	}
	// The backfill alone cannot rebuild DIRECT from the surviving observation;
	// the mask must already hold it and stay unchanged.
	if err := s.backfillTxRouteMask(context.Background(), s.db); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	var final sql.NullInt64
	if err := s.db.QueryRow(`SELECT route_mask FROM transmissions WHERE id = 90`).Scan(&final); err != nil {
		t.Fatalf("survivor row after backfill: %v", err)
	}
	if !final.Valid || final.Int64 != want {
		t.Fatalf("route_mask after backfill = %v, want DIRECT|FLOOD = %04b", final, want)
	}
}

// Requirement 2: the other columns a merge folds use COALESCE(survivor, loser),
// which fills an unknown (NULL) survivor value from a known loser value and
// never fabricates a sentinel. When BOTH sides are NULL the column must stay
// NULL, so a column whose NULL means "pending" keeps that meaning after a merge.
// Kills a mutant that coalesces a fill column to a literal (e.g.
// COALESCE(scope_name, '')), which would pass "pending" off as a known value.
func TestContentHashMigration_MergeKeepsFillColumnsNullWhenBothUnknown_287(t *testing.T) {
	s := hm215Reopen(t, filepath.Join(t.TempDir(), "fill287.db"), func(db *sql.DB) {
		hm215Exec(t, db, `INSERT OR IGNORE INTO observers (id, name, first_seen, last_seen) VALUES ('o1', 'o1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
		o1 := hm215Count(t, db, `SELECT rowid FROM observers WHERE id = 'o1'`)
		// Both sides leave scope_name, channel_hash and from_pubkey NULL.
		hm215Exec(t, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, last_seen, route_type, payload_type, decoded_json, route_mask, scope_name, channel_hash, from_pubkey)
			VALUES (80, ?, 'stale-fill-80', '2026-01-01T00:00:00Z', 1, 1, 4, '{}', 1, NULL, NULL, NULL)`, hm215Raw(8))
		hm215Exec(t, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, last_seen, route_type, payload_type, decoded_json, route_mask, scope_name, channel_hash, from_pubkey)
			VALUES (81, ?, 'stale-fill-81', '2026-01-01T00:00:00Z', 1, 1, 4, '{}', 1, NULL, NULL, NULL)`, hm215Raw(8))
		hm287InsObs(t, db, 80, o1, `["aa"]`, hm287FloodFrame)
	})
	var scope, channel, from sql.NullString
	if err := s.db.QueryRow(`SELECT scope_name, channel_hash, from_pubkey FROM transmissions WHERE id = 80`).Scan(&scope, &channel, &from); err != nil {
		t.Fatalf("survivor row: %v", err)
	}
	if scope.Valid || channel.Valid || from.Valid {
		t.Fatalf("a both-NULL fill column was given a value: scope=%v channel=%v from=%v; want all NULL", scope, channel, from)
	}
}
