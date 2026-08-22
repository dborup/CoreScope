package main

// Tests for #1865: ingest the observer /neighbors report as concrete evidence
// of configured region scopes. Covers handleNeighborsReport dispatch semantics
// and the UpdateNodeConfiguredScope store method (status gating, case folding,
// last-write-wins, and the "absence/timeout is not a signal" contract).

import (
	"bytes"
	"database/sql"
	"log"
	"path/filepath"
	"strings"
	"testing"
)

// seedNode inserts a node into both nodes and inactive_nodes (lowercase key).
func seedNode(t *testing.T, store *Store, pubkey string) {
	t.Helper()
	if _, err := store.db.Exec(`INSERT INTO nodes (public_key, name) VALUES (?, ?)`, pubkey, "n_"+pubkey[:4]); err != nil {
		t.Fatalf("seed node %s: %v", pubkey, err)
	}
	if _, err := store.db.Exec(`INSERT INTO inactive_nodes (public_key, name) VALUES (?, ?)`, pubkey, "n_"+pubkey[:4]); err != nil {
		t.Fatalf("seed inactive node %s: %v", pubkey, err)
	}
}

func configuredScope(t *testing.T, store *Store, pubkey string) (sql.NullString, sql.NullString) {
	t.Helper()
	var sc, at sql.NullString
	if err := store.db.QueryRow(
		`SELECT configured_scope, configured_scope_at FROM nodes WHERE public_key = ?`, pubkey,
	).Scan(&sc, &at); err != nil {
		t.Fatalf("read configured_scope for %s: %v", pubkey, err)
	}
	return sc, at
}

func defaultScopeConfirmed(t *testing.T, store *Store, pubkey string) (sql.NullString, sql.NullString) {
	t.Helper()
	var sc, at sql.NullString
	if err := store.db.QueryRow(
		`SELECT default_scope, default_scope_confirmed_at FROM nodes WHERE public_key = ?`, pubkey,
	).Scan(&sc, &at); err != nil {
		t.Fatalf("read default_scope for %s: %v", pubkey, err)
	}
	return sc, at
}

// seedActiveNodeOnly inserts a node into nodes only, leaving inactive_nodes
// untouched. seedNode (above) always inserts into both tables, which would
// hide a nodes-only regression -- use this whenever a test needs to prove
// LWW protection does not depend on the node also existing in inactive_nodes.
func seedActiveNodeOnly(t *testing.T, store *Store, pubkey string) {
	t.Helper()
	if _, err := store.db.Exec(`INSERT INTO nodes (public_key, name) VALUES (?, ?)`, pubkey, "n_"+pubkey[:4]); err != nil {
		t.Fatalf("seed active-only node %s: %v", pubkey, err)
	}
}

// seedInactiveNodeOnly inserts a node into inactive_nodes only, leaving
// nodes untouched -- the exact shape retention (MoveStaleNodes) produces for
// a node that has gone stale, and the case the pre-#7 LWW guard (which only
// ever read nodes.configured_scope_at) could not protect at all.
func seedInactiveNodeOnly(t *testing.T, store *Store, pubkey string) {
	t.Helper()
	if _, err := store.db.Exec(`INSERT INTO inactive_nodes (public_key, name) VALUES (?, ?)`, pubkey, "n_"+pubkey[:4]); err != nil {
		t.Fatalf("seed inactive-only node %s: %v", pubkey, err)
	}
}

func configuredScopeInactive(t *testing.T, store *Store, pubkey string) (sql.NullString, sql.NullString) {
	t.Helper()
	var sc, at sql.NullString
	if err := store.db.QueryRow(
		`SELECT configured_scope, configured_scope_at FROM inactive_nodes WHERE public_key = ?`, pubkey,
	).Scan(&sc, &at); err != nil {
		t.Fatalf("read inactive_nodes configured_scope for %s: %v", pubkey, err)
	}
	return sc, at
}

func openNeighborsStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestHandleNeighborsReportWritesSelfAndResponded(t *testing.T) {
	store := openNeighborsStore(t)

	// Report keys are UPPERCASE; nodes.public_key is lowercase hex.
	const originUpper = "FEEDCA4AD4E2AE615AAAB3CB73FAEC6EF0C7AF4D410F5C58A70FC0F724B7C933"
	const respUpper = "B0D17C59FCF580592F8FB78B67D2F0CE9E9187EF3483A765BDFF1D7947A5109C"
	const timeoutUpper = "0CE5EA7CFA3AB01D11810EF56B73DD899CD6C58644D6A6832A5C1AE89AFC5E25"
	originLower := "feedca4ad4e2ae615aaab3cb73faec6ef0c7af4d410f5c58a70fc0f724b7c933"
	respLower := "b0d17c59fcf580592f8fb78b67d2f0ce9e9187ef3483a765bdff1d7947a5109c"
	timeoutLower := "0ce5ea7cfa3ab01d11810ef56b73dd899cd6c58644d6a6832a5c1ae89afc5e25"

	seedNode(t, store, originLower)
	seedNode(t, store, respLower)
	seedNode(t, store, timeoutLower)
	// Pre-existing confirmed scope on the timeout node — a timeout must NOT clobber it.
	if err := store.UpdateNodeConfiguredScope(timeoutLower, "eu", "2026-07-24T00:00:00Z"); err != nil {
		t.Fatal(err)
	}

	report := map[string]interface{}{
		"timestamp": "2026-07-25T10:46:14.000000+00:00",
		"origin_id": originUpper,
		"self":      map[string]interface{}{"scopes": "*"},
		"neighbors": []interface{}{
			map[string]interface{}{"pubkey": timeoutUpper, "scopes": "", "status": "timeout"},
			map[string]interface{}{"pubkey": respUpper, "scopes": "de,eu", "status": "responded"},
		},
	}
	handleNeighborsReport(store, "test", "obs-topic-id", report)

	// self scopes written under the lowercased origin_id.
	if sc, _ := configuredScope(t, store, originLower); !sc.Valid || sc.String != "*" {
		t.Errorf("self configured_scope = %v, want '*'", sc)
	}
	// responded neighbor written, lowercased, and normalized to #-prefixed scope form.
	if sc, _ := configuredScope(t, store, respLower); !sc.Valid || sc.String != "#de,#eu" {
		t.Errorf("responded configured_scope = %v, want '#de,#eu'", sc)
	}
	// timeout neighbor untouched — prior confirmed value survives.
	if sc, _ := configuredScope(t, store, timeoutLower); !sc.Valid || sc.String != "#eu" {
		t.Errorf("timeout node configured_scope = %v, want prior '#eu' (must not be cleared)", sc)
	}
}

func TestHandleNeighborsReportUnknownNeighborIsNoop(t *testing.T) {
	store := openNeighborsStore(t)
	// No node seeded for this pubkey — UPDATE must match no row (no insert, no error).
	report := map[string]interface{}{
		"timestamp": "2026-07-25T10:46:14Z",
		"neighbors": []interface{}{
			map[string]interface{}{"pubkey": "aa" + "00000000000000000000000000000000000000000000000000000000000000"[2:], "scopes": "de", "status": "responded"},
		},
	}
	handleNeighborsReport(store, "test", "obs", report)
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("nodes count = %d, want 0 (report must not create nodes)", n)
	}
}

func TestHandleNeighborsReportRespondedEmptyScopeIsStored(t *testing.T) {
	store := openNeighborsStore(t)
	pk := "cc00000000000000000000000000000000000000000000000000000000000001"
	seedNode(t, store, pk)
	// A responded query with empty scopes is a valid "no scopes configured".
	report := map[string]interface{}{
		"timestamp": "2026-07-25T10:46:14Z",
		"neighbors": []interface{}{
			map[string]interface{}{"pubkey": pk, "scopes": "", "status": "responded"},
		},
	}
	handleNeighborsReport(store, "test", "obs", report)
	sc, at := configuredScope(t, store, pk)
	if !sc.Valid || sc.String != "" {
		t.Errorf("responded-empty configured_scope = %v, want stored empty string", sc)
	}
	if !at.Valid || at.String == "" {
		t.Errorf("configured_scope_at = %v, want the report timestamp", at)
	}
}

