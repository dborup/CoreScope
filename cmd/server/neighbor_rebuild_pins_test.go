package main

import (
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Follow-ups from #550 (issue #553).

// 1a: the tick decision, table-tested. Kills `every > 1 && tick%every == 0`,
// which never rebuilds at the default cadence (every == 1).
func TestShouldRebuildNeighborGraph(t *testing.T) {
	cases := []struct {
		tick, every int
		want        bool
	}{
		{1, 1, true}, // default 300 s cadence: every tick rebuilds
		{2, 1, true},
		{100, 1, true},
		{1, 3, false},
		{2, 3, false},
		{3, 3, true},
		{4, 3, false},
		{6, 3, true},
		{30, 30, true},
		{29, 30, false},
		{1, 0, true}, // defensive: a non-positive divisor rebuilds, never panics
		{5, -2, true},
	}
	for _, c := range cases {
		if got := shouldRebuildNeighborGraph(c.tick, c.every); got != c.want {
			t.Errorf("shouldRebuildNeighborGraph(%d, %d) = %v, want %v", c.tick, c.every, got, c.want)
		}
	}
}

// 1b: the loop itself consults the decision once per tick with the right
// tick number. Driven by a fake tick channel and a recording step.
func TestRunNeighborGraphLoopRebuildPattern(t *testing.T) {
	for _, c := range []struct {
		every int
		want  []bool
	}{
		{1, []bool{true, true, true, true}},
		{3, []bool{false, false, true, false, false, true}},
	} {
		ticks := make(chan time.Time)
		stop := make(chan struct{})
		got := make(chan bool, len(c.want))
		done := make(chan struct{})
		go func() {
			defer close(done)
			runNeighborGraphLoop(ticks, stop, c.every, func(rebuild bool) { got <- rebuild })
		}()
		for range c.want {
			ticks <- time.Now()
		}
		close(stop)
		<-done
		close(got)
		var seq []bool
		for r := range got {
			seq = append(seq, r)
		}
		if len(seq) != len(c.want) {
			t.Fatalf("every=%d: %d steps, want %d", c.every, len(seq), len(c.want))
		}
		for i := range seq {
			if seq[i] != c.want[i] {
				t.Errorf("every=%d: step %d rebuild=%v, want %v (seq %v)", c.every, i+1, seq[i], c.want[i], seq)
			}
		}
	}
}

// 2: publish ordering. Builds are serialized by neighborBuildMu, so a build
// that started earlier can never publish after one that started later.
// Without the lock the second build overtakes the blocked first one and the
// older graph overwrites the newer (lost publish).
func TestNeighborRebuildPublishOrdering(t *testing.T) {
	older, newer := NewNeighborGraph(), NewNeighborGraph()
	var inFlight, maxInFlight, calls atomic.Int32
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	srv := &Server{store: followupStore()}
	srv.buildGraphFn = func() *NeighborGraph {
		n := calls.Add(1)
		cur := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if cur <= m || maxInFlight.CompareAndSwap(m, cur) {
				break
			}
		}
		entered <- struct{}{}
		defer inFlight.Add(-1)
		if n == 1 {
			<-release // the older build is slow
			return older
		}
		return newer
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); srv.rebuildNeighborGraph() }()
	<-entered // first build is now running and blocked
	go func() { defer wg.Done(); srv.rebuildNeighborGraph() }()
	select {
	case <-entered:
		t.Fatal("second build started while the first was still running: builds are not serialized")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	wg.Wait()

	if got := maxInFlight.Load(); got != 1 {
		t.Errorf("max concurrent builds = %d, want 1", got)
	}
	if srv.neighborGraph.Load() != newer {
		t.Error("published graph is not the most recently started build (lost publish)")
	}
}

