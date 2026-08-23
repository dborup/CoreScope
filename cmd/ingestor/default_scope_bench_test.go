package main

// Benchmark for the #7-follow-up default_scope hardening design: compares
// two correctness-equivalent candidate write strategies for
// UpdateNodeDefaultScope's cross-table protection (a packet-inferred write
// must never overwrite firmware-confirmed evidence in EITHER nodes or
// inactive_nodes -- see the design rationale on UpdateNodeDefaultScope
// itself).
//
// Candidate A (benchCrossTableTx): always run both cross-table conditional
// UPDATEs inside one transaction, unconditionally, on every call.
//
// Candidate B (benchFastPathThenTx): a read-only UNION ALL fast path over at
// most two rows decides whether a write could possibly be needed at all; on
// the dominant no-op/confirmed/unknown-pubkey paths it returns without ever
// opening a transaction. When a write MIGHT be needed, it falls through to
// the exact same guarded transaction as candidate A -- the fast path is
// purely an optimization, never a correctness authority, so a race between
// the fast-read and the eventual UPDATE is harmless: the UPDATE's own
// row-local + NOT EXISTS guards are what actually decide whether any row
// changes.
//
// Run with: go test ./... -bench=DefaultScope -benchtime=3000x -count=10 -run=^$
// Compare with benchstat if available: benchstat -count=10 <(the run above)

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"testing"
)

