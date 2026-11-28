package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/channelregistry/antennaclaim"
	"github.com/meshcore-analyzer/dbschema"
	"github.com/meshcore-analyzer/dhm"
)

// #941 M2 — nodes in reach (terrain_coverage_reach.go): repeaters and
// rooms within the radius, evaluated point to point over the raster's own
// elevation grid, hidden identities left out at serve time. Every test
// runs on a fake ElevationSource.

const reachLat, reachLon = 56.1629, 10.2039

// reachPos is the position dE, dN metres from the reach centre in
// EPSG:25832, the projection the radials and paths use.
func reachPos(dE, dN float64) (float64, float64) {
	e, n := dhm.ToUTM32(reachLat, reachLon)
	return dhm.FromUTM32(e+dE, n+dN)
}

func addReachNode(t *testing.T, env *coverageEnv, pk, name, role string, dE, dN float64) {
	t.Helper()
	lat, lon := reachPos(dE, dN)
	if _, err := env.srv.db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count)
		VALUES (?, ?, ?, ?, ?, datetime('now'), datetime('now'), 1)`, pk, name, role, lat, lon); err != nil {
		t.Fatal(err)
	}
}

func reachPK(c byte) string { return strings.Repeat(string(c), 64) }

// ridgeEastElev is flat ground at 3 m with a 120 m ridge 4 km east of the
// reach centre.
func ridgeEastElev(e, n float64) float32 {
	e0, _ := dhm.ToUTM32(reachLat, reachLon)
	if x := e - e0; x > 3950 && x < 4050 {
		return 120
	}
	return 3
}

// TestCoverage941M2_NodesInReachOrderAndCounts on a synthetic DEM: only
// positioned repeaters and rooms inside the radius are evaluated (not the
// centre node, a companion, an unpositioned node or one beyond the radius),
// each with its own antenna height (operator per node, role default), in
// loss order with the shadowed node behind the ridge last; the counts are
// the list's classes at the operator's budget, and the losses are
// dhm.ReachLosses' own.
func TestCoverage941M2_NodesInReachOrderAndCounts(t *testing.T) {
	radio := &RadioConfig{AntennaHeightsM: map[string]float64{reachPK('3'): 25}}
	// A 60 m grid: the same paths at a quarter of the cells, so the test
	// stays quick under -race.
	const res = 60
	env := newCoverageEnv(t, newFakeElevation(t, ridgeEastElev), CoverageConfig{ResolutionM: intp(res)}, radio, nil)
	centre := reachPK('c')
	// 5 m off the point the centre snaps to (4 decimals): inside the
	// radius by distance, so only its key keeps it out of its own list.
	addReachNode(t, env, centre, "Centre mast", "repeater", 3, 4)
	addReachNode(t, env, reachPK('1'), "North near", "repeater", 0, 2000)
	addReachNode(t, env, reachPK('2'), "Behind the ridge", "repeater", 8000, 0)
	addReachNode(t, env, reachPK('3'), "West far, tall", "repeater", -11000, 0)
	addReachNode(t, env, reachPK('4'), "South room", "room", 0, -6000)
	addReachNode(t, env, reachPK('5'), "A companion", "companion", 0, 3000)
	addReachNode(t, env, reachPK('6'), "Beyond", "repeater", 0, 12500)
	if _, err := env.srv.db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count)
		VALUES (?, 'No fix', 'repeater', 0, 0, datetime('now'), datetime('now'), 1)`, reachPK('7')); err != nil {
		t.Fatal(err)
	}
	env.srv.store.InvalidateNodeCache()
	// The reference: the same terrain on the same grid, built once.
	e0, n0 := dhm.ToUTM32(reachLat, reachLon)
	grid := dhm.NewCoverageGrid(e0, n0, 12000, res)
	for _, tl := range dhm.PlanCoverageTiles(e0, n0, 12000, res) {
		vals := make([]float32, dhm.CoverageTileCells)
		for row := 0; row < dhm.CoverageTilePx; row++ {
			for col := 0; col < dhm.CoverageTilePx; col++ {
				vals[row*dhm.CoverageTilePx+col] = ridgeEastElev(tl.Cell(res, col, row))
			}
		}
		grid.Paste(tl, vals)
	}

	for _, model := range []string{"bullington", "delta"} {
		w := env.get(reachQuery(centre, "r=12", "model="+model))
		resp := decodeCoverage(t, w)
		r := resp.Reach
		if r == nil {
			t.Fatalf("%s: no reach: %s", model, w.Body.String())
		}
		var keys []string
		for _, n := range r.Nodes {
			keys = append(keys, n.PublicKey[:1])
		}
		// Loss order: the near node first and the shadowed node 8 km
		// behind the 120 m ridge last. Bullington ranks the 5 m room at
		// 6 km above the 25 m mast at 11 km; the complete method, which
		// charges a ground-hugging path its smooth-earth loss, the other
		// way round.
		order := map[string]string{"bullington": "1432", "delta": "1342"}[model]
		if got := strings.Join(keys, ""); got != order {
			t.Fatalf("%s: order %q: %+v", model, got, r.Nodes)
		}
		if r.Omitted != 0 || r.NoTerrain != 0 || strings.Join(r.Roles, ",") != "repeater,room" {
			t.Fatalf("%s: %+v", model, r)
		}
		want := map[string]struct {
			name   string
			role   string
			km, h  float64
			source string
		}{
			"1": {"North near", "repeater", 2, 10, "role-default"},
			"4": {"South room", "room", 6, 5, "role-default"},
			"3": {"West far, tall", "repeater", 11, 25, "operator-node"},
			"2": {"Behind the ridge", "repeater", 8, 10, "role-default"},
		}
		budget := 22 + 3 + 3 - 0 + 130.0
		var counts CoverageReachCounts
		spec := dhm.CoverageSpec{Center: dhm.LatLon{Lat: reachLat, Lon: reachLon}, RadiusM: 12000, ResolutionM: res,
			TxHeightM: 10, FreqMHz: dhm.FirmwareFreqMHz, K: dhm.DefaultKFactor, Model: dhm.PathModel(model)}
		for _, n := range r.Nodes {
			w := want[n.PublicKey[:1]]
			if n.Name != w.name || n.Role != w.role || math.Abs(n.DistanceKm-w.km) > 0.01 || n.HeightM != w.h || n.HeightSource != w.source {
				t.Fatalf("%s: node %+v, want %+v", model, n, w)
			}
			ref, err := dhm.ReachLosses(context.Background(), grid, spec, []dhm.ReachTarget{reachTarget(t, env, n.PublicKey, n.HeightM)})
			if err != nil || math.Abs(ref[0].LossDb-n.LossDb) > 0.05 {
				t.Fatalf("%s: %s loss %.2f, ReachLosses %.2f (%v)", model, n.Name, n.LossDb, ref[0].LossDb, err)
			}
			switch m := budget - n.LossDb; {
			case m >= 10:
				counts.Good++
			case m >= 3:
				counts.Marginal++
			case m >= 0:
				counts.Weak++
			default:
				counts.None++
			}
		}
		if r.BudgetDb != budget || r.Counts != counts {
			t.Fatalf("%s: counts %+v at %v dB, recount %+v", model, r.Counts, r.BudgetDb, counts)
		}
		if r.Nodes[3].LossDb < r.Nodes[2].LossDb+10 {
			t.Fatalf("%s: the ridge does not shadow: %+v", model, r.Nodes)
		}
	}
	// The centre node is never in its own list, and its key and name stay
	// out of the response as in M1.
	w := env.get(reachQuery(centre, "r=12"))
	if strings.Contains(w.Body.String(), centre) || strings.Contains(w.Body.String(), "Centre mast") {
		t.Fatal("the centre node is in its own response")
	}
}

