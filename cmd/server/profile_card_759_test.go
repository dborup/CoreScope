package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/copdem"
	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/profilecard"
)

// #759 review (CS-VM3) — the fixround's pins. Every test runs on the fake
// ElevationSource of terrain_profile_test.go: nothing goes upstream.

// Finding 2: the card credits what the profile API credits. The terrain
// profile is a derived view of DHM (#733), and so is the card that
// pictures it, so both carry dhm.AttributionDerived.
func TestProfileCard_CreditIsTheProfileAttribution(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{})
	resp := decodeProfile(t, env.get(cardQuery(3000)))
	if resp.Attribution != profilecard.Credit {
		t.Fatalf("profile attribution %q, card credit %q", resp.Attribution, profilecard.Credit)
	}
}

// Finding 3: a card stops waiting for a slow upstream after its wait, but
// the fetch its share token bought runs on to the profile deadline and is
// cached, so the next card is complete and costs no further upstream
// calls. The upstream answers only when the test releases it, never on a
// timer: a degraded card under -race takes longer than any margin a timer
// could leave (#759 delta review finding 1).
func TestProfileCard_SlowFetchOutlivesTheCardWait(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{Share: dhm.ShareConfig{UpstreamPerMin: intp(5)}})
	clock := time.Unix(1_700_000_000, 0)
	env.p.now = func() time.Time { return clock }
	env.p.card.wait = 50 * time.Millisecond
	release := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	defer open()
	env.src.hook = func(ctx context.Context, _ dhm.RasterRequest) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	wantCard(t, env.card(cardQuery(3000)), "degraded")
	// A second card while the fetch runs joins it, gives its token back
	// and is degraded too: still one fetch.
	wantCard(t, env.card(cardQuery(3000)), "degraded")
	// Both cards stopped waiting; the fetch is held, in its slot.
	if n := env.p.admit.InUse(); n != 1 {
		t.Fatalf("%d slots in use after both cards, want the one held fetch", n)
	}
	if got := env.p.card.budget.available(clock); got < 3.99 || got > 4.01 {
		t.Fatalf("%.2f share tokens left, want 4: one fetch bought one profile", got)
	}
	open()
	// The fetch holds its maxInFlight slot until it is done, and no longer.
	waitUntil(t, "the held fetch to finish", func() bool { return env.p.admit.InUse() == 0 })
	calls := env.src.calls.Load()
	if calls == 0 {
		t.Fatal("the fetch never reached upstream")
	}
	wantCard(t, env.card(cardQuery(3000)), "miss")
	if n := env.src.calls.Load(); n != calls {
		t.Fatalf("the complete card cost %d more upstream calls", n-calls)
	}
	if got := env.p.card.budget.available(clock); got < 3.99 || got > 4.01 {
		t.Fatalf("%.2f share tokens left after the complete card, want 4", got)
	}
}

// Finding 3, the bound: a held fetch runs under the profile deadline, not
// the card's wait and not unbounded, so an upstream that never answers
// cannot keep a slot. The test reads the deadline off the fetch's own
// context instead of waiting for it to pass, so a slow -race render
// cannot outlast it (#759 delta review finding 1). That the deadline then
// cancels a held call is internal/dhm's TestFlightHeldCallOutlivesItsCaller.
func TestProfileCard_HeldFetchEndsAtTheDeadline(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{})
	env.p.card.wait = 20 * time.Millisecond
	// Below the per-raster timeout (10 s), so the context the upstream
	// sees carries the flight's deadline; far above any -race render.
	env.p.deadline = 5 * time.Second
	release := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	defer open()
	type seen struct {
		at, deadline time.Time
		ok           bool
	}
	fetch := make(chan seen, 8)
	env.src.hook = func(ctx context.Context, _ dhm.RasterRequest) error {
		dl, ok := ctx.Deadline()
		fetch <- seen{time.Now(), dl, ok}
		select {
		case <-release:
			return errors.New("upstream never answered")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	before := time.Now()
	wantCard(t, env.card(cardQuery(3000)), "degraded")
	if env.p.admit.InUse() != 1 {
		t.Fatalf("%d slots in use right after the card: the fetch was not held", env.p.admit.InUse())
	}
	var f seen
	select {
	case f = <-fetch:
	default:
		t.Fatal("the held fetch never reached upstream")
	}
	if !f.ok {
		t.Fatal("the held fetch has no deadline")
	}
	if lo, hi := before.Add(env.p.deadline), f.at.Add(env.p.deadline); f.deadline.Before(lo) || f.deadline.After(hi) {
		t.Fatalf("held fetch deadline %v after the card started, want the profile deadline %v", f.deadline.Sub(before), env.p.deadline)
	}
	open()
	waitUntil(t, "the held fetch to give its slot back", func() bool { return env.p.admit.InUse() == 0 })
}

// RM7: a pending (degraded) render and a complete one of the same card
// key never share a render flight. If they did, a request with the
// profile cached could join a pending render and serve its picture as an
// immutable complete card.
func TestProfileCard_PendingAndCompleteRendersDoNotShareAFlight(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{Share: dhm.ShareConfig{UpstreamPerMin: intp(0)}})
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer release()
	env.p.card.render = func(resp profilecard.TerrainProfileResponse, names [2]string, pal profilecard.Palette, opt profilecard.Options) ([]byte, error) {
		if opt.Pending {
			<-gate
		}
		return profilecard.Render(resp, names, pal, opt)
	}
	pending := make(chan []byte, 1)
	go func() { pending <- env.card(cardQuery(3000)).Body.Bytes() }()
	waitUntil(t, "the pending render to start", func() bool { return env.p.card.flight.Pending() == 1 })
	// The panel measures the profile: the elevations are now cached.
	decodeProfile(t, env.get(cardQuery(3000)))
	done := make(chan []byte, 1)
	go func() { done <- wantCard(t, env.card(cardQuery(3000)), "miss") }()
	var complete []byte
	select {
	case complete = <-done:
	case <-time.After(3 * time.Second):
		release()
		<-done
		t.Fatal("the complete card waited on the pending render")
	}
	release()
	if bytes.Equal(complete, <-pending) {
		t.Fatal("the complete card is the pending picture")
	}
}

