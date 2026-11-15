package buildings

import (
	"strings"
	"testing"

	"github.com/meshcore-analyzer/dhm"
)

// TestAttributionDerived (#730 M2): the profile's credit with the
// footprints is the coordinator's line, word for word, built from DHM's
// own credit; it is plain text, and the handle is no link.
func TestAttributionDerived(t *testing.T) {
	const want = "Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel og INSPIRE – Bygninger (GeoDanmark, @geodanmark), CC BY 4.0 — data bearbejdet"
	if AttributionDerived != want {
		t.Fatalf("AttributionDerived = %q, want %q", AttributionDerived, want)
	}
	head, tail, ok := strings.Cut(dhm.AttributionDerived, dhm.DatasetName)
	if !ok || !strings.HasPrefix(AttributionDerived, head+dhm.DatasetName) || !strings.HasSuffix(AttributionDerived, tail) {
		t.Fatalf("AttributionDerived %q is not DHM's derived credit %q with the footprints added", AttributionDerived, dhm.AttributionDerived)
	}
	for _, part := range []string{dhm.Owner, dhm.DatasetName, DatasetName, Source, SourceHandle, dhm.LicenseName, dhm.DerivedNote} {
		if !strings.Contains(AttributionDerived, part) {
			t.Errorf("AttributionDerived lacks %q", part)
		}
	}
	for name, v := range map[string]string{"DatasetName": DatasetName, "Source": Source, "SourceHandle": SourceHandle, "AttributionDerived": AttributionDerived} {
		if strings.ContainsAny(v, "<>&\"'") || strings.Contains(v, "://") {
			t.Errorf("%s must be plain text without a link: %q", name, v)
		}
	}
	for name, u := range map[string]string{"DatasetURL": DatasetURL, "SourceTermsURL": SourceTermsURL} {
		if !strings.HasPrefix(u, "https://") {
			t.Errorf("%s = %q, want an absolute https URL", name, u)
		}
	}
}
