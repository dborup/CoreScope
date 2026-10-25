package main

import (
	"container/heap"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
)

// Mesh-wide advert interval analytics (#245 M2): which flood and zero-hop
// advert intervals the nodes of the mesh run with, by role and by region,
// how many show no adverts of a class, and which advertise more often than
// the firmware's timers can.
//
// Every node is estimated exactly as node detail does it (M1,
// advert_intervals.go): the newest nodeAdvertRouteLimit flood and zero-hop
// adverts per node, classified by classifyAdvertRouteRow and fed to
// estimateAdvertInterval, which also snaps to the firmware's settable
// values. TestAdvertIntervalAnalytics_MatchesNodeDetail pins that.
//
// Cost: one advert-interval recomputer (analyticsRecomputer, so the
// analytics cycle, its config interval and #492 pauseWhenIdle apply) runs
// computeAdvertIntervalAnalytics: three read-only queries - every ADVERT
// row's classifier columns newest first (idx_transmissions_payload_type,
// capped at advertIntervalScanRowCap rows), then decoded_json and the
// observer regions of the kept adverts only (at most 2 x
// nodeAdvertRouteLimit per node) - and the estimator per node. It never
// takes the packet store lock (#495). A request only filters and
// aggregates the cached per-node rows, O(nodes); the unfiltered response is
// precomputed in the snapshot.
//
// The thresholds below (minimum confidence, outlier limit, scan cap) and the
// estimator's own are hardcoded; they are candidates for the customizer
// (#245 "Later", AGENTS.md rule 8).

const (
	// advertIntervalAnalyticsMinConfidence is the least confidence a
	// class estimate needs to count in the distributions; low-confidence
	// estimates are counted beside them.
	advertIntervalAnalyticsMinConfidence = advertConfidenceMedium
	// advertIntervalScanRowCap bounds the ADVERT rows one pass reads,
	// newest first; older adverts beyond it are left out (scan.truncated).
	advertIntervalScanRowCap = 1000000
	// advertIntervalOutlierLimit bounds the outlier list (outliers_total
	// has the full count).
	advertIntervalOutlierLimit = 50
	// advertIntervalRegionLimit bounds by_region (regions_truncated).
	advertIntervalRegionLimit = 50
	// advertIntervalScanTimeout bounds one pass's queries.
	advertIntervalScanTimeout = 2 * time.Minute
	// advertIntervalNoRegion is the by_region key of nodes no observer
	// with a region heard; lower case, so it cannot clash with an IATA code.
	advertIntervalNoRegion = "unknown"
)

// The firmware's shortest timer settings (see advert_intervals.go):
// flood.advert.interval 3 h (src/helpers/CommonCLI.cpp:486-495),
// advert.interval 60 min (src/helpers/CommonCLI.cpp:496-505,
// MIN_LOCAL_ADVERT_INTERVAL at :160), at most 240 min, and the 2-minute
// new-install zero-hop default (examples/simple_repeater/MyMesh.cpp:903).
const (
	advertFloodMinIntervalS     = 3 * 3600
	advertZeroHopMinIntervalS   = 60 * 60
	advertZeroHopMaxIntervalS   = 240 * 60
	advertZeroHopNewInstallS    = 120
	advertOutlierFloodFast      = "flood_faster_than_settable"
	advertOutlierZeroHopFast    = "zero_hop_faster_than_settable"
	advertOutlierZeroHopDefault = "zero_hop_new_install_default"
)

// AdvertIntervalAnalyticsResponse is GET /api/analytics/advert-intervals.
type AdvertIntervalAnalyticsResponse struct {
	// Window is the most adverts per class and node estimated from.
	Window        int                        `json:"window"`
	MinConfidence string                     `json:"min_confidence"`
	Nodes         int                        `json:"nodes"`
	Flood         AdvertIntervalClassSummary `json:"flood"`
	ZeroHop       AdvertIntervalClassSummary `json:"zero_hop"`
	ByRole        []AdvertIntervalGroup      `json:"by_role"`
	ByRegion      []AdvertIntervalGroup      `json:"by_region"`
	// RegionsTruncated: by_region holds the advertIntervalRegionLimit
	// largest regions only.
	RegionsTruncated bool                     `json:"regions_truncated"`
	Outliers         []AdvertIntervalOutlier  `json:"outliers"`
	OutliersTotal    int                      `json:"outliers_total"`
	FloodReArm       AdvertIntervalFloodReArm `json:"flood_re_arm"`
	Scan             AdvertIntervalScanInfo   `json:"scan"`
}

