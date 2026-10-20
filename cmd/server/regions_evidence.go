package main

import (
	"sort"
	"time"
)

const (
	// Aggregated membership cells, one per (relay, scope, hour) (#508). The
	// retired budget retained one entry per (relay, transmission), so it grew
	// with observations: 48 h of real traffic already reached a third of it,
	// and 168 h would exhaust it. Cells grow with populated hours instead —
	// about 134,000 for 400 relays x 2 scopes x 168 hours — and repeated
	// transmissions in one hour reuse their cell.
	maxRegionsMembershipCells = 500000
	maxRegionsMembershipNodes = 10000
)

// regionsObservationClass is the single provenance gate for Regions
// memberships and directed steps. Firmware builds flood paths, but consumes
// direct paths as intended remaining hops (Mesh.cpp routeRecvPacket and
// removeSelfFromPath). Forwarders never rewrite transport codes (they are set
// only by sendFlood/sendZeroHop), so own codes that differ from the stored
// frame identify a frame for another scope code. Canonical transport codes
// only associate the own frame with its stored scope; they do not
// independently authenticate the scope name.
type regionsObservationClass uint8

const (
	regionsObservationEligible regionsObservationClass = iota
	// Identified as not eligible: a known DIRECT/unscoped header or own
	// transport codes that differ from the stored canonical codes. Excluded
	// observations never make counts incomplete.
	regionsObservationExcluded
	// The own header, own codes or canonical scope code are unknown, so
	// eligibility cannot be decided. Counts for the bucket/scope are qualified.
	regionsObservationUnprovable
)

func classifyRegionsObservation(obs *StoreObs, canonical uint32, known bool) regionsObservationClass {
	if obs == nil || obs.wireRoute == 0 {
		return regionsObservationUnprovable
	}
	if obs.wireRoute != 1 { // Explicit ROUTE_TYPE_TRANSPORT_FLOOD, encoded +1.
		return regionsObservationExcluded
	}
	if !known || !obs.transportKnown {
		return regionsObservationUnprovable
	}
	if obs.wireTransport != canonical {
		return regionsObservationExcluded
	}
	return regionsObservationEligible
}

// RegionsEvidenceCounts classifies observations of scoped transmissions that
// did not contribute memberships. Excluded observations were identified as not
// eligible. Unprovable observations lacked the evidence to decide. Capped
// counts observations, or per-transmission relay memberships, omitted by a
// safety limit. Only unprovable and capped make a bucket/scope incomplete.
type RegionsEvidenceCounts struct {
	Excluded   int `json:"excluded"`
	Unprovable int `json:"unprovable"`
	Capped     int `json:"capped"`
}

func (c *RegionsEvidenceCounts) add(d RegionsEvidenceCounts) {
	c.Excluded += d.Excluded
	c.Unprovable += d.Unprovable
	c.Capped += d.Capped
}

type RegionsEvidenceBucket struct {
	Index int `json:"index"`
	RegionsEvidenceCounts
}

type RegionsEvidenceScope struct {
	Name string `json:"name"`
	RegionsEvidenceCounts
	Buckets []RegionsEvidenceBucket `json:"buckets,omitempty"`
}

// RegionsEvidence is attributed to each transmission's stored scope and its
// FirstSeen hour. Truncated means the per-bucket budget was exhausted: the
// affected buckets are unknown and every rate or comparison must be withheld.
type RegionsEvidence struct {
	RegionsEvidenceCounts
	Truncated bool                   `json:"truncated"`
	Scopes    []RegionsEvidenceScope `json:"scopes"`
}

// 128 scopes (the Regions scope selector limit) x 505 hourly buckets: 21 days
// plus Latest's current hour.
const maxRegionsEvidenceCells = 128 * (regionsHistoryBucketCount + 1)

type regionsEvidenceKey struct {
	scope string
	index int
}

type regionsEvidenceBuilder struct {
	cells     map[regionsEvidenceKey]RegionsEvidenceCounts
	truncated bool
}

func newRegionsEvidenceBuilder() *regionsEvidenceBuilder {
	return &regionsEvidenceBuilder{cells: make(map[regionsEvidenceKey]RegionsEvidenceCounts)}
}

// add is O(1) and allocation-free for an existing cell, so it may run in
// capture under the packet read lock.
func (b *regionsEvidenceBuilder) add(scope string, index int, d RegionsEvidenceCounts) {
	if d == (RegionsEvidenceCounts{}) {
		return
	}
	key := regionsEvidenceKey{scope, index}
	cell, exists := b.cells[key]
	if !exists && len(b.cells) >= maxRegionsEvidenceCells {
		b.truncated = true
		return
	}
	cell.add(d)
	b.cells[key] = cell
}

// regionsEvidenceSnapshot is immutable once published. Scopes are sorted by
// name and buckets by index; index regionsHistoryBucketCount is the current,
// unfinished hour, which only Latest summarises.
type regionsEvidenceSnapshot struct {
	start     time.Time
	scopes    []RegionsEvidenceScope
	truncated bool
	history   RegionsEvidence
}

