package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/meshcore-analyzer/channelregistry/antennaclaim"
)

// #789 M3 — operator config for the read-side thresholds and the owner
// page's client settings, and the node's current antenna height.

// The config route carries the client settings, open or closed, so the
// owner page and the admin queue never hardcode them.
func TestAntennaClaims789M3_ConfigCarriesClientSettings(t *testing.T) {
	for name, tc := range map[string]struct {
		cc   *antennaclaim.Config
		want antennaclaim.ClientSettings
	}{
		"defaults, closed": {nil, antennaclaim.ClientSettings{PollInitialMs: 2000, PollBackoff: 1.5, PollMaxMs: 30000, PollMaxAttempts: 60, AdminDefaultFilter: "proven"}},
		"configured, open": {claimsOn(func(c *antennaclaim.Config) {
			c.PollInitialMs, c.PollBackoff, c.PollMaxMs, c.PollMaxAttempts, c.AdminDefaultFilter = 1000, 2, 8000, 5, "pending"
		}), antennaclaim.ClientSettings{PollInitialMs: 1000, PollBackoff: 2, PollMaxMs: 8000, PollMaxAttempts: 5, AdminDefaultFilter: "pending"}},
	} {
		t.Run(name, func(t *testing.T) {
			env := newClaim789Env(t, claimAdminKey, tc.cc, nil)
			w := env.do("GET", "/api/antenna-claims/config", "", "")
			if w.Code != http.StatusOK {
				t.Fatalf("config %d %s", w.Code, w.Body)
			}
			got := decode[AntennaClaimConfigResponse](t, w)
			if got.ClientSettings != tc.want {
				t.Fatalf("client settings %+v, want %+v (body %s)", got.ClientSettings, tc.want, w.Body)
			}
		})
	}
}

// reviewDeltaM moves the review flag the server puts on a claim.
func TestAntennaClaims789M3_ReviewDeltaFromConfig(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, claimsOn(func(c *antennaclaim.Config) { c.ReviewDeltaM = 10 }), nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	env.endA(t, pk) // caches the 20 m surface
	env.post(pk, `{"heightM":32}`)
	env.post(pk, `{"heightM":33}`)
	cmds := env.queued(t)
	if len(cmds) != 2 || cmds[0].NeedsReview || !cmds[1].NeedsReview {
		t.Fatalf("with reviewDeltaM 10 over a 20 m surface, 32 m must pass and 33 m be flagged: %+v", cmds)
	}
}

// staleMoveM moves both the served height and the admin list's stale flag.
func TestAntennaClaims789M3_StaleMoveFromConfig(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, &antennaclaim.Config{StaleMoveM: 100}, nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	h := 31.5
	env.insertClaim(t, "a000000000000001", pk, &h, antennaclaim.StatusApproved, est789Lat, est789Lon)
	env.move(t, pk, est789Lat+60/111320.0, est789Lon) // stale under the 50 m default
	if a := env.endA(t, pk); a.AntennaSource != "owner" || a.AntennaM != 31.5 {
		t.Fatalf("60 m moved with staleMoveM 100: %+v, want the owner height", a)
	}
	list := decode[AdminAntennaClaimListResponse](t, env.do("GET", "/api/admin/antenna-claims", "", claimAdminKey))
	if len(list.Claims) != 1 || list.Claims[0].Stale {
		t.Fatalf("admin list marks a 60 m move stale under staleMoveM 100: %+v", list.Claims)
	}
	env.move(t, pk, est789Lat+120/111320.0, est789Lon)
	env.svc.invalidateOwners()
	if a := env.endA(t, pk); a.AntennaSource == "owner" {
		t.Fatalf("120 m moved with staleMoveM 100 still served the owner height: %+v", a)
	}
}

