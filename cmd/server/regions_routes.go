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
	// Published directed-step payload budget (#508). The retired fixed caps
	// (5,000 rows and 100,000 hourly cells) were both reached on real data at
	// 48 h retention, which left directed steps permanently incomplete, and a
	// larger fixed row count gives no payload guarantee. Rows and hourly cells
	// now share one estimated-byte budget, so a snapshot whose rows have few
	// populated hours publishes many more rows while the response-size
	// guarantee stays explicit and measurable.
	regionsRouteStepRowBytes  = 180 // from/to/scope plus the row's JSON scaffolding
	regionsRouteStepCellBytes = 75  // one {index, count, lastRelayed} bucket
	maxRegionsRouteStepBytes  = 16 << 20
	// Publication ceilings implied by the byte budget, for the OpenAPI
	// description and the browser's defensive bounds.
	maxRegionsRouteSteps = maxRegionsRouteStepBytes / regionsRouteStepRowBytes
	maxRegionsRouteCells = maxRegionsRouteStepBytes / regionsRouteStepCellBytes
	// Visible step endpoints published with a snapshot. Steps whose endpoints
	// are omitted are reported as missing-endpoint steps, never dropped
	// silently.
	maxRegionsRouteEndpoints = 10000

	maxRegionsRoutePathsPerTx = 128
	maxRegionsRoutePairsPerTx = 8192
	maxRegionsRoutePathBytes  = 8192
	// Interning table for observation path strings: a bounded dedup cache, not
	// a budget. Identical paths share one entry, so one relay chain observed by
	// thousands of transmissions is parsed and resolved once. Past the table a
	// path is retained un-interned instead of being refused — it costs a path
	// slot and a reference rather than a map entry. Refusing would reintroduce
	// exactly the starvation #508 reported, at a lower ceiling: real path
	// diversity is high (e2e-fixture.db, real captured traffic, holds 376
	// distinct path strings in 399 path-carrying observations), so a global
	// table fills on ordinary traffic and would then cap every later hour.
	maxRegionsRoutePathVariants = 1 << 17
	// Retained path references per hourly bucket (#508). The retired global
	// 250,000-reference budget was exhausted by 48 h of real traffic (~805k
	// observations): every hour after it was reported capped, which withheld
	// the Health and Timeline rates for #dk and for all scopes. A per-hour
	// budget makes the ceiling scale with retention and stops one busy hour
	// from starving the rest of the horizon. 65,536 is about four times the
	// ~16,800 observations/hour that 48 h of real traffic averaged across all
	// scopes, so even a peak hour has headroom if every observation were
	// transport-scoped and carried a distinct path. It is a fairness and
	// pathology net, not the memory bound: capture retains at most one
	// reference per eligible observation and one header per transmission
	// already in the packet store, so the store's own packetStore.maxMemoryMB
	// budget binds first (see docs/regions-observed.md).
	maxRegionsRoutePathRefsPerHour = 1 << 16
	// Estimated retained cost of the transient capture state, for the capture
	// diagnostic and for the disclosed horizon ceiling. Path content is shared
	// with the packet store, so only headers and map overhead are counted.
	regionsRoutePathRefBytes      = 8  // one regionsRoutePathRef
	regionsRoutePathSlotBytes     = 24 // one retained path: header plus resolver slot
	regionsRoutePathInternedBytes = 48 // its interning-table entry on top
	regionsRouteTxBytes           = 72 // one retained transmission header
	// Hop tokens cached with resolved paths. Past it, paths are resolved
	// without caching: slower, never truncated.
	maxRegionsRouteResolvedTokens = 1 << 21
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

// RegionsStepEvidence is snapshot-wide. Observation counts use the same
// classes as RegionsEvidence; CappedStepAppearances counts directed step
// appearances dropped by the step/pair/cell caps, which never affect
// memberships or history coverage.
type RegionsStepEvidence struct {
	ExcludedObservations   int  `json:"excludedObservations"`
	UnprovableObservations int  `json:"unprovableObservations"`
	CappedObservations     int  `json:"cappedObservations"`
	UnresolvedHops         int  `json:"unresolvedHops"`
	SourceTruncated        bool `json:"sourceTruncated"`
	CappedStepAppearances  int  `json:"cappedStepAppearances"`
	Incomplete             bool `json:"incomplete"`
	MissingEndpointSteps   int  `json:"missingEndpointSteps"`
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
	capture                          RegionsRouteCapture
	memberships                      *regionsMembershipSnapshot
	history                          *regionsRelayHistory
}

