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

// scopeAuditCacheDefaultMax bounds a cache whose max is unset: the declared
// audit has exactly three keys (its windows).
const scopeAuditCacheDefaultMax = 3

type scopeAuditCacheEntry[T any] struct {
	response *T
	at       time.Time
}

// scopeAuditCache is a TTL cache with singleflight and at most max entries.
type scopeAuditCache[T any] struct {
	mu      sync.Mutex
	entries map[string]scopeAuditCacheEntry[T]
	sf      singleflight.Group
	max     int
	// recheckHook, if non-nil, runs once after the outer cache miss and
	// before entering singleflight. Tests use it to land a concurrent fill
	// in that gap and prove the recheck inside Do reuses it instead of
	// rebuilding; nil (zero overhead) in production.
	recheckHook func()
	// buildHook, if non-nil, runs before every build. Tests count builds.
	buildHook func()
}

func (c *scopeAuditCache[T]) get(key string) *T {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if ok && time.Since(e.at) < scopeAuditTTL {
		return e.response
	}
	return nil
}

// storeLocked inserts key, first evicting expired entries and then the oldest
// one while the cache is full, so keys from request parameters stay bounded.
func (c *scopeAuditCache[T]) storeLocked(key string, response *T, now time.Time) {
	limit := c.max
	if limit <= 0 {
		limit = scopeAuditCacheDefaultMax
	}
	if c.entries == nil {
		c.entries = make(map[string]scopeAuditCacheEntry[T], limit)
	}
	if _, exists := c.entries[key]; !exists && len(c.entries) >= limit {
		for k, e := range c.entries {
			if now.Sub(e.at) >= scopeAuditTTL {
				delete(c.entries, k)
			}
		}
		for len(c.entries) >= limit {
			oldest := ""
			for k, e := range c.entries {
				if oldest == "" || e.at.Before(c.entries[oldest].at) {
					oldest = k
				}
			}
			delete(c.entries, oldest)
		}
	}
	c.entries[key] = scopeAuditCacheEntry[T]{response: response, at: now}
}

