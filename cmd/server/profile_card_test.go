package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/profilecard"
)

// #727 M3 — GET /api/terrain/profile.png, the share card, and the /p tags
// that point at it. Every test runs on a fake ElevationSource: nothing
// reaches Dataforsyningen and no real token exists here.

// newCardEnv is newShareEnv with sharing on and pc for the rest.
func newCardEnv(t *testing.T, pc dhm.ProfileConfig) *shareEnv {
	t.Helper()
	pc.Share.Enabled = true
	return newShareEnvWith(t, pc, nil)
}

func (e *shareEnv) card(query string, mod ...func(*http.Request)) *httptest.ResponseRecorder {
	return e.share("/api/terrain/profile.png?"+query, mod...)
}

// cardPt is a point d metres east of Aarhus: distinct d, distinct profile.
func cardPt(d float64) string { return offsetPt(56.1629, 10.2039, d, 0) }

func cardQuery(d float64, extra ...string) string { return pathQuery(aarhusPt, cardPt(d), extra...) }

// wantCard checks a 200 PNG of 1200×630 with the given X-CoreScope-Card
// state and the Cache-Control that goes with it.
func wantCard(t *testing.T, w *httptest.ResponseRecorder, state string) []byte {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	h := w.Header()
	if h.Get("Content-Type") != "image/png" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers %v", h)
	}
	if got := h.Get("X-CoreScope-Card"); got != state {
		t.Fatalf("X-CoreScope-Card %q, want %q", got, state)
	}
	wantCC := "public, max-age=86400, immutable"
	if state == "degraded" {
		wantCC = "public, max-age=60"
	}
	if got := h.Get("Cache-Control"); got != wantCC {
		t.Fatalf("Cache-Control %q, want %q", got, wantCC)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(w.Body.Bytes()))
	if err != nil || cfg.Width != 1200 || cfg.Height != 630 {
		t.Fatalf("not a 1200×630 PNG: %v %+v", err, cfg)
	}
	return w.Body.Bytes()
}

// wantCardError checks a refusal: the fixed JSON error, never cached,
// never sniffed.
func wantCardError(t *testing.T, w *httptest.ResponseRecorder, code int, msg string) {
	t.Helper()
	wantError(t, w, code, msg)
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("refusal headers %v", w.Header())
	}
	if got := w.Header().Get("X-CoreScope-Card"); got != "refused" {
		t.Fatalf("X-CoreScope-Card %q on a refusal", got)
	}
}

// fillSlots takes every slot of adm with calls that block until release.
func fillSlots(adm *dhm.Admission) (release func()) {
	var f dhm.Flight[[]byte]
	block := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < adm.Cap(); i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f.DoAdmitted(context.Background(), fmt.Sprint("fill-", i), 0, adm, func(context.Context) ([]byte, error) {
				<-block
				return nil, nil
			})
		}(i)
	}
	deadline := time.Now().Add(5 * time.Second)
	for adm.InUse() < adm.Cap() {
		if time.Now().After(deadline) {
			panic("fillSlots: slots never filled")
		}
		time.Sleep(time.Millisecond)
	}
	return func() { close(block); wg.Wait() }
}

// ── Route and headers ──────────────────────────────────────────────────────

func TestProfileCard_OffIs404(t *testing.T) {
	off := newShareEnv(t, false, nil)
	wantCardError(t, off.card(cardQuery(3000)), 404, "not found")
	_, router := setupTestServer(t) // no profile at all
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/terrain/profile.png?"+cardQuery(3000), nil))
	wantCardError(t, w, 404, "not found")
}

func TestProfileCard_CompleteCardThenHit(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{})
	first := wantCard(t, env.card(cardQuery(3000)), "miss")
	calls := env.src.calls.Load()
	if calls == 0 {
		t.Fatal("a cold card made no upstream call")
	}
	second := wantCard(t, env.card(cardQuery(3000)), "hit")
	if !bytes.Equal(first, second) || env.src.calls.Load() != calls {
		t.Fatal("a cache hit re-rendered or went upstream")
	}
	// The card warmed the elevation cache: the JSON endpoint is a hit.
	w := env.get(cardQuery(3000))
	if w.Header().Get("X-CoreScope-Cache") != "hit" || env.src.calls.Load() != calls {
		t.Fatalf("JSON after card: %q", w.Header().Get("X-CoreScope-Cache"))
	}
}

