package main

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/meshcore-analyzer/packetpath"
)

// Direct-evidence region placement for /api/nodes?region= (#488 M2).
//
// The heard rule (nodes_region_cache.go) puts a node in every region whose
// observers heard any of its adverts, so a flooded advert drags a distant
// node into every region it reaches. The placed rule decides from what each
// ADVERT observation says about its originator (packetpath.ClassifyAdvert,
// M1):
//
//  1. direct: every region that heard the node's own transmission (hash
//     count 0 on its own frame) is a home region. Ties are a set.
//  2. fewest_hops: with no direct evidence anywhere, the regions that heard
//     it in the fewest hops, up to MaxHops. A legacy row's explicit '[]'
//     (no frame of its own, so a contact re-share cannot be excluded) is a
//     0-hop fallback, never direct evidence (owner decision on #488).
//  3. unknown: in no region filter; still listed unfiltered.
//
// Evidence older than Lifetime before the node's own newest hearing is
// ignored, so a quiet node keeps the placement its last active period
// earned and a node that moved drops its old region. With PersistSpan set,
// direct regions whose evidence spans less than it are dropped when another
// region's spans at least it (one-off long-distance reception).
//
// Placement needs every region's evidence for a node at once (direct
// evidence anywhere decides whether the fallback applies here), so it is
// computed from ONE global aggregate rather than per region set: the
// region index in region_index.go, which the heard rule shares since #618
// (watermark, delta scans, periodic full rebuild, singleflight, warmer).
// Placement is not monotone — a new direct hearing removes fallback
// regions — so every refresh re-derives the placements from the aggregate
// instead of extending them; the heard rule's do the same, which costs one
// pass over the pairs.
//
// Everything is read from SQLite on the server's read-only handle; nothing
// touches the PacketStore or its lock.

// regionPlacementHopSlots bounds the per-pair state: hop counts
// 1..regionPlacementHopSlots-1 keep their newest timestamp. Higher counts
// can never place a node (MaxHops is capped below this) and are only
// counted.
const regionPlacementHopSlots = 8

const (
	regionModeHeard  = "heard"
	regionModePlaced = "placed"
)

const (
	placementBasisDirect  = "direct"
	placementBasisFewest  = "fewest_hops"
	placementBasisUnknown = "unknown"
)

type regionPlacementParams struct {
	// MaxHops caps the relayed fallback (0 = none).
	MaxHops int
	// Lifetime is the evidence window before the node's own newest
	// hearing; 0 = no window.
	Lifetime time.Duration
	// PersistSpan turns on the transient-region rule; 0 = off.
	PersistSpan time.Duration
}

type regionPlacementSettings struct {
	Mode   string
	Params regionPlacementParams
}

// defaultRegionPlacementParams: MaxHops 3 is a working default to be
// checked with the dry-run on a real instance; 30 days matches the default
// retention.packetDays; the transient rule stays off (deferred on #488).
var defaultRegionPlacementParams = regionPlacementParams{
	MaxHops:  3,
	Lifetime: 30 * 24 * time.Hour,
}

// regionPlacement says where a node is placed and on what basis.
type regionPlacement struct {
	Basis string
	// Regions are the inferred home regions, sorted; empty for unknown.
	Regions []string
	// Transient are direct regions outranked by persistent direct
	// evidence elsewhere (PersistSpan only).
	Transient []string
	// Hops is 0 for direct, the fallback hop count for fewest_hops (0 for
	// a legacy '[]'), -1 for unknown.
	Hops int
}

// regionEvidenceStats is the bounded evidence one region holds about one
// node. Every field merges by min/max, so folding a delta scan into a full
// build equals a full build over the union.
type regionEvidenceStats struct {
	// LastAt[0] is the newest verified direct hearing, LastAt[h] the newest
	// hearing relayed h times; 0 = never.
	LastAt [regionPlacementHopSlots]int64
	// DirectFirst is the oldest verified direct hearing; 0 = never.
	DirectFirst int64
	// LegacyAt is the newest explicit '[]' on a row without a frame.
	LegacyAt int64
}

// regionHearingKind is how one observation was folded into the aggregate.
type regionHearingKind uint8