// load rechecks after entering singleflight so a concurrent cache fill is shared.
func (c *scopeAuditCache[T]) load(key string, build func() (*T, error)) (*T, error) {
	if cached := c.get(key); cached != nil {
		return cached, nil
	}
	if c.recheckHook != nil {
		c.recheckHook()
	}
	value, err, _ := c.sf.Do(key, func() (interface{}, error) {
		if cached := c.get(key); cached != nil {
			return cached, nil
		}
		if c.buildHook != nil {
			c.buildHook()
		}
		response, err := build()
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.storeLocked(key, response, time.Now())
		c.mu.Unlock()
		return response, nil
	})
	if err != nil {
		return nil, err
	}
	return value.(*T), nil
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

// scopeAuditNode is one row of the node snapshot both audit views read.
// scope is NULL when the node was never asked for a declaration.
type scopeAuditNode struct {
	info  nodeInfo
	scope sql.NullString
	at    string
}

// scopeAuditNodes reads the node snapshot with one bulk query. Undeclared and
// hidden nodes are included: they belong to the hop-collision universe, and
// visibility is applied only when a finished result is served (cache hits
// included). Without the configured_scope column every node is undeclared.
func (s *Server) scopeAuditNodes(ctx context.Context) ([]scopeAuditNode, error) {
	cols := "configured_scope,COALESCE(configured_scope_at,'')"
	if !s.db.hasConfiguredScope() {
		cols = "NULL,''"
	}
	rows, err := s.db.conn.QueryContext(ctx, `SELECT public_key,COALESCE(name,''),COALESCE(role,''),`+cols+` FROM nodes ORDER BY public_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	nodes := []scopeAuditNode{}
	for rows.Next() {
		var n scopeAuditNode
		if err := rows.Scan(&n.info.PublicKey, &n.info.Name, &n.info.Role, &n.scope, &n.at); err != nil {
			return nil, err
		}
		n.info.PublicKey = strings.ToLower(n.info.PublicKey)
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// scopeAuditPrefixMap builds the collision-safe hop resolver input and
// respects listener-only identities just as the normal relay attribution does.
func (s *Server) scopeAuditPrefixMap(nodes []scopeAuditNode) (*prefixMap, error) {
	infos := make([]nodeInfo, len(nodes))
	for i, n := range nodes {
		infos[i] = n.info
	}
	pm := buildPrefixMap(infos)
	listeners, err := s.db.GetNonRelayObserverPubkeys()
	if err != nil {
		return nil, err
	}
	pm.markNonRelay(listeners)
	return pm, nil
}

// scopeDeclarationAge returns a declaration's age and whether it is stale:
// older than scopeAuditDeclarationMaxAge, unparseable or future-dated.
func scopeDeclarationAge(at string, now time.Time) (*int64, bool) {
	stamp, err := parseAnyRFC3339(at)
	if err != nil || stamp.After(now) {
		return nil, true
	}
	age := int64(now.Sub(stamp).Seconds())
	return &age, now.Sub(stamp) > scopeAuditDeclarationMaxAge
}

// scopeEvidenceSink receives one window scan in transmission order: tx at
// every new transmission, obs once per flood observation and hop once per raw
// path hop. kind is "*" for unscoped flood, "" for scope evidence that cannot
// be trusted (unknown, or the observation's own transport code differs from
// the canonical frame's) and otherwise the normalised scope name. key is the
// confirmed relay key, or "" when token does not resolve to exactly one relay.
// legacy marks an observation stored without its own frame header.
type scopeEvidenceSink struct {
	tx  func()
	obs func(kind string, legacy bool)
	hop func(key, token, kind string, legacy bool)
}

// scanScopeEvidence is the one streaming observation query both audit views
// share, never a scan per node. observers, when non-nil, restricts the scan to
// those observer keys (observers.rowid on v3, observers.id on v2). The query
// runs on the read-only connection and never touches PacketStore.mu.
func (s *Server) scanScopeEvidence(ctx context.Context, since, until time.Time, observers []interface{}, pm *prefixMap, sink scopeEvidenceSink) error {
	resolve := newRelayTokenResolver(pm)
	rawCol := "''"
	if s.db.hasObsRawHex() {
		rawCol = "COALESCE(o.raw_hex,'')"
	}
	var sinceArg, untilArg interface{} = since.Unix(), until.Unix()
	if !s.db.isV3() {
		sinceArg, untilArg = since.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339)
	}
	args := []interface{}{sinceArg, untilArg}
	observerClause := ""
	if observers != nil {
		// Unary + keeps the plan on the timestamp window: the observer index
		// would read every observation those observers ever made.
		col := "+o.observer_idx"
		if !s.db.isV3() {
			col = "o.observer_id"
		}
		observerClause = " AND " + col + " IN (" + strings.TrimSuffix(strings.Repeat("?,", len(observers)), ",") + ")"
		args = append(args, observers...)
	}
	// Observation timestamps, rather than first_seen, include retransmissions
	// heard inside the selected window even when their payload was first seen earlier.
	// substr retains only header/transport bytes, avoiding loading full raw frames.
	query := `SELECT t.id,t.route_type,t.scope_name,substr(t.raw_hex,1,10),COALESCE(o.path_json,''),substr(` + rawCol + `,1,10)
 FROM transmissions t JOIN observations o ON o.transmission_id=t.id
 WHERE o.timestamp >= ? AND o.timestamp <= ?` + observerClause + ` ORDER BY t.id`
	obs, err := s.db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer obs.Close()
	lastID := -1
	for obs.Next() {
		var id int
		var route sql.NullInt64
		var scope sql.NullString
		var canonical, path, raw string
		if err = obs.Scan(&id, &route, &scope, &canonical, &path, &raw); err != nil {
			return err
		}
		if id != lastID {
			sink.tx()
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
		legacy := raw == ""
		if sink.obs != nil {
			sink.obs(kind, legacy)
		}
		visitPathJSONHops(path, func(token string) bool {
			if len(resolve.cache) >= 4096 {
				clear(resolve.cache)
			}
			sink.hop(resolve.key(token), strings.ToLower(token), kind, legacy)
			return true
		})
	}
	return obs.Err()
}

// Complexity is O(nodes + observed path hops + distinct ambiguous-prefix
// candidates), with token resolution memoized. Dedup sets hold one transmission
// at a time because SQL orders by t.id. The response/cache has at most 3 windows.
func (s *Server) buildScopeAuditContext(ctx context.Context, window string, now time.Time) (*ScopeAuditResponse, error) {
	if !s.db.hasConfiguredScope() || !s.db.hasScopeName() {
		return nil, fmt.Errorf("scope audit requires configured_scope and scope_name; run ingestor migrations")
	}
	since := now.Add(-scopeAuditDuration(window))
	resp := &ScopeAuditResponse{Window: window, GeneratedAt: now.UTC().Format(time.RFC3339), Since: since.UTC().Format(time.RFC3339), Rows: []ScopeAuditRow{}}
	nodes, err := s.scopeAuditNodes(ctx)
	if err != nil {
		return nil, err
	}
	targets := make(map[string]*scopeAuditAccumulator)
	for _, n := range nodes {
		if !n.scope.Valid {
			continue
		}
		declared, wild := declaredAuditScopes(n.scope.String)
		row := ScopeAuditRow{PublicKey: n.info.PublicKey, Name: n.info.Name, Role: n.info.Role, ConfiguredScope: n.scope.String, ConfiguredScopeAt: n.at, DeclaredScopes: declared, AllowsUnscoped: wild, ObservedScopes: []ScopeAuditCount{}, NotObserved: []string{}, UndeclaredObserved: []string{}}
		row.DeclarationAgeSeconds, row.StaleDeclaration = scopeDeclarationAge(n.at, now)
		targets[n.info.PublicKey] = &scopeAuditAccumulator{row: row, counts: make(map[string]int)}
	}
	// With no confirmed declaration there is nothing to compare.
	if len(targets) == 0 {
		return resp, nil
	}
	pm, err := s.scopeAuditPrefixMap(nodes)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	ambSeen := make(map[string]bool)
	ambCounts := make(map[string]int)
	err = s.scanScopeEvidence(ctx, since, now, nil, pm, scopeEvidenceSink{
		tx: func() { clear(seen); clear(ambSeen) },
		hop: func(key, token, kind string, legacy bool) {
			if key == "" {
				if len(pm.m[token]) > 1 && !ambSeen[token] {
					ambSeen[token] = true
					ambCounts[token]++
					resp.AmbiguousHops++
				}
				return
			}
			target := targets[key]
			if target == nil {
				return
			}
			// Legacy observations lack their own header; retain the evidence caveat.
			if legacy {
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
				return
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
		},
	})
	if err != nil {
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
		// The build is shared across every coalesced singleflight waiter, so it
		// must not die with the first caller's connection: detach from its
		// cancellation and keep only the absolute deadline.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), scopeAuditQueryTimeout)
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
