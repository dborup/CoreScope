package main

// #733 — the server's half of the CC BY 4.0 credit compliance work.
//
// Dataforsyningen's terms ("Vilkår for brug af data", v3.1, August 2024,
// https://dataforsyningen.dk/Vilkaar) require a credit for Klimadatastyrelsen
// data to name the owner, name and link the licence, link the dataset, and
// state whether the data was changed. internal/dhm owns the strings (see
// internal/dhm/credit_733_test.go); this file pins that the server hands
// them to the browser and uses the derived form where the data is derived.
//
// The other two overlays are checked against their own owners' terms here
// too, so "we looked" is a test and not a claim in a commit message.

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/dhm"
)

// clientDHMRaw returns the raw `dhm` block of /api/config/client, or nil
// when the key is absent.
func clientDHMRaw(t *testing.T, router *mux.Router) json.RawMessage {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/config/client", nil))
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body["dhm"]
}

// The overlay credit is rendered by the browser, so the link targets have
// to reach it. The text itself stays plain: the page turns the dataset and
// licence marks inside it into links, which is why the marks travel with
// the URLs.
func TestClientDHMBlockCarriesCreditLinks(t *testing.T) {
	up := newFakeUpstream(t, nil)
	srv, router := setupTestServer(t)
	srv.cfg.DHM = DHMConfig{Enabled: true, Token: dhmTestToken}
	srv.dhm = newTestDHMProxy(t, up, &dhmLogSink{}, nil)

	raw := clientDHMRaw(t, router)
	if raw == nil {
		t.Fatal("no dhm block while the proxy is active")
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(keys))
	for k := range keys {
		got = append(got, k)
	}
	sort.Strings(got)
	// Exactly these keys: the four credit fields the links need, the
	// derived credit the profile footer shows (#765), the per-overlay
	// dataset links (#734), and nothing else new.
	want := []string{
		"attribution", "attributionDerived", "bounds", "datasetName", "datasetUrl", "enabled",
		"layer", "licenseName", "licenseUrl", "maxZoom", "minZoom",
		"overlays", "profile", "share", "url",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dhm block keys %v, want %v", got, want)
	}

	var b DHMClientConfig
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, got, want string }{
		{"attribution", b.Attribution, dhm.Attribution},
		{"datasetName", b.DatasetName, dhm.DatasetName},
		{"datasetUrl", b.DatasetURL, dhm.DatasetURL},
		{"licenseName", b.LicenseName, dhm.LicenseName},
		{"licenseUrl", b.LicenseURL, dhm.LicenseURL},
	} {
		if c.got != c.want {
			t.Errorf("dhm.%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	// Both marks must occur in the text, or the page has nothing to turn
	// into a link.
	for _, mark := range []string{b.DatasetName, b.LicenseName} {
		if !strings.Contains(b.Attribution, mark) {
			t.Errorf("attribution %q does not contain the link mark %q", b.Attribution, mark)
		}
	}
	// The hillshade tiles are the agency's own rendering, shown as served,
	// so the overlay credit must not say the data was changed.
	if strings.Contains(b.Attribution, dhm.DerivedNote) {
		t.Errorf("overlay credit claims derived data: %q", b.Attribution)
	}
	// #765: the profile footer is built from this block, so the derived
	// credit travels with it and public/ keeps no copy of any credit
	// string. Read from the raw keys, so a block without it fails here and
	// not only in the browser.
	var derived string
	if err := json.Unmarshal(keys["attributionDerived"], &derived); err != nil {
		t.Fatalf("dhm.attributionDerived: %v (block %s)", err, raw)
	}
	if derived != dhm.AttributionDerived {
		t.Errorf("dhm.attributionDerived = %q, want %q", derived, dhm.AttributionDerived)
	}
	// The footer turns the same two marks into links, so both must occur
	// in the derived credit too.
	for _, mark := range []string{b.DatasetName, b.LicenseName} {
		if !strings.Contains(derived, mark) {
			t.Errorf("attributionDerived %q does not contain the link mark %q", derived, mark)
		}
	}
	// Five more public strings must not become five more ways to leak the
	// token.
	if strings.Contains(string(raw), dhmTestToken) {
		t.Fatalf("token in the dhm block: %s", raw)
	}
}

// The profile is computed from DHM — sampled, interpolated and assessed —
// so its credit is the derived one. The API says so in its own body, which
// is what a consumer other than our own page reads.
func TestTerrainProfileResponseCreditsDerivedData(t *testing.T) {
	env := newTerrainEnv(t, newFakeElevation(t, flatElev(12)), dhm.ProfileConfig{}, nil)
	resp := decodeProfile(t, env.get(pathQuery(aarhusPt, offsetPt(56.1629, 10.2039, 4000, 90))))
	if resp.Attribution != dhm.AttributionDerived {
		t.Fatalf("profile attribution %q, want the derived credit %q", resp.Attribution, dhm.AttributionDerived)
	}
	if !strings.Contains(resp.Attribution, dhm.DerivedNote) {
		t.Errorf("profile attribution %q does not say the data was changed", resp.Attribution)
	}
}

// #723 — the protected ancient monuments overlay is checked against its own
// owner's terms, not Dataforsyningen's. Fund og Fortidsminder's rules
// (https://www.kulturarv.dk/fundogfortidsminder/Regler) make the service
// free for most purposes with no credit requirement, and Slots- og
// Kulturstyrelsen publishes the register's textual data under CC0
// (https://slks.dk/omraader/kulturarv/databaserne/rettigheder-til-data).
// CC0 waives attribution, so no licence link and no dataset link are owed:
// the "© Slots- og Kulturstyrelsen" line CoreScope shows is a courtesy that
// already exceeds the terms. Only the agency's images and documents carry
// stricter conditions, and the overlay draws neither.
func TestHeritageCreditNeedsNoLicenceOrDatasetLink(t *testing.T) {
	b := heritageClientConfig()
	if b.Attribution != "© Slots- og Kulturstyrelsen" {
		t.Errorf("heritage attribution %q", b.Attribution)
	}
	if !strings.Contains(b.Attribution, "Slots- og Kulturstyrelsen") {
		t.Errorf("heritage credit does not name the data owner: %q", b.Attribution)
	}
	// Not CC BY: no licence mark, and so nothing to link.
	for _, mark := range []string{"CC BY", dhm.LicenseName, dhm.LicenseURL, dhm.DatasetURL, dhm.DerivedNote} {
		if strings.Contains(b.Attribution, mark) {
			t.Errorf("heritage credit carries the DHM mark %q: %q", mark, b.Attribution)
		}
	}
	// The block stays link-free: adding one would be a claim about terms
	// that do not ask for it.
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"datasetUrl", "licenseUrl", "derivedNote"} {
		if strings.Contains(string(raw), k) {
			t.Errorf("heritage block carries %q: %s", k, raw)
		}
	}
}