// A path entirely outside DHM needs no upstream call: the card is complete
// (immutable) at once, even with no share budget at all.
func TestProfileCard_OutsideCoverageIsCompleteWithoutUpstream(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{Share: dhm.ShareConfig{UpstreamPerMin: intp(0)}})
	wantCard(t, env.card(pathQuery("50.00000,5.00000", "50.05000,5.00000")), "miss")
	if n := env.src.calls.Load(); n != 0 {
		t.Fatalf("%d upstream calls", n)
	}
}

func TestProfileCard_BadRequestsAre400(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{})
	for _, q := range []string{
		"", "a=" + aarhusPt, cardQuery(3000, "x=1"), cardQuery(3000, "ha=-1"), cardQuery(3000, "model=dsm"),
		cardQuery(3000, "v=1", "v=2"), cardQuery(3000, "v="+strings.Repeat("a", 65)),
		pathQuery(aarhusPt, aarhusPt), pathQuery(aarhusPt, cardPt(90000)),
	} {
		w := env.card(q)
		if w.Code != 400 || w.Header().Get("X-CoreScope-Card") != "refused" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%q: status %d %v", q, w.Code, w.Header())
		}
	}
	if n := env.src.calls.Load(); n != 0 {
		t.Fatalf("%d upstream calls for bad requests", n)
	}
}

// v is a preview cache-bust: any value up to 64 bytes is accepted and
// ignored, so a hand-edited link still works and every v is one card.
func TestProfileCard_UnknownVIsIgnored(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{})
	a := wantCard(t, env.card(cardQuery(3000, "v=abc")), "miss")
	b := wantCard(t, env.card(cardQuery(3000, "v=not-a-hash-at-all")), "hit")
	c := wantCard(t, env.card(cardQuery(3000)), "hit")
	if !bytes.Equal(a, b) || !bytes.Equal(a, c) {
		t.Fatal("v changed the card")
	}
}

// ── Privacy: the #747 gate ─────────────────────────────────────────────────

// Hidden (here through an observer alias, #743), unknown and unpositioned
// identities all get the same refusal, before any charge or upstream
// call, and no name ever reaches the renderer.
func TestProfileCard_HiddenUnknownUnpositionedAreTheSame(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{})
	var seen []string
	env.p.card.render = func(resp profilecard.TerrainProfileResponse, names [2]string, pal profilecard.Palette, opt profilecard.Options) ([]byte, error) {
		seen = append(seen, names[0], names[1])
		return profilecard.Render(resp, names, pal, opt)
	}
	hiddenKey := strings.Repeat("3", 64)
	env.addNode(t, hiddenKey, "Secret Roof", "repeater", 56.15, 10.2)
	if _, err := env.srv.db.conn.Exec(`INSERT INTO observers (id, name) VALUES (?, 'private-observer')`, strings.ToUpper(hiddenKey)); err != nil {
		t.Fatal(err)
	}
	env.srv.cfg.SetHiddenNamePrefixes([]string{"private-"})
	unplaced := strings.Repeat("4", 64)
	env.addNode(t, unplaced, "No Fix", "repeater", nil, nil)
	env.srv.store.InvalidateNodeCache()
	if hidden, err := env.srv.isIdentityHidden(context.Background(), hiddenKey); err != nil || !hidden {
		t.Fatalf("precondition: alias not hidden (%v, %v)", hidden, err)
	}

	ref := env.card(pathQuery(strings.Repeat("d", 64), aarhusPt))
	wantCardError(t, ref, 404, "node not found")
	for _, q := range []string{
		pathQuery(hiddenKey, aarhusPt), pathQuery(aarhusPt, hiddenKey), pathQuery(strings.ToUpper(hiddenKey), aarhusPt),
		pathQuery(unplaced, aarhusPt),
	} {
		w := env.card(q)
		wantCardError(t, w, 404, "node not found")
		if w.Body.String() != ref.Body.String() || len(w.Header()) != len(ref.Header()) {
			t.Fatalf("%s: not the same refusal as an unknown node", q)
		}
	}
	if len(seen) != 0 || env.src.calls.Load() != 0 || env.p.card.cache.Len() != 0 {
		t.Fatalf("a refused identity reached the renderer (%q), upstream or the cache", seen)
	}
	// The visible node is named; a point never is.
	wantCard(t, env.card(pathQuery(shareKeyA, aarhusPt)), "miss")
	if len(seen) != 2 || seen[0] != "Søby Bakker" || seen[1] != "" {
		t.Fatalf("names passed to the renderer %q", seen)
	}
}

