package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
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
func newReachVisibilityDB(t *testing.T) *visFixture {
	t.Helper()
	f := &visFixture{
		n: pk64("01fa"), hidName: pk64("aabb"), blNode: pk64("ccdd"), blObs: pk64("ee11"),
		hidObsName: pk64("dd22"), visible: pk64("e5e5"),
		o1: pk64("0b01"), o2Hidden: pk64("0b02"), o3Blacklisted: pk64("0b03"),
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
	}
	for i, p := range paths {
		if _, err := conn.Exec(`INSERT INTO transmissions (id, from_pubkey, payload_type) VALUES (?, '', 5)`, i+1); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(`INSERT INTO observations (id, transmission_id, observer_idx, snr, path_json, timestamp) VALUES (?, ?, ?, -7.0, ?, ?)`, i+1, i+1, p.obs, p.path, now); err != nil {
			t.Fatal(err)
		}
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
	if got, want := linkSet(resp), strings.Join(sortedPrefixes(f.visible, f.o1, watcher), ","); got != want {
		t.Fatalf("links=%s want %s", got, want)
	}
	if got, want := obsSet(resp), strings.Join(sortedPrefixes(f.o1, watcher), ","); got != want {
		t.Fatalf("direct_observers=%s want %s", got, want)
	}
	if resp.Importance.BidirectionalLinks != 1 || resp.Importance.DirectObservers != 2 {
		t.Fatalf("counts=%+v want 1 two-way (Echo; Bravo removed) and 2 direct observers", resp.Importance)
	}
	for _, leak := range []string{f.hidName, f.blNode, f.blObs, f.hidObsName, f.o2Hidden, f.o3Blacklisted,
		"private A", "Bravo", "Charlie", "Delta", "hidden obs", "Obs three", "obs D"} {
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
	var got NodeReachResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Links) != len(resp.Links) || len(got.DirectObservers) != len(resp.DirectObservers) ||
		got.Importance != resp.Importance {
		t.Fatalf("unfiltered report changed: got %d links/%d obs %+v, want %d/%d %+v",
			len(got.Links), len(got.DirectObservers), got.Importance, len(resp.Links), len(resp.DirectObservers), resp.Importance)
	}
	if names, err := (&Server{cfg: &Config{}}).identityNames([]string{f.n}); names != nil || err != nil {
		t.Fatalf("identityNames without prefixes = %v, %v; want nil, nil and no DB access", names, err)
	}
}

// identityNames handles a large batch (a hub's full link list) in one query
// and maps upper-case observer ids back to lower-case pubkeys.
func TestIdentityNames_LargeBatch(t *testing.T) {
	f := newReachVisibilityDB(t)
	pks := make([]string, 0, 1010)
	for i := 0; i < 1000; i++ {
		pks = append(pks, pk64(fmt.Sprintf("f%05x", i)))
	}
	pks = append(pks, f.o2Hidden, strings.ToUpper(f.hidName), f.hidObsName)
	names, err := f.srv.identityNames(pks)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(names[f.o2Hidden]) != "[🚫 hidden obs]" || fmt.Sprint(names[f.hidName]) != "[🚫 private A]" {
		t.Fatalf("names=%v", names)
	}
	d := names[f.hidObsName]
	sort.Strings(d)
	if fmt.Sprint(d) != "[Delta 🚫 obs D]" {
		t.Fatalf("node + observer names for D = %v", d)
	}
}