// TestCoverage941M2_RadiusEdgeAndRoles: the list holds exactly the nodes
// inside the radius, 20 m either side of its edge, and of the roles only
// repeaters and rooms (owner decision 5): a sensor or a companion well
// inside is not evaluated.
func TestCoverage941M2_RadiusEdgeAndRoles(t *testing.T) {
	env := newCoverageEnv(t, newFakeElevation(t, flatElev(3)), CoverageConfig{}, nil, nil)
	addReachNode(t, env, reachPK('1'), "Just inside", "repeater", 0, 4980)
	addReachNode(t, env, reachPK('2'), "Just outside", "repeater", 0, 5020)
	addReachNode(t, env, reachPK('3'), "Room inside", "room", -3000, 0)
	addReachNode(t, env, reachPK('4'), "Sensor inside", "sensor", 2000, 0)
	addReachNode(t, env, reachPK('5'), "Companion inside", "companion", 0, -2500)
	env.srv.store.InvalidateNodeCache()
	w := env.get(reachQuery("56.1629,10.2039", "r=5"))
	r := decodeCoverage(t, w).Reach
	if r == nil {
		t.Fatalf("no reach: %s", w.Body.String())
	}
	var got []string
	for _, n := range r.Nodes {
		got = append(got, n.Name)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "Just inside,Room inside" || r.NoTerrain != 0 || r.Omitted != 0 {
		t.Fatalf("listed %v (%+v)", got, r)
	}
	for _, n := range r.Nodes {
		if n.Name == "Just inside" && (n.DistanceKm != 4.98 || math.IsNaN(n.LossDb)) {
			t.Fatalf("edge node %+v", n)
		}
	}
	if strings.Contains(w.Body.String(), "Sensor") || strings.Contains(w.Body.String(), "Companion") {
		t.Fatalf("another role is in the response: %s", w.Body.String())
	}
}

func reachTarget(t *testing.T, env *coverageEnv, pk string, h float64) dhm.ReachTarget {
	t.Helper()
	var lat, lon float64
	if err := env.srv.db.conn.QueryRow(`SELECT lat, lon FROM nodes WHERE public_key = ?`, pk).Scan(&lat, &lon); err != nil {
		t.Fatal(err)
	}
	return dhm.ReachTarget{Lat: lat, Lon: lon, HeightM: h}
}

// TestCoverage941M2_OwnerHeightsReachTheList: an approved owner height
// (#789) is a node's height in the list, ahead of its role default.
func TestCoverage941M2_OwnerHeightsReachTheList(t *testing.T) {
	env := newCoverageEnv(t, newFakeElevation(t, flatElev(3)), CoverageConfig{}, nil, nil)
	if err := dbschema.EnsureNodeAntennaClaims(env.srv.db.conn); err != nil {
		t.Fatal(err)
	}
	svc := newAntennaClaimService(env.srv.cfg, env.srv.db.conn, filepath.Join(t.TempDir(), antennaclaim.QueueDirName))
	env.srv.claims = svc
	pk := reachPK('a')
	addReachNode(t, env, pk, "Owner mast", "repeater", 0, 3000)
	addReachNode(t, env, reachPK('b'), "Plain", "repeater", 0, -3000)
	env.srv.store.InvalidateNodeCache()
	lat, lon := reachPos(0, 3000)
	now := time.Now().UnixMilli()
	if _, err := env.srv.db.conn.Exec(`INSERT INTO node_antenna_claims
		(request_id, pubkey, height_m, height_op, height_in_force, code, status, created_at, expires_at, proven_at, proof_hash, proof_lat, proof_lon, reviewed_at, approved_by)
		VALUES ('a000000000000001', ?, 31.5, 'set', 1, '7KX9QM', ?, ?, ?, ?, 'abcd', ?, ?, ?, 'admin')`,
		pk, antennaclaim.StatusApproved, now, now+86400000, now, lat, lon, now); err != nil {
		t.Fatal(err)
	}
	svc.invalidateOwners()
	r := decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "r=5"))).Reach
	if r == nil || len(r.Nodes) != 2 {
		t.Fatalf("reach %+v", r)
	}
	for _, n := range r.Nodes {
		switch n.PublicKey {
		case pk:
			if n.HeightM != 31.5 || n.HeightSource != "owner" {
				t.Fatalf("owner node %+v", n)
			}
		default:
			if n.HeightM != 10 || n.HeightSource != "role-default" {
				t.Fatalf("plain node %+v", n)
			}
		}
	}
	if r.Nodes[0].PublicKey != pk {
		t.Fatalf("the 31.5 m mast is not the best: %+v", r.Nodes)
	}
}

