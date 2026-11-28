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
	"reflect"
	"sort"
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

// rpRefTime is the one reference time every seed helper derives its
// timestamps from. Reading the clock per seed let two calls straddle a second
// boundary and split the firstSeen of otherwise identical rows (#483).
var rpRefTime = time.Now().UTC().Truncate(time.Second)

// seedRoutePatternsDB builds repeaters (and one companion) plus one
// transmission per tested path. ambWinner picks which of the two nodes
// sharing the "11" prefix the resolver prefers (higher advert count).
func seedRoutePatternsDB(t testing.TB, ambWinner string) *DB {
	t.Helper()
	db := setupTestDB(t)
	now := rpRefTime
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
	// An observer whose id is a node key: it shares that node's identity.
	mustExec(`INSERT INTO observers (id, name, iata, last_seen, first_seen, packet_count) VALUES (?,'Gateway-Three','SJC',?,'2026-01-01T00:00:00Z',10)`, rpHiddenB, recent)
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
		{`["aa","dd"]`, 3},      // observed by Gateway-Three
	}
	for i, p := range paths {
		mustExec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json) VALUES ('AABB', ?, ?, 1, 5, '{"type":"CHAN"}')`, fmt.Sprintf("rp410-%d", i), recent)
		mustExec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp) VALUES (?, ?, 10, -90, ?, ?)`, i+1, p.obs, p.path, epoch)
	}
	return db
}

func setupRoutePatternsServer(t *testing.T, ambWinner string, cfg *Config) (*Server, *mux.Router) {
	t.Helper()
	return newRoutePatternsServer(t, seedRoutePatternsDB(t, ambWinner), cfg)
}

