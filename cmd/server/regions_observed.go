package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/meshcore-analyzer/regions"
)

const maxObservedRegionNodes = 5000

type regionScopeRelay struct {
	name string
	when time.Time
}

type repeaterRelaySnapshot struct {
	info              map[string]RepeaterRelayInfo
	regionMemberships *regionsMembershipSnapshot
	history           *regionsRelayHistory
	routes            *regionsRouteSnapshot
	generatedAt       time.Time
	coverageStart     time.Time
	loading           bool
}

type RegionsObservedCoverage struct {
	Start    *string         `json:"start"`
	Complete bool            `json:"complete"`
	Loading  bool            `json:"loading"`
	Evidence RegionsEvidence `json:"evidence"`
}

type RegionsObservedScope struct {
	Name        string `json:"name"`
	LastRelayed string `json:"lastRelayed"`
}

type RegionsObservedNode struct {
	PublicKey      string                 `json:"publicKey"`
	Name           string                 `json:"name"`
	Role           string                 `json:"role"`
	Lat            *float64               `json:"lat"`
	Lon            *float64               `json:"lon"`
	PositionSource string                 `json:"positionSource"`
	Scopes         []RegionsObservedScope `json:"scopes"`
}

type RegionsObservedResponse struct {
	Window          string                  `json:"window"`
	GeneratedAt     string                  `json:"generatedAt"`
	WindowStart     string                  `json:"windowStart"`
	WindowEnd       string                  `json:"windowEnd"`
	Coverage        RegionsObservedCoverage `json:"coverage"`
	Nodes           []RegionsObservedNode   `json:"nodes"`
	Truncated       bool                    `json:"truncated"`
	ScopesTruncated bool                    `json:"scopesTruncated"`
}

// newestRegionScopeRelays caps retained evidence by recency before rendering
// by name. A very old scope cannot crowd out an active scope alphabetically.
func newestRegionScopeRelays(latest map[string]time.Time) ([]regionScopeRelay, bool) {
	var out []regionScopeRelay
	for name, when := range latest {
		if regions.ValidateCandidate(name) == nil {
			out = append(out, regionScopeRelay{name: name, when: when})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].when.Equal(out[j].when) {
			return out[i].name < out[j].name
		}
		return out[i].when.After(out[j].when)
	})
	truncated := len(out) > maxTransportedScopes
	if truncated {
		// Copy the retained prefix so the published slice does not keep the
		// arbitrarily larger backing allocation alive between recomputes.
		out = append([]regionScopeRelay(nil), out[:maxTransportedScopes]...)
	}
	return out, truncated
}

func observedRegionWindow(value string) (string, time.Duration, bool) {
	switch value {
	case "", "24h":
		return "24h", 24 * time.Hour, true
	case "1h":
		return "1h", time.Hour, true
	case "7d":
		return "7d", 7 * 24 * time.Hour, true
	default:
		return "", 0, false
	}
}

type observedRegionNodeMetadata struct {
	publicKey, name, role, source string
	lat, lon                      sql.NullFloat64
	foreign                       bool
}

// forEachObservedRegionNode streams current metadata with one bulk read. It
// runs outside s.mu and checks Scan/Rows errors rather than reporting an empty
// successful result when node metadata is unavailable.
func (db *DB) forEachObservedRegionNode(ctx context.Context, visit func(observedRegionNodeMetadata) bool) error {
	return db.forEachRegionNodeMetadata(ctx, `SELECT public_key, name, role, lat, lon, foreign_advert, 'current'
		FROM nodes WHERE role IN ('repeater','room') ORDER BY public_key`, visit)
}

func (db *DB) forEachRegionNodeMetadata(ctx context.Context, query string, visit func(observedRegionNodeMetadata) bool) error {
	rows, err := db.conn.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var n observedRegionNodeMetadata
		var name, role sql.NullString
		var foreign sql.NullInt64
		if err := rows.Scan(&n.publicKey, &name, &role, &n.lat, &n.lon, &foreign, &n.source); err != nil {
			return err
		}
		n.publicKey, n.name, n.role, n.foreign = strings.ToLower(n.publicKey), name.String, role.String, foreign.Valid && foreign.Int64 != 0
		if !visit(n) {
			break
		}
	}
	return rows.Err()
}

