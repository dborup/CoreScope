package main

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"
)

// Issue #337: bound neighbor-report evidence and reject far-future
// timestamps. See neighbor_report_bounds.go for the policy and the trust
// model these tests pin.
//
// Every test here pins reportTSNow to a fixed instant so "far future" is
// deterministic rather than racing the wall clock.

const issue337Now = "2026-10-08T12:00:00Z"

// pinReportClock freezes the report-evidence clock for the duration of the
// test, mirroring the pruneNeighborMetricsNow swap-in-test pattern.
func pinReportClock(t *testing.T, iso string) time.Time {
	t.Helper()
	now, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		t.Fatalf("parse pinned clock %q: %v", iso, err)
	}
	prev := reportTSNow
	reportTSNow = func() time.Time { return now }
	t.Cleanup(func() { reportTSNow = prev })
	return now
}

// ---------------------------------------------------------------------------
// Requirement: validate node public keys before writes, preserving full-key
// identity (never truncated to a prefix).
// ---------------------------------------------------------------------------

func TestNormalizeNodePubkey(t *testing.T) {
	const lower = "feedca4ad4e2ae615aaab3cb73faec6ef0c7af4d410f5c58a70fc0f724b7c933"
	const upper = "FEEDCA4AD4E2AE615AAAB3CB73FAEC6EF0C7AF4D410F5C58A70FC0F724B7C933"

	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"lowercase full key", lower, lower, true},
		{"uppercase full key is lowercased, not truncated", upper, lower, true},
		{"mixed case full key", "FeEdCa4aD4e2Ae615aAaB3cB73fAeC6eF0c7Af4d410F5c58A70fC0f724B7c933", lower, true},
		{"empty", "", "", false},
		{"short legacy prefix", "deadbeef", "", false},
		{"63 hex chars", lower[:63], "", false},
		{"65 hex chars", lower + "a", "", false},
		{"64 chars with a non-hex byte", strings.Repeat("g", 64), "", false},
		{"64 chars, one bad byte at the end", lower[:63] + "z", "", false},
		{"64 chars, one bad byte at the start", "z" + lower[1:], "", false},
		{"whitespace padded", " " + lower[1:], "", false},
		{"sql injection attempt", "' OR 1=1 --" + strings.Repeat("a", 53), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := normalizeNodePubkey(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("normalizeNodePubkey(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
			}
			// Full-key identity: an accepted key is never shortened.
			if ok && len(got) != nodePubkeyHexLen {
				t.Fatalf("accepted key length = %d, want %d (full-key identity must be preserved)", len(got), nodePubkeyHexLen)
			}
		})
	}
}

