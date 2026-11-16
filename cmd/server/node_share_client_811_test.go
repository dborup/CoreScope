package main

// #811 M4: /api/config/client tells the frontend when Copy URL may hand
// out /p?node= links, for which roles, and how the card looks. The block
// is absent while share.nodes.enabled is off, so an older or switched-off
// server keeps the plain #/nodes/ link.

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/nodecard"
)

func clientShareRaw(t *testing.T, e *nodeShareEnv) (json.RawMessage, bool) {
	t.Helper()
	w := e.get("/api/config/client")
	if w.Code != 200 {
		t.Fatalf("/api/config/client status %d", w.Code)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &keys); err != nil {
		t.Fatal(err)
	}
	raw, ok := keys["share"]
	return raw, ok
}

func TestNodeShare_ClientConfigOff(t *testing.T) {
	e := newNodeShareEnv(t, false, nil)
	if raw, ok := clientShareRaw(t, e); ok {
		t.Fatalf("share block %s while share.nodes.enabled is off", raw)
	}
}

func TestNodeShare_ClientConfigOn(t *testing.T) {
	no := false
	for _, tc := range []struct {
		name string
		card dhm.CardConfig
		want DHMCardClientConfig
	}{
		{"defaults", dhm.CardConfig{}, DHMCardClientConfig{Theme: "dark", Names: true}},
		{"light without names", dhm.CardConfig{Theme: "light", Names: &no}, DHMCardClientConfig{Theme: "light", Names: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newNodeShareEnv(t, true, func(c *Config) { c.DHM.Profile.Card = tc.card })
			raw, ok := clientShareRaw(t, e)
			if !ok {
				t.Fatal("no share block while share.nodes.enabled is on")
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(fields))
			for k := range fields {
				got = append(got, k)
			}
			sort.Strings(got)
			if !reflect.DeepEqual(got, []string{"card", "nodes", "roles"}) {
				t.Fatalf("share keys %v, want [card nodes roles]", got)
			}
			var sc ShareClientConfig
			if err := json.Unmarshal(raw, &sc); err != nil {
				t.Fatal(err)
			}
			if !sc.Nodes {
				t.Error("share.nodes is false while node sharing is on")
			}
			if sc.Card == nil || *sc.Card != tc.want {
				t.Errorf("share.card %+v, want %+v", sc.Card, tc.want)
			}
			// The roles are the renderer's own rule, not a second copy.
			for _, r := range []string{"repeater", "room", "sensor", "companion", "", "observer"} {
				in := false
				for _, s := range sc.Roles {
					in = in || s == r
				}
				if in != nodecard.Carded(r) {
					t.Errorf("role %q in share.roles %t, carded %t", r, in, nodecard.Carded(r))
				}
			}
		})
	}
}
