package main

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/meshcore-analyzer/dhm"
)

// #727 M4 — the client config's dhm.card: the share card's look as the
// operator set it (map.dhm.profile.card.theme and .names), so the
// customizer can say what a shared link's preview shows. Present only
// while /p serves previews; it carries the two display choices and
// nothing else of the card block (no cache size).
func TestShareProfile_ClientCard(t *testing.T) {
	up := wcsUpstream(t, tiffAnswer)
	no := false
	for _, tc := range []struct {
		name  string
		share bool
		card  dhm.CardConfig
		want  *DHMCardClientConfig
	}{
		{"defaults", true, dhm.CardConfig{}, &DHMCardClientConfig{Theme: "dark", Names: true}},
		{"light without names", true, dhm.CardConfig{Theme: "light", Names: &no}, &DHMCardClientConfig{Theme: "light", Names: false}},
		{"an unknown theme is the dark default", true, dhm.CardConfig{Theme: "sepia"}, &DHMCardClientConfig{Theme: "dark", Names: true}},
		{"sharing off", false, dhm.CardConfig{Theme: "light"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, router := setupTestServer(t)
			srv.cfg.DHM = DHMConfig{Enabled: true, Token: dhmTestToken}
			srv.dhm = newTestDHMProxy(t, up, &dhmLogSink{}, nil)
			srv.terrain = newTerrainProfiler(dhm.ProfileConfig{Enabled: true, Share: dhm.ShareConfig{Enabled: tc.share}, Card: tc.card},
				nil, terrainProfilerDeps{src: newFakeElevation(t, flatElev(10)), logf: (&dhmLogSink{}).logf})
			raw := clientDHMRaw(t, router)
			var keys map[string]json.RawMessage
			if err := json.Unmarshal(raw, &keys); err != nil {
				t.Fatal(err)
			}
			cardRaw, present := keys["card"]
			if tc.want == nil {
				if present {
					t.Fatalf("dhm.card %s while sharing is off", cardRaw)
				}
				return
			}
			if !present {
				t.Fatal("no dhm.card while sharing is on")
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(cardRaw, &fields); err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(fields))
			for k := range fields {
				got = append(got, k)
			}
			sort.Strings(got)
			if !reflect.DeepEqual(got, []string{"names", "theme"}) {
				t.Fatalf("dhm.card keys %v, want [names theme]", got)
			}
			var card DHMCardClientConfig
			if err := json.Unmarshal(cardRaw, &card); err != nil {
				t.Fatal(err)
			}
			if card != *tc.want {
				t.Fatalf("dhm.card %+v, want %+v", card, *tc.want)
			}
		})
	}
}