func TestUpdateNodeConfiguredScopeLastWriteWins(t *testing.T) {
	store := openNeighborsStore(t)
	pk := "dd00000000000000000000000000000000000000000000000000000000000001"
	seedNode(t, store, pk)

	if err := store.UpdateNodeConfiguredScope(pk, "eu", "2026-07-25T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	// Older report must NOT clobber the newer confirmed value.
	if err := store.UpdateNodeConfiguredScope(pk, "stale", "2026-07-24T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if sc, _ := configuredScope(t, store, pk); sc.String != "#eu" {
		t.Errorf("configured_scope = %q, want '#eu' (older report must not overwrite)", sc.String)
	}
	// Newer report updates.
	if err := store.UpdateNodeConfiguredScope(pk, "de", "2026-07-26T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if sc, _ := configuredScope(t, store, pk); sc.String != "#de" {
		t.Errorf("configured_scope = %q, want '#de' (newer report should update)", sc.String)
	}
	// inactive_nodes mirrored.
	var inactive sql.NullString
	if err := store.db.QueryRow(`SELECT configured_scope FROM inactive_nodes WHERE public_key = ?`, pk).Scan(&inactive); err != nil {
		t.Fatal(err)
	}
	if inactive.String != "#de" {
		t.Errorf("inactive_nodes.configured_scope = %q, want '#de'", inactive.String)
	}
}

// TestUpdateNodeConfiguredScopeNormalizesAndOrdersByInstant proves the
// last-write-wins guard orders by chronological instant, not by raw string.
// A "+02:00" report that is lexicographically "greater" but chronologically
// EARLIER than the stored UTC value must not win, and stored timestamps are
// canonicalized to UTC "Z" form regardless of the incoming offset/precision.
// Flagged upstream on #1865 (SaarMesh-Bot, PR #1867 commit 631686a).
func TestUpdateNodeConfiguredScopeNormalizesAndOrdersByInstant(t *testing.T) {
	store := openNeighborsStore(t)
	pk := "ee00000000000000000000000000000000000000000000000000000000000001"
	seedNode(t, store, pk)

	// Baseline: noon UTC.
	if err := store.UpdateNodeConfiguredScope(pk, "eu", "2026-07-25T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, at := configuredScope(t, store, pk); at.String != "2026-07-25T12:00:00Z" {
		t.Fatalf("stored at = %q, want canonical 'Z' form", at.String)
	}

	// "2026-07-25T13:30:00+02:00" == 11:30Z, chronologically EARLIER than 12:00Z,
	// but lexicographically GREATER ("13:30..." > "12:00...Z"). Must be skipped.
	if err := store.UpdateNodeConfiguredScope(pk, "stale", "2026-07-25T13:30:00+02:00"); err != nil {
		t.Fatal(err)
	}
	if sc, _ := configuredScope(t, store, pk); sc.String != "#eu" {
		t.Errorf("configured_scope = %q, want '#eu' (earlier +02:00 report must not win)", sc.String)
	}

	// "2026-07-25T15:00:00+02:00" == 13:00Z, chronologically LATER. Must update,
	// and be stored canonicalized to UTC.
	if err := store.UpdateNodeConfiguredScope(pk, "de", "2026-07-25T15:00:00+02:00"); err != nil {
		t.Fatal(err)
	}
	sc, at := configuredScope(t, store, pk)
	if sc.String != "#de" {
		t.Errorf("configured_scope = %q, want '#de' (later +02:00 report should win)", sc.String)
	}
	if at.String != "2026-07-25T13:00:00Z" {
		t.Errorf("stored at = %q, want canonical '2026-07-25T13:00:00Z'", at.String)
	}

	// Firmware's real format (microseconds + "+00:00") canonicalizes to "Z".
	if err := store.UpdateNodeConfiguredScope(pk, "dk", "2026-07-26T09:43:48.000000+00:00"); err != nil {
		t.Fatal(err)
	}
	if _, at := configuredScope(t, store, pk); at.String != "2026-07-26T09:43:48Z" {
		t.Errorf("stored at = %q, want canonical '2026-07-26T09:43:48Z'", at.String)
	}
}

// Tests below cover the #1865 follow-up: firmware's /neighbors report added
// self.default_scope (2026-07-29), a direct self-report of the region a
// node floods to by default -- distinct from the packet-inferred
// default_scope UpdateNodeDefaultScope sets elsewhere in the ingest path.

func TestHandleNeighborsReportWritesSelfDefaultScope(t *testing.T) {
	store := openNeighborsStore(t)
	const originUpper = "FEEDCA4AD4E2AE615AAAB3CB73FAEC6EF0C7AF4D410F5C58A70FC0F724B7C933"
	originLower := "feedca4ad4e2ae615aaab3cb73faec6ef0c7af4d410f5c58a70fc0f724b7c933"
	seedNode(t, store, originLower)

	report := map[string]interface{}{
		"timestamp": "2026-07-29T22:40:00.000000+00:00",
		"origin_id": originUpper,
		"self":      map[string]interface{}{"scopes": "dk", "default_scope": "dk"},
	}
	handleNeighborsReport(store, "test", "obs-topic-id", report)

	sc, at := defaultScopeConfirmed(t, store, originLower)
	if !sc.Valid || sc.String != "#dk" {
		t.Errorf("default_scope = %v, want '#dk' (normalized like configured_scope)", sc)
	}
	if !at.Valid || at.String == "" {
		t.Errorf("default_scope_confirmed_at = %v, want the report timestamp", at)
	}
}

func TestHandleNeighborsReportSelfDefaultScope_WildcardPassthrough(t *testing.T) {
	store := openNeighborsStore(t)
	pk := "ff00000000000000000000000000000000000000000000000000000000000001"
	seedNode(t, store, pk)

	report := map[string]interface{}{
		"timestamp": "2026-07-29T22:40:00Z",
		"origin_id": pk,
		"self":      map[string]interface{}{"scopes": "*", "default_scope": "*"},
	}
	handleNeighborsReport(store, "test", "obs", report)

	sc, _ := defaultScopeConfirmed(t, store, pk)
	if sc.String != "*" {
		t.Errorf("default_scope = %q, want '*' unchanged (protocol wildcard, not a real region)", sc.String)
	}
}

func TestUpdateNodeDefaultScope_DoesNotOverwriteConfirmedValue(t *testing.T) {
	store := openNeighborsStore(t)
	pk := "aa11111111111111111111111111111111111111111111111111111111111111"
	seedNode(t, store, pk)

	// Firmware self-report confirms #dk.
	if err := store.UpdateNodeDefaultScopeConfirmed(pk, "dk", "2026-07-29T22:40:00Z"); err != nil {
		t.Fatal(err)
	}
	// A later packet-inferred observation must NOT be allowed to downgrade it,
	// even though UpdateNodeDefaultScope has no timestamp-ordering concept of
	// its own -- confirmation always wins over inference.
	if err := store.UpdateNodeDefaultScope(pk, "#eu"); err != nil {
		t.Fatal(err)
	}
	sc, at := defaultScopeConfirmed(t, store, pk)
	if sc.String != "#dk" {
		t.Errorf("default_scope = %q, want '#dk' (confirmed value must survive an inferred write)", sc.String)
	}
	if !at.Valid || at.String == "" {
		t.Errorf("default_scope_confirmed_at = %v, want unchanged (still set)", at)
	}
}

func TestUpdateNodeDefaultScope_StillWritesWhenUnconfirmed(t *testing.T) {
	store := openNeighborsStore(t)
	pk := "bb11111111111111111111111111111111111111111111111111111111111111"
	seedNode(t, store, pk)

	// No confirmation yet -- packet inference should write normally, same as
	// before this feature existed.
	if err := store.UpdateNodeDefaultScope(pk, "#eu"); err != nil {
		t.Fatal(err)
	}
	sc, at := defaultScopeConfirmed(t, store, pk)
	if sc.String != "#eu" {
		t.Errorf("default_scope = %q, want '#eu'", sc.String)
	}
	if at.Valid && at.String != "" {
		t.Errorf("default_scope_confirmed_at = %v, want still unset (inference doesn't confirm)", at)
	}
}

func TestUpdateNodeDefaultScopeConfirmedLastWriteWins(t *testing.T) {
	store := openNeighborsStore(t)
	pk := "cc11111111111111111111111111111111111111111111111111111111111111"
	seedNode(t, store, pk)

	if err := store.UpdateNodeDefaultScopeConfirmed(pk, "eu", "2026-07-25T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	// Older report must NOT clobber the newer confirmed value.
	if err := store.UpdateNodeDefaultScopeConfirmed(pk, "stale", "2026-07-24T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if sc, _ := defaultScopeConfirmed(t, store, pk); sc.String != "#eu" {
		t.Errorf("default_scope = %q, want '#eu' (older report must not overwrite)", sc.String)
	}
	// Newer report updates.
	if err := store.UpdateNodeDefaultScopeConfirmed(pk, "dk", "2026-07-26T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	sc, _ := defaultScopeConfirmed(t, store, pk)
	if sc.String != "#dk" {
		t.Errorf("default_scope = %q, want '#dk' (newer report should update)", sc.String)
	}
	// inactive_nodes mirrored.
	var inactive sql.NullString
	if err := store.db.QueryRow(`SELECT default_scope FROM inactive_nodes WHERE public_key = ?`, pk).Scan(&inactive); err != nil {
		t.Fatal(err)
	}
	if inactive.String != "#dk" {
		t.Errorf("inactive_nodes.default_scope = %q, want '#dk'", inactive.String)
	}
}

// ─── #7: invalid/missing timestamp is a strict no-op (no server-time fallback) ─

func TestUpdateNodeConfiguredScope_MalformedTimestampIsNoop(t *testing.T) {
	store := openNeighborsStore(t)
	pk := "a100000000000000000000000000000000000000000000000000000000000001"
	seedActiveNodeOnly(t, store, pk)

	if err := store.UpdateNodeConfiguredScope(pk, "eu", "2026-07-25T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateNodeConfiguredScope(pk, "de", "not-a-timestamp"); err != nil {
		t.Fatal(err)
	}
	sc, at := configuredScope(t, store, pk)
	if sc.String != "#eu" {
		t.Errorf("configured_scope = %q, want unchanged '#eu' (malformed timestamp must be a no-op)", sc.String)
	}
	if at.String != "2026-07-25T12:00:00Z" {
		t.Errorf("configured_scope_at = %q, want unchanged", at.String)
	}
}

func TestUpdateNodeConfiguredScope_MissingTimestampIsNoop(t *testing.T) {
	store := openNeighborsStore(t)
	pk := "a200000000000000000000000000000000000000000000000000000000000002"
	seedActiveNodeOnly(t, store, pk)

	if err := store.UpdateNodeConfiguredScope(pk, "eu", "2026-07-25T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateNodeConfiguredScope(pk, "de", ""); err != nil {
		t.Fatal(err)
	}
	sc, at := configuredScope(t, store, pk)
	if sc.String != "#eu" {
		t.Errorf("configured_scope = %q, want unchanged '#eu' (missing timestamp must be a no-op)", sc.String)
	}
	if at.String != "2026-07-25T12:00:00Z" {
		t.Errorf("configured_scope_at = %q, want unchanged", at.String)
	}
}

// TestUpdateNodeConfiguredScope_InvalidTimestampCreatesNoFalseEvidenceOnNullRow
// pins the specific hazard the pre-#7 code had: writing a blank
// configured_scope_at over a row that had never received ANY evidence would
// turn "no evidence yet" (NULL/NULL) into a false "confirmed empty" state
// (empty scope, empty timestamp). The fix must reject before ever reaching SQL.
func TestUpdateNodeConfiguredScope_InvalidTimestampCreatesNoFalseEvidenceOnNullRow(t *testing.T) {
	store := openNeighborsStore(t)
	pk := "a300000000000000000000000000000000000000000000000000000000000003"
	seedActiveNodeOnly(t, store, pk) // configured_scope/at both NULL, never touched

	if err := store.UpdateNodeConfiguredScope(pk, "de", "garbage"); err != nil {
		t.Fatal(err)
	}
	sc, at := configuredScope(t, store, pk)
	if sc.Valid {
		t.Errorf("configured_scope = %v, want still NULL (no evidence was ever accepted)", sc)
	}
	if at.Valid {
		t.Errorf("configured_scope_at = %v, want still NULL, not a false empty confirmation", at)
	}
}

// TestUpdateNodeConfiguredScope_InvalidTimestampChangesNeitherTable proves
// the reject happens before either UPDATE is even attempted -- both nodes
// and inactive_nodes must be untouched, not just one of them.
func TestUpdateNodeConfiguredScope_InvalidTimestampChangesNeitherTable(t *testing.T) {
	store := openNeighborsStore(t)
	pk := "a400000000000000000000000000000000000000000000000000000000000004"
	seedNode(t, store, pk) // both tables

	if err := store.UpdateNodeConfiguredScope(pk, "eu", "2026-07-25T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateNodeConfiguredScope(pk, "de", "not-a-timestamp"); err != nil {
		t.Fatal(err)
	}
	sc, at := configuredScope(t, store, pk)
	if sc.String != "#eu" || at.String != "2026-07-25T12:00:00Z" {
		t.Errorf("nodes row changed: configured_scope=%q configured_scope_at=%q, want unchanged", sc.String, at.String)
	}
	isc, iat := configuredScopeInactive(t, store, pk)
	if isc.String != "#eu" || iat.String != "2026-07-25T12:00:00Z" {
		t.Errorf("inactive_nodes row changed: configured_scope=%q configured_scope_at=%q, want unchanged", isc.String, iat.String)
	}
}

func TestUpdateNodeConfiguredScope_EqualTimestampIsNoop(t *testing.T) {
	store := openNeighborsStore(t)
	pk := "a500000000000000000000000000000000000000000000000000000000000005"
	seedActiveNodeOnly(t, store, pk)

	if err := store.UpdateNodeConfiguredScope(pk, "eu", "2026-07-25T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	// Exact same instant, different scope value — must NOT overwrite.
	if err := store.UpdateNodeConfiguredScope(pk, "de", "2026-07-25T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	sc, _ := configuredScope(t, store, pk)
	if sc.String != "#eu" {
		t.Errorf("configured_scope = %q, want unchanged '#eu' (equal timestamp must be an idempotent no-op)", sc.String)
	}
}

func TestUpdateNodeConfiguredScope_WildcardPreservedDirectly(t *testing.T) {
	store := openNeighborsStore(t)
	pk := "a600000000000000000000000000000000000000000000000000000000000006"
	seedActiveNodeOnly(t, store, pk)

	if err := store.UpdateNodeConfiguredScope(pk, "*", "2026-07-25T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	sc, at := configuredScope(t, store, pk)
	if sc.String != "*" {
		t.Errorf("configured_scope = %q, want literal '*' (never rewritten to '#*')", sc.String)
	}
	if at.String != "2026-07-25T12:00:00Z" {
		t.Errorf("configured_scope_at = %q, want the report timestamp", at.String)
	}
}

// ─── #7: independent LWW for active-only, inactive-only, and both-at-once ──

func TestUpdateNodeConfiguredScope_ActiveOnlyNode_LastWriteWinsApplies(t *testing.T) {
	store := openNeighborsStore(t)
	pk := "b100000000000000000000000000000000000000000000000000000000000007"
	seedActiveNodeOnly(t, store, pk)

	if err := store.UpdateNodeConfiguredScope(pk, "eu", "2026-07-25T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateNodeConfiguredScope(pk, "stale", "2026-07-24T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if sc, _ := configuredScope(t, store, pk); sc.String != "#eu" {
		t.Errorf("configured_scope = %q, want '#eu' (older report on an active-only node must still be rejected)", sc.String)
	}
	if err := store.UpdateNodeConfiguredScope(pk, "de", "2026-07-26T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if sc, _ := configuredScope(t, store, pk); sc.String != "#de" {
		t.Errorf("configured_scope = %q, want '#de' (newer report on an active-only node must update)", sc.String)
	}
}

// TestUpdateNodeConfiguredScope_InactiveOnlyNode_LastWriteWinsApplies is the
// direct regression test for #7's core finding: before this fix, the LWW
// guard only ever read nodes.configured_scope_at, so a node that exists ONLY
// in inactive_nodes (the shape MoveStaleNodes produces) had no ordering
// protection at all -- an older report could clobber a newer confirmed
// value there. seedNode (which inserts into both tables) could never catch
// this; seedInactiveNodeOnly reproduces the real retention-produced shape.
func TestUpdateNodeConfiguredScope_InactiveOnlyNode_LastWriteWinsApplies(t *testing.T) {
	store := openNeighborsStore(t)
	pk := "b200000000000000000000000000000000000000000000000000000000000008"
	seedInactiveNodeOnly(t, store, pk)

	if err := store.UpdateNodeConfiguredScope(pk, "eu", "2026-07-25T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	// Older report must be ignored — exactly the scenario the pre-fix code
	// could not protect (SELECT ... FROM nodes found no row, so the guard
	// was skipped and the unconditional UPDATE ran anyway).
	if err := store.UpdateNodeConfiguredScope(pk, "stale", "2026-07-24T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if sc, _ := configuredScopeInactive(t, store, pk); sc.String != "#eu" {
		t.Errorf("inactive_nodes.configured_scope = %q, want '#eu' (older report on an inactive-only node must be rejected)", sc.String)
	}
	if err := store.UpdateNodeConfiguredScope(pk, "de", "2026-07-26T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if sc, _ := configuredScopeInactive(t, store, pk); sc.String != "#de" {
		t.Errorf("inactive_nodes.configured_scope = %q, want '#de' (newer report on an inactive-only node must update)", sc.String)
	}
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE public_key = ?`, pk).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("nodes row count = %d, want 0 (this node only ever existed in inactive_nodes)", n)
	}
}

// TestUpdateNodeConfiguredScope_BothTablesIndependentLWW reproduces the
// resurrection-overlap shape found during design review: a node can
// transiently exist in BOTH nodes and inactive_nodes at once (nothing
// deletes the stale inactive_nodes row when a node resurrects), and the two
// rows can carry different configured_scope_at values. Each table's own
// timestamp must gate its own update independently of the other.
func TestUpdateNodeConfiguredScope_BothTablesIndependentLWW(t *testing.T) {
	store := openNeighborsStore(t)
	pk := "b300000000000000000000000000000000000000000000000000000000000009"
	seedActiveNodeOnly(t, store, pk)
	seedInactiveNodeOnly(t, store, pk)

	// nodes gets fresher evidence than inactive_nodes.
	if err := store.UpdateNodeConfiguredScope(pk, "eu", "2026-07-25T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	// Seed an OLDER value directly into inactive_nodes only, bypassing the
	// guarded method, simulating a stale orphaned row left over from before
	// the two rows diverged.
	if _, err := store.db.Exec(
		`UPDATE inactive_nodes SET configured_scope = ?, configured_scope_at = ? WHERE public_key = ?`,
		"#dk", "2026-07-20T00:00:00Z", pk); err != nil {
		t.Fatal(err)
	}

	// A report timestamped BETWEEN the two existing values: newer than
	// inactive_nodes' (2026-07-20), older than nodes' (2026-07-25).
	if err := store.UpdateNodeConfiguredScope(pk, "de", "2026-07-22T00:00:00Z"); err != nil {
		t.Fatal(err)
	}

	// nodes must be UNCHANGED — its own timestamp (2026-07-25) is newer.
	if sc, at := configuredScope(t, store, pk); sc.String != "#eu" || at.String != "2026-07-25T12:00:00Z" {
		t.Errorf("nodes = (%q,%q), want unchanged ('#eu','2026-07-25T12:00:00Z')", sc.String, at.String)
	}
	// inactive_nodes MUST update — its own timestamp (2026-07-20) was older.
	if sc, at := configuredScopeInactive(t, store, pk); sc.String != "#de" || at.String != "2026-07-22T00:00:00Z" {
		t.Errorf("inactive_nodes = (%q,%q), want ('#de','2026-07-22T00:00:00Z')", sc.String, at.String)
	}
}

// ─── #7: whole-transaction rollback on partial failure ─────────────────────

// TestUpdateNodeConfiguredScope_SecondUpdateFailureRollsBackFirst forces the
// inactive_nodes UPDATE to fail — by dropping that table in this test's own
// throwaway temp-file database only, never touching the real schema — after
// the nodes UPDATE would otherwise have succeeded, and proves the whole
// transaction rolls back: nodes must NOT retain the update either.
func TestUpdateNodeConfiguredScope_SecondUpdateFailureRollsBackFirst(t *testing.T) {
	store := openNeighborsStore(t)
	pk := "c100000000000000000000000000000000000000000000000000000000000010"
	seedActiveNodeOnly(t, store, pk)

	if _, err := store.db.Exec(`DROP TABLE inactive_nodes`); err != nil {
		t.Fatalf("drop inactive_nodes in throwaway test db: %v", err)
	}

	err := store.UpdateNodeConfiguredScope(pk, "eu", "2026-07-25T12:00:00Z")
	if err == nil {
		t.Fatal("want a non-nil error once inactive_nodes is gone, got nil")
	}

	sc, at := configuredScope(t, store, pk)
	if sc.Valid || at.Valid {
		t.Errorf("nodes row = (%v,%v), want still NULL/NULL — the nodes UPDATE must have been rolled back when the inactive_nodes UPDATE failed", sc, at)
	}
}

// ─── #7: one log line per invalid report, not one per neighbor ─────────────

func TestHandleNeighborsReportInvalidTimestampLogsOnceForConfiguredScope(t *testing.T) {
	store := openNeighborsStore(t)
	respA := "d100000000000000000000000000000000000000000000000000000000000011"
	respB := "d200000000000000000000000000000000000000000000000000000000000012"
	respC := "d300000000000000000000000000000000000000000000000000000000000013"
	seedActiveNodeOnly(t, store, respA)
	seedActiveNodeOnly(t, store, respB)
	seedActiveNodeOnly(t, store, respC)

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	report := map[string]interface{}{
		"timestamp": "not-a-valid-timestamp",
		"neighbors": []interface{}{
			map[string]interface{}{"pubkey": respA, "scopes": "de", "status": "responded"},
			map[string]interface{}{"pubkey": respB, "scopes": "eu", "status": "responded"},
			map[string]interface{}{"pubkey": respC, "scopes": "dk", "status": "responded"},
		},
	}
	handleNeighborsReport(store, "test", "obs-many", report)

	got := strings.Count(buf.String(), "configured-scope evidence ignored")
	if got != 1 {
		t.Errorf("logged %d 'configured-scope evidence ignored' line(s) for a 3-neighbor report, want exactly 1", got)
	}
	for _, pk := range []string{respA, respB, respC} {
		if sc, _ := configuredScope(t, store, pk); sc.Valid {
			t.Errorf("node %s configured_scope = %v, want still NULL", pk, sc)
		}
	}
}