func (b *regionsEvidenceBuilder) build(start time.Time) *regionsEvidenceSnapshot {
	byScope := make(map[string][]RegionsEvidenceBucket)
	for key, counts := range b.cells {
		byScope[key.scope] = append(byScope[key.scope], RegionsEvidenceBucket{Index: key.index, RegionsEvidenceCounts: counts})
	}
	out := &regionsEvidenceSnapshot{start: start, truncated: b.truncated, scopes: make([]RegionsEvidenceScope, 0, len(byScope))}
	for name, buckets := range byScope {
		sort.Slice(buckets, func(i, j int) bool { return buckets[i].Index < buckets[j].Index })
		out.scopes = append(out.scopes, RegionsEvidenceScope{Name: name, Buckets: buckets})
	}
	sort.Slice(out.scopes, func(i, j int) bool { return out.scopes[i].Name < out.scopes[j].Name })
	out.history = out.summary(0, regionsHistoryBucketCount-1, true)
	return out
}

// summary restricts evidence to buckets first..last inclusive. It is O(cells),
// bounded by maxRegionsEvidenceCells, and never touches the packet store.
func (s *regionsEvidenceSnapshot) summary(first, last int, buckets bool) RegionsEvidence {
	out := RegionsEvidence{Truncated: s == nil || s.truncated, Scopes: []RegionsEvidenceScope{}}
	if s == nil {
		return out
	}
	for _, scope := range s.scopes {
		item := RegionsEvidenceScope{Name: scope.Name}
		for _, bucket := range scope.Buckets {
			if bucket.Index < first || bucket.Index > last {
				continue
			}
			item.add(bucket.RegionsEvidenceCounts)
			if buckets {
				item.Buckets = append(item.Buckets, bucket)
			}
		}
		if item.RegionsEvidenceCounts != (RegionsEvidenceCounts{}) {
			out.add(item.RegionsEvidenceCounts)
			out.Scopes = append(out.Scopes, item)
		}
	}
	return out
}

// window summarises every bucket that overlaps [start, end], including the
// current hour, for Latest's rolling windows.
func (s *regionsEvidenceSnapshot) window(start, end time.Time) RegionsEvidence {
	if s == nil {
		return s.summary(0, 0, false)
	}
	first := int(start.Sub(s.start) / time.Hour)
	if start.Before(s.start) {
		first = 0
	}
	return s.summary(first, int(end.Sub(s.start)/time.Hour), false)
}

func regionsEvidenceSchemas() map[string]*openAPISchema {
	counts := func(extra map[string]*openAPISchema) map[string]*openAPISchema {
		properties := map[string]*openAPISchema{
			"excluded":   {Type: "integer", Description: "Observations identified as not eligible: known DIRECT/unscoped header or own transport codes that differ from the stored frame. Never makes counts incomplete."},
			"unprovable": {Type: "integer", Description: "Observations whose own header, own transport codes or canonical scope code are unknown or malformed. Makes the bucket/scope incomplete."},
			"capped":     {Type: "integer", Description: "Observations or per-transmission relay memberships omitted by a safety limit. Makes the bucket/scope incomplete. Directed-step caps are reported in stepEvidence instead."},
		}
		for key, value := range extra {
			properties[key] = value
		}
		return properties
	}
	return map[string]*openAPISchema{
		"RegionsEvidence": {Type: "object", Description: "Attribution evidence by the transmission's stored scope and FirstSeen hour. Rates and comparisons are withheld only for buckets/scopes with unprovable or capped evidence.", Properties: counts(map[string]*openAPISchema{
			"truncated": {Type: "boolean", Description: "The per-bucket evidence budget was exhausted; affected buckets are unknown, so every rate and comparison is withheld."},
			"scopes":    {Type: "array", Items: openAPIRef("RegionsEvidenceScope"), Description: "Scopes with any evidence count, sorted by name."},
		})},
		"RegionsEvidenceScope": {Type: "object", Properties: counts(map[string]*openAPISchema{
			"name":    {Type: "string"},
			"buckets": {Type: "array", Items: openAPIRef("RegionsEvidenceBucket"), Description: "History only: buckets with any count, ascending by index."},
		})},
		"RegionsEvidenceBucket": {Type: "object", Properties: counts(map[string]*openAPISchema{
			"index": {Type: "integer", Description: "Zero-based completed UTC hour, 0 through 503."},
		})},
	}
}

type regionsNodeMembership struct {
	scopes          []regionScopeRelay
	scopesTruncated bool
}

// Kept separate from generic relay/liveness info: Regions must neither inherit
// its canonical-route heuristics nor broaden its existing public semantics.
type regionsMembershipSnapshot struct {
	nodes    map[string]regionsNodeMembership
	evidence *regionsEvidenceSnapshot
}

