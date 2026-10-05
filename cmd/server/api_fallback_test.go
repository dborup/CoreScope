package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// #233: an unknown /api/* path, or a known one called with the wrong
// method, must get a JSON error, never the SPA's 200 index.html. These
// tests build the router the same two ways production code does:
//   - setupTestServer: RegisterRoutes only (what most other _test.go files use)
//   - productionStyleRouter below: RegisterRoutes + /ws + the SPA catch-all,
//     matching main.go's actual composition order.
func productionStyleRouter(t *testing.T, srv *Server) *mux.Router {
	t.Helper()
	router := mux.NewRouter()
	srv.RegisterRoutes(router)
	router.HandleFunc("/ws", NewHub().ServeWS)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>SPA</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	router.PathPrefix("/").Handler(wsOrStatic(NewHub(), spaHandler(dir, http.FileServer(http.Dir(dir)))))
	return router
}

func TestAPIFallbackUnknownPathReturns404JSON(t *testing.T) {
	_, router := setupTestServer(t)
	req := httptest.NewRequest("GET", "/api/this-path-does-not-exist", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("GET /api/this-path-does-not-exist: want 404, got %d (body %q)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("want application/json content-type, got %q (body %q)", ct, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, w.Body.String())
	}
	if body["error"] == "" {
		t.Errorf("expected a non-empty \"error\" field, got %v", body)
	}
}

func TestAPIFallbackUnknownPathInProductionRouterReturns404JSON(t *testing.T) {
	srv, _ := setupTestServer(t)
	router := productionStyleRouter(t, srv)

	req := httptest.NewRequest("GET", "/api/this-path-does-not-exist", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("GET /api/this-path-does-not-exist: want 404, got %d (body %q)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("want application/json content-type, got %q", ct)
	}
	if strings.Contains(strings.ToLower(w.Body.String()), "<html") {
		t.Fatalf("response leaked the SPA page: %q", w.Body.String())
	}
}

// Locks POST /api/packets specifically (#233 acceptance criterion), in the
// full production-style composition where the SPA catch-all used to win.
func TestAPIFallbackPostPacketsReturns405NotSPA(t *testing.T) {
	srv, _ := setupTestServer(t)
	router := productionStyleRouter(t, srv)

	req := httptest.NewRequest("POST", "/api/packets", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/packets: want 405, got %d (body %q)", w.Code, w.Body.String())
	}
	assertAllowSet(t, w.Header().Get("Allow"), "GET", "HEAD")
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("want application/json content-type, got %q", ct)
	}
	if strings.Contains(strings.ToLower(w.Body.String()), "<html") {
		t.Fatalf("response leaked the SPA page: %q", w.Body.String())
	}
}

// Known /api path, wrong method, on the bare API router (no SPA involved).
func TestAPIFallbackKnownPathWrongMethodReturns405(t *testing.T) {
	_, router := setupTestServer(t)
	req := httptest.NewRequest("DELETE", "/api/stats", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /api/stats: want 405, got %d (body %q)", w.Code, w.Body.String())
	}
	assertAllowSet(t, w.Header().Get("Allow"), "GET", "HEAD")
}

// Everything else must be unaffected: every route documented by the served
// OpenAPI spec still matches its own real handler, not the new fallback.
func TestAPIFallbackDoesNotShadowExistingRoutes(t *testing.T) {
	srv, router := setupTestServer(t)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/spec", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/spec: want 200, got %d", w.Code)
	}
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.Paths) < 20 {
		t.Fatalf("expected at least 20 documented paths, got %d", len(spec.Paths))
	}

	_ = srv
	checked := 0
	for path, ops := range spec.Paths {
		testPath := path
		for strings.Contains(testPath, "{") {
			start := strings.Index(testPath, "{")
			end := strings.Index(testPath[start:], "}") + start
			testPath = testPath[:start] + "x" + testPath[end+1:]
		}
		for method := range ops {
			req := httptest.NewRequest(strings.ToUpper(method), testPath, nil)
			var match mux.RouteMatch
			if !router.Match(req, &match) {
				t.Errorf("%s %s: no longer matches any route (err %v)", strings.ToUpper(method), testPath, match.MatchErr)
				continue
			}
			tmpl, err := match.Route.GetPathTemplate()
			if err != nil || tmpl != path {
				t.Errorf("%s %s: matched route template %q (err %v), want %q — shadowed by the fallback",
					strings.ToUpper(method), testPath, tmpl, err, path)
			}
			checked++
		}
	}
	if checked < 20 {
		t.Fatalf("only checked %d routes, expected at least 20", checked)
	}
}

// WebSocket upgrade path and SPA deep links/static files must be untouched
// by the new /api/ fallback, since they are not under /api/.
func TestAPIFallbackDoesNotAffectWebSocketOrSPA(t *testing.T) {
	srv, _ := setupTestServer(t)
	router := productionStyleRouter(t, srv)

	// /ws still routes to the hub handler (not caught by PathPrefix("/api/")).
	var match mux.RouteMatch
	if !router.Match(httptest.NewRequest("GET", "/ws", nil), &match) {
		t.Fatalf("/ws: no longer routed (err %v)", match.MatchErr)
	}
	if tmpl, _ := match.Route.GetPathTemplate(); tmpl != "/ws" {
		t.Errorf("/ws: matched template %q, want \"/ws\"", tmpl)
	}

	// SPA deep link (non-/api path) still falls through to index.html.
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/#/packets", nil))
	if w.Code != http.StatusOK || w.Body.String() != "<html>SPA</html>" {
		t.Errorf("/#/packets: want 200 SPA page, got %d %q", w.Code, w.Body.String())
	}
}

