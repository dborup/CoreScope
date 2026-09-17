package main

import (
	"strings"
	"time"
)

// GetRepeaterRelayInfoMap returns a cached pubkey → RepeaterRelayInfo
// map covering EVERY pubkey that currently appears as a path hop in any
// non-advert StoreTx. This is the bulk equivalent of calling
// GetRepeaterRelayInfo(pk, windowHours) once per node.
//
// Why this exists (issue #1257): handleNodes used to call the per-node
// helper inside a per-page loop. Each call grabbed its own RLock and
// re-parsed FirstSeen on every StoreTx indexed under that pubkey's
// byPathHop entry (plus, when the pubkey was >= 2 hex chars, the 1-byte
// prefix bucket — which on busy networks fans out to almost the whole
// non-advert tx set). For the default top-50 page of hot repeaters this
// burned 50 lock acquisitions and hundreds of thousands of timestamp
// parses per request, dominating /api/nodes latency.
//
// The cached map is keyed by lowercase pubkey/hop key (same shape as
// byPathHop). Lookups should use strings.ToLower(pk).
//
// The cache is refreshed by the background recomputer (every 5 min by
// default). This function never rebuilds inline on a populated cache —
// serving a slightly stale snapshot is always preferable to a 700ms
// on-request rebuild. The only time an inline compute happens is when
// the cache is nil (i.e. before the recomputer's synchronous prewarm
// completes, which can occur in tests without a running recomputer).
func (s *PacketStore) GetRepeaterRelayInfoMap(windowHours float64) map[string]RepeaterRelayInfo {
	s.repeaterEnrichMu.Lock()
	cached := s.repeaterRelayCache
	s.repeaterEnrichMu.Unlock()
	if cached != nil {
		return cached
	}

	// Cache is nil — recomputer hasn't prewarmed yet (edge case: tests
	// without a running recomputer, or a request racing the initial
	// synchronous prewarm). Build once inline; the recomputer takes over.
	result := s.computeRepeaterRelayInfoMap(windowHours)

	s.repeaterEnrichMu.Lock()
	if s.repeaterRelayCache == nil {
		s.repeaterRelayCache = result
		s.repeaterRelayCacheWin = windowHours
		s.repeaterRelayAt = time.Now()
	}
	cached = s.repeaterRelayCache
	s.repeaterEnrichMu.Unlock()
	return cached
}

// computeRepeaterRelayInfoMap walks byPathHop once under a single RLock,
// pre-parses every FirstSeen timestamp once (not once-per-pubkey-bucket),
// and emits one RepeaterRelayInfo per hop key.
//
// Time-complexity invariant: O(unique-tx-in-byPathHop + total-key-bucket
// entries + total raw-path hops): each tx's identity-safe keys and timestamp
// are processed once, then bucket membership uses O(1) lookups. A unique raw
// prefix belongs to only one full key, avoiding collided-prefix fanout.
// Memory: O(unique tx + keys + raw-path hops), bounded by store eviction.
func (s *PacketStore) computeRepeaterRelayInfoMap(windowHours float64) map[string]RepeaterRelayInfo {
	s.mu.RLock()
	pm := s.relayPrefixMapLocked()

	// Snapshot the slices (header copy) so we can release the lock before
	// the expensive parse pass. Slice headers point at the live underlying
	// arrays but those are append-only-by-id; the worst-case race here is
	// that ingest grows a slice we already snapshotted (we miss the new
	// tail), which is acceptable for a 15s-TTL status read.
	snap := make(map[string][]*StoreTx, len(s.byPathHop))
	for k, list := range s.byPathHop {
		snap[k] = list
	}

	// Build a tx-id-keyed pre-parsed cache so the inner loop doesn't
	// re-parse the same FirstSeen N times when the same tx is indexed
	// under multiple hop keys (very common — every hop on a path indexes
	// the tx).
	parseCache := make(map[int]relayEntry, 1<<14)
	for _, list := range snap {
		for _, tx := range list {
			if tx == nil {
				continue
			}
			if _, ok := parseCache[tx.ID]; ok {
				continue
			}
			t, ok := parseRelayTS(tx.FirstSeen)
			p := relayEntry{t: t, valid: ok}
			if txHasObservedFloodPath(tx) {
				p.confirmedKeys = make(map[string]struct{})
				for _, token := range txGetParsedPath(tx) {
					if key := confirmedRelayKey(token, pm); key != "" {
						p.confirmedKeys[key] = struct{}{}
					}
				}
			}
			parseCache[tx.ID] = p
		}
	}
	s.mu.RUnlock()

	out := make(map[string]RepeaterRelayInfo, len(snap))
	keys := make(map[string]struct{}, len(snap))
	for key := range snap {
		keys[key] = struct{}{}
		// Unique raw-only prefixes need a full-key result too; otherwise
		// list and detail disagree until a resolved-path index is populated.
		if resolved := confirmedRelayKey(key, pm); resolved != "" {
			keys[resolved] = struct{}{}
		}
	}
	for key := range keys {
		entries := collectConfirmedRelayEntries(snap, key, pm, parseCache)
		out[key] = computeRelayInfoFromEntries(entries, windowHours)
	}
	return out
}

