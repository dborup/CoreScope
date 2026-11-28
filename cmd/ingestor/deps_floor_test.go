package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Minimum dependency versions that close the advisories govulncheck reports
// against the ingestor (upstream PR 2297 audit, #627). Lowering one reopens
// the advisory named next to it.
var depFloors = []struct {
	module, min, advisory string
}{
	{"github.com/eclipse/paho.mqtt.golang", "v1.5.1", "GO-2025-4173 / CVE-2025-10543"},
	{"golang.org/x/net", "v0.60.0", "GO-2026-6603, 6611, 6612, 6617 and GO-2025-3503"},
}

var semverRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)`)

func semverParts(t *testing.T, v string) [3]int {
	t.Helper()
	m := semverRe.FindStringSubmatch(v)
	if m == nil {
		t.Fatalf("not a semver: %q", v)
	}
	var out [3]int
	for i := range out {
		out[i], _ = strconv.Atoi(m[i+1])
	}
	return out
}

func semverLess(t *testing.T, a, b string) bool {
	pa, pb := semverParts(t, a), semverParts(t, b)
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

// requiredVersions maps module path to version from a go.mod's require lines.
func requiredVersions(t *testing.T, gomod string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(gomod)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "require "))
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		f := strings.Fields(line)
		if len(f) == 2 && semverRe.MatchString(f[1]) {
			out[f[0]] = f[1]
		}
	}
	return out
}

func TestIngestorDependencyFloors(t *testing.T) {
	got := requiredVersions(t, "go.mod")
	for _, d := range depFloors {
		v, ok := got[d.module]
		if !ok {
			t.Errorf("%s missing from go.mod", d.module)
			continue
		}
		if semverLess(t, v, d.min) {
			t.Errorf("%s %s is below %s (%s)", d.module, v, d.min, d.advisory)
		}
	}
}

// The runtime image and every other alpine reference must stay on a supported
// release (3.24+).
func TestRuntimeBaseImageNotBehind(t *testing.T) {
	tag := regexp.MustCompile(`(?m)^FROM\s+alpine:(\d+)\.(\d+)`)
	for _, f := range []string{"Dockerfile", "Dockerfile.go"} {
		b, err := os.ReadFile(filepath.Join("..", "..", f))
		if err != nil {
			t.Fatal(err)
		}
		m := tag.FindStringSubmatch(string(b))
		if m == nil {
			t.Errorf("%s: no alpine runtime FROM line", f)
			continue
		}
		maj, _ := strconv.Atoi(m[1])
		min, _ := strconv.Atoi(m[2])
		if maj < 3 || (maj == 3 && min < 24) {
			t.Errorf("%s: runtime base alpine:%s.%s is below 3.24 (out of support)", f, m[1], m[2])
		}
	}
	// Any other alpine reference (CI smoke steps, docs) must not lag either.
	stale := regexp.MustCompile(`alpine:3\.(\d+)`)
	for _, f := range []string{".github/workflows/deploy.yml"} {
		b, err := os.ReadFile(filepath.Join("..", "..", f))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range stale.FindAllStringSubmatch(string(b), -1) {
			if n, _ := strconv.Atoi(m[1]); n < 24 {
				t.Errorf("%s references %s (< 3.24)", f, m[0])
			}
		}
	}
}
