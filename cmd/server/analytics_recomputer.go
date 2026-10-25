// Package main: analytics recomputer (issue #1240).
//
// Steady-state background recompute loop for expensive analytics
// endpoints. Reads always hit an atomic-pointer cache; compute runs
// on a fixed ticker in a goroutine. This eliminates the on-request
// compute-then-cache pattern where the first reader after expiry pays
// the full compute cost and blocks under writer contention.
//
// See issue #1240 and AGENTS.md "Performance is a feature".
package main

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// analyticsRecomputer holds the latest snapshot of an analytics result
// in an atomic.Value, refreshed periodically by a background goroutine.
//
// Lifecycle:
//   1. Construct via newAnalyticsRecomputer(...)
//   2. Call Start() — runs initial compute synchronously, then launches
//      the recompute goroutine. Initial compute is synchronous so the
//      first Load() after Start returns never sees a nil cache.
//   3. Call Load() any number of times concurrently — never blocks
//      beyond an atomic-pointer load.
//   4. Call Stop() to terminate the background goroutine cleanly.
//
// Compute func is called WITHOUT any lock held by this struct, so it
// may freely take any application-level locks it needs.
type analyticsRecomputer struct {
	name     string
	interval time.Duration
	compute  func() interface{}

	cache atomic.Value // holds *recomputerSnapshot — the latest snapshot
	stop  chan struct{}
	done  chan struct{}
	// recomputeReq carries RecomputeNow requests to the loop goroutine,
	// which closes the inner channel when that pass is done (#116).
	recomputeReq chan chan struct{}

	startOnce sync.Once
	stopOnce  sync.Once

	// Issue #492 — opt-in pauseWhenIdle (analytics.pauseWhenIdle). Set
	// before Start. A tick with no Load since the previous pass is skipped
	// once the recomputer has settled; the next Load serves the existing
	// snapshot and queues one wake-up on kick. See pauseTick / markRead.
	pauseWhenIdle bool
	// deps are recomputers whose snapshot this one reads while computing
	// (roles reads nodes-clock-skew). A paused dependency is refreshed
	// before this one computes, so it never computes over a snapshot
	// that stopped refreshing.
	deps []*analyticsRecomputer
	// pauseReadyGate: pausing is allowed only after a successful pass that
	// STARTED with this gate open (the startup load has terminated). nil
	// means no gate.
	pauseReadyGate   func() bool
	settled          atomic.Bool // a successful pass started with pauseReadyGate open
	lastPassOK       atomic.Bool // the last pass returned a snapshot without panicking
	readSinceCompute atomic.Bool
	paused           atomic.Bool
	// missedRefresh: the current snapshot outlived a scheduled refresh
	// that was skipped. Cleared only when a new snapshot is stored, so a
	// stale snapshot is never reported fresh while a wake pass runs or
	// after one fails.
	missedRefresh atomic.Bool
	kick          chan struct{} // buffered(1): coalesced wake-ups

	newTicker func(name string, d time.Duration) recomputeTicker
	now       func() time.Time
	// Test hooks; nil in production.
	onLoopEvent         func(event string)
	testHookPauseWindow func()

	// Stats (atomic).
	computeRuns   atomic.Int64
	lastComputeNs atomic.Int64 // duration of last compute in nanoseconds
	lastStartNs   atomic.Int64 // wall clock when the last compute started (#116)

	// Issue #1659 (PR #1688 r1) — warmup gate state, inlined here so
	// hot-path readers (IsWarmingUp_1659) do lock-free atomic loads
	// only (replaces the r0 package-level map + chanLock). See
	// analytics_warmup_1659.go for full design notes.
	firstPassDoneNs atomic.Int64
	warmupStartedNs atomic.Int64
	warmupReadyGate atomic.Value // *func() bool — gate must return true for markFirstPassDone to take effect
}

// newAnalyticsRecomputer constructs an unstarted recomputer.
// interval must be > 0; compute must be non-nil.
func newAnalyticsRecomputer(name string, interval time.Duration, compute func() interface{}) *analyticsRecomputer {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &analyticsRecomputer{
		name:     name,
		interval: interval,
		compute:  compute,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),

		recomputeReq: make(chan chan struct{}),
		kick:         make(chan struct{}, 1),
		newTicker:    newRecomputeTicker,
		now:          time.Now,
	}
}

