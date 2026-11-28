package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

// #1006: New Nodes (and the Network Digest counts built on it) must apply the
// same identity rule as the node list, identityHidden: node blacklist,
// observer blacklist and hidden name prefixes.

func newNodeKeys(entries []NewNodeEntry) map[string]bool {
	out := make(map[string]bool, len(entries))
	for _, e := range entries {
		out[e.PublicKey] = true
	}
	return out
}

// TestComputeNewNodes_ExcludesHiddenNamePrefix: a node first seen inside the
// window whose name starts with a hidden prefix is not listed; a visible node
// next to it still is.
func TestComputeNewNodes_ExcludesHiddenNamePrefix(t *testing.T) {
	srv, _ := setupTestServer(t)
	ensureInactiveNodesTable(t, srv)
	srv.cfg.SetHiddenNamePrefixes([]string{"🚫"})

	now := time.Now().UTC()
	insertNewNodeRow(t, srv, "hiddenprefix00001", "🚫 Private", "repeater", f64(56.0), f64(10.0), now.Add(-1*time.Minute).Format(time.RFC3339))
	insertNewNodeRow(t, srv, "visiblenode000001", "Public", "repeater", f64(56.0), f64(10.0), now.Add(-2*time.Minute).Format(time.RFC3339))

	entries, err := srv.computeNewNodes(50)
	if err != nil {
		t.Fatalf("computeNewNodes: %v", err)
	}
	keys := newNodeKeys(entries)
	if keys["hiddenprefix00001"] {
		t.Errorf("hidden-prefix node must not be listed, got %+v", entries)
	}
	if !keys["visiblenode000001"] {
		t.Errorf("visible node must still be listed, got %+v", entries)
	}
}

// TestComputeNewNodes_ExcludesObserverBlacklisted: identityHidden also hides
// an observer-blacklisted pubkey, as on the per-node views.
func TestComputeNewNodes_ExcludesObserverBlacklisted(t *testing.T) {
	srv, _ := setupTestServer(t)
	ensureInactiveNodesTable(t, srv)
	srv.cfg.ObserverBlacklist = []string{"OBSBLACKLISTED0001"}

	now := time.Now().UTC()
	insertNewNodeRow(t, srv, "obsblacklisted0001", "Observer", "companion", nil, nil, now.Add(-1*time.Minute).Format(time.RFC3339))

	entries, err := srv.computeNewNodes(50)
	if err != nil {
		t.Fatalf("computeNewNodes: %v", err)
	}
	if newNodeKeys(entries)["obsblacklisted0001"] {
		t.Errorf("observer-blacklisted pubkey must not be listed, got %+v", entries)
	}
}

// TestComputeNewNodes_HiddenRowsDoNotShortenLimit: hidden rows are skipped
// before the limit is applied, so a request for 2 still gets 2 visible rows.
func TestComputeNewNodes_HiddenRowsDoNotShortenLimit(t *testing.T) {
	srv, _ := setupTestServer(t)
	ensureInactiveNodesTable(t, srv)
	srv.cfg.SetHiddenNamePrefixes([]string{"🚫"})
	srv.cfg.SetNodeBlacklist([]string{"limitblack0000001"})

	now := time.Now().UTC()
	insertNewNodeRow(t, srv, "limithidden000001", "🚫 Newest", "repeater", nil, nil, now.Add(-1*time.Second).Format(time.RFC3339))
	insertNewNodeRow(t, srv, "limitblack0000001", "Black", "repeater", nil, nil, now.Add(-2*time.Second).Format(time.RFC3339))
	insertNewNodeRow(t, srv, "limitvisible00001", "A", "repeater", nil, nil, now.Add(-3*time.Second).Format(time.RFC3339))
	insertNewNodeRow(t, srv, "limitvisible00002", "B", "repeater", nil, nil, now.Add(-4*time.Second).Format(time.RFC3339))

	entries, err := srv.computeNewNodes(2)
	if err != nil {
		t.Fatalf("computeNewNodes: %v", err)
	}
	if len(entries) != 2 || entries[0].PublicKey != "limitvisible00001" || entries[1].PublicKey != "limitvisible00002" {
		t.Errorf("want the two visible nodes, newest first; got %+v", entries)
	}
}

