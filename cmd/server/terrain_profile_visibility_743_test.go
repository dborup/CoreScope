package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dhm"
)

// #743 — a profile's node endpoints go through the same identity-visibility
// policy as Reach (isIdentityHidden): an identity hidden only through an
// observer alias — an observer row for the same pubkey, in any case, whose
// name has a hidden prefix — is 404 like an unknown node, before any
// coordinate is read or any elevation fetched.

const terrain743Pos = "56.1629, 10.2039"

// terrain743Env has one positioned node under an ordinary name and an
// observer row for the same identity (upper-case id, as MQTT topics carry
// it) named obsName.
func terrain743Env(t *testing.T, obsName string) (*terrainEnv, *fakeElevation, string) {
	t.Helper()
	src := newFakeElevation(t, flatElev(0))
	env := newTerrainEnv(t, src, dhm.ProfileConfig{}, nil)
	pk := strings.Repeat("a", 64)
	env.srv.cfg.SetHiddenNamePrefixes([]string{"private-"})
	if _, err := env.srv.db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count)
		VALUES (?, 'Ordinary name', 'repeater', `+terrain743Pos+`, datetime('now'), datetime('now'), 1)`, pk); err != nil {
		t.Fatal(err)
	}
	if _, err := env.srv.db.conn.Exec(`INSERT INTO observers (id, name) VALUES (?, ?)`, strings.ToUpper(pk), obsName); err != nil {
		t.Fatal(err)
	}
	env.srv.store.InvalidateNodeCache()
	return env, src, pk
}

func TestTerrainProfile743_HiddenObserverAliasIs404(t *testing.T) {
	env, src, pk := terrain743Env(t, "private-observer")
	w := get(env.router, "/api/nodes/"+pk+"/reach")
	if w.Code != 404 {
		t.Fatalf("precondition: Reach status %d, want 404", w.Code)
	}
	b := offsetPt(56.1629, 10.2039, 4000, 0)
	for _, q := range []string{pathQuery(pk, b), pathQuery(b, pk), pathQuery(strings.ToUpper(pk), b)} {
		w := env.get(q + "&model=both")
		wantError(t, w, 404, "node not found")
		if body := w.Body.String(); strings.Contains(body, "56.1629") || strings.Contains(body, "10.2039") || strings.Contains(strings.ToLower(body), pk) {
			t.Fatalf("hidden identity leaked: %s", body)
		}
	}
	if n := src.calls.Load(); n != 0 {
		t.Fatalf("%d upstream fetches for a hidden identity, want 0", n)
	}
}

// A rename into a hidden prefix takes effect on the next request even when
// the profile itself is cached: visibility is decided before the cache.
func TestTerrainProfile743_AliasRenameHidesCachedProfile(t *testing.T) {
	env, src, pk := terrain743Env(t, "public-observer")
	q := pathQuery(pk, offsetPt(56.1629, 10.2039, 4000, 0))
	decodeProfile(t, env.get(q))
	before := src.calls.Load()
	if _, err := env.srv.db.conn.Exec(`UPDATE observers SET name = 'private-observer' WHERE id = ?`, strings.ToUpper(pk)); err != nil {
		t.Fatal(err)
	}
	wantError(t, env.get(q), 404, "node not found")
	if n := src.calls.Load(); n != before {
		t.Fatalf("%d upstream fetches after hiding, want %d", n, before)
	}
}

// A failed name lookup fails closed: the node is not served, and nothing is
// fetched upstream.
func TestTerrainProfile743_LookupFailureFailsClosed(t *testing.T) {
	env, src, pk := terrain743Env(t, "public-observer")
	if _, err := env.srv.db.conn.Exec(`DROP TABLE observers`); err != nil {
		t.Fatal(err)
	}
	wantError(t, env.get(pathQuery(pk, offsetPt(56.1629, 10.2039, 4000, 0))), 404, "node not found")
	if n := src.calls.Load(); n != 0 {
		t.Fatalf("%d upstream fetches after a failed lookup, want 0", n)
	}
	if !strings.Contains(env.logs.String(), "visibility") {
		t.Fatalf("failed lookup not logged: %q", env.logs.String())
	}
}

// Every node endpoint costs exactly one name lookup, whether the identity
// is visible, alias-hidden, blacklisted, unknown or unpositioned, so the
// work done does not tell a hidden identity apart from one that does not
// exist (as #410). The blacklisted key is configured and requested in mixed
// case: a blacklist hit must not short-circuit the lookup. Free points cost
// none.
func TestTerrainProfile743_OneLookupPerNodeEndpoint(t *testing.T) {
	env, _, pk := terrain743Env(t, "public-observer")
	hidden, unplaced := strings.Repeat("b", 64), strings.Repeat("c", 64)
	blacklisted := strings.Repeat("eE", 32)
	if _, err := env.srv.db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count)
		VALUES (?, 'x', 'repeater', 56.2, 10.2, datetime('now'), datetime('now'), 1),
		       (?, 'y', 'repeater', NULL, NULL, datetime('now'), datetime('now'), 1),
		       (?, 'z', 'repeater', 56.3, 10.3, datetime('now'), datetime('now'), 1)`, hidden, unplaced, strings.ToLower(blacklisted)); err != nil {
		t.Fatal(err)
	}
	env.srv.cfg.SetNodeBlacklist([]string{strings.ToUpper(blacklisted[:32]) + blacklisted[32:]})
	if _, err := env.srv.db.conn.Exec(`INSERT INTO observers (id, name) VALUES (?, 'private-o')`, hidden); err != nil {
		t.Fatal(err)
	}
	env.srv.store.InvalidateNodeCache()
	var mu sync.Mutex
	var looked [][]string
	env.srv.db.identityNamesQueryHook = func(keys []string) {
		mu.Lock()
		looked = append(looked, append([]string(nil), keys...))
		mu.Unlock()
	}
	b := offsetPt(56.1629, 10.2039, 4000, 0)
	for _, c := range []struct {
		pk   string
		code int
	}{{pk, 200}, {hidden, 404}, {blacklisted, 404}, {unplaced, 404}, {strings.Repeat("d", 64), 404}} {
		looked = nil
		if w := env.get(pathQuery(c.pk, b)); w.Code != c.code {
			t.Fatalf("%s…: status %d, want %d: %s", c.pk[:4], w.Code, c.code, w.Body.String())
		}
		if len(looked) != 1 || len(looked[0]) != 1 || looked[0][0] != strings.ToLower(c.pk) {
			t.Fatalf("%s…: name lookups %v, want exactly one for the endpoint", c.pk[:4], looked)
		}
	}
	looked = nil
	decodeProfile(t, env.get(pathQuery(aarhusPt, b)))
	if len(looked) != 0 {
		t.Fatalf("free points looked up names: %v", looked)
	}
}