// recomputerSnapshot is one stored compute result and when it was stored.
type recomputerSnapshot struct {
	v  interface{}
	at time.Time
}

// recomputeTicker is the loop's schedule; tests inject a manual one.
type recomputeTicker interface {
	C() <-chan time.Time
	Reset(d time.Duration)
	Stop()
}

type timeRecomputeTicker struct{ *time.Ticker }

func (t timeRecomputeTicker) C() <-chan time.Time { return t.Ticker.C }

var newRecomputeTicker = func(_ string, d time.Duration) recomputeTicker {
	return timeRecomputeTicker{time.NewTicker(d)}
}

func (r *analyticsRecomputer) event(ev string) {
	if r.onLoopEvent != nil {
		r.onLoopEvent(ev)
	}
}

// Start runs the initial compute synchronously (so the first Load
// after Start returns a populated snapshot, never nil), then launches
// a background goroutine to periodically recompute.
//
// Calling Start multiple times is a no-op after the first call.
func (r *analyticsRecomputer) Start() {
	r.startOnce.Do(func() {
		// Issue #1659 (#1688 munger #2): record warmup-start before
		// the first compute, so IsWarmingUp_1659's fallback timeout
		// is measured from "recomputer started" — not "first pass
		// returned", which never happens if compute() hangs.
		r.noteWarmupStart_1659()
		// Initial synchronous compute — first read must NOT see empty
		// or uninitialized data (acceptance criterion #1240).
		r.runOnce()
		go r.loop()
	})
}

func (r *analyticsRecomputer) loop() {
	defer close(r.done)
	t := r.newTicker(r.name, r.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C():
			if r.pauseTick() {
				r.event("tick-skip")
				continue
			}
			r.refreshPausedDeps()
			r.runOnce()
			r.event("tick-run")
		case <-r.kick:
			// #492: a paused recomputer was read. A wake-up that arrives
			// after a pass already ran (the burst raced it) is dropped,
			// so a burst costs at most one pass.
			if !r.paused.Load() {
				r.event("kick-ignored")
				continue
			}
			r.refreshPausedDeps()
			r.runOnce()
			t.Reset(r.interval)
			r.event("kick-run")
		case ack := <-r.recomputeReq:
			// #116: on-demand pass, on this goroutine so it never runs
			// concurrently with a periodic one; restart the ticker so no
			// periodic pass follows right behind it. Runs whether or not
			// the recomputer is paused (startup load, distance index).
			r.runOnce()
			t.Reset(r.interval)
			close(ack)
			r.event("recompute-now")
		case <-r.stop:
			return
		}
	}
}

// pauseTick decides whether a scheduled pass is skipped (#492): only with
// pauseWhenIdle on, only once a successful pass started after the startup
// load, never while the last pass failed, and only when nothing read the
// snapshot since the previous pass.
//
// paused is published BEFORE readSinceCompute is checked, and markRead
// stores readSinceCompute BEFORE checking paused, so a read racing this
// tick is either seen here (the tick computes) or sees paused (it queues a
// wake-up): it is never lost.
func (r *analyticsRecomputer) pauseTick() bool {
	if !r.pauseWhenIdle || !r.settled.Load() || !r.lastPassOK.Load() {
		return false
	}
	r.paused.Store(true)
	if r.testHookPauseWindow != nil {
		r.testHookPauseWindow()
	}
	if r.readSinceCompute.Load() {
		return false
	}
	r.missedRefresh.Store(true)
	return true
}

// refreshPausedDeps runs one pass of every paused dependency before this
// recomputer computes over its snapshot. Gives up on either side stopping.
func (r *analyticsRecomputer) refreshPausedDeps() {
	for _, d := range r.deps {
		if d == nil || !d.paused.Load() {
			continue
		}
		ack := make(chan struct{})
		select {
		case d.recomputeReq <- ack:
		case <-d.stop:
			continue
		case <-r.stop:
			return
		}
		select {
		case <-ack:
		case <-d.stop:
		case <-r.stop:
			return
		}
	}
}

