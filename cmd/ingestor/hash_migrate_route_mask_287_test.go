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

// hm287Mask reads a transmission's route_mask.
func hm287Mask(t *testing.T, db *sql.DB, id int) sql.NullInt64 {
	t.Helper()
	var m sql.NullInt64
	if err := db.QueryRow(`SELECT route_mask FROM transmissions WHERE id = ?`, id).Scan(&m); err != nil {
		t.Fatalf("transmission %d: %v", id, err)
	}
	return m
}

// The inline recompute must read the surviving observation headers. The only
// DIRECT evidence is an observation of the not-yet-computed survivor whose
// header is DIRECT (its route_type is FLOOD, and the loser's computed mask and
// route_type are FLOOD too). The merged mask is non-NULL, so the backfill never
// gets a second chance: a merge that skips or misreads the observation scan
// leaves FLOOD for good. Kills reviewer mutants MX2 (no observation scan), MX3
// (scan the loser's observations, empty after the move) and MX4
// (routeMaskBitFromHeader returns 0).
func TestContentHashMigration_MergeRecomputesBitFromSurvivingObservationHeader_287(t *testing.T) {
	s := hm215Reopen(t, filepath.Join(t.TempDir(), "rm287-header.db"), func(db *sql.DB) {
		o1, o2 := hm287Observers(t, db)
		// Survivor (lowest id): NULL, route_type FLOOD, heard once as FLOOD and
		// once as DIRECT.
		hm215Exec(t, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, last_seen, route_type, payload_type, decoded_json, route_mask)
			VALUES (100, ?, 'stale-rm-100', '2026-01-01T00:00:00Z', 1, 1, 4, '{}', NULL)`, hm215Raw(10))
		// Loser: computed FLOOD, route_type FLOOD.
		hm215Exec(t, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, last_seen, route_type, payload_type, decoded_json, route_mask)
			VALUES (101, ?, 'stale-rm-101', '2026-01-01T00:00:00Z', 1, 1, 4, '{}', ?)`, hm215Raw(10), hm287FloodBit)
		hm287InsObs(t, db, 100, o1, `["aa"]`, hm287FloodFrame)
		hm287InsObs(t, db, 100, o2, `["bb"]`, hm287DirectFrame) // the only DIRECT evidence
		hm287InsObs(t, db, 101, o1, `["cc"]`, hm287FloodFrame)
	})

	want := hm287DirectBit | hm287FloodBit
	if merged := hm287Mask(t, s.db, 100); !merged.Valid || merged.Int64 != want {
		t.Fatalf("merged route_mask = %v, want DIRECT|FLOOD = %04b (DIRECT from the surviving observation header)", merged, want)
	}
	if err := s.backfillTxRouteMask(context.Background(), s.db); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if final := hm287Mask(t, s.db, 100); !final.Valid || final.Int64 != want {
		t.Fatalf("route_mask after backfill = %v, want DIRECT|FLOOD = %04b", final, want)
	}
}

// The inline recompute must read the loser's route_type, not only the
// survivor's. The only DIRECT evidence is the not-yet-computed loser's
// route_type: its one observation has no stored frame (raw_hex NULL), and the
// survivor is computed FLOOD with a FLOOD observation. Master's
// COALESCE(route_mask, 0) merge ends at FLOOD here too. Kills reviewer mutant
// MX1 (read only the winner's route_type).
func TestContentHashMigration_MergeRecomputesBitFromLoserRouteType_287(t *testing.T) {
	s := hm215Reopen(t, filepath.Join(t.TempDir(), "rm287-loser-rt.db"), func(db *sql.DB) {
		o1, o2 := hm287Observers(t, db)
		// Survivor (lowest id): computed FLOOD, route_type FLOOD.
		hm215Exec(t, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, last_seen, route_type, payload_type, decoded_json, route_mask)
			VALUES (110, ?, 'stale-rm-110', '2026-01-01T00:00:00Z', 1, 1, 4, '{}', ?)`, hm215Raw(11), hm287FloodBit)
		// Loser: NULL, heard as DIRECT; its observation kept no raw_hex.
		hm215Exec(t, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, last_seen, route_type, payload_type, decoded_json, route_mask)
			VALUES (111, ?, 'stale-rm-111', '2026-01-01T00:00:00Z', 1, 2, 4, '{}', NULL)`, hm215Raw(11))
		hm287InsObs(t, db, 110, o1, `["aa"]`, hm287FloodFrame)
		hm215Obs(t, db, 111, o2, `["bb"]`)
	})

	want := hm287DirectBit | hm287FloodBit
	if merged := hm287Mask(t, s.db, 110); !merged.Valid || merged.Int64 != want {
		t.Fatalf("merged route_mask = %v, want DIRECT|FLOOD = %04b (DIRECT from the loser's route_type)", merged, want)
	}
	if err := s.backfillTxRouteMask(context.Background(), s.db); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if final := hm287Mask(t, s.db, 110); !final.Valid || final.Int64 != want {
		t.Fatalf("route_mask after backfill = %v, want DIRECT|FLOOD = %04b", final, want)
	}
}

