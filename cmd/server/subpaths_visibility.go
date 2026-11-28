package main

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
)

// Identity visibility for Route Patterns (#410): /api/analytics/subpaths,
// /subpaths-bulk and /subpath-detail apply the shared identityHidden policy
// (node blacklist, observer blacklist, hidden-name prefixes incl. observer
// aliases) with bulk lookups and without touching the store's cached,
// shared responses.
//
// One rule for every hop: a hop that stands for a hidden identity is shown
// as its raw hop, exactly like a hop that matches no node. Rows and parent
// paths through it are kept, not dropped, so a hidden hop with traffic has
// the same form as an unknown hop with the same traffic. Hidden observers are
// left out of a detail's observer list.
//
// Metrics keep their documented meaning: totalPaths, a row's count and pct,
// and a detail's totalMatches/signal/hourDistribution are computed over all
// traffic, hidden or not. Rows that become identical once masked are merged,
// so a list can hold fewer rows than its limit.
//
// Masking runs after the store has merged raw keys by display name, so a
// hidden node seen under several raw keys of one hop pair (bb, bb22) is one
// row in the store and one masked row (smallest raw key, counts summed),
// while an unknown hop with the same traffic (f0, f0f1) has no name to merge
// by and stays one row per raw key. Documented in docs/api-spec.md; pinned by
// TestRoutePatterns464_HiddenNodeSeveralRawKeysVsUnknownHop.

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
// reading live names in one bulk query. The query runs even for an empty
// key set (see lookupHiddenIdentityNames), so its cost does not depend on
// whether a requested hop resolves to a node.
func (s *Server) hiddenPubkeys(ctx context.Context, pubkeys []string) (map[string]bool, error) {
	names, err := s.lookupHiddenIdentityNames(ctx, pubkeys)
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

// visibleSubpathRow returns a row as it may be shown, with every hop that
// stands for a hidden identity shown as its raw hop. Each raw hop is a short
// prefix, never an identity by itself: it counts only through the nodes that
// carry it (candidates). A prefix with no hidden candidate is shown as
// named, unless the name itself is hidden (nameHidden): a cached row can
// still show the name of a node that has since left the node list or been
// renamed. Once a prefix has a hidden candidate it is shown as named only
// when that name is a visible candidate's and no hidden candidate's (fail
// closed: the name cannot tell them apart otherwise). A row whose hops and
// path segments do not line up is not shown at all (ok false).
func visibleSubpathRow(row SubpathResp, candidates map[string][]nodeInfo, hidden map[string]bool, nameHidden func(string) bool) (SubpathResp, bool) {
	names := strings.Split(row.Path, " → ")
	if len(names) != len(row.RawHops) {
		return SubpathResp{}, false
	}
	masked := false
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
		if nameHidden(names[i]) || (anyHidden && (!shownVisible || shownHidden)) {
			names[i] = hop
			masked = true
		}
	}
	if masked {
		row.Path = strings.Join(names, " → ")
	}
	return row, true
}

// mergeSubpathRows merges rows that show the same path once masked, the way
// the store merges raw keys that resolve to one name (counts summed, the
// smallest raw key reported), and ranks them as the store does.
func mergeSubpathRows(rows []SubpathResp, totalPaths int) []SubpathResp {
	byPath := make(map[string]int, len(rows))
	out := make([]SubpathResp, 0, len(rows))
	for _, row := range rows {
		i, ok := byPath[row.Path]
		if !ok {
			byPath[row.Path] = len(out)
			out = append(out, row)
			continue
		}
		if strings.Join(row.RawHops, ",") < strings.Join(out[i].RawHops, ",") {
			out[i].RawHops = row.RawHops
		}
		out[i].Count += row.Count
		out[i].Pct = subpathPct(out[i].Count, totalPaths)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// filterHiddenFromSubpaths masks hidden identities in list responses (one
// for /subpaths, one per group for /subpaths-bulk). A group that cannot be
// read is returned empty (fail closed).
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
		rows := make([]SubpathResp, 0, len(g.Subpaths))
		for _, row := range g.Subpaths {
			if v, ok := visibleSubpathRow(row, candidates, hidden, s.cfg.IsNameHidden); ok {
				rows = append(rows, v)
			}
		}
		out[i] = SubpathsResponse{Subpaths: mergeSubpathRows(rows, g.TotalPaths), TotalPaths: g.TotalPaths}
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
