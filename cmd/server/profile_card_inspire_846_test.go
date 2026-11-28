package main

import (
	"bytes"
	"context"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/meshcore-analyzer/buildings"
	"github.com/meshcore-analyzer/dhm"
)

// #846: the share card credits the INSPIRE building footprints and colours
// its surface band by class (internal/profilecard). Here: the card's
// identity — its cache key and the v of its immutable URL — follows the
// classification and the credit, and og:image:alt names the footprints.
// Fakes only: the Dokk1 fake elevations and stubOGC footprints.

// resolveShare is the resolved request of a /p target.
func (e *shareEnv) resolveShare(t *testing.T, target string) terrainRequest {
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
	return req
}

// reclassify rewrites the cached profile of target with edit applied to
// its classification, as a building retry that ends differently would
// (#797): the same elevations, the same elevation key.
func (e *shareEnv) reclassify(t *testing.T, target string, edit func(*terrainClasses)) {
	t.Helper()
	req := e.resolveShare(t, target)
	key := e.p.elevationKey(req)
	data, ok := e.p.cache.Get(key, e.p.now())
	if !ok {
		t.Fatal("profile not cached")
	}
	el, err := decodeTerrainElevation(data, len(req.plan.E), true, true)
	if err != nil || el.classes == nil {
		t.Fatalf("cached profile not classified: %v", err)
	}
	edit(el.classes)
	e.p.cache.Put(key, el.encode(), e.p.now())
}

func TestProfileCard846_URLAndCardFollowTheClassification(t *testing.T) {
	env := newCardBuildingsEnv(t, newStubOGC(t, dokk1Footprint))
	q, target := pathQuery(dokk1A, dokk1B, "model=both"), shareQ(dokk1A, dokk1B, "model=both")
	url1, png1 := cardImageAfter(t, env, target, q)
	if env.cachedResponse(t, target).Attribution != buildings.AttributionDerived {
		t.Fatal("precondition: the Dokk1 profile does not credit the footprints")
	}
	// The card key of a classified profile, read off the cached entry,
	// is the key the card was stored under.
	wantCard(t, env.card(q), "hit")

	// Same elevations, other classes: the building is now trees/other on
	// a partial classification. The card is drawn anew and v moves.
	env.reclassify(t, target, func(c *terrainClasses) {
		c.state = terrainClassPartial
		for i, cl := range c.cls {
			if cl == buildings.Building {
				c.cls[i] = buildings.Vegetation
			}
		}
	})
	png2 := wantCard(t, env.card(q), "miss")
	if bytes.Equal(png1, png2) {
		t.Fatal("precondition: the new classes did not change the card")
	}
	url2 := mustMeta(t, env.share(target).Body.String(), "og:image")
	if url2 == url1 {
		t.Fatal("the classification changed the card, but not its immutable og:image URL")
	}
	wantCard(t, env.card(q), "hit")
	if url3 := mustMeta(t, env.share(target).Body.String(), "og:image"); url3 != url2 {
		t.Fatalf("v moved without a change: %q then %q", url2, url3)
	}

	// The state alone, the same classes: partial back to full. The card
	// changes ("Not told apart" leaves the legend), so v moves too.
	env.reclassify(t, target, func(c *terrainClasses) { c.state = terrainClassFull })
	pngFull := wantCard(t, env.card(q), "miss")
	if bytes.Equal(png2, pngFull) {
		t.Fatal("precondition: the state did not change the card")
	}
	urlFull := mustMeta(t, env.share(target).Body.String(), "og:image")
	if urlFull == url2 || urlFull == url1 {
		t.Fatal("the classification state changed the card, but not its immutable og:image URL")
	}
	url2 = urlFull

	// No class left: the footprints are no longer credited, so the credit
	// changes, and with it the card and v.
	env.reclassify(t, target, func(c *terrainClasses) {
		c.state = terrainClassFull
		for i := range c.cls {
			c.cls[i] = buildings.Unclassified
		}
	})
	if env.cachedResponse(t, target).Attribution != dhm.AttributionDerived {
		t.Fatal("precondition: an unused classification still credits the footprints")
	}
	png3 := wantCard(t, env.card(q), "miss")
	url4 := mustMeta(t, env.share(target).Body.String(), "og:image")
	if bytes.Equal(png2, png3) || url4 == url2 || url4 == url1 {
		t.Fatal("dropping the footprints' credit did not change the card and its URL")
	}
	for _, s := range []string{dokk1A, dokk1B, url.QueryEscape(dokk1A)} {
		if strings.Contains(url4[strings.Index(url4, "&v="):], s) {
			t.Fatalf("v carries a position: %q", url4)
		}
	}
}

