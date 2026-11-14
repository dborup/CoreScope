package profilecard

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// The goldens are compared byte for byte, and they are made on an arm64
// laptop and checked on amd64 CI. Go may fuse x*y+z into one FMA
// instruction on arm64 but not on amd64 (GOAMD64=v1), and a fused result
// can differ in the last bit — enough to move a pixel. So no float
// arithmetic in this package or its tests' fixtures may compile to a fused
// multiply-add on arm64; dhm helpers inlined here are covered too, since
// they compile as part of this package's functions. Products that meet an
// addition are wrapped in float64(...), which the spec says rounds.
//
// Not covered: dhm's own non-inlined functions (Link.Assess fuses on
// arm64). The fixtures use Assess only for the verdict numbers, which are
// rounded to centimetres before they reach the card, so a last-bit
// difference would have to land exactly on a rounding boundary to show.
//
// This cross-compiles for arm64 on any host and greps the assembly. It is
// also why glyph outlines are flattened here in integer arithmetic instead
// of by x/image/vector's QuadTo, whose lerp fuses on arm64.
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
		"-gcflags=github.com/meshcore-analyzer/profilecard=-S", ".")
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
