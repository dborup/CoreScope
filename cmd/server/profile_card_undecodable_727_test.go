package main

import (
	"context"
	"net/url"
	"testing"

	"github.com/meshcore-analyzer/dhm"
)

// #727 M4: a cached profile the card cannot decode (an entry of another
// shape, such as a classified profile read as unclassified) is drawn as
// the degraded card. It used to reach buildResponse as a zero
// terrainElevation and panic the handler on its empty elevation slices.
func TestProfileCard_UndecodableCachedProfileIsDegraded(t *testing.T) {
	env := newCardEnv(t, dhm.ProfileConfig{})
	q := cardQuery(3000)
	vals, _ := url.ParseQuery(q)
	tq, err := parseCardQuery(vals)
	if err != nil {
		t.Fatal(err)
	}
	req, rerr := env.srv.resolveTerrainRequest(context.Background(), env.p, tq)
	if rerr != nil {
		t.Fatal(rerr.msg)
	}
	if req.plan.Calls() == 0 {
		t.Fatal("precondition: the path is outside DHM, so nothing is read from the cache")
	}
	env.p.cache.Put(env.p.elevationKey(req), []byte{1, 2, 3, 4, 5}, env.p.now())

	wantCard(t, env.card(q), "degraded")
	if n := env.src.calls.Load(); n != 0 {
		t.Fatalf("%d upstream calls: a cache hit, even an undecodable one, is not refetched by the card", n)
	}
}
