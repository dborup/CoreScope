package copdem

import (
	"strings"
	"testing"
)

// #738 M2 — the short credit line the profile UI shows on a mixed
// profile (decision 2). The page turns its two marks into links with the
// one credit walker (public/map-tile-providers.js appendCredit), which
// takes each mark after the previous one, so they must occur in the
// order dataset, licence, and each exactly once.
func TestCreditMarksInOrder(t *testing.T) {
	d := strings.Index(Credit, DatasetName)
	l := strings.Index(Credit, LicenseName)
	if d < 0 || l < 0 || d >= l {
		t.Fatalf("Credit %q: dataset mark at %d, licence mark at %d", Credit, d, l)
	}
	if strings.Count(Credit, DatasetName) != 1 || strings.Count(Credit, LicenseName) != 1 {
		t.Fatalf("Credit %q repeats a mark", Credit)
	}
	// The walker links only absolute https targets.
	for _, u := range []string{DatasetURL, LicenseURL} {
		if !strings.HasPrefix(u, "https://") {
			t.Fatalf("%q is not an https target", u)
		}
	}
	// The short line is short: the long Art. 6(b) line stays one step away.
	if len(Credit) >= len(AttributionDerived) {
		t.Fatalf("Credit (%d bytes) is not shorter than AttributionDerived (%d)", len(Credit), len(AttributionDerived))
	}
}