// A profile the server does not classify keeps its card key without the
// profile, as before #846: a cached card is served after its elevations
// have left the cache. A classified one needs them for its key, so it is
// drawn again from a fresh fetch, and that card is then a hit.
func TestProfileCard846_KeyNeedsTheProfileOnlyWhenClassified(t *testing.T) {
	evict := func(env *shareEnv, target string) {
		req := env.resolveShare(t, target)
		env.p.cache.PutFor(env.p.elevationKey(req), []byte{0}, env.p.now(), 0)
	}
	plain := newCardEnv(t, dhm.ProfileConfig{})
	q, target := pathQuery(shareKeyA, shareKeyB), shareQ(shareKeyA, shareKeyB)
	wantCard(t, plain.card(q), "miss")
	evict(plain, target)
	calls := plain.src.calls.Load()
	wantCard(t, plain.card(q), "hit")
	if plain.src.calls.Load() != calls {
		t.Fatal("an unclassified card hit went upstream")
	}

	env := newCardBuildingsEnv(t, newStubOGC(t, dokk1Footprint))
	q, target = pathQuery(dokk1A, dokk1B, "model=both"), shareQ(dokk1A, dokk1B, "model=both")
	wantCard(t, env.card(q), "miss")
	evict(env, target)
	wantCard(t, env.card(q), "miss")
	wantCard(t, env.card(q), "hit")
}

// A model=both path wholly outside DHM on a classifying server is never
// in the elevation cache: it needs no fetch and has no classes. Its card is
// still a cache hit the second time, served before the per-IP charge
// (#850 review F5).
func TestProfileCard846_PathOutsideDHMIsACardHit(t *testing.T) {
	env := newCardBuildingsEnv(t, newStubOGC(t, dokk1Footprint))
	const lundPt = "55.70470,13.19100"
	q, target := pathQuery(malmoPt, lundPt, "model=both"), shareQ(malmoPt, lundPt, "model=both")
	if req := env.resolveShare(t, target); req.plan.Calls() != 0 {
		t.Fatalf("precondition: Malmö → Lund makes %d upstream calls", req.plan.Calls())
	}
	wantCard(t, env.card(q), "miss")
	calls := env.src.calls.Load()
	for i := 0; i < 2; i++ {
		wantCard(t, env.card(q), "hit")
	}
	if env.src.calls.Load() != calls {
		t.Fatal("a card hit went upstream")
	}
}

// og:image:alt names the footprints when the card credits them, and keeps
// whole sentences within its 300 runes (#850 review F1): the verdict and
// the surface sentence stay next to the credit, with coordinate endpoints
// and with names of the longest drawable length.
func TestShareProfile846_AltNamesTheFootprints(t *testing.T) {
	env := newCardBuildingsEnv(t, newStubOGC(t, dokk1Footprint))
	const pkA = "a846a846a846a846a846a846a846a846a846a846a846a846a846a846a846a846"
	const pkB = "b846b846b846b846b846b846b846b846b846b846b846b846b846b846b846b846"
	long := strings.Repeat("Havnefront-Repeater-", 3)[:48]
	for i, pk := range []string{pkA, pkB} {
		pt := []string{dokk1A, dokk1B}[i]
		ll := strings.Split(pt, ",")
		env.addNode(t, pk, long, "repeater", ll[0], ll[1])
	}
	credit := "Buildings: " + buildings.DatasetName + " (" + buildings.Source + "), " + dhm.LicenseName + "."
	for _, tc := range []struct{ a, b string }{{dokk1A, dokk1B}, {pkA, pkB}} {
		q, target := pathQuery(tc.a, tc.b, "model=both"), shareQ(tc.a, tc.b, "model=both")
		wantCard(t, env.card(q), "miss")
		alt := mustMeta(t, env.share(target).Body.String(), "og:image:alt")
		if !strings.HasSuffix(alt, " Buildings/trees: Blocked. "+credit) {
			t.Errorf("og:image:alt %q\ndoes not end with the surface sentence and the credit %q", alt, credit)
		}
		if !strings.Contains(alt, ". Terrain: ") || strings.Contains(alt, "…") {
			t.Errorf("og:image:alt %q lost the verdict or was cut", alt)
		}
		if n := utf8.RuneCountInString(alt); n > ogImageAltMaxRunes {
			t.Errorf("og:image:alt is %d runes, over %d", n, ogImageAltMaxRunes)
		}
		if tw := mustMeta(t, env.share(target).Body.String(), "twitter:image:alt"); tw != alt {
			t.Errorf("twitter:image:alt %q differs", tw)
		}
	}
	// The long names leave no room for the tightest point.
	alt := mustMeta(t, env.share(shareQ(pkA, pkB, "model=both")).Body.String(), "og:image:alt")
	if !strings.Contains(alt, long) || strings.Contains(alt, "Tightest point") {
		t.Errorf("precondition: og:image:alt %q kept every sentence", alt)
	}

	// Without the classification, nothing names the footprints.
	plain := newCardEnv(t, dhm.ProfileConfig{})
	q, target := pathQuery(shareKeyA, shareKeyB), shareQ(shareKeyA, shareKeyB)
	wantCard(t, plain.card(q), "miss")
	if alt := mustMeta(t, plain.share(target).Body.String(), "og:image:alt"); strings.Contains(alt, buildings.DatasetName) || strings.Contains(alt, buildings.Source) {
		t.Errorf("unclassified og:image:alt %q names the footprints", alt)
	}
}