// map.dhm.profile.card.names: false keeps every name off the card.
func TestProfileCard_NamesOff(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{Card: dhm.CardConfig{Names: boolp(false)}})
	var opt profilecard.Options
	env.p.card.render = func(resp profilecard.TerrainProfileResponse, names [2]string, pal profilecard.Palette, o profilecard.Options) ([]byte, error) {
		opt = o
		return profilecard.Render(resp, names, pal, o)
	}
	wantCard(t, env.card(pathQuery(shareKeyA, shareKeyB)), "miss")
	if !opt.HideNames {
		t.Fatal("names: false did not reach the renderer")
	}
	alt := mustMeta(t, env.share(shareQ(shareKeyA, shareKeyB)).Body.String(), "og:image:alt")
	if strings.Contains(alt, "Søby") || !strings.Contains(alt, "Node A to Node B") {
		t.Fatalf("og:image:alt %q with names off", alt)
	}
}

func boolp(v bool) *bool { return &v }

// ── Caches and flight ─────────────────────────────────────────────────────

// Antenna heights, k and the frequency change the picture but not the
// elevations: an elevation hit, a card miss.
func TestProfileCard_PhysicsChangeIsElevationHitCardMiss(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{})
	base := wantCard(t, env.card(cardQuery(3000)), "miss")
	calls := env.src.calls.Load()
	seen := map[string]bool{string(base): true}
	for _, extra := range []string{"ha=9", "hb=9", "k=1", "freq=868.1", "ha=9&hb=3"} {
		b := wantCard(t, env.card(cardQuery(3000, extra)), "miss")
		if seen[string(b)] {
			t.Fatalf("%s: same picture as an earlier card", extra)
		}
		seen[string(b)] = true
	}
	if env.src.calls.Load() != calls {
		t.Fatal("a physics change went upstream")
	}
	// The model is part of the elevation key: model=dtm is a new fetch.
	wantCard(t, env.card(cardQuery(3000, "model=dtm")), "miss")
	if env.src.calls.Load() == calls {
		t.Fatal("model=dtm reused the both-model elevations")
	}
}

// A palette change (theme.json at the next start) never serves stale
// pixels: the palette is part of the card key and of v.
func TestProfileCard_PaletteChangeInvalidates(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{})
	before := wantCard(t, env.card(cardQuery(3000)), "miss")
	vBefore := mustMeta(t, env.share(shareQ(aarhusPt, cardPt(3000))).Body.String(), "og:image")
	env.p.card.usePalette(map[string]string{}, map[string]string{"terrainFill": "#ff00ff"})
	after := wantCard(t, env.card(cardQuery(3000)), "miss")
	if bytes.Equal(before, after) {
		t.Fatal("the new palette drew the same card")
	}
	if vAfter := mustMeta(t, env.share(shareQ(aarhusPt, cardPt(3000))).Body.String(), "og:image"); vAfter == vBefore {
		t.Fatal("og:image (v) did not change with the palette")
	}
}

// Two identical concurrent requests render once.
func TestProfileCard_FlightRendersOnce(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{})
	// Warm the elevations, so the two requests meet at the render.
	decodeProfile(t, env.get(cardQuery(3000)))
	var renders atomic.Int64
	gate := make(chan struct{})
	env.p.card.render = func(resp profilecard.TerrainProfileResponse, names [2]string, pal profilecard.Palette, opt profilecard.Options) ([]byte, error) {
		renders.Add(1)
		<-gate
		return profilecard.Render(resp, names, pal, opt)
	}
	var wg sync.WaitGroup
	out := make([]*httptest.ResponseRecorder, 2)
	for i := range out {
		wg.Add(1)
		go func(i int) { defer wg.Done(); out[i] = env.card(cardQuery(3000)) }(i)
	}
	waitUntil(t, "both requests waiting on one render", func() bool { return env.p.card.flight.Pending() == 2 })
	close(gate)
	wg.Wait()
	if n := renders.Load(); n != 1 {
		t.Fatalf("%d renders for two identical requests", n)
	}
	if !bytes.Equal(wantCard(t, out[0], "miss"), wantCard(t, out[1], "miss")) {
		t.Fatal("the two requests got different cards")
	}
}

