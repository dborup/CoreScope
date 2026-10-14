package regions

import "testing"

func TestValidateCandidateExactCaseSensitiveASCII(t *testing.T) {
	for _, name := range []string{"#DK", "#dk-123", "#A_B", "#a$", "#Ærø"} {
		if err := ValidateCandidate(name); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
	for _, name := range []string{"", "#", "DK", "#bad name", "#with/slash", "#line\n", "#" + string(make([]byte, 31))} {
		if err := ValidateCandidate(name); err == nil {
			t.Errorf("accepted invalid %q", name)
		}
	}
}

// Review of #355: an approved name must compare identically to a configured
// one, so it has to be a fixed point of Normalize — the rule hashRegions and
// hashRegionsPath entries go through. Unicode whitespace passes the
// firmware byte classes (every byte is >= 'A') but Normalize trims it.
func TestValidateCandidateRequiresNormalizedName(t *testing.T) {
	for _, name := range []string{"#a ", "#dk ", "#x　", "#y\u0085"} {
		if err := ValidateCandidate(name); err == nil {
			t.Errorf("accepted %q, which Normalize turns into another name", name)
		}
	}
	for _, name := range []string{"#DK", "#dk-123", "#Ærø"} {
		if got, ok := Normalize(name); !ok || got != name {
			t.Fatalf("fixture %q is not normalized", name)
		}
	}
}
