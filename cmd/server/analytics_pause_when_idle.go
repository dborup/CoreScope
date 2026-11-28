// Package main: snapshot freshness headers for analytics.pauseWhenIdle
// (issue #492).
//
// With pauseWhenIdle on, a recomputer nobody reads stops refreshing, and
// the first read after that is served the snapshot it already holds while
// one refresh runs. That snapshot can be older than the recompute
// interval, so every default-shape response served from a recomputer says
// how old its snapshot is and whether it missed a scheduled refresh. With
// pauseWhenIdle off no header is set: responses are unchanged.
package main

import (
	"net/http"
	"strconv"
	"time"
)

const (
	// analyticsSnapshotHeader is "current" or "stale" ("stale": the
	// snapshot outlived a refresh skipped while idle; a refresh is on
	// its way).
	analyticsSnapshotHeader = "X-Analytics-Snapshot"
	// analyticsSnapshotAgeHeader is the snapshot's age in whole seconds.
	analyticsSnapshotAgeHeader = "X-Analytics-Snapshot-Age-Seconds"
)

// recomputer returns *field under analyticsRecomputerMu (nil before
// StartAnalyticsRecomputers).
func (s *PacketStore) recomputer(field **analyticsRecomputer) *analyticsRecomputer {
	s.analyticsRecomputerMu.RLock()
	defer s.analyticsRecomputerMu.RUnlock()
	return *field
}

// setAnalyticsSnapshotHeaders labels a response about to be served from
// rc's snapshot. Call it BEFORE reading the snapshot: a refresh that lands
// in between only makes the served snapshot newer than labelled, never
// older. No-op when rc is nil, has no snapshot or pauseWhenIdle is off.
func setAnalyticsSnapshotHeaders(w http.ResponseWriter, rc *analyticsRecomputer) {
	if rc == nil || !rc.pauseWhenIdle {
		return
	}
	at, stale := rc.SnapshotInfo()
	if at.IsZero() {
		return
	}
	state := "current"
	if stale {
		state = "stale"
	}
	age := rc.now().Sub(at)
	if age < 0 {
		age = 0
	}
	w.Header().Set(analyticsSnapshotHeader, state)
	w.Header().Set(analyticsSnapshotAgeHeader, strconv.FormatInt(int64(age/time.Second), 10))
}