// The card cache is bounded by entries and by bytes: a card is ~90 KB
// (#755 review), so the byte bound is cacheEntries × cardBytesPerEntry.
func TestProfileCard_CacheBounds(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{Card: dhm.CardConfig{CacheEntries: intp(2)}, Share: dhm.ShareConfig{UpstreamPerMin: intp(60)}})
	for i := 0; i < 4; i++ {
		wantCard(t, env.card(cardQuery(3000+float64(i)*500)), "miss")
	}
	if n := env.p.card.cache.Len(); n != 2 {
		t.Fatalf("%d cards cached with cacheEntries 2", n)
	}
	if got := env.p.card.cache.Size(); got > 2*cardBytesPerEntry || got < 2*60_000 {
		t.Fatalf("cache holds %d bytes for 2 cards", got)
	}
	// The byte bound on its own: three over-sized cards in room for two.
	c := newCardCache(4, time.Hour)
	big := make([]byte, 2*cardBytesPerEntry-1)
	for i := 0; i < 3; i++ {
		c.Put(cardKey{Inputs: [16]byte{byte(i)}}, big, time.Unix(0, 0))
	}
	if c.Len() != 2 || c.Size() > 4*cardBytesPerEntry {
		t.Fatalf("byte bound: %d entries, %d bytes", c.Len(), c.Size())
	}
	if cardBytesPerEntry < 94_000 {
		t.Fatalf("cardBytesPerEntry %d is below a real card (88–94 KB)", cardBytesPerEntry)
	}
}

// ── Budgets ────────────────────────────────────────────────────────────────

// shareUpstreamPerMin 0: sharing never goes upstream. Every cold card is a
// 200 degraded card, refreshed by crawlers within a minute, and the
// operator hears about it once per minute, not once per request.
func TestProfileCard_ZeroBudgetNeverGoesUpstream(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{Share: dhm.ShareConfig{UpstreamPerMin: intp(0)}, RateLimitPerMin: intp(0)})
	clock := time.Unix(1_700_000_000, 0)
	env.p.now = func() time.Time { return clock }
	for i := 0; i < 20; i++ {
		wantCard(t, env.card(cardQuery(3000+float64(i)*100)), "degraded")
	}
	if n := env.src.calls.Load(); n != 0 {
		t.Fatalf("%d upstream calls with a zero budget", n)
	}
	if env.p.card.cache.Len() != 0 {
		t.Fatal("a degraded card was cached")
	}
	lines := strings.Count(env.logs.String(), "share.upstreamPerMin")
	if lines != 1 || !strings.Contains(env.logs.String(), "degraded") {
		t.Fatalf("%d log lines for 20 degraded cards: %q", lines, env.logs.String())
	}
	clock = clock.Add(time.Minute + time.Second)
	wantCard(t, env.card(cardQuery(9000)), "degraded")
	if !strings.Contains(env.logs.String(), "20 degraded card") || strings.Contains(strings.ToLower(env.logs.String()), "token") {
		t.Fatalf("second report: %q", env.logs.String())
	}
}

// The budget is server-wide (not per IP) and refills at upstreamPerMin.
func TestProfileCard_BudgetRefills(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{Share: dhm.ShareConfig{UpstreamPerMin: intp(2)}, RateLimitPerMin: intp(0)})
	clock := time.Unix(1_700_000_000, 0)
	env.p.now = func() time.Time { return clock }
	ip := func(n int) func(*http.Request) {
		return func(r *http.Request) { r.RemoteAddr = fmt.Sprintf("198.51.100.%d:1000", n) }
	}
	wantCard(t, env.card(cardQuery(3000), ip(1)), "miss")
	wantCard(t, env.card(cardQuery(3500), ip(2)), "miss")
	wantCard(t, env.card(cardQuery(4000), ip(3)), "degraded") // burst of 2 spent, whoever asks
	calls := env.src.calls.Load()
	clock = clock.Add(29 * time.Second)
	wantCard(t, env.card(cardQuery(4000), ip(4)), "degraded")
	clock = clock.Add(2 * time.Second) // 31 s: one token back at 2/min
	wantCard(t, env.card(cardQuery(4000), ip(5)), "miss")
	wantCard(t, env.card(cardQuery(4500), ip(6)), "degraded")
	if env.src.calls.Load() == calls {
		t.Fatal("the refilled token did not fetch")
	}
	// Hits never spend the budget.
	for i := 0; i < 10; i++ {
		wantCard(t, env.card(cardQuery(3000), ip(7)), "hit")
	}
	// A JSON-only elevation hit is not upstream work either.
	decodeProfile(t, env.get(cardQuery(5000)))
	clock = clock.Add(10 * time.Second)
	wantCard(t, env.card(cardQuery(5000), ip(8)), "miss")
}

