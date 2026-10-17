package main

import (
	"context"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/meshcore-analyzer/regions"
)

const (
	regionsHistoryBucketCount = 21 * 24
	maxRegionsHistoryCells    = 250000
)

// Published with the existing relay snapshot. These slices are immutable and
// contain only confirmed scoped relay evidence, never raw packet contents.
type regionsRelayHistory struct {
	start, end       time.Time
	nodes            map[string][]RegionsHistoryScope
	truncated        bool
	scopesTruncated  bool
	bucketsTruncated bool
}

type RegionsHistoryCoverage struct {
	Start              *string `json:"start"`
	Loading            bool    `json:"loading"`
	EvidenceIncomplete bool    `json:"evidenceIncomplete"`
}

type RegionsHistoryBucket struct {
	Index       int    `json:"index"`
	Count       int    `json:"count"`
	LastRelayed string `json:"lastRelayed"`
}

type RegionsHistoryScope struct {
	Name    string                 `json:"name"`
	Buckets []RegionsHistoryBucket `json:"buckets"`
}

type RegionsHistoryNode struct {
	PublicKey      string                `json:"publicKey"`
	Name           string                `json:"name"`
	Role           string                `json:"role"`
	Lat            *float64              `json:"lat"`
	Lon            *float64              `json:"lon"`
	PositionSource string                `json:"positionSource"`
	MetadataSource string                `json:"metadataSource"`
	Scopes         []RegionsHistoryScope `json:"scopes"`
}

type RegionsHistoryResponse struct {
	GeneratedAt      string                 `json:"generatedAt"`
	HistoryStart     string                 `json:"historyStart"`
	HistoryEnd       string                 `json:"historyEnd"`
	BucketSeconds    int                    `json:"bucketSeconds"`
	BucketCount      int                    `json:"bucketCount"`
	Coverage         RegionsHistoryCoverage `json:"coverage"`
	Nodes            []RegionsHistoryNode   `json:"nodes"`
	Truncated        bool                   `json:"truncated"`
	ScopesTruncated  bool                   `json:"scopesTruncated"`
	BucketsTruncated bool                   `json:"bucketsTruncated"`
}

type regionsHistoryBuilder struct {
	history        *regionsRelayHistory
	remainingCells int
}

func newRegionsHistoryBuilder(now time.Time) *regionsHistoryBuilder {
	end := now.UTC().Truncate(time.Hour)
	return &regionsHistoryBuilder{history: &regionsRelayHistory{
		start: end.Add(-regionsHistoryBucketCount * time.Hour), end: end,
		nodes: make(map[string][]RegionsHistoryScope),
	}, remainingCells: maxRegionsHistoryCells}
}

func historyRelayTime(e relayEntry, start, end time.Time) (time.Time, bool) {
	if e.fromPrefix || e.pt == payloadTypeAdvert || regions.ValidateCandidate(e.scope) != nil {
		return time.Time{}, false
	}
	t, valid := e.t, e.valid
	if !e.parsed {
		t, valid = parseRelayTS(e.ts)
	}
	return t, valid && !t.Before(start) && t.Before(end)
}