// TestCoverage941M2_HiddenNodesAreNeverNamed: the raw response bytes never
// carry a hidden identity's key (in any case) or name, whether it is
// hidden by the node blacklist, the observer blacklist, a hidden-name
// prefix on the node or only through an observer alias. Visibility is
// decided at serve time, so a rename into a hidden prefix hides a node of
// a cached result on the next request, with no new computation and no
// upstream call.
func TestCoverage941M2_HiddenNodesAreNeverNamed(t *testing.T) {
	src := newFakeElevation(t, flatElev(3))
	env := newCoverageEnv(t, src, CoverageConfig{}, nil, nil)
	env.srv.cfg.SetHiddenNamePrefixes([]string{"private-"})
	// Keys with letters, so a digit run in a coordinate cannot match one.
	visible := reachPK('1')
	hidden := map[string]string{
		strings.Repeat("ab", 32): "Blacklisted Mast",
		strings.Repeat("cd", 32): "Obs Blacklisted",
		strings.Repeat("ef", 32): "private-roof",
		strings.Repeat("fa", 32): "Alias Hidden",
	}
	addReachNode(t, env, visible, "Visible mast", "repeater", 0, 2000)
	i := 0
	for pk, name := range hidden {
		i++
		addReachNode(t, env, pk, name, "repeater", float64(i)*900, -1500)
	}
	env.srv.cfg.SetNodeBlacklist([]string{strings.ToUpper(strings.Repeat("ab", 32))})
	env.srv.cfg.ObserverBlacklist = []string{strings.Repeat("cd", 32)}
	if _, err := env.srv.db.conn.Exec(`INSERT INTO observers (id, name) VALUES (?, 'private-observer')`, strings.ToUpper(strings.Repeat("fa", 32))); err != nil {
		t.Fatal(err)
	}
	env.srv.store.InvalidateNodeCache()

	check := func(label string, wantVisible bool) []byte {
		t.Helper()
		w := env.get(reachQuery("56.1629,10.2039", "r=5"))
		body := w.Body.Bytes()
		r := decodeCoverage(t, w).Reach
		if r == nil || (len(r.Nodes) == 1) != wantVisible || r.Omitted != 0 || r.NoTerrain != 0 {
			t.Fatalf("%s: reach %+v", label, r)
		}
		low := strings.ToLower(string(body))
		for pk, name := range hidden {
			if strings.Contains(low, pk) || strings.Contains(low, strings.ToLower(name)) || strings.Contains(low, pk[:16]) {
				t.Fatalf("%s: hidden %s (%q) in the response: %s", label, pk[:4], name, body)
			}
		}
		if r.Counts.Good+r.Counts.Marginal+r.Counts.Weak+r.Counts.None != len(r.Nodes) {
			t.Fatalf("%s: counts %+v for %d listed", label, r.Counts, len(r.Nodes))
		}
		return body
	}
	check("miss", true)
	calls := src.calls.Load()
	check("hit", true)
	// Rename the visible node into the hidden prefix: hidden on the next
	// request, from the same cached result.
	if _, err := env.srv.db.conn.Exec(`UPDATE nodes SET name = 'private-now' WHERE public_key = ?`, visible); err != nil {
		t.Fatal(err)
	}
	env.srv.store.InvalidateNodeCache()
	hidden[visible] = "private-now"
	body := check("renamed", false)
	if src.calls.Load() != calls {
		t.Fatal("the visibility change recomputed")
	}
	if strings.Contains(string(body), "Visible mast") {
		t.Fatal("the old name is served")
	}
}