// RegionsRouteCapture is the transient capture state's fill against its own
// budgets (#510). Retained with the published snapshot and reported by the
// memory diagnostics, so an operator can see how close the budgets that
// decide `capped` are to their ceilings before one is reached. EstimatedBytes
// is the same deliberate estimate the store breakdown uses: struct headers,
// map overhead and resolver slots, with path content excluded because it is
// shared with the packet store.
type RegionsRouteCapture struct {
	PathVariants            int   `json:"pathVariants"`
	PathVariantsInterned    int   `json:"pathVariantsInterned"`
	PathVariantInternBudget int   `json:"pathVariantInternBudget"`
	PathReferences          int   `json:"pathReferences"`
	MaxHourPathReferences   int   `json:"maxHourPathReferences"`
	HourPathReferenceBudget int   `json:"hourPathReferenceBudget"`
	Transmissions           int   `json:"transmissions"`
	MembershipCells         int   `json:"membershipCells"`
	MembershipCellBudget    int   `json:"membershipCellBudget"`
	MembershipNodes         int   `json:"membershipNodes"`
	MembershipNodeBudget    int   `json:"membershipNodeBudget"`
	EstimatedBytes          int64 `json:"estimatedBytes"`
	// SourceTruncated repeats the capture budget's own refusal flag, so the
	// fill figures above are never read as a complete capture.
	SourceTruncated bool `json:"sourceTruncated"`
}

// regionsRoutePathRef points one transmission at an interned observation path
// and counts the observations that reported it. Eight bytes per retained
// reference, so the deferred capture list stays a small fraction of what the
// packet store already holds for the same observations.
type regionsRoutePathRef struct{ path, observations int32 }

type regionsRouteTx struct {
	id    int
	when  time.Time
	scope string
	paths []regionsRoutePathRef
}
type regionsRoutesCollector struct {
	start, end time.Time
	now        time.Time
	txs        []regionsRouteTx
	// Interned observation paths, and this snapshot's per-hour reference
	// budget. Indexed by hourly bucket, 0 through regionsHistoryBucketCount.
	pathIDs  map[string]int32
	pathList []string
	hourRefs []int32
	// internBound is the interning table's size. A field rather than the
	// constant so tests can exercise the un-interned fallback without building
	// 131,072 distinct paths; capture never mutates it.
	internBound int
	// stepBytes is the directed-step payload budget for this snapshot. A field
	// rather than the constant so tests can exercise truncation without
	// building a 16 MiB snapshot; build() never mutates it.
	stepBytes int
	evidence  RegionsStepEvidence
	cells     *regionsEvidenceBuilder
}

func newRegionsRoutesCollector(now time.Time) *regionsRoutesCollector {
	end := now.UTC().Truncate(time.Hour)
	return &regionsRoutesCollector{start: end.Add(-regionsHistoryBucketCount * time.Hour), end: end, now: now.UTC(),
		pathIDs: make(map[string]int32), hourRefs: make([]int32, regionsHistoryBucketCount+1),
		internBound: maxRegionsRoutePathVariants,
		stepBytes:   maxRegionsRouteStepBytes, cells: newRegionsEvidenceBuilder()}
}

// pathRefs reports how many observation path references this snapshot
// retained, for the capture tests and benchmarks.
func (c *regionsRoutesCollector) pathRefs() int {
	total := 0
	for _, used := range c.hourRefs {
		total += int(used)
	}
	return total
}

