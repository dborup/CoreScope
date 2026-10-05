package main

import (
	"net/http"
	"sort"
	"strings"

	"github.com/gorilla/mux"
)

// registerAPIFallback adds a catch-all for /api/* requests that don't match
// any registered route, so an unknown path or a known path called with the
// wrong method gets a JSON error instead of falling through to the SPA's
// index.html (#233).
//
// gorilla/mux tries routes in registration order; a route whose path
// matches but whose method doesn't is a "keep trying", not a final 405.
// Without this, that fall-through reaches the SPA PathPrefix("/") handler
// registered later in main.go, which matches any method and serves 200
// index.html — e.g. POST /api/packets hitting the GET-only route.
//
// Called as the last statement of RegisterRoutes, so it sits after every
// real /api/* route (registered earlier in the same call) and before
// main.go registers /ws and the SPA catch-all (registered after
// RegisterRoutes returns). Every caller of RegisterRoutes — main.go and
// every test's setupTestServer — gets the fallback for free.
func registerAPIFallback(router *mux.Router) {
	router.PathPrefix("/api/").HandlerFunc(apiFallbackHandler(router))
}

// apiFallbackHandler responds 405 with an Allow header if the request path
// matches a known /api route under a different method, otherwise 404. Both
// use the existing writeError JSON shape.
func apiFallbackHandler(router *mux.Router) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if allowed := allowedMethodsForPath(router, r); len(allowed) > 0 {
			w.Header().Set("Allow", strings.Join(allowed, ", "))
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		writeError(w, http.StatusNotFound, "not found")
	}
}

// allowedMethodsForPath walks the router's registered /api/* routes and
// returns the sorted, deduplicated set of methods whose route would match
// r's path if r had been sent with that method. It reuses mux's own route
// matching (path templates, {params}, etc.) rather than reimplementing it —
// the same trick buildOpenAPISpec uses to list routes.
func allowedMethodsForPath(router *mux.Router, r *http.Request) []string {
	seen := map[string]bool{}
	router.Walk(func(route *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		path, err := route.GetPathTemplate()
		if err != nil || !strings.HasPrefix(path, "/api/") {
			return nil
		}
		methods, err := route.GetMethods()
		if err != nil {
			// Routes without .Methods() — this fallback itself — match any
			// method, so they can never contribute an Allow entry.
			return nil
		}
		for _, m := range methods {
			if seen[m] {
				continue
			}
			testReq := r.Clone(r.Context())
			testReq.Method = m
			var match mux.RouteMatch
			if route.Match(testReq, &match) {
				seen[m] = true
			}
		}
		return nil
	})
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}