// gapNorthElev is flat ground at 3 m with a strip without terrain 3 km
// north of the reach centre.
func gapNorthElev(e, n float64) float32 {
	_, n0 := dhm.ToUTM32(reachLat, reachLon)
	if d := n - n0; d > 3000 && d < 3100 {
		return -9999
	}
	return 3
}

type reachTestNode struct {
	pk, name, role string
	dE, dN         float64
}

// reachHiddenEnv is a reach over gapNorthElev with the hidden-name prefix
// "private-" and a node blacklist, holding nodes; an alias is an observer
// row naming a node's key.
func reachHiddenEnv(t *testing.T, nodes []reachTestNode, aliases map[string]string) *coverageEnv {
	t.Helper()
	env := newCoverageEnv(t, newFakeElevation(t, gapNorthElev), CoverageConfig{}, nil, nil)
	env.srv.cfg.SetHiddenNamePrefixes([]string{"private-"})
	env.srv.cfg.SetNodeBlacklist([]string{strings.Repeat("cd", 32)})
	for _, n := range nodes {
		addReachNode(t, env, n.pk, n.name, n.role, n.dE, n.dN)
	}
	for pk, name := range aliases {
		if _, err := env.srv.db.conn.Exec(`INSERT INTO observers (id, name) VALUES (?, ?)`, strings.ToUpper(pk), name); err != nil {
			t.Fatal(err)
		}
	}
	env.srv.store.InvalidateNodeCache()
	return env
}

// TestCoverage941M2_HiddenNodesLeaveNoTrace: a hidden node inside the
// radius changes nothing in the response, byte for byte, on a miss and on
// a hit: it is not named, and no count (the classes, omitted, noTerrain)
// includes it. The client picks the centre and the radius, so a count of
// hidden nodes would bracket one's distance from any point (#743, #410).
// A node hidden after its result was cached is gone the same way.
func TestCoverage941M2_HiddenNodesLeaveNoTrace(t *testing.T) {
	visible := []reachTestNode{
		{reachPK('1'), "Visible mast", "repeater", 0, -2000},
		{reachPK('2'), "Visible room", "room", 2500, 0},
		{reachPK('3'), "Visible past the gap", "repeater", 0, 4000}, // no terrain on its path
	}
	hidden := []reachTestNode{
		{strings.Repeat("ab", 32), "private-near", "repeater", 0, -1000}, // the lowest loss
		{strings.Repeat("cd", 32), "Blacklisted room", "room", -3000, 0},
		{strings.Repeat("ef", 32), "private-gap", "repeater", 500, 4000}, // no terrain on its path
		{strings.Repeat("fa", 32), "Alias hidden", "repeater", -1000, -1000},
	}
	aliases := map[string]string{strings.Repeat("fa", 32): "private-observer"}
	q := reachQuery("56.1629,10.2039", "r=5")
	with := reachHiddenEnv(t, append(append([]reachTestNode{}, visible...), hidden...), aliases)
	without := reachHiddenEnv(t, visible, nil)
	for _, state := range []string{"miss", "hit"} {
		a, b := with.get(q), without.get(q)
		if a.Header().Get("X-CoreScope-Cache") != state || b.Header().Get("X-CoreScope-Cache") != state {
			t.Fatalf("%s: %s / %s", state, a.Header().Get("X-CoreScope-Cache"), b.Header().Get("X-CoreScope-Cache"))
		}
		if a.Body.String() != b.Body.String() {
			t.Fatalf("%s: a hidden node changes the response:\nwith    %s\nwithout %s", state, a.Body.String(), b.Body.String())
		}
	}
	r := decodeCoverage(t, without.get(q)).Reach
	if r == nil || len(r.Nodes) != 2 || r.NoTerrain != 1 || r.Omitted != 0 {
		t.Fatalf("reference reach %+v", r)
	}
	// Renamed into the hidden prefix after the result was cached: served
	// from the same cached result as if it had never been there.
	if _, err := with.srv.db.conn.Exec(`UPDATE nodes SET name = 'private-now' WHERE public_key = ?`, reachPK('2')); err != nil {
		t.Fatal(err)
	}
	with.srv.store.InvalidateNodeCache()
	a := with.get(q)
	b := reachHiddenEnv(t, []reachTestNode{visible[0], visible[2]}, nil).get(q)
	if a.Header().Get("X-CoreScope-Cache") != "hit" || a.Body.String() != b.Body.String() {
		t.Fatalf("hidden after the hit (%s):\nwith    %s\nwithout %s", a.Header().Get("X-CoreScope-Cache"), a.Body.String(), b.Body.String())
	}
}

