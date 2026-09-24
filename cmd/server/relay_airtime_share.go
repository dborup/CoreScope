package main

import (
	"log"
	"sort"
	"time"

	"github.com/meshcore-analyzer/lora"
	"github.com/meshcore-analyzer/packetpath"
)

// relay_airtime_share.go — issues #1359 + #1768
//
// Implements the "Relay Airtime Share" analytics metric:
//   score(packet) = TimeOnAir(payload_bytes, preset)
//                   × COUNT(DISTINCT repeater_pubkey across observations)
//
// #1768 swapped the original byte-only proxy (`bytes × relays`) for
// closed-form LoRa Time-on-Air. The byte proxy underweighted small
// frames by ~3-4× because the additive preamble + fixed-symbol
// intercept does NOT cancel under per-type normalization. ToA fixes
// the headline divergence the dumbbell chart is supposed to show.
//
// The PHY preset is config-driven (analytics.loraPreset in
// config.example.json); defaults match the actual deployment
// preset 869.6 MHz / BW 62.5 kHz / SF 8 / CR 4/5, with the
// SF-dependent preamble pulled from internal/lora.PreambleForSF.
//
// Aggregated by payload_type, except ADVERT packets which are split into flood
// and zero-hop route classes. Originator TX is deliberately excluded — a
// never-relayed direct message scores 0, which is the correct framing for a
// "relay amplification" metric. In-memory only; no SQL, no new index.

// defaultLoRaPreset is the canonical fallback when config is absent.
// Matches the reporter's `get radio` output `869.6179809, 62.5, 8, 5`.
func defaultLoRaPreset() lora.Preset {
	return lora.Preset{
		FreqHz:   869.6e6,
		BWkHz:    62.5,
		SF:       8,
		CR:       5,
		Preamble: lora.PreambleForSF(8),
	}
}

// resolveLoRaPreset returns the effective preset, falling back to
// defaults for any unset / zero / out-of-range field.
//
// Out-of-range SF / CR are NOT silently clamped on a per-field basis
// (the prior behaviour produced a confusing hybrid preset, partially
// operator-supplied and partially defaulted). Instead we keep the
// default for the offending field AND log a single WARN at resolve time
// naming the field plus the actual vs. effective value. There is no
// startup-time analytics-config validation gate today, so refusal-to-
// start is not an option — the WARN is the gate. Zero / unset fields
// fall back silently as before (the operator opted out of overriding
// that param).
func (s *PacketStore) resolveLoRaPreset() lora.Preset {
	p := defaultLoRaPreset()
	if s == nil || s.config == nil || s.config.Analytics == nil || s.config.Analytics.LoRaPreset == nil {
		return p
	}
	cfg := s.config.Analytics.LoRaPreset
	if cfg.FreqHz > 0 {
		p.FreqHz = cfg.FreqHz
	}
	if cfg.BWkHz > 0 {
		p.BWkHz = cfg.BWkHz
	}
	if cfg.SF != 0 {
		if cfg.SF >= 6 && cfg.SF <= 12 {
			p.SF = cfg.SF
			p.Preamble = lora.PreambleForSF(cfg.SF)
		} else {
			log.Printf("[analytics.loraPreset] WARN: sf=%d out of range [6,12], using default sf=%d", cfg.SF, p.SF)
		}
	}
	if cfg.CR != 0 {
		if cfg.CR >= 5 && cfg.CR <= 8 {
			p.CR = cfg.CR
		} else {
			log.Printf("[analytics.loraPreset] WARN: cr=%d out of range [5,8], using default cr=%d", cfg.CR, p.CR)
		}
	}
	return p
}

// presetResponse shapes the preset for the API response and the
// analytics caption (issue #1768 — operators can't interpret an
// "Airtime %" headline without knowing what PHY assumptions it bakes
// in). All four free params plus the derived preamble are surfaced.
type presetResponse struct {
	FreqHz   float64 `json:"freq_hz"`
	BWkHz    float64 `json:"bw_khz"`
	SF       int     `json:"sf"`
	CR       int     `json:"cr"`
	Preamble int     `json:"preamble"`
}

func presetJSON(p lora.Preset) presetResponse {
	return presetResponse{
		FreqHz:   p.FreqHz,
		BWkHz:    p.BWkHz,
		SF:       p.SF,
		CR:       p.CR,
		Preamble: p.Preamble,
	}
}