// RM3: the endpoint names are part of the card key, so a renamed node
// gets a new card, never the cached picture with the old name.
func TestProfileCard_NamesAreInTheCardKey(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{})
	q := pathQuery(shareKeyA, shareKeyB)
	before := wantCard(t, env.card(q), "miss")
	wantCard(t, env.card(q), "hit")
	if _, err := env.srv.db.conn.Exec(`UPDATE nodes SET name = ? WHERE public_key = ?`, "Søby Nord", shareKeyA); err != nil {
		t.Fatal(err)
	}
	env.srv.store.InvalidateNodeCache()
	after := wantCard(t, env.card(q), "miss")
	if bytes.Equal(before, after) {
		t.Fatal("the renamed node drew the same card")
	}
}

// rawMeta is the content attribute of <meta property|name="key"> exactly
// as the document carries it, entities and all.
func rawMeta(t *testing.T, body, key string) string {
	t.Helper()
	m := regexp.MustCompile(`<meta (?:property|name)="`+regexp.QuoteMeta(key)+`" content="([^"]*)">`).FindAllStringSubmatch(body, -1)
	if len(m) != 1 {
		t.Fatalf("%d %s tags", len(m), key)
	}
	return m[0][1]
}

// RM2: the og:image URL is attribute-escaped: its query's & are &amp; in
// the document, as og:url's are.
func TestShareProfile_CardImageIsAttributeEscaped(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{})
	body := env.share(shareQ(shareKeyA, shareKeyB, "ha=12")).Body.String()
	for _, k := range []string{"og:image", "twitter:image"} {
		raw := rawMeta(t, body, k)
		if !strings.Contains(raw, "profile.png?a=") || !strings.Contains(raw, "&amp;b=") || strings.Contains(strings.ReplaceAll(raw, "&amp;", ""), "&") {
			t.Fatalf("%s not attribute-escaped: %q", k, raw)
		}
	}
}

// RM9: with no usable origin (no publicBaseUrl and a Host that is not a
// host name) og:image is never a relative card URL: the page keeps the
// site image, as every other page does, and no card alt text.
func TestShareProfile_NoOriginKeepsTheSiteImage(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{})
	body := env.share(shareQ(shareKeyA, shareKeyB), func(r *http.Request) { r.Host = "bad host" }).Body.String()
	img := mustMeta(t, body, "og:image")
	if strings.Contains(img, "profile.png") {
		t.Fatalf("og:image %q is the card without an origin", img)
	}
	if _, ok := metaContent(body, "og:image:alt"); ok {
		t.Fatal("the card's alt text without the card")
	}
}

// RM4: a refund never raises the share budget above its burst.
func TestShareBudget_RefundIsCappedAtTheBurst(t *testing.T) {
	b := newShareBudget(2)
	now := time.Unix(1_700_000_000, 0)
	b.refund()
	b.refund()
	if got := b.available(now); got != 2 {
		t.Fatalf("%v tokens after refunds into a full budget, want 2", got)
	}
}

// Finding 7 (#727 plan): a path with no DHM verdict keeps the title free
// of a verdict word; the card's badge still says "No verdict".
func TestShareProfile_NoVerdictWordInTheTitleOutsideDHM(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{})
	a, b := "50.00000,5.00000", "50.05000,5.00000"
	var drawn profilecard.Verdict
	env.p.card.render = func(resp profilecard.TerrainProfileResponse, names [2]string, pal profilecard.Palette, opt profilecard.Options) ([]byte, error) {
		drawn = profilecard.MainVerdict(resp)
		return profilecard.Render(resp, names, pal, opt)
	}
	body := env.share(shareQ(a, b)).Body.String()
	for _, k := range []string{"og:title", "twitter:title"} {
		if got := mustMeta(t, body, k); got != "Path profile: 5561.5 m" {
			t.Fatalf("%s %q", k, got)
		}
	}
	if got := titleOf(t, body); got != "Path profile: 5561.5 m" {
		t.Fatalf("title %q", got)
	}
	wantCard(t, env.card(pathQuery(a, b)), "miss")
	if drawn.Known || drawn.Word != "No verdict" {
		t.Fatalf("badge %+v, want the unknown verdict", drawn)
	}
}