// Malformed neighbor keys must never reach observer_neighbors: those rows are
// inserted verbatim with no foreign key to nodes, so junk keys accumulate as
// permanent, unjoinable rows. A valid uppercase key in the same report is
// still stored, lowercased and in full.
func TestReplaceObserverNeighbors_RejectsMalformedPubkeys(t *testing.T) {
	pinReportClock(t, issue337Now)
	store := openNeighborsStore(t)
	const goodUpper = "B0D17C59FCF580592F8FB78B67D2F0CE9E9187EF3483A765BDFF1D7947A5109C"
	const goodLower = "b0d17c59fcf580592f8fb78b67d2f0ce9e9187ef3483a765bdff1d7947a5109c"

	entries := []ObserverNeighborEntry{
		{Pubkey: goodUpper, Status: "responded", Scopes: "#dk"},
		{Pubkey: "deadbeef", Status: "responded", Scopes: "#dk"},
		{Pubkey: strings.Repeat("z", 64), Status: "timeout"},
		{Pubkey: "", Status: "timeout"},
	}
	if err := store.ReplaceObserverNeighbors("obs-337", entries, "2026-10-08T11:00:00Z"); err != nil {
		t.Fatalf("ReplaceObserverNeighbors: %v", err)
	}

	rows, err := store.db.Query(`SELECT neighbor_pubkey FROM observer_neighbors WHERE observer_id = ? ORDER BY neighbor_pubkey`, "obs-337")
	if err != nil {
		t.Fatalf("query observer_neighbors: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var pk string
		if err := rows.Scan(&pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, pk)
	}
	if len(got) != 1 || got[0] != goodLower {
		t.Fatalf("observer_neighbors rows = %v, want exactly [%s]", got, goodLower)
	}
}

func TestRecordObserverNeighborMetrics_RejectsMalformedPubkeys(t *testing.T) {
	pinReportClock(t, issue337Now)
	store := openNeighborsStore(t)
	const goodUpper = "B0D17C59FCF580592F8FB78B67D2F0CE9E9187EF3483A765BDFF1D7947A5109C"
	const goodLower = "b0d17c59fcf580592f8fb78b67d2f0ce9e9187ef3483a765bdff1d7947a5109c"
	snr := -7.5

	entries := []ObserverNeighborEntry{
		{Pubkey: goodUpper, Status: "responded", SNR: &snr},
		{Pubkey: "deadbeef", Status: "responded", SNR: &snr},
		{Pubkey: strings.Repeat("Z", 64), Status: "responded", SNR: &snr},
	}
	if err := store.RecordObserverNeighborMetrics("obs-337", entries, "2026-10-08T11:00:00Z"); err != nil {
		t.Fatalf("RecordObserverNeighborMetrics: %v", err)
	}

	rows, err := store.db.Query(`SELECT neighbor_pubkey FROM observer_neighbor_metrics WHERE observer_id = ? ORDER BY neighbor_pubkey`, "obs-337")
	if err != nil {
		t.Fatalf("query observer_neighbor_metrics: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var pk string
		if err := rows.Scan(&pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, pk)
	}
	if len(got) != 1 || got[0] != goodLower {
		t.Fatalf("observer_neighbor_metrics rows = %v, want exactly [%s]", got, goodLower)
	}
}

// End-to-end: a report whose neighbor list is entirely malformed keys writes
// no neighbor rows at all, yet the observer is still recorded as a
// /neighbors-report sender (the #1865 opt-in-feature signal, which must not
// depend on payload validity).
func TestHandleNeighborsReport_MalformedKeysWriteNothingButStillTouchObserver(t *testing.T) {
	pinReportClock(t, issue337Now)
	store := openNeighborsStore(t)
	if _, err := store.db.Exec(`INSERT INTO observers (id, name) VALUES (?, ?)`, "obs-337", "Observer 337"); err != nil {
		t.Fatalf("seed observer: %v", err)
	}

	report := map[string]interface{}{
		"timestamp": "2026-10-08T11:00:00Z",
		"origin_id": "not-a-pubkey",
		"self":      map[string]interface{}{"scopes": "dk", "default_scope": "dk"},
		"neighbors": []interface{}{
			map[string]interface{}{"pubkey": "deadbeef", "scopes": "dk", "status": "responded", "snr": -4.0},
			map[string]interface{}{"pubkey": strings.Repeat("q", 64), "scopes": "dk", "status": "responded", "snr": -4.0},
		},
	}
	handleNeighborsReport(store, "test", "obs-337", report)

	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM observer_neighbors`).Scan(&n); err != nil {
		t.Fatalf("count observer_neighbors: %v", err)
	}
	if n != 0 {
		t.Fatalf("observer_neighbors rows = %d, want 0", n)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM observer_neighbor_metrics`).Scan(&n); err != nil {
		t.Fatalf("count observer_neighbor_metrics: %v", err)
	}
	if n != 0 {
		t.Fatalf("observer_neighbor_metrics rows = %d, want 0", n)
	}
	var at string
	if err := store.db.QueryRow(`SELECT COALESCE(last_neighbors_report_at, '') FROM observers WHERE id = ?`, "obs-337").Scan(&at); err != nil {
		t.Fatalf("select last_neighbors_report_at: %v", err)
	}
	if at != "2026-10-08T11:00:00Z" {
		t.Fatalf("last_neighbors_report_at = %q, want '2026-10-08T11:00:00Z'", at)
	}
}

// ---------------------------------------------------------------------------
// Requirement: reject far-future timestamps on every report path.
// ---------------------------------------------------------------------------

func TestNormalizeReportTS_FutureClockTolerance(t *testing.T) {
	now := pinReportClock(t, issue337Now)
	fmtTS := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"past", "2026-09-01T00:00:00Z", "2026-09-01T00:00:00Z"},
		{"exactly now", fmtTS(0), issue337Now},
		{"one second ahead", fmtTS(time.Second), fmtTS(time.Second)},
		{"at the tolerance boundary", fmtTS(maxReportFutureSkew), fmtTS(maxReportFutureSkew)},
		{"one second past the boundary", fmtTS(maxReportFutureSkew + time.Second), ""},
		{"an hour ahead", fmtTS(time.Hour), ""},
		{"a day ahead", fmtTS(24 * time.Hour), ""},
		{"year 9999 dominates last-write-wins forever", "9999-12-31T23:59:59Z", ""},
		{"far future with an offset suffix", "2099-01-01T00:00:00+02:00", ""},
		// Pre-existing contract, unchanged by #337.
		{"empty", "", ""},
		{"unparseable", "not-a-timestamp", ""},
		{"naive (zone-less) is still rejected", "2026-09-01T00:00:00", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeReportTS(tc.in); got != tc.want {
				t.Fatalf("normalizeReportTS(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A far-future configured-scope report must be a complete no-op on a node
// with no prior evidence: writing it would both fabricate evidence and, worse,
// make every genuine report for years to come lose the lexicographic
// last-write-wins comparison.
func TestUpdateNodeConfiguredScope_RejectsFarFutureTimestamp(t *testing.T) {
	pinReportClock(t, issue337Now)
	store := openNeighborsStore(t)
	pk := "a337000000000000000000000000000000000000000000000000000000000001"
	seedNode(t, store, pk)

	if err := store.UpdateNodeConfiguredScope(pk, "dk", "9999-12-31T23:59:59Z"); err != nil {
		t.Fatalf("UpdateNodeConfiguredScope: %v", err)
	}
	if sc, at := configuredScope(t, store, pk); sc.Valid || at.Valid {
		t.Fatalf("after far-future report: configured_scope = %v / %v, want both NULL", sc, at)
	}
	if sc, at := configuredScopeInactive(t, store, pk); sc.Valid || at.Valid {
		t.Fatalf("after far-future report (inactive_nodes): %v / %v, want both NULL", sc, at)
	}

	// A genuine report still lands, and a later far-future one cannot
	// overwrite it.
	if err := store.UpdateNodeConfiguredScope(pk, "dk", "2026-10-08T11:00:00Z"); err != nil {
		t.Fatalf("UpdateNodeConfiguredScope (genuine): %v", err)
	}
	if err := store.UpdateNodeConfiguredScope(pk, "se", "9999-12-31T23:59:59Z"); err != nil {
		t.Fatalf("UpdateNodeConfiguredScope (far future over genuine): %v", err)
	}
	sc, at := configuredScope(t, store, pk)
	if !sc.Valid || sc.String != "#dk" || at.String != "2026-10-08T11:00:00Z" {
		t.Fatalf("configured_scope = %v at %v, want '#dk' at '2026-10-08T11:00:00Z'", sc, at)
	}
}

func TestUpdateNodeDefaultScopeConfirmed_RejectsFarFutureTimestamp(t *testing.T) {
	pinReportClock(t, issue337Now)
	store := openNeighborsStore(t)
	pk := "a337000000000000000000000000000000000000000000000000000000000002"
	seedNode(t, store, pk)

	if err := store.UpdateNodeDefaultScopeConfirmed(pk, "dk", "9999-12-31T23:59:59Z"); err != nil {
		t.Fatalf("UpdateNodeDefaultScopeConfirmed: %v", err)
	}
	if sc, at := defaultScopeConfirmed(t, store, pk); sc.Valid || at.Valid {
		t.Fatalf("after far-future report: default_scope = %v / %v, want both NULL", sc, at)
	}
	if sc, at := defaultScopeConfirmedInactive(t, store, pk); sc.Valid || at.Valid {
		t.Fatalf("after far-future report (inactive_nodes): %v / %v, want both NULL", sc, at)
	}
}

// The freshness path is the nastiest of the three: TouchObserverNeighborsReport
// stores MAX(stored, incoming), and ReplaceObserverNeighbors then skips any
// report whose timestamp isn't the stored one. A single year-9999 report would
// therefore freeze the observer's Direct Neighbors snapshot permanently. This
// pins that the poisoned write never happens and the next genuine report is
// still applied.
func TestHandleNeighborsReport_FarFutureCannotFreezeReportFreshness(t *testing.T) {
	pinReportClock(t, issue337Now)
	store := openNeighborsStore(t)
	if _, err := store.db.Exec(`INSERT INTO observers (id, name) VALUES (?, ?)`, "obs-337", "Observer 337"); err != nil {
		t.Fatalf("seed observer: %v", err)
	}
	nbA := "a337000000000000000000000000000000000000000000000000000000000003"
	nbB := "a337000000000000000000000000000000000000000000000000000000000004"

	poisoned := map[string]interface{}{
		"timestamp": "9999-12-31T23:59:59Z",
		"neighbors": []interface{}{
			map[string]interface{}{"pubkey": nbA, "scopes": "dk", "status": "responded"},
		},
	}
	handleNeighborsReport(store, "test", "obs-337", poisoned)

	var at string
	if err := store.db.QueryRow(`SELECT COALESCE(last_neighbors_report_at, '') FROM observers WHERE id = ?`, "obs-337").Scan(&at); err != nil {
		t.Fatalf("select last_neighbors_report_at: %v", err)
	}
	if at != "" {
		t.Fatalf("last_neighbors_report_at = %q after a year-9999 report, want '' (never poisoned)", at)
	}

	genuine := map[string]interface{}{
		"timestamp": "2026-10-08T11:30:00Z",
		"neighbors": []interface{}{
			map[string]interface{}{"pubkey": nbB, "scopes": "dk", "status": "responded"},
		},
	}
	handleNeighborsReport(store, "test", "obs-337", genuine)

	if err := store.db.QueryRow(`SELECT COALESCE(last_neighbors_report_at, '') FROM observers WHERE id = ?`, "obs-337").Scan(&at); err != nil {
		t.Fatalf("select last_neighbors_report_at: %v", err)
	}
	if at != "2026-10-08T11:30:00Z" {
		t.Fatalf("last_neighbors_report_at = %q, want '2026-10-08T11:30:00Z' (genuine report must still be accepted)", at)
	}
	var pk string
	if err := store.db.QueryRow(`SELECT neighbor_pubkey FROM observer_neighbors WHERE observer_id = ?`, "obs-337").Scan(&pk); err != nil {
		t.Fatalf("select observer_neighbors: %v", err)
	}
	if pk != nbB {
		t.Fatalf("observer_neighbors holds %q, want the genuine report's neighbor %q", pk, nbB)
	}
}

// A far-future report must be logged exactly once, with the same
// once-per-report discipline the invalid-timestamp line already has.
func TestHandleNeighborsReport_FarFutureTimestampLogsOnce(t *testing.T) {
	pinReportClock(t, issue337Now)
	store := openNeighborsStore(t)

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	report := map[string]interface{}{
		"timestamp": "9999-12-31T23:59:59Z",
		"origin_id": "a337000000000000000000000000000000000000000000000000000000000005",
		"self":      map[string]interface{}{"scopes": "dk", "default_scope": "dk"},
		"neighbors": []interface{}{
			map[string]interface{}{"pubkey": "a337000000000000000000000000000000000000000000000000000000000006", "scopes": "dk", "status": "responded"},
			map[string]interface{}{"pubkey": "a337000000000000000000000000000000000000000000000000000000000007", "scopes": "dk", "status": "responded"},
			map[string]interface{}{"pubkey": "a337000000000000000000000000000000000000000000000000000000000008", "scopes": "dk", "status": "responded"},
		},
	}
	handleNeighborsReport(store, "test", "obs-337", report)

	if n := strings.Count(buf.String(), "evidence ignored"); n != 1 {
		t.Fatalf("logged %d 'evidence ignored' lines for one report, want exactly 1:\n%s", n, buf.String())
	}
}

// ---------------------------------------------------------------------------
// Requirement: bound scope-string length.
// ---------------------------------------------------------------------------

func TestUpdateNodeConfiguredScope_ScopeLengthBoundary(t *testing.T) {
	pinReportClock(t, issue337Now)
	store := openNeighborsStore(t)

	atLimit := strings.Repeat("a", maxReportScopeBytes)
	overLimit := strings.Repeat("a", maxReportScopeBytes+1)

	// At the limit: accepted, stored normalized ("#"-prefixed), full length
	// preserved.
	pkOK := "a337000000000000000000000000000000000000000000000000000000000009"
	seedNode(t, store, pkOK)
	if err := store.UpdateNodeConfiguredScope(pkOK, atLimit, "2026-10-08T11:00:00Z"); err != nil {
		t.Fatalf("UpdateNodeConfiguredScope (at limit): %v", err)
	}
	sc, at := configuredScope(t, store, pkOK)
	if !sc.Valid || sc.String != "#"+atLimit || at.String != "2026-10-08T11:00:00Z" {
		t.Fatalf("at-limit scope not stored: %v / %v", sc, at)
	}

	// One byte over: complete no-op, leaving the no-evidence NULL/NULL state
	// intact rather than fabricating a "confirmed empty" row.
	pkBad := "a33700000000000000000000000000000000000000000000000000000000000a"
	seedNode(t, store, pkBad)
	if err := store.UpdateNodeConfiguredScope(pkBad, overLimit, "2026-10-08T11:00:00Z"); err != nil {
		t.Fatalf("UpdateNodeConfiguredScope (over limit): %v", err)
	}
	if sc, at := configuredScope(t, store, pkBad); sc.Valid || at.Valid {
		t.Fatalf("over-limit scope wrote %v / %v, want both NULL", sc, at)
	}
	if sc, at := configuredScopeInactive(t, store, pkBad); sc.Valid || at.Valid {
		t.Fatalf("over-limit scope wrote to inactive_nodes: %v / %v, want both NULL", sc, at)
	}

	// An over-limit report must not clobber previously confirmed evidence
	// either, even with a newer timestamp.
	if err := store.UpdateNodeConfiguredScope(pkOK, overLimit, "2026-10-08T11:59:00Z"); err != nil {
		t.Fatalf("UpdateNodeConfiguredScope (over limit over good): %v", err)
	}
	sc, at = configuredScope(t, store, pkOK)
	if sc.String != "#"+atLimit || at.String != "2026-10-08T11:00:00Z" {
		t.Fatalf("over-limit report clobbered confirmed evidence: %v / %v", sc, at)
	}
}

func TestUpdateNodeDefaultScopeConfirmed_RejectsOversizedScope(t *testing.T) {
	pinReportClock(t, issue337Now)
	store := openNeighborsStore(t)
	pk := "a33700000000000000000000000000000000000000000000000000000000000b"
	seedNode(t, store, pk)

	if err := store.UpdateNodeDefaultScopeConfirmed(pk, strings.Repeat("b", maxReportScopeBytes+1), "2026-10-08T11:00:00Z"); err != nil {
		t.Fatalf("UpdateNodeDefaultScopeConfirmed: %v", err)
	}
	if sc, at := defaultScopeConfirmed(t, store, pk); sc.Valid || at.Valid {
		t.Fatalf("over-limit default_scope wrote %v / %v, want both NULL", sc, at)
	}
}

// A huge scope string is malformed input, not evidence — but the neighbor
// itself is still a real zero-hop entry in the observer's firmware neighbor
// table, so it stays in observer_neighbors with its reported status and no
// scope data. The same rule as a timeout: losing the scope must not lose the
// neighbor.
func TestHandleNeighborsReport_OversizedScopeKeepsNeighborDropsEvidence(t *testing.T) {
	pinReportClock(t, issue337Now)
	store := openNeighborsStore(t)
	pk := "a33700000000000000000000000000000000000000000000000000000000000c"
	seedNode(t, store, pk)

	report := map[string]interface{}{
		"timestamp": "2026-10-08T11:00:00Z",
		"neighbors": []interface{}{
			map[string]interface{}{"pubkey": pk, "scopes": strings.Repeat("dk,", maxReportScopeBytes), "status": "responded", "snr": -3.5},
		},
	}
	handleNeighborsReport(store, "test", "obs-337", report)

	if sc, at := configuredScope(t, store, pk); sc.Valid || at.Valid {
		t.Fatalf("oversized scope wrote configured_scope %v / %v, want both NULL", sc, at)
	}
	var status string
	var scopes string
	if err := store.db.QueryRow(`SELECT status, COALESCE(scopes, '') FROM observer_neighbors WHERE neighbor_pubkey = ?`, pk).Scan(&status, &scopes); err != nil {
		t.Fatalf("select observer_neighbors: %v", err)
	}
	if status != "responded" {
		t.Fatalf("status = %q, want 'responded' (reported status must be preserved)", status)
	}
	if scopes != "" {
		t.Fatalf("stored scopes = %q (len %d), want '' (unusable evidence is not stored)", scopes, len(scopes))
	}
	// The RF metrics reading is independent of the scope query and must survive.
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM observer_neighbor_metrics WHERE neighbor_pubkey = ?`, pk).Scan(&n); err != nil {
		t.Fatalf("count metrics: %v", err)
	}
	if n != 1 {
		t.Fatalf("observer_neighbor_metrics rows = %d, want 1 (snr is not scope evidence)", n)
	}
}

// ---------------------------------------------------------------------------
// Requirement: bound report collection sizes and per-report work.
// ---------------------------------------------------------------------------

func TestHandleNeighborsReport_BoundsNeighborCount(t *testing.T) {
	pinReportClock(t, issue337Now)
	store := openNeighborsStore(t)
	if _, err := store.db.Exec(`INSERT INTO observers (id, name) VALUES (?, ?)`, "obs-337", "Observer 337"); err != nil {
		t.Fatalf("seed observer: %v", err)
	}

	const over = maxReportNeighbors + 40
	neighbors := make([]interface{}, 0, over)
	for i := 0; i < over; i++ {
		neighbors = append(neighbors, map[string]interface{}{
			"pubkey": fmt.Sprintf("%064x", i+1),
			"scopes": "dk",
			"status": "responded",
			"snr":    -5.0,
		})
	}
	report := map[string]interface{}{
		"timestamp": "2026-10-08T11:00:00Z",
		"neighbors": neighbors,
	}
	handleNeighborsReport(store, "test", "obs-337", report)

	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM observer_neighbors WHERE observer_id = ?`, "obs-337").Scan(&n); err != nil {
		t.Fatalf("count observer_neighbors: %v", err)
	}
	if n != maxReportNeighbors {
		t.Fatalf("observer_neighbors rows = %d, want the %d cap", n, maxReportNeighbors)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM observer_neighbor_metrics WHERE observer_id = ?`, "obs-337").Scan(&n); err != nil {
		t.Fatalf("count observer_neighbor_metrics: %v", err)
	}
	if n != maxReportNeighbors {
		t.Fatalf("observer_neighbor_metrics rows = %d, want the %d cap", n, maxReportNeighbors)
	}
	// The cap keeps the first N entries in report order: the firmware already
	// truncates its own 10 KB-capped report by ordering, so dropping the tail
	// matches how a legitimately oversized report is already shortened.
	var first, last string
	if err := store.db.QueryRow(`SELECT MIN(neighbor_pubkey), MAX(neighbor_pubkey) FROM observer_neighbors WHERE observer_id = ?`, "obs-337").Scan(&first, &last); err != nil {
		t.Fatalf("select bounds: %v", err)
	}
	if first != fmt.Sprintf("%064x", 1) || last != fmt.Sprintf("%064x", maxReportNeighbors) {
		t.Fatalf("stored key range = [%s..%s], want the first %d entries in report order", first, last, maxReportNeighbors)
	}
}

// ---------------------------------------------------------------------------
// Requirement: preserve the #7 / #8 / #9 behaviour under the new policy.
// ---------------------------------------------------------------------------

func TestHandleNeighborsReport_Issue337PreservesEvidenceContract(t *testing.T) {
	pinReportClock(t, issue337Now)
	store := openNeighborsStore(t)

	origin := "a33700000000000000000000000000000000000000000000000000000000001a"
	wildcard := "a33700000000000000000000000000000000000000000000000000000000001b"
	emptyOK := "a33700000000000000000000000000000000000000000000000000000000001c"
	timedOut := "a33700000000000000000000000000000000000000000000000000000000001d"
	unknown := "a33700000000000000000000000000000000000000000000000000000000001e"
	inactiveOnly := "a33700000000000000000000000000000000000000000000000000000000001f"

	seedNode(t, store, origin)
	seedNode(t, store, wildcard)
	seedNode(t, store, emptyOK)
	seedNode(t, store, timedOut)
	seedNode(t, store, unknown)
	seedInactiveNodeOnly(t, store, inactiveOnly)

	// Prior confirmed evidence on the timeout node — a timeout must not clear it.
	if err := store.UpdateNodeConfiguredScope(timedOut, "eu", "2026-10-07T00:00:00Z"); err != nil {
		t.Fatalf("seed timeout evidence: %v", err)
	}

	report := map[string]interface{}{
		"timestamp": "2026-10-08T11:00:00Z",
		"origin_id": strings.ToUpper(origin),
		"self":      map[string]interface{}{"scopes": "dk,eu", "default_scope": "dk"},
		"neighbors": []interface{}{
			map[string]interface{}{"pubkey": strings.ToUpper(wildcard), "scopes": "*", "status": "responded"},
			map[string]interface{}{"pubkey": emptyOK, "scopes": "", "status": "responded"},
			map[string]interface{}{"pubkey": timedOut, "scopes": "", "status": "timeout"},
			map[string]interface{}{"pubkey": inactiveOnly, "scopes": "se", "status": "responded"},
		},
	}
	handleNeighborsReport(store, "test", "obs-337", report)

	// self scopes + confirmed default_scope land under the full lowercased key.
	if sc, _ := configuredScope(t, store, origin); sc.String != "#dk,#eu" {
		t.Errorf("self configured_scope = %v, want '#dk,#eu'", sc)
	}
	if sc, at := defaultScopeConfirmed(t, store, origin); sc.String != "#dk" || at.String != "2026-10-08T11:00:00Z" {
		t.Errorf("self default_scope = %v / %v, want '#dk' at the report time", sc, at)
	}
	// "*" passes through literally, never rewritten to "#*".
	if sc, _ := configuredScope(t, store, wildcard); sc.String != "*" {
		t.Errorf("wildcard configured_scope = %v, want '*'", sc)
	}
	// Successful-empty: "" with a non-NULL timestamp, distinct from unknown.
	sc, at := configuredScope(t, store, emptyOK)
	if !sc.Valid || sc.String != "" || !at.Valid {
		t.Errorf("responded-empty configured_scope = %v / %v, want '' with a timestamp", sc, at)
	}
	// Unknown: a node absent from the report keeps NULL/NULL.
	if sc, at := configuredScope(t, store, unknown); sc.Valid || at.Valid {
		t.Errorf("unreported node = %v / %v, want both NULL", sc, at)
	}
	// Timeout preservation: prior evidence survives untouched.
	sc, at = configuredScope(t, store, timedOut)
	if sc.String != "#eu" || at.String != "2026-10-07T00:00:00Z" {
		t.Errorf("timeout node = %v / %v, want the earlier '#eu' evidence unchanged", sc, at)
	}
	// Inactive-node mirroring: a node that lives only in inactive_nodes still
	// gets its confirmed scope.
	if sc, at := configuredScopeInactive(t, store, inactiveOnly); sc.String != "#se" || at.String != "2026-10-08T11:00:00Z" {
		t.Errorf("inactive-only node = %v / %v, want '#se' at the report time", sc, at)
	}
}

// Older / equal / newer last-write-wins ordering, re-pinned under the #337
// clock so the future bound cannot be mistaken for an ordering change.
func TestUpdateNodeConfiguredScope_OrderingUnderFutureBound(t *testing.T) {
	pinReportClock(t, issue337Now)
	store := openNeighborsStore(t)
	pk := "a337000000000000000000000000000000000000000000000000000000000020"
	seedNode(t, store, pk)

	if err := store.UpdateNodeConfiguredScope(pk, "dk", "2026-10-08T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	// Older: ignored.
	if err := store.UpdateNodeConfiguredScope(pk, "se", "2026-10-08T09:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if sc, at := configuredScope(t, store, pk); sc.String != "#dk" || at.String != "2026-10-08T10:00:00Z" {
		t.Fatalf("after older report: %v / %v, want '#dk' at 10:00", sc, at)
	}
	// Equal: idempotent no-op, value unchanged.
	if err := store.UpdateNodeConfiguredScope(pk, "no", "2026-10-08T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if sc, at := configuredScope(t, store, pk); sc.String != "#dk" || at.String != "2026-10-08T10:00:00Z" {
		t.Fatalf("after equal report: %v / %v, want '#dk' at 10:00", sc, at)
	}
	// Newer, still within tolerance: accepted.
	if err := store.UpdateNodeConfiguredScope(pk, "se", "2026-10-08T12:10:00Z"); err != nil {
		t.Fatal(err)
	}
	if sc, at := configuredScope(t, store, pk); sc.String != "#se" || at.String != "2026-10-08T12:10:00Z" {
		t.Fatalf("after newer report: %v / %v, want '#se' at 12:10", sc, at)
	}
}