const (
	hearingUnknown regionHearingKind = iota // no usable hop count, or undated
	hearingDirect                           // verified, own transmission
	hearingLegacy                           // '[]' without a frame: fallback only
	hearingRelayed                          // relayed, within the stored slots
	hearingFar                              // relayed past the stored slots
	hearingShared                           // a contact re-share: no evidence
	hearingKinds
)

// regionHearingCounts counts folded observations per kind, for the dry-run.
type regionHearingCounts [hearingKinds]int64

// add folds one hearing at unix time ts and reports how it was counted.
func (s *regionEvidenceStats) add(ts int64, h packetpath.AdvertHearing) regionHearingKind {
	if ts <= 0 {
		return hearingUnknown
	}
	switch h.Kind {
	case packetpath.AdvertDirect:
		if !h.Verified {
			s.LegacyAt = max(s.LegacyAt, ts)
			return hearingLegacy
		}
		s.LastAt[0] = max(s.LastAt[0], ts)
		s.DirectFirst = minNonZero(s.DirectFirst, ts)
		return hearingDirect
	case packetpath.AdvertRelayed:
		switch {
		case h.Hops < 1:
			return hearingUnknown // the classifiers never yield this; keep slot 0 direct-only
		case h.Hops < regionPlacementHopSlots:
			s.LastAt[h.Hops] = max(s.LastAt[h.Hops], ts)
			return hearingRelayed
		}
		return hearingFar
	case packetpath.AdvertShared:
		return hearingShared
	}
	return hearingUnknown
}

// merge folds o into s.
func (s *regionEvidenceStats) merge(o *regionEvidenceStats) {
	for i, ts := range o.LastAt {
		s.LastAt[i] = max(s.LastAt[i], ts)
	}
	s.DirectFirst = minNonZero(s.DirectFirst, o.DirectFirst)
	s.LegacyAt = max(s.LegacyAt, o.LegacyAt)
}

// newest is the newest dated hearing that can place a node.
func (s *regionEvidenceStats) newest() int64 {
	n := s.LegacyAt
	for _, ts := range s.LastAt {
		n = max(n, ts)
	}
	return n
}

// minNonZero is the smaller of a and b, where 0 means "none".
func minNonZero(a, b int64) int64 {
	if a == 0 || (b != 0 && b < a) {
		return b
	}
	return a
}

// regionEvidenceAgg is the global evidence table: one entry per (node,
// region) pair that heard the node's adverts, keyed by node. Its size is
// bounded by the nodes in the retained history times the regions each was
// heard in; repeated hearings only update the fixed-size stats.
type regionEvidenceAgg struct {
	nodes     map[string]map[string]*regionEvidenceStats
	pairs     int
	watermark int64 // max observations.id folded in
	hearings  regionHearingCounts
}

func newRegionEvidenceAgg() *regionEvidenceAgg {
	return &regionEvidenceAgg{nodes: make(map[string]map[string]*regionEvidenceStats)}
}

// pair returns the stats for (pk, region), creating them.
func (a *regionEvidenceAgg) pair(pk, region string) *regionEvidenceStats {
	m := a.nodes[pk]
	if m == nil {
		m = make(map[string]*regionEvidenceStats, 2)
		a.nodes[pk] = m
	}
	s := m[region]
	if s == nil {
		s = &regionEvidenceStats{}
		m[region] = s
		a.pairs++
	}
	return s
}

func (a *regionEvidenceAgg) add(pk, region string, ts int64, h packetpath.AdvertHearing) {
	a.hearings[a.pair(pk, region).add(ts, h)]++
}

// merge folds the delta d (scanned past a's watermark) into a.
func (a *regionEvidenceAgg) merge(d *regionEvidenceAgg) {
	for pk, per := range d.nodes {
		for region, s := range per {
			a.pair(pk, region).merge(s)
		}
	}
	for i, n := range d.hearings {
		a.hearings[i] += n
	}
	a.watermark = max(a.watermark, d.watermark)
}

