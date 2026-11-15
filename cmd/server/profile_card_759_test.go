package main

import (
	"bytes"
	"context"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

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
// calls. This is the reviewer's probe: a 50 ms wait against a 300 ms
// upstream.
func TestProfileCard_SlowFetchOutlivesTheCardWait(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{Share: dhm.ShareConfig{UpstreamPerMin: intp(5)}})
	clock := time.Unix(1_700_000_000, 0)
	env.p.now = func() time.Time { return clock }
	env.p.card.wait = 50 * time.Millisecond
	env.src.hook = func(ctx context.Context, _ dhm.RasterRequest) error {
		select {
		case <-time.After(300 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	wantCard(t, env.card(cardQuery(3000)), "degraded")
	// A second card while the fetch runs joins it, gives its token back
	// and is degraded too: still one fetch.
	wantCard(t, env.card(cardQuery(3000)), "degraded")
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
		t.Fatalf("%.2f share tokens left, want 4: one fetch bought one profile", got)
	}
}

// Finding 3, the bound: a held fetch is still cancelled at the profile
// deadline, so an upstream that never answers cannot keep a slot.
func TestProfileCard_HeldFetchEndsAtTheDeadline(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{})
	env.p.card.wait = 20 * time.Millisecond
	env.p.deadline = 200 * time.Millisecond
	env.src.hook = func(ctx context.Context, _ dhm.RasterRequest) error { <-ctx.Done(); return ctx.Err() }
	wantCard(t, env.card(cardQuery(3000)), "degraded")
	if env.p.admit.InUse() != 1 {
		t.Fatalf("%d slots in use right after the card: the fetch was not held", env.p.admit.InUse())
	}
	waitUntil(t, "the deadline to end the held fetch", func() bool { return env.p.admit.InUse() == 0 })
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
	m := regexp.MustCompile(`<meta (?:property|name)="` + regexp.QuoteMeta(key) + `" content="([^"]*)">`).FindAllStringSubmatch(body, -1)
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