// 2: the initial build in getNeighborGraph shares the same lock, so a
// concurrent rebuild cannot be overwritten by a slower initial build.
func TestNeighborInitialBuildCannotOverwriteRebuild(t *testing.T) {
	initial, rebuilt := NewNeighborGraph(), NewNeighborGraph()
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	var calls atomic.Int32
	srv := &Server{store: followupStore()}
	srv.buildGraphFn = func() *NeighborGraph {
		n := calls.Add(1)
		entered <- struct{}{}
		if n == 1 {
			<-release
			return initial
		}
		return rebuilt
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); srv.getNeighborGraph() }()
	<-entered
	go func() { defer wg.Done(); srv.rebuildNeighborGraph() }()
	select {
	case <-entered:
		t.Fatal("rebuild started while the initial build was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	if srv.neighborGraph.Load() != rebuilt {
		t.Error("initial build overwrote the newer rebuild")
	}
}

// 3: the interval docs state the ceil() bound, not "as old as the interval".
func TestNeighborCacheIntervalDocsStateAgeBound(t *testing.T) {
	for _, path := range []string{"config.go", "../../config.example.json"} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if !strings.Contains(s, "one TTL plus one interval") {
			t.Errorf("%s: missing the %q age bound", path, "one TTL plus one interval")
		}
		if strings.Contains(s, "as old as the interval") {
			t.Errorf("%s: still claims the graph is \"as old as the interval\"", path)
		}
	}
}

// 4 (F4 of #545, strengthened): the issue refs must sit in the same comment as the setting, not merely
// somewhere in the same file.
func TestNeighborCacheIntervalDocsKeepIssueRefs(t *testing.T) {
	cfg, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatal(err)
	}
	if block := goCommentBlockAbove(string(cfg), "CacheRecomputeIntervalSeconds int"); !refsIn(block) {
		t.Errorf("config.go: comment above CacheRecomputeIntervalSeconds lacks #1481 P0-1 / #1483:\n%s", block)
	}
	ex, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if val := jsonCommentValue(string(ex), "neighborGraph"); !strings.Contains(val, "cacheRecomputeIntervalSeconds") || !refsIn(val) {
		t.Errorf("config.example.json: the neighborGraph _comment must mention cacheRecomputeIntervalSeconds with its refs, got %q", val)
	}
}

// Self-test of the helpers: a ref in another comment must not count.
func TestNeighborDocHelpersRejectRefsElsewhere(t *testing.T) {
	src := "// #1481 P0-1 #1483 other\nOther int\n\n// unrelated\nCacheRecomputeIntervalSeconds int\n"
	if refsIn(goCommentBlockAbove(src, "CacheRecomputeIntervalSeconds int")) {
		t.Error("refs from a different comment were attributed to the setting")
	}
	j := `{"neighborGraph": {"a": 1, "_comment": "no mention"}, "x": {"_comment": "cacheRecomputeIntervalSeconds #1481 P0-1 #1483"}}`
	if v := jsonCommentValue(j, "neighborGraph"); strings.Contains(v, "cacheRecomputeIntervalSeconds") {
		t.Errorf("comment of another object leaked: %q", v)
	}
}

func refsIn(s string) bool {
	return strings.Contains(s, "#1481 P0-1") && strings.Contains(s, "#1483")
}

// goCommentBlockAbove returns the contiguous // comment lines directly above
// the first line containing needle.
func goCommentBlockAbove(src, needle string) string {
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		if !strings.Contains(l, needle) {
			continue
		}
		j := i
		for j > 0 && strings.HasPrefix(strings.TrimSpace(lines[j-1]), "//") {
			j--
		}
		return strings.Join(lines[j:i], "\n")
	}
	return ""
}

// jsonCommentValue returns the "_comment" string of the named top-level object.
func jsonCommentValue(src, object string) string {
	i := strings.Index(src, `"`+object+`"`)
	if i < 0 {
		return ""
	}
	end := strings.Index(src[i:], "}")
	if end < 0 {
		return ""
	}
	obj := src[i : i+end]
	k := strings.Index(obj, `"_comment"`)
	if k < 0 {
		return ""
	}
	return obj[k:]
}
