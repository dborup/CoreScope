package main

// #941 M2 — nodes in reach: which repeaters and rooms the predicted reach
// reaches, each evaluated point to point from the centre with its own
// antenna height, over the elevation grid the raster was computed on.
//
//   - Who: every repeater and room (owner decision 5: infrastructure
//     only) with a valid position from its own adverts (the node
//     snapshot; estimated positions never enter it) within the radius,
//     the centre node itself excepted. The antenna height follows the
//     profile's rules without measuring anything: the operator's
//     per-node value, the owner's approved height (#789), the role.
//   - When: during the computation, after the tiles are gathered, so it
//     makes no upstream call of its own (dhm.ReachLosses). The losses are
//     kept with the cached result; names are not.
//   - What a viewer gets: the alias-aware visibility check (identityHidden
//     over hiddenIdentityNames, one bulk lookup) runs on every response,
//     cached or not. A hidden identity is never named, only counted as
//     withheld; a node gone from the snapshot is withheld too, and a
//     failed lookup withholds every node (fail closed).
//   - The counts per class are at the operator's link budget (config
//     radio); the browser recounts and recolours the list with the
//     viewer's own budget from the same losses, without a request.

import (
	"context"
	"math"
	"sort"
	"strings"

	"github.com/meshcore-analyzer/dhm"
)

const (
	// coverageReachMaxNodes bounds the nodes in reach a result keeps and
	// serves, the lowest losses first.
	coverageReachMaxNodes = 1000
	// coverageReachEntryBytes bounds one kept node in a cached result.
	coverageReachEntryBytes = 192
)

// coverageReachRoles are the roles evaluated (#941 owner decision 5).
var coverageReachRoles = []string{"repeater", "room"}

// coverageClasses are the margin classes of M0 §3.5 (owner decision 7),
// best first, with their lower bounds in dB; "none" below the last. The
// panel's CLASSES in public/coverage.js are the same
// (TestCoverage941M2_ClassesMatchTheClient).
var coverageClasses = []struct {
	id    string
	minDb float64
}{{"good", 10}, {"marginal", 3}, {"weak", 0}}

// coverageReachTarget is one node to evaluate, resolved at compute time.
type coverageReachTarget struct {
	pubkey, role string
	pt           dhm.LatLon
	heightM      float64
	heightSource string
}

// coverageReachEntry is one evaluated node in a cached result.
type coverageReachEntry struct {
	PubKey       string  `json:"k"`
	Role         string  `json:"r"`
	DistanceM    float64 `json:"d"`
	HeightM      float64 `json:"h"`
	HeightSource string  `json:"s"`
	LossDb       float64 `json:"l"`
}

// coverageReachMeta is the nodes in reach of a cached result.
type coverageReachMeta struct {
	Nodes []coverageReachEntry `json:"n"`
	// Omitted are evaluated nodes beyond coverageReachMaxNodes.
	Omitted int `json:"o"`
	// NoTerrain are nodes inside the radius whose path has a gap.
	NoTerrain int `json:"t"`
}

// CoverageReach is the response's nodes in reach.
type CoverageReach struct {
	Roles []string `json:"roles"` // the roles evaluated
	// BudgetDb is the operator's link budget Counts are at: power plus
	// both gains minus cable loss and sensitivity (config radio).
	BudgetDb float64             `json:"budgetDb"`
	Counts   CoverageReachCounts `json:"counts"`
	// Nodes are the visible evaluated nodes, lowest loss first, whatever
	// their class: a larger budget may bring a "none" node into reach.
	Nodes []CoverageReachNode `json:"nodes"`
	// Withheld are evaluated nodes that are hidden (or whose visibility
	// could not be checked): counted, never named.
	Withheld int `json:"withheld"`
	// Omitted are evaluated nodes past the list's bound, the lossiest.
	Omitted int `json:"omitted"`
	// NoTerrain are nodes inside the radius without terrain on the path.
	NoTerrain int `json:"noTerrain"`
}

