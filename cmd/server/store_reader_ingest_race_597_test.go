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

// raceSweepPathVars597 fills a route template's variables with fixture ids.
var raceSweepPathVars597 = strings.NewReplacer(
	"{pubkey}", "aabbccdd11223344",
	"{hash}", "abc123def4567890",
	"{id}", "obs1",
	"{requestId}", "1",
)

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
	const readers, reads = 2, 4

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
	hashes := make([]string, len(txs))
	for i, tx := range txs {
		hashes[i] = tx.Hash
	}
	ingest := ingestInto470(srv.store, txs, "obs1")
	migrations := 0
	write := func(n int) {
		if n%2 == 0 {
			ingest(n)
			return
		}
		// Rehash to a new hash and back, so the fixture's own hash keeps
		// resolving for the {hash} routes half of the time.
		i := migrations % len(txs)
		h := hashes[i]
		if migrations/len(txs)%2 == 0 {
			h += "97"
		}
		migrations++
		applyHashMigration470(srv.store, txs[i], h)
	}

	for _, tpl := range routes {
		path := raceSweepPathVars597.Replace(tpl)
		t.Run(tpl, func(t *testing.T) {
			raceIngest470(t, readers, reads, 2*len(txs), func(r, i int) error {
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
				return nil
			}, write)
		})
	}
}