// An upstream failure or a slow upstream still answers 200 with a
// degraded card that crawlers re-fetch within a minute.
func TestProfileCard_UpstreamFailureIsDegraded(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
	}{
		{"502", errors.New("upstream broke")},
		{"504", context.DeadlineExceeded},
		{"fetch error", dhm.FetchError{Reason: "status"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newCardEnv(t, dhm.ProfileConfig{})
			env.src.hook = func(context.Context, dhm.RasterRequest) error { return c.err }
			wantCard(t, env.card(cardQuery(3000)), "degraded")
			if strings.Contains(env.logs.String(), "upstream broke") {
				t.Fatal("upstream text reached the log")
			}
		})
	}
	t.Run("slow", func(t *testing.T) {
		env := newCardEnv(t, dhm.ProfileConfig{})
		env.p.card.wait = 50 * time.Millisecond
		env.src.hook = func(ctx context.Context, _ dhm.RasterRequest) error { <-ctx.Done(); return ctx.Err() }
		start := time.Now()
		wantCard(t, env.card(cardQuery(3000)), "degraded")
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("the card waited %v for a slow upstream", d)
		}
	})
}

// maxInFlight full: 503 with Retry-After before any work, and both the
// per-IP charge and the share token are given back, so a client retrying
// as told never meets 429 and the budget is not drained by refusals.
func TestProfileCard_AdmissionFullIs503AndRefunds(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{Share: dhm.ShareConfig{UpstreamPerMin: intp(1)}, RateLimitPerMin: intp(3)})
	clock := time.Unix(1_700_000_000, 0)
	env.p.now = func() time.Time { return clock }
	client := func(r *http.Request) { r.RemoteAddr = "198.51.100.9:1000" }
	release := fillSlots(env.p.admit)
	for i := 0; i < 10; i++ {
		w := env.card(cardQuery(3000), client)
		wantCardError(t, w, http.StatusServiceUnavailable, "terrain profiles busy")
		if w.Header().Get("Retry-After") != "5" {
			t.Fatalf("Retry-After %q", w.Header().Get("Retry-After"))
		}
	}
	release()
	if env.src.calls.Load() != 0 {
		t.Fatal("a refused card went upstream")
	}
	// The one share token survived ten refusals, and so did the per-IP
	// budget of 3: one complete card, then two degraded (the token is
	// spent), then 429.
	wantCard(t, env.card(cardQuery(3000), client), "miss")
	wantCard(t, env.card(cardQuery(3500), client), "degraded")
	wantCard(t, env.card(cardQuery(4000), client), "degraded")
	wantCardError(t, env.card(cardQuery(4500), client), http.StatusTooManyRequests, "rate limit exceeded")
}

// Renders have their own admission: a full one is 503, refunded too.
func TestProfileCard_RenderAdmissionFullIs503(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{RateLimitPerMin: intp(1)})
	client := func(r *http.Request) { r.RemoteAddr = "198.51.100.10:1000" }
	release := fillSlots(env.p.card.admit)
	for i := 0; i < 5; i++ {
		wantCardError(t, env.card(cardQuery(3000), client), http.StatusServiceUnavailable, "terrain profiles busy")
	}
	release()
	wantCard(t, env.card(cardQuery(3000), client), "miss")
}

// A share request that joins a fetch already running adds no upstream
// work, so it gives its share token back.
func TestProfileCard_JoiningAFetchRefundsTheToken(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{Share: dhm.ShareConfig{UpstreamPerMin: intp(2)}, RateLimitPerMin: intp(0)})
	clock := time.Unix(1_700_000_000, 0)
	env.p.now = func() time.Time { return clock }
	gate := make(chan struct{})
	var once sync.Once
	env.src.hook = func(ctx context.Context, _ dhm.RasterRequest) error {
		select {
		case <-gate:
		case <-ctx.Done():
		}
		return ctx.Err()
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); wantCard(t, env.card(cardQuery(3000)), "miss") }()
	}
	waitUntil(t, "two card requests on one fetch", func() bool { return env.p.flight.Pending() == 2 })
	once.Do(func() { close(gate) })
	wg.Wait()
	if got := env.p.card.budget.available(clock); got < 0.99 || got > 1.01 {
		t.Fatalf("%.2f tokens left after one fetch shared by two cards, want 1", got)
	}
}

