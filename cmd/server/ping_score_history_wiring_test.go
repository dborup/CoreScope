package main

import (
	"os"
	"strings"
	"testing"
)

// mainGoSource reads main.go's own source once per call -- these are
// deliberate, narrow static/textual checks proving main.go's wiring
// shape (which callsite exists, and in what relative order), since
// main() itself is not a unit-testable function: it opens a real
// database, binds a real HTTP port, and blocks on os.Signal. There is no
// way to pin down "the OLD ping-scores callsite is gone" or "the
// explicit stop happens before dbClose in the signal handler" without
// either refactoring main() into smaller testable pieces (out of scope
// for fase 5G) or literally running the process end-to-end.
func mainGoSource(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("could not read main.go: %v", err)
	}
	return string(data)
}

func TestMainGo_NoLongerCallsOldPingScoresRecomputer(t *testing.T) {
	src := mainGoSource(t)
	if strings.Contains(src, "srv.StartPingScoresRecomputer(") {
		t.Error("main.go still calls srv.StartPingScoresRecomputer -- fase 5G must replace this callsite entirely; the old and new recomputers must never run at the same time")
	}
}

func TestMainGo_CallsStartPingScoreHistoryEngine(t *testing.T) {
	src := mainGoSource(t)
	if !strings.Contains(src, "StartPingScoreHistoryEngine(") {
		t.Error("main.go does not call StartPingScoreHistoryEngine")
	}
}

func TestMainGo_UsesRetentionBridge(t *testing.T) {
	src := mainGoSource(t)
	if !strings.Contains(src, "pingScoreHistoryRetentionDuration(cfg)") {
		t.Error("main.go does not derive retention duration via pingScoreHistoryRetentionDuration(cfg)")
	}
}

func TestMainGo_UsesDefaultConfigAndOverridesRetention(t *testing.T) {
	src := mainGoSource(t)
	if !strings.Contains(src, "defaultPingScoreHistoryEngineConfig()") {
		t.Error("main.go does not start from defaultPingScoreHistoryEngineConfig()")
	}
	if !strings.Contains(src, ".RetentionDuration = retentionDuration") {
		t.Error("main.go does not override RetentionDuration on the default engine config")
	}
}

func TestMainGo_HistoryPathDerivedFromResolvedDB(t *testing.T) {
	src := mainGoSource(t)
	if !strings.Contains(src, "DefaultPingScoreHistoryPath(resolvedDB)") {
		t.Error("main.go does not derive the history path from resolvedDB")
	}
}

// Fase 5H fix: StartPingScoreHistoryEngine's new mainDBPath argument must
// be resolvedDB itself, not a re-derived or hardcoded value -- that is
// what lets its synchronous path-collision guard
// (pingScoreHistoryPathsCollide) actually catch a misconfigured dbPath
// before any goroutine starts.
func TestMainGo_PassesResolvedDBAsMainDBPathToStartPingScoreHistoryEngine(t *testing.T) {
	src := mainGoSource(t)
	// Deliberately a literal, exact-formatting substring rather than
	// paren-matching: pingScoreHistoryProductionBackoff() (itself one of
	// the later arguments) contains its own "()" pair, which would
	// confuse a naive "find the first closing paren" scan.
	if !strings.Contains(src, "StartPingScoreHistoryEngine(\n\t\tsrv,\n\t\tresolvedDB,\n") {
		t.Error("main.go's StartPingScoreHistoryEngine call does not pass resolvedDB as the argument right after srv (mainDBPath)")
	}
}

func TestMainGo_UsesProductionBackoff(t *testing.T) {
	src := mainGoSource(t)
	if !strings.Contains(src, "pingScoreHistoryProductionBackoff()") {
		t.Error("main.go does not pass pingScoreHistoryProductionBackoff() to StartPingScoreHistoryEngine")
	}
}

func TestMainGo_DeclaresFallbackDeferForStopPingScoreHistory(t *testing.T) {
	src := mainGoSource(t)
	if !strings.Contains(src, "defer stopPingScoreHistory()") {
		t.Error("main.go must keep `defer stopPingScoreHistory()` as a fallback for non-signal-based exits")
	}
}

// ShutdownHandlerStopsHistoryBeforeDBClose is the key ordering guarantee
// from fase 5G's shutdown-race analysis: the signal-handling goroutine's
// OWN explicit dbClose() call has no happens-before relationship with
// main()'s defer-unwind on the OTHER goroutine (httpServer.ListenAndServe()
// returns as soon as Shutdown() closes the listener, well before
// Shutdown()'s own graceful drain -- let alone the rest of this
// goroutine's steps -- has finished). Only an explicit, same-goroutine,
// program-order call to stopPingScoreHistory() BEFORE this goroutine's
// own dbClose() call actually guarantees the required
// stop → active Cycle finishes → history store closes → main DB closes
// ordering on this shutdown path.
func TestMainGo_ShutdownHandlerStopsHistoryBeforeDBClose(t *testing.T) {
	src := mainGoSource(t)

	goroutineStart := strings.Index(src, "signal.Notify(sigCh")
	if goroutineStart == -1 {
		t.Fatal("could not locate the signal-handling goroutine in main.go")
	}
	body := src[goroutineStart:]

	explicitStop := strings.Index(body, "\n\t\tstopPingScoreHistory()\n")
	explicitDBClose := strings.Index(body, "\n\t\tif err := dbClose(); err != nil {")

	if explicitStop == -1 {
		t.Fatal("no explicit, non-deferred stopPingScoreHistory() call found inside the signal-handling goroutine")
	}
	if explicitDBClose == -1 {
		t.Fatal("no explicit, non-deferred dbClose() call found inside the signal-handling goroutine")
	}
	if explicitStop >= explicitDBClose {
		t.Errorf("stopPingScoreHistory() (offset %d within the goroutine) must come before dbClose() (offset %d) inside the signal-handling goroutine", explicitStop, explicitDBClose)
	}
}