// captureStats reports how much of each capture budget this snapshot spent, so
// the budgets that decide `capped` are observable before they cap (#510). It
// is O(hourly buckets) and runs with the rest of build(), outside the packet
// lock.
func (c *regionsRoutesCollector) captureStats(memberships *regionsMembershipBuilder) RegionsRouteCapture {
	out := RegionsRouteCapture{
		PathVariants:            len(c.pathList),
		PathVariantsInterned:    len(c.pathIDs),
		PathVariantInternBudget: c.internBound,
		PathReferences:          c.pathRefs(),
		HourPathReferenceBudget: maxRegionsRoutePathRefsPerHour,
		Transmissions:           len(c.txs),
		MembershipCells:         len(memberships.cells),
		MembershipCellBudget:    maxRegionsMembershipCells,
		MembershipNodes:         len(memberships.nodes),
		MembershipNodeBudget:    maxRegionsMembershipNodes,
		SourceTruncated:         c.evidence.SourceTruncated,
	}
	for _, used := range c.hourRefs {
		if int(used) > out.MaxHourPathReferences {
			out.MaxHourPathReferences = int(used)
		}
	}
	out.EstimatedBytes = int64(out.PathReferences)*regionsRoutePathRefBytes +
		int64(out.PathVariants)*regionsRoutePathSlotBytes +
		int64(out.PathVariantsInterned)*regionsRoutePathInternedBytes +
		int64(out.Transmissions)*regionsRouteTxBytes
	return out
}

// retainPath spends this hour's reference budget and interns the path when it
// is new and the table has room. Identical paths share one interned entry, so
// a repeated relay chain is parsed and resolved once per snapshot; the only
// refusal is the per-hour reference budget, which is fair across the horizon.
// held is how many distinct paths this transmission already retained. The
// caller has already looked the path up, so capture costs one string-map
// lookup per observation, as it did before interning.
func (c *regionsRoutesCollector) retainPath(path string, id int32, known bool, index, held int) (int32, bool) {
	if held >= maxRegionsRoutePathsPerTx || index < 0 || index >= len(c.hourRefs) ||
		c.hourRefs[index] >= maxRegionsRoutePathRefsPerHour {
		return 0, false
	}
	if !known {
		id = int32(len(c.pathList))
		c.pathList = append(c.pathList, path)
		// Past the interning table the path is still retained, just without a
		// table entry: identical paths then cost one slot each instead of
		// sharing one. The per-hour reference budget bounds that; nothing is
		// refused for lack of table space.
		if len(c.pathIDs) < c.internBound {
			c.pathIDs[path] = id
		}
	}
	c.hourRefs[index]++
	return id, true
}

// addEvidence records one transmission's evidence in its own FirstSeen hour
// and stored scope, and in the snapshot-wide step diagnostics.
func (c *regionsRoutesCollector) addEvidence(tx regionsRouteTx, d RegionsEvidenceCounts) {
	c.evidence.ExcludedObservations += d.Excluded
	c.evidence.UnprovableObservations += d.Unprovable
	c.evidence.CappedObservations += d.Capped
	c.cells.add(tx.scope, c.bucket(tx), d)
}

// bucket is 0 through regionsHistoryBucketCount; the last is the current hour.
func (c *regionsRoutesCollector) bucket(tx regionsRouteTx) int {
	return int(tx.when.Sub(c.start) / time.Hour)
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
	index := c.bucket(item)
	var counts RegionsEvidenceCounts
	// Keyed by interned path id: repeated observations of one path dedupe
	// without hashing the path string a second time.
	seen := make(map[int32]int)
	for _, obs := range tx.Observations {
		switch classifyRegionsObservation(obs, canonical, known) {
		case regionsObservationExcluded:
			counts.Excluded++
			continue
		case regionsObservationUnprovable:
			counts.Unprovable++
			continue
		}
		if len(obs.PathJSON) > maxRegionsRoutePathBytes {
			counts.Capped++
			continue
		}
		id, interned := c.pathIDs[obs.PathJSON]
		if interned {
			if at, exists := seen[id]; exists {
				item.paths[at].observations++
				continue
			}
		}
		id, admitted := c.retainPath(obs.PathJSON, id, interned, index, len(item.paths))
		if !admitted {
			c.evidence.SourceTruncated = true
			counts.Capped++
			continue
		}
		seen[id] = len(item.paths)
		item.paths = append(item.paths, regionsRoutePathRef{path: id, observations: 1})
	}
	if len(tx.Observations) == 0 {
		counts.Unprovable++
	}
	c.addEvidence(item, counts)
	if len(item.paths) > 0 {
		c.txs = append(c.txs, item)
	}
}

type regionsRouteKey struct{ from, to, scope string }
type regionsRouteCell struct {
	count int
	last  time.Time
}