// benchCrossTableTx is candidate A: unconditional, always-transactional,
// cross-table-guarded write.
func benchCrossTableTx(db *sql.DB, pubkey, scope string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`UPDATE nodes SET default_scope = ?
		 WHERE public_key = ?
		   AND (default_scope_confirmed_at IS NULL OR default_scope_confirmed_at = '')
		   AND (default_scope IS NULL OR default_scope != ?)
		   AND NOT EXISTS (
		       SELECT 1 FROM inactive_nodes i
		       WHERE i.public_key = nodes.public_key
		         AND i.default_scope_confirmed_at IS NOT NULL
		         AND i.default_scope_confirmed_at != ''
		   )`,
		scope, pubkey, scope); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE inactive_nodes SET default_scope = ?
		 WHERE public_key = ?
		   AND (default_scope_confirmed_at IS NULL OR default_scope_confirmed_at = '')
		   AND (default_scope IS NULL OR default_scope != ?)
		   AND NOT EXISTS (
		       SELECT 1 FROM nodes n
		       WHERE n.public_key = inactive_nodes.public_key
		         AND n.default_scope_confirmed_at IS NOT NULL
		         AND n.default_scope_confirmed_at != ''
		   )`,
		scope, pubkey, scope); err != nil {
		return err
	}
	return tx.Commit()
}

// benchFastPathThenTx is candidate B: a two-row-at-most read fast path,
// falling through to the identical guarded transaction as candidate A when
// (and only when) a write cannot be ruled out cheaply.
func benchFastPathThenTx(db *sql.DB, pubkey, scope string) error {
	rows, err := db.Query(
		`SELECT default_scope, default_scope_confirmed_at FROM nodes WHERE public_key = ?
		 UNION ALL
		 SELECT default_scope, default_scope_confirmed_at FROM inactive_nodes WHERE public_key = ?`,
		pubkey, pubkey)
	if err != nil {
		return err
	}
	found := 0
	allSame := true
	anyConfirmed := false
	for rows.Next() {
		var sc, at sql.NullString
		if err := rows.Scan(&sc, &at); err != nil {
			rows.Close()
			return err
		}
		found++
		if at.Valid && at.String != "" {
			anyConfirmed = true
		}
		if !(sc.Valid && sc.String == scope) {
			allSame = false
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	if found == 0 || anyConfirmed || allSame {
		return nil
	}
	return benchCrossTableTx(db, pubkey, scope)
}

// ─── fixtures ────────────────────────────────────────────────────────────────

// rowSnapshot captures the columns UpdateNodeDefaultScope can observe or
// change for one pubkey in one table. It is comparable (both fields of
// sql.NullString are comparable, as is bool), so two snapshots can be
// compared with == directly -- no reflect.DeepEqual needed.
type rowSnapshot struct {
	exists    bool
	scope     sql.NullString
	confirmed sql.NullString
}

func readSnapshot(tb testing.TB, db *sql.DB, table, pubkey string) rowSnapshot {
	tb.Helper()
	var sc, at sql.NullString
	err := db.QueryRow(`SELECT default_scope, default_scope_confirmed_at FROM `+table+` WHERE public_key = ?`, pubkey).Scan(&sc, &at)
	if err == sql.ErrNoRows {
		return rowSnapshot{exists: false}
	}
	if err != nil {
		tb.Fatalf("read %s snapshot for %s: %v", table, pubkey, err)
	}
	return rowSnapshot{exists: true, scope: sc, confirmed: at}
}

type dsFixture struct {
	name  string
	setup func(tb testing.TB, db *sql.DB, pubkey string) // leaves nodes/inactive_nodes rows (or none) for pubkey
	scope string

	// anyConfirmedBefore selects which invariant TestDefaultScopeBenchCandidates_Correctness
	// checks: true -> confirmation exists somewhere for this pubkey, so
	// neither table's row may be mutated at all (checked against a snapshot
	// taken right before the candidate write, not a fixed expectation).
	// false -> no confirmation exists anywhere, so the exact expected
	// end-state below must hold, for both candidates identically.
	anyConfirmedBefore bool
	wantNodesAfter     rowSnapshot // only checked when !anyConfirmedBefore
	wantInactiveAfter  rowSnapshot // only checked when !anyConfirmedBefore
}

// dsSeed inserts (or updates) a row in table for pubkey with the given
// default_scope/confirmed state, failing the test/benchmark immediately on
// any SQL error -- a silently-dropped seed insert would make every
// assertion downstream meaningless rather than catching the real bug.
func dsSeed(tb testing.TB, db *sql.DB, table, pubkey, scope string, confirmed bool) {
	tb.Helper()
	at := ""
	if confirmed {
		at = "2026-07-29T22:40:00Z"
	}
	if _, err := db.Exec(
		`INSERT INTO `+table+` (public_key, default_scope, default_scope_confirmed_at, last_seen, first_seen) VALUES (?, ?, ?, '', '')`,
		pubkey, scope, at); err != nil {
		tb.Fatalf("seed %s: table=%s pubkey=%s scope=%q confirmed=%v: %v", "dsSeed", table, pubkey, scope, confirmed, err)
	}
}

// unconfirmedScope is the sentinel default_scope_confirmed_at value dsSeed
// writes for confirmed=false: an empty string, not SQL NULL (matching real
// unconfirmed rows, which are never touched by UpdateNodeDefaultScope's
// UPDATE -- it only ever sets default_scope). Both NULL and "" mean
// "unconfirmed" to the production guard, but readSnapshot reads back
// whatever was actually stored, so expected end-states below must use this,
// not a NULL NullString.
var unconfirmedScope = sql.NullString{String: "", Valid: true}

var dsFixtures = []dsFixture{
	{
		name:               "ActiveOnly_SameValue",
		setup:              func(tb testing.TB, db *sql.DB, pk string) { dsSeed(tb, db, "nodes", pk, "#eu", false) },
		scope:              "#eu",
		anyConfirmedBefore: false,
		wantNodesAfter:     rowSnapshot{exists: true, scope: sql.NullString{String: "#eu", Valid: true}, confirmed: unconfirmedScope},
		wantInactiveAfter:  rowSnapshot{exists: false},
	},
	{
		name:               "ActiveOnly_NewValue",
		setup:              func(tb testing.TB, db *sql.DB, pk string) { dsSeed(tb, db, "nodes", pk, "#dk", false) },
		scope:              "#eu",
		anyConfirmedBefore: false,
		wantNodesAfter:     rowSnapshot{exists: true, scope: sql.NullString{String: "#eu", Valid: true}, confirmed: unconfirmedScope},
		wantInactiveAfter:  rowSnapshot{exists: false},
	},
	{
		name:               "ActiveOnly_Confirmed",
		setup:              func(tb testing.TB, db *sql.DB, pk string) { dsSeed(tb, db, "nodes", pk, "#dk", true) },
		scope:              "#eu",
		anyConfirmedBefore: true,
	},
	{
		name:               "InactiveOnly_SameValue",
		setup:              func(tb testing.TB, db *sql.DB, pk string) { dsSeed(tb, db, "inactive_nodes", pk, "#eu", false) },
		scope:              "#eu",
		anyConfirmedBefore: false,
		wantNodesAfter:     rowSnapshot{exists: false},
		wantInactiveAfter:  rowSnapshot{exists: true, scope: sql.NullString{String: "#eu", Valid: true}, confirmed: unconfirmedScope},
	},
	{
		name:               "InactiveOnly_Confirmed",
		setup:              func(tb testing.TB, db *sql.DB, pk string) { dsSeed(tb, db, "inactive_nodes", pk, "#dk", true) },
		scope:              "#eu",
		anyConfirmedBefore: true,
	},
	{
		name: "BothUnconfirmed_SameValue",
		setup: func(tb testing.TB, db *sql.DB, pk string) {
			dsSeed(tb, db, "nodes", pk, "#eu", false)
			dsSeed(tb, db, "inactive_nodes", pk, "#eu", false)
		},
		scope:              "#eu",
		anyConfirmedBefore: false,
		wantNodesAfter:     rowSnapshot{exists: true, scope: sql.NullString{String: "#eu", Valid: true}, confirmed: unconfirmedScope},
		wantInactiveAfter:  rowSnapshot{exists: true, scope: sql.NullString{String: "#eu", Valid: true}, confirmed: unconfirmedScope},
	},
	{
		name: "BothUnconfirmed_DifferentValue",
		setup: func(tb testing.TB, db *sql.DB, pk string) {
			dsSeed(tb, db, "nodes", pk, "#dk", false)
			dsSeed(tb, db, "inactive_nodes", pk, "#dk", false)
		},
		scope:              "#eu",
		anyConfirmedBefore: false,
		wantNodesAfter:     rowSnapshot{exists: true, scope: sql.NullString{String: "#eu", Valid: true}, confirmed: unconfirmedScope},
		wantInactiveAfter:  rowSnapshot{exists: true, scope: sql.NullString{String: "#eu", Valid: true}, confirmed: unconfirmedScope},
	},
	{
		name: "ConfirmedInActiveOnly",
		setup: func(tb testing.TB, db *sql.DB, pk string) {
			dsSeed(tb, db, "nodes", pk, "#dk", true)
			dsSeed(tb, db, "inactive_nodes", pk, "#dk", false)
		},
		scope:              "#eu",
		anyConfirmedBefore: true,
	},
	{
		name: "ConfirmedInInactiveOnly",
		setup: func(tb testing.TB, db *sql.DB, pk string) {
			dsSeed(tb, db, "nodes", pk, "", false)
			dsSeed(tb, db, "inactive_nodes", pk, "#dk", true)
		},
		scope:              "#eu",
		anyConfirmedBefore: true,
	},
	{
		name:               "UnknownPubkey",
		setup:              func(tb testing.TB, db *sql.DB, pk string) {}, // no row seeded at all
		scope:              "#eu",
		anyConfirmedBefore: false,
		wantNodesAfter:     rowSnapshot{exists: false},
		wantInactiveAfter:  rowSnapshot{exists: false},
	},
}

func runDefaultScopeBench(b *testing.B, write func(db *sql.DB, pubkey, scope string) error) {
	// OpenStore logs verbosely on every open (schema migrations etc.); silence
	// it here so the benchmark's own name+ns/op lines aren't interleaved with
	// and split apart by unrelated log output.
	prevOut := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(prevOut)

	for _, fx := range dsFixtures {
		fx := fx
		b.Run(fx.name, func(b *testing.B) {
			store := newTestStore(b)
			db := store.db
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				pk := fmt.Sprintf("pk%d", i)
				fx.setup(b, db, pk)
				b.StartTimer()
				if err := write(db, pk, fx.scope); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkDefaultScope_CrossTableTx(b *testing.B) {
	runDefaultScopeBench(b, benchCrossTableTx)
}

func BenchmarkDefaultScope_FastPathThenTx(b *testing.B) {
	runDefaultScopeBench(b, benchFastPathThenTx)
}

// TestDefaultScopeBenchCandidates_Correctness is not a benchmark -- it pins
// that both candidates reach the identical, correct end state for every
// fixture above before their timings are trusted. Two invariants are
// checked, selected per fixture by anyConfirmedBefore:
//
//   - A confirmation exists somewhere for this pubkey (before the candidate
//     write runs): neither table's row may be mutated AT ALL -- checked
//     against a snapshot taken immediately before the write, not a
//     hardcoded expectation, and including the "no row was created where
//     none existed" case.
//   - No confirmation exists anywhere: the exact expected end-state (an
//     explicit value per fixture, not derived from the fixture's name) must
//     hold for both candidates identically.
func TestDefaultScopeBenchCandidates_Correctness(t *testing.T) {
	for _, fx := range dsFixtures {
		for _, cand := range []struct {
			name  string
			write func(db *sql.DB, pubkey, scope string) error
		}{
			{"CrossTableTx", benchCrossTableTx},
			{"FastPathThenTx", benchFastPathThenTx},
		} {
			t.Run(fx.name+"/"+cand.name, func(t *testing.T) {
				store := newTestStore(t)
				db := store.db
				pk := "correctness-" + fx.name
				fx.setup(t, db, pk)

				beforeNodes := readSnapshot(t, db, "nodes", pk)
				beforeInactive := readSnapshot(t, db, "inactive_nodes", pk)

				if err := cand.write(db, pk, fx.scope); err != nil {
					t.Fatal(err)
				}

				afterNodes := readSnapshot(t, db, "nodes", pk)
				afterInactive := readSnapshot(t, db, "inactive_nodes", pk)

				if fx.anyConfirmedBefore {
					if afterNodes != beforeNodes {
						t.Errorf("%s: nodes row changed despite a confirmation existing somewhere for this pubkey: before=%+v after=%+v", cand.name, beforeNodes, afterNodes)
					}
					if afterInactive != beforeInactive {
						t.Errorf("%s: inactive_nodes row changed despite a confirmation existing somewhere for this pubkey: before=%+v after=%+v", cand.name, beforeInactive, afterInactive)
					}
				} else {
					if afterNodes != fx.wantNodesAfter {
						t.Errorf("%s: nodes after = %+v, want %+v", cand.name, afterNodes, fx.wantNodesAfter)
					}
					if afterInactive != fx.wantInactiveAfter {
						t.Errorf("%s: inactive_nodes after = %+v, want %+v", cand.name, afterInactive, fx.wantInactiveAfter)
					}
				}
			})
		}
	}
}
