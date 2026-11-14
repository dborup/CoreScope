package mastedb

import (
	"os"
	"testing"

	"github.com/meshcore-analyzer/mastedb/mastedbtest"
)

// testCell is the cell of testdata/cell_small.gml.
var testCell = Cell{E: 263, N: 3127}

func readFixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// genGML is mastedbtest.GML for testCell: n rows at sites of six rows each,
// the density observed in the #724 samples. matched is the numberMatched
// attribute verbatim.
func genGML(n int, matched string) []byte {
	return mastedbtest.GML(testCell.E, testCell.N, n, matched)
}