// TestComputeNetworkDigest_HiddenPrefixNotCounted: the digest's new-node count
// and its area breakdown leave a hidden-prefix node out too.
func TestComputeNetworkDigest_HiddenPrefixNotCounted(t *testing.T) {
	srv, _ := setupTestServer(t)
	ensureInactiveNodesTable(t, srv)
	ensureNodeChangesTable(t, srv)
	srv.cfg.SetHiddenNamePrefixes([]string{"🚫"})
	srv.cfg.Areas = map[string]AreaEntry{
		"AREAA": {Label: "Area A", LatMin: f64(55.9), LatMax: f64(56.2), LonMin: f64(9.9), LonMax: f64(10.2)},
	}

	now := time.Now().UTC()
	insertNewNodeRow(t, srv, "digesthidden00001", "🚫 Private", "repeater", f64(56.0), f64(10.0), now.Add(-1*time.Hour).Format(time.RFC3339))
	insertNewNodeRow(t, srv, "digestvisible0001", "Public", "repeater", f64(56.0), f64(10.0), now.Add(-2*time.Hour).Format(time.RFC3339))

	digest, err := srv.computeNetworkDigest(now.Add(-7*24*time.Hour), "all")
	if err != nil {
		t.Fatalf("computeNetworkDigest: %v", err)
	}
	if digest.NewNodes != 1 {
		t.Errorf("NewNodes = %d, want 1 (the hidden-prefix node is not counted)", digest.NewNodes)
	}
	if len(digest.AreaBreakdown) != 1 || digest.AreaBreakdown[0].Count != 1 ||
		len(digest.AreaBreakdown[0].Nodes) != 1 || digest.AreaBreakdown[0].Nodes[0].PublicKey != "digestvisible0001" {
		t.Errorf("AreaBreakdown = %+v, want Area A with only the visible node", digest.AreaBreakdown)
	}
}

// TestNewNodesEndpoints_FollowLiveHiddenChanges: no result is served stale
// after a hidden-prefix or blacklist change. Both endpoints are requested
// before and after each change, through the router.
func TestNewNodesEndpoints_FollowLiveHiddenChanges(t *testing.T) {
	srv, router := setupTestServer(t)
	ensureInactiveNodesTable(t, srv)
	ensureNodeChangesTable(t, srv)

	now := time.Now().UTC()
	insertNewNodeRow(t, srv, "livechange0000001", "~Secret", "repeater", nil, nil, now.Add(-1*time.Minute).Format(time.RFC3339))

	listed := func() bool {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/new-nodes?limit=50", nil))
		if w.Code != 200 {
			t.Fatalf("new-nodes status %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			NewNodes []NewNodeEntry `json:"newNodes"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode new-nodes: %v", err)
		}
		return newNodeKeys(resp.NewNodes)["livechange0000001"]
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
		return d.NewNodes
	}
	step := func(label string, wantListed bool) {
		t.Helper()
		if got := listed(); got != wantListed {
			t.Errorf("%s: new-nodes lists the node = %v, want %v", label, got, wantListed)
		}
		want := 0
		if wantListed {
			want = 1
		}
		if got := counted(); got != want {
			t.Errorf("%s: network-digest newNodes = %d, want %d", label, got, want)
		}
	}

	step("before any change", true)
	srv.cfg.SetHiddenNamePrefixes([]string{"~"})
	step("after adding the prefix", false)
	srv.cfg.SetHiddenNamePrefixes(nil)
	step("after removing the prefix", true)
	srv.cfg.SetNodeBlacklist([]string{"LIVECHANGE0000001"})
	step("after blacklisting", false)
	srv.cfg.SetNodeBlacklist(nil)
	step("after un-blacklisting", true)
}

// TestComputeNetworkDigest_BlacklistedNotCounted is the regression guard for
// what already worked: a blacklisted node (listed in any case) is neither
// listed by New Nodes nor counted by the digest.
func TestComputeNetworkDigest_BlacklistedNotCounted(t *testing.T) {
	srv, _ := setupTestServer(t)
	ensureInactiveNodesTable(t, srv)
	ensureNodeChangesTable(t, srv)
	srv.cfg.SetNodeBlacklist([]string{"  DIGESTBLACK00001 "})

	now := time.Now().UTC()
	insertNewNodeRow(t, srv, "digestblack00001", "Black", "repeater", nil, nil, now.Add(-1*time.Hour).Format(time.RFC3339))
	insertNewNodeRow(t, srv, "digestwhite00001", "White", "repeater", nil, nil, now.Add(-2*time.Hour).Format(time.RFC3339))

	entries, err := srv.computeNewNodes(50)
	if err != nil {
		t.Fatalf("computeNewNodes: %v", err)
	}
	if keys := newNodeKeys(entries); keys["digestblack00001"] || !keys["digestwhite00001"] {
		t.Errorf("want only the non-blacklisted node, got %+v", entries)
	}
	digest, err := srv.computeNetworkDigest(now.Add(-7*24*time.Hour), "all")
	if err != nil {
		t.Fatalf("computeNetworkDigest: %v", err)
	}
	if digest.NewNodes != 1 {
		t.Errorf("NewNodes = %d, want 1 (the blacklisted node is not counted)", digest.NewNodes)
	}
}
