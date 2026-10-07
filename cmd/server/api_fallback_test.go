package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
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
//   - productionStyleRouter below: newHTTPRouter, the RegisterRoutes + /ws +
//     SPA catch-all composition main.go serves, with a stub index.html.
func productionStyleRouter(t *testing.T, srv *Server) *mux.Router {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>SPA</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	return newHTTPRouter(srv, NewHub(), dir)
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
// via the SPA). Every documented path answers HEAD with the status and
// headers GET gets, and no body; where GET itself is 405 (no GET route for
// that URL), HEAD is 405 with the exact Allow set. Some analytics endpoints
// answer 202 until a background compute finishes, so HEAD is compared with
// the GET just before and just after it. Runs over a real listener so the
// HEAD body suppression is net/http's, as in production.
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
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	// http.Client never surfaces a HEAD body, so read the raw bytes after
	// the header block to prove the server sends none.
	rawHeadBody := func(path string) int {
		t.Helper()
		conn, err := net.DialTimeout("tcp", ts.Listener.Addr().String(), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(30 * time.Second))
		fmt.Fprintf(conn, "HEAD %s HTTP/1.0\r\nHost: test\r\n\r\n", path)
		raw, err := io.ReadAll(conn)
		if err != nil {
			t.Fatalf("HEAD %s (raw): %v", path, err)
		}
		i := bytes.Index(raw, []byte("\r\n\r\n"))
		if i < 0 {
			t.Fatalf("HEAD %s (raw): no header terminator in %q", path, raw)
		}
		return len(raw) - (i + 4)
	}

	ops := openAPIOperations(t, router)
	served := 0
	for path, methods := range ops {
		before := do("GET", path)
		head := do("HEAD", path)
		after := do("GET", path)

		if before.StatusCode == http.StatusMethodNotAllowed {
			if head.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("HEAD %s (GET is 405): want 405, got %d", path, head.StatusCode)
			}
			assertAllowSet(t, head.Header.Get("Allow"), methods...)
			continue
		}
		get := before
		if head.StatusCode != before.StatusCode {
			get = after
		}
		if head.StatusCode != get.StatusCode {
			t.Errorf("HEAD %s: status %d (Allow %q), GET status %d then %d",
				path, head.StatusCode, head.Header.Get("Allow"), before.StatusCode, after.StatusCode)
			continue
		}
		for _, h := range []string{"Content-Type", "Cache-Control", "Allow"} {
			if hv, gv := head.Header.Get(h), get.Header.Get(h); hv != gv {
				t.Errorf("HEAD %s: %s %q, GET %s %q", path, h, hv, h, gv)
			}
		}
		if n := rawHeadBody(path); n != 0 {
			t.Errorf("HEAD %s: server sent a %d-byte body", path, n)
		}
		served++
	}
	t.Logf("HEAD served like GET on %d of %d documented paths; the rest have no GET route (405)", served, len(ops))
	if served < 20 {
		t.Fatalf("only %d documented paths served HEAD like GET, expected at least 20", served)
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

// The router main.go serves must not register any /api route after the
// fallback: mux tries routes in order and the fallback matches every method
// and path under /api, so such a route would be dead with no error.
func TestProductionRouterHasNoShadowedAPIRoutes(t *testing.T) {
	srv, _ := setupTestServer(t)
	router := productionStyleRouter(t, srv)

	// The guard is only meaningful if the fallback is there and is the last
	// /api route.
	var last string
	router.Walk(func(route *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		if tmpl, err := route.GetPathTemplate(); err == nil && (tmpl == "/api" || strings.HasPrefix(tmpl, "/api/")) {
			last = route.GetName()
		}
		return nil
	})
	if last != apiFallbackRouteName {
		t.Fatalf("last /api route in the production router is %q, want the fallback %q", last, apiFallbackRouteName)
	}
	if shadowed := apiRoutesShadowedByFallback(router); len(shadowed) > 0 {
		t.Errorf("/api routes registered after the fallback are unreachable: %v", shadowed)
	}
}

// The guard itself: a late /api route is reported and really is dead;
// a late non-/api route that merely starts with "api" is not reported.
func TestAPIRoutesShadowedByFallbackReportsLateRoutes(t *testing.T) {
	srv, _ := setupTestServer(t)
	router := mux.NewRouter()
	srv.RegisterRoutes(router)
	late := func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("late")) }
	router.HandleFunc("/api/late", late).Methods("GET")
	router.HandleFunc("/api/late/{id}", late)
	router.HandleFunc("/apiary-late", late)

	got := apiRoutesShadowedByFallback(router)
	if strings.Join(got, " ") != "/api/late /api/late/{id}" {
		t.Errorf("shadowed routes: got %v, want [/api/late /api/late/{id}]", got)
	}

	// The fallback answers instead of the late handler (with a 405 that
	// names GET, since it sees the late route's method).
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/late", nil))
	if w.Code == http.StatusOK || w.Body.String() == "late" {
		t.Errorf("GET /api/late: the late handler ran (%d %q); the fallback should shadow it", w.Code, w.Body.String())
	}
}

