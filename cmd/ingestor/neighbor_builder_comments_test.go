package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// #952 follow-up: #953 deleted the server's extractEdgesFromObs, so no
// comment may point at it, and the ingestor's resolution comments must name
// the server's live rule, buildFromStoreWithNodes.
func TestNoStaleExtractEdgesFromObsReference(t *testing.T) {
	out, err := exec.Command("git", "grep", "-n", "extractEdgesFromObs", "--", ":(top)").CombinedOutput()
	if err != nil && len(out) > 0 {
		t.Skipf("git grep unavailable: %v", err)
	}
	var hits []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" && !strings.Contains(l, "neighbor_builder_comments_test.go") {
			hits = append(hits, l)
		}
	}
	if len(hits) > 0 {
		t.Fatalf("stale extractEdgesFromObs references:\n%s", strings.Join(hits, "\n"))
	}
}

func TestNeighborBuilderCommentsPointAtLiveServerRule(t *testing.T) {
	src, err := os.ReadFile("neighbor_builder.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(src), "cmd/server/neighbor_graph.go buildFromStoreWithNodes"); n < 2 {
		t.Fatalf("want both resolution comments to cite cmd/server/neighbor_graph.go buildFromStoreWithNodes, found %d", n)
	}
}
