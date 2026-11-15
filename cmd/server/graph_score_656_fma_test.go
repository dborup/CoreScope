package main

// Issue #656: TestGraphScore629_GoldenBitIdentical pins every Bridge and
// Coverage score bit for bit. The Go spec lets a compiler fuse x*y + z into one
// fused multiply-add (one rounding instead of two) unless an explicit
// conversion forces the product to be rounded: float64(x*y) + z. arm64, ppc64
// and s390x fuse; amd64 does not. Unguarded multiply-adds therefore made the
// golden test fail in the last ulp on Apple Silicon while CI (amd64) stayed
// green.
//
// The golden test only catches such a site on an FMA-fusing machine, so this
// guard checks the source instead, on every architecture: no float
// multiply may feed an add or subtract directly in the files that compute the
// pinned scores or generate the pinned synthetic graphs.

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// graphScore656PinnedFiles compute the pinned Bridge/Coverage scores or the
// synthetic input graphs of TestGraphScore629_GoldenBitIdentical.
var graphScore656PinnedFiles = []string{
	"bridge_score.go",
	"coverage_score.go",
	"graph_weighted.go",
	"graph_score_561_bench_test.go",
	"graph_score_561_test.go",
	"graph_score_629_bench_test.go",
}

// fusableFloatMulAdds returns "file:line" for every float multiply that is a
// direct operand of + or - (also += / -=) without an explicit conversion
// around it, i.e. every site a compiler may legally fuse into an FMA.
func fusableFloatMulAdds(t *testing.T, files []string) []string {
	t.Helper()
	fset := token.NewFileSet()
	var parsed []*ast.File
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, f)
	}
	info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue)}
	conf := types.Config{
		Importer: importer.ForCompiler(fset, "source", nil),
		// The files are a subset of the package: references to the rest of
		// it do not resolve. Every float expression in the checked arithmetic
		// still gets a type, which is all this guard needs.
		Error: func(error) {},
	}
	_, _ = conf.Check("main", fset, parsed, info)

	isFloat := func(e ast.Expr) bool {
		tv, ok := info.Types[e]
		if !ok || tv.Value != nil { // constant expressions are folded exactly
			return false
		}
		b, ok := tv.Type.Underlying().(*types.Basic)
		return ok && b.Info()&types.IsFloat != 0
	}
	// isUnroundedMul: e is a float product whose result is not forced to be
	// rounded (parentheses and unary minus do not round; a conversion does).
	isUnroundedMul := func(e ast.Expr) bool {
		for {
			switch x := e.(type) {
			case *ast.ParenExpr:
				e = x.X
				continue
			case *ast.UnaryExpr:
				if x.Op == token.SUB || x.Op == token.ADD {
					e = x.X
					continue
				}
			case *ast.BinaryExpr:
				return x.Op == token.MUL && isFloat(x)
			}
			return false
		}
	}

	var sites []string
	report := func(n ast.Node) {
		p := fset.Position(n.Pos())
		sites = append(sites, p.Filename+":"+strconv.Itoa(p.Line))
	}
	for _, f := range parsed {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BinaryExpr:
				if (x.Op == token.ADD || x.Op == token.SUB) && isFloat(x) &&
					(isUnroundedMul(x.X) || isUnroundedMul(x.Y)) {
					report(x)
				}
			case *ast.AssignStmt:
				if (x.Tok == token.ADD_ASSIGN || x.Tok == token.SUB_ASSIGN) &&
					len(x.Rhs) == 1 && isFloat(x.Lhs[0]) && isUnroundedMul(x.Rhs[0]) {
					report(x)
				}
			}
			return true
		})
	}
	return sites
}

func TestGraphScore656_NoFusableMulAddInPinnedScores(t *testing.T) {
	if sites := fusableFloatMulAdds(t, graphScore656PinnedFiles); len(sites) > 0 {
		t.Fatalf("float multiply-add the compiler may fuse into an FMA (differs on arm64 vs amd64, breaks the #629 golden pin); "+
			"wrap the product in float64(...): %s", strings.Join(sites, ", "))
	}
}

// The detector itself: it must flag every fusable shape and none of the
// explicitly rounded ones, otherwise the guard above proves nothing.
func TestGraphScore656_DetectorFlagsFusableShapes(t *testing.T) {
	src := `package main

func fused(a, b, c float64, s []float64, i, j int) {
	_ = a*b + c         // flagged
	_ = c - a*b         // flagged
	_ = (a * b) + c     // flagged, parens do not round
	_ = -(a * b) + c    // flagged
	s[0] += a * (1 + c) // flagged
	s[0] -= (a / b) * c // flagged
	_ = a*b*c + c       // flagged
}

func rounded(a, b, c float64, s []float64, i, j int) {
	_ = float64(a*b) + c
	_ = c - float64(a*b)
	s[0] += float64(a * (1 + c))
	_ = a/b + c
	_ = a*b
	_ = 2*i + j
	_ = 0.5*2 + 1
}
`
	dir := t.TempDir()
	path := filepath.Join(dir, "fma_shapes.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	got := fusableFloatMulAdds(t, []string{path})
	var lines []string
	for _, s := range got {
		lines = append(lines, s[strings.LastIndexByte(s, ':')+1:])
	}
	want := []string{"4", "5", "6", "7", "8", "9", "10"}
	if strings.Join(lines, ",") != strings.Join(want, ",") {
		t.Fatalf("flagged lines %v, want %v", lines, want)
	}
}