func (r *analyticsRecomputer) runOnce() {
	if r.compute == nil {
		return
	}
	ok := false
	defer func() {
		// Don't let a compute panic kill the background goroutine.
		// The previous snapshot remains valid. Even on panic, we
		// still want IsWarmingUp_1659's fallback timeout to be the
		// safety net (a perpetually panicking compute would never
		// reach markFirstPassDone otherwise).
		_ = recover()
		// #492: a failed pass (panic or nil) keeps the recomputer from
		// pausing, so the next tick retries.
		r.lastPassOK.Store(ok)
	}()
	// #116: sample the warm-up readiness gate BEFORE computing, so a
	// pass that started on a partially loaded store never ends the
	// warm-up, even if the load finishes while it runs.
	ready := r.warmupReadyGateOpen_1659()
	// #492: same rule for pausing: only a pass that started after the
	// startup load may settle the recomputer.
	pauseReady := r.pauseReadyGate == nil || r.pauseReadyGate()
	// #492: reads from here on count towards the next tick.
	r.readSinceCompute.Store(false)
	r.paused.Store(false)
	t0 := time.Now()
	r.lastStartNs.Store(t0.UnixNano())
	result := r.compute()
	r.lastComputeNs.Store(int64(time.Since(t0)))
	r.computeRuns.Add(1)
	if result != nil {
		r.cache.Store(&recomputerSnapshot{v: result, at: r.now()})
		r.missedRefresh.Store(false)
		ok = true
		if pauseReady {
			r.settled.Store(true)
		}
	}
	// Issue #1659: mark the first-pass clock so the warmup gate
	// in GetAnalyticsRFWithWindow / Topology / Channels handlers
	// can flip from 503-Retry-After to serving the cache.
	//
	// PR #1688 r1: called on EVERY successful pass (even nil
	// result) so a compute that returns nil but doesn't panic
	// still lifts the gate — banner-stuck-forever fix (munger #2).
	// The markFirstPassDone helper is idempotent; it only runs for a
	// pass that started with the readiness gate open (munger #5, #116).
	if ready {
		r.markFirstPassDone_1659()
	}
}

// RecomputeNow has the loop goroutine run a pass that starts after this
// call and waits for it to finish; the periodic ticker restarts from
// that pass (#116). Returns early once the recomputer is stopped; blocks
// until Start if it has not started yet.
func (r *analyticsRecomputer) RecomputeNow() {
	ack := make(chan struct{})
	select {
	case r.recomputeReq <- ack:
	case <-r.stop:
		return
	}
	select {
	case <-ack:
	case <-r.stop:
	}
}

// LastStartAt returns when the most recent compute started (zero before
// the first one).
func (r *analyticsRecomputer) LastStartAt() time.Time {
	ns := r.lastStartNs.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// recomputeWhenLoaded waits for loaded to close, then recomputes each
// recomputer once, one at a time, in slice order (#116). Sequential so
// the post-load passes do not all hold the store read lock at once, and
// so a recomputer that reads another's snapshot can follow it. Returns
// when done or when stop closes.
func recomputeWhenLoaded(loaded, stop <-chan struct{}, rcs []*analyticsRecomputer) {
	select {
	case <-loaded:
	case <-stop:
		return
	}
	t0 := time.Now()
	parts := make([]string, 0, len(rcs))
	for _, rc := range rcs {
		select {
		case <-stop:
			return
		default:
		}
		rc.RecomputeNow()
		parts = append(parts, fmt.Sprintf("%s=%s", rc.name, rc.LastComputeDuration().Round(time.Millisecond)))
	}
	log.Printf("[analytics-recompute] startup load done: recomputed %d snapshots in %s (%s)",
		len(rcs), time.Since(t0).Round(time.Millisecond), strings.Join(parts, " "))
}

// markRead counts a read for pauseWhenIdle (#492): the next tick
// computes, and a paused recomputer gets one non-blocking wake-up (a
// wake-up already queued absorbs the rest of a burst). A no-op when
// pauseWhenIdle is off. See pauseTick for the ordering.
func (r *analyticsRecomputer) markRead() {
	if !r.pauseWhenIdle {
		return
	}
	r.readSinceCompute.Store(true)
	if r.paused.Load() {
		select {
		case r.kick <- struct{}{}:
		default:
		}
	}
}

// Load returns the most recently computed snapshot, or nil if Start
// has not been called (or the very first compute returned nil).
// Never blocks: beyond the atomic load, a paused recomputer
// (pauseWhenIdle) gets a non-blocking wake-up and the snapshot it already
// holds is returned while the refresh runs.
func (r *analyticsRecomputer) Load() interface{} {
	r.markRead()
	snap, _ := r.cache.Load().(*recomputerSnapshot)
	if snap == nil {
		return nil
	}
	return snap.v
}

// SnapshotInfo reports when the current snapshot was stored and whether it
// is stale: it outlived a scheduled refresh that pauseWhenIdle skipped and
// no newer snapshot has been stored yet (#492). It is not a read. Always
// fresh with pauseWhenIdle off.
func (r *analyticsRecomputer) SnapshotInfo() (at time.Time, stale bool) {
	if snap, _ := r.cache.Load().(*recomputerSnapshot); snap != nil {
		at = snap.at
	}
	return at, r.missedRefresh.Load()
}

// Stop signals the background goroutine to exit and waits for it.
// Safe to call multiple times. Safe to call before Start (no-op).
func (r *analyticsRecomputer) Stop() {
	r.stopOnce.Do(func() {
		close(r.stop)
	})
	// Only wait if the goroutine was actually started.
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		// Defensive timeout: shouldn't happen in practice.
	}
}

