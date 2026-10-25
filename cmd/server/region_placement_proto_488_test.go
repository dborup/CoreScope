package main

// Research prototype for #488: direct-evidence-first regional placement.
//
// TEST-ONLY. Nothing in this file is wired into the server; it exists so the
// analysis posted on #488 is backed by executable definitions, and so the
// measurement in region_placement_measure_488_test.go can compare the real
// membership cache (nodes_region_cache.go) with one concrete candidate.
//
// Protocol facts the classifier relies on (firmware/src, firmware/docs):
//
//   - header bits 0-1 are the route type; TRANSPORT_FLOOD (0) and
//     TRANSPORT_DIRECT (3) carry 4 bytes of transport codes BEFORE the path
//     byte (packet_format.md, Packet::readFrom). Reading the path byte at
//     offset 1 for TRANSPORT_DIRECT, as computeNodeHomeRegions does, reads
//     the first transport-code byte instead.
//   - the path byte packs hash size - 1 (bits 6-7, 0b11 reserved) and the
//     hash count (bits 0-5); hash_count*hash_size <= MAX_PATH_SIZE (64)
//     (Packet::isValidPathLen).
//   - an ADVERT is originated by sendFlood/sendFloodScoped (FLOOD or
//     TRANSPORT_FLOOD, hash count 0, each relay appends one hash in
//     routeRecvPacket) or by sendZeroHop (DIRECT, path_len 0). Nothing
//     originates a DIRECT advert with a path, and a DIRECT path is the hops
//     still to go (removeSelfFromPath), not the hops travelled.
//   - BaseChatMesh::shareContactZeroHop re-sends ANOTHER node's stored advert
//     as TRANSPORT_DIRECT, path_len 0, transport codes {0,0} ("send this
//     nowhere"). It is heard zero-hop from the sharing companion, not from
//     the advertised node, so it is no evidence of where that node is.
//   - ComputeContentHash hashes the payload type and payload, not the route
//     bits, so such a re-share lands as one more observation of the
//     original transmission: only the observation's own raw_hex tells it
//     apart from a real zero-hop hearing; transmissions.route_type and
//     path_json ('[]') do not.

import (
	"context"
	"database/sql"
	"time"
)

// rp488Kind is what one ADVERT observation says about where its originator is.
type rp488Kind uint8

const (
	// rp488Unknown: no usable hop count (NULL/empty path_json without raw
	// bytes, undecodable raw, reserved hash size, or a DIRECT route with a
	// path, whose length is not the hops travelled).
	rp488Unknown rp488Kind = iota
	// rp488Direct: heard from the originator's own transmission (hash count 0
	// on FLOOD, TRANSPORT_FLOOD or DIRECT).
	rp488Direct
	// rp488Relayed: a flood advert heard after Hops relays.
	rp488Relayed
	// rp488Shared: a contact re-shared zero-hop by a companion.
	rp488Shared
)

func (k rp488Kind) String() string {
	switch k {
	case rp488Direct:
		return "direct"
	case rp488Relayed:
		return "relayed"
	case rp488Shared:
		return "shared"
	}
	return "unknown"
}

type rp488Evidence struct {
	Kind rp488Kind
	Hops int
	// Verified is true when the hop count came from this observation's own
	// raw bytes, false when only path_json was available (a legacy row: a
	// '[]' there cannot be told apart from a re-shared contact).
	Verified bool
}

// rp488ClassifyAdvert classifies one ADVERT observation, preferring the
// observation's own raw bytes over path_json.
func rp488ClassifyAdvert(obsRaw, pathJSON sql.NullString) rp488Evidence {
	return rp488Evidence{} // stub: red commit
}

func rp488ClassifyRaw(rawHex string) rp488Evidence {
	return rp488Evidence{} // stub: red commit
}

func rp488ClassifyPathJSON(p sql.NullString) rp488Evidence {
	return rp488Evidence{} // stub: red commit
}

// rp488HopSlots bounds the per-pair state: hop counts 0..rp488HopSlots-1 keep
// their newest timestamp, higher counts can never place a node (the cap is
// below this) and are only counted.
const rp488HopSlots = 8

