package main

// #723 M3 follow-up from the #762 review: every heritage error response
// (429, 502, 503, 504) says Cache-Control: no-store itself, as the masts
// proxy's do (masts_proxy.go writeFetchError). The router's /api/*
// middleware (noStoreAPIMiddleware, #1551) already adds it on the way
// out, so these tests call the handler directly, without the router: the
// handler must not depend on that middleware for its refusals.

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/meshcore-analyzer/dhm"
)

// direct serves one request through the handler alone, no middleware.
func (e *heritageEnv) direct(query string, mod ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/api/terrain/heritage?"+query, nil)
	for _, m := range mod {
		m(req)
	}
	w := httptest.NewRecorder()
	e.srv.handleHeritageInfo(w, req)
	return w
}

func wantNoStore(t *testing.T, what string, w *httptest.ResponseRecorder) {
	t.Helper()
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("%s: status %d, Cache-Control %q, want no-store", what, w.Code, got)
	}
}

func TestHeritageQuery_ErrorResponsesAreNoStore(t *testing.T) {
	t.Run("429", func(t *testing.T) {
		env := newHeritageEnv(t, HeritageConfig{RateLimitPerMin: intp(1)}, &fakeHeritage{page: heritageFixturePage(t)})
		client := fromIP("198.51.100.7")
		decodeHeritage(t, env.direct(at(526400, 6179600), client))
		w := env.direct(at(527400, 6179600), client)
		wantError(t, w, http.StatusTooManyRequests, "rate limit exceeded")
		wantNoStore(t, "429", w)
	})
	t.Run("502", func(t *testing.T) {
		env := newHeritageEnv(t, HeritageConfig{}, &fakeHeritage{err: dhm.FetchError{Reason: "status 500"}})
		w := env.direct(at(526400, 6179600))
		wantError(t, w, http.StatusBadGateway, "heritage data unavailable")
		wantNoStore(t, "502", w)
	})
	t.Run("504", func(t *testing.T) {
		env := newHeritageEnv(t, HeritageConfig{}, &fakeHeritage{err: dhm.FetchError{Reason: "timeout"}})
		w := env.direct(at(526400, 6179600))
		wantError(t, w, http.StatusGatewayTimeout, "heritage data timed out")
		wantNoStore(t, "504", w)
	})
	t.Run("503", func(t *testing.T) {
		src := &fakeHeritage{page: heritageFixturePage(t), gate: make(chan struct{})}
		env := newHeritageEnv(t, HeritageConfig{MaxInFlight: intp(1)}, src)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			env.direct(at(540400, 6179600), fromIP("198.51.100.8"))
		}()
		waitUntil(t, "fetch admitted", func() bool { return env.q.admit.InUse() == 1 })
		w := env.direct(at(526400, 6179600), fromIP("198.51.100.7"))
		close(src.gate)
		wg.Wait()
		wantError(t, w, http.StatusServiceUnavailable, "heritage lookups busy")
		wantNoStore(t, "503", w)
	})
	// Through the router as well, where the middleware sets it too: the
	// two must agree rather than stack.
	t.Run("router", func(t *testing.T) {
		env := newHeritageEnv(t, HeritageConfig{}, &fakeHeritage{err: dhm.FetchError{Reason: "status 500"}})
		w := env.get(at(526400, 6179600))
		wantNoStore(t, "router 502", w)
		if n := len(w.Header().Values("Cache-Control")); n != 1 {
			t.Fatalf("%d Cache-Control headers", n)
		}
	})
}
