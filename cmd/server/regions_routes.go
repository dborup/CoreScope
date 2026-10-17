package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/meshcore-analyzer/regions"
)

const (
	maxRegionsRouteSteps      = 5000
	maxRegionsRouteCells      = 100000
	maxRegionsRoutePaths      = 250000
	maxRegionsRoutePathsPerTx = 128
	maxRegionsRoutePairsPerTx = 8192
	maxRegionsRoutePathBytes  = 8192
)

// Cache only the observation's own header/transport evidence. Full raw frames
// remain discarded. The two added fields fit StoreObs's existing padding.
func cacheObservationWireEvidence(o *StoreObs, raw string) {
	o.wireRoute = wireRouteFromRawHex(raw)
	o.wireTransport, o.transportKnown = wireTransportCodes(raw)
}

func wireTransportCodes(raw string) (uint32, bool) {
	route := wireRouteFromRawHex(raw)
	if (route != 1 && route != 4) || len(raw) < 10 {
		return 0, false
	}
	value, err := strconv.ParseUint(raw[2:10], 16, 32)
	for _, c := range raw[2:10] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return 0, false
		}
	}
	return uint32(value), err == nil
}

type RegionsRouteStep struct {
	From    string                 `json:"from"`
	To      string                 `json:"to"`
	Scope   string                 `json:"scope"`
	Buckets []RegionsHistoryBucket `json:"buckets"`
}

type RegionsStepEvidence struct {
	SkippedObservations  int  `json:"skippedObservations"`
	UnresolvedHops       int  `json:"unresolvedHops"`
	SourceTruncated      bool `json:"sourceTruncated"`
	Incomplete           bool `json:"incomplete"`
	MissingEndpointSteps int  `json:"missingEndpointSteps"`
}

type RegionsRouteNode struct {
	PublicKey      string   `json:"publicKey"`
	Name           string   `json:"name"`
	Role           string   `json:"role"`
	Lat            *float64 `json:"lat"`
	Lon            *float64 `json:"lon"`
	PositionSource string   `json:"positionSource"`
	MetadataSource string   `json:"metadataSource"`
}

func regionRouteNodeMetadata(n observedRegionNodeMetadata) RegionsRouteNode {
	node := RegionsRouteNode{PublicKey: n.publicKey, Name: n.name, Role: n.role, PositionSource: "none", MetadataSource: n.source}
	if node.Name == "" {
		node.Name = n.publicKey
	}
	if reportedRegionPosition(n.lat, n.lon) {
		lat, lon := n.lat.Float64, n.lon.Float64
		node.Lat, node.Lon, node.PositionSource = &lat, &lon, "reported"
	}
	return node
}

type RegionsRoutesResponse struct {
	RegionsHistoryResponse
	Steps                []RegionsRouteStep  `json:"steps"`
	StepNodes            []RegionsRouteNode  `json:"stepNodes"`
	StepsTruncated       bool                `json:"stepsTruncated"`
	StepBucketsTruncated bool                `json:"stepBucketsTruncated"`
	StepEvidence         RegionsStepEvidence `json:"stepEvidence"`
}

type regionsRouteSnapshot struct {
	steps                            []RegionsRouteStep
	stepsTruncated, bucketsTruncated bool
	evidence                         RegionsStepEvidence
	memberships                      *regionsMembershipSnapshot
	history                          *regionsRelayHistory
}

type regionsRoutePath struct {
	path         string
	observations int
}
type regionsRouteTx struct {
	id    int
	when  time.Time
	scope string
	paths []regionsRoutePath
}
type regionsRoutesCollector struct {
	start, end time.Time
	now        time.Time
	txs        []regionsRouteTx
	pathCount  int
	evidence   RegionsStepEvidence
}

func newRegionsRoutesCollector(now time.Time) *regionsRoutesCollector {
	end := now.UTC().Truncate(time.Hour)
	return &regionsRoutesCollector{start: end.Add(-regionsHistoryBucketCount * time.Hour), end: end, now: now.UTC()}
}

// capture copies immutable strings and scalar wire evidence while the packet
// lock is held. Path parsing, identity resolution and aggregation run later.
// It is called once per distinct transmission by the shared relay collector.
func (c *regionsRoutesCollector) capture(tx *StoreTx) {
	when, valid := parseRelayTS(tx.FirstSeen)
	c.captureAt(tx, when, valid)
}

