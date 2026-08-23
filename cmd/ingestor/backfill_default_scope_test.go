package main

// Tests for BackfillDefaultScopeAsync's #7 default_scope follow-up hardening:
// the backfill must never overwrite confirmed default_scope evidence,
// whether that confirmation lives on the row it's about to backfill (nodes)
// or on the sibling inactive_nodes row for the same public key (the
// resurrection shape). No test for this function existed before this change.

import (
	"database/sql"
	"testing"
)

// seedTransmissionScope inserts a minimal payload_type=4 (ADVERT) transmission
// row carrying scope_name for pubkey, so BackfillDefaultScopeAsync's
// correlated subquery has something to find.
func seedTransmissionScope(t *testing.T, store *Store, pubkey, hash, scopeName, firstSeen string) {
	t.Helper()
	if _, err := store.db.Exec(
		`INSERT INTO transmissions (raw_hex, hash, first_seen, payload_type, from_pubkey, scope_name, last_seen)
		 VALUES ('aabb', ?, ?, 4, ?, ?, 0)`,
		hash, firstSeen, pubkey, scopeName); err != nil {
		t.Fatalf("seed transmission for %s: %v", pubkey, err)
	}
}

func runBackfillDefaultScopeSync(t *testing.T, store *Store, regionKeys map[string][]byte) {
	t.Helper()
	store.BackfillDefaultScopeAsync(regionKeys)
	store.backfillWg.Wait() // same synchronization point Close() uses
}

var backfillRegionKeys = map[string][]byte{"eu": {}, "dk": {}}

// A. Active confirmed: transmissions suggest a different scope, but a
// confirmed default_scope/confirmed_at must survive the backfill untouched.
func TestBackfillDefaultScopeAsync_ActiveConfirmedIsPreserved(t *testing.T) {
	store := newTestStore(t)
	pk := "aa00000000000000000000000000000000000000000000000000000000000a"
	seedActiveNodeOnly(t, store, pk)
	seedDefaultScope(t, store, "nodes", pk, "#dk", "2026-07-29T22:40:00Z")
	seedTransmissionScope(t, store, pk, "h-a", "#eu", "2026-08-01T00:00:00Z")

	runBackfillDefaultScopeSync(t, store, backfillRegionKeys)

	sc, at := defaultScopeConfirmed(t, store, pk)
	if sc.String != "#dk" || at.String != "2026-07-29T22:40:00Z" {
		t.Errorf("nodes = (%q,%q), want unchanged ('#dk','2026-07-29T22:40:00Z') -- backfill must not overwrite confirmed evidence with a packet-inferred guess", sc.String, at.String)
	}
}

// B. Inactive confirmed / active unconfirmed (resurrection): the backfill
// must NOT fill in the active row just because IT has no confirmation of
// its own -- a confirmed sibling row in inactive_nodes must still block it.
// This is the direct regression test for the backfill's cross-table gap.
func TestBackfillDefaultScopeAsync_InactiveConfirmedBlocksActiveResurrectionRow(t *testing.T) {
	store := newTestStore(t)
	pk := "bb00000000000000000000000000000000000000000000000000000000000b"
	seedActiveNodeOnly(t, store, pk) // unconfirmed, default_scope NULL
	seedInactiveNodeOnly(t, store, pk)
	seedDefaultScope(t, store, "inactive_nodes", pk, "#dk", "2026-07-29T22:40:00Z")
	seedTransmissionScope(t, store, pk, "h-b", "#eu", "2026-08-01T00:00:00Z")

	runBackfillDefaultScopeSync(t, store, backfillRegionKeys)

	sc, at := defaultScopeConfirmed(t, store, pk) // reads nodes
	if sc.Valid {
		t.Errorf("nodes.default_scope = %v, want still NULL -- a confirmation in inactive_nodes must cross-table-block the backfill from filling in the active row", sc)
	}
	if at.Valid {
		t.Errorf("nodes.default_scope_confirmed_at = %v, want still NULL", at)
	}
	// The confirmed sibling row itself must also be untouched (backfill only
	// ever targets nodes).
	isc, iat := defaultScopeConfirmedInactive(t, store, pk)
	if isc.String != "#dk" || iat.String != "2026-07-29T22:40:00Z" {
		t.Errorf("inactive_nodes = (%q,%q), want unchanged", isc.String, iat.String)
	}
}