// Both Regions endpoints apply live visibility through the same bulk metadata
// cursor. Any current row supersedes archived metadata, even a non-relay role.
func (s *Server) forEachVisibleRegionNode(ctx context.Context, keys []string, includeArchived bool, visit func(observedRegionNodeMetadata) bool) error {
	var hiddenNames map[string][]string
	if s.cfg != nil && len(s.cfg.EnforcedHiddenNamePrefixes()) > 0 {
		var err error
		hiddenNames, err = s.hiddenIdentityNames(ctx, keys)
		if err != nil {
			return err
		}
	}
	geo := s.getGeoFilter()
	applyGeo := s.cfg != nil && !s.cfg.GeoFilterExemptNodeList
	visible := func(n observedRegionNodeMetadata) bool {
		if identityHidden(s.cfg, n.publicKey, n.name) || identityHidden(s.cfg, n.publicKey, hiddenNames[n.publicKey]...) {
			return true
		}
		if geo != nil && applyGeo && !n.foreign && !NodePassesGeoFilter(nullFloat(n.lat), nullFloat(n.lon), geo) {
			return true
		}
		return visit(n)
	}
	if !includeArchived {
		return s.db.forEachObservedRegionNode(ctx, visible)
	}
	hasArchived, err := s.hasInactiveNodesTable(ctx)
	if err != nil {
		return err
	}
	if !hasArchived {
		return s.db.forEachObservedRegionNode(ctx, visible)
	}
	return s.db.forEachRegionNodeMetadata(ctx, `SELECT public_key, name, role, lat, lon, foreign_advert, 'current' AS source
		FROM nodes WHERE role IN ('repeater','room')
		UNION ALL
		SELECT i.public_key, i.name, i.role, i.lat, i.lon, i.foreign_advert, 'archived' AS source
		FROM inactive_nodes i WHERE i.role IN ('repeater','room')
		AND NOT EXISTS (SELECT 1 FROM nodes n WHERE n.public_key=i.public_key)
		ORDER BY public_key`, visible)
}

func reportedRegionPosition(lat, lon sql.NullFloat64) bool {
	return lat.Valid && lon.Valid && isValidNodePosition(lat.Float64, lon.Float64)
}

func (s *Server) handleRegionsObserved(w http.ResponseWriter, r *http.Request) {
	window, duration, valid := observedRegionWindow(r.URL.Query().Get("window"))
	if !valid {
		writeError(w, http.StatusBadRequest, "window must be 1h, 24h or 7d")
		return
	}
	if s.store == nil || s.db == nil || s.db.conn == nil {
		writeError(w, http.StatusServiceUnavailable, "observed relay data unavailable")
		return
	}
	s.store.repeaterEnrichMu.Lock()
	snapshot := s.store.repeaterRelaySnapshot
	s.store.repeaterEnrichMu.Unlock()
	if snapshot == nil || snapshot.regionMemberships == nil {
		writeError(w, http.StatusServiceUnavailable, "observed relay snapshot is warming up")
		return
	}
	start := snapshot.generatedAt.Add(-duration)
	stamp := snapshot.generatedAt.Format(time.RFC3339Nano)
	response := RegionsObservedResponse{Window: window, GeneratedAt: stamp, WindowStart: start.Format(time.RFC3339Nano), WindowEnd: stamp,
		Coverage: RegionsObservedCoverage{Loading: snapshot.loading, Evidence: snapshot.regionMemberships.evidence.window(start, snapshot.generatedAt)}, Nodes: []RegionsObservedNode{}}
	if !snapshot.coverageStart.IsZero() && !snapshot.coverageStart.After(snapshot.generatedAt) {
		floor := snapshot.coverageStart.Format(time.RFC3339Nano)
		response.Coverage.Start = &floor
		response.Coverage.Complete = !snapshot.coverageStart.After(start)
	}
	// A relay can also be known as an observer under another display name.
	// Resolve hiding aliases in bulk before opening the metadata cursor: both
	// queries must also work with SQLite's single-connection test databases.
	keys := make([]string, 0, len(snapshot.regionMemberships.nodes))
	for key, info := range snapshot.regionMemberships.nodes {
		if len(key) == 64 && len(info.scopes) > 0 {
			keys = append(keys, key)
		}
	}
	err := s.forEachVisibleRegionNode(r.Context(), keys, false, func(n observedRegionNodeMetadata) bool {
		info, exists := snapshot.regionMemberships.nodes[n.publicKey]
		if !exists {
			return true
		}
		scopes := make([]RegionsObservedScope, 0, len(info.scopes))
		for _, scope := range info.scopes {
			if !scope.when.Before(start) && !scope.when.After(snapshot.generatedAt) {
				scopes = append(scopes, RegionsObservedScope{Name: scope.name, LastRelayed: scope.when.Format(time.RFC3339Nano)})
			}
		}
		if len(scopes) == 0 {
			return true
		}
		if len(response.Nodes) >= maxObservedRegionNodes {
			response.Truncated = true
			return false
		}
		sort.Slice(scopes, func(i, j int) bool { return scopes[i].Name < scopes[j].Name })
		node := RegionsObservedNode{PublicKey: n.publicKey, Name: n.name, Role: n.role, PositionSource: "none", Scopes: scopes}
		if node.Name == "" {
			node.Name = n.publicKey
		}
		if reportedRegionPosition(n.lat, n.lon) {
			lat, lon := n.lat.Float64, n.lon.Float64
			node.Lat, node.Lon, node.PositionSource = &lat, &lon, "reported"
		}
		response.Nodes = append(response.Nodes, node)
		response.ScopesTruncated = response.ScopesTruncated || info.scopesTruncated
		return true
	})
	if err != nil {
		log.Printf("[regions-observed] node metadata: %v", err)
		writeError(w, http.StatusInternalServerError, "could not read region node metadata")
		return
	}
	// Visibility is evaluated on every serve; shared caches must not bypass a
	// freshly changed blacklist or hidden-name setting.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, response)
}

