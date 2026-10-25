package main

import (
	"context"
	"time"

	"github.com/meshcore-analyzer/packetpath"
)

// Direct-evidence region placement for /api/nodes?region= (#488 M2).
//
// STUB (red commit): types and signatures only.

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
	MaxHops     int
	Lifetime    time.Duration
	PersistSpan time.Duration
}

type regionPlacementSettings struct {
	Mode   string
	Params regionPlacementParams
}

var defaultRegionPlacementParams = regionPlacementParams{
	MaxHops:  3,
	Lifetime: 30 * 24 * time.Hour,
}

type regionPlacement struct {
	Basis     string
	Regions   []string
	Transient []string
	Hops      int
}

type regionEvidenceStats struct {
	LastAt      [regionPlacementHopSlots]int64
	DirectFirst int64
	LegacyAt    int64
}

func (s *regionEvidenceStats) add(ts int64, h packetpath.AdvertHearing) regionHearingKind {
	return hearingUnknown
}

func (s *regionEvidenceStats) merge(o *regionEvidenceStats) {}

type regionHearingKind uint8

const (
	hearingUnknown regionHearingKind = iota
	hearingDirect
	hearingLegacy
	hearingRelayed
	hearingFar
	hearingShared
	hearingKinds
)

type regionHearingCounts [hearingKinds]int64

type regionEvidenceAgg struct {
	nodes     map[string]map[string]*regionEvidenceStats
	pairs     int
	watermark int64
	hearings  regionHearingCounts
}

func newRegionEvidenceAgg() *regionEvidenceAgg {
	return &regionEvidenceAgg{nodes: map[string]map[string]*regionEvidenceStats{}}
}

func (a *regionEvidenceAgg) add(pk, region string, ts int64, h packetpath.AdvertHearing) {}

func (a *regionEvidenceAgg) merge(d *regionEvidenceAgg) {}

func (db *DB) scanRegionEvidence(ctx context.Context, lo, hi int64, delta bool, agg *regionEvidenceAgg) (int, error) {
	return 0, nil
}

func placeNode(perRegion map[string]*regionEvidenceStats, p regionPlacementParams) regionPlacement {
	return regionPlacement{}
}

type regionPlacementSnapshot struct {
	byRegion  map[string][]string
	watermark int64
	refreshed time.Time
	built     time.Time
}

func newRegionPlacementSnapshot(agg *regionEvidenceAgg, p regionPlacementParams, built time.Time) *regionPlacementSnapshot {
	return &regionPlacementSnapshot{}
}

func (s *regionPlacementSnapshot) members(codes []string) map[string]struct{} {
	return map[string]struct{}{}
}

func (db *DB) currentRegionPlacement(ctx context.Context) (*regionPlacementSnapshot, error) {
	return &regionPlacementSnapshot{}, nil
}
