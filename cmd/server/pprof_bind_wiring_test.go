package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The pprof listener used to be resolved inline in main() with
// `":"+pprofPort`, which binds every interface and reads the shared
// PPROF_PORT that the ingestor reads too. #372 moved that decision into
// internal/pprofbind so the bind host is an explicit, testable setting and
// the two roles cannot collide on one port.
//
// main() starts a real server, so this asserts the wiring at the source
// level: that the resolver is the one in use and the inline form is gone.
func readServerMain(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	return string(data)
}

func TestServerResolvesPprofThroughPprofbind(t *testing.T) {
	src := readServerMain(t)
	if !strings.Contains(src, `"github.com/meshcore-analyzer/pprofbind"`) {
		t.Fatalf("cmd/server/main.go must import github.com/meshcore-analyzer/pprofbind")
	}
	resolve := regexp.MustCompile(`pprofbind\.Server\.Resolve\(\s*os\.Getenv\s*\)`)
	if !resolve.MatchString(src) {
		t.Fatalf("cmd/server/main.go must resolve pprof via pprofbind.Server.Resolve(os.Getenv)")
	}
	if strings.Contains(src, "pprofbind.Ingestor") {
		t.Fatalf("cmd/server/main.go must use the server role, not the ingestor role")
	}
}

func TestServerNoLongerBindsEveryInterfaceInline(t *testing.T) {
	src := readServerMain(t)
	for _, banned := range []string{
		`":"+pprofPort`,
		`":" + pprofPort`,
		`os.Getenv("PPROF_PORT")`,
		`os.Getenv("ENABLE_PPROF")`,
	} {
		if strings.Contains(src, banned) {
			t.Errorf("cmd/server/main.go still resolves pprof inline (%s); the bind address must come from pprofbind", banned)
		}
	}
}