// The inline recompute must run after the move, so its observation scan sees
// the loser's re-parented observations, not only the survivor's own. The only
// DIRECT evidence is the header of the not-yet-computed loser's one observation,
// which survives being re-parented (distinct observer and path): the survivor is
// computed FLOOD with a FLOOD observation, and the loser's route_type is FLOOD.
// Master's COALESCE(route_mask, 0) merge ends at FLOOD here too. Kills reviewer
// mutant N2 (recompute above the UPDATE OR IGNORE observations move, #324).
func TestContentHashMigration_MergeRecomputesBitFromLoserReparentedObservation_324(t *testing.T) {
	s := hm215Reopen(t, filepath.Join(t.TempDir(), "rm324-loser-obs.db"), func(db *sql.DB) {
		o1, o2 := hm287Observers(t, db)
		// Survivor (lowest id): computed FLOOD, route_type FLOOD.
		hm215Exec(t, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, last_seen, route_type, payload_type, decoded_json, route_mask)
			VALUES (130, ?, 'stale-rm-130', '2026-01-01T00:00:00Z', 1, 1, 4, '{}', ?)`, hm215Raw(13), hm287FloodBit)
		// Loser: NULL, route_type FLOOD, heard once as DIRECT.
		hm215Exec(t, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, last_seen, route_type, payload_type, decoded_json, route_mask)
			VALUES (131, ?, 'stale-rm-131', '2026-01-01T00:00:00Z', 1, 1, 4, '{}', NULL)`, hm215Raw(13))
		hm287InsObs(t, db, 130, o1, `["aa"]`, hm287FloodFrame)
		hm287InsObs(t, db, 131, o2, `["bb"]`, hm287DirectFrame) // the only DIRECT evidence
	})

	want := hm287DirectBit | hm287FloodBit
	if merged := hm287Mask(t, s.db, 130); !merged.Valid || merged.Int64 != want {
		t.Fatalf("merged route_mask = %v, want DIRECT|FLOOD = %04b (DIRECT from the loser's re-parented observation header)", merged, want)
	}
	// The loser's DIRECT observation survived the move onto the survivor.
	if n := hm215Count(t, s.db, `SELECT COUNT(*) FROM observations WHERE transmission_id = 130`); n != 2 {
		t.Fatalf("survivor holds %d observations, want 2 (its own and the loser's re-parented one)", n)
	}
	if err := s.backfillTxRouteMask(context.Background(), s.db); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if final := hm287Mask(t, s.db, 130); !final.Valid || final.Int64 != want {
		t.Fatalf("route_mask after backfill = %v, want DIRECT|FLOOD = %04b", final, want)
	}
}

// Both sides NULL: the merge writes a known mask instead of leaving NULL, and an
// uncomputed survivor that the merge gives a mask (even 0) logs exactly one
// route_mask_changes row. With evidence the mask is the recomputed union; with
// none it is a known 0, which the backfill leaves alone (it would have written 0
// too) and live ingest still grows. Kills the mutant that leaves a both-NULL
// merge NULL (deferring to the backfill as master did) and the mutant that logs
// a change row only when the survivor's mask was already known.
func TestContentHashMigration_MergeBothRouteMasksUnknown_287(t *testing.T) {
	cases := []struct {
		name          string
		survivorRT    interface{}
		loserRT       interface{}
		survivorFrame string // "" = no observation
		loserFrame    string
		want          int64
	}{
		{"with evidence", 1, 2, hm287FloodFrame, hm287DirectFrame, hm287DirectBit | hm287FloodBit},
		{"without evidence", nil, nil, "", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := hm215Reopen(t, filepath.Join(t.TempDir(), "rm287-both-null.db"), func(db *sql.DB) {
				o1, o2 := hm287Observers(t, db)
				hm215Exec(t, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, last_seen, route_type, payload_type, decoded_json, route_mask)
					VALUES (120, ?, 'stale-rm-120', '2026-01-01T00:00:00Z', 1, ?, 4, '{}', NULL)`, hm215Raw(12), tc.survivorRT)
				hm215Exec(t, db, `INSERT INTO transmissions (id, raw_hex, hash, first_seen, last_seen, route_type, payload_type, decoded_json, route_mask)
					VALUES (121, ?, 'stale-rm-121', '2026-01-01T00:00:00Z', 1, ?, 4, '{}', NULL)`, hm215Raw(12), tc.loserRT)
				if tc.survivorFrame != "" {
					hm287InsObs(t, db, 120, o1, `["aa"]`, tc.survivorFrame)
				}
				if tc.loserFrame != "" {
					hm287InsObs(t, db, 121, o2, `["bb"]`, tc.loserFrame)
				}
			})

			if merged := hm287Mask(t, s.db, 120); !merged.Valid || merged.Int64 != tc.want {
				t.Fatalf("merged route_mask = %v, want known %04b written by the merge", merged, tc.want)
			}
			if n := hm215Count(t, s.db, `SELECT COUNT(*) FROM route_mask_changes WHERE transmission_id = 120`); n != 1 {
				t.Fatalf("route_mask_changes rows for the survivor = %d, want 1", n)
			}
			if got := hm215Count(t, s.db, `SELECT route_mask FROM route_mask_changes WHERE transmission_id = 120`); int64(got) != tc.want {
				t.Fatalf("route_mask_changes announces %d, want %d", got, tc.want)
			}
			if err := s.backfillTxRouteMask(context.Background(), s.db); err != nil {
				t.Fatalf("backfill: %v", err)
			}
			if final := hm287Mask(t, s.db, 120); !final.Valid || final.Int64 != tc.want {
				t.Fatalf("route_mask after backfill = %v, want %04b", final, tc.want)
			}
		})
	}
}
