package main

import (
	"math"
	"testing"
)

// TestIsValidNodePosition covers the #381 review round 2 findings (2 and 3):
// GetNodePositionsByKeys, resolveNodesByPubkey, and resolveNodesByName all
// share this one validity check, so a position is only ever treated as
// "real" when it is a plausible coordinate -- not the (0,0) sentinel, not
// NaN/Inf, and within the valid latitude/longitude ranges.
func TestIsValidNodePosition(t *testing.T) {
	cases := []struct {
		name     string
		lat, lon float64
		want     bool
	}{
		{"ordinary position", 10.0001, 20.0001, true},
		{"negative ordinary position", -33.5, -70.5, true},
		{"zero,zero sentinel", 0, 0, false},
		{"zero lat, nonzero lon is real", 0, 20.0001, true},
		{"nonzero lat, zero lon is real", 10.0001, 0, true},
		{"lat at +90 boundary", 90, 20.0001, true},
		{"lat at -90 boundary", -90, 20.0001, true},
		{"lon at +180 boundary", 10.0001, 180, true},
		{"lon at -180 boundary", 10.0001, -180, true},
		{"lat just over +90", 90.0001, 20.0001, false},
		{"lat just under -90", -90.0001, 20.0001, false},
		{"lon just over +180", 10.0001, 180.0001, false},
		{"lon just under -180", 10.0001, -180.0001, false},
		{"lat is NaN", math.NaN(), 20.0001, false},
		{"lon is NaN", 10.0001, math.NaN(), false},
		{"lat is +Inf", math.Inf(1), 20.0001, false},
		{"lon is -Inf", 10.0001, math.Inf(-1), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isValidNodePosition(c.lat, c.lon); got != c.want {
				t.Errorf("isValidNodePosition(%v, %v) = %v, want %v", c.lat, c.lon, got, c.want)
			}
		})
	}
}