// distinctRelayCount returns the number of distinct repeater pubkeys that
// forwarded `tx`, unioned across ALL observations of that transmission_id.
//
// Source: the resolved-pubkey reverse index — populated by
// indexResolvedPathHops / addToResolvedPubkeyIndex from every observation's
// resolved_path. Each entry is one distinct pubkey hash for THIS tx (the
// indexer dedups (hash, txID) pairs before appending).
//
// Caller MUST hold s.mu at least RLock.
func (s *PacketStore) distinctRelayCount(tx *StoreTx) int {
	if tx == nil || !s.useResolvedPathIndex {
		return 0
	}
	return len(s.resolvedPubkeyReverse[tx.ID])
}

// AirtimeForTransmissions sums LoRa Time-on-Air × distinct-resolved-repeater
// count (the same score formula as computeRelayAirtimeShare / issue #1768)
// for an arbitrary, caller-supplied set of transmission IDs — e.g. one
// wardriving session's messages, rather than every packet in a time window.
//
// Returns ok=false when the resolved-path index is unavailable, OR when
// NONE of the requested IDs are currently held in memory — the store is
// memory-bounded (maxMemoryMB/maxPackets, see store.go), so a transmission
// well within the SQL window can still have been evicted from RAM. In that
// case a silent 0 would look like "genuinely never relayed" when it's
// really "don't know" — callers should omit the field, not show a zero.
// A PARTIAL match (some found, some evicted) still returns ok=true with
// the known subset's total; the alternative (only ok when ALL match) would
// make the feature return nothing at all once retention exceeds the
// in-memory window, which every other airtime metric already tolerates.
func (s *PacketStore) AirtimeForTransmissions(txIDs []int64) (total time.Duration, ok bool) {
	if s == nil || !s.useResolvedPathIndex || len(txIDs) == 0 {
		return 0, false
	}
	want := make(map[int]bool, len(txIDs))
	for _, id := range txIDs {
		want[int(id)] = true
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	preset := s.resolveLoRaPreset()
	matched := 0
	for id := range want {
		tx := s.byTxID[id]
		if tx == nil {
			continue
		}
		matched++
		d, _ := s.airtimeForTx(tx, preset)
		total += d
	}
	if matched == 0 {
		return 0, false
	}
	return total, true
}

// airtimeForTx computes one transmission's LoRa Time-on-Air ×
// distinct-resolved-repeater-count contribution -- the shared per-tx
// formula behind both AirtimeForTransmissions (aggregate) and
// AirtimeAndRelayCountForTransmission (single tx, relay count exposed).
// Caller MUST hold s.mu at least RLock.
func (s *PacketStore) airtimeForTx(tx *StoreTx, preset lora.Preset) (time.Duration, int) {
	payloadBytes := len(tx.RawHex) / 2
	relays := s.distinctRelayCount(tx)
	return lora.TimeOnAir(payloadBytes, preset) * time.Duration(relays), relays
}

// AirtimeAndRelayCountForTransmission is AirtimeForTransmissions narrowed
// to a single transmission, additionally returning the distinct-relay
// count that fed the estimate -- View Path's "N relays" caveat needs it;
// AirtimeForTransmissions's aggregate-only callers don't. Same
// availability caveat: ok=false when the resolved-path index is off or
// this transmission isn't currently held in memory (evicted) -- callers
// should omit the field, not show a zero.
func (s *PacketStore) AirtimeAndRelayCountForTransmission(txID int64) (total time.Duration, relays int, ok bool) {
	if s == nil || !s.useResolvedPathIndex {
		return 0, 0, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	tx := s.byTxID[int(txID)]
	if tx == nil {
		return 0, 0, false
	}
	preset := s.resolveLoRaPreset()
	total, relays = s.airtimeForTx(tx, preset)
	return total, relays, true
}

type relayAirtimeAdvertRoute uint8

const (
	relayAirtimeAdvertUnknown relayAirtimeAdvertRoute = iota
	relayAirtimeAdvertFlood
	relayAirtimeAdvertZeroHop
	relayAirtimeAdvertMixed // #89: the same content hash was seen on flood and zero-hop routes
)

type relayAirtimeBucketKey struct {
	payloadType int
	advertRoute relayAirtimeAdvertRoute
}

// advertRouteClass is the single place that classifies an ADVERT's route.
// With a known route_mask (#89) it uses every raw route observed for the
// hash, so the result does not depend on ingest order: only routes 0/1 is
// flood, only routes 2/3 is zero-hop, both groups is mixed. Without usable
// mask bits (column absent, row not backfilled yet, or no valid route ever
// recorded) it falls back to the legacy first-inserted route_type, and
// NULL/out-of-range values stay unknown (the historical ADVERT bucket).
func advertRouteClass(tx *StoreTx) relayAirtimeAdvertRoute {
	if tx.routeMaskKnown && int64(tx.routeMask)&packetpath.RouteMaskAll != 0 {
		flood := int64(tx.routeMask)&packetpath.RouteMaskFlood != 0
		direct := int64(tx.routeMask)&packetpath.RouteMaskDirect != 0
		switch {
		case flood && direct:
			return relayAirtimeAdvertMixed
		case flood:
			return relayAirtimeAdvertFlood
		default:
			return relayAirtimeAdvertZeroHop
		}
	}
	if tx.RouteType == nil {
		return relayAirtimeAdvertUnknown
	}
	switch *tx.RouteType {
	case RouteTransportFlood, RouteFlood:
		return relayAirtimeAdvertFlood
	case RouteDirect, RouteTransportDirect:
		return relayAirtimeAdvertZeroHop
	}
	return relayAirtimeAdvertUnknown
}

// relayAirtimeKey keeps non-ADVERT aggregation unchanged while separating the
// ADVERT route classes (see advertRouteClass). tx.PayloadType must be non-nil.
func relayAirtimeKey(tx *StoreTx) relayAirtimeBucketKey {
	key := relayAirtimeBucketKey{payloadType: *tx.PayloadType}
	if key.payloadType == PayloadADVERT {
		key.advertRoute = advertRouteClass(tx)
	}
	return key
}

func relayAirtimeBucketName(key relayAirtimeBucketKey) string {
	name := payloadTypeNames[key.payloadType]
	if name == "" {
		name = "UNK"
	}
	if key.payloadType != PayloadADVERT {
		return name
	}
	switch key.advertRoute {
	case relayAirtimeAdvertFlood:
		return name + " (flood)"
	case relayAirtimeAdvertZeroHop:
		return name + " (zero-hop)"
	case relayAirtimeAdvertMixed:
		return name + " (mixed)"
	default:
		return name
	}
}

// Machine-readable ADVERT route classes, returned as route_class. They are
// part of the API contract and deliberately independent of the display
// labels built by relayAirtimeBucketName.
const (
	relayAirtimeRouteClassFlood   = "flood"
	relayAirtimeRouteClassZeroHop = "zero_hop"
	relayAirtimeRouteClassMixed   = "mixed"
	relayAirtimeRouteClassLegacy  = "legacy"
)

// relayAirtimeRouteClass returns the route_class value for a row: one of the
// constants above for ADVERT rows and nil (JSON null) for every other payload
// type, so (type, route_class) identifies each row.
func relayAirtimeRouteClass(key relayAirtimeBucketKey) *string {
	if key.payloadType != PayloadADVERT {
		return nil
	}
	var class string
	switch key.advertRoute {
	case relayAirtimeAdvertFlood:
		class = relayAirtimeRouteClassFlood
	case relayAirtimeAdvertZeroHop:
		class = relayAirtimeRouteClassZeroHop
	case relayAirtimeAdvertMixed:
		class = relayAirtimeRouteClassMixed
	default:
		class = relayAirtimeRouteClassLegacy
	}
	return &class
}

// computeRelayAirtimeShare aggregates relay-airtime-share per payload_type,
// separating ADVERT rows by their flood and zero-hop route classes.
//
// The route class is the route_type stored on the transmission, which is the
// route of the first observation the ingestor inserted for that content hash.
// The content hash ignores route bits, so a contact re-shared as a zero-hop
// advert has the same hash as the original flood advert; later observations
// never change the stored route (see cmd/ingestor InsertTransmission).
//
// Returns:
//
//	{
//	  "rows":        [{payload_type, type, route_class, count, count_pct, score, airtime_pct}, ...]
//	                 sorted by airtime_pct desc, where type is the numeric payload type,
//	                 payload_type the display label and route_class "flood" / "zero_hop" /
//	                 "legacy" on ADVERT rows and null otherwise; up to three ADVERT rows
//	                 share type 4, and (type, route_class) identifies each row,
//	  "total_count": int,
//	  "total_score": int64 (nanoseconds of LoRa Time-on-Air × repeater-count, summed across packets),
//	  "window":      window label,
//	  "cached":      false (overwritten by cached wrapper),
//	}
func (s *PacketStore) computeRelayAirtimeShare(window TimeWindow) map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	preset := s.resolveLoRaPreset()

	type bucket struct {
		count int
		score int64 // sum of ToA(payload) × relays, in nanoseconds
	}
	buckets := make(map[relayAirtimeBucketKey]*bucket)
	seenHash := make(map[string]bool, len(s.packets))
	totalCount := 0
	var totalScore int64

	for _, tx := range s.packets {
		if tx == nil || tx.PayloadType == nil {
			continue
		}
		if !window.Includes(tx.FirstSeen) {
			continue
		}
		// Dedup per-hash: each distinct packet counted once. ACKs in the
		// test fixture have unique hashes so this only collapses true
		// re-observations of the same packet.
		if tx.Hash != "" {
			if seenHash[tx.Hash] {
				continue
			}
			seenHash[tx.Hash] = true
		}
		key := relayAirtimeKey(tx)
		b := buckets[key]
		if b == nil {
			b = &bucket{}
			buckets[key] = b
		}
		b.count++
		totalCount++

		// payload bytes from RawHex (2 hex chars per byte). Score is
		// LoRa Time-on-Air (nanoseconds) × distinct relays — see
		// resolveLoRaPreset for the assumed PHY block (issue #1768).
		payloadBytes := len(tx.RawHex) / 2
		relays := s.distinctRelayCount(tx)
		toa := lora.TimeOnAir(payloadBytes, preset)
		score := int64(toa) * int64(relays)
		b.score += score
		totalScore += score
	}

	rows := make([]map[string]interface{}, 0, len(buckets))
	for key, b := range buckets {
		name := relayAirtimeBucketName(key)
		var countPct, airtimePct float64
		if totalCount > 0 {
			countPct = float64(b.count) / float64(totalCount) * 100.0
		}
		if totalScore > 0 {
			airtimePct = float64(b.score) / float64(totalScore) * 100.0
		}
		rows = append(rows, map[string]interface{}{
			"payload_type": name,
			"type":         key.payloadType,
			"route_class":  relayAirtimeRouteClass(key),
			"count":        b.count,
			"count_pct":    countPct,
			"score":        b.score,
			"airtime_pct":  airtimePct,
		})
	}

	// Sort descending by airtime_pct; tiebreak count desc, then name asc,
	// then numeric type asc for deterministic ordering.
	sort.SliceStable(rows, func(i, j int) bool {
		ai, _ := rows[i]["airtime_pct"].(float64)
		aj, _ := rows[j]["airtime_pct"].(float64)
		if ai != aj {
			return ai > aj
		}
		ci, _ := rows[i]["count"].(int)
		cj, _ := rows[j]["count"].(int)
		if ci != cj {
			return ci > cj
		}
		ni, _ := rows[i]["payload_type"].(string)
		nj, _ := rows[j]["payload_type"].(string)
		if ni != nj {
			return ni < nj
		}
		// Unnamed payload types all share the "UNK" label.
		ti, _ := rows[i]["type"].(int)
		tj, _ := rows[j]["type"].(int)
		return ti < tj
	})

	label := ""
	if !window.IsZero() {
		label = window.Label
	}
	return map[string]interface{}{
		"rows":        rows,
		"total_count": totalCount,
		"total_score": totalScore,
		"preset":      presetJSON(preset),
		"window":      label,
		"cached":      false,
	}
}

// GetRelayAirtimeShareWithWindow is the cached wrapper around
// computeRelayAirtimeShare. Reuses the existing rfCache + rfCacheTTL pool
// (shared with RF / topology / distance analytics — no new cache layer per
// #1359 spec).
func (s *PacketStore) GetRelayAirtimeShareWithWindow(window TimeWindow) map[string]interface{} {
	cacheKey := "relay-airtime-share|"
	if !window.IsZero() {
		cacheKey += window.CacheKey()
	}
	s.cacheMu.Lock()
	if cached, ok := s.rfCache[cacheKey]; ok && time.Now().Before(cached.expiresAt) {
		s.cacheHits++
		s.cacheMu.Unlock()
		// Shallow copy with cached=true so the JSON client can tell.
		m := cached.data
		out := make(map[string]interface{}, len(m)+1)
		for k, v := range m {
			out[k] = v
		}
		out["cached"] = true
		return out
	}
	s.cacheMisses++
	s.cacheMu.Unlock()

	// #89: outside s.mu, and read before the rows so a refresh that lands
	// in between errs toward "backfilling". While the route_mask backfill is
	// not complete, some ADVERT rows are still classified by their legacy
	// first-inserted route.
	var backfill *RouteMaskBackfillStatus
	if s.db != nil {
		st := s.routeMaskBackfillStatus()
		backfill = &st
	}
	result := s.computeRelayAirtimeShare(window)
	if backfill != nil {
		result["route_mask_backfill"] = *backfill
	}

	s.cacheMu.Lock()
	s.rfCache[cacheKey] = &cachedResult{data: result, expiresAt: time.Now().Add(s.rfCacheTTL)}
	s.cacheMu.Unlock()

	return result
}