func newRoutePatternsServer(t testing.TB, db *DB, cfg *Config) (*Server, *mux.Router) {
	t.Helper()
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
			} else if got[shown] || rpAnyContains(got, "Hidden-Two") || !got["Alpha → 11"] {
				t.Errorf("hidden winner leaks or is not shown as its raw hop: %v", got)
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
		rpEve, rpRefTime.Format(time.RFC3339)); err != nil {
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

// rpDetailNode returns nodes[i] of a detail response.
func rpDetailNode(t *testing.T, body map[string]interface{}, i int) map[string]interface{} {
	t.Helper()
	nodes, ok := body["nodes"].([]interface{})
	if !ok || len(nodes) <= i {
		t.Fatalf("detail has no nodes[%d]: %v", i, body)
	}
	return nodes[i].(map[string]interface{})
}

// rpRequireUnresolved asserts that nodes[i] reports the hop as matching no
// node: the raw hop as its name and no pubkey or position.
func rpRequireUnresolved(t *testing.T, node map[string]interface{}, hop string) {
	t.Helper()
	if node["hop"] != hop || node["name"] != hop || node["pubkey"] != nil || node["lat"] != nil || node["lon"] != nil {
		t.Errorf("hop %q is not reported as unresolved: %v", hop, node)
	}
}

// Detail: a requested hop that RESOLVES to a hidden identity is reported as
// an unresolved hop, by hidden name or by blacklist; a short prefix is never
// compared with a key.
func TestRoutePatterns410_DetailHiddenHop(t *testing.T) {
	cfg := &Config{Port: 3000}
	_, router := setupRoutePatternsServer(t, "visible", cfg)
	resolved := func(hop, name string) {
		t.Helper()
		if got := rpDetailNode(t, rpGet(t, router, rpDetail("aa", hop), 200), 1); got["name"] != name {
			t.Errorf("hop %q resolved to %v, want %s", hop, got["name"], name)
		}
	}
	unresolved := func(hop string) {
		t.Helper()
		rpRequireUnresolved(t, rpDetailNode(t, rpGet(t, router, rpDetail("aa", hop), 200), 1), hop)
	}
	resolved("bb", "Hidden-Bravo") // baseline
	resolved("cc", "Charlie")

	cfg.SetHiddenNamePrefixes([]string{"Hidden-"})
	unresolved("bb")
	resolved("cc", "Charlie") // blacklist not set

	cfg.SetHiddenNamePrefixes(nil)
	cfg.SetNodeBlacklist([]string{rpCharlie})
	unresolved("cc")
	resolved("bb", "Hidden-Bravo") // hidden-name not set

	// Ambiguous prefix: only the resolved node counts.
	cfg.SetNodeBlacklist([]string{rpAmbHid})
	body := rpGet(t, router, rpDetail("aa", "11"), 200)
	if strings.Contains(fmt.Sprint(body), "Hidden-Two") {
		t.Errorf("detail names the hidden candidate: %v", body)
	}
	resolved("11", "Visible-One")

	// Duplicate visible names: a sibling's blacklist entry does not hide it.
	resolved("dd", "Dup")
	resolved("d2", "Dup")
}

// rpDetailRaw returns the status and body of a detail request.
func rpDetailRaw(router *mux.Router, hops ...string) (int, string) {
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", rpDetail(hops...), nil))
	return w.Code, w.Body.String()
}

// A hidden hop and a hop that matches no node must be indistinguishable, for
// short prefixes of every length and for full keys, under every hiding rule:
// same status, the same nodes[i] apart from the echoed hop text and, where
// neither subpath has traffic, the same whole response.
func TestRoutePatterns410_DetailHiddenLooksUnknown(t *testing.T) {
	hidden := []string{"bb", "bb2", "bb22", "bb220000", rpHiddenB}
	unknown := []string{"f0", "f0f", "f0f0", "f0f0f0f0", "f0f0f0f0f0f0f0f0"}
	rules := []struct {
		name  string
		apply func(*Config)
	}{
		{"hidden name", func(c *Config) { c.SetHiddenNamePrefixes([]string{"Hidden-"}) }},
		{"node blacklist", func(c *Config) { c.SetNodeBlacklist([]string{rpHiddenB}) }},
		{"observer blacklist", func(c *Config) { c.ObserverBlacklist = []string{rpHiddenB} }},
	}
	for _, rule := range rules {
		t.Run(rule.name, func(t *testing.T) {
			cfg := &Config{Port: 3000}
			_, router := setupRoutePatternsServer(t, "visible", cfg)
			for _, h := range hidden {
				if pk := rpDetailNode(t, rpGet(t, router, rpDetail("cc", h), 200), 1)["pubkey"]; pk != rpHiddenB {
					t.Fatalf("baseline: hop %q resolves to %v, want the hidden node", h, pk)
				}
			}
			rule.apply(cfg)
			for i := range hidden {
				// "cc" has no traffic with either hop; "aa" → "bb" has.
				for _, lead := range []string{"cc", "aa"} {
					hCode, hBody := rpDetailRaw(router, lead, hidden[i])
					uCode, uBody := rpDetailRaw(router, lead, unknown[i])
					if hCode != uCode {
						t.Errorf("%s,%s: status %d, unknown hop gives %d", lead, hidden[i], hCode, uCode)
						continue
					}
					if strings.Contains(hBody, "Hidden-Bravo") || strings.Contains(hBody, "Gateway-Three") {
						t.Errorf("%s,%s: body names the hidden identity: %s", lead, hidden[i], hBody)
					}
					var hd, ud map[string]interface{}
					if json.Unmarshal([]byte(hBody), &hd) != nil || json.Unmarshal([]byte(uBody), &ud) != nil {
						t.Errorf("%s,%s: not JSON: %s / %s", lead, hidden[i], hBody, uBody)
						continue
					}
					hn, un := rpDetailNode(t, hd, 1), rpDetailNode(t, ud, 1)
					if len(hn) != len(un) {
						t.Errorf("%s,%s: nodes[1] fields %v, unknown hop has %v", lead, hidden[i], hn, un)
					}
					for k, hv := range hn {
						uv, ok := un[k]
						switch {
						case !ok:
							t.Errorf("%s,%s: nodes[1].%s missing for the unknown hop", lead, hidden[i], k)
						case k == "hop" || k == "name":
							if hv != hidden[i] || uv != unknown[i] {
								t.Errorf("%s,%s: nodes[1].%s = %v / %v, want the raw hops", lead, hidden[i], k, hv, uv)
							}
						case !reflect.DeepEqual(hv, uv):
							t.Errorf("%s,%s: nodes[1].%s = %v, unknown hop gives %v", lead, hidden[i], k, hv, uv)
						}
					}
					if hd["totalMatches"] == 0.0 {
						norm := func(body, hop string) string { return strings.ReplaceAll(body, `"`+hop+`"`, `"HOP"`) }
						if norm(hBody, hidden[i]) != norm(uBody, unknown[i]) {
							t.Errorf("%s,%s: response differs from the unknown hop's:\n%s\n%s", lead, hidden[i], hBody, uBody)
						}
					}
				}
			}
		})
	}
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

// Detail observers go through the same live identity lookup as the hops: an
// observer whose id is a hidden identity (a node key whose node name is
// hidden) is left out, and so is an observer renamed into a hidden name
// after the store loaded its transmissions.
func TestRoutePatterns410_DetailObserverLiveIdentity(t *testing.T) {
	cfg := &Config{Port: 3000}
	srv, router := setupRoutePatternsServer(t, "visible", cfg)
	observers := func() map[string]bool {
		t.Helper()
		out := map[string]bool{}
		for _, o := range rpGet(t, router, rpDetail("aa", "dd"), 200)["observers"].([]interface{}) {
			out[o.(map[string]interface{})["name"].(string)] = true
		}
		return out
	}
	if base := observers(); !base["Gateway-Three"] || !base["Observer One"] {
		t.Fatalf("baseline lacks an observer: %v", base)
	}
	cfg.SetHiddenNamePrefixes([]string{"Hidden-"})
	got := observers()
	if got["Gateway-Three"] {
		t.Errorf("observer sharing a hidden node's key is listed: %v", got)
	}
	if !got["Observer One"] {
		t.Errorf("visible observer dropped: %v", got)
	}
	if _, err := srv.db.conn.Exec(`UPDATE observers SET name = 'Hidden-Renamed' WHERE id = 'obs1'`); err != nil {
		t.Fatal(err)
	}
	if got := observers(); got["Observer One"] {
		t.Errorf("observer renamed into a hidden name is listed under its old name: %v", got)
	}
}

// Ambiguous prefix with a hidden candidate: a row is kept only when the name
// it shows for that hop is a visible candidate's name. A cached row can still
// show the previous name of a node that has since been renamed and hidden;
// that row goes too.
func TestRoutePatterns410_AmbiguousPrefixStaleName(t *testing.T) {
	cfg := &Config{Port: 3000}
	srv, router := setupRoutePatternsServer(t, "visible", cfg)
	rpRequire(t, "list baseline", rpList(t, router), "Alpha → Visible-One") // fills the cache
	rpRequire(t, "bulk baseline", rpBulk(t, router), "Alpha → Visible-One")
	if _, err := srv.db.conn.Exec(`UPDATE nodes SET name = 'Renamed-One' WHERE public_key = ?`, rpAmbVis); err != nil {
		t.Fatal(err)
	}
	// The node list refreshes on its own schedule; the route cache still
	// holds the row under the old name.
	srv.store.cacheMu.Lock()
	srv.store.nodeCache = nil
	srv.store.cacheMu.Unlock()
	cfg.SetNodeBlacklist([]string{rpAmbVis})
	for name, get := range map[string]func(*testing.T, *mux.Router) map[string]bool{"list": rpList, "bulk": rpBulk} {
		got := get(t, router)
		if got["Alpha → Visible-One"] {
			t.Errorf("%s keeps a row under a hidden node's previous name: %v", name, got)
		}
		rpRequire(t, name, got, "Alpha → Charlie", "Alpha → Eve")
	}
}

// Two candidates of one prefix sharing a name, one of them hidden: the shown
// name cannot tell them apart, so the hop is shown raw (fail closed).
func TestVisibleSubpathRow410_SharedNameFailsClosed(t *testing.T) {
	cands := map[string][]nodeInfo{"dd": {{PublicKey: "dd01", Name: "Dup"}, {PublicKey: "dd02", Name: "Dup"}}}
	row := SubpathResp{Path: "Alpha → Dup", RawHops: []string{"aa", "dd"}}
	noName := func(string) bool { return false }
	if got, ok := visibleSubpathRow(row, cands, map[string]bool{"dd02": true}, noName); !ok || got.Path != "Alpha → dd" {
		t.Errorf("shown name is also a hidden candidate's: got %q (ok %v), want the raw hop", got.Path, ok)
	}
	if got, ok := visibleSubpathRow(row, cands, map[string]bool{}, noName); !ok || got.Path != "Alpha → Dup" {
		t.Errorf("nothing hidden: got %q (ok %v)", got.Path, ok)
	}
	if _, ok := visibleSubpathRow(SubpathResp{Path: "Alpha", RawHops: []string{"aa", "dd"}}, cands, nil, noName); ok {
		t.Error("a row whose hops and names do not line up is shown")
	}
}

// Rows that show the same path once masked merge like the store merges raw
// keys of one name: counts summed, pct recomputed, the smallest raw key, and
// ranked by count then path.
func TestMergeSubpathRows410(t *testing.T) {
	rows := []SubpathResp{
		{Path: "Alpha → Eve", RawHops: []string{"aa", "ee"}, Count: 3, Hops: 2, Pct: 30},
		{Path: "Alpha → 11", RawHops: []string{"aa", "11"}, Count: 2, Hops: 2, Pct: 20},
		{Path: "Alpha → 11", RawHops: []string{"aa", "10"}, Count: 2, Hops: 2, Pct: 20},
	}
	got := mergeSubpathRows(rows, 10)
	want := []SubpathResp{
		{Path: "Alpha → 11", RawHops: []string{"aa", "10"}, Count: 4, Hops: 2, Pct: 40},
		{Path: "Alpha → Eve", RawHops: []string{"aa", "ee"}, Count: 3, Hops: 2, Pct: 30},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merged %+v, want %+v", got, want)
	}
}

// The detail's identity lookup covers exactly what the detail can show: the
// requested hops' nodes, the parent paths' nodes and the observer ids, and
// not every known node.
func TestRoutePatterns410_DetailLookupScope(t *testing.T) {
	srv, _ := setupRoutePatternsServer(t, "visible", &Config{Port: 3000})
	got := srv.store.SubpathDetail([]string{"aa", "dd"}).pubkeys()
	want := []string{rpAlpha, rpDup1, rpHiddenB, "obs1", "obs2"}
	gotSet := map[string]bool{}
	for _, pk := range got {
		if gotSet[pk] {
			t.Errorf("duplicate key %q in %v", pk, got)
		}
		gotSet[pk] = true
	}
	if len(got) != len(want) {
		t.Errorf("lookup keys %v, want %v", got, want)
	}
	for _, pk := range want {
		if !gotSet[pk] {
			t.Errorf("lookup keys %v lack %q", got, pk)
		}
	}
}

// BenchmarkRoutePatterns410_Detail2000Nodes times one restricted detail
// request with about 2,000 known nodes, i.e. what the identity lookup costs
// at a production-like node count.
func BenchmarkRoutePatterns410_Detail2000Nodes(b *testing.B) {
	db := seedRoutePatternsDB(b, "visible")
	recent := rpRefTime.Add(-time.Hour).Format(time.RFC3339)
	tx, err := db.conn.Begin()
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		if _, err := tx.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count) VALUES (?, ?, 'repeater', 37.5, -122.0, ?, '2026-01-01T00:00:00Z', 1)`,
			fmt.Sprintf("f1%014x", i), fmt.Sprintf("Bench-%d", i), recent); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	cfg := &Config{Port: 3000}
	cfg.SetHiddenNamePrefixes([]string{"Hidden-"})
	_, router := newRoutePatternsServer(b, db, cfg)
	url := rpDetail("aa", "dd")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", url, nil))
		if w.Code != 200 {
			b.Fatalf("HTTP %d", w.Code)
		}
	}
}

// rpAddTraffic adds one transmission per path, observed by the observer with
// rowid obs, on top of seedRoutePatternsDB (before the store loads).
func rpAddTraffic(t testing.TB, db *DB, obs int, paths ...string) {
	t.Helper()
	recent := rpRefTime.Add(-time.Hour)
	for _, p := range paths {
		res, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json) VALUES ('AABB', ?, ?, 1, 5, '{"type":"CHAN"}')`,
			fmt.Sprintf("rp410-extra-%d-%s", obs, p), recent.Format(time.RFC3339))
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		id, _ := res.LastInsertId()
		if _, err := db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp) VALUES (?, ?, 10, -90, ?, ?)`,
			id, obs, p, recent.Unix()); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

// rpInsertObserver adds an observer row and returns its rowid.
func rpInsertObserver(t testing.TB, db *DB, id, name string) int {
	t.Helper()
	res, err := db.conn.Exec(`INSERT INTO observers (id, name, iata, last_seen, first_seen, packet_count) VALUES (?, ?, 'SJC', ?, '2026-01-01T00:00:00Z', 1)`,
		id, name, rpRefTime.Format(time.RFC3339))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	rowid, _ := res.LastInsertId()
	return int(rowid)
}

// rpHidingRules are the three ways an identity is hidden.
var rpHidingRules = []struct {
	name  string
	apply func(*Config)
}{
	{"hidden name", func(c *Config) { c.SetHiddenNamePrefixes([]string{"Hidden-"}) }},
	{"node blacklist", func(c *Config) { c.SetNodeBlacklist([]string{rpHiddenB}) }},
	{"observer blacklist", func(c *Config) { c.ObserverBlacklist = []string{rpHiddenB} }},
}

// With hidden-name prefixes configured the detail issues its identity lookup
// whether or not a requested hop resolves to a node, so the work a request
// does is the same for a hop that resolves and one that does not. Without
// hidden-name prefixes no request issues one.
func TestRoutePatterns410_DetailLookupAlwaysRuns(t *testing.T) {
	cfg := &Config{Port: 3000}
	srv, router := setupRoutePatternsServer(t, "visible", cfg)
	var queries int
	srv.db.identityNamesQueryHook = func([]string) { queries++ }
	count := func(hops ...string) int {
		t.Helper()
		queries = 0
		if code, body := rpDetailRaw(router, hops...); code != 200 {
			t.Fatalf("%v: HTTP %d %s", hops, code, body)
		}
		return queries
	}
	cfg.SetHiddenNamePrefixes([]string{"Hidden-"})
	for _, hops := range [][]string{{"aa", "bb"}, {"cc", "bb"}, {"aa", "f0"}, {"f0", "f1"}, {"f0f0f0f0f0f0f0f0", rpHiddenB}} {
		if n := count(hops...); n != 1 {
			t.Errorf("hidden-name config, hops %v: %d identity lookups, want 1", hops, n)
		}
	}
	cfg.SetHiddenNamePrefixes(nil)
	cfg.SetNodeBlacklist([]string{rpHiddenB})
	for _, hops := range [][]string{{"aa", "bb"}, {"f0", "f1"}} {
		if n := count(hops...); n != 0 {
			t.Errorf("blacklist-only config, hops %v: %d identity lookups, want 0", hops, n)
		}
	}
}

// A hidden hop that carries traffic has the same form as an unknown hop that
// carries the same traffic: in the detail (parent paths included) and in
// list and bulk rows it is shown as its raw hop, not dropped. The fixture
// mirrors every path through the hidden node "bb" with the unknown hop "f0".
func TestRoutePatterns410_HiddenHopWithTrafficLooksUnknown(t *testing.T) {
	for _, rule := range rpHidingRules {
		t.Run(rule.name, func(t *testing.T) {
			db := seedRoutePatternsDB(t, "visible")
			rpAddTraffic(t, db, 1, `["aa","f0"]`)
			rpAddTraffic(t, db, 2, `["aa","dd","f0"]`)
			cfg := &Config{Port: 3000}
			_, router := newRoutePatternsServer(t, db, cfg)
			rule.apply(cfg)

			norm := func(s, hop string) string {
				s = strings.ReplaceAll(s, `"`+hop+`"`, `"HOP"`)
				return strings.ReplaceAll(s, "→ "+hop, "→ HOP")
			}
			for _, lead := range []string{"aa", "dd"} {
				hCode, hBody := rpDetailRaw(router, lead, "bb")
				uCode, uBody := rpDetailRaw(router, lead, "f0")
				if hCode != 200 || uCode != 200 {
					t.Fatalf("%s: HTTP %d / %d", lead, hCode, uCode)
				}
				if !strings.Contains(uBody, `"totalMatches":1`) {
					t.Fatalf("%s,f0: fixture has no traffic: %s", lead, uBody)
				}
				if norm(hBody, "bb") != norm(uBody, "f0") {
					t.Errorf("%s: hidden hop with traffic differs from an unknown one:\n%s\n%s", lead, hBody, uBody)
				}
			}

			rows := func(group map[string]interface{}, hop string) []string {
				var out []string
				for _, r := range group["subpaths"].([]interface{}) {
					m := r.(map[string]interface{})
					raw := m["rawHops"].([]interface{})
					if raw[len(raw)-1] != hop {
						continue
					}
					b, _ := json.Marshal(m)
					out = append(out, norm(string(b), hop))
				}
				sort.Strings(out)
				return out
			}
			list := rpGet(t, router, "/api/analytics/subpaths?minLen=2&maxLen=8&limit=100", 200)
			groups := []map[string]interface{}{list}
			for _, g := range rpGet(t, router, "/api/analytics/subpaths-bulk?groups=2-2:50,3-3:50", 200)["results"].([]interface{}) {
				groups = append(groups, g.(map[string]interface{}))
			}
			for i, g := range groups {
				h, u := rows(g, "bb"), rows(g, "f0")
				if len(u) == 0 || !reflect.DeepEqual(h, u) {
					t.Errorf("group %d: rows through the hidden hop %v, through the unknown hop %v", i, h, u)
				}
			}
		})
	}
}

// An ambiguous requested hop is resolved over the visible candidates only:
// with the resolver's favourite hidden, the detail names the visible
// candidate, exactly as if the hidden node did not exist.
func TestRoutePatterns410_DetailResolvesVisibleCandidates(t *testing.T) {
	rules := []struct {
		name  string
		apply func(*Config)
	}{
		{"hidden name", func(c *Config) { c.SetHiddenNamePrefixes([]string{"Hidden-"}) }},
		{"node blacklist", func(c *Config) { c.SetNodeBlacklist([]string{rpAmbHid}) }},
		{"observer blacklist", func(c *Config) { c.ObserverBlacklist = []string{rpAmbHid} }},
	}
	for _, rule := range rules {
		t.Run(rule.name, func(t *testing.T) {
			cfg := &Config{Port: 3000}
			_, router := setupRoutePatternsServer(t, "hidden", cfg)
			for _, lead := range []string{"aa", "cc"} {
				if pk := rpDetailNode(t, rpGet(t, router, rpDetail(lead, "11"), 200), 1)["pubkey"]; pk != rpAmbHid {
					t.Fatalf("baseline %s,11: resolves to %v, want the hidden candidate", lead, pk)
				}
			}
			rule.apply(cfg)
			for _, lead := range []string{"aa", "cc"} {
				got := rpDetailNode(t, rpGet(t, router, rpDetail(lead, "11"), 200), 1)
				if got["pubkey"] != rpAmbVis || got["name"] != "Visible-One" {
					t.Errorf("%s,11: nodes[1] = %v, want the visible candidate", lead, got)
				}
			}
		})
	}
}

// List and bulk apply the name rules to the name a cached row shows, as the
// detail does: a row cached under a hidden-prefixed name does not show it
// after (a) the node left the node list or (b) was renamed out of the prefix.
func TestRoutePatterns410_CachedHiddenNameFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change string
	}{
		{"node gone", `DELETE FROM nodes WHERE public_key = ?`},
		{"renamed", `UPDATE nodes SET name = 'Bravo' WHERE public_key = ?`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Port: 3000}
			srv, router := setupRoutePatternsServer(t, "visible", cfg)
			rpRequire(t, "list baseline", rpList(t, router), "Alpha → Hidden-Bravo") // fills the caches
			rpRequire(t, "bulk baseline", rpBulk(t, router), "Alpha → Hidden-Bravo")
			if _, err := srv.db.conn.Exec(tc.change, rpHiddenB); err != nil {
				t.Fatal(err)
			}
			srv.store.cacheMu.Lock()
			srv.store.nodeCache = nil
			srv.store.cacheMu.Unlock()
			cfg.SetHiddenNamePrefixes([]string{"Hidden-"})
			for name, get := range map[string]func(*testing.T, *mux.Router) map[string]bool{"list": rpList, "bulk": rpBulk} {
				if got := get(t, router); rpAnyContains(got, "Hidden-") {
					t.Errorf("%s shows a cached hidden name: %v", name, got)
				}
			}
		})
	}
}

// The live lookup is case-insensitive: an observer whose id is a hidden
// node's key in upper case is left out of the detail.
func TestRoutePatterns410_DetailObserverIdCase(t *testing.T) {
	db := seedRoutePatternsDB(t, "visible")
	rpAddTraffic(t, db, rpInsertObserver(t, db, strings.ToUpper(rpHiddenB), "Gateway-Upper"), `["aa","dd"]`)
	cfg := &Config{Port: 3000}
	_, router := newRoutePatternsServer(t, db, cfg)
	if base := rpDetailText(t, router, rpDetail("aa", "dd")); !strings.Contains(base, "Gateway-Upper") {
		t.Fatalf("baseline lacks the observer: %s", base)
	}
	cfg.SetHiddenNamePrefixes([]string{"Hidden-"})
	if got := rpDetailText(t, router, rpDetail("aa", "dd")); strings.Contains(got, "Gateway-Upper") {
		t.Errorf("observer with an upper-case hidden key is listed: %s", got)
	}
}

// A node that appears only in a parent path and is hidden only through an
// observer alias (an observer row with its key and a hidden name, but no
// traffic of its own) is looked up too.
func TestRoutePatterns410_DetailParentPathAliasOnly(t *testing.T) {
	db := seedRoutePatternsDB(t, "visible")
	rpAddTraffic(t, db, 1, `["aa","dd","ee"]`)
	rpInsertObserver(t, db, rpEve, "Hidden-Alias")
	cfg := &Config{Port: 3000}
	_, router := newRoutePatternsServer(t, db, cfg)
	if base := rpDetailText(t, router, rpDetail("aa", "dd")); !strings.Contains(base, "Dup → Eve") {
		t.Fatalf("baseline lacks the parent path: %s", base)
	}
	cfg.SetHiddenNamePrefixes([]string{"Hidden-"})
	if got := rpDetailText(t, router, rpDetail("aa", "dd")); strings.Contains(got, "Eve") {
		t.Errorf("parent path names a node hidden by its observer alias: %s", got)
	}
}

// The lookup scope covers every candidate of a requested hop, not only the
// resolver's pick, so a hidden candidate can be excluded from the choice.
func TestRoutePatterns410_DetailLookupScopeCandidates(t *testing.T) {
	srv, _ := setupRoutePatternsServer(t, "visible", &Config{Port: 3000})
	got := map[string]bool{}
	for _, pk := range srv.store.SubpathDetail([]string{"aa", "11"}).pubkeys() {
		got[pk] = true
	}
	for _, pk := range []string{rpAlpha, rpAmbVis, rpAmbHid} {
		if !got[pk] {
			t.Errorf("lookup keys %v lack %q", got, pk)
		}
	}
}

// Every seed helper takes its timestamps from one reference time, so a clock
// that ticks over a second boundary between two seed calls cannot split the
// firstSeen of the hidden and the unknown rows (#483). The test waits for the
// next second boundary between the base seed and the added traffic.
func TestRoutePatterns410_SeedsShareOneClock(t *testing.T) {
	db := seedRoutePatternsDB(t, "visible")
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second + 20*time.Millisecond)))
	rpAddTraffic(t, db, 1, `["aa","f0"]`)
	var distinct int
	if err := db.conn.QueryRow(`SELECT COUNT(DISTINCT first_seen) FROM transmissions`).Scan(&distinct); err != nil {
		t.Fatal(err)
	}
	if distinct != 1 {
		t.Errorf("seeded transmissions carry %d distinct first_seen values, want 1", distinct)
	}
}