// regionsResolvedPath is one interned observation path, parsed and identity-
// resolved once per snapshot instead of once per transmission that observed
// it. keys keeps path order; an empty key is an unresolved hop token, which
// breaks adjacency without qualifying resolved memberships.
type regionsResolvedPath struct {
	keys []string
	bad  bool // malformed path JSON
	over bool // more hop tokens than the adjacency scan accepts
}

// regionsPathResolver runs outside the packet lock. The cache is bounded by
// retained hop tokens; past that budget paths are resolved uncached, which
// costs time but never truncates evidence.
type regionsPathResolver struct {
	resolve   *relayTokenResolver
	paths     []string
	cached    []*regionsResolvedPath
	remaining int
}

func newRegionsPathResolver(paths []string, pm *prefixMap) *regionsPathResolver {
	return &regionsPathResolver{resolve: newRelayTokenResolver(pm), paths: paths,
		cached: make([]*regionsResolvedPath, len(paths)), remaining: maxRegionsRouteResolvedTokens}
}

func (r *regionsPathResolver) at(id int32) *regionsResolvedPath {
	if id < 0 || int(id) >= len(r.paths) {
		return &regionsResolvedPath{bad: true}
	}
	if hit := r.cached[id]; hit != nil {
		return hit
	}
	out := &regionsResolvedPath{}
	var tokens []string
	if err := json.Unmarshal([]byte(r.paths[id]), &tokens); err != nil {
		out.bad = true
	} else if len(tokens) > 64 {
		out.over = true
	} else {
		out.keys = make([]string, len(tokens))
		for i, token := range tokens {
			if len(r.resolve.cache) >= 4096 {
				clear(r.resolve.cache)
			}
			out.keys[i] = r.resolve.key(token)
		}
	}
	if r.remaining >= len(out.keys) {
		r.remaining -= len(out.keys)
		r.cached[id] = out
	}
	return out
}

