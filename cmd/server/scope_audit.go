package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// The observer supplies a snapshot, not a continuously verified declaration.
// Keep the initial evidence-age threshold explicit; a future customizer can
// expose it without changing the findings themselves.
const scopeAuditDeclarationMaxAge = 7 * 24 * time.Hour
const scopeAuditTTL = 30 * time.Second
const scopeAuditQueryTimeout = 20 * time.Second

type ScopeAuditCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}
type ScopeAuditRow struct {
	PublicKey             string            `json:"publicKey"`
	Name                  string            `json:"name"`
	Role                  string            `json:"role"`
	ConfiguredScope       string            `json:"configuredScope"`
	ConfiguredScopeAt     string            `json:"configuredScopeAt"`
	DeclaredScopes        []string          `json:"declaredScopes"`
	AllowsUnscoped        bool              `json:"allowsUnscoped"`
	DeclarationAgeSeconds *int64            `json:"declarationAgeSeconds"`
	StaleDeclaration      bool              `json:"staleDeclaration"`
	ObservedScopes        []ScopeAuditCount `json:"observedScopes"`
	UnscopedObserved      int               `json:"unscopedObserved"`
	UnknownScopeObserved  int               `json:"unknownScopeObserved"`
	AmbiguousHops         int               `json:"ambiguousHops"`
	Forwarded             int               `json:"forwarded"`
	NotObserved           []string          `json:"notObserved"`
	UndeclaredObserved    []string          `json:"undeclaredObserved"`
	WildcardContradiction bool              `json:"wildcardContradiction"`
	IncompleteEvidence    bool              `json:"incompleteEvidence"`
	Status                string            `json:"status"`
}
type ScopeAuditSummary struct {
	Total                 int `json:"total"`
	NotObserved           int `json:"notObserved"`
	UndeclaredObserved    int `json:"undeclaredObserved"`
	WildcardContradiction int `json:"wildcardContradiction"`
	Incomplete            int `json:"incomplete"`
	StaleDeclarations     int `json:"staleDeclarations"`
}
type ScopeAuditResponse struct {
	Window        string            `json:"window"`
	GeneratedAt   string            `json:"generatedAt"`
	Since         string            `json:"since"`
	Summary       ScopeAuditSummary `json:"summary"`
	AmbiguousHops int               `json:"ambiguousHops"`
	Rows          []ScopeAuditRow   `json:"rows"`
}
type scopeAuditCacheEntry struct {
	response *ScopeAuditResponse
	at       time.Time
}
type scopeAuditCache struct {
	mu      sync.Mutex
	entries map[string]scopeAuditCacheEntry
	sf      singleflight.Group
}

func (c *scopeAuditCache) get(window string) *ScopeAuditResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[window]
	if ok && time.Since(e.at) < scopeAuditTTL {
		return e.response
	}
	return nil
}

// load rechecks after entering singleflight so a concurrent cache fill is shared.
func (c *scopeAuditCache) load(window string, build func() (*ScopeAuditResponse, error)) (*ScopeAuditResponse, error) {
	if cached := c.get(window); cached != nil {
		return cached, nil
	}
	value, err, _ := c.sf.Do(window, func() (interface{}, error) {
		if cached := c.get(window); cached != nil {
			return cached, nil
		}
		response, err := build()
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		if c.entries == nil {
			c.entries = make(map[string]scopeAuditCacheEntry, 3)
		}
		c.entries[window] = scopeAuditCacheEntry{response: response, at: time.Now()}
		c.mu.Unlock()
		return response, nil
	})
	if err != nil {
		return nil, err
	}
	return value.(*ScopeAuditResponse), nil
}