// LastComputeDuration returns the duration of the most recent compute.
func (r *analyticsRecomputer) LastComputeDuration() time.Duration {
	return time.Duration(r.lastComputeNs.Load())
}

// ComputeRuns returns the total number of compute invocations.
func (r *analyticsRecomputer) ComputeRuns() int64 {
	return r.computeRuns.Load()
}

// AnalyticsRecomputeIntervals lets callers (main.go) override the
// per-endpoint recompute interval from config.json. Zero values fall
// back to the defaultInterval passed to StartAnalyticsRecomputers.
type AnalyticsRecomputeIntervals struct {
	Topology             time.Duration
	RF                   time.Duration
	Distance             time.Duration
	Channels             time.Duration
	HashCollisions       time.Duration
	HashSizes            time.Duration
	Roles                time.Duration
	ObserversClockSkew   time.Duration
	NodesClockSkew       time.Duration
	AdvertIntervals      time.Duration // #245 M2

	// PauseWhenIdle is applied to every recomputer (#492,
	// AnalyticsConfig.PauseWhenIdle).
	PauseWhenIdle bool
}

func pickInterval(override, def time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	return def
}

// StartAnalyticsRecomputers wires each analytics endpoint to a
// background recompute goroutine. Each runs an initial compute
// synchronously (so the first read after startup is a cache hit, never
// cold) and then refreshes on a ticker.
//
// All recomputers serve the DEFAULT query shape only: region="" and
// zero-window (no ?since= / ?until= params). Region-keyed or windowed
// queries continue to use the legacy on-request compute + TTL cache —
// the recomputer count would explode if we maintained one per
// (endpoint × region × window) combination, and region filtering is
// fast read-time work anyway.
//
// Returns a stop closure that signals all goroutines and blocks until
// they exit. Safe to call once per PacketStore. Idempotent if called
// multiple times (subsequent calls return the first stop closure).
func (s *PacketStore) StartAnalyticsRecomputers(defaultInterval time.Duration, overrides ...AnalyticsRecomputeIntervals) func() {
	if defaultInterval <= 0 {
		defaultInterval = 5 * time.Minute
	}
	var ov AnalyticsRecomputeIntervals
	if len(overrides) > 0 {
		ov = overrides[0]
	}

	s.analyticsRecomputerMu.Lock()
	if s.recompTopology != nil {
		// Already started; return a no-op so the caller's defer is harmless.
		s.analyticsRecomputerMu.Unlock()
		return func() {}
	}

	// Each recomputer wraps the underlying compute* function with the
	// default arguments. We use computeAnalytics* (not GetAnalytics*) to
	// bypass the legacy TTL cache layer — the recomputer IS the cache.
	s.recompTopology = newAnalyticsRecomputer(
		"topology", pickInterval(ov.Topology, defaultInterval),
		func() interface{} { return s.computeAnalyticsTopology("", "", TimeWindow{}) },
	)
	s.recompRF = newAnalyticsRecomputer(
		"rf", pickInterval(ov.RF, defaultInterval),
		func() interface{} { return s.computeAnalyticsRF("", "", TimeWindow{}) },
	)
	s.recompDistance = newAnalyticsRecomputer(
		"distance", pickInterval(ov.Distance, defaultInterval),
		func() interface{} { return s.computeAnalyticsDistance("", "") },
	)
	s.recompChannels = newAnalyticsRecomputer(
		"channels", pickInterval(ov.Channels, defaultInterval),
		func() interface{} { return s.computeAnalyticsChannels("", "", TimeWindow{}) },
	)
	s.recompHashCollisions = newAnalyticsRecomputer(
		"hash-collisions", pickInterval(ov.HashCollisions, defaultInterval),
		func() interface{} { return s.computeHashCollisions("", "") },
	)
	s.recompHashSizes = newAnalyticsRecomputer(
		"hash-sizes", pickInterval(ov.HashSizes, defaultInterval),
		func() interface{} { return s.computeAnalyticsHashSizesWithCapability("", "") },
	)
	s.recompRoles = newAnalyticsRecomputer(
		"roles", pickInterval(ov.Roles, defaultInterval),
		func() interface{} { return s.computeAnalyticsRoles() },
	)
	s.recompObserversClockSkew = newAnalyticsRecomputer(
		"observers-clock-skew", pickInterval(ov.ObserversClockSkew, defaultInterval),
		func() interface{} { return s.computeObserverCalibrations() },
	)
	s.recompNodesClockSkew = newAnalyticsRecomputer(
		"nodes-clock-skew", pickInterval(ov.NodesClockSkew, defaultInterval),
		func() interface{} { return s.computeFleetClockSkew() },
	)
	s.recompAdvertIntervals = newAnalyticsRecomputer(
		"advert-intervals", pickInterval(ov.AdvertIntervals, defaultInterval),
		func() interface{} { return s.computeAdvertIntervalAnalytics() },
	)
	// computeAnalyticsRoles reads the nodes-clock-skew snapshot
	// (GetFleetClockSkew): with pauseWhenIdle a paused clock-skew is
	// refreshed before roles computes (#492).
	s.recompRoles.deps = []*analyticsRecomputer{s.recompNodesClockSkew}
	all := s.analyticsRecomputersLocked()
	s.analyticsRecomputerMu.Unlock()

	// Issue #1659 (PR #1688 r1, munger #5): wire the loader readiness
	// gate on the three warmup-gated recomputers (RF, Topology,
	// Channels). #116: only a pass that STARTS after the whole startup
	// load terminated (hot window AND background fill) ends the
	// warm-up. LoadComplete() is not enough: it flips at the end of the
	// hot window, so the gate used to open on a snapshot that missed the
	// background fill.
	loaded := s.StartupLoadDone()
	loadedGate := func() bool {
		select {
		case <-loaded:
			return true
		default:
			return false
		}
	}
	s.recompRF.setWarmupReadyGate_1659(loadedGate)
	s.recompTopology.setWarmupReadyGate_1659(loadedGate)
	s.recompChannels.setWarmupReadyGate_1659(loadedGate)
	for _, rc := range all {
		rc.pauseWhenIdle = ov.PauseWhenIdle
		rc.pauseReadyGate = loadedGate
	}

	for _, rc := range all {
		rc.Start()
	}

	// #116: main.go starts the recomputers at the first load chunk, so
	// the initial computes above saw part of the data. Recompute every
	// one as soon as the startup load terminates instead of a full
	// interval later.
	stopPostLoad := make(chan struct{})
	postLoadDone := make(chan struct{})
	go func() {
		defer close(postLoadDone)
		recomputeWhenLoaded(loaded, stopPostLoad, all)
	}()

	var stopOnce sync.Once
	return func() {
		stopOnce.Do(func() {
			close(stopPostLoad)
			for _, rc := range all {
				rc.Stop()
			}
			<-postLoadDone
		})
	}
}

// analyticsRecomputersLocked lists every analytics recomputer in start and
// post-load recompute order (#116): the three warm-up-gated recomputers
// first, and roles after nodes-clock-skew because computeAnalyticsRoles
// reads that snapshot (GetFleetClockSkew). Caller holds
// analyticsRecomputerMu.
func (s *PacketStore) analyticsRecomputersLocked() []*analyticsRecomputer {
	return []*analyticsRecomputer{
		s.recompRF, s.recompTopology, s.recompChannels,
		s.recompDistance, s.recompHashCollisions, s.recompHashSizes,
		s.recompObserversClockSkew, s.recompNodesClockSkew,
		s.recompRoles, s.recompAdvertIntervals,
	}
}
