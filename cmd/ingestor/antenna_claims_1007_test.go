package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/channelregistry/antennaclaim"
	"github.com/meshcore-analyzer/configkey"
)

// #1007 — antennaClaims.autoApproveDetails: an opt-in switch that lets a
// proven claim that sets station details through without an administrator.
// Adverts are signed with the TEST key of antenna_claims_test.go.

// kind1007 is one shape of claim: the parts it carries.
type kind1007 struct {
	name        string
	h, d        string // "set", "clear" or "" (not carried)
	setsDetails bool
	clearOnly   bool
}

var kinds1007 = []kind1007{
	{name: "height", h: "set"},
	{name: "details", d: "set", setsDetails: true},
	{name: "height+details", h: "set", d: "set", setsDetails: true},
	{name: "clear-height+details", h: "clear", d: "set", setsDetails: true},
	{name: "height+clear-details", h: "set", d: "clear"},
	{name: "clear-only", h: "clear", d: "clear", clearOnly: true},
}

func (k kind1007) apply(cmd *antennaclaim.Command) {
	switch k.h {
	case "set":
		setHeight(12)(cmd)
	case "clear":
		cmd.Clear = true
	}
	switch k.d {
	case "set":
		setDetails(cmd)
	case "clear":
		cmd.ClearDetails = true
	}
}

// want1007 is the decision #1007 specifies, written out rule by rule: a
// clear-only claim is the owner's at once; a flagged claim always waits; a
// claim that sets details is auto-approved only by autoApproveDetails with
// detailsEnabled, whatever autoApproveProven says; any other claim only by
// autoApproveProven.
func want1007(proven, details, detailsEnabled bool, k kind1007, needsReview bool) (status, approvedBy string) {
	switch {
	case k.clearOnly:
		return antennaclaim.StatusApproved, antennaclaim.ApprovedByOwner
	case needsReview:
		return antennaclaim.StatusProven, ""
	case k.setsDetails && details && detailsEnabled:
		return antennaclaim.StatusApproved, antennaclaim.ApprovedByAuto
	case !k.setsDetails && proven:
		return antennaclaim.StatusApproved, antennaclaim.ApprovedByAuto
	}
	return antennaclaim.StatusProven, ""
}

// The full decision table: every combination of autoApproveProven,
// autoApproveDetails, detailsEnabled, the claim's parts and needs_review,
// with the status and approved_by after the proving advert. An approved
// claim has all its parts in force, contacts included.
func TestAntennaClaim1007_DecisionTable(t *testing.T) {
	bools := []bool{false, true}
	n := 0
	for _, proven := range bools {
		for _, details := range bools {
			for _, detailsEnabled := range bools {
				for _, k := range kinds1007 {
					for _, review := range bools {
						name := fmt.Sprintf("proven=%v/details=%v/detailsEnabled=%v/%s/review=%v", proven, details, detailsEnabled, k.name, review)
						cfg := &antennaclaim.Config{AutoApproveProven: proven, AutoApproveDetails: details, DetailsEnabled: detailsEnabled}
						wantStatus, wantBy := want1007(proven, details, detailsEnabled, k, review)
						n++
						t.Run(name, func(t *testing.T) {
							r := newClaimRig(t, cfg)
							if res := r.claim898(claimA, claimCode, func(c *antennaclaim.Command) { k.apply(c); c.NeedsReview = review }); res.Status != antennaclaim.StatusPending {
								t.Fatalf("claim: %+v", res)
							}
							r.prove898(claimCode, true)
							p := r.parts898(claimA)
							if p.status != wantStatus || p.approvedBy != wantBy {
								t.Fatalf("status %q approved_by %q, want %q %q", p.status, p.approvedBy, wantStatus, wantBy)
							}
							if wantStatus == antennaclaim.StatusApproved {
								if p.hIn != (k.h != "") || p.dIn != (k.d != "") {
									t.Fatalf("in force h=%v d=%v, want the carried parts", p.hIn, p.dIn)
								}
								if k.d == "set" && (!p.adminC || !p.publicC) {
									t.Fatalf("auto-approved details lost a contact: %+v", p)
								}
							} else if p.hIn || p.dIn {
								t.Fatalf("a waiting claim has a part in force: %+v", p)
							}
						})
					}
				}
			}
		}
	}
	if n != 2*2*2*len(kinds1007)*2 {
		t.Fatalf("%d cases", n)
	}
}

// An auto-approved details claim replaces the node's previous details and
// can be revoked like any other approval.
func TestAntennaClaim1007_AutoApprovedDetailsReplaceAndRevoke(t *testing.T) {
	r := newClaimRig(t, &antennaclaim.Config{AutoApproveDetails: true, DetailsEnabled: true})
	r.claim898(claimA, claimCode, setDetails)
	r.prove898(claimCode, true)
	r.claim898(claimB, "BBBBBB", setDetails)
	r.prove898("BBBBBB", true)
	if a, b := r.parts898(claimA), r.parts898(claimB); a.status != antennaclaim.StatusSuperseded || a.adminC || a.publicC ||
		b.status != antennaclaim.StatusApproved || b.approvedBy != antennaclaim.ApprovedByAuto || !b.dIn {
		t.Fatalf("after the second auto-approval: A %+v, B %+v", a, b)
	}
	if res := r.decide898(antennaclaim.OpRevoke, claimB); res.Status != antennaclaim.StatusRevoked {
		t.Fatalf("revoke: %+v", res)
	}
	if b := r.parts898(claimB); b.status != antennaclaim.StatusRevoked || b.dIn || b.adminC || b.publicC {
		t.Fatalf("revoked auto-approval: %+v", b)
	}
}

