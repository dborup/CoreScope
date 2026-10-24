// Issue #1008: background-deferred pathHop index build.
//
// Pattern mirrors the distance index (#1011) — but where distance is
// fully lazy (built on first request), this index is kicked off
// eagerly by Load() in a background goroutine so HTTP becomes ready
// immediately while the index finishes populating. Route Patterns
// (subpaths) had a second such index until #413; they are now counted
// on demand and need no build or gate.
//
// Concurrency model:
//
//   - pathHopReady is an atomic.Bool flag written exactly once by the
//     background builder (false → true) and never reset thereafter.
//     Handlers read it via PathHopIndexReady() before touching
//     s.byPathHop. While it is false, a gated handler responds 503 +
//     Retry-After: 5.
//
//   - The builder itself acquires s.mu.Lock() and calls the existing
//     buildPathHopIndex() method. It replaces s.byPathHop with a
//     freshly-allocated map under the write lock. Visibility of the populated
//     maps to handlers that see Ready()==true is guaranteed by Go's
//     sync/atomic acquire-release semantics (formalized in Go 1.19):
//     the atomic.Store(true) happens-after the s.mu.Unlock() that
//     completes the build, and the handler's atomic.Load()==true
//     synchronizes-with that store. The handler's subsequent s.mu.RLock
//     is not what establishes visibility — it only serializes against
//     concurrent ingest writers — so dropping the RLock would still be
//     safe for the build's "populated map" snapshot (we keep it for
//     ingest serialization).
//
//   - Ingest-side incremental updates in StoreNewTransmissions /
//     pruning / hash-collision paths continue to write s.byPathHop
//     directly under s.mu.Lock(). Because
//     the builder also runs under s.mu.Lock() and the builder
//     overwrites whatever is there, the brief window between Load()
//     returning and the goroutine acquiring s.mu means any
//     concurrent ingest writes will be overwritten by the build —
//     this matches the prior behavior where ingest could not start
//     until Load() released s.mu, so in practice ingest does not
//     run during the build window. Documenting this rather than
//     adding a separate gate: the existing main.go boot sequence
//     does not start ingest goroutines until after store.Load()
//     and graph init complete.
//
// Handler scope of the ready gate (issue #1008 review M2):
//
//   - HARD-GATED with 503 + Retry-After: 5 — endpoints whose entire
//     response is the index lookup. Empty data would be visibly broken.
//     See routes.go: /api/nodes/{pubkey}/paths.
//
//   - BEST-EFFORT (not gated) — endpoints where the index drives
//     enrichment fields that callers already treat as optional. During
//     the not-ready window these report zero counts / nil scores
//     rather than 503-ing the whole list. Acceptable because:
//
//       * /api/nodes and /api/nodes/{pubkey} have many other fields
//         (last-seen, position, advert metadata) that callers depend
//         on at startup. 503-ing the SPA bootstrap to wait for an
//         index that exclusively affects "relay activity" badges
//         would be a worse UX than a 30–60s window of "—" badges.
//
//       * GetRepeaterRelayInfoMap / GetRepeaterUsefulnessScoreMap /
//         GetBridgeScore / repeater_liveness / repeater_usefulness
//         all walk s.byPathHop. During the build window they return
//         empty maps or zero scores; the steady-state recomputer
//         (#1262) refreshes them every 5min once the index flips ready
//         (prewarm guarded by WaitIndexesReady — see review M1).
//
//     This is documented rather than gated so operators do not see
//     /api/nodes 503 during routine restarts on Cascadia-scale data.
package main

import (
	"log"
	"net/http"
	"time"
)

// writeIndexLoading503 emits the standard 503 response used by handlers
// that depend on a not-yet-built index (#1008). Body shape matches the
// triage spec: {"error":"index loading","retryAfter":5}. The Retry-After
// header is also set so well-behaved clients back off automatically.
func writeIndexLoading503(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "5")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`{"error":"index loading","retryAfter":5}`))
}

