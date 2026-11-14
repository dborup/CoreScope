package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// #727 M2: internal/profilecard mirrors TerrainProfileResponse field for
// field so M3 can alias it. Both sides compare their reflection of the
// types against one file; this side fails when the server's types move
// without the mirror. responseShapeOf is the twin of profilecard's
// responseShape (shape_test.go); this module cannot import that package's
// tests.
var profileShapeQualifier = regexp.MustCompile(`\b\w+\.`)

func responseShapeOf(t reflect.Type) string {
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
			typ := profileShapeQualifier.ReplaceAllString(f.Type.String(), "")
			lines = append(lines, fmt.Sprintf("%s.%s %s embedded=%v json=%q", t.Name(), f.Name, typ, f.Anonymous, f.Tag.Get("json")))
			walk(f.Type)
		}
	}
	walk(t)
	return strings.Join(lines, "\n") + "\n"
}

func TestTerrainProfileResponseMatchesProfileCard(t *testing.T) {
	path := filepath.Join("..", "..", "internal", "profilecard", "testdata", "response_shape.txt")
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := responseShapeOf(reflect.TypeOf(TerrainProfileResponse{})); got != string(want) {
		t.Errorf("TerrainProfileResponse no longer matches internal/profilecard's mirror (%s); change both:\n--- want\n%s--- got\n%s", path, want, got)
	}
}
