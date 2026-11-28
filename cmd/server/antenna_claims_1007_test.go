package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/meshcore-analyzer/channelregistry/antennaclaim"
)

// #1007: GET /api/antenna-claims/config and the admin list report
// antennaClaims.autoApproveDetails as a named field next to the others. It
// is the ingestor's policy: on only with detailsEnabled too.
func TestAntennaClaims1007_ReportsAutoApproveDetails(t *testing.T) {
	for name, tc := range map[string]struct {
		cc                *antennaclaim.Config
		wantAuto, wantDet bool
	}{
		"no config":              {nil, false, false},
		"details on, switch off": {detailsOn(nil), false, false},
		"switch without details": {claimsOn(func(c *antennaclaim.Config) { c.AutoApproveDetails = true }), false, false},
		"details on, switch on":  {detailsOn(func(c *antennaclaim.Config) { c.AutoApproveDetails = true }), false, true},
		"proven on, switch off":  {detailsOn(func(c *antennaclaim.Config) { c.AutoApproveProven = true }), true, false},
		"both on":                {detailsOn(func(c *antennaclaim.Config) { c.AutoApproveProven, c.AutoApproveDetails = true, true }), true, true},
	} {
		t.Run(name, func(t *testing.T) {
			env := newClaim789Env(t, claimAdminKey, tc.cc, nil)
			w := env.do("GET", "/api/antenna-claims/config", "", "")
			if w.Code != http.StatusOK {
				t.Fatalf("config %d %s", w.Code, w.Body)
			}
			want := `"autoApproveDetails":` + map[bool]string{false: "false", true: "true"}[tc.wantDet]
			if !strings.Contains(w.Body.String(), want) {
				t.Fatalf("config %s, want %s", w.Body, want)
			}
			if cfg := decode[AntennaClaimConfigResponse](t, w); cfg.AutoApproveDetails != tc.wantDet {
				t.Fatalf("config %+v", cfg)
			}
			lw := env.do("GET", "/api/admin/antenna-claims", "", claimAdminKey)
			if lw.Code != http.StatusOK || !strings.Contains(lw.Body.String(), want) {
				t.Fatalf("admin list %d %s, want %s", lw.Code, lw.Body, want)
			}
			list := decode[AdminAntennaClaimListResponse](t, lw)
			if list.AutoApprove != tc.wantAuto || list.AutoApproveDetails != tc.wantDet {
				t.Fatalf("admin list autoApprove=%v autoApproveDetails=%v, want %v %v", list.AutoApprove, list.AutoApproveDetails, tc.wantAuto, tc.wantDet)
			}
		})
	}
}

// The server reads the switch by its exact spelling only, like the ingestor.
func TestAntennaClaimAutoApproveDetailsGate1007(t *testing.T) {
	for _, tc := range []struct {
		name, cfg string
		want      bool
		warnings  int
	}{
		{"canonical on", `{"antennaClaims":{"enabled":true,"detailsEnabled":true,"autoApproveDetails":true}}`, true, 0},
		{"needs detailsEnabled", `{"antennaClaims":{"enabled":true,"autoApproveDetails":true}}`, false, 0},
		{"AutoApproveDetails alone", `{"antennaClaims":{"enabled":true,"detailsEnabled":true,"AutoApproveDetails":true}}`, false, 1},
		{"autoapprovedetails after canonical false", `{"antennaClaims":{"enabled":true,"detailsEnabled":true,"autoApproveDetails":false,"autoapprovedetails":true}}`, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, logged := loadConfigLogged(t, tc.cfg)
			if got := cfg.AntennaClaims.AutoApproveDetailsOn(); got != tc.want {
				t.Fatalf("AutoApproveDetailsOn = %v, want %v", got, tc.want)
			}
			if n := ignoredWarnings(logged); n != tc.warnings {
				t.Fatalf("%d warnings, want %d:\n%s", n, tc.warnings, logged)
			}
		})
	}
}