// AdvertIntervalClassSummary counts one route class's estimates.
type AdvertIntervalClassSummary struct {
	// Estimated have at least advertIntervalAnalyticsMinConfidence and are
	// in Buckets; the other counts are the nodes left out of them.
	Estimated     int                    `json:"estimated"`
	LowConfidence int                    `json:"low_confidence"`
	Irregular     int                    `json:"irregular"`
	TooFew        int                    `json:"too_few"`
	NotObserved   int                    `json:"not_observed"`
	Buckets       []AdvertIntervalBucket `json:"buckets"`
}

// AdvertIntervalBucket is one interval value and how many nodes run it.
type AdvertIntervalBucket struct {
	IntervalS int64 `json:"interval_s"`
	Snapped   bool  `json:"snapped"`
	Nodes     int   `json:"nodes"`
}

// AdvertIntervalGroup is the class summaries of one role or region.
type AdvertIntervalGroup struct {
	Key     string                     `json:"key"`
	Nodes   int                        `json:"nodes"`
	Flood   AdvertIntervalClassSummary `json:"flood"`
	ZeroHop AdvertIntervalClassSummary `json:"zero_hop"`
}

// AdvertIntervalOutlier is a node advertising more often than the class's
// shortest timer setting, or on the 2-minute new-install zero-hop default.
type AdvertIntervalOutlier struct {
	PublicKey  string   `json:"public_key"`
	Name       string   `json:"name"`
	Role       string   `json:"role"`
	Regions    []string `json:"regions"`
	Class      string   `json:"class"`
	Reason     string   `json:"reason"`
	MedianGapS int64    `json:"median_gap_s"`
	Samples    int      `json:"samples"`
	IntervalS  *int64   `json:"interval_s"`
}

// AdvertIntervalFloodReArm counts nodes whose zero-hop reading the flood
// timer shapes: a flood advert re-arms the zero-hop timer
// (examples/simple_repeater/MyMesh.cpp:1294-1306).
type AdvertIntervalFloodReArm struct {
	// ZeroHopPacedByFlood: the zero-hop estimate equals the flood interval
	// (zero-hop Z < flood F <= 2Z gives every zero-hop gap F).
	ZeroHopPacedByFlood int `json:"zero_hop_paced_by_flood"`
	// ZeroHopMaybeSuppressed: no zero-hop adverts and a flood interval of
	// at most 240 min, so advert.interval >= flood would never fire.
	ZeroHopMaybeSuppressed int `json:"zero_hop_maybe_suppressed"`
}

// AdvertIntervalScanInfo describes the pass the response comes from.
type AdvertIntervalScanInfo struct {
	GeneratedAt string `json:"generated_at"`
	Rows        int    `json:"rows"`
	Truncated   bool   `json:"truncated"`
}

// advertIntervalNodeRow is one node of the snapshot: both estimates, the
// median gap of each class's adverts (0 below the minimum samples) and the
// regions of the observers that heard them.
type advertIntervalNodeRow struct {
	pubkey, name, role     string
	regions                []string
	flood, zeroHop         AdvertIntervalEstimate
	floodGapS, zeroHopGapS float64
}

// advertIntervalSnapshot is what the recomputer stores: the rows the
// filtered requests aggregate and the unfiltered response.
type advertIntervalSnapshot struct {
	rows []advertIntervalNodeRow
	all  AdvertIntervalAnalyticsResponse
}

func newAdvertIntervalClassSummary() AdvertIntervalClassSummary {
	return AdvertIntervalClassSummary{Buckets: []AdvertIntervalBucket{}}
}

func emptyAdvertIntervalAnalytics() AdvertIntervalAnalyticsResponse {
	return AdvertIntervalAnalyticsResponse{
		Window:        nodeAdvertRouteLimit,
		MinConfidence: advertIntervalAnalyticsMinConfidence,
		Flood:         newAdvertIntervalClassSummary(),
		ZeroHop:       newAdvertIntervalClassSummary(),
		ByRole:        []AdvertIntervalGroup{},
		ByRegion:      []AdvertIntervalGroup{},
		Outliers:      []AdvertIntervalOutlier{},
	}
}

