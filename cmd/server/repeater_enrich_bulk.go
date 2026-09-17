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

// computeRepeaterRelayInfoMap walks the relay candidate indexes once under a
// single RLock, pre-parses every FirstSeen timestamp and relay identity once
// (not once-per-pubkey-bucket), and emits one RepeaterRelayInfo per hop key
// and per confirmed relay key.
//
// Time-complexity invariant: O(unique candidate tx + observation-path hops +
// total candidate-bucket entries): each tx's identity-safe keys and timestamp
// are computed once (hop tokens memoized per request), then every key walks
// only its own candidate buckets (see forEachRelayCandidate) with O(1)
// generation-stamped dedupe. A unique raw prefix belongs to only one full key,
// avoiding collided-prefix fanout. Memory: O(unique tx + keys + distinct hop
// tokens), bounded by store eviction.
func (s *PacketStore) computeRepeaterRelayInfoMap(windowHours float64) map[string]RepeaterRelayInfo {
	s.mu.RLock()
	pm := s.relayPrefixMapLocked()

	// Snapshot the slices (header copy) so we can release the lock before
	// the per-key pass. Slice headers point at the live underlying
	// arrays but those are append-only-by-id; the worst-case race here is
	// that ingest grows a slice we already snapshotted (we miss the new
	// tail), which is acceptable for a 15s-TTL status read.
	snap := make(map[string][]*StoreTx, len(s.byPathHop))
	for k, list := range s.byPathHop {
		snap[k] = list
	}
	// byNode candidates only matter for keys a raw hop can confirm: known
	// relay prefixes/keys or exact 64-hex identities.
	nodeSnap := make(map[string][]*StoreTx)
	for k, list := range s.byNode {
		if len(k) == 64 || (pm != nil && len(pm.m[k]) > 0) {
			nodeSnap[k] = list
		}
	}

	// Pre-compute, per transmission and under the lock (observations are
	// mutable), its timestamp and the identity-safe relay keys confirmed by
	// its raw observed flood paths. The unlocked pass below uses only this.
	type bulkRelayTx struct {
		entry relayEntry
		keys  []string
		gen   int
	}
	resolve := newRelayTokenResolver(pm)
	index := make(map[int]int, 1<<14)
	txs := make([]bulkRelayTx, 0, 1<<14)
	confirmed := make(map[string]struct{})
	add := func(list []*StoreTx) {
		for _, tx := range list {
			if tx == nil {
				continue
			}
			if _, ok := index[tx.ID]; ok {
				continue
			}
			b := bulkRelayTx{entry: newRelayEntry(tx, false)}
			b.entry.parsed = true
			b.entry.t, b.entry.valid = parseRelayTS(tx.FirstSeen)
			if txHasObservedFloodPath(tx) {
				forEachObservedRelayHop(tx, func(token string) bool {
					key := resolve.key(token)
					if key == "" {
						return true
					}
					for _, have := range b.keys {
						if have == key {
							return true
						}
					}
					b.keys = append(b.keys, key)
					confirmed[key] = struct{}{}
					return true
				})
			}
			index[tx.ID] = len(txs)
			txs = append(txs, b)
		}
	}
	for _, list := range snap {
		add(list)
	}
	for _, list := range nodeSnap {
		add(list)
	}
	s.mu.RUnlock()

	keys := make(map[string]struct{}, len(snap)+len(confirmed))
	for key := range snap {
		keys[key] = struct{}{}
		// Unique raw-only prefixes need a full-key result too.
		if resolved := resolve.key(key); resolved != "" {
			keys[resolved] = struct{}{}
		}
	}
	// Keys with evidence only via byNode (e.g. non-display observations after
	// a restart) need a result too; otherwise list and detail disagree.
	for key := range confirmed {
		keys[key] = struct{}{}
	}
	out := make(map[string]RepeaterRelayInfo, len(keys))
	var entries []relayEntry
	gen := 0
	for key := range keys {
		gen++
		entries = entries[:0]
		m := newRelayKeyMatcher(key, pm)
		if m.possible() {
			forEachRelayCandidate(snap, nodeSnap, m, func(tx *StoreTx, fromPrefix bool) {
				b := &txs[index[tx.ID]]
				if b.gen == gen {
					return
				}
				b.gen = gen
				for _, have := range b.keys {
					if have == key {
						e := b.entry
						e.fromPrefix = fromPrefix
						entries = append(entries, e)
						return
					}
				}
			})
		}
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
