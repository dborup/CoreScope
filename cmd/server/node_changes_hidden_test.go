package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

// #1015: Node Changes (and the Network Digest change counts built on it) must
// apply the node list's identity rule, identityHidden: node blacklist,
// observer blacklist and hidden name prefixes. A node's current name decides
// whether it is hidden; a "name" row is also left out when its old or new
// value is a hidden name, so the rename that hid a node is never published.

func nodeChangeIDsByKey(entries []NodeChangeEntry) map[string]int {
	out := make(map[string]int, len(entries))
	for _, e := range entries {
		out[e.PublicKey]++
	}
	return out
}

// TestComputeNodeChanges_ExcludesHiddenNamePrefix: every row of a node whose
// current name starts with a hidden prefix is left out, including the rename
// that hid it ("Public" -> "🚫 Private"); a visible node next to it is listed.
func TestComputeNodeChanges_ExcludesHiddenNamePrefix(t *testing.T) {
	srv, _ := setupTestServer(t)
	ensureInactiveNodesTable(t, srv)
	ensureNodeChangesTable(t, srv)
	srv.cfg.SetHiddenNamePrefixes([]string{"🚫"})

	insertNewNodeRow(t, srv, "chghidden00000001", "🚫 Private", "repeater", nil, nil, "2026-07-01T00:00:00Z")
	insertNewNodeRow(t, srv, "chgvisible0000001", "Public", "repeater", nil, nil, "2026-07-01T00:00:00Z")
	insertNodeChangeRow(t, srv, "chghidden00000001", "name", "Public", "🚫 Private", "2026-07-29T10:00:00Z")
	insertNodeChangeRow(t, srv, "chghidden00000001", "position", "56.000000,10.000000", "56.100000,10.000000", "2026-07-29T11:00:00Z")
	insertNodeChangeRow(t, srv, "chghidden00000001", "role", "companion", "repeater", "2026-07-29T12:00:00Z")
	insertNodeChangeRow(t, srv, "chgvisible0000001", "role", "companion", "repeater", "2026-07-29T13:00:00Z")

	entries, err := srv.computeNodeChanges(50)
	if err != nil {
		t.Fatalf("computeNodeChanges: %v", err)
	}
	keys := nodeChangeIDsByKey(entries)
	if keys["chghidden00000001"] != 0 {
		t.Errorf("hidden-prefix node's changes must not be listed, got %+v", entries)
	}
	if keys["chgvisible0000001"] != 1 {
		t.Errorf("visible node's change must still be listed, got %+v", entries)
	}
}

// TestComputeNodeChanges_ExcludesHiddenNameRowsAfterUnhide: a node that hid
// itself and later took a visible name again is listed, but the two name rows
// that carry the hidden name are not; its other rows stay.
func TestComputeNodeChanges_ExcludesHiddenNameRowsAfterUnhide(t *testing.T) {
	srv, _ := setupTestServer(t)
	ensureInactiveNodesTable(t, srv)
	ensureNodeChangesTable(t, srv)
	srv.cfg.SetHiddenNamePrefixes([]string{"🚫"})

	insertNewNodeRow(t, srv, "chgunhide00000001", "Public again", "repeater", nil, nil, "2026-07-01T00:00:00Z")
	insertNodeChangeRow(t, srv, "chgunhide00000001", "name", "Public", "🚫 Secret", "2026-07-29T10:00:00Z")
	insertNodeChangeRow(t, srv, "chgunhide00000001", "name", "🚫 Secret", "Public again", "2026-07-29T11:00:00Z")
	insertNodeChangeRow(t, srv, "chgunhide00000001", "position", "56.000000,10.000000", "56.100000,10.000000", "2026-07-29T12:00:00Z")
	insertNodeChangeRow(t, srv, "chgunhide00000001", "name", "Public again", "Public 2", "2026-07-29T13:00:00Z")

	entries, err := srv.computeNodeChanges(50)
	if err != nil {
		t.Fatalf("computeNodeChanges: %v", err)
	}
	if len(entries) != 2 || entries[0].ChangeType != "name" || entries[0].NewValue != "Public 2" ||
		entries[1].ChangeType != "position" {
		t.Errorf("want only the visible rename and the move, newest first; got %+v", entries)
	}
	for _, e := range entries {
		if e.OldValue == "🚫 Secret" || e.NewValue == "🚫 Secret" {
			t.Errorf("a row carrying the hidden name must not be listed: %+v", e)
		}
	}
}