// TestCoverage941M2_LookupFailureFailsClosed: when the visibility lookup
// fails, no node is named and nothing is counted: reach is null, the same
// bytes whether a hidden node is in the radius or not. The raster is still
// served, and the failure is logged without detail.
func TestCoverage941M2_LookupFailureFailsClosed(t *testing.T) {
	visible := []reachTestNode{
		{reachPK('1'), "Visible one", "repeater", 0, 2000},
		{reachPK('2'), "Visible two", "room", 0, -2000},
	}
	q := reachQuery("56.1629,10.2039", "r=5")
	var bodies []string
	for _, nodes := range [][]reachTestNode{visible, append(append([]reachTestNode{}, visible...),
		reachTestNode{strings.Repeat("ef", 32), "private-roof", "repeater", 1500, 0})} {
		env := reachHiddenEnv(t, nodes, nil)
		if _, err := env.srv.db.conn.Exec(`DROP TABLE observers`); err != nil {
			t.Fatal(err)
		}
		for _, state := range []string{"miss", "hit"} {
			w := env.get(q)
			resp := decodeCoverage(t, w)
			if w.Header().Get("X-CoreScope-Cache") != state || resp.Reach != nil || !strings.Contains(w.Body.String(), `"reach":null`) {
				t.Fatalf("%s: reach %+v", state, resp.Reach)
			}
			if strings.Contains(w.Body.String(), "Visible") || strings.Contains(w.Body.String(), reachPK('1')) {
				t.Fatalf("a node was named after a failed lookup: %s", w.Body.String())
			}
			if env.raster(resp.Raster.URL, true).Code != 200 {
				t.Fatal("the raster is not served")
			}
			bodies = append(bodies, w.Body.String())
		}
		if !strings.Contains(env.logs.String(), "visibility lookup failed") || strings.Contains(env.logs.String(), "observers") {
			t.Fatalf("log %q", env.logs.String())
		}
	}
	if bodies[0] != bodies[2] || bodies[1] != bodies[3] {
		t.Fatalf("a hidden node changes a failed lookup's response:\n%s\n%s", bodies[0], bodies[2])
	}
}

// TestCoverage941M2_LookupRunsWhateverTheKeys: the visibility lookup runs
// even when no node is inside the radius, so a failed lookup serves reach
// null whether or not a hidden node is there, the same bytes. A lookup
// skipped for an empty key set would serve an empty reach without the
// hidden node and null with it.
func TestCoverage941M2_LookupRunsWhateverTheKeys(t *testing.T) {
	outside := reachTestNode{reachPK('1'), "Outside", "repeater", 0, 8000} // beyond r=5
	hidden := reachTestNode{strings.Repeat("ef", 32), "private-roof", "repeater", 1500, 0}
	q := reachQuery("56.1629,10.2039", "r=5")
	// With the lookup working, no node inside the radius is an empty
	// reach, not null.
	if r := decodeCoverage(t, reachHiddenEnv(t, []reachTestNode{outside}, nil).get(q)).Reach; r == nil || len(r.Nodes) != 0 || r.NoTerrain != 0 || r.Omitted != 0 {
		t.Fatalf("reach with no node inside %+v", r)
	}
	var bodies []string
	for _, nodes := range [][]reachTestNode{{outside}, {outside, hidden}} {
		env := reachHiddenEnv(t, nodes, nil)
		if _, err := env.srv.db.conn.Exec(`DROP TABLE observers`); err != nil {
			t.Fatal(err)
		}
		for _, state := range []string{"miss", "hit"} {
			w := env.get(q)
			if w.Header().Get("X-CoreScope-Cache") != state || !strings.Contains(w.Body.String(), `"reach":null`) {
				t.Fatalf("%d nodes, %s: %s", len(nodes), state, w.Body.String())
			}
			bodies = append(bodies, w.Body.String())
		}
	}
	if bodies[0] != bodies[2] || bodies[1] != bodies[3] {
		t.Fatalf("a hidden node changes a failed lookup's response:\n%s\n%s", bodies[0], bodies[2])
	}
}

