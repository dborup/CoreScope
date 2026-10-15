package main

// Route Patterns (/api/analytics/subpaths, /subpaths-bulk, /subpath-detail)
// must apply the same identity visibility as the rest of the analytics
// surface (#410): node blacklist, observer blacklist and hidden name
// prefixes (#1181), including observer aliases.
//
// The tests drive the real handlers over data produced by the real store
// (list rows are []map[string]interface{} with an int "hops" count, not the
// []interface{} / []interface{} shape the old filter asserted). Every test
// first proves the hidden target IS in the unfiltered response, so "absent
// after filtering" cannot pass vacuously.

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

const (
	rpAlpha   = "aa11000000000001"
	rpHiddenB = "bb22000000000002" // named Hidden-Bravo
	rpCharlie = "cc33000000000003"
	rpDup1    = "dd44000000000004" // two visible nodes sharing a name
	rpDup2    = "d2d2d2d200000010"
	rpEve     = "ee55000000000005"
	rpAmbVis  = "11a1000000000007" // 11.. is an ambiguous 1-byte prefix
	rpAmbHid  = "11b1000000000008"
	rpComp    = "99aa000000000009" // companion: never a path hop candidate
)

// seedRoutePatternsDB builds repeaters (and one companion) plus one
// transmission per tested path. ambWinner picks which of the two nodes
// sharing the "11" prefix the resolver prefers (higher advert count).
func seedRoutePatternsDB(t *testing.T, ambWinner string) *DB {
	t.Helper()
	db := setupTestDB(t)
	now := time.Now().UTC()
	recent := now.Add(-1 * time.Hour).Format(time.RFC3339)
	epoch := now.Add(-1 * time.Hour).Unix()
	mustExec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := db.conn.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	mustExec(`INSERT INTO observers (id, name, iata, last_seen, first_seen, packet_count) VALUES ('obs1','Observer One','SJC',?,'2026-01-01T00:00:00Z',10)`, recent)
	mustExec(`INSERT INTO observers (id, name, iata, last_seen, first_seen, packet_count) VALUES ('obs2','Hidden-Observer','SFO',?,'2026-01-01T00:00:00Z',10)`, recent)
	visCount, hidCount := 5, 500
	if ambWinner == "visible" {
		visCount, hidCount = 500, 5
	}
	for _, n := range []struct {
		pk, name, role string
		adverts        int
	}{
		{rpAlpha, "Alpha", "repeater", 5},
		{rpHiddenB, "Hidden-Bravo", "repeater", 5},
		{rpCharlie, "Charlie", "repeater", 5},
		{rpDup1, "Dup", "repeater", 5},
		{rpDup2, "Dup", "repeater", 5},
		{rpEve, "Eve", "repeater", 5},
		{rpAmbVis, "Visible-One", "repeater", visCount},
		{rpAmbHid, "Hidden-Two", "repeater", hidCount},
		{rpComp, "Hidden-Comp", "companion", 5},
	} {
		// The ambiguous pair has no GPS so the resolver falls to its
		// advert-count tie-break instead of an arbitrary geo pick.
		lat, lon := 37.5, -122.0
		if strings.HasPrefix(n.pk, "11") {
			lat, lon = 0, 0
		}
		mustExec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count) VALUES (?, ?, ?, ?, ?, ?, '2026-01-01T00:00:00Z', ?)`,
			n.pk, n.name, n.role, lat, lon, recent, n.adverts)
	}
	paths := []struct {
		path string
		obs  int
	}{
		{`["aa","bb"]`, 1},
		{`["aa","cc"]`, 1},
		{`["aa","dd"]`, 1},
		{`["aa","d2"]`, 1},
		{`["aa","ee"]`, 1},
		{`["aa","11"]`, 1},
		{`["aa","99"]`, 1},
		{`["aa","dd","bb"]`, 2}, // observed by the Hidden-Observer
	}
	for i, p := range paths {
		mustExec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json) VALUES ('AABB', ?, ?, 1, 5, '{"type":"CHAN"}')`, fmt.Sprintf("rp410-%d", i), recent)
		mustExec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp) VALUES (?, ?, 10, -90, ?, ?)`, i+1, p.obs, p.path, epoch)
	}
	return db
}

func setupRoutePatternsServer(t *testing.T, ambWinner string, cfg *Config) (*Server, *mux.Router) {
	t.Helper()
	db := seedRoutePatternsDB(t, ambWinner)
	srv := NewServer(db, cfg, NewHub())
	store := NewPacketStore(db, nil)
	if err := store.Load(); err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if !store.WaitIndexesReady(5 * time.Second) {
		t.Fatalf("indexes never became ready")
	}
	store.config = cfg
	srv.store = store
	router := mux.NewRouter()
	srv.RegisterRoutes(router)
	return srv, router
}

func rpGet(t *testing.T, router *mux.Router, url string, want int) map[string]interface{} {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", url, nil))
	if w.Code != want {
		t.Fatalf("GET %s: HTTP %d (want %d): %s", url, w.Code, want, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET %s: not JSON: %v", url, err)
	}
	return body
}

// rpPaths returns the "path" of every list row (list or one bulk group).
func rpPaths(t *testing.T, group map[string]interface{}) map[string]bool {
	t.Helper()
	rows, ok := group["subpaths"].([]interface{})
	if !ok {
		if group["subpaths"] == nil {
			return map[string]bool{}
		}
		t.Fatalf("subpaths is %T, want a list", group["subpaths"])
	}
	out := map[string]bool{}
	for _, r := range rows {
		m := r.(map[string]interface{})
		if _, ok := m["hops"].(float64); !ok {
			t.Fatalf("row hops is %T, want a number: %v", m["hops"], m)
		}
		out[m["path"].(string)] = true
	}
	return out
}

func rpList(t *testing.T, router *mux.Router) map[string]bool {
	return rpPaths(t, rpGet(t, router, "/api/analytics/subpaths?minLen=2&maxLen=8&limit=100", 200))
}

func rpBulk(t *testing.T, router *mux.Router) map[string]bool {
	t.Helper()
	body := rpGet(t, router, "/api/analytics/subpaths-bulk?groups=2-2:50,3-3:50", 200)
	results := body["results"].([]interface{})
	out := map[string]bool{}
	for _, g := range results {
		for p := range rpPaths(t, g.(map[string]interface{})) {
			out[p] = true
		}
	}
	return out
}

func rpAnyContains(paths map[string]bool, sub string) bool {
	for p := range paths {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}

func rpRequire(t *testing.T, what string, paths map[string]bool, present ...string) {
	t.Helper()
	for _, p := range present {
		if !paths[p] {
			t.Errorf("%s: expected row %q, got %v", what, p, paths)
		}
	}
}

// Hidden-name-only configuration (no node blacklist): the old handlers ran
// the filter only when NodeBlacklist was non-empty, so this leaked.
func TestRoutePatterns410_HiddenNameOnly(t *testing.T) {
	cfg := &Config{Port: 3000}
	_, router := setupRoutePatternsServer(t, "visible", cfg)
	for name, get := range map[string]func(*testing.T, *mux.Router) map[string]bool{"list": rpList, "bulk": rpBulk} {
		if base := get(t, router); !rpAnyContains(base, "Hidden-Bravo") {
			t.Fatalf("%s baseline lacks the target: %v", name, base)
		}
	}
	cfg.SetHiddenNamePrefixes([]string{"Hidden-"})
	for name, get := range map[string]func(*testing.T, *mux.Router) map[string]bool{"list": rpList, "bulk": rpBulk} {
		got := get(t, router)
		if rpAnyContains(got, "Hidden-") {
			t.Errorf("%s leaks a hidden name: %v", name, got)
		}
		// Duplicate visible names, an unresolved companion hop and the
		// ambiguous prefix whose resolver pick is visible all stay.
		rpRequire(t, name, got, "Alpha → Dup", "Alpha → Eve", "Alpha → Charlie", "Alpha → Visible-One", "Alpha → 99")
	}
}

// Blacklist-only configuration, including a blacklisted node that is not a
// path-hop candidate (companion): an unresolved raw hop is not an identity.
func TestRoutePatterns410_BlacklistOnly(t *testing.T) {
	cfg := &Config{Port: 3000}
	_, router := setupRoutePatternsServer(t, "visible", cfg)
	cfg.SetNodeBlacklist([]string{rpCharlie, rpComp, rpAmbHid})
	for name, get := range map[string]func(*testing.T, *mux.Router) map[string]bool{"list": rpList, "bulk": rpBulk} {
		got := get(t, router)
		if rpAnyContains(got, "Charlie") {
			t.Errorf("%s leaks a blacklisted name: %v", name, got)
		}
		// Names hidden only by prefix stay visible without a prefix config.
		rpRequire(t, name, got, "Alpha → Hidden-Bravo", "Alpha → Dup", "Alpha → Visible-One", "Alpha → 99", "Alpha → Eve")
	}
}

// A short hop prefix must not be matched as an identity: the blacklist holds
// the full key of the hidden ambiguous node, the resolver picks the VISIBLE
// one, so the row stays; when the resolver picks the hidden node it goes.
func TestRoutePatterns410_AmbiguousPrefix(t *testing.T) {
	for _, winner := range []string{"visible", "hidden"} {
		t.Run(winner, func(t *testing.T) {
			cfg := &Config{Port: 3000}
			_, router := setupRoutePatternsServer(t, winner, cfg)
			base := rpList(t, router)
			shown := "Alpha → Visible-One"
			if winner == "hidden" {
				shown = "Alpha → Hidden-Two"
			}
			rpRequire(t, "baseline", base, shown)
			cfg.SetNodeBlacklist([]string{rpAmbHid})
			got := rpList(t, router)
			if winner == "visible" {
				rpRequire(t, "visible winner", got, shown)
			} else if got[shown] || rpAnyContains(got, "Hidden-Two") {
				t.Errorf("hidden winner leaks: %v", got)
			}
		})
	}
}

// Observer aliases: an observer whose id is a node's pubkey and whose name is
// hidden hides that identity (identityHidden policy), and observer blacklist
// entries hide it too.
func TestRoutePatterns410_ObserverAlias(t *testing.T) {
	cfg := &Config{Port: 3000}
	srv, router := setupRoutePatternsServer(t, "visible", cfg)
	if _, err := srv.db.conn.Exec(`INSERT INTO observers (id, name, iata, last_seen, first_seen, packet_count) VALUES (?, 'Hidden-Alias', 'SJC', ?, '2026-01-01T00:00:00Z', 1)`,
		rpEve, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if base := rpList(t, router); !base["Alpha → Eve"] {
		t.Fatalf("baseline lacks Eve: %v", base)
	}
	cfg.SetHiddenNamePrefixes([]string{"Hidden-"})
	if got := rpList(t, router); got["Alpha → Eve"] {
		t.Errorf("observer alias did not hide Eve: %v", got)
	}
	cfg.SetHiddenNamePrefixes(nil)
	cfg.ObserverBlacklist = []string{rpEve}
	if got := rpList(t, router); got["Alpha → Eve"] {
		t.Errorf("observer blacklist did not hide Eve: %v", got)
	}
}

// Cached and plain requests must not contaminate each other: the first
// request fills the store cache under one visibility context, later ones run
// under another.
func TestRoutePatterns410_CacheOrder(t *testing.T) {
	cfg := &Config{Port: 3000}
	srv, router := setupRoutePatternsServer(t, "visible", cfg)
	// plain first (fills the cache) ...
	if !rpList(t, router)["Alpha → Charlie"] {
		t.Fatal("baseline lacks Charlie")
	}
	// ... then restricted over the cached entry ...
	cfg.SetNodeBlacklist([]string{rpCharlie})
	if rpAnyContains(rpList(t, router), "Charlie") {
		t.Error("cached entry leaked after blacklisting")
	}
	if rpAnyContains(rpBulk(t, router), "Charlie") {
		t.Error("bulk leaked after blacklisting")
	}
	// ... the shared cached response itself must be untouched ...
	cached := srv.store.GetAnalyticsSubpaths("", 2, 8, 100)
	found := false
	for _, r := range cached["subpaths"].([]map[string]interface{}) {
		if r["path"] == "Alpha → Charlie" {
			found = true
		}
	}
	if !found {
		t.Error("filtering mutated the cached shared response")
	}
	// ... and lifting the restriction restores the row from the same cache.
	cfg.SetNodeBlacklist(nil)
	if !rpList(t, router)["Alpha → Charlie"] {
		t.Error("lifting the blacklist did not restore the cached row")
	}
	// restricted first, then plain, on a fresh cache (bulk fills "|2|2|50").
	cfg.SetNodeBlacklist([]string{rpCharlie})
	srv.store.cacheMu.Lock()
	srv.store.subpathCache = make(map[string]*cachedResult)
	srv.store.cacheMu.Unlock()
	if rpAnyContains(rpBulk(t, router), "Charlie") {
		t.Error("bulk leaked on a cold cache")
	}
	cfg.SetNodeBlacklist(nil)
	if !rpBulk(t, router)["Alpha → Charlie"] {
		t.Error("plain bulk after a restricted one lost the row")
	}
}

// totalPaths and per-row count/pct keep their documented meaning: they are
// computed over all traffic, hidden or not, and are not rewritten.
func TestRoutePatterns410_MetricsUnchanged(t *testing.T) {
	cfg := &Config{Port: 3000}
	_, router := setupRoutePatternsServer(t, "visible", cfg)
	before := rpGet(t, router, "/api/analytics/subpaths?minLen=2&maxLen=2&limit=100", 200)
	cfg.SetNodeBlacklist([]string{rpCharlie})
	after := rpGet(t, router, "/api/analytics/subpaths?minLen=2&maxLen=2&limit=100", 200)
	if before["totalPaths"] != after["totalPaths"] {
		t.Errorf("totalPaths changed: %v -> %v", before["totalPaths"], after["totalPaths"])
	}
	pct := func(b map[string]interface{}, path string) interface{} {
		for _, r := range b["subpaths"].([]interface{}) {
			if m := r.(map[string]interface{}); m["path"] == path {
				return m["pct"]
			}
		}
		return nil
	}
	if pct(before, "Alpha → Eve") == nil || pct(before, "Alpha → Eve") != pct(after, "Alpha → Eve") {
		t.Errorf("pct of a visible row changed: %v -> %v", pct(before, "Alpha → Eve"), pct(after, "Alpha → Eve"))
	}
}

func rpDetail(hops ...string) string {
	return "/api/analytics/subpath-detail?hops=" + strings.Join(hops, ",")
}

func rpDetailText(t *testing.T, router *mux.Router, url string) string {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", url, nil))
	if w.Code != 200 {
		t.Fatalf("GET %s: HTTP %d", url, w.Code)
	}
	return w.Body.String()
}

// Detail: a requested hop that RESOLVES to a hidden identity is a 404, by
// hidden name or by blacklist; a short prefix is never compared with a key.
func TestRoutePatterns410_DetailHiddenHop(t *testing.T) {
	cfg := &Config{Port: 3000}
	_, router := setupRoutePatternsServer(t, "visible", cfg)
	rpGet(t, router, rpDetail("aa", "bb"), 200) // baseline
	rpGet(t, router, rpDetail("aa", "cc"), 200)

	cfg.SetHiddenNamePrefixes([]string{"Hidden-"})
	rpGet(t, router, rpDetail("aa", "bb"), 404)
	rpGet(t, router, rpDetail("aa", "cc"), 200) // blacklist not set

	cfg.SetHiddenNamePrefixes(nil)
	cfg.SetNodeBlacklist([]string{rpCharlie})
	rpGet(t, router, rpDetail("aa", "cc"), 404)
	rpGet(t, router, rpDetail("aa", "bb"), 200) // hidden-name not set

	// Ambiguous prefix: only the resolved node counts.
	cfg.SetNodeBlacklist([]string{rpAmbHid})
	body := rpGet(t, router, rpDetail("aa", "11"), 200)
	if strings.Contains(fmt.Sprint(body), "Hidden-Two") {
		t.Errorf("detail names the hidden candidate: %v", body)
	}

	// Duplicate visible names: a sibling's blacklist entry does not hide it.
	rpGet(t, router, rpDetail("aa", "dd"), 200)
	rpGet(t, router, rpDetail("aa", "d2"), 200)
}

// Detail of a visible subpath must not surface a hidden node through the
// parent paths, nor a hidden observer through the observer list.
func TestRoutePatterns410_DetailParentPathsAndObservers(t *testing.T) {
	cfg := &Config{Port: 3000}
	_, router := setupRoutePatternsServer(t, "visible", cfg)
	base := rpDetailText(t, router, rpDetail("aa", "dd"))
	for _, want := range []string{"Hidden-Bravo", "Hidden-Observer"} {
		if !strings.Contains(base, want) {
			t.Fatalf("baseline lacks %q: %s", want, base)
		}
	}
	cfg.SetHiddenNamePrefixes([]string{"Hidden-"})
	got := rpDetailText(t, router, rpDetail("aa", "dd"))
	if strings.Contains(got, "Hidden-") {
		t.Errorf("detail leaks a hidden name: %s", got)
	}

	cfg.SetHiddenNamePrefixes(nil)
	cfg.SetNodeBlacklist([]string{rpHiddenB})
	if got := rpDetailText(t, router, rpDetail("aa", "dd")); strings.Contains(got, "Hidden-Bravo") {
		t.Errorf("detail leaks a blacklisted parent-path node: %s", got)
	}

	cfg.SetNodeBlacklist(nil)
	cfg.ObserverBlacklist = []string{"obs2"}
	if got := rpDetailText(t, router, rpDetail("aa", "dd")); strings.Contains(got, "Hidden-Observer") {
		t.Errorf("detail leaks a blacklisted observer: %s", got)
	}
}

// With nothing hidden the list is returned as the store produced it.
func TestRoutePatterns410_NoRestrictionPassThrough(t *testing.T) {
	cfg := &Config{Port: 3000}
	srv, router := setupRoutePatternsServer(t, "visible", cfg)
	got := rpList(t, router)
	cached := srv.store.GetAnalyticsSubpaths("", 2, 8, 100)
	if len(got) != len(cached["subpaths"].([]map[string]interface{})) {
		t.Errorf("pass-through changed the row count: %d vs %d", len(got), len(cached["subpaths"].([]map[string]interface{})))
	}
}

// Two raw keys that resolve to one display name merge into one row; the
// reported rawHops must not depend on map iteration order.
func TestSubpathAccumMergeIsDeterministic(t *testing.T) {
	for _, order := range [][]string{{"dd,ee", "d2,ee"}, {"d2,ee", "dd,ee"}} {
		a := &subpathAccum{}
		for _, raw := range order {
			a.add(raw, 3)
		}
		if a.raw != "d2,ee" || a.count != 6 {
			t.Errorf("order %v: raw=%q count=%d, want d2,ee/6", order, a.raw, a.count)
		}
	}
}