func regionsObservedSchemas() map[string]*openAPISchema {
	return map[string]*openAPISchema{
		"RegionsObservedCoverage": {Type: "object", Properties: map[string]*openAPISchema{
			"start":    {Type: "string", Nullable: true, Description: "Known retained-history floor (RFC3339), or null when unknown."},
			"complete": {Type: "boolean", Description: "Retained in-memory history covers the requested interval; not an observer uptime guarantee."},
			"loading":  {Type: "boolean", Description: "Startup loading state when this snapshot was captured."},
			"evidence": openAPIRef("RegionsEvidence"),
		}},
		"RegionsObservedScope": {Type: "object", Properties: map[string]*openAPISchema{
			"name":        {Type: "string", Description: "Exact observed scope spelling and case."},
			"lastRelayed": {Type: "string", Description: "Latest confirmed non-future scoped relay timestamp (RFC3339)."},
		}},
		"RegionsObservedNode": {Type: "object", Properties: map[string]*openAPISchema{
			"publicKey": {Type: "string"}, "name": {Type: "string"}, "role": {Type: "string", Enum: []string{"repeater", "room"}},
			"lat": {Type: "number", Nullable: true}, "lon": {Type: "number", Nullable: true},
			"positionSource": {Type: "string", Enum: []string{"reported", "none"}, Description: "Self-reported usable node coordinates or no usable position; no neighbor estimates."},
			"scopes":         {Type: "array", Items: openAPIRef("RegionsObservedScope"), Description: "At most 32 newest valid scope timestamps, filtered to the requested interval and sorted by name."},
		}},
		"RegionsObservedResponse": {Type: "object", Properties: map[string]*openAPISchema{
			"window":      {Type: "string", Enum: []string{"1h", "24h", "7d"}},
			"generatedAt": {Type: "string", Description: "Shared relay snapshot time (RFC3339)."},
			"windowStart": {Type: "string"}, "windowEnd": {Type: "string", Description: "Equals generatedAt."},
			"coverage":        openAPIRef("RegionsObservedCoverage"),
			"nodes":           {Type: "array", Items: openAPIRef("RegionsObservedNode"), Description: "At most 5000 eligible visible relays, including nodes without positions, sorted by public key."},
			"truncated":       {Type: "boolean", Description: "Additional eligible visible nodes were omitted."},
			"scopesTruncated": {Type: "boolean", Description: "A returned node's retained scope evidence exceeded the 32-scope limit."},
		}},
	}
}
