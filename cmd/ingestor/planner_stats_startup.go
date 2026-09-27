package main

import (
	"context"
	"log"
	"sync"
	"time"
)

// Startup ordering of the one-off single-writer holds (issue #100, the fork
// adaptation of `Kpa-clawbot/CoreScope#2072`/`#2074`).
//
// Two startup steps hold the store's single write connection (writerMu,
// SetMaxOpenConns(1)) for one long statement each:
//
//   - EnsurePlannerStats: ANALYZE on a database that has never been analyzed.
//     Once per database; 3m43.9s cold on upstream's 9.4 GB staging file.
//   - the #89 route_mask backfill: CREATE INDEX of its pending index (~20 s on
//     a staging-sized DB), then short batches with a yield between them.
//
// While either runs, IngestBuffer's consumer is blocked and packets queue.
// Started side by side, the two holds would run back to back (writerMu is
// FIFO once contended) and the ingest pause would be their sum; started the
// moment the buffer opens, either would also sit on top of the backlog the
// buffer collected during the startup migrations. So they run in sequence on
// one goroutine, each behind a drain of the buffer:
//
//	drain → EnsurePlannerStats → drain → StartRouteMaskBackfill
//
// ANALYZE goes first because it is what the read path is waiting for (the
// channel queries run on the wrong index until sqlite_stat1 exists), while the
// backfill is background repair whose batch phase alone runs for minutes on a
// staging-sized table. On a restart EnsurePlannerStats is one sqlite_master
// query, so the backfill starts after nothing more than the first drain.
//
// The routine refresh (schedulePlannerStatsRefresh) starts its stagger only
// after this sequence hands over, so it cannot land on the startup ANALYZE.
// The other startup tickers (WAL checkpoint +30s, observer retention +90s,
// multibyte persist +2m, neighbor prune +4m) are left where they were: each is
// a short write that queues behind whichever hold is running and then yields
// writerMu back, so they add seconds, not a second long hold.

// plannerStatsDrainTimeout bounds each drain wait: if ingest outpaces the
// writer the buffer may never read empty, and the next step must still run.
// Vars so tests can shrink them.
var (
	plannerStatsDrainTimeout = 2 * time.Minute
	plannerStatsDrainPoll    = 250 * time.Millisecond
)

// startPlannerStatsThenRouteMaskBackfill runs the sequence above on its own
// goroutine and returns a channel closed once the backfill has been scheduled
// (or skipped because ctx was cancelled). pending reports the ingest backlog;
// main passes IngestBuffer.Pending.
func (s *Store) startPlannerStatsThenRouteMaskBackfill(ctx context.Context, analysisLimit int, pending func() int) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		waitForIngestDrain(ctx, pending, "planner statistics")
		if ctx.Err() != nil {
			return
		}
		s.EnsurePlannerStats(analysisLimit)
		waitForIngestDrain(ctx, pending, "route_mask backfill")
		if ctx.Err() != nil {
			return
		}
		s.StartRouteMaskBackfill(ctx)
	}()
	return done
}

// waitForIngestDrain blocks until pending() reads zero, ctx is cancelled, or
// plannerStatsDrainTimeout passes.
func waitForIngestDrain(ctx context.Context, pending func() int, next string) {
	deadline := time.Now().Add(plannerStatsDrainTimeout)
	for pending() > 0 {
		if time.Now().After(deadline) {
			log.Printf("[startup] ingest buffer still has %d pending after %v; starting %s anyway", pending(), plannerStatsDrainTimeout, next)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(plannerStatsDrainPoll):
		}
	}
}

// schedulePlannerStatsRefresh starts the routine refresh (#2058): once after
// the stagger, then every interval. The stagger starts when after closes (the
// startup sequence above has handed over). A negative analysisLimit disables
// it and reports enabled=false. stop ends the ticker.
func (s *Store) schedulePlannerStatsRefresh(analysisLimit int, after <-chan struct{}, stagger, interval time.Duration) (stop func(), enabled bool) {
	if analysisLimit < 0 {
		return func() {}, false
	}
	quit := make(chan struct{})
	go func() {
		select {
		case <-after:
		case <-quit:
			return
		}
		select {
		case <-time.After(stagger):
		case <-quit:
			return
		}
		s.RefreshPlannerStats(analysisLimit)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.RefreshPlannerStats(analysisLimit)
			case <-quit:
				return
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(quit) }) }, true
}
