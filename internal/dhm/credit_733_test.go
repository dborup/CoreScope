package dhm

// #733 — Dataforsyningen's terms for Klimadatastyrelsen data under
// CC BY 4.0 ("Vilkår for brug af data", v3.1, August 2024,
// https://dataforsyningen.dk/Vilkaar) require a credit to
//
//  1. name the data owner;
//  2. name the licence and link to it;
//  3. link to the specific dataset (or the Dataforsyningen data overview);
//  4. state whether the data was changed.
//
// internal/dhm is the one place those parts are spelled out, so the map
// overlay, the profile footer and the share card all show the same credit.
// Every part is pinned below: dropping one, or pointing a link somewhere
// else, fails here rather than silently going live out of compliance.

import (
	"strings"
	"testing"
)

// creditParts is every credit string, by the name it has in the package, so
// the plain-text and scheme checks below cannot miss one.
func creditParts() map[string]string {
	return map[string]string{
		"Owner":              Owner,
		"DatasetName":        DatasetName,
		"DatasetURL":         DatasetURL,
		"LicenseName":        LicenseName,
		"LicenseURL":         LicenseURL,
		"DerivedNote":        DerivedNote,
		"Attribution":        Attribution,
		"AttributionDerived": AttributionDerived,
	}
}

// 1 + 2 + 3: the unchanged credit names the owner, names the dataset and
// names the licence, and the two link targets are the dataset page and the
// licence deed.
func TestCreditNamesOwnerDatasetAndLicence(t *testing.T) {
	if Owner != "Klimadatastyrelsen" {
		t.Errorf("Owner = %q, want the data owner Dataforsyningen's terms name", Owner)
	}
	if DatasetName != "Danmarks Højdemodel" {
		t.Errorf("DatasetName = %q", DatasetName)
	}
	// DHM/Terræn on Dataforsyningen. The terms accept the dataset page or
	// the data overview; anywhere else is not a dataset link.
	if DatasetURL != "https://dataforsyningen.dk/data/930" {
		t.Errorf("DatasetURL = %q, want the Danmarks Højdemodel dataset page on Dataforsyningen", DatasetURL)
	}
	if LicenseName != "CC BY 4.0" {
		t.Errorf("LicenseName = %q", LicenseName)
	}
	if LicenseURL != "https://creativecommons.org/licenses/by/4.0/" {
		t.Errorf("LicenseURL = %q, want the CC BY 4.0 deed", LicenseURL)
	}
	for _, part := range []string{Owner, DatasetName, LicenseName} {
		if !strings.Contains(Attribution, part) {
			t.Errorf("Attribution %q does not name %q", Attribution, part)
		}
	}
}

// 4: a view CoreScope computes from DHM says so, and the unchanged
// hillshade tiles do not.
func TestDerivedCreditStatesTheDataWasChanged(t *testing.T) {
	if DerivedNote != "data bearbejdet" {
		t.Errorf("DerivedNote = %q, want the statement that the data was changed", DerivedNote)
	}
	if !strings.HasPrefix(AttributionDerived, Attribution) {
		t.Errorf("AttributionDerived %q does not start with the shared credit %q", AttributionDerived, Attribution)
	}
	if !strings.Contains(AttributionDerived, DerivedNote) {
		t.Errorf("AttributionDerived %q lacks the derived note %q", AttributionDerived, DerivedNote)
	}
	// The agency renders the hillshade tiles and CoreScope shows them as
	// served, so the plain credit must not claim they were changed.
	if strings.Contains(Attribution, DerivedNote) {
		t.Errorf("Attribution %q claims the data was changed", Attribution)
	}
	if AttributionDerived == Attribution {
		t.Error("AttributionDerived adds nothing to Attribution")
	}
}

// Every credit string is plain text, so each renderer (Leaflet's control,
// the SVG card, the panel's DOM) escapes it its own way, and both links are
// absolute https, so a browser may turn them into anchors.
func TestCreditPartsArePlainTextAndHTTPS(t *testing.T) {
	for name, v := range creditParts() {
		if v == "" {
			t.Errorf("%s is empty", name)
		}
		if strings.ContainsAny(v, "<>&\"'") {
			t.Errorf("%s must be plain text: %q", name, v)
		}
		if strings.TrimSpace(v) != v {
			t.Errorf("%s has surrounding space: %q", name, v)
		}
	}
	for name, u := range map[string]string{"DatasetURL": DatasetURL, "LicenseURL": LicenseURL} {
		if !strings.HasPrefix(u, "https://") {
			t.Errorf("%s = %q, want an absolute https URL", name, u)
		}
	}
}