// rp488Stats is the bounded evidence one region holds about one node. Every
// field merges by min/max/sum, so a delta scan past a watermark folded into
// a full build equals a full build over the union (append-only input).
type rp488Stats struct {
	LastAt      [rp488HopSlots]int64 // newest unix ts per hop count (0 = direct); 0 = never
	DirectFirst int64                // oldest direct hearing; 0 = never
	Far         int                  // relayed hearings at >= rp488HopSlots hops
	Shared      int
	Unknown     int
}

func (s *rp488Stats) add(ts int64, ev rp488Evidence) {
	// stub: red commit
}

// newest is the newest hearing with a usable hop count.
func (s *rp488Stats) newest() int64 {
	return 0 // stub: red commit
}

// rp488Agg is the global evidence table: one entry per (node, region) pair
// that ever heard the node's adverts, keyed by node so placing a node sees
// all of its regions at once (direct evidence anywhere decides whether the
// fallback applies here, which a per-region-set scan cannot know).
type rp488Agg struct {
	Nodes     map[string]map[string]*rp488Stats
	Watermark int64
	Pairs     int
}

func newRP488Agg() *rp488Agg {
	return &rp488Agg{Nodes: make(map[string]map[string]*rp488Stats)}
}

func (a *rp488Agg) add(pk, region string, ts int64, ev rp488Evidence) {
	m := a.Nodes[pk]
	if m == nil {
		m = make(map[string]*rp488Stats, 2)
		a.Nodes[pk] = m
	}
	s := m[region]
	if s == nil {
		s = &rp488Stats{}
		m[region] = s
		a.Pairs++
	}
	s.add(ts, ev)
}

// rp488ScanAgg folds every ADVERT observation with lo < id <= hi, heard by an
// observer with a region, into agg. Same joins as scanNodeRegionKeys, plus
// the per-observation path and raw bytes the classifier needs.
func rp488ScanAgg(ctx context.Context, db *DB, lo, hi int64, delta bool, agg *rp488Agg) (int, error) {
	agg.Watermark = max(agg.Watermark, hi) // stub: red commit
	return 0, nil
}

type rp488Params struct {
	// Lifetime is the evidence window, anchored at the node's own newest
	// hearing (not at now), so a quiet node keeps the placement its last
	// active period earned instead of decaying to unknown.
	Lifetime time.Duration
	// MaxHops caps the fewest-hops fallback (< rp488HopSlots).
	MaxHops int
	// PersistSpan is how long a region's direct evidence must span (first to
	// last hearing inside the window) to outrank a region whose direct
	// evidence does not. It is a comparison, never a reason on its own to
	// leave a node unplaced.
	PersistSpan time.Duration
}

var rp488DefaultParams = rp488Params{
	Lifetime:    30 * 24 * time.Hour,
	MaxHops:     3,
	PersistSpan: 24 * time.Hour,
}

const (
	rp488BasisDirect  = "direct"
	rp488BasisFewest  = "fewest_hops"
	rp488BasisUnknown = "unknown"
)

// rp488Placement says where a node is placed and on what basis.
type rp488Placement struct {
	Basis string
	// Regions are the inferred home regions (sorted); empty for unknown.
	Regions []string
	// Transient are regions that heard the node directly but were outranked
	// by persistent direct evidence elsewhere (one-off long-distance
	// reception). They stay reception regions, not home regions.
	Transient []string
	// Hops is 0 for direct, the fallback hop count for fewest_hops, -1 for
	// unknown.
	Hops int
}

// rp488Place applies the rule: direct evidence first (persistent direct
// regions outrank transient ones), then the fewest-hops fallback up to
// MaxHops (all tied regions), else unknown.
func rp488Place(perRegion map[string]*rp488Stats, p rp488Params) rp488Placement {
	return rp488Placement{} // stub: red commit
}

// rp488Placements places every node in agg.
func rp488Placements(agg *rp488Agg, p rp488Params) map[string]rp488Placement {
	out := make(map[string]rp488Placement, len(agg.Nodes))
	for pk, per := range agg.Nodes {
		out[pk] = rp488Place(per, p)
	}
	return out
}

// rp488Members returns the sorted pubkeys placed in any of codes: what
// /api/nodes?region= would return under the candidate rule.
func rp488Members(pl map[string]rp488Placement, codes []string) []string {
	return nil // stub: red commit
}