// GET /api/nodes/{pubkey}/antenna is the height a profile would use for the
// node, from the caches alone.
func TestAntennaClaims789M3_NodeAntenna(t *testing.T) {
	pk, other, op := pk789("a"), pk789("b"), pk789("d")
	env := newClaim789Env(t, claimAdminKey, nil, &RadioConfig{AntennaHeightsM: map[string]float64{op: 24}})
	env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	env.addNode(t, other, "Hill", "repeater", est789Lat+0.01, est789Lon)
	env.addNode(t, op, "Mast", "repeater", est789Lat+0.02, est789Lon)
	get := func(pk string) NodeAntennaResponse {
		t.Helper()
		w := env.do("GET", "/api/nodes/"+pk+"/antenna", "", "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", pk[:4], w.Code, w.Body)
		}
		return decode[NodeAntennaResponse](t, w)
	}

	// Nothing cached: the role default, and the estimate is still to come.
	if got := get(other); got.AntennaM != 10 || got.AntennaSource != "role-default" || !got.Assumed || !got.EstimatePending {
		t.Fatalf("uncached: %+v", got)
	}
	if n := env.surface.calls.Load(); n != 0 {
		t.Fatalf("the node page measured the surface (%d rasters)", n)
	}
	env.endA(t, other) // a profile caches the 20 m surface
	if got := get(other); got.AntennaM != 22 || got.AntennaSource != "dsm-estimate" || !got.Assumed || got.EstimatePending {
		t.Fatalf("cached estimate: %+v", got)
	}
	h := 18.5
	env.insertClaim(t, "a000000000000001", pk, &h, antennaclaim.StatusApproved, est789Lat, est789Lon)
	if got := get(pk); got.AntennaM != 18.5 || got.AntennaSource != "owner" || got.Assumed || got.EstimatePending {
		t.Fatalf("owner: %+v", got)
	}
	if got := get(op); got.AntennaM != 24 || got.AntennaSource != "operator-node" || got.Assumed {
		t.Fatalf("operator: %+v", got)
	}
}

func TestAntennaClaims789M3_NodeAntennaHiddenLikeUnknown(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, nil, nil)
	env.srv.cfg.SetHiddenNamePrefixes([]string{"private-"})
	hidden, nogps, unknown := pk789("a"), pk789("b"), pk789("c")
	env.addNode(t, hidden, "private-roof", "repeater", est789Lat, est789Lon)
	env.addNode(t, nogps, "No position", "repeater", 0, 0)
	h := 30.0
	env.insertClaim(t, "a000000000000001", hidden, &h, antennaclaim.StatusApproved, est789Lat, est789Lon)
	ref := env.do("GET", "/api/nodes/"+unknown+"/antenna", "", "")
	if ref.Code != http.StatusNotFound {
		t.Fatalf("unknown: %d", ref.Code)
	}
	for name, pk := range map[string]string{"hidden": hidden, "no position": nogps} {
		w := env.do("GET", "/api/nodes/"+pk+"/antenna", "", "")
		if w.Code != ref.Code || w.Body.String() != ref.Body.String() {
			t.Fatalf("%s: %d %q differs from unknown %d %q", name, w.Code, w.Body, ref.Code, ref.Body)
		}
	}
	if w := env.do("GET", "/api/nodes/not-a-key/antenna", "", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("bad key: %d", w.Code)
	}
}

func TestAntennaClaims789M3_NodeAntennaTerrainOff(t *testing.T) {
	env := newClaim789Env(t, claimAdminKey, nil, nil)
	pk := pk789("a")
	env.addNode(t, pk, "Roof", "repeater", est789Lat, est789Lon)
	env.srv.terrain = nil
	if w := env.do("GET", "/api/nodes/"+pk+"/antenna", "", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("terrain off: %d %s", w.Code, w.Body)
	}
}

// Review PR#893 F6: the thresholds are operator settings since M3, so the
// API description names them instead of their default values.
func TestAntennaClaims789M3_OpenAPINamesThresholdSettings(t *testing.T) {
	d := routeDescriptions()["GET /api/admin/antenna-claims"].Description
	for _, want := range []string{"antennaClaims.reviewDeltaM", "antennaClaims.staleMoveM"} {
		if !strings.Contains(d, want) {
			t.Errorf("description does not name %s: %s", want, d)
		}
	}
	for _, stale := range []string{"60 m", "50 m"} {
		if strings.Contains(d, stale) {
			t.Errorf("description still hardcodes %q: %s", stale, d)
		}
	}
}
