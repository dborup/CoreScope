package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// Reach visibility: the target and every listed neighbour / direct observer
// follow identityHidden — node blacklist, observer blacklist, hidden node or
// observer name — checked live on every request, cached report or not.

type visFixture struct {
	db  *DB
	cfg *Config
	srv *Server
	// target and neighbours (pubkeys, lower-case)
	n, hidName, blNode, blObs, hidObsName, visible string
	// observer-only pubkeys (lower-case; stored upper-case in observers.id)
	o1, o2Hidden, o3Blacklisted string
	// hidden identities outside the plain nodes/observers tables
	inactiveHidden, mixedCaseObs string
}

// newReachVisibilityDB builds a Reach DB where target N has, in 30 days:
//
//	A  "🚫 private A"  — hidden by node name         (we hear A)
//	B  "Bravo"          — node-blacklisted             (bidirectional)
//	C  "Charlie"        — observer-blacklisted         (we hear C)
//	D  "Delta"          — visible node name, observer row "🚫 obs D" (D hears us)
//	E  "Echo"           — visible                      (bidirectional)
//	O1 "Obs one"        — observer-only, visible       (direct observer)
//	O2 "🚫 hidden obs"  — observer-only, hidden name   (direct observer)
//	O3 "Obs three"      — observer-only, observer-blacklisted (direct observer)
//	G  "🚫 gone quiet"  — only in inactive_nodes, hidden name (advert heard by N)
//	M  "Mixed case"     — observer-only, visible, id stored in mixed case (direct observer)
func newReachVisibilityDB(t *testing.T) *visFixture {
	t.Helper()
	f := &visFixture{
		n: pk64("01fa"), hidName: pk64("aabb"), blNode: pk64("ccdd"), blObs: pk64("ee11"),
		hidObsName: pk64("dd22"), visible: pk64("e5e5"),
		o1: pk64("0b01"), o2Hidden: pk64("0b02"), o3Blacklisted: pk64("0b03"),
		inactiveHidden: pk64("7777"), mixedCaseObs: pk64("0b0abc"),
	}
	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1) // one shared :memory: database
	t.Cleanup(func() { conn.Close() })
	up := strings.ToUpper
	now := time.Now().Unix()
	stmts := []string{
		`CREATE TABLE nodes (public_key TEXT PRIMARY KEY, name TEXT, role TEXT, lat REAL, lon REAL, last_seen TEXT, first_seen TEXT, advert_count INTEGER DEFAULT 0, battery_mv INTEGER, temperature_c REAL, foreign_advert INTEGER DEFAULT 0)`,
		`CREATE TABLE observers (id TEXT PRIMARY KEY, name TEXT)`,
		`CREATE TABLE transmissions (id INTEGER PRIMARY KEY, from_pubkey TEXT, payload_type INTEGER)`,
		`CREATE TABLE observations (id INTEGER PRIMARY KEY, transmission_id INTEGER, observer_idx INTEGER, snr REAL, path_json TEXT, timestamp INTEGER)`,
		`CREATE TABLE neighbor_edges (node_a TEXT NOT NULL, node_b TEXT NOT NULL, count INTEGER DEFAULT 1, last_seen TEXT, PRIMARY KEY (node_a, node_b))`,
		`CREATE TABLE inactive_nodes (public_key TEXT PRIMARY KEY, name TEXT, role TEXT, lat REAL, lon REAL, last_seen TEXT, first_seen TEXT, advert_count INTEGER DEFAULT 0)`,
	}
	for _, s := range stmts {
		if _, err := conn.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []struct{ pk, name string }{
		{f.n, "Target"}, {f.hidName, "🚫 private A"}, {f.blNode, "Bravo"}, {f.blObs, "Charlie"},
		{f.hidObsName, "Delta"}, {f.visible, "Echo"},
	} {
		if _, err := conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count) VALUES (?, ?, 'repeater', 56.1, 10.2, '2026-09-01T00:00:00Z', '2026-06-01T00:00:00Z', 1)`, n.pk, n.name); err != nil {
			t.Fatal(err)
		}
	}
	// observers rowid 1..5 (observations reference them by rowid)
	for _, o := range []struct{ id, name string }{
		{up(f.o1), "Obs one"}, {up(f.o2Hidden), "🚫 hidden obs"}, {up(f.o3Blacklisted), "Obs three"},
		{up(f.hidObsName), "🚫 obs D"}, {up(pk64("0b05")), "Relay watcher"},
		{"0B0aBC" + f.mixedCaseObs[6:], "Mixed case"}, // rowid 6, raw mixed-case id
	} {
		if _, err := conn.Exec(`INSERT INTO observers (id, name) VALUES (?, ?)`, o.id, o.name); err != nil {
			t.Fatal(err)
		}
	}
	paths := []struct {
		path string
		obs  int
	}{
		{`["AABB","01FA","CCDD"]`, 5}, // we hear A, B hears us
		{`["CCDD","01FA"]`, 5},        // we hear B → B bidirectional; watcher hears us
		{`["EE11","01FA","DD22"]`, 5}, // we hear C, D hears us
		{`["E5E5","01FA","E5E5"]`, 5}, // E both ways
		{`["01FA"]`, 1},               // O1 heard us directly
		{`["01FA"]`, 2},               // O2 heard us directly
		{`["01FA"]`, 3},               // O3 heard us directly
		{`["01FA"]`, 6},               // M (mixed-case id) heard us directly
	}
	for i, p := range paths {
		if _, err := conn.Exec(`INSERT INTO transmissions (id, from_pubkey, payload_type) VALUES (?, '', 5)`, i+1); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(`INSERT INTO observations (id, transmission_id, observer_idx, snr, path_json, timestamp) VALUES (?, ?, ?, -7.0, ?, ?)`, i+1, i+1, p.obs, p.path, now); err != nil {
			t.Fatal(err)
		}
	}
	// G aged out of `nodes` into inactive_nodes; its advert, relayed first by
	// N, is still inside the 30-day window ("we hear G").
	if _, err := conn.Exec(`INSERT INTO inactive_nodes (public_key, name, role) VALUES (?, '🚫 gone quiet', 'repeater')`, f.inactiveHidden); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO transmissions (id, from_pubkey, payload_type) VALUES (100, ?, ?)`, f.inactiveHidden, PayloadADVERT); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO observations (id, transmission_id, observer_idx, snr, path_json, timestamp) VALUES (100, 100, 5, -7.0, '["01FA"]', ?)`, now-10*86400); err != nil {
		t.Fatal(err)
	}
	f.db = &DB{conn: conn}
	f.db.isV3Flag.forceTrue()
	f.cfg = &Config{
		NodeBlacklist:      []string{f.blNode},
		ObserverBlacklist:  []string{up(f.blObs), f.o3Blacklisted},
		HiddenNamePrefixes: []string{"🚫"},
	}
	f.srv = &Server{store: newTestStoreWithDB(t, f.db, f.cfg), db: f.db, cfg: f.cfg, perfStats: NewPerfStats()}
	resetReachState(t, f.srv)
	return f
}

func (f *visFixture) get(t *testing.T, pk string) (*http.Response, NodeReachResponse, string) {
	t.Helper()
	rr := serveReach(f.srv, "/api/nodes/"+pk+"/reach?days=30")
	var resp NodeReachResponse
	if rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("bad json: %v", err)
		}
	}
	return rr.Result(), resp, rr.Body.String()
}

func linkSet(resp NodeReachResponse) string {
	var out []string
	for _, l := range resp.Links {
		out = append(out, l.Pubkey[:4])
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func obsSet(resp NodeReachResponse) string {
	var out []string
	for _, o := range resp.DirectObservers {
		out = append(out, o.Pubkey[:4])
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// The reviewer's target cases: every hidden or blacklisted identity 404s with
// the same body as an unknown node, including observer-only pubkeys and
// hiding by observer name.
func TestNodeReach_HiddenTargets404(t *testing.T) {
	f := newReachVisibilityDB(t)
	unknown := serveReach(f.srv, "/api/nodes/"+pk64("beef")+"/reach?days=30")
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown node: %d", unknown.Code)
	}
	for name, pk := range map[string]string{
		"hidden by node name":                     f.hidName,
		"node-blacklisted":                        f.blNode,
		"observer-blacklisted (has a node row)":   f.blObs,
		"hidden by observer name only":            f.hidObsName,
		"observer-only, hidden name":              f.o2Hidden,
		"observer-only, observer-blacklisted":     f.o3Blacklisted,
		"upper-case form of a hidden observer id": strings.ToUpper(f.o2Hidden),
	} {
		res, _, body := f.get(t, pk)
		if res.StatusCode != http.StatusNotFound || body != unknown.Body.String() {
			t.Errorf("%s: status=%d body=%q, want the unknown-node 404", name, res.StatusCode, body)
		}
	}
	for name, pk := range map[string]string{"visible node": f.n, "visible observer-only": f.o1} {
		if res, _, body := f.get(t, pk); res.StatusCode != http.StatusOK {
			t.Errorf("%s: status=%d body=%s, want 200", name, res.StatusCode, body)
		}
	}
}

// A visible node's Reach page never lists a hidden or blacklisted identity —
// not in links, not in direct_observers, not by name or pubkey anywhere in the
// body — and the counts derived from those lists match what is shown.
func TestNodeReach_HiddenNeighboursFiltered(t *testing.T) {
	f := newReachVisibilityDB(t)
	res, resp, body := f.get(t, f.n)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.StatusCode, body)
	}
	watcher := pk64("0b05")
	if got, want := linkSet(resp), strings.Join(sortedPrefixes(f.visible, f.o1, watcher, f.mixedCaseObs), ","); got != want {
		t.Fatalf("links=%s want %s", got, want)
	}
	if got, want := obsSet(resp), strings.Join(sortedPrefixes(f.o1, watcher, f.mixedCaseObs), ","); got != want {
		t.Fatalf("direct_observers=%s want %s", got, want)
	}
	if resp.Importance.BidirectionalLinks != 1 || resp.Importance.DirectObservers != 3 {
		t.Fatalf("counts=%+v want 1 two-way (Echo; Bravo removed) and 3 direct observers", resp.Importance)
	}
	for _, leak := range []string{f.hidName, f.blNode, f.blObs, f.hidObsName, f.o2Hidden, f.o3Blacklisted,
		f.inactiveHidden, "private A", "Bravo", "Charlie", "Delta", "hidden obs", "Obs three",
		"obs D", "gone quiet"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(leak)) {
			t.Errorf("body leaks %q", leak)
		}
	}
}

func sortedPrefixes(pks ...string) []string {
	out := make([]string, len(pks))
	for i, pk := range pks {
		out[i] = pk[:4]
	}
	sort.Strings(out)
	return out
}

// A warm cache never outlives a visibility change: renames (node or observer
// row), hidden-prefix config and blacklist changes apply on the very next
// request, in both directions, for neighbours and for the target itself.
func TestNodeReach_WarmCacheHonoursVisibilityChanges(t *testing.T) {
	f := newReachVisibilityDB(t)
	exec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := f.db.conn.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	has := func(resp NodeReachResponse, pk string) bool {
		return strings.Contains(","+linkSet(resp)+",", ","+pk[:4]+",")
	}
	_, warm, _ := f.get(t, f.n)
	if !has(warm, f.visible) || f.srv.reachCacheLen() == 0 {
		t.Fatalf("warm-up: links=%s cache=%d", linkSet(warm), f.srv.reachCacheLen())
	}

	// Neighbour renamed into a hidden prefix → gone on the next (cached) request.
	exec(`UPDATE nodes SET name = '🚫 Echo' WHERE public_key = ?`, f.visible)
	_, resp, body := f.get(t, f.n)
	if has(resp, f.visible) || strings.Contains(body, f.visible) || resp.Importance.BidirectionalLinks != 0 {
		t.Fatalf("renamed-hidden neighbour still served: links=%s two-way=%d", linkSet(resp), resp.Importance.BidirectionalLinks)
	}
	if res, _, _ := f.get(t, f.visible); res.StatusCode != http.StatusNotFound {
		t.Fatalf("renamed-hidden node's own reach: %d", res.StatusCode)
	}
	// ...and back.
	exec(`UPDATE nodes SET name = 'Echo' WHERE public_key = ?`, f.visible)
	if _, resp, _ := f.get(t, f.n); !has(resp, f.visible) {
		t.Fatalf("un-hidden neighbour not restored: %s", linkSet(resp))
	}

	// Observer row renamed into a hidden prefix.
	exec(`UPDATE observers SET name = '🚫 now hidden' WHERE id = ?`, strings.ToUpper(f.o1))
	if _, resp, _ := f.get(t, f.n); has(resp, f.o1) || strings.Contains(obsSet(resp), f.o1[:4]) {
		t.Fatalf("renamed-hidden observer still served: links=%s obs=%s", linkSet(resp), obsSet(resp))
	}
	exec(`UPDATE observers SET name = 'Obs one' WHERE id = ?`, strings.ToUpper(f.o1))

	// Observer whose id is stored in mixed case, renamed into a hidden prefix.
	exec(`UPDATE observers SET name = '🚫 mixed now hidden' WHERE lower(id) = ?`, f.mixedCaseObs)
	if _, resp, _ := f.get(t, f.n); has(resp, f.mixedCaseObs) {
		t.Fatalf("renamed-hidden mixed-case observer still served: %s", linkSet(resp))
	}
	if res, _, _ := f.get(t, f.mixedCaseObs); res.StatusCode != http.StatusNotFound {
		t.Fatalf("renamed-hidden mixed-case observer's own reach: %d", res.StatusCode)
	}

	// Hidden-prefix config change (does not purge the cache).
	f.cfg.SetHiddenNamePrefixes([]string{"🚫", "Obs one"})
	if _, resp, _ := f.get(t, f.n); has(resp, f.o1) {
		t.Fatalf("prefix change ignored by the cache: %s", linkSet(resp))
	}
	f.cfg.SetHiddenNamePrefixes([]string{"🚫"})
	if _, resp, _ := f.get(t, f.n); !has(resp, f.o1) {
		t.Fatalf("prefix removal ignored: %s", linkSet(resp))
	}

	// Blacklist change.
	f.cfg.SetNodeBlacklist([]string{f.blNode, f.visible})
	if _, resp, _ := f.get(t, f.n); has(resp, f.visible) {
		t.Fatalf("blacklist change ignored: %s", linkSet(resp))
	}

	// The target itself hidden after its report was cached.
	_, _, _ = f.get(t, f.n)
	exec(`UPDATE nodes SET name = '🚫 Target' WHERE public_key = ?`, f.n)
	if res, _, _ := f.get(t, f.n); res.StatusCode != http.StatusNotFound {
		t.Fatalf("target renamed hidden after warm-up: %d, want 404", res.StatusCode)
	}
}

// A failed live-name lookup never serves unfiltered data from the cache.
func TestNodeReach_NameLookupFailureFailsClosed(t *testing.T) {
	f := newReachVisibilityDB(t)
	if res, _, _ := f.get(t, f.n); res.StatusCode != http.StatusOK {
		t.Fatalf("warm-up: %d", res.StatusCode)
	}
	if _, err := f.db.conn.Exec(`ALTER TABLE observers RENAME TO observers_gone`); err != nil {
		t.Fatal(err)
	}
	res, _, body := f.get(t, f.n)
	if res.StatusCode != http.StatusInternalServerError || strings.Contains(body, "links") {
		t.Fatalf("status=%d body=%s, want 500 without report data", res.StatusCode, body)
	}
}

// With nothing hidden the body is exactly the computed report, and without
// configured prefixes no name lookup touches the DB.
func TestNodeReach_NothingHiddenBodyUnchanged(t *testing.T) {
	f := newReachVisibilityDB(t)
	f.cfg.SetNodeBlacklist(nil)
	f.cfg.ObserverBlacklist = nil
	f.cfg.SetHiddenNamePrefixes(nil)
	resetReachState(t, f.srv)

	res, _, body := f.get(t, f.n)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", res.StatusCode)
	}
	resp, ok, err := f.srv.computeNodeReach(context.Background(), f.n, 30)
	if err != nil || !ok {
		t.Fatalf("compute: ok=%v err=%v", ok, err)
	}
	// computeNodeReach itself leaves the rank fields zero — the handler
	// applies them from the shared rank view at serve time (applyReachRank),
	// same as visibleReach does for the HTTP body compared below.
	applyReachRank(&resp.Importance, resp.Node.Pubkey, f.srv.currentReachRankView(context.Background()))
	var got NodeReachResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	// Full comparison (not just counts). Link order among equal-scored links
	// is not specified, so compare as sorted lists; round-trip resp through
	// JSON so pointer fields compare by value.
	var want NodeReachResponse
	raw, _ := json.Marshal(resp)
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	sortReport := func(r *NodeReachResponse) {
		sort.Slice(r.Links, func(i, j int) bool { return r.Links[i].Pubkey < r.Links[j].Pubkey })
		sort.Slice(r.DirectObservers, func(i, j int) bool { return r.DirectObservers[i].Pubkey < r.DirectObservers[j].Pubkey })
	}
	sortReport(&got)
	sortReport(&want)
	want.Window.Since = got.Window.Since // computed a moment apart
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unfiltered report changed:\n got %+v\nwant %+v", got, want)
	}
	if names, err := (&Server{cfg: &Config{}}).hiddenIdentityNames(context.Background(), []string{f.n}); names != nil || err != nil {
		t.Fatalf("hiddenIdentityNames without prefixes = %v, %v; want nil, nil and no DB access", names, err)
	}
}

// hiddenIdentityNames handles a large batch (a hub's full link list) in one
// query, returns only names that start with a hidden prefix, and maps
// upper-case observer ids back to lower-case pubkeys.
func TestHiddenIdentityNames_LargeBatchOnlyHidingNames(t *testing.T) {
	f := newReachVisibilityDB(t)
	pks := make([]string, 0, 1010)
	for i := 0; i < 1000; i++ {
		pks = append(pks, pk64(fmt.Sprintf("f%05x", i)))
	}
	pks = append(pks, f.o2Hidden, strings.ToUpper(f.hidName), f.hidObsName, f.visible, f.o1, f.inactiveHidden)
	names, err := f.srv.hiddenIdentityNames(context.Background(), pks)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		f.o2Hidden:       "[🚫 hidden obs]",
		f.hidName:        "[🚫 private A]",
		f.hidObsName:     "[🚫 obs D]", // its visible node name "Delta" is not returned
		f.inactiveHidden: "[🚫 gone quiet]",
	}
	if len(names) != len(want) {
		t.Fatalf("names=%v, want only the hiding names %v", names, want)
	}
	for pk, w := range want {
		if fmt.Sprint(names[pk]) != w {
			t.Fatalf("names[%s]=%v want %s (all=%v)", pk[:4], names[pk], w, names)
		}
	}
}

// The SQL prefix test is byte-exact strings.HasPrefix, as IsNameHidden.
func TestHiddenIdentityNames_PrefixMatchIsExact(t *testing.T) {
	f := newReachVisibilityDB(t)
	cases := []struct {
		name string
		hide bool
	}{
		{"Ab visible", false}, // case differs from prefix "AB"
		{"AB", true},          // exactly the prefix
		{"A", false},          // shorter than the prefix
		{"AB side", true},
		{"x AB", false}, // prefix not at the start
		{"🚫", true},     // multi-byte prefix
		{"🚫x", true},
		{"\U0001F6ABx", true}, // same emoji, escaped
	}
	f.cfg.SetHiddenNamePrefixes([]string{"AB", "🚫"})
	pks := make([]string, len(cases))
	for i, c := range cases {
		pks[i] = pk64(fmt.Sprintf("e0%02x", i))
		if _, err := f.db.conn.Exec(`INSERT INTO nodes (public_key, name, role) VALUES (?, ?, 'repeater')`, pks[i], c.name); err != nil {
			t.Fatal(err)
		}
	}
	names, err := f.srv.hiddenIdentityNames(context.Background(), pks)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		if got := len(names[pks[i]]) > 0; got != c.hide || got != f.cfg.IsNameHidden(c.name) {
			t.Errorf("%q: SQL hiding=%v, IsNameHidden=%v, want %v", c.name, got, f.cfg.IsNameHidden(c.name), c.hide)
		}
	}
}

// reachCacheSetBody never attaches a body to an entry computed at another
// time: a body built from an older report must not be reused for a newer one.
// Covers both freshness keys it carries: hiddenKey (visibility) and viewID
// (rank).
func TestReachCacheSetBody_StaleEntryUntouched(t *testing.T) {
	srv := &Server{}
	resetReachState(t, srv)
	old := time.Now().Add(-time.Minute)
	cur := reachCacheEntry{at: time.Now(), raw: []byte(`{"current":true}`), hiddenKey: "", viewID: 1, found: true}
	srv.reachCachePut("k", cur)
	srv.reachCacheSetBody("k", old, []byte(`{"stale":true}`), "x", 2)
	got, ok := srv.reachCacheGet("k")
	if !ok || string(got.raw) != `{"current":true}` || got.hiddenKey != "" || got.viewID != 1 {
		t.Fatalf("stale body attached to a newer entry: %s %q view=%d", got.raw, got.hiddenKey, got.viewID)
	}
	srv.reachCacheSetBody("k", cur.at, []byte(`{"refreshed":true}`), "y", 3)
	if got, _ := srv.reachCacheGet("k"); string(got.raw) != `{"refreshed":true}` || got.hiddenKey != "y" || got.viewID != 3 {
		t.Fatalf("matching entry not refreshed: %s %q view=%d", got.raw, got.hiddenKey, got.viewID)
	}
}

// A neighbour hidden when the report was computed stays hidden after its row
// disappears from the DB: the name recorded in the cached report still counts.
func TestNodeReach_RecordedNameStillHidesAfterRowDeleted(t *testing.T) {
	f := newReachVisibilityDB(t)
	if _, resp, _ := f.get(t, f.n); strings.Contains(linkSet(resp), f.hidName[:4]) {
		t.Fatalf("warm-up already leaks %s", f.hidName[:4])
	}
	if _, err := f.db.conn.Exec(`DELETE FROM nodes WHERE public_key = ?`, f.hidName); err != nil {
		t.Fatal(err)
	}
	if _, resp, body := f.get(t, f.n); strings.Contains(linkSet(resp), f.hidName[:4]) || strings.Contains(body, "private A") {
		t.Fatalf("deleted hidden neighbour served from the cache: %s", linkSet(resp))
	}
}

// A whitespace-only prefix is honoured by IsNameHidden, so it must also turn
// on the live name lookup.
func TestNodeReach_WhitespacePrefixStillLooksUpNames(t *testing.T) {
	f := newReachVisibilityDB(t)
	f.get(t, f.n) // warm
	f.cfg.SetHiddenNamePrefixes([]string{" "})
	if _, err := f.db.conn.Exec(`UPDATE nodes SET name = ' sneaky' WHERE public_key = ?`, f.visible); err != nil {
		t.Fatal(err)
	}
	if _, resp, _ := f.get(t, f.n); strings.Contains(linkSet(resp), f.visible[:4]) {
		t.Fatalf("whitespace-prefixed rename not hidden: %s", linkSet(resp))
	}
	if len(f.cfg.EnforcedHiddenNamePrefixes()) != 1 || (&Config{HiddenNamePrefixes: []string{""}}).EnforcedHiddenNamePrefixes() != nil {
		t.Fatalf("EnforcedHiddenNamePrefixes must mirror IsNameHidden: only empty prefixes are inert")
	}
}

// Minimal DBs without inactive_nodes still work (the table is probed).
func TestNodeReach_NoInactiveNodesTable(t *testing.T) {
	f := newReachVisibilityDB(t)
	if _, err := f.db.conn.Exec(`DROP TABLE inactive_nodes`); err != nil {
		t.Fatal(err)
	}
	res, resp, body := f.get(t, f.n)
	if res.StatusCode != http.StatusOK || strings.Contains(body, "private A") || !strings.Contains(linkSet(resp), f.visible[:4]) {
		t.Fatalf("status=%d links=%s", res.StatusCode, linkSet(resp))
	}
}

// inactive_nodes keeps a returning node's old row. A node that went quiet as
// "🚫 …" and came back under a visible name is visible again: an inactive
// name only counts while the pubkey has no nodes row.
func TestNodeReach_StaleInactiveNameDoesNotHideReturnedNode(t *testing.T) {
	f := newReachVisibilityDB(t)
	if _, err := f.db.conn.Exec(`INSERT INTO inactive_nodes (public_key, name, role) VALUES (?, '🚫 old name', 'repeater')`, f.visible); err != nil {
		t.Fatal(err)
	}
	if res, _, _ := f.get(t, f.visible); res.StatusCode != http.StatusOK {
		t.Fatalf("returned node's own reach: %d, want 200", res.StatusCode)
	}
	if _, resp, _ := f.get(t, f.n); !strings.Contains(linkSet(resp), f.visible[:4]) {
		t.Fatalf("returned node hidden from links by a stale inactive name: %s", linkSet(resp))
	}
	// A nameless return (the ingestor stores '') does not supersede the old
	// hidden name: the node stays hidden.
	if _, err := f.db.conn.Exec(`UPDATE nodes SET name = '' WHERE public_key = ?`, f.visible); err != nil {
		t.Fatal(err)
	}
	if _, resp, _ := f.get(t, f.n); strings.Contains(linkSet(resp), f.visible[:4]) {
		t.Fatalf("nameless return un-hid a hidden inactive name: %s", linkSet(resp))
	}
	// (The inactive-only case — no nodes row, hidden inactive name — is
	// fixture node G, checked in TestNodeReach_HiddenNeighboursFiltered.)
}

// Over-filtering controls: names that merely contain a hidden prefix, and a
// node visible under both its node and observer name, stay listed and keep
// their own Reach page.
func TestNodeReach_VisibleControlsNotOverfiltered(t *testing.T) {
	f := newReachVisibilityDB(t)
	midName, dual := pk64("5151"), pk64("6262")
	now := time.Now().Unix()
	for _, q := range []struct {
		sql  string
		args []interface{}
	}{
		{`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count) VALUES (?, 'Relay 🚫 mid-name', 'repeater', 56.1, 10.2, '2026-09-01T00:00:00Z', '2026-06-01T00:00:00Z', 1)`, []interface{}{midName}},
		{`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count) VALUES (?, 'Dual', 'repeater', 56.1, 10.2, '2026-09-01T00:00:00Z', '2026-06-01T00:00:00Z', 1)`, []interface{}{dual}},
		{`INSERT INTO observers (id, name) VALUES (?, 'Dual obs')`, []interface{}{strings.ToUpper(dual)}},
		{`INSERT INTO transmissions (id, from_pubkey, payload_type) VALUES (200, '', 5), (201, '', 5)`, nil},
		{`INSERT INTO observations (id, transmission_id, observer_idx, snr, path_json, timestamp) VALUES (200, 200, 5, -7.0, '["5151","01FA"]', ?), (201, 201, 5, -7.0, '["01FA","6262"]', ?)`, []interface{}{now, now}},
	} {
		if _, err := f.db.conn.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	_, resp, _ := f.get(t, f.n)
	for name, pk := range map[string]string{"mid-name 🚫": midName, "dual visible": dual, "visible node": f.visible, "visible observer": f.o1} {
		if !strings.Contains(linkSet(resp), pk[:4]) {
			t.Errorf("%s (%s) over-filtered: links=%s", name, pk[:4], linkSet(resp))
		}
	}
	for _, pk := range []string{midName, dual} {
		if res, _, body := f.get(t, pk); res.StatusCode != http.StatusOK {
			t.Errorf("%s own reach: %d %s", pk[:4], res.StatusCode, body)
		}
	}
}

// Cache: a hit reuses the computed report; a hidden-prefix change is applied
// to that same cached report at serve time (no recompute, no bypass); a
// blacklist change purges and recomputes.
func TestNodeReach_CacheHitMissAndInvalidation(t *testing.T) {
	f := newReachVisibilityDB(t)
	key := func() string {
		return f.n + "|30|g" + strconv.FormatUint(f.cfg.BlacklistGeneration(), 10)
	}
	_, first, body1 := f.get(t, f.n) // miss → computed and cached
	e1, ok := f.srv.reachCacheGet(key())
	if !ok || !strings.Contains(linkSet(first), f.visible[:4]) {
		t.Fatalf("miss did not cache (ok=%v) or links wrong: %s", ok, linkSet(first))
	}
	_, _, body2 := f.get(t, f.n) // hit
	if e2, _ := f.srv.reachCacheGet(key()); !e2.at.Equal(e1.at) || body2 != body1 {
		t.Fatalf("second request was not a cache hit with the same body")
	}

	f.cfg.SetHiddenNamePrefixes([]string{"🚫", "Echo"})
	_, resp, _ := f.get(t, f.n)
	e3, ok := f.srv.reachCacheGet(key())
	if !ok || !e3.at.Equal(e1.at) {
		t.Fatalf("prefix change must filter the cached report at serve time, not recompute (ok=%v)", ok)
	}
	if strings.Contains(linkSet(resp), f.visible[:4]) || !strings.Contains(e3.hiddenKey, f.visible) {
		t.Fatalf("cached report served unfiltered after prefix change: links=%s hiddenKey=%q", linkSet(resp), e3.hiddenKey)
	}
	f.cfg.SetHiddenNamePrefixes([]string{"🚫"})

	genBefore := f.cfg.BlacklistGeneration()
	f.cfg.SetNodeBlacklist([]string{f.blNode, f.o1})
	_, resp, _ = f.get(t, f.n)
	e4, ok := f.srv.reachCacheGet(key())
	if !ok || f.cfg.BlacklistGeneration() == genBefore || e4.at.Equal(e1.at) || f.srv.reachCacheLen() != 1 {
		t.Fatalf("blacklist change must purge and recompute (ok=%v, len=%d)", ok, f.srv.reachCacheLen())
	}
	if strings.Contains(linkSet(resp), f.o1[:4]) || strings.Contains(obsSet(resp), f.o1[:4]) {
		t.Fatalf("newly blacklisted observer still served: links=%s obs=%s", linkSet(resp), obsSet(resp))
	}
}

// The filtering does not touch the numeric neighbour count: NeighborDegree
// counts every valid edge — including ones to a hidden or blacklisted
// neighbour — the same with or without hiding configured (it is a count, not
// an identity, so it does not leak who the neighbour is). DegreeRank and
// NodesWithEdges, by contrast, ARE affected by hiding: they are computed over
// the visible (ranked) population, so a hidden or blacklisted node never
// itself occupies a placement or is counted in the total (the agreed "visible
// population" contract — see reach_rank.go and the PR description).
func TestNodeReach_FilteringLeavesRankFieldsUnchanged(t *testing.T) {
	f := newReachVisibilityDB(t)
	// n–hidName, n–visible, n–blNode, visible–hidName: n has 3 neighbours;
	// hidName and blNode are each hidden from ranking one way (name prefix,
	// node blacklist) with the fixture's own config.
	for _, pair := range [][2]string{{f.n, f.hidName}, {f.n, f.visible}, {f.n, f.blNode}, {f.visible, f.hidName}} {
		a, b := pair[0], pair[1]
		if a > b {
			a, b = b, a
		}
		if _, err := f.db.conn.Exec(`INSERT INTO neighbor_edges (node_a, node_b) VALUES (?, ?)`, a, b); err != nil {
			t.Fatal(err)
		}
	}
	_, hiddenCfg, _ := f.get(t, f.n)
	plain := &Config{}
	srv := &Server{store: newTestStoreWithDB(t, f.db, plain), db: f.db, cfg: plain, perfStats: NewPerfStats()}
	resetReachState(t, srv)
	rr := serveReach(srv, "/api/nodes/"+f.n+"/reach?days=30")
	var noHiding NodeReachResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &noHiding); err != nil {
		t.Fatal(err)
	}
	h, p := hiddenCfg.Importance, noHiding.Importance
	if h.NeighborDegree != 3 || p.NeighborDegree != 3 {
		t.Fatalf("NeighborDegree changed by filtering: with hiding %d, without %d, want 3 both (hidden neighbours still counted)", h.NeighborDegree, p.NeighborDegree)
	}
	// With hiding: only n and visible are ranked (hidName is name-hidden,
	// blNode is blacklisted) — total 2. Without hiding: all four edge
	// endpoints are ranked — total 4.
	if h.NodesWithEdges != 2 {
		t.Fatalf("NodesWithEdges with hiding = %d, want 2 (hidden/blacklisted nodes excluded from the ranked population)", h.NodesWithEdges)
	}
	if p.NodesWithEdges != 4 {
		t.Fatalf("NodesWithEdges without hiding = %d, want 4 (nothing excluded)", p.NodesWithEdges)
	}
	// n has the highest degree (3) in both populations, so removing the two
	// lower-degree nodes from the ranking does not change n's own placement.
	if h.DegreeRank != 1 || p.DegreeRank != 1 {
		t.Fatalf("DegreeRank changed unexpectedly: with hiding %d, without %d, want 1 both (n is always the top neighbour count)", h.DegreeRank, p.DegreeRank)
	}
}

// Cross-endpoint integration: a hidden neighbour of a visible node (a) never
// appears in that node's Reach links, (b) never appears in the leaderboard's
// rows or search — by pubkey or by name — yet (c) is still counted in the
// visible node's own NeighborDegree, and the leaderboard's total reflects
// only the visible population. Exercises /api/nodes/{pk}/reach and
// /api/reach-rank together, on the same fixture config (hidName is
// name-hidden, blNode is node-blacklisted).
func TestNodeReach_HiddenNeighbourCountedNotListedOrRanked(t *testing.T) {
	f := newReachVisibilityDB(t)
	for _, pair := range [][2]string{{f.n, f.hidName}, {f.n, f.visible}, {f.n, f.blNode}} {
		a, b := pair[0], pair[1]
		if a > b {
			a, b = b, a
		}
		if _, err := f.db.conn.Exec(`INSERT INTO neighbor_edges (node_a, node_b) VALUES (?, ?)`, a, b); err != nil {
			t.Fatal(err)
		}
	}

	// (a) + (c): n's own Reach page.
	_, resp, body := f.get(t, f.n)
	for _, l := range resp.Links {
		if l.Pubkey == f.hidName || l.Pubkey == f.blNode {
			t.Fatalf("hidden/blacklisted neighbour listed in links: %+v", l)
		}
	}
	for _, leak := range []string{f.hidName, f.blNode, "🚫 private A", "Bravo"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(leak)) {
			t.Fatalf("hidden identity %q leaked into n's Reach body: %s", leak, body)
		}
	}
	if resp.Importance.NeighborDegree != 3 {
		t.Fatalf("n's NeighborDegree = %d, want 3 (counts hidden/blacklisted neighbours numerically)", resp.Importance.NeighborDegree)
	}

	// (b): the leaderboard.
	lb := getRank(t, f.srv, "/api/reach-rank")
	for _, row := range lb.Rows {
		if row.Pubkey == f.hidName || row.Pubkey == f.blNode {
			t.Fatalf("hidden/blacklisted node placed on the leaderboard: %+v", row)
		}
	}
	// n and visible are the only ranked pubkeys from this edge set (hidName,
	// blNode are excluded); n is #1 with 3 neighbours.
	if lb.Total != 2 {
		t.Fatalf("leaderboard total = %d, want 2", lb.Total)
	}
	if len(lb.Rows) == 0 || lb.Rows[0].Pubkey != f.n || lb.Rows[0].Neighbors != 3 {
		t.Fatalf("leaderboard top row = %+v, want n with 3 neighbours", lb.Rows)
	}
	for _, q := range []string{f.hidName, f.hidName[:10], "private", "🚫", f.blNode, f.blNode[:10], "bravo"} {
		if r := getRank(t, f.srv, "/api/reach-rank?q="+q); r.Matched != 0 {
			t.Fatalf("search %q found %d hidden/blacklisted rows, want 0: %+v", q, r.Matched, r.Rows)
		}
	}

	// Cross-check: the hidden node's own Reach page is still 404.
	if rr := serveRankRoutes(f.srv, "/api/nodes/"+f.hidName+"/reach?days=30"); rr.Code != http.StatusNotFound {
		t.Fatalf("hidden neighbour's own reach page: %d want 404", rr.Code)
	}
}
