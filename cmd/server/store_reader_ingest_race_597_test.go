package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/gorilla/mux"
)

// Issue #597: audit of the store's readers against upstream's lock fixes
// (StoreTx state read under s.mu; region and area sets resolved before it).
// The #470 tests pin the readers that were found racing one by one. This test
// sweeps every GET /api route the router registers, so a reader added later
// is covered without anyone listing it: each route is requested while one
// writer applies observations (Observations append, header rewrite) and the
// content-hash migration (Hash, observation TransmissionID, RouteType) under
// s.mu.Lock, exactly as the #470 helpers do. CI runs this package with -race,
// which reports any reader that reads that state without s.mu.

// raceSweepPath597 fills a route template's variables with fixture ids.
// {hash} and the packet route's {id} take hash, a transmission the writer
// never rehashes, so those routes find it for the whole run instead of
// answering 404 before they reach the store. Other {id} routes are observers.
func raceSweepPath597(tpl, hash string) string {
	id := "obs1"
	if strings.HasPrefix(tpl, "/api/packets/{id}") {
		id = hash
	}
	return strings.NewReplacer(
		"{pubkey}", "aabbccdd11223344",
		"{hash}", hash,
		"{id}", id,
		"{requestId}", "1",
	).Replace(tpl)
}

// apiGETRoutes597 returns the path templates of every GET /api route.
func apiGETRoutes597(t *testing.T, router *mux.Router) []string {
	t.Helper()
	seen := map[string]bool{}
	err := router.Walk(func(route *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		tpl, err := route.GetPathTemplate()
		if err != nil || !strings.HasPrefix(tpl, "/api/") {
			return nil
		}
		methods, err := route.GetMethods()
		if err != nil { // no method matcher: serves GET too
			seen[tpl] = true
			return nil
		}
		for _, m := range methods {
			if m == http.MethodGet {
				seen[tpl] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	routes := make([]string, 0, len(seen))
	for tpl := range seen {
		routes = append(routes, tpl)
	}
	sort.Strings(routes)
	return routes
}

func TestEveryAPIReaderConcurrentIngest597(t *testing.T) {
	const readers, reads, hashReads, maxIngests, seedObs = 2, 4, 12, 100, 300

	srv, router := setupTestServer(t)
	routes := apiGETRoutes597(t, router)
	// Guard against a walk that silently stops finding the readers.
	if len(routes) < 80 {
		t.Fatalf("found only %d GET /api routes; the router walk no longer sees them", len(routes))
	}
	txs := storeTxs470(srv.store)
	if len(txs) == 0 {
		t.Fatal("test server has no transmissions")
	}
	if len(txs) < 2 {
		t.Fatal("test server needs a transmission to keep and one to rehash")
	}
	// txs[0] keeps its hash; the writer rehashes the others.
	stableHash := txs[0].Hash
	if !hashPattern.MatchString(stableHash) {
		t.Fatalf("fixture hash %q is not a packet hash; {id} routes would 404", stableHash)
	}
	hashes := make([]string, len(txs))
	for i, tx := range txs {
		hashes[i] = tx.Hash
	}
	movable := len(txs) - 1
	ingest := ingestInto470(srv.store, txs, "obs1")
	// Give the transmission the hash routes read plenty of observations, so
	// an unlocked read of them spans a long loop and a write lands in it. A
	// hash route that read three observations without s.mu would otherwise
	// finish before the writer's next write.
	for k := 0; k < seedObs; k++ {
		ingest(1_000_002 + len(txs)*k - (1_000_002 % len(txs))) // n%len(txs)==0: txs[0]
	}
	migrations := 0
	write := func(n int) {
		// Ingest stops growing the observation lists past maxIngests, so
		// a slow reader (the race detector makes them slow) never faces a
		// list that grows without bound while it runs; the migration
		// keeps writing.
		if n%2 == 0 && n <= 2*maxIngests {
			ingest(n)
			return
		}
		// Rehash to a new hash and back (txs[0] stays put, see stableHash).
		i := 1 + migrations%movable
		h := hashes[i]
		if migrations/movable%2 == 0 {
			h += "97"
		}
		migrations++
		applyHashMigration470(srv.store, txs[i], h)
	}

	for _, tpl := range routes {
		path := raceSweepPath597(tpl, stableHash)
		n := reads
		if strings.Contains(tpl, "{hash}") || strings.HasPrefix(tpl, "/api/packets/{id}") {
			n = hashReads // hash lookups only race in a narrow window
		}
		t.Run(tpl, func(t *testing.T) {
			raceIngest470(t, readers, n, 2*len(txs), func(r, i int) error {
				// Recompute the analytics instead of serving them from the
				// cache, which would hide the computation from the writer.
				srv.store.invalidateCachesFor(cacheInvalidation{eviction: true})
				target := path
				if i%2 == 1 { // the region filter path of every reader that has one
					target += "?region=SJC"
				}
				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
				// The status is not asserted: some routes need schema the
				// fixture lacks (scope_name) and answer 500 before reading
				// the store. The race detector is the assertion.
				if w.Code == 0 {
					return fmt.Errorf("GET %s: no response", target)
				}
				// The packet route must reach the store, not 404 on the id.
				if strings.HasPrefix(tpl, "/api/packets/{id}") && w.Code != http.StatusOK {
					return fmt.Errorf("GET %s: status %d, want 200", target, w.Code)
				}
				return nil
			}, write)
		})
	}
}
