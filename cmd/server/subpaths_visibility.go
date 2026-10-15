package main

import (
	"context"
	"encoding/json"
	"strings"
)

// Identity visibility for Route Patterns (#410): /api/analytics/subpaths,
// /subpaths-bulk and /subpath-detail apply the shared identityHidden policy
// (node blacklist, observer blacklist, hidden-name prefixes incl. observer
// aliases) with bulk lookups and without touching the store's cached,
// shared responses.
//
// Metrics keep their documented meaning: totalPaths, a row's count and pct,
// and a detail's totalMatches/signal/hourDistribution are computed over all
// traffic, hidden or not. Only identities are removed, so after filtering a
// list can hold fewer rows than its limit and its percentages need not add up.

// subpathVisibility is the policy the detail view is built under. A nil
// *subpathVisibility hides nothing.
type subpathVisibility struct {
	cfg    *Config
	hidden map[string]bool // lower-case keys the live lookup found hidden
}

// hides reports whether an identity (a node or an observer) shown under name
// is hidden: by the live lookup of its key, or by the configured rules
// applied to its key and the name the detail would show (fail closed when
// that name is older than the live one).
func (v *subpathVisibility) hides(id, name string) bool {
	return v != nil && (v.hidden[strings.ToLower(id)] || identityHidden(v.cfg, id, name))
}

// routePatternsRestricted reports whether any identity-hiding rule is
// configured; with none, responses pass through untouched.
func (s *Server) routePatternsRestricted() bool {
	return s != nil && s.cfg != nil &&
		(s.cfg.HasNodeBlacklist() || len(s.cfg.ObserverBlacklist) > 0 || len(s.cfg.EnforcedHiddenNamePrefixes()) > 0)
}

// hiddenPubkeys returns the lower-case subset of pubkeys that are hidden,
// reading live names in one bulk query (see hiddenIdentityNames).
func (s *Server) hiddenPubkeys(ctx context.Context, pubkeys []string) (map[string]bool, error) {
	names, err := s.hiddenIdentityNames(ctx, pubkeys)
	if err != nil {
		return nil, err
	}
	hidden := make(map[string]bool)
	for _, pk := range pubkeys {
		lpk := strings.ToLower(pk)
		if identityHidden(s.cfg, lpk, names[lpk]...) {
			hidden[lpk] = true
		}
	}
	return hidden, nil
}

// subpathsResponseOf reads a store list result (any shape that carries the
// SubpathsResponse fields) into the typed response, which is also a copy: the
// store's cached result is never shared with what the filter returns.
func subpathsResponseOf(v interface{}) (SubpathsResponse, bool) {
	var out SubpathsResponse
	b, err := json.Marshal(v)
	if err != nil || json.Unmarshal(b, &out) != nil {
		return SubpathsResponse{}, false
	}
	return out, true
}

// subpathRowHidden decides one row. Each raw hop is a short prefix, never an
// identity by itself: it counts only through the nodes that carry it
// (candidates). A prefix with no hidden candidate never hides the row. Once a
// prefix has a hidden candidate the row is kept only when the name it shows
// for that hop is a visible candidate's name and no hidden candidate's (fail
// closed: a cached row can still show a name the node no longer has). Rows
// that do not line up (hops and path segments differ) are hidden.
func subpathRowHidden(row SubpathResp, candidates map[string][]nodeInfo, hidden map[string]bool) bool {
	names := strings.Split(row.Path, " → ")
	if len(names) != len(row.RawHops) {
		return true
	}
	for i, hop := range row.RawHops {
		anyHidden, shownVisible, shownHidden := false, false, false
		for _, c := range candidates[strings.ToLower(hop)] {
			h := hidden[strings.ToLower(c.PublicKey)]
			anyHidden = anyHidden || h
			if c.Name == names[i] {
				shownHidden = shownHidden || h
				shownVisible = shownVisible || !h
			}
		}
		if anyHidden && (!shownVisible || shownHidden) {
			return true
		}
	}
	return false
}

// filterHiddenFromSubpaths removes hidden-identity rows from list responses
// (one for /subpaths, one per group for /subpaths-bulk). A group that cannot
// be read is returned empty (fail closed).
func (s *Server) filterHiddenFromSubpaths(ctx context.Context, groups []SubpathsResponse) ([]SubpathsResponse, error) {
	prefixSet := map[string]bool{}
	for _, g := range groups {
		for _, row := range g.Subpaths {
			for _, h := range row.RawHops {
				prefixSet[strings.ToLower(h)] = true
			}
		}
	}
	prefixes := make([]string, 0, len(prefixSet))
	for p := range prefixSet {
		prefixes = append(prefixes, p)
	}
	candidates := s.store.HopCandidates(prefixes)
	var pubkeys []string
	seen := map[string]bool{}
	for _, cs := range candidates {
		for _, c := range cs {
			if pk := strings.ToLower(c.PublicKey); !seen[pk] {
				seen[pk] = true
				pubkeys = append(pubkeys, pk)
			}
		}
	}
	hidden, err := s.hiddenPubkeys(ctx, pubkeys)
	if err != nil {
		return nil, err
	}
	out := make([]SubpathsResponse, len(groups))
	for i, g := range groups {
		out[i] = SubpathsResponse{Subpaths: make([]SubpathResp, 0, len(g.Subpaths)), TotalPaths: g.TotalPaths}
		for _, row := range g.Subpaths {
			if !subpathRowHidden(row, candidates, hidden) {
				out[i].Subpaths = append(out[i].Subpaths, row)
			}
		}
	}
	return out, nil
}

// visibleSubpaths is what the list handlers call with the store's results:
// untouched when nothing is configured to be hidden, otherwise typed filtered
// copies.
func (s *Server) visibleSubpaths(ctx context.Context, results ...interface{}) ([]SubpathsResponse, error) {
	groups := make([]SubpathsResponse, len(results))
	for i, r := range results {
		g, ok := subpathsResponseOf(r)
		if !ok {
			g = SubpathsResponse{Subpaths: []SubpathResp{}} // unreadable: fail closed
		}
		groups[i] = g
	}
	return s.filterHiddenFromSubpaths(ctx, groups)
}

// subpathVisibility builds the detail-view policy for the identities a detail
// can show (requested hops, parent-path hops and observer ids, see
// subpathDetailParts.pubkeys) in one bulk lookup, so its cost follows the
// detail and not the number of known nodes.
func (s *Server) subpathVisibility(ctx context.Context, pubkeys []string) (*subpathVisibility, error) {
	if !s.routePatternsRestricted() {
		return nil, nil
	}
	hidden, err := s.hiddenPubkeys(ctx, pubkeys)
	if err != nil {
		return nil, err
	}
	return &subpathVisibility{cfg: s.cfg, hidden: hidden}, nil
}
