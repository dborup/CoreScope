package cardkit

import "testing"

func TestFmtCount(t *testing.T) {
	for v, want := range map[int]string{0: "0", 7: "7", 999: "999", 1000: "1,000", 1234567: "1,234,567", -123: "-123", -1234: "-1,234"} {
		if got := FmtCount(v); got != want {
			t.Errorf("FmtCount(%d) = %q, want %q", v, got, want)
		}
	}
}

// ToFixed1 is JS toFixed(1): an exact binary tie rounds up, where Go's
// formatter rounds it to even; anything else is the nearest decimal.
func TestToFixed1(t *testing.T) {
	for v, want := range map[float64]string{0: "0.0", 1.25: "1.3", 0.75: "0.8", 2.25: "2.3", 1.45: "1.4", 0.35: "0.3", 4.2: "4.2", 9.96: "10.0"} {
		if got := ToFixed1(v); got != want {
			t.Errorf("ToFixed1(%v) = %q, want %q", v, got, want)
		}
	}
}