// TestCoverage941M2_NodeGoneFromTheSnapshotAfterTheHit: a node that leaves
// the snapshot after its result was cached, listed or without terrain, is
// in no list and no count on the hit: the response is the one without it.
func TestCoverage941M2_NodeGoneFromTheSnapshotAfterTheHit(t *testing.T) {
	nodes := []reachTestNode{
		{reachPK('1'), "Stays", "repeater", 0, -2000},
		{reachPK('2'), "Goes", "room", 2500, 0},
		{reachPK('3'), "Goes past the gap", "repeater", 0, 4000}, // no terrain on its path
	}
	q := reachQuery("56.1629,10.2039", "r=5")
	env := reachHiddenEnv(t, nodes, nil)
	if r := decodeCoverage(t, env.get(q)).Reach; r == nil || len(r.Nodes) != 2 || r.NoTerrain != 1 {
		t.Fatalf("reach before %+v", r)
	}
	if _, err := env.srv.db.conn.Exec(`DELETE FROM nodes WHERE public_key IN (?, ?)`, reachPK('2'), reachPK('3')); err != nil {
		t.Fatal(err)
	}
	env.srv.store.InvalidateNodeCache()
	a := env.get(q)
	b := reachHiddenEnv(t, nodes[:1], nil).get(q)
	if a.Header().Get("X-CoreScope-Cache") != "hit" || a.Body.String() != b.Body.String() {
		t.Fatalf("gone after the hit (%s):\nhit     %s\nwithout %s", a.Header().Get("X-CoreScope-Cache"), a.Body.String(), b.Body.String())
	}
}

// TestCoverage941M2_NoExtraUpstreamCalls: nodes in reach use the tiles the
// raster already loaded. With 300 nodes in a 10 km disk a cold computation
// makes exactly the planned tile fetches, the same as with none, and a
// cache hit makes none.
func TestCoverage941M2_NoExtraUpstreamCalls(t *testing.T) {
	// A 60 m grid: several tiles to plan at a quarter of the cells, so the
	// test stays quick under -race.
	const res = 60
	e0, n0 := dhm.ToUTM32(reachLat, reachLon)
	planned := int64(len(dhm.PlanCoverageTiles(e0, n0, 10000, res)))
	counts := map[bool]int64{}
	for _, withNodes := range []bool{false, true} {
		src := newFakeElevation(t, hillyElev)
		env := newCoverageEnv(t, src, CoverageConfig{ResolutionM: intp(res)}, nil, nil)
		if withNodes {
			for i := 0; i < 300; i++ {
				a := float64(i) * 2.399963
				d := 9500 * math.Sqrt(float64(i+1)/300)
				addReachNode(t, env, fmt.Sprintf("%064x", i+1), fmt.Sprintf("N%d", i), []string{"repeater", "room"}[i%2], d*math.Sin(a), d*math.Cos(a))
			}
			env.srv.store.InvalidateNodeCache()
		}
		resp := decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "r=10")))
		counts[withNodes] = src.calls.Load()
		if withNodes && (resp.Reach == nil || len(resp.Reach.Nodes)+resp.Reach.NoTerrain != 300) {
			t.Fatalf("evaluated %+v", resp.Reach)
		}
		if w := env.get(reachQuery("56.1629,10.2039", "r=10")); w.Header().Get("X-CoreScope-Cache") != "hit" || src.calls.Load() != counts[withNodes] {
			t.Fatal("a hit fetched")
		}
	}
	if counts[false] != planned || counts[true] != planned {
		t.Fatalf("upstream calls: %d without nodes, %d with 300 (planned %d)", counts[false], counts[true], planned)
	}
}

