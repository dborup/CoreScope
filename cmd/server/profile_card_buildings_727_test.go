package main

import (
	"testing"

	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/profilecard"
)

// #727 M4, carried from the final #759 review: the share card with the
// building classification (#730) on. A classified profile is cached with
// its class bytes after the elevations, so every place the card and /p
// decode a cached profile must ask decodeTerrainElevation for exactly
// p.classifies(both): a classified profile read as unclassified, or an
// unclassified one read as classified, fails to decode and the card would
// be served degraded for good. The three places are the card's cache hit,
// the card's own fetch and /p's cachedResponse; each case below reaches
// all three for one model. Fakes only: the elevations are the Dokk1 fake
// and the footprints come from stubOGC.

// newCardBuildingsEnv is newCardEnv with the classification on, on the
// Dokk1 fake elevations and ogc.
func newCardBuildingsEnv(t *testing.T, ogc *stubOGC) *shareEnv {
	t.Helper()
	pc := dhm.ProfileConfig{Share: dhm.ShareConfig{Enabled: true}, RateLimitPerMin: intp(0)}
	pc.Buildings.Enabled = true
	src := newFakeElevation(t, dokk1Ground)
	src.surface = dokk1Surface
	set := resolveTerrainSettings(pc, func(string, ...interface{}) {})
	env := newTerrainEnvWith(t, src, pc, nil, func(d *terrainProfilerDeps) { d.buildings = ogc.source(set) })
	if !env.p.classifies(true) {
		t.Fatal("precondition: the profiler does not classify")
	}
	return shareEnvAround(t, src, env, nil)
}

// drawnClassification records the classification each rendered card was
// drawn from (nil: unclassified).
func drawnClassification(env *shareEnv) *[]*profilecard.TerrainClassification {
	var seen []*profilecard.TerrainClassification
	env.p.card.render = func(resp profilecard.TerrainProfileResponse, names [2]string, pal profilecard.Palette, opt profilecard.Options) ([]byte, error) {
		seen = append(seen, resp.Classification)
		return profilecard.Render(resp, names, pal, opt)
	}
	return &seen
}

func wantDrawnClassification(t *testing.T, seen []*profilecard.TerrainClassification, model string) {
	t.Helper()
	if len(seen) == 0 {
		t.Fatal("no card was drawn")
	}
	for i, c := range seen {
		switch {
		case model == "dtm" && c != nil:
			t.Fatalf("card %d of a terrain-only profile drawn classified: %+v", i, c)
		case model == "both" && (c == nil || c.State != "full"):
			t.Fatalf("card %d drawn with classification %+v, want full", i, c)
		}
	}
}

// wantTitleVerdict: /p reads the cached profile (cachedResponse) and its
// title gains the verdict word the card shows.
func wantTitleVerdict(t *testing.T, env *shareEnv, target, before string) {
	t.Helper()
	word := profilecard.MainVerdict(env.cachedResponse(t, target)).Word
	if word == "" || word == "No verdict" {
		t.Fatalf("precondition: the Dokk1 fake gives verdict %q", word)
	}
	if got := titleOf(t, env.share(target).Body.String()); got != before+" · "+word {
		t.Fatalf("/p title %q, want %q", got, before+" · "+word)
	}
}

// The card fetches the profile itself (cold), then is drawn again from
// the cache with another antenna height (a card miss on an elevation hit).
func TestProfileCard_BuildingsOnCardFetchesThenHits(t *testing.T) {
	for _, model := range []string{"both", "dtm"} {
		t.Run(model, func(t *testing.T) {
			ogc := newStubOGC(t, dokk1Footprint)
			env := newCardBuildingsEnv(t, ogc)
			seen := drawnClassification(env)
			m := "model=" + model
			target := shareQ(dokk1A, dokk1B, m)
			before := titleOf(t, env.share(target).Body.String())

			wantCard(t, env.card(pathQuery(dokk1A, dokk1B, m)), "miss") // the card's fetch
			calls, corridors := env.src.calls.Load(), len(ogc.requests())
			if calls == 0 {
				t.Fatal("a cold card made no upstream call")
			}
			wantCard(t, env.card(pathQuery(dokk1A, dokk1B, m, "ha=12")), "miss") // the card's cache hit
			wantTitleVerdict(t, env, target, before)                             // /p's cachedResponse
			if env.src.calls.Load() != calls || len(ogc.requests()) != corridors {
				t.Fatal("a cached profile went upstream again")
			}
			wantDrawnClassification(t, *seen, model)
			want := 0
			if model == "both" {
				want = 1
			}
			if corridors != want {
				t.Fatalf("%d corridor requests for model %s, want %d", corridors, model, want)
			}
		})
	}
}

// The panel (GET /api/terrain/profile) caches the profile first, the way a
// link is usually shared: measured, then copied. The card and /p read
// what the panel cached, without an upstream call.
func TestProfileCard_BuildingsOnCardReadsThePanelsProfile(t *testing.T) {
	for _, model := range []string{"both", "dtm"} {
		t.Run(model, func(t *testing.T) {
			ogc := newStubOGC(t, dokk1Footprint)
			env := newCardBuildingsEnv(t, ogc)
			seen := drawnClassification(env)
			m := "model=" + model
			target := shareQ(dokk1A, dokk1B, m)
			before := titleOf(t, env.share(target).Body.String())

			resp := decodeProfile(t, env.get(pathQuery(dokk1A, dokk1B, m)))
			if model == "both" {
				wantClassification(t, resp, "full", false)
			} else if resp.Classification != nil {
				t.Fatalf("terrain-only profile classified: %+v", resp.Classification)
			}
			calls, corridors := env.src.calls.Load(), len(ogc.requests())
			wantCard(t, env.card(pathQuery(dokk1A, dokk1B, m)), "miss")
			wantTitleVerdict(t, env, target, before)
			if env.src.calls.Load() != calls || len(ogc.requests()) != corridors {
				t.Fatal("the card or /p went upstream for a profile the panel cached")
			}
			wantDrawnClassification(t, *seen, model)
		})
	}
}