func scopeAuditDuration(window string) time.Duration {
	switch window {
	case "1h":
		return time.Hour
	case "7d":
		return 7 * 24 * time.Hour
	default:
		return 24 * time.Hour
	}
}
func normalizeAuditScope(name string) string { return strings.TrimPrefix(strings.TrimSpace(name), "#") }
func declaredAuditScopes(raw string) ([]string, bool) {
	set := make(map[string]struct{})
	wild := false
	for _, part := range strings.Split(raw, ",") {
		name := normalizeAuditScope(part)
		if name == "*" {
			wild = true
		} else if name != "" {
			set[name] = struct{}{}
		}
	}
	scopes := make([]string, 0, len(set))
	for name := range set {
		scopes = append(scopes, name)
	}
	sort.Strings(scopes)
	return scopes, wild
}

type scopeAuditAccumulator struct {
	row    ScopeAuditRow
	counts map[string]int
}

func (s *Server) buildScopeAudit(window string, now time.Time) (*ScopeAuditResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), scopeAuditQueryTimeout)
	defer cancel()
	return s.buildScopeAuditContext(ctx, window, now)
}

// One node snapshot and one streaming observation query, never a scan per node.
// Complexity is O(nodes + observed path hops + distinct ambiguous-prefix
// candidates), with token resolution memoized. Dedup sets hold one transmission
// at a time because SQL orders by t.id. The response/cache has at most 3 windows.
func (s *Server) buildScopeAuditContext(ctx context.Context, window string, now time.Time) (*ScopeAuditResponse, error) {
	if !s.db.hasConfiguredScope() || !s.db.hasScopeName() {
		return nil, fmt.Errorf("scope audit requires configured_scope and scope_name; run ingestor migrations")
	}
	resp := &ScopeAuditResponse{Window: window, GeneratedAt: now.UTC().Format(time.RFC3339), Since: now.Add(-scopeAuditDuration(window)).UTC().Format(time.RFC3339), Rows: []ScopeAuditRow{}}
	rows, err := s.db.conn.QueryContext(ctx, `SELECT public_key,COALESCE(name,''),COALESCE(role,''),configured_scope,COALESCE(configured_scope_at,'') FROM nodes ORDER BY public_key`)
	if err != nil {
		return nil, err
	}
	nodes := []nodeInfo{}
	targets := make(map[string]*scopeAuditAccumulator)
	for rows.Next() {
		var pk, name, role, at string
		var scope sql.NullString
		if err = rows.Scan(&pk, &name, &role, &scope, &at); err != nil {
			rows.Close()
			return nil, err
		}
		pk = strings.ToLower(pk)
		// Include undeclared and hidden nodes in the collision universe. Visibility is
		// applied only when serving the finished result, including cache hits.
		nodes = append(nodes, nodeInfo{PublicKey: pk, Name: name, Role: role})
		if !scope.Valid {
			continue
		}
		declared, wild := declaredAuditScopes(scope.String)
		row := ScopeAuditRow{PublicKey: pk, Name: name, Role: role, ConfiguredScope: scope.String, ConfiguredScopeAt: at, DeclaredScopes: declared, AllowsUnscoped: wild, ObservedScopes: []ScopeAuditCount{}, NotObserved: []string{}, UndeclaredObserved: []string{}, StaleDeclaration: true}
		if stamp, e := parseAnyRFC3339(at); e == nil && !stamp.After(now) {
			age := int64(now.Sub(stamp).Seconds())
			row.DeclarationAgeSeconds = &age
			row.StaleDeclaration = now.Sub(stamp) > scopeAuditDeclarationMaxAge
		}
		targets[pk] = &scopeAuditAccumulator{row: row, counts: make(map[string]int)}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// With no confirmed declaration there is nothing to compare.
	if len(targets) == 0 {
		return resp, nil
	}
	pm := buildPrefixMap(nodes)
	resolve := newRelayTokenResolver(pm)
	// Respect listener-only identities just as the normal relay attribution does.
	listeners, err := s.db.GetNonRelayObserverPubkeys()
	if err != nil {
		return nil, err
	}
	pm.markNonRelay(listeners)
	rawCol := "''"
	if s.db.hasObsRawHex() {
		rawCol = "COALESCE(o.raw_hex,'')"
	}
	// Observation timestamps, rather than first_seen, include retransmissions
	// heard inside the selected window even when their payload was first seen earlier.
	// substr retains only header/transport bytes, avoiding loading full raw frames.
	query := `SELECT t.id,t.route_type,t.scope_name,substr(t.raw_hex,1,10),COALESCE(o.path_json,''),substr(` + rawCol + `,1,10)
 FROM transmissions t JOIN observations o ON o.transmission_id=t.id
 WHERE o.timestamp >= ? AND o.timestamp <= ? ORDER BY t.id`
	var sinceArg, untilArg interface{} = now.Add(-scopeAuditDuration(window)).Unix(), now.Unix()
	if !s.db.isV3() {
		sinceArg, untilArg = resp.Since, resp.GeneratedAt
	}
	obs, err := s.db.conn.QueryContext(ctx, query, sinceArg, untilArg)
	if err != nil {
		return nil, err
	}
	defer obs.Close()
	lastID := -1
	seen := make(map[string]bool)
	ambSeen := make(map[string]bool)
	ambCounts := make(map[string]int)
	for obs.Next() {
		var id int
		var route sql.NullInt64
		var scope sql.NullString
		var canonical, path, raw string
		if err = obs.Scan(&id, &route, &scope, &canonical, &path, &raw); err != nil {
			return nil, err
		}
		if id != lastID {
			clear(seen)
			clear(ambSeen)
			lastID = id
		}
		effectiveRoute := int(route.Int64)
		if !route.Valid {
			effectiveRoute = -1
		}
		if len(raw) >= 2 {
			header, e := strconv.ParseUint(raw[:2], 16, 8)
			if e != nil {
				continue
			}
			effectiveRoute = int(header & 3)
		}
		if effectiveRoute != 0 && effectiveRoute != 1 {
			continue
		} // Packet::isRouteFlood, firmware/src/Packet.h.
		kind := normalizeAuditScope(scope.String)
		if effectiveRoute == 1 {
			kind = "*"
		} else if !scope.Valid || kind == "" || len(raw) > 0 && (len(raw) < 6 || len(canonical) < 6 || !strings.EqualFold(raw[2:6], canonical[2:6])) {
			kind = ""
		}
		visitPathJSONHops(path, func(token string) bool {
			if len(resolve.cache) >= 4096 {
				clear(resolve.cache)
			}
			key := resolve.key(token)
			if key == "" {
				token = strings.ToLower(token)
				if len(pm.m[token]) > 1 && !ambSeen[token] {
					ambSeen[token] = true
					ambCounts[token]++
					resp.AmbiguousHops++
				}
				return true
			}
			target := targets[key]
			if target == nil {
				return true
			}
			// Legacy observations lack their own header; retain the evidence caveat.
			if raw == "" {
				target.row.IncompleteEvidence = true
			}
			// Same target/content transmission is counted once, even when observed by
			// several listeners, or repeated in a looped path. Different scope evidence
			// remains visible because a payload may cross scoped and unscoped boundaries.
			if !seen[key] {
				seen[key] = true
				target.row.Forwarded++
			}
			evidence := key + "|" + kind
			if seen[evidence] {
				return true
			}
			seen[evidence] = true
			switch kind {
			case "*":
				target.row.UnscopedObserved++
			case "":
				target.row.UnknownScopeObserved++
			default:
				target.counts[kind]++
			}
			return true
		})
	}
	if err = obs.Err(); err != nil {
		return nil, err
	}
	// Expand collided tokens only once, after aggregation. A collision on an
	// unrelated prefix cannot mark every declared node's evidence incomplete.
	for token, count := range ambCounts {
		for _, n := range pm.m[token] {
			if target := targets[strings.ToLower(n.PublicKey)]; target != nil {
				target.row.AmbiguousHops += count
			}
		}
	}
	for _, target := range targets {
		finishScopeAuditRow(target)
		resp.Rows = append(resp.Rows, target.row)
	}
	sort.Slice(resp.Rows, func(i, j int) bool {
		a, b := resp.Rows[i], resp.Rows[j]
		if auditPriority(a) != auditPriority(b) {
			return auditPriority(a) > auditPriority(b)
		}
		return a.PublicKey < b.PublicKey
	})
	resp.Summary = summarizeScopeAudit(resp.Rows)
	return resp, nil
}

// Findings outrank quiet declarations; incompleteness remains explicit in status.
func auditPriority(r ScopeAuditRow) int {
	if r.WildcardContradiction || len(r.UndeclaredObserved) > 0 {
		return 4
	}
	if r.UnknownScopeObserved > 0 || r.AmbiguousHops > 0 {
		return 3
	}
	if r.Forwarded > 0 && len(r.NotObserved) > 0 {
		return 2
	}
	if r.Forwarded > 0 {
		return 1
	}
	return 0
}
func finishScopeAuditRow(a *scopeAuditAccumulator) {
	r := &a.row
	declared := make(map[string]bool, len(r.DeclaredScopes))
	for _, name := range r.DeclaredScopes {
		declared[name] = true
		if a.counts[name] == 0 {
			r.NotObserved = append(r.NotObserved, name)
		}
	}
	for name, count := range a.counts {
		r.ObservedScopes = append(r.ObservedScopes, ScopeAuditCount{Name: name, Count: count})
		if !declared[name] {
			r.UndeclaredObserved = append(r.UndeclaredObserved, name)
		}
	}
	sort.Slice(r.ObservedScopes, func(i, j int) bool { return r.ObservedScopes[i].Name < r.ObservedScopes[j].Name })
	sort.Strings(r.UndeclaredObserved)
	r.WildcardContradiction = !r.AllowsUnscoped && r.UnscopedObserved > 0
	r.IncompleteEvidence = r.IncompleteEvidence || r.Forwarded == 0 || r.UnknownScopeObserved > 0 || r.AmbiguousHops > 0 || r.StaleDeclaration
	switch {
	case r.Forwarded == 0:
		r.Status = "no-evidence"
	case r.IncompleteEvidence:
		r.Status = "incomplete"
	case len(r.NotObserved) > 0 || len(r.UndeclaredObserved) > 0 || r.WildcardContradiction:
		r.Status = "findings"
	default:
		r.Status = "consistent"
	}
}
func summarizeScopeAudit(rows []ScopeAuditRow) ScopeAuditSummary {
	summary := ScopeAuditSummary{Total: len(rows)}
	for _, r := range rows {
		if len(r.NotObserved) > 0 {
			summary.NotObserved++
		}
		if len(r.UndeclaredObserved) > 0 {
			summary.UndeclaredObserved++
		}
		if r.WildcardContradiction {
			summary.WildcardContradiction++
		}
		if r.IncompleteEvidence {
			summary.Incomplete++
		}
		if r.StaleDeclaration {
			summary.StaleDeclarations++
		}
	}
	return summary
}
func (s *Server) handleScopeAudit(w http.ResponseWriter, r *http.Request) {
	window := r.URL.Query().Get("window")
	if window == "" {
		window = "24h"
	}
	if window != "1h" && window != "24h" && window != "7d" {
		writeError(w, 400, "window must be 1h, 24h, or 7d")
		return
	}
	response, err := s.scopeAudit.load(window, func() (*ScopeAuditResponse, error) {
		ctx, cancel := context.WithTimeout(r.Context(), scopeAuditQueryTimeout)
		defer cancel()
		return s.buildScopeAuditContext(ctx, window, time.Now())
	})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	keys := make([]string, len(response.Rows))
	for i, row := range response.Rows {
		keys[i] = row.PublicKey
	}
	names, err := s.hiddenIdentityNames(r.Context(), keys)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	filtered := *response
	filtered.Rows = make([]ScopeAuditRow, 0, len(response.Rows))
	for _, row := range response.Rows {
		allNames := append([]string{row.Name}, names[row.PublicKey]...)
		if !identityHidden(s.cfg, row.PublicKey, allNames...) {
			filtered.Rows = append(filtered.Rows, row)
		}
	}
	filtered.Summary = summarizeScopeAudit(filtered.Rows)
	writeJSON(w, &filtered)
}