// Perf proof: the visibility lookup is one indexed bulk query per node
// endpoint (at most two per request), and none for free points or when no
// hidden-name prefix is configured. Measured at a realistic scale — 2,000
// nodes, 500 observers — with a prefix configured, so the query really
// runs. Run with -v for the numbers. The timing check is informational
// only: a warm serve contains the lookup, so lookup < warm holds by
// construction and does not catch a cost regression. The guard against
// extra work is the lookup count in OneLookupPerNodeEndpoint.
func TestTerrainProfile743_VisibilityLookupCost(t *testing.T) {
	env, _, pk := terrain743Env(t, "public-observer")
	tx, err := env.srv.db.conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		if _, err := tx.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count)
			VALUES (?, ?, 'repeater', 56.0, 10.0, datetime('now'), datetime('now'), 1)`, fmt.Sprintf("%064x", i+1), fmt.Sprintf("n%d", i)); err != nil {
			t.Fatal(err)
		}
		if i < 500 {
			if _, err := tx.Exec(`INSERT INTO observers (id, name) VALUES (?, ?)`, fmt.Sprintf("%064X", i+1), fmt.Sprintf("o%d", i)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	env.srv.store.InvalidateNodeCache()
	q := pathQuery(pk, offsetPt(56.1629, 10.2039, 4000, 0))
	decodeProfile(t, env.get(q)) // fill the profile cache
	const runs = 200
	start := time.Now()
	for i := 0; i < runs; i++ {
		decodeProfile(t, env.get(q))
	}
	warm := time.Since(start) / runs
	start = time.Now()
	for i := 0; i < runs; i++ {
		if hidden, err := env.srv.isIdentityHidden(context.Background(), pk); err != nil || hidden {
			t.Fatalf("lookup: hidden=%v err=%v", hidden, err)
		}
	}
	lookup := time.Since(start) / runs
	if lookup >= warm {
		t.Fatalf("visibility lookup %v not below a warm serve %v", lookup, warm)
	}
	t.Logf("2,000 nodes, 500 observers: warm node-endpoint serve %v, visibility lookup %v (mean of %d)", warm, lookup, runs)
}