// GetRepeaterUsefulnessScoreMap returns a cached pubkey → 0..1 score
// for every pubkey appearing in byPathHop. Bulk equivalent of
// GetRepeaterUsefulnessScore. See GetRepeaterRelayInfoMap for the
// motivation (#1257) and the no-inline-rebuild rationale (#1272).
func (s *PacketStore) GetRepeaterUsefulnessScoreMap() map[string]float64 {
	s.repeaterEnrichMu.Lock()
	cached := s.repeaterUsefulCache
	s.repeaterEnrichMu.Unlock()
	if cached != nil {
		return cached
	}

	result := s.computeRepeaterUsefulnessScoreMap()

	s.repeaterEnrichMu.Lock()
	if s.repeaterUsefulCache == nil {
		s.repeaterUsefulCache = result
		s.repeaterUsefulAt = time.Now()
	}
	cached = s.repeaterUsefulCache
	s.repeaterEnrichMu.Unlock()
	return cached
}

func (s *PacketStore) computeRepeaterUsefulnessScoreMap() map[string]float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	totalNonAdvert := 0
	for pt, list := range s.byPayloadType {
		if pt == payloadTypeAdvert {
			continue
		}
		totalNonAdvert += len(list)
	}
	out := make(map[string]float64, len(s.byPathHop))
	if totalNonAdvert == 0 {
		return out
	}
	denom := float64(totalNonAdvert)
	for key, list := range s.byPathHop {
		relayed := 0
		for _, tx := range list {
			if tx == nil {
				continue
			}
			if tx.PayloadType != nil && *tx.PayloadType == payloadTypeAdvert {
				continue
			}
			relayed++
		}
		if relayed == 0 {
			continue
		}
		score := float64(relayed) / denom
		if score < 0 {
			score = 0
		} else if score > 1 {
			score = 1
		}
		out[key] = score
	}
	return out
}

// lookupRelayInfo is a small helper to make handleNodes' map lookup
// case-insensitive (byPathHop keys are lowercase; pubkeys arriving from
// the DB row may be either case).
func lookupRelayInfo(m map[string]RepeaterRelayInfo, pubkey string) (RepeaterRelayInfo, bool) {
	if v, ok := m[pubkey]; ok {
		return v, true
	}
	if lc := strings.ToLower(pubkey); lc != pubkey {
		if v, ok := m[lc]; ok {
			return v, true
		}
	}
	return RepeaterRelayInfo{}, false
}

// lookupUsefulnessScore mirrors lookupRelayInfo for the score map.
func lookupUsefulnessScore(m map[string]float64, pubkey string) float64 {
	if v, ok := m[pubkey]; ok {
		return v
	}
	if lc := strings.ToLower(pubkey); lc != pubkey {
		if v, ok := m[lc]; ok {
			return v
		}
	}
	return 0
}