// TestComputeNodeChanges_ExcludesObserverBlacklisted: identityHidden also
// hides an observer-blacklisted pubkey, as in New Nodes (#1009).
func TestComputeNodeChanges_ExcludesObserverBlacklisted(t *testing.T) {
	srv, _ := setupTestServer(t)
	ensureNodeChangesTable(t, srv)
	srv.cfg.ObserverBlacklist = []string{"CHGOBSBLACK000001"}

	insertNodeChangeRow(t, srv, "chgobsblack000001", "role", "companion", "repeater", "2026-07-29T10:00:00Z")
	insertNodeChangeRow(t, srv, "chgobsvisible0001", "role", "companion", "repeater", "2026-07-29T09:00:00Z")

	entries, err := srv.computeNodeChanges(50)
	if err != nil {
		t.Fatalf("computeNodeChanges: %v", err)
	}
	keys := nodeChangeIDsByKey(entries)
	if keys["chgobsblack000001"] != 0 || keys["chgobsvisible0001"] != 1 {
		t.Errorf("want only the non-blacklisted observer's row, got %+v", entries)
	}
}

// TestComputeNodeChanges_HiddenRowsDoNotShortenLimit: hidden rows are skipped
// before the limit is applied, so limit=2 still returns 2 visible rows.
func TestComputeNodeChanges_HiddenRowsDoNotShortenLimit(t *testing.T) {
	srv, _ := setupTestServer(t)
	ensureInactiveNodesTable(t, srv)
	ensureNodeChangesTable(t, srv)
	srv.cfg.SetHiddenNamePrefixes([]string{"🚫"})
	srv.cfg.SetNodeBlacklist([]string{"chglimitblack0001"})

	insertNewNodeRow(t, srv, "chglimithidden001", "🚫 Newest", "repeater", nil, nil, "2026-07-01T00:00:00Z")
	insertNodeChangeRow(t, srv, "chglimitvisible01", "role", "companion", "repeater", "2026-07-29T10:00:00Z")
	insertNodeChangeRow(t, srv, "chglimitvisible02", "role", "companion", "repeater", "2026-07-29T11:00:00Z")
	insertNodeChangeRow(t, srv, "chglimitblack0001", "role", "companion", "repeater", "2026-07-29T12:00:00Z")
	insertNodeChangeRow(t, srv, "chglimithidden001", "role", "companion", "repeater", "2026-07-29T13:00:00Z")

	entries, err := srv.computeNodeChanges(2)
	if err != nil {
		t.Fatalf("computeNodeChanges: %v", err)
	}
	if len(entries) != 2 || entries[0].PublicKey != "chglimitvisible02" || entries[1].PublicKey != "chglimitvisible01" {
		t.Errorf("want the two visible rows, newest first; got %+v", entries)
	}
}

// TestComputeNodeChanges_HiddenPrefixIsCaseSensitive pins IsNameHidden's
// case-sensitive prefix rule on this path: "Private" hides "Private x" but
// not "private y".
func TestComputeNodeChanges_HiddenPrefixIsCaseSensitive(t *testing.T) {
	srv, _ := setupTestServer(t)
	ensureInactiveNodesTable(t, srv)
	ensureNodeChangesTable(t, srv)
	srv.cfg.SetHiddenNamePrefixes([]string{"Private"})

	insertNewNodeRow(t, srv, "chgcasehidden0001", "Private x", "repeater", nil, nil, "2026-07-01T00:00:00Z")
	insertNewNodeRow(t, srv, "chgcasevisible001", "private y", "repeater", nil, nil, "2026-07-01T00:00:00Z")
	insertNodeChangeRow(t, srv, "chgcasehidden0001", "role", "companion", "repeater", "2026-07-29T10:00:00Z")
	insertNodeChangeRow(t, srv, "chgcasevisible001", "role", "companion", "repeater", "2026-07-29T11:00:00Z")

	entries, err := srv.computeNodeChanges(50)
	if err != nil {
		t.Fatalf("computeNodeChanges: %v", err)
	}
	keys := nodeChangeIDsByKey(entries)
	if keys["chgcasehidden0001"] != 0 || keys["chgcasevisible001"] != 1 {
		t.Errorf(`want "private y" listed and "Private x" hidden, got %+v`, entries)
	}
}