// add receives deduplicated own-observation memberships from the strict
// Regions collector, after the packet lock has been released. Callers
// visit keys in sorted order so the shared cell budget is deterministic.
func (b *regionsHistoryBuilder) add(key string, entries []relayEntry) {
	if len(key) != 64 {
		return
	}
	h := b.history
	// Select the 32 newest valid scopes with bounded scratch space, even if a
	// sender introduces arbitrarily many names in the retained packet store.
	latest := make(map[string]time.Time, maxTransportedScopes)
	for _, e := range entries {
		t, valid := historyRelayTime(e, h.start, h.end)
		if !valid {
			continue
		}
		h.scopesTruncated = retainLatestRegionScope(latest, e.scope, t) || h.scopesTruncated
	}
	if len(latest) == 0 {
		return
	}
	if len(h.nodes) >= maxObservedRegionNodes {
		h.truncated = true
		return
	}
	if b.remainingCells == 0 {
		h.bucketsTruncated = true
		return
	}
	// Scratch aggregation is bounded by 32 scopes x 504 hours. Only after
	// sorting scopes and indexes do we spend the global publication budget,
	// so its selection never depends on packet or map iteration order.
	type historyCell struct {
		count  int
		latest time.Time
	}
	cells := make(map[string]map[int]historyCell, len(latest))
	for _, e := range entries {
		t, valid := historyRelayTime(e, h.start, h.end)
		if !valid {
			continue
		}
		if _, selected := latest[e.scope]; !selected {
			continue
		}
		index := int(t.Sub(h.start) / time.Hour)
		byIndex := cells[e.scope]
		bucket, exists := byIndex[index]
		if !exists {
			if byIndex == nil {
				byIndex = make(map[int]historyCell)
				cells[e.scope] = byIndex
			}
		}
		bucket.count++
		if t.After(bucket.latest) {
			bucket.latest = t
		}
		byIndex[index] = bucket
	}
	scopes := make([]RegionsHistoryScope, 0, len(cells))
	names := make([]string, 0, len(cells))
	for name := range cells {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		byIndex := cells[name]
		indexes := make([]int, 0, len(byIndex))
		for index := range byIndex {
			indexes = append(indexes, index)
		}
		sort.Ints(indexes)
		retain := len(indexes)
		if retain > b.remainingCells {
			retain = b.remainingCells
			h.bucketsTruncated = true
		}
		if retain == 0 {
			continue
		}
		buckets := make([]RegionsHistoryBucket, 0, retain)
		for _, index := range indexes[:retain] {
			cell := byIndex[index]
			buckets = append(buckets, RegionsHistoryBucket{Index: index, Count: cell.count, LastRelayed: cell.latest.UTC().Format(time.RFC3339Nano)})
		}
		b.remainingCells -= retain
		scopes = append(scopes, RegionsHistoryScope{Name: name, Buckets: buckets})
	}
	if len(scopes) > 0 {
		h.nodes[key] = scopes
	}
}

func (s *Server) handleRegionsHistory(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.db == nil || s.db.conn == nil {
		writeError(w, http.StatusServiceUnavailable, "observed relay data unavailable")
		return
	}
	s.store.repeaterEnrichMu.Lock()
	snapshot := s.store.repeaterRelaySnapshot
	s.store.repeaterEnrichMu.Unlock()
	if snapshot == nil || snapshot.history == nil {
		writeError(w, http.StatusServiceUnavailable, "observed relay history is warming up")
		return
	}
	response, err := s.regionsHistoryResponse(r.Context(), snapshot)
	if err != nil {
		log.Printf("[regions-history] node metadata: %v", err)
		writeError(w, http.StatusInternalServerError, "could not read region node metadata")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, response)
}

func (s *Server) regionsHistoryResponse(ctx context.Context, snapshot *repeaterRelaySnapshot) (RegionsHistoryResponse, error) {
	response, _, err := s.regionsHistoryAndStepNodes(ctx, snapshot, nil)
	return response, err
}

