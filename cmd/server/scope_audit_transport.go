package main

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Transport view of the Scope Audit (#414): every repeater seen forwarding
// flood traffic in the window, including those never asked for a declaration,
// with carrier/non-carrier evidence for one selected scope. It reads the same
// single evidence scan as the declaration audit (scanScopeEvidence) on demand,
// never periodically, and never through PacketStore.mu.
//
// Initial thresholds and limits; candidates for the customizer (AGENTS.md
// rule 8) in a later milestone.
const (
	// Fewer forwarded flood transmissions than this are too little traffic
	// to report a repeater as not seen carrying a scope.
	scopeTransportMinForwarded = 5
	// Cached scans: one per window × normalised observer region.
	scopeTransportCacheMax       = 8
	scopeTransportMaxRegionCodes = 32
	scopeTransportMaxScopeLen    = 64
	// Distinct scope names one scan tracks; later names count as unknown
	// scope evidence and set scopesTruncated.
	scopeTransportMaxScopes = 256
	// Rows in one response, after visibility filtering.
	scopeTransportMaxRows = 5000
	// Population scopes listed as suggestions.
	scopeTransportListedScopes = 50
)

type ScopeTransportDeclaration struct {
	ConfiguredScope   string   `json:"configuredScope"`
	ConfiguredScopeAt string   `json:"configuredScopeAt"`
	DeclaredScopes    []string `json:"declaredScopes"`
	AllowsUnscoped    bool     `json:"allowsUnscoped"`
	StaleDeclaration  bool     `json:"staleDeclaration"`
	// DeclaresScope is set only when a scope is selected.
	DeclaresScope *bool `json:"declaresScope"`
}

// ScopeTransportRow is one repeater seen forwarding. Counts are distinct
// transmissions; one transmission can count under several scope kinds when
// its observations disagree. Carrier, NonCarrier and AmbiguousScopeHops are
// null without a selected scope.
type ScopeTransportRow struct {
	PublicKey string `json:"publicKey"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	// Declaration is null when the repeater was never asked for one, which is
	// not the same as an empty declaration.
	Declaration             *ScopeTransportDeclaration `json:"declaration"`
	Forwarded               int                        `json:"forwarded"`
	Carrier                 *int                       `json:"carrier"`
	NonCarrier              *int                       `json:"nonCarrier"`
	Unscoped                int                        `json:"unscoped"`
	UnknownScope            int                        `json:"unknownScope"`
	ObservedScopes          []ScopeAuditCount          `json:"observedScopes"`
	ObservedScopesTruncated bool                       `json:"observedScopesTruncated"`
	// AmbiguousHops counts hop observations whose prefix matches this and at
	// least one other repeater; they are credited to none of them.
	AmbiguousHops      int  `json:"ambiguousHops"`
	AmbiguousScopeHops *int `json:"ambiguousScopeHops"`
	// LegacyObservations counts transmissions seen only through frames stored
	// without their own header.
	LegacyObservations int    `json:"legacyObservations"`
	Evidence           string `json:"evidence"`
}
type ScopeTransportPopulation struct {
	Transmissions      int  `json:"transmissions"`
	Observations       int  `json:"observations"`
	ScopeTransmissions *int `json:"scopeTransmissions"`
	AmbiguousHops      int  `json:"ambiguousHops"`
	LegacyObservations int  `json:"legacyObservations"`
	// RegionObservers is null without a region filter.
	RegionObservers *int `json:"regionObservers"`
}

// ScopeTransportCoverage reports the oldest stored observation: a window
// that starts earlier than that is not fully captured.
type ScopeTransportCoverage struct {
	OldestObservation *string `json:"oldestObservation"`
	Complete          bool    `json:"complete"`
}
type ScopeTransportSummary struct {
	Total           int `json:"total"`
	Carrier         int `json:"carrier"`
	Ambiguous       int `json:"ambiguous"`
	Incomplete      int `json:"incomplete"`
	LowEvidence     int `json:"lowEvidence"`
	NoScopeTraffic  int `json:"noScopeTraffic"`
	NotSeenCarrying int `json:"notSeenCarrying"`
	NeverQueried    int `json:"neverQueried"`
	Declared        int `json:"declared"`
}
type ScopeTransportResponse struct {
	Window          string                   `json:"window"`
	GeneratedAt     string                   `json:"generatedAt"`
	Since           string                   `json:"since"`
	Scope           string                   `json:"scope"`
	Region          []string                 `json:"region"`
	MinForwarded    int                      `json:"minForwarded"`
	Coverage        ScopeTransportCoverage   `json:"coverage"`
	Population      ScopeTransportPopulation `json:"population"`
	Summary         ScopeTransportSummary    `json:"summary"`
	Scopes          []ScopeAuditCount        `json:"scopes"`
	ScopesTruncated bool                     `json:"scopesTruncated"`
	Rows            []ScopeTransportRow      `json:"rows"`
	Truncated       bool                     `json:"truncated"`
}

// scopeTransportKey is a cached scan: region is the normalised codes joined
// by ",", empty for all observers. The selected scope is not part of the key:
// one scan serves every scope.
type scopeTransportKey struct{ window, region string }

func (k scopeTransportKey) String() string { return k.window + "|" + k.region }

type scopeTransportAcc struct {
	node              scopeAuditNode
	forwarded, legacy int
	counts            map[string]int // scope kind → distinct transmissions
	amb               map[string]int // scope kind → ambiguous hop observations
}

type scopeTransportScan struct {
	generatedAt, since time.Time
	region             []string
	regionObservers    *int
	oldest             *time.Time
	population         ScopeTransportPopulation
	scopeTx            map[string]int // scope kind → distinct transmissions
	scopesTruncated    bool
	rows               []*scopeTransportAcc // by public key
}

// classifyScopeTransport labels one repeater's evidence for the selected
// scope. Absence is never a configuration verdict: ambiguous hops, unknown
// scopes and legacy frames may hide the scope, a population without the
// scope says nothing about anyone, and little traffic is too little to judge.
func classifyScopeTransport(forwarded, carrier, ambScope, unknown, legacy, scopeTraffic int) string {
	switch {
	case carrier > 0:
		return "carrier"
	case ambScope > 0:
		return "ambiguous"
	case unknown > 0 || legacy > 0:
		return "incomplete"
	case scopeTraffic == 0:
		return "no-scope-traffic"
	case forwarded < scopeTransportMinForwarded:
		return "low-evidence"
	default:
		return "not-seen-carrying"
	}
}

// Non-carriers first: the rows a scope rollout still has to explain.
var scopeTransportEvidenceOrder = map[string]int{"not-seen-carrying": 0, "ambiguous": 1, "incomplete": 2, "low-evidence": 3, "no-scope-traffic": 4, "carrier": 5}

// scopeTransportRegion normalises a region parameter like every other
// endpoint (normalizeRegionCodes, "All" = no filter), sorted and deduplicated
// so equal selections share one cache entry.
func scopeTransportRegion(raw string) ([]string, error) {
	codes := normalizeRegionCodes(raw)
	if len(codes) == 0 {
		return []string{}, nil
	}
	sort.Strings(codes)
	out := codes[:1]
	for _, c := range codes[1:] {
		if c != out[len(out)-1] {
			out = append(out, c)
		}
	}
	if len(out) > scopeTransportMaxRegionCodes {
		return nil, fmt.Errorf("at most %d region codes", scopeTransportMaxRegionCodes)
	}
	return out, nil
}

// scopeAuditRegionObservers returns the observer keys scanScopeEvidence
// filters on (rowid on v3, id on v2) for the IATA codes, in one query.
// Blacklisted observers are left out; the result is non-nil, so a region
// without observers matches nothing instead of widening to all.
func (s *Server) scopeAuditRegionObservers(ctx context.Context, codes []string) ([]interface{}, error) {
	col := "rowid"
	if !s.db.isV3() {
		col = "id"
	}
	args := make([]interface{}, len(codes))
	for i, c := range codes {
		args[i] = c
	}
	rows, err := s.db.conn.QueryContext(ctx, `SELECT `+col+`,id FROM observers WHERE UPPER(TRIM(iata)) IN (`+strings.TrimSuffix(strings.Repeat("?,", len(codes)), ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []interface{}{}
	for rows.Next() {
		var key interface{}
		var id string
		if err := rows.Scan(&key, &id); err != nil {
			return nil, err
		}
		if !s.cfg.IsObserverBlacklisted(id) {
			out = append(out, key)
		}
	}
	return out, rows.Err()
}

// oldestScopeObservation is the capture start: MIN over the timestamp index.
func (s *Server) oldestScopeObservation(ctx context.Context) (*time.Time, error) {
	var raw interface{}
	if err := s.db.conn.QueryRowContext(ctx, `SELECT MIN(timestamp) FROM observations`).Scan(&raw); err != nil {
		return nil, err
	}
	var t time.Time
	switch v := raw.(type) {
	case int64:
		t = time.Unix(v, 0).UTC()
	case string:
		parsed, err := parseAnyRFC3339(v)
		if err != nil {
			return nil, nil
		}
		t = parsed.UTC()
	case []byte:
		parsed, err := parseAnyRFC3339(string(v))
		if err != nil {
			return nil, nil
		}
		t = parsed.UTC()
	default:
		return nil, nil
	}
	return &t, nil
}

func (s *Server) buildScopeTransportScan(key scopeTransportKey, now time.Time) (*scopeTransportScan, error) {
	ctx, cancel := context.WithTimeout(context.Background(), scopeAuditQueryTimeout)
	defer cancel()
	return s.buildScopeTransportScanContext(ctx, key, now)
}

// buildScopeTransportScanContext is one node snapshot, at most one small
// observer query, one MIN and one streaming observation query. Work is
// O(nodes + observed path hops); memory is one accumulator per repeater seen
// forwarding, with at most scopeTransportMaxScopes scope kinds anywhere.
// Dedup sets hold one transmission at a time because SQL orders by t.id.
func (s *Server) buildScopeTransportScanContext(ctx context.Context, key scopeTransportKey, now time.Time) (*scopeTransportScan, error) {
	if !s.db.hasScopeName() {
		return nil, fmt.Errorf("scope transport view requires scope_name; run ingestor migrations")
	}
	scan := &scopeTransportScan{generatedAt: now, since: now.Add(-scopeAuditDuration(key.window)), region: []string{}, scopeTx: map[string]int{}}
	var observers []interface{}
	if key.region != "" {
		scan.region = strings.Split(key.region, ",")
		var err error
		if observers, err = s.scopeAuditRegionObservers(ctx, scan.region); err != nil {
			return nil, err
		}
		n := len(observers)
		scan.population.RegionObservers = &n
	}
	var err error
	if scan.oldest, err = s.oldestScopeObservation(ctx); err != nil {
		return nil, err
	}
	if observers != nil && len(observers) == 0 {
		return scan, nil
	}
	nodes, err := s.scopeAuditNodes(ctx)
	if err != nil {
		return nil, err
	}
	pm, err := s.scopeAuditPrefixMap(nodes)
	if err != nil {
		return nil, err
	}
	relays := make(map[string]int, len(nodes))
	for i, n := range nodes {
		if canAppearInPath(n.info.Role) {
			relays[n.info.PublicKey] = i
		}
	}
	accs := make(map[string]*scopeTransportAcc)
	tracked := make(map[string]struct{})
	// Fold a scope beyond the tracking bound into unknown: never dropped,
	// never silently counted as "not carrying".
	track := func(kind string) string {
		if kind == "" || kind == "*" {
			return kind
		}
		if _, ok := tracked[kind]; ok {
			return kind
		}
		if len(tracked) >= scopeTransportMaxScopes {
			scan.scopesTruncated = true
			return ""
		}
		tracked[kind] = struct{}{}
		return kind
	}
	seen := make(map[string]bool)
	// Legacy-frame dedup lives in its own set: a scope may be named "legacy",
	// so no key shape in seen is safe from a scope name.
	seenLegacy := make(map[string]bool)
	txKinds := make(map[string]bool)
	flood := false
	ambCounts := make(map[string]map[string]int)
	err = s.scanScopeEvidence(ctx, scan.since, now, observers, pm, scopeEvidenceSink{
		tx: func() { clear(seen); clear(seenLegacy); clear(txKinds); flood = false },
		obs: func(kind string, legacy bool) {
			if !flood {
				flood = true
				scan.population.Transmissions++
			}
			scan.population.Observations++
			if legacy {
				scan.population.LegacyObservations++
			}
			if kind = track(kind); !txKinds[kind] {
				txKinds[kind] = true
				scan.scopeTx[kind]++
			}
		},
		hop: func(relay, token, kind string, legacy bool) {
			kind = track(kind)
			if relay == "" {
				if len(pm.m[token]) > 1 && !seen["?"+token+"|"+kind] {
					seen["?"+token+"|"+kind] = true
					if ambCounts[token] == nil {
						ambCounts[token] = make(map[string]int)
					}
					ambCounts[token][kind]++
					scan.population.AmbiguousHops++
				}
				return
			}
			i, ok := relays[relay]
			if !ok {
				return
			}
			acc := accs[relay]
			if acc == nil {
				acc = &scopeTransportAcc{node: nodes[i], counts: make(map[string]int), amb: make(map[string]int)}
				accs[relay] = acc
			}
			if !seen[relay] {
				seen[relay] = true
				acc.forwarded++
			}
			if legacy && !seenLegacy[relay] {
				seenLegacy[relay] = true
				acc.legacy++
			}
			if evidence := relay + "|" + kind; !seen[evidence] {
				seen[evidence] = true
				acc.counts[kind]++
			}
		},
	})
	if err != nil {
		return nil, err
	}
	// Ambiguous hops are expanded once, after aggregation, and only onto
	// repeaters with attributed forwarding: a repeater seen only through
	// shared prefixes cannot be listed as a forwarder.
	for token, kinds := range ambCounts {
		for _, n := range pm.m[token] {
			if acc := accs[strings.ToLower(n.PublicKey)]; acc != nil {
				for kind, count := range kinds {
					acc.amb[kind] += count
				}
			}
		}
	}
	scan.rows = make([]*scopeTransportAcc, 0, len(accs))
	for _, acc := range accs {
		scan.rows = append(scan.rows, acc)
	}
	sort.Slice(scan.rows, func(i, j int) bool { return scan.rows[i].node.info.PublicKey < scan.rows[j].node.info.PublicKey })
	return scan, nil
}

// namedScopeCounts lists the named scope kinds of counts, most first.
func namedScopeCounts(counts map[string]int, limit int) ([]ScopeAuditCount, bool) {
	out := make([]ScopeAuditCount, 0, len(counts))
	for name, count := range counts {
		if name != "" && name != "*" {
			out = append(out, ScopeAuditCount{Name: name, Count: count})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > limit {
		return out[:limit], true
	}
	return out, false
}

func scopeTransportRow(acc *scopeTransportAcc, scope string, scopeTraffic int, now time.Time) ScopeTransportRow {
	n := acc.node
	row := ScopeTransportRow{PublicKey: n.info.PublicKey, Name: n.info.Name, Role: n.info.Role, Forwarded: acc.forwarded,
		Unscoped: acc.counts["*"], UnknownScope: acc.counts[""], LegacyObservations: acc.legacy, Evidence: "forwarding"}
	row.ObservedScopes, row.ObservedScopesTruncated = namedScopeCounts(acc.counts, maxTransportedScopes)
	for _, count := range acc.amb {
		row.AmbiguousHops += count
	}
	if n.scope.Valid {
		declared, wild := declaredAuditScopes(n.scope.String)
		d := &ScopeTransportDeclaration{ConfiguredScope: n.scope.String, ConfiguredScopeAt: n.at, DeclaredScopes: declared, AllowsUnscoped: wild}
		_, d.StaleDeclaration = scopeDeclarationAge(n.at, now)
		if scope != "" {
			declares := false
			for _, name := range declared {
				declares = declares || name == scope
			}
			d.DeclaresScope = &declares
		}
		row.Declaration = d
	}
	if scope != "" {
		carrier, ambScope := acc.counts[scope], acc.amb[scope]
		nonCarrier := acc.forwarded - carrier
		row.Carrier, row.NonCarrier, row.AmbiguousScopeHops = &carrier, &nonCarrier, &ambScope
		row.Evidence = classifyScopeTransport(acc.forwarded, carrier, ambScope, row.UnknownScope, acc.legacy, scopeTraffic)
	}
	return row
}

// scopeTransportResponse serves one scope from a cached scan. visible decides
// identity visibility live, so a cache hit never leaks a newly hidden node;
// summary counts are taken from the visible rows only.
func scopeTransportResponse(scan *scopeTransportScan, window, scope string, visible func(*scopeTransportAcc) bool) *ScopeTransportResponse {
	resp := &ScopeTransportResponse{Window: window, GeneratedAt: scan.generatedAt.UTC().Format(time.RFC3339), Since: scan.since.UTC().Format(time.RFC3339),
		Scope: scope, Region: scan.region, MinForwarded: scopeTransportMinForwarded, Population: scan.population, Rows: []ScopeTransportRow{}}
	if scan.oldest != nil {
		stamp := scan.oldest.Format(time.RFC3339)
		resp.Coverage = ScopeTransportCoverage{OldestObservation: &stamp, Complete: !scan.oldest.After(scan.since)}
	}
	scopeTraffic := scan.scopeTx[scope]
	if scope != "" {
		resp.Population.ScopeTransmissions = &scopeTraffic
	}
	var listedTruncated bool
	resp.Scopes, listedTruncated = namedScopeCounts(scan.scopeTx, scopeTransportListedScopes)
	resp.ScopesTruncated = scan.scopesTruncated || listedTruncated
	for _, acc := range scan.rows {
		if visible(acc) {
			resp.Rows = append(resp.Rows, scopeTransportRow(acc, scope, scopeTraffic, scan.generatedAt))
		}
	}
	sort.SliceStable(resp.Rows, func(i, j int) bool {
		a, b := resp.Rows[i], resp.Rows[j]
		if oa, ob := scopeTransportEvidenceOrder[a.Evidence], scopeTransportEvidenceOrder[b.Evidence]; oa != ob {
			return oa < ob
		}
		return a.Forwarded > b.Forwarded
	})
	if len(resp.Rows) > scopeTransportMaxRows {
		resp.Rows, resp.Truncated = resp.Rows[:scopeTransportMaxRows], true
	}
	sum := &resp.Summary
	for _, r := range resp.Rows {
		sum.Total++
		if r.Declaration == nil {
			sum.NeverQueried++
		} else {
			sum.Declared++
		}
		switch r.Evidence {
		case "carrier":
			sum.Carrier++
		case "ambiguous":
			sum.Ambiguous++
		case "incomplete":
			sum.Incomplete++
		case "low-evidence":
			sum.LowEvidence++
		case "no-scope-traffic":
			sum.NoScopeTraffic++
		case "not-seen-carrying":
			sum.NotSeenCarrying++
		}
	}
	return resp
}

func (s *Server) handleScopeTransport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	window := q.Get("window")
	if window == "" {
		window = "24h"
	}
	if window != "1h" && window != "24h" && window != "7d" {
		writeError(w, 400, "window must be 1h, 24h, or 7d")
		return
	}
	region, err := scopeTransportRegion(q.Get("region"))
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	// Firmware matches region names exactly (RegionMap::findByName, strcmp);
	// only the optional leading '#' is dropped, as in declarations.
	scope := normalizeAuditScope(q.Get("scope"))
	if scope == "*" || len(scope) > scopeTransportMaxScopeLen {
		writeError(w, 400, fmt.Sprintf("scope must be a region scope name of at most %d bytes", scopeTransportMaxScopeLen))
		return
	}
	key := scopeTransportKey{window: window, region: strings.Join(region, ",")}
	scan, err := s.scopeTransport.load(key.String(), func() (*scopeTransportScan, error) {
		// Shared by every coalesced waiter: keep only the absolute deadline.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), scopeAuditQueryTimeout)
		defer cancel()
		return s.buildScopeTransportScanContext(ctx, key, time.Now())
	})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	keys := make([]string, len(scan.rows))
	for i, acc := range scan.rows {
		keys[i] = acc.node.info.PublicKey
	}
	names, err := s.hiddenIdentityNames(r.Context(), keys)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, scopeTransportResponse(scan, window, scope, func(acc *scopeTransportAcc) bool {
		pk := acc.node.info.PublicKey
		return !identityHidden(s.cfg, pk, append([]string{acc.node.info.Name}, names[pk]...)...)
	}))
}