func (c *regionsRoutesCollector) captureAt(tx *StoreTx, when time.Time, valid bool) {
	if tx.PayloadType == nil || *tx.PayloadType == payloadTypeAdvert || *tx.PayloadType == 9 || regions.ValidateCandidate(tx.ScopeName) != nil {
		return
	}
	if !valid || when.Before(c.start) || when.After(c.now) {
		return
	}
	canonical, known := wireTransportCodes(tx.RawHex)
	known = known && canonical>>16 != 0
	item := regionsRouteTx{id: tx.ID, when: when, scope: tx.ScopeName}
	seen := make(map[string]int)
	for _, obs := range tx.Observations {
		if obs == nil {
			c.evidence.SkippedObservations++
			continue
		}
		eligible, incomplete := eligibleRegionsObservation(obs, canonical, known)
		if incomplete {
			c.evidence.SkippedObservations++
			continue
		}
		if !eligible {
			continue
		}
		if len(obs.PathJSON) > maxRegionsRoutePathBytes {
			c.evidence.SkippedObservations++
			continue
		}
		if index, exists := seen[obs.PathJSON]; exists {
			item.paths[index].observations++
			continue
		}
		if len(item.paths) >= maxRegionsRoutePathsPerTx || c.pathCount >= maxRegionsRoutePaths {
			c.evidence.SourceTruncated = true
			c.evidence.SkippedObservations++
			continue
		}
		seen[obs.PathJSON] = len(item.paths)
		item.paths = append(item.paths, regionsRoutePath{path: obs.PathJSON, observations: 1})
		c.pathCount++
	}
	if len(tx.Observations) == 0 {
		c.evidence.SkippedObservations++
	}
	if len(item.paths) > 0 {
		c.txs = append(c.txs, item)
	}
}

type regionsRouteKey struct{ from, to, scope string }
type regionsRouteCell struct {
	count int
	last  time.Time
}

func (c *regionsRoutesCollector) build(pm *prefixMap) *regionsRouteSnapshot {
	out := &regionsRouteSnapshot{steps: []RegionsRouteStep{}, evidence: c.evidence}
	sort.Slice(c.txs, func(i, j int) bool { return c.txs[i].id < c.txs[j].id })
	resolve := newRelayTokenResolver(pm)
	memberships := newRegionsMembershipBuilder()
	rows := make(map[regionsRouteKey]map[int]regionsRouteCell)
	seen := make(map[regionsRouteKey]bool)
	seenMembers := make(map[string]bool)
	cellCount := 0
	for _, tx := range c.txs {
		clear(seen)
		clear(seenMembers)
		index := int(tx.when.Sub(c.start) / time.Hour)
		for _, path := range tx.paths {
			var tokens []string
			if err := json.Unmarshal([]byte(path.path), &tokens); err != nil || len(tokens) > 64 {
				out.evidence.SkippedObservations += path.observations
				continue
			}
			previous := ""
			for _, token := range tokens {
				if len(resolve.cache) >= 4096 {
					clear(resolve.cache)
				}
				key := resolve.key(token)
				if key == "" {
					previous = ""
					out.evidence.UnresolvedHops++
					continue
				}
				if !seenMembers[key] {
					seenMembers[key] = true
					memberships.add(key, tx)
				}
				// Latest includes the current hour. Completed hourly history and
				// directed steps deliberately do not, even at its exact boundary.
				if index >= regionsHistoryBucketCount {
					continue
				}
				if previous != "" && previous != key {
					step := regionsRouteKey{previous, key, tx.scope}
					if !seen[step] {
						if len(seen) >= maxRegionsRoutePairsPerTx {
							out.stepsTruncated = true
							previous = key
							continue
						}
						seen[step] = true
						byIndex := rows[step]
						if byIndex == nil {
							if len(rows) >= maxRegionsRouteSteps {
								out.stepsTruncated = true
								previous = key
								continue
							}
							byIndex = make(map[int]regionsRouteCell)
						}
						cell, exists := byIndex[index]
						if !exists {
							if cellCount >= maxRegionsRouteCells {
								out.bucketsTruncated = true
								previous = key
								continue
							}
							cellCount++
						}
						cell.count++
						if tx.when.After(cell.last) {
							cell.last = tx.when
						}
						byIndex[index] = cell
						rows[step] = byIndex
					}
				}
				previous = key
			}
		}
	}
	keys := make([]regionsRouteKey, 0, len(rows))
	for key := range rows {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.from != b.from {
			return a.from < b.from
		}
		if a.to != b.to {
			return a.to < b.to
		}
		return a.scope < b.scope
	})
	for _, key := range keys {
		byIndex := rows[key]
		indexes := make([]int, 0, len(byIndex))
		for index := range byIndex {
			indexes = append(indexes, index)
		}
		sort.Ints(indexes)
		step := RegionsRouteStep{From: key.from, To: key.to, Scope: key.scope, Buckets: make([]RegionsHistoryBucket, 0, len(indexes))}
		for _, index := range indexes {
			cell := byIndex[index]
			step.Buckets = append(step.Buckets, RegionsHistoryBucket{Index: index, Count: cell.count, LastRelayed: cell.last.UTC().Format(time.RFC3339Nano)})
		}
		out.steps = append(out.steps, step)
	}
	membershipIncomplete := out.evidence.SkippedObservations > 0 || out.evidence.UnresolvedHops > 0 || out.evidence.SourceTruncated
	out.memberships, out.history = memberships.build(c.now, membershipIncomplete)
	out.evidence.Incomplete = membershipIncomplete || out.stepsTruncated || out.bucketsTruncated
	return out
}

