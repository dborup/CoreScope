package buildings

// #730 M2 — the follow-ups the #776 review carried over from M1b (N1-N4),
// each reproduced here before it is fixed.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// TestFetcherWithoutNowUsesTheClock (#776 review N1): FetcherOptions.Now
// had no default, so a Fetcher built from options that leave it nil
// panicked on its first Fetch. It must read the real clock instead.
func TestFetcherWithoutNowUsesTheClock(t *testing.T) {
	src := &fakeSource{page: fixturePage(t)}
	o := DefaultFetcherOptions()
	o.Now = nil
	f := NewFetcher(src, o)
	seg := dokk1Segment(t)
	for i := 0; i < 2; i++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Fetch %d with a nil Now panicked: %v", i+1, r)
				}
			}()
			if _, err := f.Fetch(context.Background(), seg); err != nil {
				t.Fatalf("Fetch %d: %v", i+1, err)
			}
		}()
	}
	// The second Fetch is a cache hit on the real clock: the page's
	// expiry is a day ahead of time.Now, not of the zero time.
	if n := src.calls.Load(); n != 1 {
		t.Fatalf("%d upstream calls, want 1 (the second Fetch is a cache hit)", n)
	}
	if got := f.now(); time.Since(got) > time.Minute || time.Until(got) > time.Minute {
		t.Fatalf("now() = %v, want the wall clock", got)
	}
}

// TestQueryLimitIsBuiltFromMaxFeatures (#776 review N2): the query's
// limit was the literal "1000" beside the constant MaxFeatures, pinned
// together only by a test, so changing one would leave the other behind
// until that test ran. The limit must be written from MaxFeatures itself:
// fixedQuery's expression holds strconv.Itoa(MaxFeatures), and none of its
// string literals carries a number after "limit=". Both values are 1000
// today, so only the source can show the drift.
func TestQueryLimitIsBuiltFromMaxFeatures(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "client.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var expr ast.Expr
	ast.Inspect(file, func(n ast.Node) bool {
		if vs, ok := n.(*ast.ValueSpec); ok {
			for i, name := range vs.Names {
				if name.Name == "fixedQuery" && i < len(vs.Values) {
					expr = vs.Values[i]
				}
			}
		}
		return expr == nil
	})
	if expr == nil {
		t.Fatal("client.go declares no fixedQuery")
	}
	literalLimit := regexp.MustCompile(`limit=[0-9]`)
	fromMax := false
	ast.Inspect(expr, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BasicLit:
			if v, err := strconv.Unquote(n.Value); err == nil && literalLimit.MatchString(v) {
				t.Errorf("fixedQuery spells the limit as a literal: %s", n.Value)
			}
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if pkg, isIdent := selX(sel); ok && isIdent && pkg == "strconv" && sel.Sel.Name == "Itoa" && len(n.Args) == 1 {
				if id, ok := n.Args[0].(*ast.Ident); ok && id.Name == "MaxFeatures" {
					fromMax = true
				}
			}
		}
		return true
	})
	if !fromMax {
		t.Error("fixedQuery does not write its limit with strconv.Itoa(MaxFeatures)")
	}
}

func selX(sel *ast.SelectorExpr) (string, bool) {
	if sel == nil {
		return "", false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	return id.Name, true
}
