package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Mirror of cmd/server/pprof_bind_wiring_test.go for the ingestor role.
// See internal/pprofbind for why the bind host became an explicit setting.
func readIngestorMain(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	return string(data)
}

func TestIngestorResolvesPprofThroughPprofbind(t *testing.T) {
	src := readIngestorMain(t)
	if !strings.Contains(src, `"github.com/meshcore-analyzer/pprofbind"`) {
		t.Fatalf("cmd/ingestor/main.go must import github.com/meshcore-analyzer/pprofbind")
	}
	resolve := regexp.MustCompile(`pprofbind\.Ingestor\.Resolve\(\s*os\.Getenv\s*\)`)
	if !resolve.MatchString(src) {
		t.Fatalf("cmd/ingestor/main.go must resolve pprof via pprofbind.Ingestor.Resolve(os.Getenv)")
	}
	if strings.Contains(src, "pprofbind.Server") {
		t.Fatalf("cmd/ingestor/main.go must use the ingestor role, not the server role")
	}
}

func TestIngestorNoLongerBindsEveryInterfaceInline(t *testing.T) {
	src := readIngestorMain(t)
	for _, banned := range []string{
		`":"+pprofPort`,
		`":" + pprofPort`,
		`os.Getenv("PPROF_PORT")`,
		`os.Getenv("ENABLE_PPROF")`,
	} {
		if strings.Contains(src, banned) {
			t.Errorf("cmd/ingestor/main.go still resolves pprof inline (%s); the bind address must come from pprofbind", banned)
		}
	}
}
