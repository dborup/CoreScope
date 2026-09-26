package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The #2073 advert route breakdown is opt-in (PR #97 review P2-2): only the
// node page asks for it with include=advertRoutes. Every other caller of
// GET /api/nodes/{pubkey} (packets, live, channels, route view, claimed
// nodes) gets master's response and costs master's queries.

const narIncludeQuery = "?include=advertRoutes"

// narCountRouteWork counts cache lookups and scans of the breakdown.
func narCountRouteWork(s *Server) (lookups, scans *atomic.Int32) {
	lookups, scans = new(atomic.Int32), new(atomic.Int32)
	s.advertRoutes.onLookup = func() { lookups.Add(1) }
	s.advertRoutes.onScan = func() { scans.Add(1) }
	return lookups, scans
}

func narTopLevelKeys(t *testing.T, raw string) []string {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Without the opt-in the response is master's: the same two top-level
// fields, no route_class on recentAdverts, and neither the breakdown's cache
// nor its scan is touched. Near misses of the parameter do not opt in.
func TestNodeDetail_WithoutIncludeIsMasterShaped(t *testing.T) {
	srv, router := narServer(t)
	lookups, scans := narCountRouteWork(srv)
	for _, q := range []string{"", "?include=", "?include=other", "?include=advertroutes", "?include=advertRoutesX", "?advertRoutes=1", "?include_advertRoutes=1"} {
		body, raw := narGetNode(t, router, narNode+q, 200)
		if got := narTopLevelKeys(t, raw); !reflect.DeepEqual(got, []string{"node", "recentAdverts"}) {
			t.Fatalf("%q: top-level keys = %v, want master's [node recentAdverts]", q, got)
		}
		if strings.Contains(raw, "route_class") || strings.Contains(raw, "advertCounts") || strings.Contains(raw, "recentAdvertsByRoute") {
			t.Fatalf("%q: breakdown fields leaked into the default response: %s", q, raw)
		}
		if len(body.RecentAdverts) != 5 {
			t.Fatalf("%q: recentAdverts = %d rows, want 5", q, len(body.RecentAdverts))
		}
		if _, ok := body.RecentAdverts[0]["observations"]; !ok {
			t.Fatalf("%q: recentAdverts rows keep their observations", q)
		}
		if got := body.Node["flood_advert_count_7d"]; got != float64(2) {
			t.Fatalf("%q: flood_advert_count_7d = %v, want 2", q, got)
		}
	}
	if l, s := lookups.Load(), scans.Load(); l != 0 || s != 0 {
		t.Fatalf("default requests touched the breakdown: %d cache lookups, %d scans", l, s)
	}
	if n := srv.advertRoutes.len(); n != 0 {
		t.Fatalf("default requests filled the breakdown cache: %d entries", n)
	}
}

// include takes a comma-separated list and may repeat; advertRoutes in any
// position opts in.
func TestNodeDetail_IncludeAdvertRoutesListForms(t *testing.T) {
	srv, router := narServer(t)
	lookups, _ := narCountRouteWork(srv)
	for _, q := range []string{narIncludeQuery, "?include=other,advertRoutes", "?include=other&include=advertRoutes", "?include=%20advertRoutes"} {
		body, raw := narGetNode(t, router, narNode+q, 200)
		if body.ByRoute == nil || body.Counts == nil || !strings.Contains(raw, "route_class") {
			t.Fatalf("%q did not opt in: %s", q, raw)
		}
	}
	if lookups.Load() != 4 {
		t.Fatalf("%d cache lookups for 4 opted-in requests", lookups.Load())
	}
}

// The privacy gates run before the cache: a hidden identity never reaches a
// lookup or a scan, even when it asks for the breakdown.
func TestNodeDetail_IncludeAdvertRoutesPrivacyBeforeCache(t *testing.T) {
	srv, router := narServer(t)
	lookups, scans := narCountRouteWork(srv)
	srv.cfg.ObserverBlacklist = []string{strings.ToUpper(narNode)}
	_, raw := narGetNode(t, router, narNode+narIncludeQuery, 200)
	if strings.Contains(raw, "route_class") || strings.Contains(raw, "advertCounts") {
		t.Fatalf("hidden identity leaked: %s", raw)
	}
	if l, s := lookups.Load(), scans.Load(); l != 0 || s != 0 {
		t.Fatalf("hidden identity reached the breakdown: %d lookups, %d scans", l, s)
	}
}

// N1 (re-review of 658d8a08): a failed identity-hidden lookup must fail
// closed, not merely agree with a hidden-by-config identity by coincidence.
// The mutant `advertRoutes = !hidden` (dropping the err check) survives the
// PrivacyBeforeCache test above because isIdentityHidden short-circuits to
// (true, nil) for a blacklisted/observer-blacklisted identity without ever
// running the name lookup that can fail — so `!hidden` and `err == nil &&
// !hidden` agree there. They only diverge when the lookup itself errors:
// isIdentityHidden then returns (false, err), so the mutant's bare !hidden
// is true (fail OPEN) while the real gate is false. Forcing that error
// needs a non-empty hidden-name-prefix list (otherwise hiddenIdentityNames
// short-circuits to (nil, nil) without touching the DB) plus a request
// whose context is already canceled once the lookup's QueryContext /
// QueryRowContext calls run. The configured prefix must not match this
// node's own name, so the request reaches this code instead of 404-ing on
// the earlier, unrelated IsNameHidden(node name) gate.
func TestNodeDetail_IncludeAdvertRoutesFailsClosedOnLookupError(t *testing.T) {
	srv, router := narServer(t)
	lookups, scans := narCountRouteWork(srv)
	srv.cfg.SetHiddenNamePrefixes([]string{"ZZ-does-not-match-any-node-or-observer"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "/api/nodes/"+narNode+narIncludeQuery, nil).WithContext(ctx)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("GET with a canceled context = %d, want 200: %s", w.Code, w.Body.String())
	}
	raw := w.Body.String()
	if strings.Contains(raw, "route_class") || strings.Contains(raw, "advertCounts") || strings.Contains(raw, "recentAdvertsByRoute") {
		t.Fatalf("a failed privacy lookup must fail closed, leaked breakdown fields: %s", raw)
	}
	var body narResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if body.ByRoute != nil || body.Counts != nil {
		t.Fatalf("a failed lookup must not populate the breakdown: byRoute=%v counts=%v", body.ByRoute, body.Counts)
	}
	if len(body.RecentAdverts) != 5 {
		t.Fatalf("recentAdverts = %d rows, want master's 5", len(body.RecentAdverts))
	}
	if _, ok := body.RecentAdverts[0]["route_class"]; ok {
		t.Fatalf("recentAdverts must not carry route_class when the lookup failed: %v", body.RecentAdverts[0])
	}
	if l, s := lookups.Load(), scans.Load(); l != 0 || s != 0 {
		t.Fatalf("a failed lookup must never reach the breakdown cache or scan: %d lookups, %d scans", l, s)
	}
}

// flood_advert_count_7d is an external contract and never cached: with the
// breakdown served from the cache it is still counted fresh, and when this
// request's own scan produced it, it equals CountFloodAdvertsForNode.
func TestNodeDetail_FloodAdvertCount7dStaysFresh(t *testing.T) {
	srv, router := narServer(t)
	_, scans := narCountRouteWork(srv)
	body, _ := narGetNode(t, router, narNode+narIncludeQuery, 200)
	if got := body.Node["flood_advert_count_7d"]; got != float64(2) {
		t.Fatalf("cold flood_advert_count_7d = %v, want 2", got)
	}
	// A new route_type 1 advert inside the debounce: the breakdown stays
	// cached, the flood count must not.
	narInsert(t, srv.db, narNode, "h-new-flood", payloadTypeAdvert, 1, 0b0010, narAgo(time.Minute), true)
	body, _ = narGetNode(t, router, narNode+narIncludeQuery, 200)
	if scans.Load() != 1 {
		t.Fatalf("%d scans, want the second request served from the cache", scans.Load())
	}
	if len(body.ByRoute.Flood) != 3 {
		t.Fatalf("cached flood list = %d rows, want the cached 3", len(body.ByRoute.Flood))
	}
	if got := body.Node["flood_advert_count_7d"]; got != float64(3) {
		t.Fatalf("flood_advert_count_7d = %v next to a cached breakdown, want the fresh 3", got)
	}
	want, err := srv.db.CountFloodAdvertsForNode(narNode, 7*24, floodAdvertRowCap)
	if err != nil || want != 3 {
		t.Fatalf("CountFloodAdvertsForNode = %d, %v", want, err)
	}
}

// The scan's flood count is handed only to the request that ran the scan:
// a request that shared another's scan (singleflight) or hit the cache counts
// its own, so the value is never older than the request.
func TestNodeAdvertRoutes_FloodCountOnlyFromOwnScan(t *testing.T) {
	s := narCacheServer(t)
	narInsert(t, s.db, narNode, "own-a", payloadTypeAdvert, 1, 0b0010, narAgo(time.Hour), true)
	release := make(chan struct{})
	var scans atomic.Int32
	s.advertRoutes.onScan = func() { scans.Add(1); <-release }
	var withFlood atomic.Int32
	var wg sync.WaitGroup
	now := time.Now()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.nodeAdvertRoutes(narNode, now)
			if err != nil {
				t.Error(err)
				return
			}
			if res.floodAdvertCount7d != nil {
				withFlood.Add(1)
				if *res.floodAdvertCount7d != 1 {
					t.Errorf("scan flood count = %d, want 1", *res.floodAdvertCount7d)
				}
			}
		}()
	}
	time.Sleep(100 * time.Millisecond) // let the goroutines join the flight
	close(release)
	wg.Wait()
	if scans.Load() != 1 || withFlood.Load() != 1 {
		t.Fatalf("scans=%d, results with a flood count=%d; want 1 and 1 (the scanning request only)", scans.Load(), withFlood.Load())
	}
	res, err := s.nodeAdvertRoutes(narNode, now)
	if err != nil || res.floodAdvertCount7d != nil {
		t.Fatalf("cache hit must not carry a flood count: %v, %v", res.floodAdvertCount7d, err)
	}
}