// scanRegionEvidence folds every ADVERT observation with lo < id <= hi,
// heard by an observer with a region, into agg, and raises agg's watermark
// to hi. Same scope as scanHeardRegions (so a placed region is always a
// heard one), plus the per-observation frame and path the classifier needs.
// A delta scan forces the join order to drive from the rowid range, as
// scanHeardRegions explains.
func (db *DB) scanRegionEvidence(ctx context.Context, lo, hi int64, delta bool, agg *regionEvidenceAgg) (int, error) {
	joinCond, tsCol := "obs.rowid = o.observer_idx", "o.timestamp"
	if !db.isV3() {
		// The legacy schema stores ISO-8601 text timestamps.
		joinCond = "obs.id = o.observer_id"
		tsCol = "CASE WHEN typeof(o.timestamp) = 'integer' THEN o.timestamp ELSE CAST(strftime('%s', o.timestamp) AS INTEGER) END"
	}
	join := "JOIN"
	if delta {
		join = "CROSS JOIN"
	}
	rawCol := "NULL"
	if db.hasObsRawHex() {
		rawCol = "o.raw_hex"
	}
	// Only fixed fragments are formatted in; every value is bound. See
	// scanHeardRegions for the from_pubkey contract.
	q := fmt.Sprintf(`SELECT t.from_pubkey, UPPER(TRIM(obs.iata)), %[3]s, o.path_json, %[4]s
		FROM observations o
		%[1]s transmissions t ON t.id = o.transmission_id
		%[1]s observers obs ON %[2]s
		WHERE t.payload_type = ?
		AND t.from_pubkey IS NOT NULL
		AND obs.iata IS NOT NULL AND TRIM(obs.iata) != ''
		AND o.id > ? AND o.id <= ?`, join, joinCond, tsCol, rawCol)
	rows, err := db.conn.QueryContext(ctx, q, payloadTypeAdvert, lo, hi)
	if err != nil {
		return 0, fmt.Errorf("scan region evidence: %w", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var pk, region string
		var ts sql.NullInt64
		var path, raw sql.NullString
		if err := rows.Scan(&pk, &region, &ts, &path, &raw); err != nil {
			return n, fmt.Errorf("scan region evidence: %w", err)
		}
		agg.add(pk, region, ts.Int64, packetpath.ClassifyAdvert(raw.String, path.String))
		n++
	}
	if err := rows.Err(); err != nil {
		return n, fmt.Errorf("scan region evidence: %w", err)
	}
	agg.watermark = max(agg.watermark, hi)
	return n, nil
}

// placeNode applies the rule to one node's per-region evidence.
func placeNode(perRegion map[string]*regionEvidenceStats, p regionPlacementParams) regionPlacement {
	unknown := regionPlacement{Basis: placementBasisUnknown, Hops: -1}
	var nodeLast int64
	for _, s := range perRegion {
		nodeLast = max(nodeLast, s.newest())
	}
	if nodeLast == 0 {
		return unknown
	}
	ws := int64(math.MinInt64)
	if p.Lifetime > 0 {
		ws = nodeLast - int64(p.Lifetime/time.Second)
	}
	within := func(ts int64) bool { return ts != 0 && ts >= ws }

	var direct, persistent, transient []string
	span := int64(p.PersistSpan / time.Second)
	for r, s := range perRegion {
		if !within(s.LastAt[0]) {
			continue
		}
		direct = append(direct, r)
		if span > 0 {
			if s.LastAt[0]-max(s.DirectFirst, ws) >= span {
				persistent = append(persistent, r)
			} else {
				transient = append(transient, r)
			}
		}
	}
	if len(direct) > 0 {
		out := regionPlacement{Basis: placementBasisDirect, Regions: direct}
		if len(persistent) > 0 && len(transient) > 0 {
			out.Regions, out.Transient = persistent, transient
			sort.Strings(out.Transient)
		}
		sort.Strings(out.Regions)
		return out
	}
	// h == 0 is the legacy '[]' tier, so MaxHops 0 still lets it place.
	for h := 0; h == 0 || h <= p.MaxHops && h < regionPlacementHopSlots; h++ {
		var tied []string
		for r, s := range perRegion {
			ts := s.LastAt[h]
			if h == 0 {
				ts = s.LegacyAt // slot 0 is verified direct, handled above
			}
			if within(ts) {
				tied = append(tied, r)
			}
		}
		if len(tied) > 0 {
			sort.Strings(tied)
			return regionPlacement{Basis: placementBasisFewest, Regions: tied, Hops: h}
		}
	}
	return unknown
}