// PathHopIndexReady reports whether the path-hop index build kicked
// off by Load() has completed (#1008). Until this returns true,
// callers must NOT read s.byPathHop.
func (s *PacketStore) PathHopIndexReady() bool {
	return s.pathHopReady.Load()
}

// indexReadyCh returns the channel that is closed when the path-hop
// index has flipped ready. Lazily created on first access. Safe to call
// concurrently. Used by WaitIndexesReady and any future waiters that
// want event-driven semantics instead of polling.
func (s *PacketStore) indexReadyCh() <-chan struct{} {
	s.indexReadyChMu.Lock()
	defer s.indexReadyChMu.Unlock()
	if s.indexReadyChan == nil {
		s.indexReadyChan = make(chan struct{})
		// If it is already ready (e.g. background chunk loader
		// flipped it synchronously before any waiter showed up),
		// close immediately so the channel is usable as a one-shot.
		if s.pathHopReady.Load() {
			close(s.indexReadyChan)
		}
	}
	return s.indexReadyChan
}

// maybeCloseIndexReadyCh closes the ready channel iff the flag is set.
// Idempotent and safe to call from the builder goroutine on the
// green-path transition, as well as from markIndexesReadySync.
func (s *PacketStore) maybeCloseIndexReadyCh() {
	if !s.pathHopReady.Load() {
		return
	}
	s.indexReadyChMu.Lock()
	defer s.indexReadyChMu.Unlock()
	if s.indexReadyChan == nil {
		// Lazily allocate AND close it in one step so any future
		// indexReadyCh() caller gets a pre-closed channel.
		s.indexReadyChan = make(chan struct{})
		close(s.indexReadyChan)
		return
	}
	select {
	case <-s.indexReadyChan:
		// Already closed.
	default:
		close(s.indexReadyChan)
	}
}

// startBackgroundIndexBuilds is called from Load() after s.loaded=true
// to populate the path-hop index off the critical path (#1008). It
// returns immediately; the work runs in a background goroutine that
// acquires s.mu.Lock(), installs the map, then sets the ready flag.
//
// At Cascadia scale (~5M observations) this previously blocked HTTP
// readiness ~60s inside Load() under s.mu.
func (s *PacketStore) startBackgroundIndexBuilds() {
	gate := s.pathHopBuildGate // read before the goroutine starts
	go func() {
		if gate != nil {
			gate()
		}
		t1 := time.Now()
		s.mu.Lock()
		s.buildPathHopIndex()
		s.mu.Unlock()
		// Atomic.Store happens-after s.mu.Unlock; readers that
		// observe Ready()==true synchronize-with this store.
		s.pathHopReady.Store(true)
		s.maybeCloseIndexReadyCh()
		log.Printf("[startup] index build complete: pathHop (%s)",
			time.Since(t1).Round(time.Millisecond))
	}()
}

// markIndexesReadySync is the synchronous-build entry point used by
// the background chunk loader in store.go (and by tests). The chunk
// loader rebuilds the index under s.mu.Lock(); after the Unlock it
// calls this to flip the ready flag and close the broadcast channel
// in one shot, preserving symmetry with the goroutine path above.
func (s *PacketStore) markIndexesReadySync() {
	s.pathHopReady.Store(true)
	s.maybeCloseIndexReadyCh()
}

// WaitIndexesReady blocks until the background index built by
// startBackgroundIndexBuilds() reports ready, or the deadline expires.
// Returns true if it flipped in time. Intended for tests that read
// s.byPathHop directly after Load(), and for background work that
// needs it (recomputer prewarm, hash migration); request paths check
// PathHopIndexReady() instead of blocking.
//
// Uses the indexReadyCh broadcast channel rather than polling
// (see review m6) so wake-up is immediate with no poll-interval jitter.
func (s *PacketStore) WaitIndexesReady(timeout time.Duration) bool {
	if s.PathHopIndexReady() {
		return true
	}
	ch := s.indexReadyCh()
	select {
	case <-ch:
		return true
	case <-time.After(timeout):
		return s.PathHopIndexReady()
	}
}

