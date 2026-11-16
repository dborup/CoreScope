package profilecard

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The plan approved exactly one new third-party dependency, pinned: see the
// comment in go.mod for why v0.23.0. golang.org/x/text comes with it
// (font/sfnt reads Mac-encoded name tables) and golang.org/x/sync with the
// shared internal/dhm module; since #811 M1 the drawing primitives come
// from internal/cardkit, which carries the same pin. Nothing else may appear.
func TestDependencyPin(t *testing.T) {
	b, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	mod := string(b)
	if !regexp.MustCompile(`(?m)^go 1\.22$`).MatchString(mod) {
		t.Error("go directive must stay 1.22 (the server's); a newer x/image would raise it")
	}
	if !regexp.MustCompile(`(?m)^\s*golang\.org/x/image v0\.23\.0\s*(//.*)?$`).MatchString(mod) &&
		!regexp.MustCompile(`(?m)^require golang\.org/x/image v0\.23\.0\b`).MatchString(mod) {
		t.Error("golang.org/x/image must be pinned at v0.23.0")
	}
	if !strings.Contains(mod, "v0.23.0 keeps") {
		t.Error("the x/image pin must carry its reason as a comment")
	}
	allowed := map[string]bool{
		"golang.org/x/image": true, "golang.org/x/text": true, "golang.org/x/sync": true,
		"github.com/meshcore-analyzer/dhm": true, "github.com/meshcore-analyzer/copdem": true,
		"github.com/meshcore-analyzer/cardkit": true,
	}
	req := regexp.MustCompile(`(?m)^\s*(?:require\s+)?([a-z0-9.\-]+\.[a-z]{2,}/[^\s]+) v[0-9]`)
	for _, m := range req.FindAllStringSubmatch(mod, -1) {
		if !allowed[m[1]] {
			t.Errorf("unexpected dependency %s", m[1])
		}
	}
}