func (s *Server) handleRegionsRoutes(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.db == nil || s.db.conn == nil {
		writeError(w, http.StatusServiceUnavailable, "observed relay data unavailable")
		return
	}
	s.store.repeaterEnrichMu.Lock()
	snapshot := s.store.repeaterRelaySnapshot
	s.store.repeaterEnrichMu.Unlock()
	if snapshot == nil || snapshot.history == nil || snapshot.routes == nil {
		writeError(w, http.StatusServiceUnavailable, "observed relay routes are warming up")
		return
	}
	keys := make(map[string]bool, len(snapshot.routes.steps)*2)
	for _, step := range snapshot.routes.steps {
		keys[step.From] = true
		keys[step.To] = true
	}
	history, stepNodes, err := s.regionsHistoryAndStepNodes(r.Context(), snapshot, keys)
	if err != nil {
		log.Printf("[regions-routes] metadata: %v", err)
		writeError(w, http.StatusInternalServerError, "could not read region node metadata")
		return
	}
	visible := make(map[string]bool, len(stepNodes))
	for _, node := range stepNodes {
		visible[node.PublicKey] = true
	}
	response := RegionsRoutesResponse{RegionsHistoryResponse: history, Steps: []RegionsRouteStep{}, StepNodes: stepNodes, StepsTruncated: snapshot.routes.stepsTruncated, StepBucketsTruncated: snapshot.routes.bucketsTruncated, StepEvidence: snapshot.routes.evidence}
	for _, step := range snapshot.routes.steps {
		if visible[step.From] && visible[step.To] {
			response.Steps = append(response.Steps, step)
		} else {
			response.StepEvidence.MissingEndpointSteps++
			response.StepEvidence.Incomplete = true
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, response)
}

func regionsRoutesSchemas() map[string]*openAPISchema {
	properties := make(map[string]*openAPISchema)
	for key, value := range regionsHistorySchemas()["RegionsHistoryResponse"].Properties {
		properties[key] = value
	}
	properties["steps"] = &openAPISchema{Type: "array", Items: openAPIRef("RegionsRouteStep")}
	properties["stepNodes"] = &openAPISchema{Type: "array", Items: openAPIRef("RegionsRouteNode"), Description: "At most 10000 eligible visible endpoints, independent of legacy relay history scope attribution."}
	properties["stepsTruncated"] = &openAPISchema{Type: "boolean"}
	properties["stepBucketsTruncated"] = &openAPISchema{Type: "boolean"}
	properties["stepEvidence"] = openAPIRef("RegionsStepEvidence")
	return map[string]*openAPISchema{
		"RegionsRoutesResponse": {Type: "object", Properties: properties},
		"RegionsRouteStep":      {Type: "object", Properties: map[string]*openAPISchema{"from": {Type: "string"}, "to": {Type: "string"}, "scope": {Type: "string"}, "buckets": {Type: "array", Items: openAPIRef("RegionsRouteBucket"), Description: "Counts are not additive unique packet totals."}}},
		"RegionsRouteBucket":    {Type: "object", Properties: map[string]*openAPISchema{"index": {Type: "integer", Description: "Zero-based completed UTC hour, 0 through 503."}, "count": {Type: "integer", Description: "Distinct transmission IDs for this directed adjacent relay step and scope."}, "lastRelayed": {Type: "string", Description: "Latest transmission FirstSeen in this bucket, RFC3339."}}},
		"RegionsRouteNode":      {Type: "object", Properties: map[string]*openAPISchema{"publicKey": {Type: "string"}, "name": {Type: "string"}, "role": {Type: "string"}, "lat": {Type: "number", Nullable: true}, "lon": {Type: "number", Nullable: true}, "positionSource": {Type: "string", Enum: []string{"reported", "none"}}, "metadataSource": {Type: "string", Enum: []string{"current", "archived"}}}},
		"RegionsStepEvidence":   {Type: "object", Description: "Snapshot-wide diagnostics, not selected-window totals or scope-transition evidence.", Properties: map[string]*openAPISchema{"skippedObservations": {Type: "integer"}, "unresolvedHops": {Type: "integer"}, "sourceTruncated": {Type: "boolean"}, "incomplete": {Type: "boolean"}, "missingEndpointSteps": {Type: "integer"}}},
	}
}