// Finding 9: the 503 text matches DoAdmitted. A refused call is never
// registered, so nothing can join it; requests for a profile already
// being fetched share that fetch and take no slot.
func TestTerrainProfile_OpenAPI503MatchesAdmission(t *testing.T) {
	m, ok := routeDescriptions()["GET /api/terrain/profile"]
	if !ok {
		t.Fatal("GET /api/terrain/profile undocumented")
	}
	var text string
	for _, e := range m.Errors {
		if e.Status == "503" {
			text = e.Description
		}
	}
	if text == "" {
		t.Fatal("no 503")
	}
	if strings.Contains(text, "refused with it") {
		t.Fatalf("503 text still says joiners of a refused fetch are refused with it: %q", text)
	}
	if !strings.Contains(text, "take no slot") {
		t.Fatalf("503 text does not say joiners take no slot: %q", text)
	}
}

// Merge with #738: a profile whose foreign fill failed is cached for its
// DHM part, and the JSON route tries the fill again on a hit. Its card is
// degraded (max-age 60, not cached), never an immutable complete card of a
// profile that can still change, and /p treats it as not ready: no verdict
// word and the pending card's v (#759 delta review finding 3). Once the
// fill succeeds the profile is ready and mixed, and the card the renderer
// is handed carries Coverage.Mixed, which draws the Copernicus credit
// (finding 2; internal/profilecard TestMixedCardDrawsBothCredits).
func TestProfileCard_FailedFillIsNotAComplete(t *testing.T) {
	var fillWorks atomic.Bool
	fill := func(ctx context.Context, p *dhm.Plan) (copdem.Result, error) {
		if fillWorks.Load() {
			return everywhere(5)(ctx, p)
		}
		return copdem.Result{}, errors.New("bucket down")
	}
	f := &scriptedFill{fn: fill}
	pc := dhm.ProfileConfig{ForeignFill: dhm.ForeignFillConfig{Enabled: true}, Share: dhm.ShareConfig{Enabled: true}}
	env := newTerrainEnvWith(t, newFakeElevation(t, flatElev(fillDHMHeight)), pc, nil, func(d *terrainProfilerDeps) { d.fill = f })
	var mixed atomic.Bool
	env.p.card.render = func(resp profilecard.TerrainProfileResponse, names [2]string, pal profilecard.Palette, opt profilecard.Options) ([]byte, error) {
		mixed.Store(!opt.Pending && resp.Coverage.Mixed)
		return profilecard.Render(resp, names, pal, opt)
	}
	q := pathQuery(cphPt, malmoPt)
	// /p as newShareEnvWith serves it, on this env's fill.
	doc := newIndexDoc(sharePublicDir, newOGMeta(brandPick(nil), sharePublicDir, nil))
	router := mux.NewRouter()
	env.srv.RegisterRoutes(router)
	env.srv.registerShareRoutes(router, doc)
	serve := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "http://example.test"+path, nil))
		return w
	}
	card := func() *httptest.ResponseRecorder { return serve(cardRoute + "?" + q) }
	share := func() (title, image string) {
		w := serve(shareQ(cphPt, malmoPt))
		if w.Code != http.StatusOK {
			t.Fatalf("/p: %d", w.Code)
		}
		body := w.Body.String()
		return mustMeta(t, body, "og:title"), mustMeta(t, body, "og:image")
	}
	// Nothing cached yet: /p is not ready.
	coldTitle, coldImage := share()
	wantCard(t, card(), "degraded")
	wantCard(t, card(), "degraded")
	if env.p.card.cache.Len() != 0 {
		t.Fatal("a card of a failed fill was cached")
	}
	if f.count() == 0 {
		t.Fatal("precondition: the fill was never tried")
	}
	// The DHM part is cached now, the fill still missing: /p must say
	// exactly what it said with nothing cached.
	if title, image := share(); title != coldTitle || image != coldImage {
		t.Fatalf("/p of a failed fill: title %q image %q, want the not-ready %q %q", title, image, coldTitle, coldImage)
	}
	if strings.Contains(coldTitle, " · ") {
		t.Fatalf("not-ready /p title carries a verdict word: %q", coldTitle)
	}
	// The JSON route retries the fill on a hit; now it works.
	fillWorks.Store(true)
	resp := decodeProfile(t, env.get(q))
	if !resp.Coverage.Mixed {
		t.Fatal("precondition: the retried fill did not make the profile mixed")
	}
	if _, image := share(); image == coldImage {
		t.Fatal("/p kept the not-ready card URL for a ready profile: the pin above proves nothing")
	}
	wantCard(t, card(), "miss")
	if !mixed.Load() {
		t.Fatal("the complete card of a mixed profile was not drawn as mixed")
	}
}
