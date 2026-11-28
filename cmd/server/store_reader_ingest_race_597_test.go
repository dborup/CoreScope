package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	const readers, reads, hashReads, seedObs = 2, 4, 6, 50
	// Ingest is paced to the reads: while a read is in flight the writer
	// adds an observation at most every ingestGap, and at most ingestsPerRead
	// per read started. Each read then overlaps ingest for its whole run,
	// while the observation lists stay bounded: an unpaced writer outruns
	// the readers (the race detector makes them slow), so the lists, and
	// each read of them, grow without bound.
	const ingestsPerRead, ingestGap = 10, 50 * time.Microsecond

	srv, router := setupTestServer(t)
	routes := apiGETRoutes597(t, router)
	// Guard against a walk that silently stops finding the readers.
	if len(routes) < 80 {
		t.Fatalf("found only %d GET /api routes; the router walk no longer sees them", len(routes))
	}
	txs := storeTxs470(srv.store)
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
	now := time.Now().UTC()
	obsSeq := 0
	ingest := func(tx *StoreTx) {
		obsSeq++
		applyObservation470(srv.store, tx, &StoreObs{
			ID: 300000 + obsSeq, TransmissionID: tx.ID, ObserverID: "obs1",
			PathJSON: fmt.Sprintf(`["aa","bb","%04x"]`, obsSeq), Timestamp: now.Add(time.Duration(obsSeq) * time.Millisecond).Format(time.RFC3339Nano),
		})
	}
	// Give the transmission the hash routes read enough observations that an
	// unlocked read of them spans a loop, so the writer's writes land in it.
	for k := 0; k < seedObs; k++ {
		ingest(txs[0])
	}

	for _, tpl := range routes {
		path := raceSweepPath597(tpl, stableHash)
		n := reads
		// Hash routes read txs[0], so ingest goes there: a write to any
		// other transmission cannot race their read of txs[0]'s
		// observations.
		hashRoute := strings.Contains(tpl, "{hash}") || strings.HasPrefix(tpl, "/api/packets/{id}")
		if hashRoute {
			n = hashReads // each read of txs[0] gives the writer another window
		}
		var started, inflight atomic.Int64
		ingests, migrations, selfMigrate := 0, 0, false
		var lastIngest time.Time
		write := func(int) {
			if inflight.Load() > 0 && int64(ingests) < ingestsPerRead*started.Load() && time.Since(lastIngest) >= ingestGap {
				tx := txs[ingests%len(txs)]
				if hashRoute {
					tx = txs[0]
				}
				ingests++
				lastIngest = time.Now()
				ingest(tx)
				return
			}
			if hashRoute {
				selfMigrate = !selfMigrate
				if selfMigrate {
					// Migrate txs[0] to the hash it has: that writes each
					// of its observations, and its list does not grow, so
					// these writes can go on for the whole read.
					applyHashMigration470(srv.store, txs[0], stableHash)
					return
				}
			}
			// Rehash to a new hash and back (txs[0] keeps its hash).
			i := 1 + migrations%movable
			h := hashes[i]
			if migrations/movable%2 == 0 {
				h += "97"
			}
			migrations++
			applyHashMigration470(srv.store, txs[i], h)
		}
		t.Run(tpl, func(t *testing.T) {
			raceIngest470(t, readers, n, 2*len(txs), func(r, i int) error {
				started.Add(1)
				inflight.Add(1)
				defer inflight.Add(-1)
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
