package main

// #812 M4: /api/config/client tells the frontend when Copy link may hand
// out /p?packet= links (the packet page and View Path). share.packets is
// true exactly while share.packets.enabled is on; with node and packet
// sharing both off the block stays absent, as #811 M4 left it, and with
// node sharing alone it is byte for byte #811 M4's block.

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/meshcore-analyzer/dhm"
)

func packetClientShare(t *testing.T, e *packetShareEnv) (json.RawMessage, bool) {
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

func clientShareKeys(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(fields))
	for k := range fields {
		got = append(got, k)
	}
	sort.Strings(got)
	return got
}

func TestPacketShare_ClientConfigOff(t *testing.T) {
	e := newPacketShareEnv(t, false, nil)
	if raw, ok := packetClientShare(t, e); ok {
		t.Fatalf("share block %s while node and packet sharing are off", raw)
	}
}

func TestPacketShare_ClientConfigPacketsOnly(t *testing.T) {
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
			e := newPacketShareEnv(t, true, func(c *Config) { c.DHM.Profile.Card = tc.card })
			raw, ok := packetClientShare(t, e)
			if !ok {
				t.Fatal("no share block while share.packets.enabled is on")
			}
			// No roles: they are the node card's, and node sharing is off.
			if got := clientShareKeys(t, raw); !reflect.DeepEqual(got, []string{"card", "nodes", "packets"}) {
				t.Fatalf("share keys %v, want [card nodes packets]", got)
			}
			var sc ShareClientConfig
			if err := json.Unmarshal(raw, &sc); err != nil {
				t.Fatal(err)
			}
			if !sc.Packets || sc.Nodes {
				t.Errorf("packets %t nodes %t, want packets only", sc.Packets, sc.Nodes)
			}
			if sc.Card == nil || *sc.Card != tc.want {
				t.Errorf("share.card %+v, want %+v", sc.Card, tc.want)
			}
		})
	}
}

func TestPacketShare_ClientConfigBoth(t *testing.T) {
	e := newPacketShareEnv(t, true, func(c *Config) { c.Share.Nodes.Enabled = true })
	raw, ok := packetClientShare(t, e)
	if !ok {
		t.Fatal("no share block")
	}
	if got := clientShareKeys(t, raw); !reflect.DeepEqual(got, []string{"card", "nodes", "packets", "roles"}) {
		t.Fatalf("share keys %v, want [card nodes packets roles]", got)
	}
	var sc ShareClientConfig
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatal(err)
	}
	if !sc.Packets || !sc.Nodes || len(sc.Roles) == 0 {
		t.Errorf("%+v, want nodes, packets and roles", sc)
	}
}

// Node sharing alone keeps #811 M4's block: no packets key at all.
func TestPacketShare_ClientConfigNodesOnly(t *testing.T) {
	e := newPacketShareEnv(t, false, func(c *Config) { c.Share.Nodes.Enabled = true })
	raw, ok := packetClientShare(t, e)
	if !ok {
		t.Fatal("no share block while share.nodes.enabled is on")
	}
	if got := clientShareKeys(t, raw); !reflect.DeepEqual(got, []string{"card", "nodes", "roles"}) {
		t.Fatalf("share keys %v, want #811 M4's [card nodes roles]", got)
	}
}