// advertIntervalCounted is whether e counts in the distributions.
func advertIntervalCounted(e AdvertIntervalEstimate) bool {
	return e.Status == advertIntervalEstimated && e.IntervalS != nil &&
		(e.Confidence == advertConfidenceHigh || e.Confidence == advertConfidenceMedium)
}

// advertIntervalBucketValue is e's bucket: the snapped firmware value, or an
// unsnapped estimate rounded to the class's unit (flood hours, zero-hop
// minutes) so the buckets stay few.
func advertIntervalBucketValue(e AdvertIntervalEstimate, class advertIntervalClass) int64 {
	v := *e.IntervalS
	if e.Snapped {
		return v
	}
	unit := 3600.0
	if class == advertIntervalZeroHop {
		unit = 60
	}
	return int64(math.Round(float64(v)/unit) * unit)
}

type advertIntervalBucketKey struct {
	v       int64
	snapped bool
}

// advertIntervalClassAcc accumulates one AdvertIntervalClassSummary.
type advertIntervalClassAcc struct {
	sum     AdvertIntervalClassSummary
	buckets map[advertIntervalBucketKey]int
}

func (a *advertIntervalClassAcc) add(e AdvertIntervalEstimate, class advertIntervalClass) {
	switch e.Status {
	case advertIntervalNoneObserved:
		a.sum.NotObserved++
	case advertIntervalTooFew:
		a.sum.TooFew++
	case advertIntervalIrregular:
		a.sum.Irregular++
	case advertIntervalEstimated:
		if !advertIntervalCounted(e) {
			a.sum.LowConfidence++
			return
		}
		a.sum.Estimated++
		if a.buckets == nil {
			a.buckets = map[advertIntervalBucketKey]int{}
		}
		a.buckets[advertIntervalBucketKey{advertIntervalBucketValue(e, class), e.Snapped}]++
	}
}

func (a *advertIntervalClassAcc) summary() AdvertIntervalClassSummary {
	out := a.sum
	out.Buckets = make([]AdvertIntervalBucket, 0, len(a.buckets))
	for k, n := range a.buckets {
		out.Buckets = append(out.Buckets, AdvertIntervalBucket{IntervalS: k.v, Snapped: k.snapped, Nodes: n})
	}
	sort.Slice(out.Buckets, func(i, j int) bool {
		bi, bj := out.Buckets[i], out.Buckets[j]
		if bi.IntervalS != bj.IntervalS {
			return bi.IntervalS < bj.IntervalS
		}
		return bi.Snapped && !bj.Snapped
	})
	return out
}

type advertIntervalGroupAcc struct {
	nodes          int
	flood, zeroHop advertIntervalClassAcc
}

func (g *advertIntervalGroupAcc) add(r *advertIntervalNodeRow) {
	g.nodes++
	g.flood.add(r.flood, advertIntervalFlood)
	g.zeroHop.add(r.zeroHop, advertIntervalZeroHop)
}