func (c *regionsRoutesCollector) build(pm *prefixMap) *regionsRouteSnapshot {
	out := &regionsRouteSnapshot{steps: []RegionsRouteStep{}}
	sort.Slice(c.txs, func(i, j int) bool { return c.txs[i].id < c.txs[j].id })
	paths := newRegionsPathResolver(c.pathList, pm)
	memberships := newRegionsMembershipBuilder()
	rows := make(map[regionsRouteKey]map[int]regionsRouteCell)
	seen := make(map[regionsRouteKey]bool)
	seenMembers := make(map[string]bool)
	// Published rows and hourly cells share one estimated-payload budget, so a
	// row cap can no longer truncate a snapshot that would fit (#508).
	payload := c.stepBytes
	// Directed-step caps are counted only as step appearances: they never
	// reach membership coverage evidence.
	capStep := func(truncated *bool) {
		*truncated = true
		c.evidence.CappedStepAppearances++
	}
	for _, tx := range c.txs {
		clear(seen)
		clear(seenMembers)
		index := c.bucket(tx)
		for _, ref := range tx.paths {
			resolved := paths.at(ref.path)
			if resolved.bad {
				c.addEvidence(tx, RegionsEvidenceCounts{Unprovable: int(ref.observations)})
				continue
			}
			if resolved.over {
				c.addEvidence(tx, RegionsEvidenceCounts{Capped: int(ref.observations)})
				continue
			}
			previous := ""
			for _, key := range resolved.keys {
				if key == "" {
					previous = ""
					c.evidence.UnresolvedHops++
					continue
				}
				if !seenMembers[key] {
					seenMembers[key] = true
					if !memberships.add(key, tx.scope, index, tx.when) {
						// A dropped membership, not an observation: no step count.
						c.cells.add(tx.scope, index, RegionsEvidenceCounts{Capped: 1})
					}
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
							capStep(&out.stepsTruncated)
							previous = key
							continue
						}
						seen[step] = true
						byIndex := rows[step]
						cell, exists := byIndex[index]
						// A new row pays for its scaffolding and its first
						// bucket; an existing bucket costs nothing more.
						cost := 0
						if byIndex == nil {
							cost = regionsRouteStepRowBytes + regionsRouteStepCellBytes
						} else if !exists {
							cost = regionsRouteStepCellBytes
						}
						if cost > payload {
							if byIndex == nil {
								capStep(&out.stepsTruncated)
							} else {
								capStep(&out.bucketsTruncated)
							}
							previous = key
							continue
						}
						payload -= cost
						if byIndex == nil {
							byIndex = make(map[int]regionsRouteCell)
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
	out.capture = c.captureStats(memberships)
	out.memberships, out.history = memberships.build(c.now, c.cells.build(c.start))
	out.evidence = c.evidence
	// Directed-step completeness only. Unresolved hops break adjacency, so they
	// qualify steps; they do not qualify memberships of resolved relays.
	out.evidence.Incomplete = out.evidence.UnprovableObservations > 0 || out.evidence.CappedObservations > 0 ||
		out.evidence.UnresolvedHops > 0 || out.evidence.SourceTruncated || out.evidence.CappedStepAppearances > 0 ||
		out.stepsTruncated || out.bucketsTruncated
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
	properties["steps"] = &openAPISchema{Type: "array", Items: openAPIRef("RegionsRouteStep"),
		Description: "Directed step/scope rows, sorted by from, to and scope. Rows and their hourly buckets share one 16 MiB estimated-payload budget (at most 93206 rows or 223696 buckets), not a fixed row count; stepsTruncated and stepBucketsTruncated disclose when it was reached."}
	properties["stepNodes"] = &openAPISchema{Type: "array", Items: openAPIRef("RegionsRouteNode"), Description: "At most 10000 eligible visible endpoints, independent of legacy relay history scope attribution."}
	properties["stepsTruncated"] = &openAPISchema{Type: "boolean", Description: "A directed step/scope row was omitted by the published payload budget or the per-transmission pair cap."}
	properties["stepBucketsTruncated"] = &openAPISchema{Type: "boolean", Description: "An hourly bucket of a published row was omitted by the published payload budget."}
	properties["stepEvidence"] = openAPIRef("RegionsStepEvidence")
	return map[string]*openAPISchema{
		"RegionsRoutesResponse": {Type: "object", Properties: properties},
		"RegionsRouteStep":      {Type: "object", Properties: map[string]*openAPISchema{"from": {Type: "string"}, "to": {Type: "string"}, "scope": {Type: "string"}, "buckets": {Type: "array", Items: openAPIRef("RegionsRouteBucket"), Description: "Counts are not additive unique packet totals."}}},
		"RegionsRouteBucket":    {Type: "object", Properties: map[string]*openAPISchema{"index": {Type: "integer", Description: "Zero-based completed UTC hour, 0 through 503."}, "count": {Type: "integer", Description: "Distinct transmission IDs for this directed adjacent relay step and scope."}, "lastRelayed": {Type: "string", Description: "Latest transmission FirstSeen in this bucket, RFC3339."}}},
		"RegionsRouteNode":      {Type: "object", Properties: map[string]*openAPISchema{"publicKey": {Type: "string"}, "name": {Type: "string"}, "role": {Type: "string"}, "lat": {Type: "number", Nullable: true}, "lon": {Type: "number", Nullable: true}, "positionSource": {Type: "string", Enum: []string{"reported", "none"}}, "metadataSource": {Type: "string", Enum: []string{"current", "archived"}}}},
		"RegionsStepEvidence": {Type: "object", Description: "Snapshot-wide directed-step diagnostics, not selected-window totals or scope-transition evidence. Per-bucket membership evidence is coverage.evidence.", Properties: map[string]*openAPISchema{
			"excludedObservations":   {Type: "integer", Description: "Known DIRECT/unscoped or mismatched-transport-code observations."},
			"unprovableObservations": {Type: "integer", Description: "Observations with unknown or malformed own/canonical evidence."},
			"cappedObservations":     {Type: "integer", Description: "Observations omitted by the per-hour capture budget, the per-transmission path cap or the path size bound."},
			"unresolvedHops":         {Type: "integer", Description: "Hop tokens without a unique relay identity; they break adjacency but not resolved memberships."},
			"sourceTruncated":        {Type: "boolean"},
			"cappedStepAppearances":  {Type: "integer", Description: "Directed step appearances omitted by the published payload budget or the per-transmission pair cap. Never part of coverage.evidence."},
			"incomplete":             {Type: "boolean", Description: "Directed steps may be missing: unprovable/capped observations, unresolved hops, step caps or missing endpoints."},
			"missingEndpointSteps":   {Type: "integer"}}},
	}
}