// TestComputeNodeChanges_NameLookupErrorFailsClosed: the current name decides
// visibility, so a fetch that cannot read it (nodes table unreadable) is an
// error, not a list that may contain hidden nodes. On master the name lookup
// failed silently and the rows were served without names.
func TestComputeNodeChanges_NameLookupErrorFailsClosed(t *testing.T) {
	srv, _ := setupTestServer(t)
	ensureNodeChangesTable(t, srv)
	srv.cfg.SetHiddenNamePrefixes([]string{"🚫"})
	insertNodeChangeRow(t, srv, "chglookupfail0001", "role", "companion", "repeater", "2026-07-29T10:00:00Z")
	if _, err := srv.db.conn.Exec(`DROP TABLE nodes`); err != nil {
		t.Fatalf("drop nodes: %v", err)
	}

	entries, err := srv.computeNodeChanges(50)
	if err == nil || entries != nil {
		t.Errorf("want an error and no entries, got %+v, %v", entries, err)
	}
}

// TestComputeNetworkDigest_HiddenPrefixChangesNotCounted: the digest's change
// counts leave a hidden-prefix node's rows out.
func TestComputeNetworkDigest_HiddenPrefixChangesNotCounted(t *testing.T) {
	srv, _ := setupTestServer(t)
	ensureInactiveNodesTable(t, srv)
	ensureNodeChangesTable(t, srv)
	srv.cfg.SetHiddenNamePrefixes([]string{"🚫"})

	now := time.Now().UTC()
	at := func(h int) string { return now.Add(-time.Duration(h) * time.Hour).Format(time.RFC3339) }
	insertNewNodeRow(t, srv, "dgchghidden000001", "🚫 Private", "repeater", nil, nil, at(24*30))
	insertNewNodeRow(t, srv, "dgchgvisible00001", "Public", "repeater", nil, nil, at(24*30))
	insertNodeChangeRow(t, srv, "dgchghidden000001", "name", "Was public", "🚫 Private", at(5))
	insertNodeChangeRow(t, srv, "dgchghidden000001", "position", "56.000000,10.000000", "56.100000,10.000000", at(4))
	insertNodeChangeRow(t, srv, "dgchghidden000001", "role", "companion", "repeater", at(3))
	insertNodeChangeRow(t, srv, "dgchghidden000001", "resurrected", "2026-01-01T00:00:00Z", "", at(2))
	insertNodeChangeRow(t, srv, "dgchgvisible00001", "role", "companion", "repeater", at(1))

	d, err := srv.computeNetworkDigest(now.Add(-7*24*time.Hour), "all")
	if err != nil {
		t.Fatalf("computeNetworkDigest: %v", err)
	}
	if d.RoleChanges != 1 || d.NameChanges != 0 || d.PositionMoves != 0 || d.Resurrections != 0 {
		t.Errorf("counts = role %d, name %d, position %d, resurrected %d; want 1, 0, 0, 0",
			d.RoleChanges, d.NameChanges, d.PositionMoves, d.Resurrections)
	}
}

