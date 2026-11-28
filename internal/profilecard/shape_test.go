package profilecard

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// responseShape lists every field reachable from the response type: owner,
// field, type and JSON tag, one per line, with package paths dropped. The
// same function lives in cmd/server/profilecard_shape_test.go; both sides
// compare against testdata/response_shape.txt, so a field added to one
// mirror and not the other fails a test on the side that moved.
var qualifier = regexp.MustCompile(`\b\w+\.`)

func responseShape(t reflect.Type) string {
	var lines []string
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct || seen[t] {
			return
		}
		seen[t] = true
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			typ := qualifier.ReplaceAllString(f.Type.String(), "")
			lines = append(lines, fmt.Sprintf("%s.%s %s embedded=%v json=%q", t.Name(), f.Name, typ, f.Anonymous, f.Tag.Get("json")))
			walk(f.Type)
		}
	}
	walk(t)
	return strings.Join(lines, "\n") + "\n"
}

func TestResponseShape(t *testing.T) {
	got := responseShape(reflect.TypeOf(TerrainProfileResponse{}))
	path := filepath.Join("testdata", "response_shape.txt")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("profilecard's response types drifted from %s:\n--- want\n%s--- got\n%s", path, want, got)
	}
}
