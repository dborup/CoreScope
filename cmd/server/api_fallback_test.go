package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if allow := w.Header().Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("want Allow header containing GET, got %q", allow)
	}
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
	if allow := w.Header().Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("want Allow header containing GET, got %q", allow)
	}
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
