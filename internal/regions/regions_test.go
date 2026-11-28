package regions

import (
	"encoding/hex"
	"reflect"
	"testing"
)

func TestNormalize(t *testing.T) {
	cases := []struct {
		raw     string
		wantOk  bool
		wantVal string
	}{
		{"dk", true, "#dk"},
		{"#dk", true, "#dk"},
		{"  dk-oj  ", true, "#dk-oj"},
		{"  #dk-oj  ", true, "#dk-oj"},
		{"", false, ""},
		{"   ", false, ""},
	}
	for _, c := range cases {
		got, ok := Normalize(c.raw)
		if ok != c.wantOk || got != c.wantVal {
			t.Errorf("Normalize(%q) = (%q, %v), want (%q, %v)", c.raw, got, ok, c.wantVal, c.wantOk)
		}
	}
}

func TestNormalizeNames(t *testing.T) {
	got := NormalizeNames([]string{"dk", "#dk", " dk-oj ", "", "  ", "dk-oj"})
	want := []string{"#dk", "#dk-oj"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NormalizeNames = %v, want %v", got, want)
	}
}

func TestNormalizeNamesPreservesFirstSeenOrder(t *testing.T) {
	got := NormalizeNames([]string{"zeta", "alpha", "#zeta"})
	want := []string{"#zeta", "#alpha"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NormalizeNames = %v, want %v (order should follow first appearance, not be sorted)", got, want)
	}
}

func TestNormalizeNamesEmptyInput(t *testing.T) {
	got := NormalizeNames(nil)
	if len(got) != 0 {
		t.Errorf("NormalizeNames(nil) = %v, want empty", got)
	}
}

// Key is MeshCore's TransportKeyStore::getAutoKeyFor: SHA-256 of the exact
// name bytes, truncated to the 16-byte TransportKey.
func TestKeyMatchesFirmwareAutoKey(t *testing.T) {
	if got := hex.EncodeToString(Key("#dk")); got != "4a447e7539b0418bd14cf28aa8241529" {
		t.Fatalf("Key(#dk) = %s", got)
	}
	if hex.EncodeToString(Key("#DK")) == hex.EncodeToString(Key("#dk")) {
		t.Fatal("Key must not fold case")
	}
}
