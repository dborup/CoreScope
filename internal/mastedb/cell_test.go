package mastedb

import (
	"math"
	"testing"

	"github.com/meshcore-analyzer/dhm"
)

// The WGS84 bounding box of mastedatabasen:MasteIBrug in the WFS
// GetCapabilities read on 2026-11-14.
const (
	capsMinLon, capsMinLat = 8.061374533838935, 54.45559748126344
	capsMaxLon, capsMaxLat = 15.5667740651819, 57.7482011320401
)

// TestCellExtentCoversCapabilities projects the capabilities bbox outline
// to EPSG:25832 and checks that the cell extent covers it exactly: every
// outline point is in a valid cell, and every extent bound is reached.
func TestCellExtentCoversCapabilities(t *testing.T) {
	minE, minN, maxE, maxN := math.MaxInt, math.MaxInt, math.MinInt, math.MinInt
	const steps = 2000
	for i := 0; i <= steps; i++ {
		f := float64(i) / steps
		lon := capsMinLon + f*(capsMaxLon-capsMinLon)
		lat := capsMinLat + f*(capsMaxLat-capsMinLat)
		for _, p := range [][2]float64{{capsMinLat, lon}, {capsMaxLat, lon}, {lat, capsMinLon}, {lat, capsMaxLon}} {
			e, n := dhm.ToUTM32(p[0], p[1])
			c := Cell{E: int(math.Floor(e / CellSizeM)), N: int(math.Floor(n / CellSizeM))}
			if !c.Valid() {
				t.Fatalf("outline point %v → cell %+v is not Valid", p, c)
			}
			minE, minN = min(minE, c.E), min(minN, c.N)
			maxE, maxN = max(maxE, c.E), max(maxN, c.N)
		}
	}
	if minE != MinCellE || maxE != MaxCellE || minN != MinCellN || maxN != MaxCellN {
		t.Fatalf("extent E %d..%d N %d..%d, constants E %d..%d N %d..%d",
			minE, maxE, minN, maxN, MinCellE, MaxCellE, MinCellN, MaxCellN)
	}
}

func TestCellValid(t *testing.T) {
	cases := []struct {
		c    Cell
		want bool
	}{
		{Cell{MinCellE, MinCellN}, true},
		{Cell{MaxCellE, MaxCellN}, true},
		{testCell, true},
		{Cell{MinCellE - 1, MinCellN}, false},
		{Cell{MaxCellE + 1, MinCellN}, false},
		{Cell{MinCellE, MinCellN - 1}, false},
		{Cell{MinCellE, MaxCellN + 1}, false},
		{Cell{0, 0}, false},
		{Cell{-263, -3127}, false},
	}
	for _, tc := range cases {
		if got := tc.c.Valid(); got != tc.want {
			t.Errorf("%+v.Valid() = %v, want %v", tc.c, got, tc.want)
		}
	}
}

func TestCellBBox(t *testing.T) {
	want := [4]float64{526000, 6254000, 528000, 6256000}
	if got := testCell.BBox(); got != want {
		t.Fatalf("BBox = %v, want %v", got, want)
	}
}

// TestCellContainsHalfOpen: a point on an edge shared by two cells is in
// exactly one of them, the one whose minimum edge it lies on.
func TestCellContainsHalfOpen(t *testing.T) {
	east := Cell{testCell.E + 1, testCell.N}
	north := Cell{testCell.E, testCell.N + 1}
	cases := []struct {
		name     string
		c        Cell
		e, n     float64
		contains bool
	}{
		{"min corner", testCell, 526000, 6254000, true},
		{"inside", testCell, 527000, 6255000, true},
		{"just below max E", testCell, 527999.999, 6255000, true},
		{"on max E", testCell, 528000, 6255000, false},
		{"on max E, east cell", east, 528000, 6255000, true},
		{"on max N", testCell, 527000, 6256000, false},
		{"on max N, north cell", north, 527000, 6256000, true},
		{"below min E", testCell, 525999.999, 6255000, false},
		{"below min N", testCell, 527000, 6253999.999, false},
	}
	for _, tc := range cases {
		if got := tc.c.Contains(tc.e, tc.n); got != tc.contains {
			t.Errorf("%s: Contains = %v, want %v", tc.name, got, tc.contains)
		}
	}
}
