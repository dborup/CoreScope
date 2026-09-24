package main

// /api/analytics/topology must not surface blacklisted nodes.
//
// These tests drive the real handler over data built by the real
// computeAnalyticsTopology (maps, not the Topology* structs), because that is
// where the filter used to fail: it type-asserted []TopRepeater and friends,
// every assertion missed, and the blacklisted node came back in all five
// pubkey-bearing parts of the response.
//
// Every test first proves the target IS in the topology without a blacklist.
// A target that was never there would make "absent after blacklisting" pass
// vacuously.

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

const (
	topoTarget = "7a7a000000000001" // in every part, as pubkeyA and pubkeyB
	topoLower  = "5b5b000000000002" // pairs with the target as pubkeyA
	topoUpper  = "8c8c000000000003" // pairs with the target as pubkeyB
	topoOther  = "9d9d000000000004" // never blacklisted
)

// topoPositions are the places a node's pubkey can appear in the response.
var topoPositions = []string{
	"topRepeaters", "topPairs.pubkeyA", "topPairs.pubkeyB",
	"bestPathList", "multiObsNodes", "perObserverReach",
}

// seedTopologyPrivacyDB builds a store whose computed topology holds the
// target in every position: four repeaters with distinct 1-byte hop prefixes,
// paths seen by two different observers (multiObsNodes needs >1 observer per
// hop), and pairs where the target sorts first and where it sorts second.
func seedTopologyPrivacyDB(t *testing.T) *DB {
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
	mustExec(`INSERT INTO observers (id, name, iata, last_seen, first_seen, packet_count) VALUES ('obs2','Observer Two','SFO',?,'2026-01-01T00:00:00Z',10)`, recent)
	for _, n := range [][2]string{{topoTarget, "Target"}, {topoLower, "Lower"}, {topoUpper, "Upper"}, {topoOther, "Other"}} {
		mustExec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count) VALUES (?, ?, 'repeater', 37.5, -122.0, ?, '2026-01-01T00:00:00Z', 5)`, n[0], n[1], recent)
	}
	for i, p := range []struct {
		path string
		obs  int
	}{
		{`["5b","7a","8c"]`, 1}, // pairs 5b|7a (target = B) and 7a|8c (target = A)
		{`["7a","9d"]`, 2},
		{`["9d","5b"]`, 1},
		{`["9d","7a"]`, 2},
	} {
		mustExec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json) VALUES ('AABB', ?, ?, 1, 5, '{"type":"CHAN"}')`, fmt.Sprintf("topo-priv-%d", i), recent)
		mustExec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp) VALUES (?, ?, 10, -90, ?, ?)`, i+1, p.obs, p.path, epoch)
	}
	return db
}

func setupTopologyPrivacyServer(t *testing.T, blacklist []string) (*Server, *mux.Router) {
	t.Helper()
	db := seedTopologyPrivacyDB(t)
	cfg := &Config{Port: 3000, NodeBlacklist: blacklist}
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

func getTopology(t *testing.T, router *mux.Router, query string) map[string]interface{} {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/topology"+query, nil))
	if w.Code != 200 {
		t.Fatalf("topology: HTTP %d: %s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("topology: not JSON: %v", err)
	}
	return body
}

// topoPubkeys returns the lower-cased pubkeys at each position of a decoded
// response. A part that is missing or has the wrong shape is a test failure:
// the response contract is what clients (and the QA script) rely on.
func topoPubkeys(t *testing.T, body map[string]interface{}) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	add := func(pos string, v interface{}) {
		if s, ok := v.(string); ok {
			out[pos] = append(out[pos], strings.ToLower(s))
		}
	}
	list := func(k string) []interface{} {
		v, ok := body[k]
		if !ok {
			t.Fatalf("response has no %q", k)
		}
		if v == nil {
			return nil
		}
		l, ok := v.([]interface{})
		if !ok {
			t.Fatalf("%q is %T, want a list", k, v)
		}
		return l
	}
	for _, e := range list("topRepeaters") {
		add("topRepeaters", e.(map[string]interface{})["pubkey"])
	}
	for _, e := range list("topPairs") {
		m := e.(map[string]interface{})
		add("topPairs.pubkeyA", m["pubkeyA"])
		add("topPairs.pubkeyB", m["pubkeyB"])
	}
	for _, e := range list("bestPathList") {
		add("bestPathList", e.(map[string]interface{})["pubkey"])
	}
	for _, e := range list("multiObsNodes") {
		add("multiObsNodes", e.(map[string]interface{})["pubkey"])
	}
	reach, ok := body["perObserverReach"].(map[string]interface{})
	if !ok {
		t.Fatalf("perObserverReach is %T, want an object", body["perObserverReach"])
	}
	for _, obs := range reach {
		rings, _ := obs.(map[string]interface{})["rings"].([]interface{})
		for _, r := range rings {
			nodes, _ := r.(map[string]interface{})["nodes"].([]interface{})
			for _, n := range nodes {
				add("perObserverReach", n.(map[string]interface{})["pubkey"])
			}
		}
	}
	return out
}

func topoHas(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// requireEverywhere is the precondition: without it, "absent" proves nothing.
func requireEverywhere(t *testing.T, body map[string]interface{}, pk string) {
	t.Helper()
	got := topoPubkeys(t, body)
	for _, pos := range topoPositions {
		if !topoHas(got[pos], pk) {
			t.Fatalf("precondition: %s not in %s before blacklisting (%v) — the test would prove nothing", pk, pos, got[pos])
		}
	}
}

func requireNowhere(t *testing.T, body map[string]interface{}, pk string) {
	t.Helper()
	got := topoPubkeys(t, body)
	for _, pos := range topoPositions {
		if topoHas(got[pos], pk) {
			t.Errorf("blacklisted %s still in %s", pk, pos)
		}
	}
}

// visibleEntries is every entry of the five parts that references none of the
// hidden pubkeys, as canonical JSON — what filtering must leave untouched.
func visibleEntries(t *testing.T, body map[string]interface{}, hidden ...string) []string {
	t.Helper()
	refs := func(m map[string]interface{}, keys ...string) bool {
		for _, k := range keys {
			if s, ok := m[k].(string); ok && topoHas(hidden, strings.ToLower(s)) {
				return true
			}
		}
		return false
	}
	var out []string
	keep := func(part string, m map[string]interface{}) {
		b, _ := json.Marshal(m)
		out = append(out, part+":"+string(b))
	}
	for _, part := range []string{"topRepeaters", "bestPathList", "multiObsNodes"} {
		l, _ := body[part].([]interface{})
		for _, e := range l {
			if m := e.(map[string]interface{}); !refs(m, "pubkey") {
				keep(part, m)
			}
		}
	}
	pairs, _ := body["topPairs"].([]interface{})
	for _, e := range pairs {
		if m := e.(map[string]interface{}); !refs(m, "pubkeyA", "pubkeyB") {
			keep("topPairs", m)
		}
	}
	reach, _ := body["perObserverReach"].(map[string]interface{})
	for obsID, obs := range reach {
		rings, _ := obs.(map[string]interface{})["rings"].([]interface{})
		for _, r := range rings {
			ring := r.(map[string]interface{})
			nodes, _ := ring["nodes"].([]interface{})
			for _, n := range nodes {
				if m := n.(map[string]interface{}); !refs(m, "pubkey") {
					keep(fmt.Sprintf("perObserverReach[%s][%v]", obsID, ring["hops"]), m)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// ─── Reproduction ────────────────────────────────────────────────────────────

func TestTopologyBlacklist_RemovesTargetFromEveryPart(t *testing.T) {
	srv, router := setupTopologyPrivacyServer(t, nil)
	before := getTopology(t, router, "")
	requireEverywhere(t, before, topoTarget)

	srv.cfg.SetNodeBlacklist([]string{topoTarget})
	after := getTopology(t, router, "")
	requireNowhere(t, after, topoTarget)
	if b, a := visibleEntries(t, before, topoTarget), visibleEntries(t, after, topoTarget); !reflect.DeepEqual(b, a) {
		t.Errorf("entries not involving the blacklisted node changed:\nbefore %v\nafter  %v", b, a)
	}
	// Other top-level fields are not privacy-filtered and must not change.
	for _, k := range []string{"uniqueNodes", "avgHops", "medianHops", "maxHops", "hopDistribution", "hopsVsSnr", "observers"} {
		if !reflect.DeepEqual(before[k], after[k]) {
			t.Errorf("%s changed: %v → %v", k, before[k], after[k])
		}
	}
}

// Configured at startup (the production path: a config change needs a
// restart), so the very first, cold computation is filtered too.
func TestTopologyBlacklist_ColdStartConfig(t *testing.T) {
	srv, router := setupTopologyPrivacyServer(t, []string{topoTarget})
	requireNowhere(t, getTopology(t, router, ""), topoTarget) // cold
	requireNowhere(t, getTopology(t, router, ""), topoTarget) // warm (cached)
	// The target is really in the computed data: clearing the blacklist shows it.
	srv.cfg.SetNodeBlacklist(nil)
	requireEverywhere(t, getTopology(t, router, ""), topoTarget)
}

// Same normalisation as every other privacy check (Config.IsBlacklisted):
// trimmed, case-insensitive.
func TestTopologyBlacklist_CaseAndWhitespace(t *testing.T) {
	for _, entry := range []string{
		topoTarget,
		strings.ToUpper(topoTarget),
		"7A7a000000000001",
		"  " + strings.ToUpper(topoTarget) + "\t",
	} {
		t.Run(fmt.Sprintf("%q", entry), func(t *testing.T) {
			srv, router := setupTopologyPrivacyServer(t, nil)
			requireEverywhere(t, getTopology(t, router, ""), topoTarget)
			srv.cfg.SetNodeBlacklist([]string{entry})
			requireNowhere(t, getTopology(t, router, ""), topoTarget)
		})
	}
}

func TestTopologyBlacklist_EmptyBlacklistChangesNothing(t *testing.T) {
	for name, bl := range map[string][]string{"nil": nil, "empty": {}, "blank entry": {"   "}} {
		t.Run(name, func(t *testing.T) {
			srv, router := setupTopologyPrivacyServer(t, nil)
			before := getTopology(t, router, "")
			requireEverywhere(t, before, topoTarget)
			srv.cfg.SetNodeBlacklist(bl)
			after := getTopology(t, router, "")
			if !reflect.DeepEqual(before, after) {
				t.Errorf("response changed with blacklist %q", bl)
			}
		})
	}
}

func TestTopologyBlacklist_MultipleNodes(t *testing.T) {
	srv, router := setupTopologyPrivacyServer(t, nil)
	before := getTopology(t, router, "")
	requireEverywhere(t, before, topoTarget)
	got := topoPubkeys(t, before)
	if !topoHas(got["topPairs.pubkeyA"], topoLower) || !topoHas(got["topPairs.pubkeyB"], topoUpper) {
		t.Fatalf("precondition: %s as pubkeyA and %s as pubkeyB expected, got %v", topoLower, topoUpper, got)
	}
	srv.cfg.SetNodeBlacklist([]string{topoTarget, strings.ToUpper(topoLower), topoUpper})
	after := getTopology(t, router, "")
	for _, pk := range []string{topoTarget, topoLower, topoUpper} {
		requireNowhere(t, after, pk)
	}
	if !topoHas(topoPubkeys(t, after)["topRepeaters"], topoOther) {
		t.Errorf("non-blacklisted %s disappeared", topoOther)
	}
	if b, a := visibleEntries(t, before, topoTarget, topoLower, topoUpper), visibleEntries(t, after, topoTarget, topoLower, topoUpper); !reflect.DeepEqual(b, a) {
		t.Errorf("entries not involving blacklisted nodes changed:\nbefore %v\nafter  %v", b, a)
	}
}

// A blacklisted node on only one side of a pair removes the pair — whichever
// side it is on.
func TestTopologyBlacklist_PairSides(t *testing.T) {
	for _, tc := range []struct{ pk, side string }{{topoLower, "pubkeyA"}, {topoUpper, "pubkeyB"}} {
		t.Run(tc.side, func(t *testing.T) {
			srv, router := setupTopologyPrivacyServer(t, nil)
			before := getTopology(t, router, "")
			if !topoHas(topoPubkeys(t, before)["topPairs."+tc.side], tc.pk) {
				t.Fatalf("precondition: %s not in topPairs.%s", tc.pk, tc.side)
			}
			srv.cfg.SetNodeBlacklist([]string{tc.pk})
			after := topoPubkeys(t, getTopology(t, router, ""))
			if topoHas(after["topPairs.pubkeyA"], tc.pk) || topoHas(after["topPairs.pubkeyB"], tc.pk) {
				t.Errorf("pair with blacklisted %s on side %s kept", tc.pk, tc.side)
			}
		})
	}
}

// Name-based hiding (HiddenNamePrefixes, #1181) applies to the same entries,
// with or without a node blacklist configured.
func TestTopologyBlacklist_HiddenNamePrefix(t *testing.T) {
	for name, bl := range map[string][]string{"prefixes only": nil, "with a blacklist": {"ffff000000000000"}} {
		t.Run(name, func(t *testing.T) {
			srv, router := setupTopologyPrivacyServer(t, nil)
			requireEverywhere(t, getTopology(t, router, ""), topoTarget)
			srv.cfg.SetNodeBlacklist(bl)
			srv.cfg.SetHiddenNamePrefixes([]string{"Targ"})
			requireNowhere(t, getTopology(t, router, ""), topoTarget)
			if !topoHas(topoPubkeys(t, getTopology(t, router, ""))["topRepeaters"], topoOther) {
				t.Errorf("a node with a visible name was hidden")
			}
		})
	}
}

// ─── Caches: filtered per response, never in place ───────────────────────────

func TestTopologyBlacklist_CacheIsNotMutated(t *testing.T) {
	srv, router := setupTopologyPrivacyServer(t, nil)
	requireEverywhere(t, getTopology(t, router, ""), topoTarget) // warms topoCache
	cachedBefore, _ := json.Marshal(srv.store.GetAnalyticsTopology("", ""))

	srv.cfg.SetNodeBlacklist([]string{topoTarget})
	requireNowhere(t, getTopology(t, router, ""), topoTarget)
	cachedAfter, _ := json.Marshal(srv.store.GetAnalyticsTopology("", ""))
	if string(cachedBefore) != string(cachedAfter) {
		t.Fatalf("the shared cached topology was modified by filtering")
	}
	// No stale privacy state in either direction.
	srv.cfg.SetNodeBlacklist(nil)
	requireEverywhere(t, getTopology(t, router, ""), topoTarget)
	srv.cfg.SetNodeBlacklist([]string{topoTarget})
	requireNowhere(t, getTopology(t, router, ""), topoTarget)
}

// The steady-state path serves the recomputer's snapshot (issue #1240) — the
// same shared-object rules apply to it.
func TestTopologyBlacklist_RecomputerSnapshot(t *testing.T) {
	srv, router := setupTopologyPrivacyServer(t, nil)
	store := srv.store
	rc := newAnalyticsRecomputer("topo-privacy-test", time.Hour, func() interface{} {
		return store.computeAnalyticsTopology("", "", TimeWindow{})
	})
	rc.runOnce()
	store.analyticsRecomputerMu.Lock()
	store.recompTopology = rc
	store.analyticsRecomputerMu.Unlock()
	snapBefore, _ := json.Marshal(rc.Load())

	requireEverywhere(t, getTopology(t, router, ""), topoTarget)
	srv.cfg.SetNodeBlacklist([]string{topoTarget})
	requireNowhere(t, getTopology(t, router, ""), topoTarget)
	if snapAfter, _ := json.Marshal(rc.Load()); string(snapBefore) != string(snapAfter) {
		t.Fatalf("the recomputer snapshot was modified by filtering")
	}
	if runs := rc.ComputeRuns(); runs != 1 {
		t.Errorf("filtering recomputed the snapshot (%d runs)", runs)
	}
}

// Region/window queries take the non-snapshot cache path.
func TestTopologyBlacklist_WindowQuery(t *testing.T) {
	srv, router := setupTopologyPrivacyServer(t, nil)
	requireEverywhere(t, getTopology(t, router, "?days=7"), topoTarget)
	srv.cfg.SetNodeBlacklist([]string{topoTarget})
	requireNowhere(t, getTopology(t, router, "?days=7"), topoTarget)
}

// Concurrent requests, blacklist changes and cache invalidation: no data race
// (run with -race), and within each phase no response disagrees with the
// blacklist in force.
func TestTopologyBlacklist_Concurrent(t *testing.T) {
	srv, router := setupTopologyPrivacyServer(t, nil)
	requireEverywhere(t, getTopology(t, router, ""), topoTarget)

	fetch := func() (map[string]interface{}, error) {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/topology", nil))
		if w.Code != 200 {
			return nil, fmt.Errorf("HTTP %d", w.Code)
		}
		var body map[string]interface{}
		return body, json.Unmarshal(w.Body.Bytes(), &body)
	}
	phase := func(blacklisted bool) {
		t.Helper()
		var wg sync.WaitGroup
		errs := make(chan error, 256)
		for g := 0; g < 12; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 15; i++ {
					body, err := fetch()
					if err != nil {
						errs <- err
						return
					}
					b, _ := json.Marshal(body)
					if has := strings.Contains(string(b), topoTarget); has == blacklisted {
						errs <- fmt.Errorf("blacklisted=%v but target present=%v", blacklisted, has)
						return
					}
				}
			}()
		}
		wg.Add(1)
		go func() { // cache invalidation racing the readers
			defer wg.Done()
			for i := 0; i < 15; i++ {
				srv.store.invalidateCachesFor(cacheInvalidation{eviction: true})
			}
		}()
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
	}
	srv.cfg.SetNodeBlacklist([]string{topoTarget})
	phase(true)
	srv.cfg.SetNodeBlacklist(nil)
	phase(false)

	// Blacklist changes racing requests: only the race detector judges this.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				srv.cfg.SetNodeBlacklist([]string{topoTarget})
			} else {
				srv.cfg.SetNodeBlacklist(nil)
			}
		}
	}()
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				if _, err := fetch(); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// ─── The filter itself: shapes, fail-closed, no mutation ─────────────────────

// computedTopology returns the real computeAnalyticsTopology output for the
// seed, plus a server whose blacklist holds the target.
func computedTopology(t *testing.T) (*Server, map[string]interface{}) {
	t.Helper()
	srv, _ := setupTopologyPrivacyServer(t, []string{topoTarget})
	return srv, srv.store.computeAnalyticsTopology("", "", TimeWindow{})
}

func jsonOf(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestFilterBlacklistedFromTopology_DoesNotMutateInput(t *testing.T) {
	srv, data := computedTopology(t)
	before := jsonOf(t, data)
	if !strings.Contains(before, topoTarget) {
		t.Fatalf("precondition: computed topology lacks the target")
	}
	out := srv.filterBlacklistedFromTopology(data)
	if after := jsonOf(t, data); after != before {
		t.Fatalf("input topology was modified in place")
	}
	if strings.Contains(jsonOf(t, out), topoTarget) {
		t.Errorf("filtered output still contains the target")
	}
}

func TestFilterBlacklistedFromTopology_NilAndNoMatch(t *testing.T) {
	srv, data := computedTopology(t)
	if got := srv.filterBlacklistedFromTopology(nil); got != nil {
		t.Errorf("nil input → %v", got)
	}
	srv.cfg.SetNodeBlacklist([]string{"ffff000000000000"})
	out := srv.filterBlacklistedFromTopology(data)
	if jsonOf(t, out) != jsonOf(t, data) {
		t.Errorf("nothing blacklisted in the data, but the output differs")
	}
}

// One required key missing: the rest is still filtered and nothing panics.
func TestFilterBlacklistedFromTopology_MissingKey(t *testing.T) {
	for _, key := range []string{"topRepeaters", "topPairs", "bestPathList", "multiObsNodes", "perObserverReach"} {
		t.Run(key, func(t *testing.T) {
			srv, data := computedTopology(t)
			cp := map[string]interface{}{}
			for k, v := range data {
				if k != key {
					cp[k] = v
				}
			}
			out := srv.filterBlacklistedFromTopology(cp)
			if _, ok := out[key]; ok {
				t.Errorf("filter invented %q", key)
			}
			if strings.Contains(jsonOf(t, out), topoTarget) {
				t.Errorf("target leaked with %q missing", key)
			}
		})
	}
}

// JSON-decoded ([]interface{}) and typed (Topology* structs) shapes are
// filtered too, so a change in how the store builds the result cannot turn
// the filter into a no-op again.
func TestFilterBlacklistedFromTopology_OtherKnownShapes(t *testing.T) {
	srv, data := computedTopology(t)
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(jsonOf(t, data)), &decoded); err != nil {
		t.Fatal(err)
	}
	if out := jsonOf(t, srv.filterBlacklistedFromTopology(decoded)); strings.Contains(out, topoTarget) || !strings.Contains(out, topoOther) {
		t.Errorf("JSON-decoded shape: target present=%v other present=%v", strings.Contains(out, topoTarget), strings.Contains(out, topoOther))
	}

	typed := map[string]interface{}{
		"topRepeaters":  []TopRepeater{{Hop: "7a", Pubkey: topoTarget, Name: "Target"}, {Hop: "9d", Pubkey: topoOther, Name: "Other"}},
		"topPairs":      []TopPair{{HopA: "7a", HopB: "9d", PubkeyA: topoTarget, PubkeyB: topoOther}, {HopA: "5b", HopB: "9d", PubkeyA: topoLower, PubkeyB: topoOther}},
		"bestPathList":  []BestPathEntry{{Hop: "7a", Pubkey: topoTarget}, {Hop: "9d", Pubkey: topoOther}},
		"multiObsNodes": []MultiObsNode{{Hop: "7a", Pubkey: topoTarget}, {Hop: "9d", Pubkey: topoOther}},
		"perObserverReach": map[string]*ObserverReach{"obs1": {ObserverName: "o", Rings: []ReachRing{{Hops: 1, Nodes: []ReachNode{
			{Hop: "7a", Pubkey: strings.ToUpper(topoTarget)}, {Hop: "9d", Pubkey: topoOther}}}}}},
	}
	out := jsonOf(t, srv.filterBlacklistedFromTopology(typed))
	if strings.Contains(strings.ToLower(out), topoTarget) {
		t.Errorf("typed shape: target leaked: %s", out)
	}
	if strings.Count(out, topoOther) != 5 {
		t.Errorf("typed shape: non-blacklisted entries lost: %s", out)
	}
}

// Anything the filter cannot read is dropped, never passed through: an
// unknown shape must not become a privacy leak. Every case carries the
// target's pubkey somewhere the filter cannot interpret it, so a pass-through
// shows up as the pubkey in the output.
func TestFilterBlacklistedFromTopology_UnknownShapeFailsClosed(t *testing.T) {
	wrapped := map[string]interface{}{"hex": topoTarget} // a pubkey field that is not a string
	node := func(pk interface{}) map[string]interface{} { return map[string]interface{}{"hop": "7a", "pubkey": pk} }
	reach := func(rings interface{}) map[string]interface{} {
		return map[string]interface{}{"obs1": map[string]interface{}{"observer_name": "o", "rings": rings}}
	}
	cases := []struct {
		name, part string
		value      interface{}
	}{
		{"string instead of list", "topRepeaters", topoTarget},
		{"object instead of list", "bestPathList", map[string]interface{}{"pubkey": topoTarget}},
		{"list of strings", "multiObsNodes", []string{topoTarget}},
		{"list of non-objects", "topRepeaters", []interface{}{topoTarget}},
		{"pubkey not a string", "topRepeaters", []interface{}{node(wrapped)}},
		{"pubkey as list", "bestPathList", []map[string]interface{}{node([]interface{}{topoTarget})}},
		{"pubkeyA not a string", "topPairs", []interface{}{map[string]interface{}{"pubkeyA": wrapped, "pubkeyB": topoOther}}},
		{"pubkeyB not a string", "topPairs", []interface{}{map[string]interface{}{"pubkeyA": topoOther, "pubkeyB": wrapped}}},
		{"reach not an object", "perObserverReach", []interface{}{node(topoTarget)}},
		{"reach entry not an object", "perObserverReach", map[string]interface{}{"obs1": topoTarget}},
		{"rings not a list", "perObserverReach", reach(map[string]interface{}{"x": topoTarget})},
		{"ring not an object", "perObserverReach", reach([]interface{}{topoTarget})},
		{"nodes not a list", "perObserverReach", reach([]interface{}{map[string]interface{}{"hops": 1, "nodes": node(topoTarget)}})},
		{"reach node pubkey not a string", "perObserverReach", reach([]interface{}{map[string]interface{}{"hops": 1, "nodes": []interface{}{node(wrapped)}}})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, data := computedTopology(t)
			cp := map[string]interface{}{}
			for k, v := range data {
				cp[k] = v
			}
			cp[tc.part] = tc.value
			if !strings.Contains(jsonOf(t, cp), topoTarget) {
				t.Fatalf("precondition: case does not carry the target")
			}
			out := jsonOf(t, srv.filterBlacklistedFromTopology(cp))
			if strings.Contains(strings.ToLower(out), topoTarget) {
				t.Errorf("unreadable %s leaked the target: %s", tc.part, out)
			}
			if !strings.Contains(out, topoOther) {
				t.Errorf("readable parts were dropped along with the unreadable one")
			}
		})
	}
}

// The filter decides from the response alone. It must not look nodes up in
// the database per entry: with HiddenNamePrefixes set that was one SQLite
// query per bestPathList entry (up to 50) on every request. A server without
// a database makes any such lookup panic.
func TestFilterBlacklistedFromTopology_NoDatabaseLookups(t *testing.T) {
	_, data := computedTopology(t)
	srv := &Server{cfg: &Config{}}
	srv.cfg.SetHiddenNamePrefixes([]string{"Targ"})
	out := jsonOf(t, srv.filterBlacklistedFromTopology(data))
	if strings.Contains(out, topoTarget) {
		t.Errorf("name-hidden target still present")
	}
	if !strings.Contains(out, topoOther) {
		t.Errorf("visible nodes were dropped")
	}
}

// Unresolved hops carry no pubkey (nil): nothing to match, so they stay.
func TestFilterBlacklistedFromTopology_UnresolvedHopsKept(t *testing.T) {
	srv, _ := computedTopology(t)
	unresolved := func(hop string) map[string]interface{} {
		return map[string]interface{}{"hop": hop, "name": nil, "pubkey": nil, "count": 1}
	}
	data := map[string]interface{}{
		"topRepeaters": []map[string]interface{}{unresolved("ee01"), {"hop": "7a", "name": "Target", "pubkey": topoTarget}},
		"perObserverReach": map[string]interface{}{"obs1": map[string]interface{}{"observer_name": "o", "rings": []map[string]interface{}{
			{"hops": 1, "nodes": []map[string]interface{}{unresolved("ee02"), {"hop": "7a", "pubkey": topoTarget}}}}}},
	}
	out := jsonOf(t, srv.filterBlacklistedFromTopology(data))
	if strings.Contains(out, topoTarget) {
		t.Errorf("target kept: %s", out)
	}
	for _, hop := range []string{"ee01", "ee02"} {
		if !strings.Contains(out, hop) {
			t.Errorf("unresolved hop %s dropped: %s", hop, out)
		}
	}
}

// A name the filter cannot read (not a string) cannot be checked against
// HiddenNamePrefixes, so the entry goes — fail closed, like pubkeys.
func TestFilterBlacklistedFromTopology_NonStringNameFailsClosed(t *testing.T) {
	srv, _ := computedTopology(t)
	data := map[string]interface{}{
		"topRepeaters": []map[string]interface{}{
			{"hop": "9d", "pubkey": topoOther, "name": map[string]interface{}{"n": "odd-name-marker"}},
			{"hop": "5b", "pubkey": topoLower, "name": "Lower"},
		},
	}
	out := jsonOf(t, srv.filterBlacklistedFromTopology(data))
	if strings.Contains(out, "odd-name-marker") {
		t.Errorf("entry with an unreadable name kept: %s", out)
	}
	if !strings.Contains(out, topoLower) {
		t.Errorf("readable entry dropped: %s", out)
	}
}