func (s *Server) regionsHistoryAndStepNodes(ctx context.Context, snapshot *repeaterRelaySnapshot, routeKeys map[string]bool) (RegionsHistoryResponse, []RegionsRouteNode, error) {
	h := snapshot.history
	response := RegionsHistoryResponse{GeneratedAt: snapshot.generatedAt.Format(time.RFC3339Nano),
		HistoryStart: h.start.Format(time.RFC3339Nano), HistoryEnd: h.end.Format(time.RFC3339Nano),
		BucketSeconds: 3600, BucketCount: regionsHistoryBucketCount,
		Coverage: RegionsHistoryCoverage{Loading: snapshot.loading, EvidenceIncomplete: snapshot.regionMemberships != nil && snapshot.regionMemberships.incomplete}, Nodes: []RegionsHistoryNode{},
		Truncated: h.truncated, ScopesTruncated: h.scopesTruncated, BucketsTruncated: h.bucketsTruncated}
	if !snapshot.coverageStart.IsZero() && !snapshot.coverageStart.After(snapshot.generatedAt) {
		start := snapshot.coverageStart.Format(time.RFC3339Nano)
		response.Coverage.Start = &start
	}
	keys := make([]string, 0, len(h.nodes)+len(routeKeys))
	for key := range h.nodes {
		keys = append(keys, key)
	}
	for key := range routeKeys {
		if _, exists := h.nodes[key]; !exists {
			keys = append(keys, key)
		}
	}
	stepNodes := []RegionsRouteNode{}
	err := s.forEachVisibleRegionNode(ctx, keys, true, func(n observedRegionNodeMetadata) bool {
		if routeKeys[n.publicKey] {
			stepNodes = append(stepNodes, regionRouteNodeMetadata(n))
		}
		scopes := h.nodes[n.publicKey]
		if len(scopes) == 0 {
			return true
		}
		if len(response.Nodes) >= maxObservedRegionNodes {
			response.Truncated = true
			return len(routeKeys) > 0
		}
		node := RegionsHistoryNode{PublicKey: n.publicKey, Name: n.name, Role: n.role,
			PositionSource: "none", MetadataSource: n.source, Scopes: scopes}
		if node.Name == "" {
			node.Name = n.publicKey
		}
		if reportedRegionPosition(n.lat, n.lon) {
			lat, lon := n.lat.Float64, n.lon.Float64
			node.Lat, node.Lon, node.PositionSource = &lat, &lon, "reported"
		}
		response.Nodes = append(response.Nodes, node)
		return true
	})
	return response, stepNodes, err
}

func regionsHistorySchemas() map[string]*openAPISchema {
	return map[string]*openAPISchema{
		"RegionsHistoryCoverage": {Type: "object", Properties: map[string]*openAPISchema{
			"start":              {Type: "string", Nullable: true, Description: "Conservative retained-history floor, paired with this snapshot; null when unknown."},
			"loading":            {Type: "boolean"},
			"evidenceIncomplete": {Type: "boolean", Description: "Snapshot-wide own-observation attribution is incomplete due to missing/mismatched wire evidence, unresolved identities, or bounded source/membership capture. Independent of retained time coverage and directed-step limits."},
		}},
		"RegionsHistoryBucket": {Type: "object", Properties: map[string]*openAPISchema{
			"index":       {Type: "integer", Description: "Zero-based hourly bucket index, 0 through 503."},
			"count":       {Type: "integer", Description: "Distinct confirmed transmissions for this node and scope. Summing nodes counts relay appearances, not globally unique transmissions."},
			"lastRelayed": {Type: "string", Description: "Latest transmission FirstSeen in this bucket (RFC3339)."},
		}},
		"RegionsHistoryScope": {Type: "object", Properties: map[string]*openAPISchema{
			"name": {Type: "string"}, "buckets": {Type: "array", Items: openAPIRef("RegionsHistoryBucket"), Description: "Only populated buckets, ascending by index."},
		}},
		"RegionsHistoryNode": {Type: "object", Properties: map[string]*openAPISchema{
			"publicKey": {Type: "string"}, "name": {Type: "string"}, "role": {Type: "string", Enum: []string{"repeater", "room"}},
			"lat": {Type: "number", Nullable: true}, "lon": {Type: "number", Nullable: true},
			"positionSource": {Type: "string", Enum: []string{"reported", "none"}},
			"metadataSource": {Type: "string", Enum: []string{"current", "archived"}, Description: "Current metadata when a nodes row exists; otherwise inactive_nodes metadata. Neither represents a historical position sample."},
			"scopes":         {Type: "array", Items: openAPIRef("RegionsHistoryScope")},
		}},
		"RegionsHistoryResponse": {Type: "object", Properties: map[string]*openAPISchema{
			"generatedAt": {Type: "string"}, "historyStart": {Type: "string"}, "historyEnd": {Type: "string", Description: "UTC completed-hour boundary at or before generatedAt. Current unfinished hour is excluded."},
			"bucketSeconds": {Type: "integer", Description: "3600."}, "bucketCount": {Type: "integer", Description: "504 (21 days)."},
			"coverage": openAPIRef("RegionsHistoryCoverage"), "nodes": {Type: "array", Items: openAPIRef("RegionsHistoryNode")},
			"truncated": {Type: "boolean"}, "scopesTruncated": {Type: "boolean"}, "bucketsTruncated": {Type: "boolean"},
		}},
	}
}