// #281 N1: apiRoutesShadowedByFallback must flag a path template of
// exactly "/api", not only ones under the "/api/" prefix.
// TestAPIRoutesShadowedByFallbackReportsLateRoutes above only registers
// late routes under the prefix, so it never exercises the `tmpl == "/api"`
// arm; dropping that arm still leaves the suite green.
func TestAPIRoutesShadowedByFallbackReportsLateBareAPIRoute(t *testing.T) {
	srv, _ := setupTestServer(t)
	router := mux.NewRouter()
	srv.RegisterRoutes(router)
	late := func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("late")) }
	router.HandleFunc("/api", late)

	got := apiRoutesShadowedByFallback(router)
	if strings.Join(got, " ") != "/api" {
		t.Errorf("shadowed routes: got %v, want [/api]", got)
	}

	// The fallback answers instead of the late handler.
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api", nil))
	if w.Code == http.StatusOK || w.Body.String() == "late" {
		t.Errorf("GET /api: the late handler ran (%d %q); the fallback should shadow it", w.Code, w.Body.String())
	}
}

// #281 N2: getRouteHandler's self-dispatch guard must return a nil handler
// when the only route a GET to the URL would reach is the fallback itself
// (no real /api route matches), rather than returning the fallback's own
// handler. Nothing in the HTTP-level test suite distinguishes the guard
// being present from it being absent: the recursive call it would otherwise
// allow carries Method: GET, lands back on the same 404/405 computation,
// and produces byte-identical output. This test calls getRouteHandler
// directly to pin the guard itself.
func TestGetRouteHandlerSelfDispatchGuardReturnsNilForUnknownPath(t *testing.T) {
	router := mux.NewRouter()
	registerAPIFallback(router)

	r := httptest.NewRequest(http.MethodHead, "/api/this-path-does-not-exist", nil)
	h, got := getRouteHandler(router, r)
	if h != nil || got != nil {
		t.Fatalf("getRouteHandler: want (nil, nil) when only the fallback matches, got (%v, %v)", h, got)
	}
}

// apiOnlyBannerBody builds the production router with a missing public dir
// (the only mode that renders the API-only banner) and returns the banner
// body served for GET /, plus the router itself.
func apiOnlyBannerBody(t *testing.T) (*mux.Router, string) {
	t.Helper()
	srv, _ := setupTestServer(t)
	missingDir := filepath.Join(t.TempDir(), "does-not-exist")
	router := newHTTPRouter(srv, NewHub(), missingDir)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	return router, w.Body.String()
}

// bannerTag matches an HTML tag, so the banner's prose can be scanned
// without closing tags like </p> leaking a slash-prefixed token.
var bannerTag = regexp.MustCompile(`<[^>]*>`)

// bannerReference matches any URL-ish token the banner advertises: an
// absolute http(s) URL, or a slash-prefixed path (preceded by start,
// whitespace, or an opening paren so the leading "/" of a path is caught but
// the "/" inside a closing HTML tag is not). It is deliberately NOT limited
// to "/api..." so a banner that points at /swagger, /docs, /API/spec, or an
// external URL is still extracted and then rejected, instead of silently
// going unchecked (#320 F1/F2). The character class keeps "."/"-"/etc. so
// /api/spec.json is matched whole, not truncated to /api/spec.
var bannerReference = regexp.MustCompile(`https?://[^\s<>"()]+|(?:^|[\s(])/[^\s<>"(),]*`)

// bannerText strips HTML tags from a banner body, leaving only its prose.
func bannerText(body string) string {
	return bannerTag.ReplaceAllString(body, " ")
}

// bannerReferences returns every URL-ish token the banner's prose advertises,
// trimmed of the leading whitespace/paren the regex captured and of trailing
// sentence punctuation.
func bannerReferences(body string) []string {
	var refs []string
	for _, m := range bannerReference.FindAllString(bannerText(body), -1) {
		ref := strings.TrimRight(strings.TrimLeft(m, " ("), ".,;:")
		if ref != "" {
			refs = append(refs, ref)
		}
	}
	return refs
}

// bannerPrimaryPointer returns the endpoint the banner names right after
// "API available at ", or "" if the phrase is absent.
func bannerPrimaryPointer(body string) string {
	text := bannerText(body)
	const marker = "API available at "
	i := strings.Index(text, marker)
	if i < 0 {
		return ""
	}
	rest := strings.TrimLeft(text[i+len(marker):], " (")
	field := strings.Fields(rest)
	if len(field) == 0 {
		return ""
	}
	return strings.TrimRight(field[0], ".,;:")
}

