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
//     makes no upstream call of its own (dhm.ReachLosses). The losses of
//     every evaluated node are kept with the cached result, hidden or not,
//     with the keys of those without terrain; names are not.
//   - What a viewer gets: the alias-aware visibility check (identityHidden
//     over hiddenIdentityNames, one bulk lookup) runs on every response,
//     cached or not, over every kept key. A hidden identity, or a node gone
//     from the snapshot, is in no list and no count: the response is the
//     one it would be without that node, so the centre and the radius,
//     which the client picks, cannot locate it (#743, #410). A failed
//     lookup serves no nodes in reach at all (fail closed).
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
	// coverageReachMaxNodes bounds the nodes in reach a response lists,
	// the lowest losses first. A result keeps every evaluated node (the
	// positioned repeaters and rooms of the snapshot inside the radius), so
	// the list and its counts are decided over visible nodes only.
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

// coverageReachMeta is the nodes in reach of a cached result: every
// evaluated node, hidden or not, so that visibility is decided per
// response for every count.
type coverageReachMeta struct {
	// Nodes are the nodes inside the radius with terrain on their path,
	// lowest loss first.
	Nodes []coverageReachEntry `json:"n"`
	// NoTerrain are the keys of the nodes inside the radius whose path has
	// a gap.
	NoTerrain []string `json:"t,omitempty"`
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
	// Hidden identities are in no list and no count.
	Nodes []CoverageReachNode `json:"nodes"`
	// Omitted are visible evaluated nodes past the list's bound, the
	// lossiest.
	Omitted int `json:"omitted"`
	// NoTerrain are visible nodes inside the radius without terrain on the
	// path.
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

// evaluateReach computes the nodes in reach of spec over grid, all of
// them: the lowest losses first (then the nearest, then the key).
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
			out.NoTerrain = append(out.NoTerrain, targets[i].pubkey)
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
// nodes in reach: the visible nodes in the current snapshot, named from
// it, with counts at the operator's budget. Hidden identities and nodes
// gone from the snapshot are in no list and no count. nil when the result
// has none (the snapshot was not ready when it was computed) or when the
// visibility lookup fails (fail closed).
func (s *Server) coverageReachView(ctx context.Context, c *terrainCoverage, m *coverageReachMeta) *CoverageReach {
	if m == nil {
		return nil
	}
	pks := make([]string, 0, len(m.Nodes)+len(m.NoTerrain))
	for _, e := range m.Nodes {
		pks = append(pks, e.PubKey)
	}
	pks = append(pks, m.NoTerrain...)
	// The lookup runs whatever the key set, so neither its work nor its
	// failure depends on whether a hidden node is in the radius.
	names, err := s.lookupHiddenIdentityNames(ctx, pks)
	if err != nil {
		c.logf("[coverage] predicted reach: node visibility lookup failed; nodes in reach not served")
		return nil
	}
	var byKey map[string]*nodeInfo
	if s.store != nil {
		nodes, _ := s.store.getCachedNodesAndPM()
		byKey = make(map[string]*nodeInfo, len(nodes))
		for i := range nodes {
			byKey[strings.ToLower(nodes[i].PublicKey)] = &nodes[i]
		}
	}
	// visible is the node of key pk when a viewer may see it.
	visible := func(pk string) *nodeInfo {
		n := byKey[pk]
		if n == nil || identityHidden(s.cfg, pk, append([]string{n.Name}, names[pk]...)...) {
			return nil
		}
		return n
	}
	out := &CoverageReach{Roles: coverageReachRoles, BudgetDb: c.radio.budgetDb(), Nodes: []CoverageReachNode{}}
	for _, pk := range m.NoTerrain {
		if visible(pk) != nil {
			out.NoTerrain++
		}
	}
	for _, e := range m.Nodes {
		n := visible(e.PubKey)
		if n == nil {
			continue
		}
		if len(out.Nodes) == coverageReachMaxNodes {
			out.Omitted++
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
