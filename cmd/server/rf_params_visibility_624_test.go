package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// #624 audits upstream PR 2276, which made upstream's GET /api/rf-params
// apply the node blacklist and hidden-name policy (it leaked the pubkeys and
// RF settings of excluded nodes, also on a warm cache). This fork never took
// the RF params feature (upstream #1268): there is no such route, snapshot
// or deviations cache, so the leak cannot occur here. These tests pin that.
//
// If you port the RF params feature, port upstream 2276's response filter
// with it and replace these tests with its warm/cold-cache visibility test.

var rfParamsPaths624 = []string{"/api/rf-params", "/api/rf-params/", "/api/rf-params?maxAgeDays=7"}

// The upstream path must stay the JSON 404 of every unknown /api path, in
// the production router composition, for both policies and on a cold or a
// warm node cache: no pubkey or name of the excluded node, no SPA page, and
// the same body whether the node is hidden or visible.
func TestRfParamsVisibility624(t *testing.T) {
	for _, policy := range []string{"blacklist", "hidden-name"} {
		for _, warm := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/warm=%t", policy, warm), func(t *testing.T) {
				srv, _ := setupTestServer(t)
				router := productionStyleRouter(t, srv)
				target := strings.Repeat("ab", 32)
				const targetName = "RF-hidden-fixture-624"
				if _, err := srv.db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen)
					VALUES (?, ?, 'repeater', 1, 1, ?)`, target, targetName, time.Now().UTC().Format(time.RFC3339)); err != nil {
					t.Fatal(err)
				}
				srv.store.InvalidateNodeCache()

				request := func(path string) *httptest.ResponseRecorder {
					w := httptest.NewRecorder()
					router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
					return w
				}
				setHidden := func(hidden bool) {
					var entries []string
					switch {
					case hidden && policy == "blacklist":
						entries = []string{" " + strings.ToUpper(target) + " "}
					case hidden:
						entries = []string{"RF-hidden-"}
					}
					if policy == "blacklist" {
						srv.cfg.SetNodeBlacklist(entries)
					} else {
						srv.cfg.SetHiddenNamePrefixes(entries)
					}
				}
				rfBodies := func(stage string, hidden bool) map[string]string {
					t.Helper()
					bodies := make(map[string]string, len(rfParamsPaths624))
					for _, path := range rfParamsPaths624 {
						w := request(path)
						body := w.Body.String()
						if w.Code != http.StatusNotFound {
							t.Errorf("%s: GET %s = %d, want 404 (body %q)", stage, path, w.Code, body)
						}
						if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
							t.Errorf("%s: GET %s content-type = %q, want JSON", stage, path, ct)
						}
						lower := strings.ToLower(body)
						if hidden && (strings.Contains(lower, target) || strings.Contains(body, targetName)) {
							t.Errorf("%s: GET %s leaks the excluded node: %q", stage, path, body)
						}
						if strings.Contains(lower, "<html") {
							t.Errorf("%s: GET %s served the SPA page", stage, path)
						}
						bodies[path] = body
					}
					return bodies
				}
				nodeStatus := func(stage string, want int) {
					t.Helper()
					if w := request("/api/nodes/" + target); w.Code != want {
						t.Fatalf("%s: node detail = %d, want %d", stage, w.Code, want)
					}
				}

				nodeStatus("visible control", http.StatusOK)
				var visible map[string]string
				if warm {
					// Warm the node cache the upstream handler read from.
					if w := request("/api/nodes"); w.Code != http.StatusOK {
						t.Fatalf("warm-up /api/nodes = %d", w.Code)
					}
					visible = rfBodies("visible, warm", false)
				}
				setHidden(true)
				nodeStatus("hidden control", http.StatusNotFound)
				hidden := rfBodies("hidden", true)
				setHidden(false)
				nodeStatus("unhidden control", http.StatusOK)
				unhidden := rfBodies("unhidden", false)
				for _, path := range rfParamsPaths624 {
					if hidden[path] != unhidden[path] || (warm && visible[path] != hidden[path]) {
						t.Errorf("GET %s varies with node visibility: visible %q, hidden %q, unhidden %q",
							path, visible[path], hidden[path], unhidden[path])
					}
				}
			})
		}
	}
}

// No registered route may serve the upstream RF params path. This is the
// tripwire for a future port: it fails until the port brings the 2276
// visibility filter and its test with it (see the comment at the top).
func TestRfParamsRouteNotRegistered624(t *testing.T) {
	srv, _ := setupTestServer(t)
	router := productionStyleRouter(t, srv)
	err := router.Walk(func(route *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		tmpl, err := route.GetPathTemplate()
		if err != nil {
			return nil // the SPA catch-all and other prefix-less routes
		}
		norm := strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(tmpl))
		if strings.Contains(norm, "rfparams") {
			t.Errorf("route %q serves RF params: port upstream 2276's visibility filter and its test with it (#624)", tmpl)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// And the paths themselves must reach only the /api fallback (JSON 404).
	for _, path := range rfParamsPaths624 {
		var match mux.RouteMatch
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if !router.Match(req, &match) || match.Route == nil || match.Route.GetName() != apiFallbackRouteName {
			name := ""
			if match.Route != nil {
				name = match.Route.GetName()
			}
			t.Errorf("GET %s matches route %q, want only %q (#624)", path, name, apiFallbackRouteName)
		}
	}
}
