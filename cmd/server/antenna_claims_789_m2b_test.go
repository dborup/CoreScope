package main

import (
	"net/http"
	"testing"

	"github.com/meshcore-analyzer/channelregistry/antennaclaim"
)

// #789 M2b — the node page offers the owner flow only while public claims
// are open. GET /api/antenna-claims/config says so with the same gate as the
// submit route, so the page and the POST can never disagree.
func TestAntennaClaims789_ConfigMatchesTheSubmitGate(t *testing.T) {
	pk := pk789("c")
	for name, tc := range map[string]struct {
		key  string
		cc   *antennaclaim.Config
		want bool
	}{
		"no config":           {claimAdminKey, nil, false},
		"enabled, no key":     {"", claimsOn(nil), false},
		"enabled, weak key":   {"changeme", claimsOn(nil), false},
		"enabled, short key":  {"short-key", claimsOn(nil), false},
		"strong key, not set": {claimAdminKey, &antennaclaim.Config{}, false},
		"open":                {claimAdminKey, claimsOn(nil), true},
	} {
		t.Run(name, func(t *testing.T) {
			env := newClaim789Env(t, tc.key, tc.cc, nil)
			env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
			w := env.do("GET", "/api/antenna-claims/config", "", "")
			if w.Code != http.StatusOK {
				t.Fatalf("config status %d %s", w.Code, w.Body)
			}
			if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
				t.Fatalf("Cache-Control %q, want no-store", cc)
			}
			if got := decode[AntennaClaimConfigResponse](t, w); got.Enabled != tc.want {
				t.Fatalf("enabled = %v, want %v", got.Enabled, tc.want)
			}
			sub := env.post(pk, `{"heightM":18}`)
			if tc.want != (sub.Code == http.StatusAccepted) {
				t.Fatalf("config says %v but submit answered %d", tc.want, sub.Code)
			}
		})
	}
}
