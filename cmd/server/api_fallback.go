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
//
// Bare /api is an API path too, so it gets its own exact-path route;
// PathPrefix("/api") would also swallow SPA paths like /api-docs.
func registerAPIFallback(router *mux.Router) {
	h := apiFallbackHandler(router)
	router.Path("/api").HandlerFunc(h).Name(apiFallbackRootRouteName)
	router.PathPrefix("/api/").HandlerFunc(h).Name(apiFallbackRouteName)
}

// Route names of the two fallback routes, so apiRoutesShadowedByFallback
// can find them.
const (
	apiFallbackRootRouteName = "api-fallback-root"
	apiFallbackRouteName     = "api-fallback"
)

// apiFallbackHandler serves HEAD through the GET route for the same path,
// otherwise responds 405 with an Allow header if the request path matches a
// known /api route under a different method, otherwise 404. Both errors use
// the existing writeError JSON shape.
func apiFallbackHandler(router *mux.Router) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			if h, getReq := getRouteHandler(router, r); h != nil {
				h.ServeHTTP(w, getReq)
				return
			}
		}
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
	if seen[http.MethodGet] {
		// Every GET route also answers HEAD, via apiFallbackHandler.
		seen[http.MethodHead] = true
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// getRouteHandler returns the handler of the route a GET to r's URL would
// reach, and that GET request with the route's path variables set, or nil if
// only this fallback matches. gorilla/mux's .Methods("GET") does not match
// HEAD, so HEAD on a known route lands here; RFC 9110 §9.3.2 wants it served
// like GET. The handler runs as GET, and net/http drops the body because the
// connection's request is HEAD. The router middleware has already run around
// this fallback, so the route's own handler is called directly, not
// match.Handler or router.ServeHTTP: that would run the middleware twice and
// could re-enter this fallback.
func getRouteHandler(router *mux.Router, r *http.Request) (http.Handler, *http.Request) {
	getReq := r.Clone(r.Context())
	getReq.Method = http.MethodGet
	var match mux.RouteMatch
	if !router.Match(getReq, &match) || match.MatchErr != nil {
		return nil, nil
	}
	if _, err := match.Route.GetMethods(); err != nil {
		// A route without .Methods(): this fallback, so no GET route exists.
		return nil, nil
	}
	return match.Route.GetHandler(), mux.SetURLVars(getReq, match.Vars)
}

// apiRoutesShadowedByFallback returns the path template of every /api route
// registered after the API fallback. mux tries routes in registration order
// and the fallback matches every method and path under /api, so such a
// route is never reached. main checks the production router at startup;
// TestProductionRouterHasNoShadowedAPIRoutes checks newHTTPRouter.
func apiRoutesShadowedByFallback(router *mux.Router) []string {
	var shadowed []string
	fallbackSeen := false
	router.Walk(func(route *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		switch route.GetName() {
		case apiFallbackRootRouteName, apiFallbackRouteName:
			fallbackSeen = true
			return nil
		}
		if !fallbackSeen {
			return nil
		}
		if tmpl, err := route.GetPathTemplate(); err == nil && (tmpl == "/api" || strings.HasPrefix(tmpl, "/api/")) {
			shadowed = append(shadowed, tmpl)
		}
		return nil
	})
	return shadowed
}
