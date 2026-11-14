package mastedb

import (
	"testing"

	"github.com/meshcore-analyzer/mastedb/mastedbtest"
)

// mastedbtest cannot import mastedb (mastedb's own tests use it), so it
// carries its own copy of the cell size. Every row it generates must parse
// and land inside the requested cell, at its documented maximum too.
func TestGMLHelperRowsInsideCell(t *testing.T) {
	for _, cell := range []Cell{testCell, {E: MinCellE, N: MinCellN}, {E: MaxCellE, N: MaxCellN}} {
		const n = 10560
		page, err := Parse(mastedbtest.GML(cell.E, cell.N, n, "10560"), cell)
		if err != nil {
			t.Fatalf("cell %v: %v", cell, err)
		}
		if len(page.Rows) != n {
			t.Fatalf("cell %v: %d of %d rows inside the cell", cell, len(page.Rows), n)
		}
		for _, s := range Sites(page.Rows) {
			if !s.Interference {
				t.Fatalf("cell %v: site %v not interference-relevant", cell, s.Stations)
			}
		}
	}
}
