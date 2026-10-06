package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gorilla/mux"
)

// #286: observer ids arrive raw from the MQTT topic, so one pubkey can hold
// several observers rows that differ only in letter case. The node-404 body
// must weigh *every* one of those aliases against the hidden-name rule, not
// just the newest row it happens to return. The newest alias below carries a
// visible name on purpose: a check that only looks at it misses the hidden
// older alias and leaks name, role and timestamps.

const (
	issue286AliasKey   = "b286000000000000000000000000000000000000000000000000000000000001"
	issue286NewestName = "Loud Relay"
	issue286HiddenName = "Quiet Alias"
	issue286NewestSeen = "2026-10-05T10:00:00Z"
	issue286OlderSeen  = "2026-09-20T08:00:00Z"
)

// issue286SeedAliases inserts the two case-variant observer rows: the newest
// (upper-case id) is named visibly, the older (lower-case id) hidden.
func issue286SeedAliases(t *testing.T, srv *Server) {
	t.Helper()
	rows := []struct{ id, name, lastSeen string }{
		{strings.ToUpper(issue286AliasKey), issue286NewestName, issue286NewestSeen},
		{issue286AliasKey, issue286HiddenName, issue286OlderSeen},
	}
	for _, r := range rows {
		if _, err := srv.db.conn.Exec(`INSERT INTO observers (id, name, last_seen, first_seen) VALUES (?, ?, ?, '2026-09-01T00:00:00Z')`,
			r.id, r.name, r.lastSeen); err != nil {
			t.Fatalf("seed observer %q: %v", r.id, err)
		}
	}
}

// issue286Body returns the raw 404 body as well as its decoded fields, so a
// leak can be asserted on the bytes and not only on the named fields.
func issue286Body(t *testing.T, router *mux.Router, pubkey string) (int, string, map[string]json.RawMessage) {
	t.Helper()
	code, body := issue199Get(t, router, pubkey)
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("re-encode body: %v", err)
	}
	return code, string(raw), body
}

// A hidden name on an older case-variant alias hides the whole identity.
func TestNodeDetail404HiddenOlderObserverAliasStaysBare(t *testing.T) {
	for _, tc := range []struct {
		name        string
		withNodeRow bool
	}{
		{"observer only", false},
		{"with inactive node row", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, router := setupTestServer(t)
			if tc.withNodeRow {
				ensureInactiveNodesTable(t, srv)
				if _, err := srv.db.conn.Exec(`INSERT INTO inactive_nodes (public_key, name, role, last_seen, first_seen) VALUES (?, 'Retired Repeater', 'repeater', '2026-09-18T00:00:00Z', '2026-08-01T00:00:00Z')`, issue286AliasKey); err != nil {
					t.Fatal(err)
				}
			}
			issue286SeedAliases(t, srv)
			srv.cfg.SetHiddenNamePrefixes([]string{"Quiet"})

			for _, key := range []string{issue286AliasKey, strings.ToUpper(issue286AliasKey)} {
				code, raw, body := issue286Body(t, router, key)
				if code != 404 || len(body) != 1 || body["error"] == nil {
					t.Fatalf("GET %s: status=%d body=%s, want a bare 404", key, code, raw)
				}
				for _, leak := range []string{issue286NewestName, issue286HiddenName, "Retired Repeater", "repeater", issue286NewestSeen, issue286OlderSeen} {
					if strings.Contains(raw, leak) {
						t.Errorf("GET %s: 404 body leaks %q: %s", key, leak, raw)
					}
				}
			}
		})
	}
}

// Control for the test above, and the row-selection rule: with no prefix
// configured the newest alias is the one returned, exactly once. This pins
// that the newest name is *visible*, so hiding above can only come from the
// older alias.
func TestNodeDetail404ObserverAliasRowIsTheNewest(t *testing.T) {
	srv, router := setupTestServer(t)
	issue286SeedAliases(t, srv)

	code, raw, body := issue286Body(t, router, issue286AliasKey)
	if code != 404 {
		t.Fatalf("status=%d, want 404", code)
	}
	var obs issue199Observer
	if err := json.Unmarshal(body["observer"], &obs); err != nil {
		t.Fatalf("observer missing or malformed: %v (body %s)", err, raw)
	}
	if obs.ID != strings.ToUpper(issue286AliasKey) || obs.Name != issue286NewestName || obs.LastSeen != issue286NewestSeen {
		t.Errorf("observer=%+v, want the newest alias %q/%q", obs, issue286NewestName, issue286NewestSeen)
	}
	if strings.Contains(raw, issue286HiddenName) {
		t.Errorf("the older alias leaked into the body: %s", raw)
	}
}

// The visibility rule covers every alias regardless of which one is newest:
// a hidden name on the *newest* alias hides the identity just as well.
func TestNodeDetail404HiddenNewestObserverAliasStaysBare(t *testing.T) {
	srv, router := setupTestServer(t)
	issue286SeedAliases(t, srv)
	srv.cfg.SetHiddenNamePrefixes([]string{"Loud"})

	code, raw, body := issue286Body(t, router, issue286AliasKey)
	if code != 404 || len(body) != 1 || body["error"] == nil {
		t.Fatalf("status=%d body=%s, want a bare 404", code, raw)
	}
}