// Turning the switch on approves nothing that was already proven: a
// restart with autoApproveDetails leaves the claim proven, a re-sent advert
// with its code does not prove it again, and the admin still decides.
func TestAntennaClaim1007_NoRetroactiveApproval(t *testing.T) {
	r := newClaimRig(t, &antennaclaim.Config{DetailsEnabled: true})
	r.claim898(claimA, claimCode, func(c *antennaclaim.Command) { setHeight(12)(c); setDetails(c) })
	r.prove898(claimCode, true)
	if p := r.parts898(claimA); p.status != antennaclaim.StatusProven {
		t.Fatalf("with the switch off: %+v", p)
	}
	r.runner = newAntennaClaimRunner(r.store, &antennaclaim.Config{AutoApproveProven: true, AutoApproveDetails: true, DetailsEnabled: true})
	r.runner.now = func() time.Time { return r.clock }
	r.store.antennaClaims = r.runner
	r.runner.RunOnce(context.Background())
	r.prove898(claimCode, true)
	r.runner.RunOnce(context.Background())
	if p := r.parts898(claimA); p.status != antennaclaim.StatusProven || p.approvedBy != "" || p.hIn || p.dIn {
		t.Fatalf("switch on after the proof approved the claim: %+v", p)
	}
	if res := r.decide898(antennaclaim.OpApprove, claimA); res.Status != antennaclaim.StatusApproved {
		t.Fatalf("approve: %+v", res)
	}
	if p := r.parts898(claimA); p.approvedBy != antennaclaim.ApprovedByAdmin {
		t.Fatalf("approved_by %q, want admin", p.approvedBy)
	}
}

// The switch is read by its exact spelling only, off by default, and a
// mis-cased key is ignored and logged.
func TestAntennaClaimAutoApproveDetailsGateReadsOnlyCanonicalKey1007(t *testing.T) {
	cases := []struct {
		name, cfg string
		want      bool
		warnings  int
	}{
		{"absent is off", `{"antennaClaims":{"enabled":true,"detailsEnabled":true}}`, false, 0},
		{"canonical on", `{"antennaClaims":{"enabled":true,"detailsEnabled":true,"autoApproveDetails":true}}`, true, 0},
		{"canonical false", `{"antennaClaims":{"detailsEnabled":true,"autoApproveDetails":false}}`, false, 0},
		{"needs detailsEnabled", `{"antennaClaims":{"enabled":true,"autoApproveDetails":true}}`, false, 0},
		{"AutoApproveDetails alone", `{"antennaClaims":{"detailsEnabled":true,"AutoApproveDetails":true}}`, false, 1},
		{"autoapprovedetails after canonical false", `{"antennaClaims":{"detailsEnabled":true,"autoApproveDetails":false,"autoapprovedetails":true}}`, false, 1},
		{"AUTOAPPROVEDETAILS before canonical false", `{"antennaClaims":{"detailsEnabled":true,"AUTOAPPROVEDETAILS":true,"autoApproveDetails":false}}`, false, 1},
		{"string true", `{"antennaClaims":{"detailsEnabled":true,"autoApproveDetails":"true"}}`, false, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.cfg), 0o644); err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			log.SetOutput(&buf)
			cfg, err := LoadConfig(path)
			log.SetOutput(os.Stderr)
			if tc.warnings < 0 {
				// A string is not a JSON boolean: encoding/json refuses it.
				if err == nil && cfg.AntennaClaims.AutoApproveDetailsOn() {
					t.Fatal(`"true" switched auto-approval of details on`)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.AntennaClaims.AutoApproveDetailsOn(); got != tc.want {
				t.Fatalf("AutoApproveDetailsOn = %v, want %v", got, tc.want)
			}
			if n := strings.Count(buf.String(), configkey.IgnoredNote); n != tc.warnings {
				t.Fatalf("%d warnings, want %d:\n%s", n, tc.warnings, buf.String())
			}
		})
	}
}

// The runner logs once when the switch is set without detailsEnabled, so
// an operator learns why nothing is auto-approved.
func TestAntennaClaim1007_WarnsWithoutDetailsEnabled(t *testing.T) {
	for _, tc := range []struct {
		cfg  *antennaclaim.Config
		warn bool
	}{
		{&antennaclaim.Config{AutoApproveDetails: true}, true},
		{&antennaclaim.Config{AutoApproveDetails: true, DetailsEnabled: true}, false},
		{&antennaclaim.Config{DetailsEnabled: true}, false},
		{nil, false},
	} {
		var buf bytes.Buffer
		log.SetOutput(&buf)
		newClaimRig(t, tc.cfg)
		log.SetOutput(os.Stderr)
		if got := strings.Contains(buf.String(), "autoApproveDetails"); got != tc.warn {
			t.Errorf("%+v: warned=%v, want %v:\n%s", tc.cfg, got, tc.warn, buf.String())
		}
	}
}
