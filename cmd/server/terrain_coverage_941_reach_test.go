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
// elevation grid, hidden identities withheld at serve time. Every test
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
	env := newCoverageEnv(t, newFakeElevation(t, ridgeEastElev), CoverageConfig{}, radio, nil)
	centre := reachPK('c')
	addReachNode(t, env, centre, "Centre mast", "repeater", 0, 0)
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
		if r.Withheld != 0 || r.Omitted != 0 || r.NoTerrain != 0 || strings.Join(r.Roles, ",") != "repeater,room" {
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
		spec := dhm.CoverageSpec{Center: dhm.LatLon{Lat: reachLat, Lon: reachLon}, RadiusM: 12000, ResolutionM: 30,
			TxHeightM: 10, FreqMHz: dhm.FirmwareFreqMHz, K: dhm.DefaultKFactor, Model: dhm.PathModel(model)}
		e0, n0 := dhm.ToUTM32(reachLat, reachLon)
		grid := dhm.NewCoverageGrid(e0, n0, 12000, 30)
		for _, tl := range dhm.PlanCoverageTiles(e0, n0, 12000, 30) {
			vals := make([]float32, dhm.CoverageTileCells)
			for row := 0; row < dhm.CoverageTilePx; row++ {
				for col := 0; col < dhm.CoverageTilePx; col++ {
					vals[row*dhm.CoverageTilePx+col] = ridgeEastElev(tl.Cell(30, col, row))
				}
			}
			grid.Paste(tl, vals)
		}
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

// TestCoverage941M2_HiddenNodesAreWithheld: the raw response bytes never
// carry a hidden identity's key (in any case) or name, whether it is
// hidden by the node blacklist, the observer blacklist, a hidden-name
// prefix on the node or only through an observer alias; each is counted
// as withheld. Visibility is decided at serve time, so a rename into a
// hidden prefix hides a node of a cached result on the next request, with
// no new computation and no upstream call.
func TestCoverage941M2_HiddenNodesAreWithheld(t *testing.T) {
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

	check := func(label string, wantVisible bool, wantWithheld int) []byte {
		t.Helper()
		w := env.get(reachQuery("56.1629,10.2039", "r=5"))
		body := w.Body.Bytes()
		r := decodeCoverage(t, w).Reach
		if r == nil || r.Withheld != wantWithheld || (len(r.Nodes) == 1) != wantVisible {
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
	check("miss", true, 4)
	calls := src.calls.Load()
	check("hit", true, 4)
	// Rename the visible node into the hidden prefix: hidden on the next
	// request, from the same cached result.
	if _, err := env.srv.db.conn.Exec(`UPDATE nodes SET name = 'private-now' WHERE public_key = ?`, visible); err != nil {
		t.Fatal(err)
	}
	env.srv.store.InvalidateNodeCache()
	hidden[visible] = "private-now"
	body := check("renamed", false, 5)
	if src.calls.Load() != calls {
		t.Fatal("the visibility change recomputed")
	}
	if strings.Contains(string(body), "Visible mast") {
		t.Fatal("the old name is served")
	}
}

// TestCoverage941M2_LookupFailureWithholdsAll: when the visibility lookup
// fails, every node is withheld and none is named (fail closed); the
// raster is still served, and the failure is logged without detail.
func TestCoverage941M2_LookupFailureWithholdsAll(t *testing.T) {
	env := newCoverageEnv(t, newFakeElevation(t, flatElev(3)), CoverageConfig{}, nil, nil)
	env.srv.cfg.SetHiddenNamePrefixes([]string{"private-"})
	addReachNode(t, env, reachPK('1'), "Visible one", "repeater", 0, 2000)
	addReachNode(t, env, reachPK('2'), "Visible two", "room", 0, -2000)
	env.srv.store.InvalidateNodeCache()
	if _, err := env.srv.db.conn.Exec(`DROP TABLE observers`); err != nil {
		t.Fatal(err)
	}
	w := env.get(reachQuery("56.1629,10.2039", "r=5"))
	resp := decodeCoverage(t, w)
	if resp.Reach == nil || resp.Reach.Withheld != 2 || len(resp.Reach.Nodes) != 0 || resp.Reach.Counts != (CoverageReachCounts{}) {
		t.Fatalf("reach %+v", resp.Reach)
	}
	if strings.Contains(w.Body.String(), "Visible") || strings.Contains(w.Body.String(), reachPK('1')) {
		t.Fatalf("a node was named after a failed lookup: %s", w.Body.String())
	}
	if !strings.Contains(env.logs.String(), "visibility lookup failed") || strings.Contains(env.logs.String(), "observers") {
		t.Fatalf("log %q", env.logs.String())
	}
	if env.raster(resp.Raster.URL, true).Code != 200 {
		t.Fatal("the raster is not served")
	}
}

// TestCoverage941M2_NoExtraUpstreamCalls: nodes in reach use the tiles the
// raster already loaded. With 300 nodes in the disk a cold computation
// makes exactly the planned tile fetches, the same as with none, and a
// cache hit makes none.
func TestCoverage941M2_NoExtraUpstreamCalls(t *testing.T) {
	e0, n0 := dhm.ToUTM32(reachLat, reachLon)
	planned := int64(len(dhm.PlanCoverageTiles(e0, n0, 20000, 30)))
	counts := map[bool]int64{}
	for _, withNodes := range []bool{false, true} {
		src := newFakeElevation(t, hillyElev)
		env := newCoverageEnv(t, src, CoverageConfig{}, nil, nil)
		if withNodes {
			for i := 0; i < 300; i++ {
				a := float64(i) * 2.399963
				d := 19500 * math.Sqrt(float64(i+1)/300)
				addReachNode(t, env, fmt.Sprintf("%064x", i+1), fmt.Sprintf("N%d", i), []string{"repeater", "room"}[i%2], d*math.Sin(a), d*math.Cos(a))
			}
			env.srv.store.InvalidateNodeCache()
		}
		resp := decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "r=20")))
		counts[withNodes] = src.calls.Load()
		if withNodes && (resp.Reach == nil || len(resp.Reach.Nodes)+resp.Reach.NoTerrain != 300) {
			t.Fatalf("evaluated %+v", resp.Reach)
		}
		if w := env.get(reachQuery("56.1629,10.2039", "r=20")); w.Header().Get("X-CoreScope-Cache") != "hit" || src.calls.Load() != counts[withNodes] {
			t.Fatal("a hit fetched")
		}
	}
	if counts[false] != planned || counts[true] != planned {
		t.Fatalf("upstream calls: %d without nodes, %d with 300 (planned %d)", counts[false], counts[true], planned)
	}
}

