package heritage

import (
	"math"
	"testing"

	"github.com/meshcore-analyzer/dhm"
)

// TestCellExtentCoversCapabilities recomputes the cell extent from the
// layers' EX_GeographicBoundingBox (the same box cmd/server sends the
// overlay as its bounds) by walking its edges in EPSG:25832.
func TestCellExtentCoversCapabilities(t *testing.T) {
	const south, west, north, east = 54.4646, 8.0899, 57.7289, 15.5689
	minE, minN, maxE, maxN := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for i := 0; i <= 1000; i++ {
		f := float64(i) / 1000
		lat, lon := south+f*(north-south), west+f*(east-west)
		for _, p := range [][2]float64{{south, lon}, {north, lon}, {lat, west}, {lat, east}} {
			e, n := dhm.ToUTM32(p[0], p[1])
			minE, maxE = math.Min(minE, e), math.Max(maxE, e)
			minN, maxN = math.Min(minN, n), math.Max(maxN, n)
		}
	}
	got := [4]int{MinCellE, MinCellN, MaxCellE, MaxCellN}
	want := [4]int{int(math.Floor(minE / CellSizeM)), int(math.Floor(minN / CellSizeM)),
		int(math.Floor(maxE / CellSizeM)), int(math.Floor(maxN / CellSizeM))}
	if got != want {
		t.Fatalf("extent %v, want %v", got, want)
	}
}

func TestCellOfAndValid(t *testing.T) {
	c, ok := CellOf(526400, 6179600)
	if !ok || c != (Cell{2105, 24718}) {
		t.Fatalf("CellOf %+v %v", c, ok)
	}
	// Half-open: the shared edge belongs to the cell above / right.
	if c, _ := CellOf(526500, 6179750); c != (Cell{2106, 24719}) {
		t.Fatalf("edge %+v", c)
	}
	for _, p := range [][2]float64{{0, 0}, {MinCellE*CellSizeM - 1, 6179600}, {526400, (MaxCellN + 1) * CellSizeM},
		{math.NaN(), 6179600}, {math.Inf(1), 6179600}, {526400, math.Inf(-1)}} {
		if _, ok := CellOf(p[0], p[1]); ok {
			t.Errorf("CellOf%v accepted", p)
		}
	}
	for _, c := range []Cell{{MinCellE, MinCellN}, {MaxCellE, MaxCellN}} {
		if !c.Valid() {
			t.Errorf("%+v invalid", c)
		}
	}
	for _, c := range []Cell{{MinCellE - 1, MinCellN}, {MaxCellE, MaxCellN + 1}} {
		if c.Valid() {
			t.Errorf("%+v valid", c)
		}
	}
}

// TestQueryBoxCoversTheRadiusOfEveryPointInTheCell: the box is the cell
// grown by the margin on every side, so the radius around any point of
// the cell lies inside it, and the box is never wider than one cell plus
// twice the largest margin.
func TestQueryBoxCoversTheRadiusOfEveryPointInTheCell(t *testing.T) {
	c := Cell{2105, 24718}
	b := c.QueryBox(100)
	if b != [4]int{526150, 6179400, 526600, 6179850} {
		t.Fatalf("box %v", b)
	}
	if w := MaxMarginM*2 + CellSizeM; w > 750 {
		t.Fatalf("widest box %d m", w)
	}
}
