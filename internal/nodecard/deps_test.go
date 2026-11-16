package nodecard

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

// The node card spends no upstream budget (#811): it depends on the shared
// drawing primitives and the standard library only, never on dhm, copdem
// or the terrain card.
func TestDependencies(t *testing.T) {
	b, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	mod := string(b)
	if !regexp.MustCompile(`(?m)^go 1\.22$`).MatchString(mod) {
		t.Error("go directive must stay 1.22 (the server's)")
	}
	allowed := map[string]bool{"github.com/meshcore-analyzer/cardkit": true, "golang.org/x/image": true, "golang.org/x/text": true}
	req := regexp.MustCompile(`(?m)^\s*(?:require\s+)?([a-z0-9.\-]+\.[a-z]{2,}/[^\s]+) v[0-9]`)
	for _, m := range req.FindAllStringSubmatch(mod, -1) {
		if !allowed[m[1]] {
			t.Errorf("unexpected dependency %s", m[1])
		}
	}
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range af.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			if strings.Contains(p, ".") && p != "github.com/meshcore-analyzer/cardkit" {
				t.Errorf("%s imports %s: only cardkit and the standard library", f, p)
			}
			if p == "net/http" || p == "net" || p == "os" || p == "database/sql" {
				t.Errorf("%s imports %s: the renderer does no I/O", f, p)
			}
		}
	}
}