// ── Settings ───────────────────────────────────────────────────────────────

func TestProfileCard_Settings(t *testing.T) {
	logs := &dhmLogSink{}
	if s := resolveTerrainSettings(dhm.ProfileConfig{}, logs.logf).card; s != (cardSettings{dark: true, names: true, cacheEntries: 64}) {
		t.Fatalf("defaults %+v", s)
	}
	s := resolveTerrainSettings(dhm.ProfileConfig{Card: dhm.CardConfig{Theme: "light", Names: boolp(false), CacheEntries: intp(9)}}, logs.logf).card
	if s != (cardSettings{dark: false, names: false, cacheEntries: 9}) {
		t.Fatalf("set %+v", s)
	}
	for _, bad := range []dhm.CardConfig{{Theme: "sepia"}, {CacheEntries: intp(0)}, {CacheEntries: intp(1025)}} {
		if s := resolveTerrainSettings(dhm.ProfileConfig{Card: bad}, logs.logf).card; s != (cardSettings{dark: true, names: true, cacheEntries: 64}) {
			t.Fatalf("%+v → %+v", bad, s)
		}
	}
	if !strings.Contains(logs.String(), "card.theme") || !strings.Contains(logs.String(), "card.cacheEntries") || strings.Contains(logs.String(), "sepia") {
		t.Fatalf("warnings %q", logs.String())
	}
	cfgJSON := `{"map":{"dhm":{"profile":{"enabled":true,"card":{"theme":"light","names":false,"cacheEntries":12}}}}}`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cc := cfg.DHM.Profile.Card; cc.Theme != "light" || cc.Names == nil || *cc.Names || cc.CacheEntries == nil || *cc.CacheEntries != 12 {
		t.Fatalf("card config %+v", cc)
	}
}

// The card palette is the browser's: /api/config/theme's sections, with
// dark mode merged as Object.assign({}, theme, themeDark) (#755 review).
func TestProfileCard_PaletteFromTheServerTheme(t *testing.T) {
	cfg := &Config{
		Theme:     map[string]interface{}{"terrainFill": "#123456", "losLine": "#abcdef", "fresnel60": 7},
		ThemeDark: map[string]interface{}{"losLine": "#fedcba"},
	}
	light, dark := cardThemeSections(cfg, &ThemeFile{ThemeDark: map[string]interface{}{"fresnel100": "#010203"}})
	pal, bad := profilecard.ResolveTheme(light, dark, true)
	want := profilecard.DefaultPalette(true)
	if c := pal.TerrainFill; c.R != 0x12 || c.G != 0x34 || c.B != 0x56 {
		t.Errorf("dark terrainFill %v: a theme-only key must apply in dark mode", c)
	}
	if c := pal.LosLine; c.R != 0xfe {
		t.Errorf("dark losLine %v: themeDark wins", c)
	}
	if c := pal.Fresnel100; c.R != 1 || c.G != 2 || c.B != 3 {
		t.Errorf("theme.json themeDark not applied: %v", c)
	}
	if pal.Background != want.Background || pal.Text != want.Text {
		t.Errorf("the server's dark defaults must keep the dark background and text")
	}
	if pal.Fresnel60 != want.Fresnel60 || len(bad) != 0 {
		t.Errorf("a non-string value must be dropped at the boundary, not reported as a colour: %v %v", pal.Fresnel60, bad)
	}
	lightPal, _ := profilecard.ResolveTheme(light, dark, false)
	if c := lightPal.LosLine; c.R != 0xab {
		t.Errorf("light losLine %v", c)
	}
	// /api/config/theme serves the same sections it always did.
	srv, router := setupTestServer(t)
	srv.cfg.Theme = cfg.Theme
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/config/theme", nil))
	if !strings.Contains(w.Body.String(), `"terrainFill":"#123456"`) || !strings.Contains(w.Body.String(), `"navBg":"#0f0f23"`) {
		t.Fatalf("/api/config/theme changed: %s", w.Body.String())
	}
}

