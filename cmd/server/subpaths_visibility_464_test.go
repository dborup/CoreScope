package main

// Follow-ups to #453 (Route Patterns identity visibility), issue #464:
//   - R2: how masked list rows merge, and how that differs from an unknown hop
//   - R3: the detail honours the store's cached (shown) name of a node that
//     has left the node list
//   - R4: rows that become identical once masked are merged by the handlers

import (
	"fmt"
	"strings"
	"testing"
)

const rpNamedLikeHop = "b2c4000000000011" // a visible node whose name is "bb"

// R3: a node deleted from `nodes` while the store's node cache still carries
// it under a hidden-prefixed name. The live lookup finds nothing for it, so
// only the cached name can hide it: as a parent-path hop and as a requested
// hop.
func TestRoutePatterns464_DetailCachedNameOfDeletedNode(t *testing.T) {
	cfg := &Config{Port: 3000}
	srv, router := setupRoutePatternsServer(t, "visible", cfg)

	parents := func(url string) string {
		t.Helper()
		return rpDetailText(t, router, url)
	}
	if base := parents(rpDetail("aa", "dd")); !strings.Contains(base, "Alpha → Dup → Hidden-Bravo") {
		t.Fatalf("baseline detail lacks the parent path through Hidden-Bravo: %s", base)
	}
	if got := rpDetailNode(t, rpGet(t, router, rpDetail("aa", "bb"), 200), 1); got["name"] != "Hidden-Bravo" {
		t.Fatalf("baseline requested hop = %v, want Hidden-Bravo", got)
	}
	if _, err := srv.db.conn.Exec(`DELETE FROM nodes WHERE public_key = ?`, rpHiddenB); err != nil {
		t.Fatal(err)
	}
	cfg.SetHiddenNamePrefixes([]string{"Hidden-"})

	body := parents(rpDetail("aa", "dd"))
	if strings.Contains(body, "Hidden-Bravo") {
		t.Errorf("parent path shows the cached hidden name of a deleted node: %s", body)
	}
	if !strings.Contains(body, "Alpha → Dup → bb") {
		t.Errorf("parent path through the deleted hidden node is not shown as its raw hop: %s", body)
	}
	rpRequireUnresolved(t, rpDetailNode(t, rpGet(t, router, rpDetail("aa", "bb"), 200), 1), "bb")
}

// R4: two list rows that become identical once masked are one row: counts
// summed, pct recomputed over all paths, the smallest raw key, in the list
// and in every bulk group. "Alpha → bb" is a visible node NAMED "bb" (raw hop
// b2); masking Hidden-Bravo (raw hop bb) turns "Alpha → Hidden-Bravo" into the
// same text.
func TestRoutePatterns464_HandlersMergeRowsIdenticalOnceMasked(t *testing.T) {
	db := seedRoutePatternsDB(t, "visible")
	if _, err := db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count) VALUES (?, 'bb', 'repeater', 37.5, -122.0, ?, '2026-01-01T00:00:00Z', 5)`,
		rpNamedLikeHop, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	rpAddTraffic(t, db, 1, `["aa","b2"]`)
	cfg := &Config{Port: 3000}
	_, router := newRoutePatternsServer(t, db, cfg)

	const listURL = "/api/analytics/subpaths?minLen=2&maxLen=2&limit=100"
	const bulkURL = "/api/analytics/subpaths-bulk?groups=2-2:100"
	rowsOf := func(group map[string]interface{}) []map[string]interface{} {
		t.Helper()
		var out []map[string]interface{}
		for _, r := range group["subpaths"].([]interface{}) {
			out = append(out, r.(map[string]interface{}))
		}
		return out
	}
	named := func(rows []map[string]interface{}, path string) []map[string]interface{} {
		var out []map[string]interface{}
		for _, r := range rows {
			if r["path"] == path {
				out = append(out, r)
			}
		}
		return out
	}
	groups := func() map[string]map[string]interface{} {
		return map[string]map[string]interface{}{
			"list": rpGet(t, router, listURL, 200),
			"bulk": rpGet(t, router, bulkURL, 200)["results"].([]interface{})[0].(map[string]interface{}),
		}
	}

	for name, g := range groups() { // baseline: two separate rows, one packet each
		rows := rowsOf(g)
		if len(named(rows, "Alpha → bb")) != 1 || len(named(rows, "Alpha → Hidden-Bravo")) != 1 {
			t.Fatalf("%s baseline lacks the two unmasked rows: %v", name, g["subpaths"])
		}
	}

	cfg.SetHiddenNamePrefixes([]string{"Hidden-"})
	for name, g := range groups() {
		rows := rowsOf(g)
		if rpAnyContains(rpPaths(t, g), "Hidden-") {
			t.Errorf("%s shows a hidden name: %v", name, g["subpaths"])
		}
		merged := named(rows, "Alpha → bb")
		if len(merged) != 1 {
			t.Fatalf("%s: %d rows show %q, want 1 merged row: %v", name, len(merged), "Alpha → bb", g["subpaths"])
		}
		m := merged[0]
		total := int(g["totalPaths"].(float64))
		if int(m["count"].(float64)) != 2 {
			t.Errorf("%s merged count = %v, want 2", name, m["count"])
		}
		if want := subpathPct(2, total); m["pct"].(float64) != want {
			t.Errorf("%s merged pct = %v, want %v", name, m["pct"], want)
		}
		if got := fmt.Sprint(m["rawHops"]); got != "[aa b2]" {
			t.Errorf("%s merged rawHops = %s, want the smallest raw key [aa b2]", name, got)
		}
	}
}

// R2: the documented difference. A hidden node seen under several raw keys of
// one hop pair is one row in the store (merged by display name) and stays one
// masked row (smallest raw key, counts summed); an unknown hop with the same
// traffic is one row per raw key.
func TestRoutePatterns464_HiddenNodeSeveralRawKeysVsUnknownHop(t *testing.T) {
	db := seedRoutePatternsDB(t, "visible")
	rpAddTraffic(t, db, 1, `["aa","bb22"]`, `["aa","f0"]`, `["aa","f0f1"]`)
	cfg := &Config{Port: 3000}
	_, router := newRoutePatternsServer(t, db, cfg)
	const listURL = "/api/analytics/subpaths?minLen=2&maxLen=2&limit=100"
	rows := func() map[string]map[string]interface{} {
		t.Helper()
		out := map[string]map[string]interface{}{}
		for _, r := range rpGet(t, router, listURL, 200)["subpaths"].([]interface{}) {
			m := r.(map[string]interface{})
			out[m["path"].(string)] = m
		}
		return out
	}
	base := rows()
	if r := base["Alpha → Hidden-Bravo"]; r == nil || r["count"].(float64) != 2 {
		t.Fatalf("baseline: Hidden-Bravo under two raw keys is not one row of count 2: %v", base)
	}
	cfg.SetHiddenNamePrefixes([]string{"Hidden-"})
	got := rows()
	if r := got["Alpha → bb"]; r == nil || r["count"].(float64) != 2 || fmt.Sprint(r["rawHops"]) != "[aa bb]" {
		t.Errorf("hidden node under two raw keys: want one row %q, count 2, rawHops [aa bb]; got %v", "Alpha → bb", got)
	}
	for _, p := range []string{"Alpha → f0", "Alpha → f0f1"} {
		if r := got[p]; r == nil || r["count"].(float64) != 1 {
			t.Errorf("unknown hop: want its own row %q of count 1; got %v", p, got)
		}
	}
}