// CoverageReachCounts are the listed nodes per margin class.
type CoverageReachCounts struct {
	Good     int `json:"good"`
	Marginal int `json:"marginal"`
	Weak     int `json:"weak"`
	None     int `json:"none"`
}

// CoverageReachNode is one node in reach.
type CoverageReachNode struct {
	PublicKey    string  `json:"pubkey"`
	Name         string  `json:"name"`
	Role         string  `json:"role"`
	DistanceKm   float64 `json:"distanceKm"`
	HeightM      float64 `json:"heightM"`
	HeightSource string  `json:"heightSource"`
	// LossDb is the predicted path loss; the margin is the budget minus it.
	LossDb float64 `json:"lossDb"`
}

// coverageReachRole reports whether a node of role r is evaluated.
func coverageReachRole(r string) bool {
	for _, x := range coverageReachRoles {
		if r == x {
			return true
		}
	}
	return false
}

// coverageReachTargets lists the nodes a reach from centre (excluding the
// centre node, exclude) evaluates: repeaters and rooms with a valid
// position near enough to be inside radiusM (dhm.ReachLosses decides the
// edge, in the raster's own projection), sorted by public key, each with
// its antenna height. ok is false when the node snapshot is not complete.
func (s *Server) coverageReachTargets(ctx context.Context, c *terrainCoverage, centre dhm.LatLon, exclude string, radiusM float64) ([]coverageReachTarget, bool) {
	if s.store == nil {
		return nil, false
	}
	nodes, _, ok := s.store.getCachedNodesAndPMOK()
	if !ok {
		return nil, false
	}
	var out []coverageReachTarget
	for i := range nodes {
		n := &nodes[i]
		pk := strings.ToLower(n.PublicKey)
		if !coverageReachRole(n.Role) || !n.HasGPS || !isValidNodePosition(n.Lat, n.Lon) || pk == exclude || !isHexPubkey(pk) {
			continue
		}
		pt := dhm.LatLon{Lat: n.Lat, Lon: n.Lon}
		// A kilometre of slack: the projection's scale, not the policy.
		if dhm.GroundDistance(centre, pt) > radiusM+1000 {
			continue
		}
		h, source := s.coverageNodeHeight(ctx, c, pk, n.Role, pt)
		out = append(out, coverageReachTarget{pubkey: pk, role: n.Role, pt: pt, heightM: h, heightSource: source})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].pubkey < out[b].pubkey })
	return out, true
}

// coverageNodeHeight is a node's antenna height above the ground in a
// reach, for nodes in reach and the validation harness alike: the
// profile's rules without measuring anything (the operator's per-node
// value, the owner's approved height unless the node moved, the role).
func (s *Server) coverageNodeHeight(ctx context.Context, c *terrainCoverage, pk, role string, pt dhm.LatLon) (float64, string) {
	end := terrainEnd{pt: pt, kind: "node", role: role, pubkey: pk}
	end.owner = s.antennaClaims().ownerHeightAt(ctx, pk, pt)
	h, source, _ := c.link.antenna(end, nil)
	return h, source
}