// ── The /p tags point at the card ──────────────────────────────────────────

func TestShareProfile_ImageIsTheCard(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{})
	target := shareQ(shareKeyA, shareKeyB, "ha=12")
	body := env.share(target).Body.String()
	img := mustMeta(t, body, "og:image")
	u, err := url.Parse(img)
	if err != nil || u.Scheme != "http" || u.Host != "example.test" || u.Path != "/api/terrain/profile.png" {
		t.Fatalf("og:image %q", img)
	}
	q := u.Query()
	if q.Get("a") != shareKeyA || q.Get("b") != shareKeyB || q.Get("ha") != "12" || q.Get("model") != "both" || len(q.Get("v")) != 12 {
		t.Fatalf("og:image query %v", q)
	}
	if mustMeta(t, body, "twitter:image") != img {
		t.Fatal("twitter:image is not the card")
	}
	if mustMeta(t, body, "og:image:width") != "1200" || mustMeta(t, body, "og:image:height") != "630" {
		t.Fatal("og:image dimensions are not the card's")
	}
	alt := mustMeta(t, body, "og:image:alt")
	if !strings.HasPrefix(alt, "Terrain profile from Søby Bakker to Ørsted Mast, 11.1 km. No verdict.") {
		t.Fatalf("og:image:alt %q", alt)
	}
	if mustMeta(t, body, "twitter:image:alt") != alt {
		t.Fatal("twitter:image:alt differs")
	}
	if got := titleOf(t, body); got != "Path profile: 11.1 km" {
		t.Fatalf("title before the profile is cached: %q", got)
	}
	// The og:image URL serves the card.
	wantCard(t, env.card(u.RawQuery), "miss")

	// Now cached: the title gains the card's verdict word, the alt its
	// verdict, and v changes so a crawler fetches the complete card.
	body2 := env.share(target).Body.String()
	img2 := mustMeta(t, body2, "og:image")
	if img2 == img {
		t.Fatal("v did not change once the profile was cached")
	}
	word := profilecard.MainVerdict(env.cachedResponse(t, target)).Word
	if word == "" || word == "No verdict" {
		t.Fatalf("precondition: the fake terrain gives verdict %q", word)
	}
	for _, k := range []string{"og:title", "twitter:title"} {
		if got := mustMeta(t, body2, k); got != "Path profile: 11.1 km · "+word {
			t.Fatalf("%s %q", k, got)
		}
	}
	if got := titleOf(t, body2); got != "Path profile: 11.1 km · "+word {
		t.Fatalf("title %q", got)
	}
	if alt2 := mustMeta(t, body2, "og:image:alt"); !strings.Contains(alt2, ". "+word+". ") && !strings.Contains(alt2, "Terrain: "+word+". ") {
		t.Fatalf("og:image:alt %q lacks %q", alt2, word)
	}
	u2, _ := url.Parse(img2)
	wantCard(t, env.card(u2.RawQuery), "hit")
	// Still no upstream call from /p itself.
	calls := env.src.calls.Load()
	env.share(target)
	if env.src.calls.Load() != calls {
		t.Fatal("/p went upstream")
	}
}

// cachedResponse is the profile /p and the card see for a share target.
func (e *shareEnv) cachedResponse(t *testing.T, target string) TerrainProfileResponse {
	t.Helper()
	u, _ := url.Parse(target)
	tq, err := parseSharedProfile(u.Query())
	if err != nil {
		t.Fatal(err)
	}
	req, rerr := e.srv.resolveTerrainRequest(context.Background(), e.p, tq)
	if rerr != nil {
		t.Fatal(rerr.msg)
	}
	el, ok := e.p.cachedElevation(req)
	if !ok {
		t.Fatal("not cached")
	}
	return e.p.buildResponse(req.q, req.ends, req.a, req.b, &req.plan, el)
}