// TestComputeNetworkDigest_ChangesCappedFlagWithHiddenRow: ChangesCapped asks
// whether the SQL LIMIT cut off rows, so it is based on the rows SQL returned,
// not on the list left after hidden rows are dropped (#1009 review F1).
func TestComputeNetworkDigest_ChangesCappedFlagWithHiddenRow(t *testing.T) {
	srv, _ := setupTestServer(t)
	ensureInactiveNodesTable(t, srv)
	ensureNodeChangesTable(t, srv)
	srv.cfg.SetHiddenNamePrefixes([]string{"~"})

	now := time.Now().UTC()
	insertNewNodeRow(t, srv, "dgcaphidden000001", "~Hidden", "repeater", nil, nil, now.Add(-30*24*time.Hour).Format(time.RFC3339))
	// Rows are ordered by id (insertion order). Oldest first: one visible row
	// the fetch cap cuts off, then 499 visible rows, then the hidden row as
	// the newest -- all inside the window.
	for i := nodeChangesSQLFetchCap; i >= 1; i-- {
		insertNodeChangeRow(t, srv, fmt.Sprintf("dgcap%013d01", i), "role", "companion", "repeater", now.Add(-time.Duration(i)*time.Minute).Format(time.RFC3339))
	}
	insertNodeChangeRow(t, srv, "dgcaphidden000001", "role", "companion", "repeater", now.Format(time.RFC3339))

	d, err := srv.computeNetworkDigest(now.Add(-7*24*time.Hour), "all")
	if err != nil {
		t.Fatal(err)
	}
	if !d.ChangesCapped {
		t.Errorf("ChangesCapped=false although the SQL LIMIT cut off a visible row (RoleChanges=%d)", d.RoleChanges)
	}
	if d.RoleChanges != nodeChangesSQLFetchCap-1 {
		t.Errorf("RoleChanges = %d, want %d (the visible rows of the full fetch)", d.RoleChanges, nodeChangesSQLFetchCap-1)
	}
}

// TestComputeNetworkDigest_ChangesNotCappedWhenOldestScannedRowIsHidden: the
// capped check uses the oldest row SQL returned, not the oldest visible row.
// Here the full fetch reaches a hidden row from before the window, so nothing
// inside the window was cut off, even though every visible row is inside it.
// Green on master too (master does not drop the hidden row); it pins the
// fix's use of the scanned row.
func TestComputeNetworkDigest_ChangesNotCappedWhenOldestScannedRowIsHidden(t *testing.T) {
	srv, _ := setupTestServer(t)
	ensureInactiveNodesTable(t, srv)
	ensureNodeChangesTable(t, srv)
	srv.cfg.SetHiddenNamePrefixes([]string{"~"})

	now := time.Now().UTC()
	insertNewNodeRow(t, srv, "dgoldhidden000001", "~Hidden", "repeater", nil, nil, now.Add(-60*24*time.Hour).Format(time.RFC3339))
	insertNodeChangeRow(t, srv, "dgoldhidden000001", "role", "companion", "repeater", now.Add(-30*24*time.Hour).Format(time.RFC3339))
	for i := nodeChangesSQLFetchCap - 1; i >= 1; i-- {
		insertNodeChangeRow(t, srv, fmt.Sprintf("dgold%013d01", i), "role", "companion", "repeater", now.Add(-time.Duration(i)*time.Minute).Format(time.RFC3339))
	}

	d, err := srv.computeNetworkDigest(now.Add(-7*24*time.Hour), "all")
	if err != nil {
		t.Fatal(err)
	}
	if d.ChangesCapped {
		t.Errorf("ChangesCapped=true although the fetch reached a row from before the window")
	}
	if d.RoleChanges != nodeChangesSQLFetchCap-1 {
		t.Errorf("RoleChanges = %d, want %d", d.RoleChanges, nodeChangesSQLFetchCap-1)
	}
}