// #300 F1: the API-only banner must name an endpoint that works with no
// outbound internet. /api/docs is a Swagger UI shell whose CSS and JS load
// only from an external CDN (openapi.go), so an API-only deployment without
// egress renders it blank. /api/spec is the raw OpenAPI JSON, served
// in-process, so it is the offline-safe pointer the banner must name.
//
// #320 F1: this no longer just substring-checks "/api/spec" (which a passing
// mention satisfies even when the banner really points elsewhere). It reads
// the *primary* pointer the banner names ("API available at <X>") and proves
// it is a local, in-process JSON endpoint: GET <X> must be 200,
// application/json, and must not be the banner catch-all answering itself.
// A banner whose primary pointer is /swagger, an external URL, or the
// CDN-backed /api/docs (text/html) therefore fails.
func TestAPIOnlyBannerNamesOfflineSpecEndpoint(t *testing.T) {
	router, body := apiOnlyBannerBody(t)

	primary := bannerPrimaryPointer(body)
	if primary == "" {
		t.Fatalf("API-only banner names no primary endpoint after %q: %q", "API available at", body)
	}
	if !strings.HasPrefix(primary, "/") {
		t.Fatalf("API-only banner's primary pointer %q is not a local path (needs outbound internet): %q", primary, body)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", primary, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("banner's primary pointer %q: GET returned %d (want 200); banner=%q", primary, w.Code, body)
	}
	if w.Body.String() == body {
		t.Fatalf("banner's primary pointer %q is answered only by the banner catch-all, not a real endpoint; banner=%q", primary, body)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("banner's primary pointer %q must be the offline-safe JSON spec, got Content-Type %q; banner=%q", primary, ct, body)
	}
}

// #281 N4 / #300 F2: the API-only banner (served by newHTTPRouter when the
// static directory doesn't exist) must point at endpoints that actually
// resolve. It used to say "/api/", which #233 turned into a JSON 404.
//
// F2 makes the test strict: instead of checking a literal string and then
// GETting that same literal, it extracts every reference the banner actually
// advertises and GETs each one. #320 F1/F2 widen that extraction beyond
// "/api..." tokens: any advertised token must be a local path that returns
// 200 with a body that is NOT the banner itself. In API-only mode the
// PathPrefix("/") banner handler answers every non-/api path with 200 and the
// banner body, so "GET -> 200" alone cannot tell a real endpoint from the
// catch-all; the body check closes that. A banner that points at /swagger,
// /api/spec.json, or an external URL — while mentioning a working path only
// in passing — can no longer pass.
func TestAPIOnlyBannerPointsToExistingEndpoint(t *testing.T) {
	router, body := apiOnlyBannerBody(t)

	refs := bannerReferences(body)
	if len(refs) == 0 {
		t.Fatalf("API-only banner advertises no endpoint reference: %q", body)
	}
	for _, p := range refs {
		if !strings.HasPrefix(p, "/") {
			t.Fatalf("banner advertises %q, which needs outbound internet (not a local path); banner=%q", p, body)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("banner advertises %q, but GET %q returned %d (want 200); banner=%q", p, p, w.Code, body)
		}
		if w.Body.String() == body {
			t.Fatalf("banner advertises %q, but only the banner catch-all answers it (no real endpoint); banner=%q", p, body)
		}
	}
}

// #300 F3: allowedMethodsForPath must also cover a method-bearing route
// registered at exactly "/api", not only under the "/api/" prefix, so a
// wrong method on bare /api answers 405 + Allow the same way it does one
// level down. Latent today: the production router registers no real
// method-bearing route at bare /api (only the fallback, which carries no
// .Methods() and so contributes nothing), so this builds a router with a
// POST-only /api route ahead of the fallback to exercise the path.
func TestAllowedMethodsForBareAPIRoute(t *testing.T) {
	router := mux.NewRouter()
	router.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}).Methods("POST")
	registerAPIFallback(router)

	// Wrong method on bare /api: 405 naming the real route's method.
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api with a POST-only route: want 405, got %d", w.Code)
	}
	if allow := w.Header().Get("Allow"); allow != "POST" {
		t.Fatalf("GET /api: want Allow: POST, got %q", allow)
	}
	// #320 F3: the bare-/api 405 shares writeError with /api/*, so it must
	// carry the same JSON error shape and Content-Type, not a bare status.
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("GET /api (405): want application/json content-type, got %q (body %q)", ct, w.Body.String())
	}
	var errBody map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("GET /api (405): body is not JSON: %v (body %q)", err, w.Body.String())
	}
	if errBody["error"] == "" {
		t.Fatalf("GET /api (405): want a non-empty \"error\" field, got %v", errBody)
	}

	// The real route is still reachable under its own method.
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, httptest.NewRequest("POST", "/api", nil))
	if w2.Code != http.StatusTeapot {
		t.Fatalf("POST /api: want 418 (real route reached), got %d", w2.Code)
	}
}

// #300 F3 (no-route branch): with no real method-bearing route at bare /api
// — the actual production shape, where only the fallback sits there — any
// method on /api answers 404 with no Allow header, never a spurious 405.
func TestBareAPIWithoutRealRouteIs404(t *testing.T) {
	srv, _ := setupTestServer(t)
	router := mux.NewRouter()
	srv.RegisterRoutes(router) // registers the fallback at the end, no real /api route

	for _, method := range []string{"GET", "POST", "DELETE"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(method, "/api", nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s /api with no real route: want 404, got %d", method, w.Code)
		}
		if allow := w.Header().Get("Allow"); allow != "" {
			t.Fatalf("%s /api: want no Allow header, got %q", method, allow)
		}
	}
}