// The share card's verdict word and the /p title's agree for every
// verdict the terrain can give.
func TestShareProfile_TitleAndCardAgree(t *testing.T) {
	for _, tc := range []struct {
		name string
		elev func(e, n float64) float32
	}{
		{"flat", flatElev(0)},
		{"ridge", func(e, n float64) float32 {
			e0, _ := dhm.ToUTM32(56.1629, 10.2039)
			if d := e - e0; d > 1400 && d < 1600 {
				return 60
			}
			return 0
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newCardEnv(t, dhm.ProfileConfig{})
			env.src.elev = tc.elev
			var drawn string
			env.p.card.render = func(resp profilecard.TerrainProfileResponse, names [2]string, pal profilecard.Palette, opt profilecard.Options) ([]byte, error) {
				drawn = profilecard.MainVerdict(resp).Word
				return profilecard.Render(resp, names, pal, opt)
			}
			target := shareQ(aarhusPt, cardPt(3000))
			before := titleOf(t, env.share(target).Body.String())
			wantCard(t, env.card(cardQuery(3000)), "miss")
			if got := titleOf(t, env.share(target).Body.String()); drawn == "" || got != before+" · "+drawn {
				t.Fatalf("title %q, card %q", got, drawn)
			}
		})
	}
}

// ── OpenAPI and perf ──────────────────────────────────────────────────────

func TestProfileCard_OpenAPI(t *testing.T) {
	m, ok := routeDescriptions()["GET /api/terrain/profile.png"]
	if !ok {
		t.Fatal("GET /api/terrain/profile.png undocumented")
	}
	codes := map[string]bool{}
	for _, e := range m.Errors {
		codes[e.Status] = true
	}
	for _, c := range []string{"400", "404", "429", "503"} {
		if !codes[c] {
			t.Fatalf("error %s undocumented", c)
		}
	}
	if !strings.Contains(m.Description, "shareUpstreamPerMin") && !strings.Contains(m.Description, "share.upstreamPerMin") {
		t.Fatal("the share budget is undocumented")
	}
}

// Perf proof (AGENTS.md rule 0): a cache hit costs microseconds, a render
// from cached elevations stays inside the renderer's 25 ms budget plus
// the profile build. Run with -v for the numbers.
func TestProfileCard_Cost(t *testing.T) {
	if testing.Short() || raceBuild {
		t.Skip("timing")
	}
	env := newCardEnv(t, dhm.ProfileConfig{RateLimitPerMin: intp(0)})
	decodeProfile(t, env.get(pathQuery(aarhusPt, cardPt(79000))))
	q := pathQuery(aarhusPt, cardPt(79000))
	i := 0
	miss := testing.Benchmark(func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			i++
			if w := env.card(q + fmt.Sprintf("&ha=%d", i%100)); w.Code != 200 {
				b.Fatalf("status %d", w.Code)
			}
			env.p.card.cache = newCardCache(64, time.Hour)
		}
	})
	hit := testing.Benchmark(func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			if w := env.card(q); w.Code != 200 {
				b.Fatalf("status %d", w.Code)
			}
		}
	})
	t.Logf("render from cached elevations (80 km): %v/op, %d B/op", time.Duration(miss.NsPerOp()), miss.AllocedBytesPerOp())
	t.Logf("card cache hit: %v/op, %d B/op", time.Duration(hit.NsPerOp()), hit.AllocedBytesPerOp())
	if miss.NsPerOp() > int64(60*time.Millisecond) || hit.NsPerOp() > int64(2*time.Millisecond) {
		t.Fatalf("card costs: render %v, hit %v", time.Duration(miss.NsPerOp()), time.Duration(hit.NsPerOp()))
	}
}

// The card's host line is the configured public base, never a request's
// Host header: one card serves every viewer, and a Host cannot be echoed
// into a picture.
func TestProfileCard_SiteIsThePublicHost(t *testing.T) {
	for _, tc := range []struct {
		base, want string
	}{{"https://share.example/sub/", "share.example"}, {"", ""}} {
		brand := map[string]string{}
		if tc.base != "" {
			brand["publicBaseUrl"] = tc.base
		}
		env := newShareEnvWith(t, dhm.ProfileConfig{Share: dhm.ShareConfig{Enabled: true}}, brand)
		var site string
		env.p.card.render = func(resp profilecard.TerrainProfileResponse, names [2]string, pal profilecard.Palette, o profilecard.Options) ([]byte, error) {
			site = o.Site
			return profilecard.Render(resp, names, pal, o)
		}
		wantCard(t, env.card(cardQuery(3000), func(r *http.Request) { r.Host = "evil.example" }), "miss")
		if site != tc.want {
			t.Fatalf("publicBaseUrl %q: site %q, want %q", tc.base, site, tc.want)
		}
	}
}