// TestNodeChangesEndpoints_FollowLiveHiddenChanges: no result is served stale
// after a hidden-prefix or blacklist change. Both endpoints are requested
// before and after each change, through the router.
func TestNodeChangesEndpoints_FollowLiveHiddenChanges(t *testing.T) {
	srv, router := setupTestServer(t)
	ensureInactiveNodesTable(t, srv)
	ensureNodeChangesTable(t, srv)

	now := time.Now().UTC()
	insertNewNodeRow(t, srv, "chglive0000000001", "~Secret", "repeater", nil, nil, now.Add(-30*24*time.Hour).Format(time.RFC3339))
	insertNodeChangeRow(t, srv, "chglive0000000001", "role", "companion", "repeater", now.Add(-time.Hour).Format(time.RFC3339))

	listed := func() bool {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/node-changes?limit=50", nil))
		if w.Code != 200 {
			t.Fatalf("node-changes status %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			NodeChanges []NodeChangeEntry `json:"nodeChanges"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode node-changes: %v", err)
		}
		return nodeChangeIDsByKey(resp.NodeChanges)["chglive0000000001"] > 0
	}
	counted := func() int {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/network-digest", nil))
		if w.Code != 200 {
			t.Fatalf("network-digest status %d: %s", w.Code, w.Body.String())
		}
		var d NetworkDigest
		if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
			t.Fatalf("decode network-digest: %v", err)
		}
		return d.RoleChanges
	}
	step := func(label string, want bool) {
		t.Helper()
		if got := listed(); got != want {
			t.Errorf("%s: node-changes lists the node = %v, want %v", label, got, want)
		}
		wantCount := 0
		if want {
			wantCount = 1
		}
		if got := counted(); got != wantCount {
			t.Errorf("%s: network-digest roleChanges = %d, want %d", label, got, wantCount)
		}
	}

	step("before any change", true)
	srv.cfg.SetHiddenNamePrefixes([]string{"~"})
	step("after adding the prefix", false)
	srv.cfg.SetHiddenNamePrefixes(nil)
	step("after removing the prefix", true)
	srv.cfg.SetNodeBlacklist([]string{"CHGLIVE0000000001"})
	step("after blacklisting", false)
	srv.cfg.SetNodeBlacklist(nil)
	step("after un-blacklisting", true)
}

// TestNodeChanges_BlacklistedAndVisibleStillWork is the regression guard for
// what already worked: a blacklisted key (any case, padded) is neither listed
// nor counted, and a visible node's rows -- name changes included -- are
// listed and counted as before.
func TestNodeChanges_BlacklistedAndVisibleStillWork(t *testing.T) {
	srv, _ := setupTestServer(t)
	ensureInactiveNodesTable(t, srv)
	ensureNodeChangesTable(t, srv)
	srv.cfg.SetNodeBlacklist([]string{"  CHGREGBLACK00001 "})

	now := time.Now().UTC()
	at := func(h int) string { return now.Add(-time.Duration(h) * time.Hour).Format(time.RFC3339) }
	insertNewNodeRow(t, srv, "chgregwhite00001", "White", "repeater", nil, nil, at(24*30))
	insertNodeChangeRow(t, srv, "chgregblack00001", "role", "companion", "repeater", at(4))
	insertNodeChangeRow(t, srv, "chgregwhite00001", "name", "Old", "White", at(3))
	insertNodeChangeRow(t, srv, "chgregwhite00001", "role", "companion", "repeater", at(2))

	entries, err := srv.computeNodeChanges(50)
	if err != nil {
		t.Fatalf("computeNodeChanges: %v", err)
	}
	keys := nodeChangeIDsByKey(entries)
	if keys["chgregblack00001"] != 0 || keys["chgregwhite00001"] != 2 {
		t.Errorf("want both rows of the visible node only, got %+v", entries)
	}
	for _, e := range entries {
		if e.Name != "White" || e.Role != "repeater" {
			t.Errorf("visible row must keep its resolved name and role, got %+v", e)
		}
	}
	d, err := srv.computeNetworkDigest(now.Add(-7*24*time.Hour), "all")
	if err != nil {
		t.Fatalf("computeNetworkDigest: %v", err)
	}
	if d.RoleChanges != 1 || d.NameChanges != 1 {
		t.Errorf("counts = role %d, name %d; want 1, 1", d.RoleChanges, d.NameChanges)
	}
}