// reachBoundEnv is a reach over flat ground with the hidden-name prefix
// "private-", holding n visible repeaters spread over a 10 km disk 200 to
// 9700 m out, and the nodes of extra, inserted in one transaction.
func reachBoundEnv(t *testing.T, n int, extra []reachTestNode) *coverageEnv {
	t.Helper()
	env := newCoverageEnv(t, newFakeElevation(t, flatElev(3)), CoverageConfig{}, nil, nil)
	env.srv.cfg.SetHiddenNamePrefixes([]string{"private-"})
	tx, err := env.srv.db.conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	insert := func(pk, name, role string, dE, dN float64) {
		lat, lon := reachPos(dE, dN)
		if _, err := tx.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count)
			VALUES (?, ?, ?, ?, ?, datetime('now'), datetime('now'), 1)`, pk, name, role, lat, lon); err != nil {
			t.Fatal(err)
		}
	}
	for _, x := range extra {
		insert(x.pk, x.name, x.role, x.dE, x.dN)
	}
	for i := 0; i < n; i++ {
		a := float64(i) * 2.399963
		d := 200 + 9500*math.Sqrt(float64(i)/float64(n))
		insert(fmt.Sprintf("%064x", i+1), fmt.Sprintf("Node %d with a long name", i), "repeater", d*math.Sin(a), d*math.Cos(a))
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	env.srv.store.InvalidateNodeCache()
	return env
}

// TestCoverage941M2_ReachIsBounded: more nodes than the list holds keeps
// the lowest losses and counts the rest as omitted; the cached result
// stays within its byte bound.
func TestCoverage941M2_ReachIsBounded(t *testing.T) {
	const n = coverageReachMaxNodes + 37
	// A hidden node with the lowest loss of all: it takes no place in the
	// list and is not among the omitted.
	hiddenPK := strings.Repeat("ab", 32)
	env := reachBoundEnv(t, n, []reachTestNode{{hiddenPK, "private-closest", "repeater", 0, 100}})
	resp := decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "r=10")))
	r := resp.Reach
	if r == nil || len(r.Nodes) != coverageReachMaxNodes || r.Omitted != 37 {
		t.Fatalf("reach: %d listed, %d omitted", len(r.Nodes), r.Omitted)
	}
	if r.Nodes[0].PublicKey == hiddenPK || strings.Contains(strings.ToLower(fmt.Sprint(resp)), "private-closest") {
		t.Fatal("the hidden node is listed")
	}
	if !sort.SliceIsSorted(r.Nodes, func(a, b int) bool { return r.Nodes[a].LossDb < r.Nodes[b].LossDb }) {
		t.Fatal("not in loss order")
	}
	data, ok := env.c.results.Get(resp.Key, env.c.now())
	if !ok || len(data) > coverageResultMaxBytes {
		t.Fatalf("cached entry %d B (bound %d)", len(data), coverageResultMaxBytes)
	}
	e, _ := decodeCoverageEntry(data)
	meta, _ := json.Marshal(e.meta.Reach)
	if per := len(meta) / len(e.meta.Reach.Nodes); per > coverageReachEntryBytes {
		t.Fatalf("%d B per kept node > %d", per, coverageReachEntryBytes)
	}
}

// TestCoverage941M2_HiddenPastTheBoundIsNotOmitted: a hidden node farther
// out than every visible one, so past the list's bound, is not among the
// omitted either. The bound applies to visible nodes only; otherwise
// omitted would count a hidden node whenever the radius holds more nodes
// than the list.
func TestCoverage941M2_HiddenPastTheBoundIsNotOmitted(t *testing.T) {
	const n = coverageReachMaxNodes + 37
	hiddenPK := strings.Repeat("ab", 32)
	env := reachBoundEnv(t, n, []reachTestNode{{hiddenPK, "private-farthest", "repeater", 0, 9900}})
	resp := decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "r=10")))
	// The premise: the hidden node was evaluated, the lossiest of all.
	data, ok := env.c.results.Get(resp.Key, env.c.now())
	if !ok {
		t.Fatal("no cached result")
	}
	e, _ := decodeCoverageEntry(data)
	if kept := e.meta.Reach.Nodes; len(kept) != n+1 || kept[n].PubKey != hiddenPK {
		t.Fatalf("the hidden node is not the lossiest of %d kept", len(kept))
	}
	r := resp.Reach
	if r == nil || len(r.Nodes) != coverageReachMaxNodes || r.Omitted != 37 {
		t.Fatalf("reach: %d listed, %d omitted (want %d, 37)", len(r.Nodes), r.Omitted, coverageReachMaxNodes)
	}
}

// TestCoverage941M2_NoSnapshotNoReach: without a node snapshot the raster
// is served with reach null, and the result is kept only briefly so a
// later request evaluates the nodes.
func TestCoverage941M2_NoSnapshotNoReach(t *testing.T) {
	env := newCoverageEnv(t, newFakeElevation(t, flatElev(3)), CoverageConfig{}, nil, nil)
	now := time.Now()
	env.c.now = func() time.Time { return now }
	store := env.srv.store
	env.srv.store = nil
	w := env.get(reachQuery("56.1629,10.2039", "r=5"))
	if resp := decodeCoverage(t, w); resp.Reach != nil || !strings.Contains(w.Body.String(), `"reach":null`) {
		t.Fatalf("reach %+v", resp.Reach)
	}
	env.srv.store = store
	now = now.Add(coveragePartialTTL + time.Second)
	w = env.get(reachQuery("56.1629,10.2039", "r=5"))
	if w.Header().Get("X-CoreScope-Cache") != "miss" || decodeCoverage(t, w).Reach == nil {
		t.Fatalf("after the partial TTL: %s", w.Header().Get("X-CoreScope-Cache"))
	}
}

// TestCoverage941M2_ClassesMatchTheClient: the server's margin classes
// (the counts) are the panel's CLASSES in public/coverage.js.
func TestCoverage941M2_ClassesMatchTheClient(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "public", "coverage.js"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`\{ id: '(\w+)', min: (-?[0-9.]+),`)
	var js []string
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		js = append(js, m[1]+"="+m[2])
	}
	var gos []string
	for _, c := range coverageClasses {
		gos = append(gos, fmt.Sprintf("%s=%g", c.id, c.minDb))
	}
	if strings.Join(js, ",") != strings.Join(gos, ",") {
		t.Fatalf("client classes %v, server %v", js, gos)
	}
	for _, c := range []struct {
		m    float64
		want string
	}{{10, "good"}, {9.99, "marginal"}, {3, "marginal"}, {2.99, "weak"}, {0, "weak"}, {-0.01, "none"}, {math.NaN(), "none"}} {
		if got := coverageMarginClass(c.m); got != c.want {
			t.Fatalf("margin %v: %s, want %s", c.m, got, c.want)
		}
	}
}

// TestCoverage941M2_PerfBudget: a warm 40 km / 30 m computation with the
// complete method and 2000 nodes in the disk, end to end in the handler.
// The M1 budget is 3 s for a warm computation; the check here is
// machine-independent (delta at most 2.2x the Bullington computation), and
// the absolute times are logged for the PR.
func TestCoverage941M2_PerfBudget(t *testing.T) {
	if testing.Short() || raceBuild {
		t.Skip("timing budget: not under -short or -race")
	}
	env := newCoverageEnv(t, newFakeElevation(t, func(e, n float64) float32 {
		x, y := e/1000, n/1000
		return float32(math.Max(0, 40+25*math.Sin(x/3.1)*math.Cos(y/4.3)+15*math.Sin(x/0.9+y/1.7)+8*math.Cos(x/0.37-y/0.53)))
	}), CoverageConfig{}, nil, nil)
	tx, _ := env.srv.db.conn.Begin()
	for i := 0; i < 2000; i++ {
		a := float64(i) * 2.399963
		d := 39500 * math.Sqrt(float64(i+1)/2000)
		lat, lon := reachPos(d*math.Sin(a), d*math.Cos(a))
		tx.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count)
			VALUES (?, ?, ?, ?, ?, datetime('now'), datetime('now'), 1)`, fmt.Sprintf("%064x", i+1), fmt.Sprintf("N%d", i), []string{"repeater", "room"}[i%2], lat, lon)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	env.srv.store.InvalidateNodeCache()
	decodeCoverage(t, env.get(reachQuery("56.1629,10.2039"))) // cold: loads the tiles
	timed := func(q string) (time.Duration, CoverageResponse) {
		start := time.Now()
		resp := decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", q)))
		return time.Since(start), resp
	}
	bull, _ := timed("ha=12&model=bullington")
	delta, resp := timed("ha=12")
	t.Logf("warm 40 km / 30 m with 2000 nodes: bullington %v, delta %v (%.2fx); %d listed, %d without terrain",
		bull.Round(time.Millisecond), delta.Round(time.Millisecond), float64(delta)/float64(bull), len(resp.Reach.Nodes), resp.Reach.NoTerrain)
	if len(resp.Reach.Nodes)+resp.Reach.Omitted+resp.Reach.NoTerrain != 2000 {
		t.Fatalf("evaluated %d", len(resp.Reach.Nodes)+resp.Reach.Omitted)
	}
	if float64(delta) > 2.2*float64(bull) {
		t.Fatalf("delta %v > 2.2 x bullington %v", delta, bull)
	}
}

