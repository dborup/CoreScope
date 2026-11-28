package buildings

// #730 M2 — the follow-ups the #776 review carried over from M1b (N1-N4),
// each reproduced here before it is fixed.

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dhm"
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

// TestClassifyHighSkipShareIsUnusable (#776 review N3): Page.Skipped was
// never read, so a page whose features were all skipped read as a corridor
// without buildings, and every elevated sample in it became Vegetation. A
// skipped feature is a building whose footprint was lost, so a page that
// lost more than 1 in 20 of its features cannot classify its segment: its
// samples stay unclassified. At or below that share the page is used.
func TestClassifyHighSkipShareIsUnusable(t *testing.T) {
	c := mustCorridor(t, [2]float64{570000, 6223000}, [2]float64{575000, 6223000}) // 1 segment
	s := profileAlong(c, 25, 20, constant(12))
	full := rect(c, 0, c.LengthM(), -30, 30)
	footprints := func(n int) []dhm.MultiPolygon {
		out := make([]dhm.MultiPolygon, n)
		for i := range out {
			out[i] = full
		}
		return out
	}
	for name, tc := range map[string]struct {
		page Page
		want Class
	}{
		"all skipped":     {Page{Skipped: 3}, Unclassified},
		"1 of 1 skipped":  {Page{Skipped: 1}, Unclassified},
		"2 of 20 skipped": {Page{Footprints: footprints(18), Skipped: 2}, Unclassified},
		"1 of 19 skipped": {Page{Footprints: footprints(18), Skipped: 1}, Unclassified},
		"1 of 20 skipped": {Page{Footprints: footprints(19), Skipped: 1}, Building},
		"none skipped":    {Page{Footprints: footprints(1)}, Building},
		"empty page":      {Page{}, Vegetation},
	} {
		page := tc.page
		got := mustClassify(t, c, s, []*Page{&page})
		for i, cl := range got {
			if cl != tc.want {
				t.Errorf("%s: sample %d is %q, want %q", name, i, cl, tc.want)
				break
			}
		}
	}
}

// TestOverFullPageIsTruncated (#776 review N4): a page with more features
// than the MaxFeatures it was asked for failed whole, as a "bad response",
// so the server would read the segment as unavailable. It is a corridor
// with more buildings than one page holds: a truncated page, whose
// samples stay unclassified, like any other truncated page. Its
// footprints are not read, since they would not be used. Counts that
// contradict the page are still refused.
func TestOverFullPageIsTruncated(t *testing.T) {
	over := strings.TrimSuffix(strings.Repeat(goodFeature+",", MaxFeatures+1), ",")
	for name, body := range map[string][]byte{
		"no counts":        fc(over, ``),
		"counts agree":     fc(over, fmt.Sprintf(`,"numberMatched":%d,"numberReturned":%d`, MaxFeatures+1, MaxFeatures+1)),
		"more matched yet": fc(over, fmt.Sprintf(`,"numberMatched":%d`, 5000)),
	} {
		page, err := Parse(body)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !page.Truncated || len(page.Footprints) != 0 || page.Skipped != 0 {
			t.Errorf("%s: truncated %v, %d footprints, %d skipped; want a truncated page without footprints",
				name, page.Truncated, len(page.Footprints), page.Skipped)
		}
	}
	for name, body := range map[string][]byte{
		"numberReturned wrong": fc(over, fmt.Sprintf(`,"numberReturned":%d`, MaxFeatures)),
		"numberMatched low":    fc(over, fmt.Sprintf(`,"numberMatched":%d`, MaxFeatures)),
	} {
		if _, err := Parse(body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// Through the client: a truncated page, not a failure.
	srv, _ := fakeOGC(t, answer(200, "application/json", fc(over, ``)))
	page, err := testClient(srv.URL).FetchSegment(context.Background(), dokk1Segment(t))
	if err != nil || !page.Truncated {
		t.Fatalf("FetchSegment: truncated %v, err %v; want a truncated page", page.Truncated, err)
	}
}