// evaluateReach computes the nodes in reach of spec over grid: the
// lowest losses first (then the nearest, then the key), at most
// coverageReachMaxNodes.
func evaluateReach(ctx context.Context, grid *dhm.CoverageGrid, spec dhm.CoverageSpec, targets []coverageReachTarget) (*coverageReachMeta, error) {
	pts := make([]dhm.ReachTarget, len(targets))
	for i, t := range targets {
		pts[i] = dhm.ReachTarget{Lat: t.pt.Lat, Lon: t.pt.Lon, HeightM: t.heightM}
	}
	losses, err := dhm.ReachLosses(ctx, grid, spec, pts)
	if err != nil {
		return nil, err
	}
	out := &coverageReachMeta{Nodes: []coverageReachEntry{}}
	for i, l := range losses {
		switch {
		case !l.Inside:
			continue
		case math.IsNaN(l.LossDb):
			out.NoTerrain++
			continue
		}
		t := targets[i]
		out.Nodes = append(out.Nodes, coverageReachEntry{PubKey: t.pubkey, Role: t.role,
			DistanceM: math.Round(l.DistanceM/10) * 10, HeightM: round2(t.heightM), HeightSource: t.heightSource,
			LossDb: math.Round(l.LossDb*10) / 10})
	}
	sort.SliceStable(out.Nodes, func(a, b int) bool {
		x, y := out.Nodes[a], out.Nodes[b]
		if x.LossDb != y.LossDb {
			return x.LossDb < y.LossDb
		}
		if x.DistanceM != y.DistanceM {
			return x.DistanceM < y.DistanceM
		}
		return x.PubKey < y.PubKey
	})
	if len(out.Nodes) > coverageReachMaxNodes {
		out.Omitted = len(out.Nodes) - coverageReachMaxNodes
		out.Nodes = out.Nodes[:coverageReachMaxNodes]
	}
	return out, nil
}

// coverageMarginClass is the class of a margin in dB; a boundary belongs
// to the better class.
func coverageMarginClass(m float64) string {
	for _, c := range coverageClasses {
		if m >= c.minDb {
			return c.id
		}
	}
	return "none"
}

// budgetDb is the operator's link budget: the largest loss a link
// tolerates.
func (r CoverageRadio) budgetDb() float64 {
	return r.TxPowerDbm + r.AntennaGainDbi + r.RxGainDbi - r.LossDb - r.SensitivityDbm
}

// coverageReachView is what one viewer may see of a cached result's
// nodes in reach: hidden identities withheld, names from the current
// snapshot, counts at the operator's budget. nil when the result has
// none (the snapshot was not ready when it was computed).
func (s *Server) coverageReachView(ctx context.Context, c *terrainCoverage, m *coverageReachMeta) *CoverageReach {
	if m == nil {
		return nil
	}
	out := &CoverageReach{Roles: coverageReachRoles, BudgetDb: c.radio.budgetDb(), Nodes: []CoverageReachNode{},
		Omitted: m.Omitted, NoTerrain: m.NoTerrain}
	if len(m.Nodes) == 0 {
		return out
	}
	pks := make([]string, len(m.Nodes))
	for i, e := range m.Nodes {
		pks[i] = e.PubKey
	}
	names, err := s.hiddenIdentityNames(ctx, pks)
	var byKey map[string]*nodeInfo
	if err == nil && s.store != nil {
		nodes, _ := s.store.getCachedNodesAndPM()
		byKey = make(map[string]*nodeInfo, len(nodes))
		for i := range nodes {
			byKey[strings.ToLower(nodes[i].PublicKey)] = &nodes[i]
		}
	}
	if err != nil {
		c.logf("[coverage] predicted reach: node visibility lookup failed; %d node(s) withheld", len(m.Nodes))
	}
	for _, e := range m.Nodes {
		n := byKey[e.PubKey] // nil after a failed lookup: withheld
		if n == nil || identityHidden(s.cfg, e.PubKey, append([]string{n.Name}, names[e.PubKey]...)...) {
			out.Withheld++
			continue
		}
		out.Nodes = append(out.Nodes, CoverageReachNode{PublicKey: e.PubKey, Name: n.Name, Role: e.Role,
			DistanceKm: math.Round(e.DistanceM/10) / 100, HeightM: e.HeightM, HeightSource: e.HeightSource, LossDb: e.LossDb})
		switch coverageMarginClass(out.BudgetDb - e.LossDb) {
		case "good":
			out.Counts.Good++
		case "marginal":
			out.Counts.Marginal++
		case "weak":
			out.Counts.Weak++
		default:
			out.Counts.None++
		}
	}
	return out
}