// assertAllowSet compares an Allow header to an exact method set, ignoring
// order and whitespace, so "GETX" or a missing/extra method fails.
func assertAllowSet(t *testing.T, allow string, want ...string) {
	t.Helper()
	var got []string
	for _, m := range strings.Split(allow, ",") {
		if m = strings.TrimSpace(m); m != "" {
			got = append(got, m)
		}
	}
	sort.Strings(got)
	want = append([]string(nil), want...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Allow: got %q (set %v), want exactly %v", allow, got, want)
	}
}

// A geo-filter path has GET and PUT routes; the Allow set must compose both
// plus HEAD (served for every GET route).
func TestAPIFallbackAllowComposesMethodsAcrossRoutes(t *testing.T) {
	_, router := setupTestServer(t)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("POST", "/api/config/geo-filter", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/config/geo-filter: want 405, got %d (body %q)", w.Code, w.Body.String())
	}
	assertAllowSet(t, w.Header().Get("Allow"), "GET", "HEAD", "PUT")
}

// openAPIOperations returns the served spec's operations as concrete
// request paths ({param} -> "x"), keyed by path, with upper-case methods.
func openAPIOperations(t *testing.T, router http.Handler) map[string][]string {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/spec", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/spec: want 200, got %d", w.Code)
	}
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &spec); err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for path, ops := range spec.Paths {
		testPath := path
		for strings.Contains(testPath, "{") {
			start := strings.Index(testPath, "{")
			end := strings.Index(testPath[start:], "}") + start
			testPath = testPath[:start] + "x" + testPath[end+1:]
		}
		for method := range ops {
			out[testPath] = append(out[testPath], strings.ToUpper(method))
		}
	}
	if len(out) < 20 {
		t.Fatalf("expected at least 20 documented paths, got %d", len(out))
	}
	return out
}

// HEAD on a known /api route must keep succeeding (it was 200 on master,
// via the SPA). Every documented GET route answers HEAD with GET's status
// and headers; net/http drops the body. A documented path without GET
// answers HEAD with 405 and the exact Allow set. Runs over a real listener
// so the HEAD body suppression is net/http's, as in production.
func TestAPIFallbackHeadMatchesGetForEveryOpenAPIRoute(t *testing.T) {
	srv, _ := setupTestServer(t)
	router := productionStyleRouter(t, srv)
	ts := httptest.NewServer(router)
	defer ts.Close()
	client := &http.Client{Timeout: 30 * time.Second}

	do := func(method, path string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, ts.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if method == "HEAD" && len(body) != 0 {
			t.Errorf("HEAD %s: got a %d-byte body", path, len(body))
		}
		return resp
	}

	ops := openAPIOperations(t, router)
	checked := 0
	for path, methods := range ops {
		hasGet := false
		for _, m := range methods {
			if m == "GET" {
				hasGet = true
			}
		}
		head := do("HEAD", path)
		if !hasGet {
			if head.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("HEAD %s (no GET route): want 405, got %d", path, head.StatusCode)
			}
			assertAllowSet(t, head.Header.Get("Allow"), methods...)
			continue
		}
		get := do("GET", path)
		if head.StatusCode == http.StatusMethodNotAllowed {
			t.Errorf("HEAD %s: got 405 (Allow %q), want GET's status %d", path, head.Header.Get("Allow"), get.StatusCode)
			continue
		}
		if head.StatusCode != get.StatusCode {
			t.Errorf("HEAD %s: status %d, GET status %d", path, head.StatusCode, get.StatusCode)
		}
		for _, h := range []string{"Content-Type", "Cache-Control", "Allow"} {
			if hv, gv := head.Header.Get(h), get.Header.Get(h); hv != gv {
				t.Errorf("HEAD %s: %s %q, GET %s %q", path, h, hv, h, gv)
			}
		}
		checked++
	}
	if checked < 20 {
		t.Fatalf("only checked %d GET routes with HEAD, expected at least 20", checked)
	}
}

// Bare /api (no trailing slash) is an API path too: JSON 404, not the SPA.
func TestAPIFallbackBareAPIReturns404JSON(t *testing.T) {
	srv, bare := setupTestServer(t)
	for name, router := range map[string]http.Handler{"api router": bare, "production router": productionStyleRouter(t, srv)} {
		for _, method := range []string{"GET", "POST"} {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(method, "/api", nil))
			if w.Code != http.StatusNotFound {
				t.Errorf("%s: %s /api: want 404, got %d (body %q)", name, method, w.Code, w.Body.String())
				continue
			}
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("%s: %s /api: want application/json, got %q (body %q)", name, method, ct, w.Body.String())
			}
		}
	}
}

// Paths that only share the "/api" string prefix are not API paths and
// must still reach the SPA.
func TestAPIFallbackPrefixSiblingsStillReachSPA(t *testing.T) {
	srv, _ := setupTestServer(t)
	router := productionStyleRouter(t, srv)
	for _, path := range []string{"/api-docs", "/apifoo", "/apiary", "/apis/x", "/api.json"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusOK || w.Body.String() != "<html>SPA</html>" {
			t.Errorf("GET %s: want 200 SPA page, got %d %q", path, w.Code, w.Body.String())
		}
	}
}