// C. No confirmation anywhere: the backfill must populate the unconfirmed
// row normally, exactly as before this change.
func TestBackfillDefaultScopeAsync_UnconfirmedRowIsFilled(t *testing.T) {
	store := newTestStore(t)
	pk := "cc00000000000000000000000000000000000000000000000000000000000c"
	seedActiveNodeOnly(t, store, pk)
	// scope_name is stored "#"-prefixed in production: loadRegionKeys
	// (cmd/ingestor/main.go) runs regions.Normalize on each configured
	// region before using it as a regionKeys map key, and matchScope returns
	// that same map key verbatim -- so the fixture matches real data shape.
	seedTransmissionScope(t, store, pk, "h-c", "#eu", "2026-08-01T00:00:00Z")

	runBackfillDefaultScopeSync(t, store, backfillRegionKeys)

	sc, at := defaultScopeConfirmed(t, store, pk)
	if sc.String != "#eu" {
		t.Errorf("nodes.default_scope = %q, want '#eu' (unconfirmed row must still be backfilled from transmissions)", sc.String)
	}
	if at.Valid {
		t.Errorf("nodes.default_scope_confirmed_at = %v, want still NULL -- backfill is inference, never confirmation", at)
	}
}

// D. Marker: a successful run records the _migrations marker; a run that
// errors before completion must not.
func TestBackfillDefaultScopeAsync_MarkerWrittenOnlyOnSuccess(t *testing.T) {
	store := newTestStore(t)
	pk := "dd00000000000000000000000000000000000000000000000000000000000d"
	seedActiveNodeOnly(t, store, pk)
	seedTransmissionScope(t, store, pk, "h-d", "#eu", "2026-08-01T00:00:00Z")

	runBackfillDefaultScopeSync(t, store, backfillRegionKeys)

	var marked int
	if err := store.db.QueryRow(`SELECT 1 FROM _migrations WHERE name = 'backfill_default_scope_v1'`).Scan(&marked); err != nil {
		t.Fatalf("expected migration marker to be recorded after a successful run: %v", err)
	}
}

func TestBackfillDefaultScopeAsync_FailureDoesNotWriteMarker(t *testing.T) {
	store := newTestStore(t)
	pk := "ee00000000000000000000000000000000000000000000000000000000000e"
	seedActiveNodeOnly(t, store, pk)
	seedTransmissionScope(t, store, pk, "h-e", "#eu", "2026-08-01T00:00:00Z")

	if _, err := store.db.Exec(`DROP TABLE transmissions`); err != nil {
		t.Fatalf("drop transmissions in throwaway test db: %v", err)
	}

	runBackfillDefaultScopeSync(t, store, backfillRegionKeys)

	var marked sql.NullInt64
	err := store.db.QueryRow(`SELECT 1 FROM _migrations WHERE name = 'backfill_default_scope_v1'`).Scan(&marked)
	if err == nil {
		t.Error("migration marker was recorded despite the backfill query failing, want no marker")
	} else if err != sql.ErrNoRows {
		t.Fatalf("unexpected error checking marker: %v", err)
	}
}

// No region keys configured: BackfillDefaultScopeAsync must return
// immediately without touching the DB at all (pre-existing behavior,
// unaffected by the cross-table guard -- pinned here for completeness).
func TestBackfillDefaultScopeAsync_NoRegionKeysIsNoop(t *testing.T) {
	store := newTestStore(t)
	pk := "ff00000000000000000000000000000000000000000000000000000000000f"
	seedActiveNodeOnly(t, store, pk)
	seedTransmissionScope(t, store, pk, "h-f", "#eu", "2026-08-01T00:00:00Z")

	runBackfillDefaultScopeSync(t, store, map[string][]byte{})

	sc, _ := defaultScopeConfirmed(t, store, pk)
	if sc.Valid {
		t.Errorf("nodes.default_scope = %v, want still NULL -- no region keys means nothing to backfill", sc)
	}
}
