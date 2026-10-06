package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// Issue #287: a NULL route_mask means "not computed yet", not "no routes". When
// a merge folds a loser into a survivor and only one side has a computed mask,
// COALESCE(route_mask, 0) injected 0 for the uncomputed side and OR-ed it in, so
// the result was non-NULL. The route_mask backfill only recomputes NULL rows, so
// it then skipped the row and the uncomputed side's route bits were lost for
// good. The fix keeps the merged mask NULL whenever either side is NULL, so the
// backfill recomputes it from the survivor's full (merged) set of observations.

// hm287InsObs writes one observation with a raw_hex header for the backfill to
// read. Distinct observer_idx keeps the survivor's and loser's copies from
// colliding on idx_observations_dedup when the loser's is re-parented.
func hm287InsObs(t *testing.T, db *sql.DB, txID, observer int, rawHex string) {
	t.Helper()
	hm215Exec(t, db, `INSERT INTO observations (transmission_id, observer_idx, direction, path_json, timestamp, raw_hex)
		VALUES (?, ?, 'RX', ?, 1767225600, ?)`, txID, observer, `["aa"]`, rawHex)
}

// A survivor with a computed DIRECT mask and a loser that is still NULL but was
// heard as FLOOD: after merge + backfill the mask must be DIRECT|FLOOD, not the
// survivor's DIRECT alone. Kills the mutant that restores COALESCE(route_mask,0)
// (which leaves a non-NULL DIRECT that the backfill skips).
func TestContentHashMigration_MergeKeepsRouteMaskNullWhenLoserUnknown_287(t *testing.T) {
	// A DIRECT frame header (low two bits = 2) and a FLOOD frame header
	// (firstIngestedFloodRaw, low two bits = 1).
	const directFrame = "12" + "00000000"
	const directBit = int64(1) << 2 // route type 2 (DIRECT)
	const floodBit = int64(1) << 1  // route type 1 (FLOOD)

	s := hm215Reopen(t, filepath.Join(t.TempDir(), "rm287.db"), func(db *sql.DB) {
		for _, o := range []string{"o1", "o2"} {
			hm215Exec(t, db, `INSERT OR IGNORE INTO observers (id, name, first_seen, last_seen) VALUES (?, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, o, o)
		}
		o1 := hm215Count(t, db, `SELECT rowid FROM observers WHERE id = 'o1'`)
		o2 := hm215Count(t, db, `SELECT rowid FROM observers WHERE id = 'o2'`)

		// Survivor (lowest id): DIRECT, already computed (route_mask = directBit).
		hm215Exec(t, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, last_seen, route_type, payload_type, decoded_json, route_mask)
			VALUES (70, ?, 'stale-rm-70', '2026-01-01T00:00:00Z', 1, 2, 4, '{}', ?)`, hm215Raw(7), directBit)
		// Loser: route_mask NULL (not computed), but heard as FLOOD.
		hm215Exec(t, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, last_seen, route_type, payload_type, decoded_json, route_mask)
			VALUES (71, ?, 'stale-rm-71', '2026-01-01T00:00:00Z', 1, 1, 4, '{}', NULL)`, hm215Raw(7))
		hm287InsObs(t, db, 70, o1, directFrame)
		hm287InsObs(t, db, 71, o2, firstIngestedFloodRaw)
	})

	// After the merge the mask must be NULL so the backfill owns it, and no
	// route_mask_changes row may be logged for a mask that is not yet known.
	var merged sql.NullInt64
	if err := s.db.QueryRow(`SELECT route_mask FROM transmissions WHERE id = 70`).Scan(&merged); err != nil {
		t.Fatalf("survivor row: %v", err)
	}
	if merged.Valid {
		t.Fatalf("merged route_mask = %d, want NULL so the backfill recomputes it", merged.Int64)
	}
	if n := hm215Count(t, s.db, `SELECT COUNT(*) FROM route_mask_changes WHERE transmission_id = 70`); n != 0 {
		t.Fatalf("route_mask_changes logged %d rows for a not-yet-known mask, want 0", n)
	}
	if n := hm215Count(t, s.db, `SELECT COUNT(*) FROM transmissions WHERE id = 71`); n != 0 {
		t.Fatalf("loser 71 was not deleted")
	}

	// The backfill recomputes from route_type and every surviving observation
	// (both sides', now re-parented to the survivor): DIRECT|FLOOD.
	if err := s.backfillTxRouteMask(context.Background(), s.db); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	var final sql.NullInt64
	if err := s.db.QueryRow(`SELECT route_mask FROM transmissions WHERE id = 70`).Scan(&final); err != nil {
		t.Fatalf("survivor row after backfill: %v", err)
	}
	if want := directBit | floodBit; !final.Valid || final.Int64 != want {
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
		hm287InsObs(t, db, 80, o1, "11"+"00000000")
	})
	var scope, channel, from sql.NullString
	if err := s.db.QueryRow(`SELECT scope_name, channel_hash, from_pubkey FROM transmissions WHERE id = 80`).Scan(&scope, &channel, &from); err != nil {
		t.Fatalf("survivor row: %v", err)
	}
	if scope.Valid || channel.Valid || from.Valid {
		t.Fatalf("a both-NULL fill column was given a value: scope=%v channel=%v from=%v; want all NULL", scope, channel, from)
	}
}
