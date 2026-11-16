package cardkit

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// cardkit is the drawing layer only (#811 M1): the one approved
// third-party dependency (golang.org/x/image, pinned; x/text comes with
// it) and the standard library. No data source, no other card package:
// a card package imports cardkit, never the other way round.
func TestDependencyPin(t *testing.T) {
	b, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	mod := string(b)
	if !regexp.MustCompile(`(?m)^go 1\.22$`).MatchString(mod) {
		t.Error("go directive must stay 1.22 (the server's); a newer x/image would raise it")
	}
	if !regexp.MustCompile(`(?m)^\s*golang\.org/x/image v0\.23\.0\s*(//.*)?$`).MatchString(mod) {
		t.Error("golang.org/x/image must be pinned at v0.23.0")
	}
	if !strings.Contains(mod, "v0.23.0 keeps") {
		t.Error("the x/image pin must carry its reason as a comment")
	}
	allowed := map[string]bool{"golang.org/x/image": true, "golang.org/x/text": true}
	req := regexp.MustCompile(`(?m)^\s*(?:require\s+)?([a-z0-9.\-]+\.[a-z]{2,}/[^\s]+) v[0-9]`)
	for _, m := range req.FindAllStringSubmatch(mod, -1) {
		if !allowed[m[1]] {
			t.Errorf("unexpected dependency %s", m[1])
		}
	}
	if strings.Contains(mod, "replace ") {
		t.Error("cardkit must not depend on another local module")
	}
}

// The import graph, from the source: every non-test file imports only the
// standard library and golang.org/x/{image,text}.
func TestImportsStayDrawingOnly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, im := range af.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			first := strings.SplitN(p, "/", 2)[0]
			if !strings.Contains(first, ".") {
				continue // standard library
			}
			if !strings.HasPrefix(p, "golang.org/x/image/") && !strings.HasPrefix(p, "golang.org/x/text/") {
				t.Errorf("%s imports %s", f, p)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no source files checked")
	}
}
