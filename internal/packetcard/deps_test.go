package packetcard

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

// The packet card spends no upstream budget (#812): it depends on the shared
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

// Every drawing primitive is cardkit's (#812: reuse, no duplicates): the
// renderer never touches pixels itself, so it has no image/draw, no Pix,
// no Set and no Blend of its own.
func TestDrawsOnlyThroughCardkit(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	pixel := regexp.MustCompile(`\.(Pix|Set|SetRGBA|SetNRGBA|PixOffset)\b|\bfunc \([^)]*\) (Blend|Disc|Line|Polyline|FillRect)\(`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), `"image/draw"`) {
			t.Errorf("%s imports image/draw: draw through cardkit", f)
		}
		for _, m := range pixel.FindAllString(string(b), -1) {
			t.Errorf("%s: %s: draw through cardkit", f, m)
		}
	}
}
