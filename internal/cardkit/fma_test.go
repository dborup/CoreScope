package cardkit

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// The cards' goldens are compared byte for byte, made on an arm64 laptop
// and checked on amd64 CI. Go may fuse x*y+z into one FMA instruction on
// arm64 but not on amd64 (GOAMD64=v1), and a fused result can differ in
// the last bit, enough to move a pixel. Every card draws through this
// package, so none of its float arithmetic may compile to a fused
// multiply-add on arm64. A card package's own check covers what is inlined
// into it; this one covers cardkit's functions as compiled here. Products
// that meet an addition are wrapped in float64(...), which the spec says
// rounds.
//
// This cross-compiles for arm64 on any host and greps the assembly. It is
// also why glyph outlines are flattened in integer arithmetic (text.go)
// instead of by x/image/vector's QuadTo, whose lerp fuses on arm64.
func TestNoFusedMultiplyAdd(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiles; skipped with -short")
	}
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not on PATH")
	}
	dir := t.TempDir()
	cmd := exec.Command(gobin, "test", "-c", "-o", dir+"/x.test",
		"-gcflags=github.com/meshcore-analyzer/cardkit=-S", ".")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cross-compile: %v\n%s", err, out)
	}
	fused := regexp.MustCompile(`\b(FMADD|FMSUB|FNMADD|FNMSUB)[DS]\b`)
	fn := regexp.MustCompile(`^"".*|^[\w./\-]+\.[\w.()*]+ STEXT`)
	var current string
	var hits []string
	for _, line := range strings.Split(string(out), "\n") {
		if fn.MatchString(line) {
			current = strings.Fields(line)[0]
		}
		if fused.MatchString(line) {
			hits = append(hits, current+": "+strings.TrimSpace(line))
		}
	}
	if !strings.Contains(string(out), "STEXT") {
		t.Fatal("no assembly listing produced; the check is not running")
	}
	if len(hits) > 0 {
		t.Errorf("fused multiply-add on arm64 (goldens would differ from amd64):\n%s", strings.Join(hits, "\n"))
	}
}