// regionsMembershipCellKey identifies one relay's membership evidence for one
// scope in one hourly bucket, 0 through regionsHistoryBucketCount; the last is
// the current unfinished hour, which only Latest summarises.
type regionsMembershipCellKey struct {
	key, scope string
	hour       int32
}

type regionsMembershipCell struct {
	count int
	last  time.Time
}

// regionsMembershipNodeCell is one relay's aggregated cell, handed to the
// hourly history builder in deterministic (scope, hour) order.
type regionsMembershipNodeCell struct {
	scope string
	hour  int32
	count int
	last  time.Time
}

type regionsMembershipBuilder struct {
	cells     map[regionsMembershipCellKey]regionsMembershipCell
	nodes     map[string]struct{}
	remaining int
	nodeBound int
	// Memberships each ceiling dropped, for the capture diagnostic (#510):
	// without them a withheld rate cannot be told apart from a capture
	// refusal, because both reach the bucket as `capped`.
	cellDrops, nodeDrops int
}

func newRegionsMembershipBuilder(cells, nodes int) *regionsMembershipBuilder {
	return &regionsMembershipBuilder{cells: make(map[regionsMembershipCellKey]regionsMembershipCell),
		nodes: make(map[string]struct{}), remaining: cells, nodeBound: nodes}
}

// retainLatestRegionScope keeps the newest 32 names with bounded scratch
// storage. Both Latest and hourly history use this exact tie-breaking rule.
func retainLatestRegionScope(latest map[string]time.Time, name string, when time.Time) (truncated bool) {
	if have, exists := latest[name]; exists {
		if when.After(have) {
			latest[name] = when
		}
		return false
	}
	if len(latest) < maxTransportedScopes {
		latest[name] = when
		return false
	}
	var oldestName string
	var oldest time.Time
	for scope, stamp := range latest {
		if oldestName == "" || stamp.Before(oldest) || (stamp.Equal(oldest) && scope > oldestName) {
			oldestName, oldest = scope, stamp
		}
	}
	if when.After(oldest) || (when.Equal(oldest) && name < oldestName) {
		delete(latest, oldestName)
		latest[name] = when
	}
	return true
}

// Called once per resolved node per distinct transmission, outside the packet
// lock, and aggregated straight into the node's (scope, hour) cell: nothing
// per-transmission is retained, so this budget no longer grows with the number
// of observations. It is never spent by directed edges or edge cells, and
// reports false when a safety limit omitted the membership.
//
// Only hour/scope/time are consumed; payload and provenance exclusion already
// happened before capture, and no canonical/direct relay fields are reused.
func (b *regionsMembershipBuilder) add(key, scope string, hour int, when time.Time) bool {
	id := regionsMembershipCellKey{key: key, scope: scope, hour: int32(hour)}
	cell, exists := b.cells[id]
	if !exists {
		if b.remaining <= 0 {
			b.cellDrops++
			return false
		}
		if _, known := b.nodes[key]; !known {
			if len(b.nodes) >= b.nodeBound {
				b.nodeDrops++
				return false
			}
			b.nodes[key] = struct{}{}
		}
		b.remaining--
	}
	cell.count++
	if when.After(cell.last) {
		cell.last = when
	}
	b.cells[id] = cell
	return true
}

func (b *regionsMembershipBuilder) build(now time.Time, evidence *regionsEvidenceSnapshot) (*regionsMembershipSnapshot, *regionsRelayHistory) {
	byNode := make(map[string][]regionsMembershipNodeCell, len(b.nodes))
	for id, cell := range b.cells {
		byNode[id.key] = append(byNode[id.key], regionsMembershipNodeCell{scope: id.scope, hour: id.hour, count: cell.count, last: cell.last})
	}
	out := &regionsMembershipSnapshot{nodes: make(map[string]regionsNodeMembership, len(byNode)), evidence: evidence}
	history := newRegionsHistoryBuilder(now)
	keys := make([]string, 0, len(byNode))
	for key := range byNode {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		cells := byNode[key]
		// Keys and cells are both visited in a deterministic order, so neither
		// the 32-scope selection nor the shared publication budget depends on
		// map iteration.
		sort.Slice(cells, func(i, j int) bool {
			if cells[i].scope != cells[j].scope {
				return cells[i].scope < cells[j].scope
			}
			return cells[i].hour < cells[j].hour
		})
		// At most 32 newest scopes per key, with bounded scratch space even
		// if an adversary creates a new scope for every retained transmission.
		latest := make(map[string]time.Time, maxTransportedScopes)
		truncated := false
		for _, cell := range cells {
			truncated = retainLatestRegionScope(latest, cell.scope, cell.last) || truncated
		}
		scopes, _ := newestRegionScopeRelays(latest)
		out.nodes[key] = regionsNodeMembership{scopes: scopes, scopesTruncated: truncated}
		history.add(key, cells)
	}
	return out, history.history
}
