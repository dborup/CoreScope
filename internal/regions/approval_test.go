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