// advertIntervalGroups turns accumulated groups into the response list,
// largest first, then by key; at most limit (0: all) and whether any were
// left out.
func advertIntervalGroups(groups map[string]*advertIntervalGroupAcc, limit int) ([]AdvertIntervalGroup, bool) {
	out := make([]AdvertIntervalGroup, 0, len(groups))
	for k, g := range groups {
		out = append(out, AdvertIntervalGroup{Key: k, Nodes: g.nodes, Flood: g.flood.summary(), ZeroHop: g.zeroHop.summary()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Nodes != out[j].Nodes {
			return out[i].Nodes > out[j].Nodes
		}
		return out[i].Key < out[j].Key
	})
	if limit > 0 && len(out) > limit {
		return out[:limit], true
	}
	return out, false
}

// advertIntervalOutliers lists r's outlier classes: a zero-hop estimate at
// the 2-minute new-install default, else a median gap under the class's
// shortest setting (less the estimator's tolerance) - manual or scripted
// adverts, a reboot loop, or a firmware without the limit.
func advertIntervalOutliers(r *advertIntervalNodeRow) []AdvertIntervalOutlier {
	var out []AdvertIntervalOutlier
	regions := r.regions
	if regions == nil {
		regions = []string{}
	}
	add := func(class, reason string, gap float64, e AdvertIntervalEstimate) {
		out = append(out, AdvertIntervalOutlier{PublicKey: r.pubkey, Name: r.name, Role: r.role, Regions: regions,
			Class: class, Reason: reason, MedianGapS: int64(math.Round(gap)), Samples: e.Samples, IntervalS: e.IntervalS})
	}
	if r.floodGapS > 0 && r.floodGapS < advertFloodMinIntervalS*(1-advertIntervalTol) {
		add(advertClassFlood, advertOutlierFloodFast, r.floodGapS, r.flood)
	}
	switch {
	case advertIntervalCounted(r.zeroHop) && r.zeroHop.Snapped && *r.zeroHop.IntervalS == advertZeroHopNewInstallS:
		add(advertClassZeroHop, advertOutlierZeroHopDefault, r.zeroHopGapS, r.zeroHop)
	case r.zeroHopGapS > 0 && r.zeroHopGapS < advertZeroHopMinIntervalS*(1-advertIntervalTol):
		add(advertClassZeroHop, advertOutlierZeroHopFast, r.zeroHopGapS, r.zeroHop)
	}
	return out
}

// countFloodReArm adds r to the flood re-arm counts (see
// AdvertIntervalFloodReArm). Only estimates that count are compared.
func countFloodReArm(acc *AdvertIntervalFloodReArm, r *advertIntervalNodeRow) {
	if !advertIntervalCounted(r.flood) {
		return
	}
	f := float64(*r.flood.IntervalS)
	switch {
	case advertIntervalCounted(r.zeroHop) && math.Abs(float64(*r.zeroHop.IntervalS)-f) <= advertIntervalTol*f:
		acc.ZeroHopPacedByFlood++
	case r.zeroHop.Status == advertIntervalNoneObserved && f <= advertZeroHopMaxIntervalS*(1+advertIntervalTol):
		acc.ZeroHopMaybeSuppressed++
	}
}

// aggregateAdvertIntervals summarizes the rows keep accepts. Pure: no
// store, lock or database.
func aggregateAdvertIntervals(rows []advertIntervalNodeRow, keep func(*advertIntervalNodeRow) bool) AdvertIntervalAnalyticsResponse {
	resp := emptyAdvertIntervalAnalytics()
	var flood, zeroHop advertIntervalClassAcc
	var outliers advertOutlierHeap
	byRole := map[string]*advertIntervalGroupAcc{}
	byRegion := map[string]*advertIntervalGroupAcc{}
	group := func(m map[string]*advertIntervalGroupAcc, k string) *advertIntervalGroupAcc {
		g := m[k]
		if g == nil {
			g = &advertIntervalGroupAcc{}
			m[k] = g
		}
		return g
	}
	for i := range rows {
		r := &rows[i]
		if !keep(r) {
			continue
		}
		resp.Nodes++
		flood.add(r.flood, advertIntervalFlood)
		zeroHop.add(r.zeroHop, advertIntervalZeroHop)
		group(byRole, normalizeRole(r.role)).add(r)
		if len(r.regions) == 0 {
			group(byRegion, advertIntervalNoRegion).add(r)
		}
		for _, reg := range r.regions {
			group(byRegion, reg).add(r)
		}
		for _, o := range advertIntervalOutliers(r) {
			resp.OutliersTotal++
			outliers.push(o, advertIntervalOutlierLimit)
		}
		countFloodReArm(&resp.FloodReArm, r)
	}
	resp.Flood, resp.ZeroHop = flood.summary(), zeroHop.summary()
	resp.ByRole, _ = advertIntervalGroups(byRole, 0)
	resp.ByRegion, resp.RegionsTruncated = advertIntervalGroups(byRegion, advertIntervalRegionLimit)
	resp.Outliers = append(resp.Outliers, outliers...)
	sort.Slice(resp.Outliers, func(i, j int) bool { return advertOutlierBefore(resp.Outliers[i], resp.Outliers[j]) })
	return resp
}

// advertOutlierBefore orders outliers most frequent first (median gap
// ascending), then by key and class.
func advertOutlierBefore(a, b AdvertIntervalOutlier) bool {
	if a.MedianGapS != b.MedianGapS {
		return a.MedianGapS < b.MedianGapS
	}
	if a.PublicKey != b.PublicKey {
		return a.PublicKey < b.PublicKey
	}
	return a.Class < b.Class
}

// advertOutlierHeap keeps the first limit outliers in advertOutlierBefore
// order: a max-heap on that order, so a mesh full of outliers costs
// O(n log limit) and limit entries, not a sort of all of them.
type advertOutlierHeap []AdvertIntervalOutlier

func (h advertOutlierHeap) Len() int            { return len(h) }
func (h advertOutlierHeap) Less(i, j int) bool  { return advertOutlierBefore(h[j], h[i]) }
func (h advertOutlierHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *advertOutlierHeap) Push(x interface{}) { *h = append(*h, x.(AdvertIntervalOutlier)) }
func (h *advertOutlierHeap) Pop() interface{} {
	old := *h
	o := old[len(old)-1]
	*h = old[:len(old)-1]
	return o
}

func (h *advertOutlierHeap) push(o AdvertIntervalOutlier, limit int) {
	switch {
	case h.Len() < limit:
		heap.Push(h, o)
	case advertOutlierBefore(o, (*h)[0]):
		(*h)[0] = o
		heap.Fix(h, 0)
	}
}

// advertIntervalFilter keeps rows heard in one of the comma-separated
// region codes (when given) and in areaNodes (when non-nil; nil is no area
// filter, as resolveAreaNodes defines it).
func advertIntervalFilter(region string, areaNodes map[string]bool) func(*advertIntervalNodeRow) bool {
	codes := normalizeRegionCodes(region)
	return func(r *advertIntervalNodeRow) bool {
		if areaNodes != nil && !areaNodes[r.pubkey] {
			return false
		}
		if len(codes) == 0 {
			return true
		}
		for _, reg := range r.regions {
			for _, c := range codes {
				if reg == c {
					return true
				}
			}
		}
		return false
	}
}

// advertMedianGap is the median gap between the samples (advertGaps: the
// sender clock where plausible), or 0 under advertIntervalMinSamples.
func advertMedianGap(samples []advertIntervalSample) float64 {
	if len(samples) < advertIntervalMinSamples {
		return 0
	}
	s := append([]advertIntervalSample(nil), samples...)
	sort.Slice(s, func(i, j int) bool { return s[i].heard.Before(s[j].heard) })
	gaps, _ := advertGaps(s)
	if len(gaps) == 0 {
		return 0
	}
	return medianFloat(gaps)
}

// --- database scan --------------------------------------------------------

// advertIntervalNodeSamples is one node's kept adverts per class and the
// regions (trimmed, upper-case IATA) of the observers that heard them.
type advertIntervalNodeSamples struct {
	flood, zeroHop []advertIntervalSample
	regions        []string
}

type advertIntervalScan struct {
	nodes     map[string]*advertIntervalNodeSamples // lower-case pubkey
	rows      int
	truncated bool
}

type advertIntervalKept struct {
	id        int64
	pubkey    string
	zeroHop   bool
	firstSeen string
}

// scanAdvertIntervalSamples reads, newest first, the classifier columns of
// at most rowCap ADVERT rows and keeps per node the newest
// nodeAdvertRouteLimit flood and zero-hop adverts - the rows
// GetNodeAdvertRoutes lists for node detail. Only the kept adverts'
// decoded_json and observers are read after that.
func (db *DB) scanAdvertIntervalSamples(rowCap int) (advertIntervalScan, error) {
	scan := advertIntervalScan{nodes: map[string]*advertIntervalNodeSamples{}}
	ctx, cancel := context.WithTimeout(context.Background(), advertIntervalScanTimeout)
	defer cancel()

	rows, err := db.conn.QueryContext(ctx, fmt.Sprintf(`SELECT id, lower(from_pubkey),
			%s,
			COALESCE(first_seen, '')
		FROM transmissions
		WHERE payload_type = ? AND from_pubkey IS NOT NULL AND from_pubkey != ''
		ORDER BY id DESC LIMIT ?`, advertRouteClassColumnsSQL(db.hasRouteMask())), payloadTypeAdvert, rowCap+1)
	if err != nil {
		return scan, err
	}
	type perClass struct{ flood, zeroHop int }
	counts := map[string]*perClass{}
	var kept []advertIntervalKept
	for rows.Next() {
		if scan.rows == rowCap {
			scan.truncated = true
			break
		}
		scan.rows++
		var id int64
		var pk, firstSeen string
		var routeType, mask sql.NullInt64
		if err := rows.Scan(&id, &pk, &routeType, &mask, &firstSeen); err != nil {
			rows.Close()
			return scan, err
		}
		c := counts[pk]
		if c == nil {
			c = &perClass{}
			counts[pk] = c
			scan.nodes[pk] = &advertIntervalNodeSamples{}
		}
		switch classifyAdvertRouteRow(mask, routeType) {
		case relayAirtimeAdvertFlood:
			if c.flood < nodeAdvertRouteLimit {
				c.flood++
				kept = append(kept, advertIntervalKept{id, pk, false, firstSeen})
			}
		case relayAirtimeAdvertZeroHop:
			if c.zeroHop < nodeAdvertRouteLimit {
				c.zeroHop++
				kept = append(kept, advertIntervalKept{id, pk, true, firstSeen})
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return scan, err
	}
	if len(kept) == 0 {
		return scan, nil
	}

	ids := make([]int64, len(kept))
	for i, k := range kept {
		ids[i] = k.id
	}
	idsJSON, err := json.Marshal(ids)
	if err != nil {
		return scan, err
	}
	decoded, err := db.advertIntervalDecoded(ctx, string(idsJSON))
	if err != nil {
		return scan, err
	}
	regions, err := db.advertIntervalRegions(ctx, string(idsJSON))
	if err != nil {
		return scan, err
	}
	nodeRegions := map[string]map[string]struct{}{}
	for _, k := range kept {
		n := scan.nodes[k.pubkey]
		if s, ok := advertIntervalSampleOf(k.firstSeen, decoded[k.id]); ok {
			if k.zeroHop {
				n.zeroHop = append(n.zeroHop, s)
			} else {
				n.flood = append(n.flood, s)
			}
		}
		for _, reg := range regions[k.id] {
			if nodeRegions[k.pubkey] == nil {
				nodeRegions[k.pubkey] = map[string]struct{}{}
			}
			nodeRegions[k.pubkey][reg] = struct{}{}
		}
	}
	for pk, set := range nodeRegions {
		list := make([]string, 0, len(set))
		for reg := range set {
			list = append(list, reg)
		}
		sort.Strings(list)
		scan.nodes[pk].regions = list
	}
	return scan, nil
}

// advertIntervalDecoded reads decoded_json of the transmissions in idsJSON.
func (db *DB) advertIntervalDecoded(ctx context.Context, idsJSON string) (map[int64]string, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT id, COALESCE(decoded_json, '') FROM transmissions
		WHERE id IN (SELECT value FROM json_each(?))`, idsJSON)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var dj string
		if err := rows.Scan(&id, &dj); err != nil {
			return nil, err
		}
		out[id] = dj
	}
	return out, rows.Err()
}

// advertIntervalRegions reads, per transmission in idsJSON, the regions of
// the observers that heard it (observers.iata trimmed and upper-cased as
// observerIATAs does; observers without one are left out). The observers
// table is small, so it is read once into a map and the observations are
// looked up by idx_observations_transmission_id without a join.
func (db *DB) advertIntervalRegions(ctx context.Context, idsJSON string) (map[int64][]string, error) {
	obsKey, obsCol := "rowid", "observer_idx"
	if !db.isV3() {
		obsKey, obsCol = "id", "observer_id"
	}
	iata := map[string]string{}
	orows, err := db.conn.QueryContext(ctx, `SELECT CAST(`+obsKey+` AS TEXT), iata FROM observers WHERE iata IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	for orows.Next() {
		var key, code string
		if err := orows.Scan(&key, &code); err != nil {
			orows.Close()
			return nil, err
		}
		if code = strings.ToUpper(strings.TrimSpace(code)); code != "" {
			iata[key] = code
		}
	}
	orows.Close()
	if err := orows.Err(); err != nil {
		return nil, err
	}
	out := map[int64][]string{}
	if len(iata) == 0 {
		return out, nil
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT transmission_id, CAST(`+obsCol+` AS TEXT) FROM observations
		WHERE transmission_id IN (SELECT value FROM json_each(?)) AND `+obsCol+` IS NOT NULL`, idsJSON)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			return nil, err
		}
		if code, ok := iata[key]; ok && !slices.Contains(out[id], code) {
			out[id] = append(out[id], code)
		}
	}
	return out, rows.Err()
}

// --- store and API --------------------------------------------------------

// advertIntervalNodeRows estimates every scanned node that has a nodes row
// (the node list; aged-out keys are left out) and whose identity is not
// hidden (identityHidden: blacklists, hidden-name prefixes), sorted by key.
func advertIntervalNodeRows(scan advertIntervalScan, roster []nodeInfo, cfg *Config) []advertIntervalNodeRow {
	byKey := make(map[string]nodeInfo, len(roster))
	for _, n := range roster {
		byKey[strings.ToLower(n.PublicKey)] = n
	}
	out := make([]advertIntervalNodeRow, 0, len(scan.nodes))
	for pk, s := range scan.nodes {
		n, ok := byKey[pk]
		if !ok || identityHidden(cfg, pk, n.Name) {
			continue
		}
		out = append(out, advertIntervalNodeRow{
			pubkey: pk, name: n.Name, role: n.Role, regions: s.regions,
			flood:       estimateAdvertInterval(s.flood, advertIntervalFlood),
			zeroHop:     estimateAdvertInterval(s.zeroHop, advertIntervalZeroHop),
			floodGapS:   advertMedianGap(s.flood),
			zeroHopGapS: advertMedianGap(s.zeroHop),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].pubkey < out[j].pubkey })
	return out
}

// computeAdvertIntervalAnalytics is the recomputer's pass. It reads the
// database and the node cache only, never s.mu (#495).
func (s *PacketStore) computeAdvertIntervalAnalytics() *advertIntervalSnapshot {
	snap := &advertIntervalSnapshot{all: emptyAdvertIntervalAnalytics()}
	info := AdvertIntervalScanInfo{GeneratedAt: time.Now().UTC().Format(time.RFC3339)}
	if s.db == nil || s.db.conn == nil {
		snap.all.Scan = info
		return snap
	}
	scan, err := s.db.scanAdvertIntervalSamples(advertIntervalScanRowCap)
	if err != nil {
		log.Printf("[advert-intervals] scan failed: %v", err)
	}
	roster, _ := s.getCachedNodesAndPM()
	snap.rows = advertIntervalNodeRows(scan, roster, s.config)
	info.Rows, info.Truncated = scan.rows, scan.truncated
	snap.all = aggregateAdvertIntervals(snap.rows, func(*advertIntervalNodeRow) bool { return true })
	snap.all.Scan = info
	return snap
}

// advertIntervalSnapshot returns the recomputer's snapshot, or computes one
// when the recomputer is not running (tests, the startup window).
func (s *PacketStore) advertIntervalSnapshot() *advertIntervalSnapshot {
	if rc := s.recomputer(&s.recompAdvertIntervals); rc != nil {
		if snap, ok := rc.Load().(*advertIntervalSnapshot); ok && snap != nil {
			return snap
		}
	}
	return s.computeAdvertIntervalAnalytics()
}

// GetAdvertIntervalAnalytics serves the snapshot: unfiltered as computed,
// else re-aggregated over the rows heard in region and placed in area.
func (s *PacketStore) GetAdvertIntervalAnalytics(region, area string) AdvertIntervalAnalyticsResponse {
	snap := s.advertIntervalSnapshot()
	if region == "" && area == "" {
		return snap.all
	}
	var areaNodes map[string]bool
	if area != "" {
		areaNodes = s.resolveAreaNodes(area)
	}
	resp := aggregateAdvertIntervals(snap.rows, advertIntervalFilter(region, areaNodes))
	resp.Scan = snap.all.Scan
	return resp
}

// handleAnalyticsAdvertIntervals serves GET /api/analytics/advert-intervals.
// Every shape reads the recomputer's snapshot, so every response carries
// its #492 headers.
func (s *Server) handleAnalyticsAdvertIntervals(w http.ResponseWriter, r *http.Request) {
	region := r.URL.Query().Get("region")
	area := r.URL.Query().Get("area")
	if s.rejectAnalyticsFilters(w, region, area) {
		return
	}
	if s.store == nil {
		writeJSON(w, emptyAdvertIntervalAnalytics())
		return
	}
	setAnalyticsSnapshotHeaders(w, s.store.recomputer(&s.store.recompAdvertIntervals)) // #492
	writeJSON(w, s.store.GetAdvertIntervalAnalytics(region, area))
}