// TestCoverage941M2_ReachIsBounded: more nodes than the list holds keeps
// the lowest losses and counts the rest as omitted; the cached result
// stays within its byte bound.
func TestCoverage941M2_ReachIsBounded(t *testing.T) {
	env := newCoverageEnv(t, newFakeElevation(t, flatElev(3)), CoverageConfig{}, nil, nil)
	const n = coverageReachMaxNodes + 37
	tx, err := env.srv.db.conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		a := float64(i) * 2.399963
		d := 200 + 9500*math.Sqrt(float64(i)/n)
		lat, lon := reachPos(d*math.Sin(a), d*math.Cos(a))
		if _, err := tx.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count)
			VALUES (?, ?, 'repeater', ?, ?, datetime('now'), datetime('now'), 1)`, fmt.Sprintf("%064x", i+1), fmt.Sprintf("Node %d with a long name", i), lat, lon); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	env.srv.store.InvalidateNodeCache()
	resp := decodeCoverage(t, env.get(reachQuery("56.1629,10.2039", "r=10")))
	r := resp.Reach
	if r == nil || len(r.Nodes) != coverageReachMaxNodes || r.Omitted != 37 {
		t.Fatalf("reach: %d listed, %d omitted", len(r.Nodes), r.Omitted)
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
	if per := len(meta) / coverageReachMaxNodes; per > coverageReachEntryBytes {
		t.Fatalf("%d B per kept node > %d", per, coverageReachEntryBytes)
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
	bull, _ := timed("ha=12")
	delta, resp := timed("ha=12&model=delta")
	t.Logf("warm 40 km / 30 m with 2000 nodes: bullington %v, delta %v (%.2fx); %d listed, %d without terrain",
		bull.Round(time.Millisecond), delta.Round(time.Millisecond), float64(delta)/float64(bull), len(resp.Reach.Nodes), resp.Reach.NoTerrain)
	if len(resp.Reach.Nodes)+resp.Reach.Omitted+resp.Reach.NoTerrain != 2000 {
		t.Fatalf("evaluated %d", len(resp.Reach.Nodes)+resp.Reach.Omitted)
	}
	if float64(delta) > 2.2*float64(bull) {
		t.Fatalf("delta %v > 2.2 x bullington %v", delta, bull)
	}
}

// TestCoverage941M2_OpenAPIReach: the nodes in reach are documented.
func TestCoverage941M2_OpenAPIReach(t *testing.T) {
	d := routeDescriptions()["GET "+coverageRoute].Description
	for _, w := range []string{"reach", "withheld", "budgetDb", "lossDb", "repeater and room"} {
		if !strings.Contains(d, w) {
			t.Fatalf("%q undocumented", w)
		}
	}
}