// TestCoverage941M2_OpenAPIReach: the nodes in reach are documented, and
// hidden identities are left out of every count, not counted on their own.
func TestCoverage941M2_OpenAPIReach(t *testing.T) {
	d := routeDescriptions()["GET "+coverageRoute].Description
	for _, w := range []string{"reach", "budgetDb", "lossDb", "repeater and room", "is in no count"} {
		if !strings.Contains(d, w) {
			t.Fatalf("%q undocumented", w)
		}
	}
	if strings.Contains(d, "withheld") {
		t.Fatal("the description still documents a count of hidden nodes")
	}
}

// TestCoverage941M2_CountsPerClass: the counts are the listed nodes'
// classes at the operator's link budget, one per class here (158 dB:
// margins 18, 8, 2 and −2 dB), boundaries to the better class.
func TestCoverage941M2_CountsPerClass(t *testing.T) {
	env := newCoverageEnv(t, newFakeElevation(t, flatElev(3)), CoverageConfig{}, nil, nil)
	losses := []float64{140, 150, 156, 160, 148, 155, 158}
	meta := &coverageReachMeta{}
	for i, l := range losses {
		pk := fmt.Sprintf("%064x", i+1)
		addReachNode(t, env, pk, fmt.Sprintf("N%d", i), "repeater", float64(i)*100, 500)
		meta.Nodes = append(meta.Nodes, coverageReachEntry{PubKey: pk, Role: "repeater", DistanceM: 500, HeightM: 10, HeightSource: "role-default", LossDb: l})
	}
	env.srv.store.InvalidateNodeCache()
	r := env.srv.coverageReachView(context.Background(), env.c, meta)
	// 148 is exactly 10 dB (good), 155 exactly 3 (marginal), 158 exactly 0 (weak).
	if want := (CoverageReachCounts{Good: 2, Marginal: 2, Weak: 2, None: 1}); r.Counts != want || r.BudgetDb != 158 || len(r.Nodes) != 7 {
		t.Fatalf("counts %+v at %v dB, want %+v", r.Counts, r.BudgetDb, want)
	}
}
