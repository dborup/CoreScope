package main

// Measurements for #488: how the candidate direct-evidence-first rule
// (region_placement_proto_488_test.go) changes /api/nodes?region= membership
// against the real cache (nodes_region_cache.go), on
//   - a synthetic scenario with known ground truth (deterministic, always run);
//   - test-fixtures/e2e-fixture.db (always run; numbers logged with -v);
//   - a large generated database for cold/warm/delta cost and memory
//     (env-gated, like nodes_region_cache_perf_176_test.go):
//
//	CORESCOPE_PERF_488_GEN=/path/big.db CORESCOPE_PERF_488_SCALE=60 \
//	  go test -run TestPerf_GenerateRegionPlacementDB_488 -timeout 1h .
//	CORESCOPE_PERF_488_DB=/path/big.db \
//	  go test -run TestPerf_RegionPlacement488 -v -timeout 1h .

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// rp488Regions lie on a line: AAA–BBB–CCC–DDD. A flood advert reaches a region
// d steps away after about 2d more relays.
var rp488Regions = []string{"AAA", "BBB", "CCC", "DDD"}

const rp488ObsPerRegion = 3

// rp488Seeder writes a scenario through an ordinary connection (test-only:
// the server's own handle is read-only).
type rp488Seeder struct {
	t    testing.TB
	exec interface {
		Exec(string, ...any) (sql.Result, error)
	}
	seq int
}

func (s *rp488Seeder) obs(region, i int) int { return region*rp488ObsPerRegion + i + 1 }

func rp488Payload(pk string, seq int) string { return pk + fmt.Sprintf("%08x", seq) + "00112233" }

func rp488FloodRaw(header byte, codes string, pk string, seq, hops int) (raw, path string) {
	hs := make([]string, hops)
	b := strings.Builder{}
	fmt.Fprintf(&b, "%02x%s%02x", header, codes, hops)
	for i := range hops {
		hs[i] = fmt.Sprintf("%02X", 0xA0+i)
		b.WriteString(hs[i])
	}
	b.WriteString(rp488Payload(pk, seq))
	if hops == 0 {
		return b.String(), "[]"
	}
	return b.String(), `["` + strings.Join(hs, `","`) + `"]`
}

// advert writes one ADVERT transmission and returns its id.
func (s *rp488Seeder) advert(pk string, ts int64, raw string) int64 {
	s.seq++
	decoded := `{"type":"ADVERT","pubKey":"` + pk + `"}`
	res, err := s.exec.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json)
		VALUES (?, ?, ?, ?, 4, ?)`, raw, fmt.Sprintf("rp488-%d", s.seq),
		time.Unix(ts, 0).UTC().Format(time.RFC3339), int(raw[1]-'0')&3, decoded)
	if err != nil {
		s.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (s *rp488Seeder) hear(txID int64, obs int, ts int64, raw, path sql.NullString) {
	if _, err := s.exec.Exec(`INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp, raw_hex, snr, rssi)
		VALUES (?, ?, ?, ?, ?, 5, -100)`, txID, obs, path, ts, raw); err != nil {
		s.t.Fatal(err)
	}
}

// directAdvert: a zero-hop DIRECT advert heard by observers obs.
func (s *rp488Seeder) directAdvert(pk string, ts int64, obs ...int) int64 {
	raw := fmt.Sprintf("1200%s", rp488Payload(pk, s.seq+1))
	id := s.advert(pk, ts, raw)
	for _, o := range obs {
		s.hear(id, o, ts, rp488Str(raw), rp488Str("[]"))
	}
	return id
}

// floodAdvert: one flood advert; byRegion maps a region index to the hop
// count its observer 0 hears it at (header 0x11, or 0x10 with codes).
func (s *rp488Seeder) floodAdvert(pk string, ts int64, header byte, codes string, byRegion map[int]int) {
	first := ""
	var id int64
	regions := make([]int, 0, len(byRegion))
	for r := range byRegion {
		regions = append(regions, r)
	}
	sort.Ints(regions)
	for _, r := range regions {
		raw, path := rp488FloodRaw(header, codes, pk, s.seq+1, byRegion[r])
		if first == "" {
			first = raw
			id = s.advert(pk, ts, raw)
		}
		s.hear(id, s.obs(r, 0), ts, rp488Str(raw), rp488Str(path))
	}
}

// leak returns hop counts for a flood heard at h0 hops at home: +2 per
// region step away, as relays carry it down the line.
func rp488Leak(home, h0 int) map[int]int {
	m := map[int]int{}
	for r := range rp488Regions {
		d := r - home
		if d < 0 {
			d = -d
		}
		m[r] = h0 + 2*d
	}
	return m
}

// rp488SeedScenario writes scale copies of each archetype per region and
// returns the expected home regions (nil = should be unknown) and the
// expected transient regions per node.
func rp488SeedScenario(t testing.TB, exec interface {
	Exec(string, ...any) (sql.Result, error)
}, scale int, now int64) (truth, transient map[string][]string) {
	t.Helper()
	s := &rp488Seeder{t: t, exec: exec}
	for r, iata := range rp488Regions {
		for i := range rp488ObsPerRegion {
			if _, err := exec.Exec(`INSERT INTO observers (rowid, id, name, iata, last_seen, first_seen)
				VALUES (?, ?, ?, ?, '2026-10-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
				s.obs(r, i), fmt.Sprintf("obs-%s-%d", iata, i), fmt.Sprintf("Obs %s %d", iata, i), iata); err != nil {
				t.Fatal(err)
			}
		}
	}
	truth, transient = map[string][]string{}, map[string][]string{}
	day := rp488Day
	at := func(d int64) int64 { return now - d*day - 3600 }
	set := func(rs ...int) []string {
		out := make([]string, len(rs))
		for i, r := range rs {
			out[i] = rp488Regions[r]
		}
		sort.Strings(out)
		return out
	}
	for c := range scale {
		for r := range rp488Regions {
			pk := func(arch int) string { return fmt.Sprintf("%02x%02x%012x", r, arch, c) }
			resident := func(p string, from, to int64) {
				for d := from; d <= to; d++ {
					s.directAdvert(p, at(d), s.obs(r, 0), s.obs(r, 1))
					s.floodAdvert(p, at(d)+60, 0x11, "", rp488Leak(r, int(d%2)))
				}
			}
			// 1 resident repeater: zero-hop adverts at home, floods leak down the line.
			resident(pk(1), 0, 13)
			truth[pk(1)] = set(r)
			// 2 flood-only companion: never heard zero-hop; 1 hop at home.
			for d := int64(0); d < 5; d++ {
				s.floodAdvert(pk(2), at(d), 0x11, "", rp488Leak(r, 1))
			}
			truth[pk(2)] = set(r)
			// 3 boundary node between r and r+1, heard directly in both.
			if r+1 < len(rp488Regions) {
				for d := int64(0); d < 7; d++ {
					s.directAdvert(pk(3), at(d), s.obs(r, 0), s.obs(r+1, 0))
				}
				truth[pk(3)] = set(r, r+1)
			}
			// 4 resident with one long-distance direct hearing two regions away.
			far := (r + 2) % len(rp488Regions)
			for d := int64(0); d < 10; d++ {
				id := s.directAdvert(pk(4), at(d), s.obs(r, 0))
				if d == 5 {
					raw := fmt.Sprintf("1200%s", rp488Payload(pk(4), int(id)))
					s.hear(id, s.obs(far, 1), at(d), rp488Str(raw), rp488Str("[]"))
				}
			}
			truth[pk(4)], transient[pk(4)] = set(r), set(far)
			// 5 resident whose advert a companion re-shares into another
			// region (TRANSPORT_DIRECT, codes {0,0}), as an extra observation
			// of the same transmission, on three days.
			other := (r + 3) % len(rp488Regions)
			for d := int64(0); d < 10; d++ {
				id := s.directAdvert(pk(5), at(d), s.obs(r, 0))
				if d%3 == 1 {
					raw := "1300000000" + "00" + rp488Payload(pk(5), int(id))
					s.hear(id, s.obs(other, 2), at(d)+600, rp488Str(raw), rp488Str("[]"))
				}
			}
			truth[pk(5)] = set(r)
			// 6 far-only: heard only after 5 relays.
			for d := int64(0); d < 5; d++ {
				s.floodAdvert(pk(6), at(d), 0x11, "", map[int]int{r: 5})
			}
			truth[pk(6)] = nil
			// 7 quiet resident: nothing in the last 90 days.
			resident(pk(7), 90, 99)
			truth[pk(7)] = set(r)
			// 8 moved from r to r+1 (mod 4) 60 days ago.
			next := (r + 1) % len(rp488Regions)
			for d := int64(60); d < 100; d++ {
				s.directAdvert(pk(8), at(d), s.obs(r, 0))
			}
			for d := int64(0); d < 20; d++ {
				s.directAdvert(pk(8), at(d), s.obs(next, 0))
			}
			truth[pk(8)] = set(next)
			// 9 legacy rows only: NULL path_json, no raw.
			for d := int64(0); d < 5; d++ {
				id := s.advert(pk(9), at(d), "1100"+rp488Payload(pk(9), int(d)))
				s.hear(id, s.obs(r, 2), at(d), rp488Null(), rp488Null())
			}
			truth[pk(9)] = nil
			// 10 scoped repeater: zero-hop TRANSPORT_FLOOD adverts at home.
			for d := int64(0); d < 10; d++ {
				s.floodAdvert(pk(10), at(d), 0x10, "1a2b3c4d", map[int]int{r: 0})
			}
			truth[pk(10)] = set(r)
		}
	}
	return truth, transient
}

// rp488Accuracy counts nodes whose region set matches truth exactly.
func rp488Accuracy(truth map[string][]string, got map[string][]string) (exact int, wrong []string) {
	for pk, want := range truth {
		g := got[pk]
		if len(g) == 0 && len(want) == 0 || reflect.DeepEqual(g, want) {
			exact++
		} else {
			wrong = append(wrong, pk)
		}
	}
	sort.Strings(wrong)
	return exact, wrong
}

// rp488RegionSets maps each node to the sorted regions whose filter includes
// it, by asking per-region membership.
func rp488RegionSets(members func(region string) []string, regions []string) map[string][]string {
	out := map[string][]string{}
	for _, r := range regions {
		for _, pk := range members(r) {
			out[pk] = append(out[pk], r)
		}
	}
	for pk := range out {
		sort.Strings(out[pk])
	}
	return out
}

// TestRP488_SyntheticAccuracy: the candidate reproduces the ground truth of
// every archetype; the current filter gets only the boundary node, the
// scoped repeater and nothing else right. Counts are pinned so a change to
// either side shows up here.
func TestRP488_SyntheticAccuracy(t *testing.T) {
	db := rp488EmptyTestDB(t)
	truth, transient := rp488SeedScenario(t, db.conn, 1, rp488T0)
	agg := rp488Build(t, db, 0, rp488MaxID(t, db), nil)
	pl := rp488Placements(agg, rp488DefaultParams)

	cand := rp488RegionSets(func(r string) []string { return rp488Members(pl, []string{r}) }, rp488Regions)
	cur := rp488RegionSets(func(r string) []string { return rp488CurrentMembers(t, db, []string{r}) }, rp488Regions)

	candExact, candWrong := rp488Accuracy(truth, cand)
	curExact, curWrong := rp488Accuracy(truth, cur)
	t.Logf("synthetic: %d nodes; exact region set: candidate %d, current %d", len(truth), candExact, curExact)
	t.Logf("current wrong: %v", curWrong)
	if len(candWrong) != 0 {
		for _, pk := range candWrong {
			t.Errorf("candidate %s: got %v (%+v), want %v", pk, cand[pk], pl[pk], truth[pk])
		}
	}
	for pk, want := range transient {
		if !reflect.DeepEqual(pl[pk].Transient, want) {
			t.Errorf("candidate %s transient %v, want %v", pk, pl[pk].Transient, want)
		}
	}
	// 39 nodes: current is right only for the 3 boundary nodes and the 4
	// scoped repeaters (heard at home only).
	if len(truth) != 39 || curExact != 7 {
		t.Errorf("pinned counts moved: nodes %d (want 39), current exact %d (want 7)", len(truth), curExact)
	}
	r := rp488Report(agg, pl, func(r string) []string { return rp488CurrentMembers(t, db, []string{r}) }, rp488Regions)
	t.Logf("\n%s", r)
	if r.added != 0 {
		t.Errorf("candidate added %d memberships the current filter does not have", r.added)
	}
}

type rp488Summary struct {
	nodes, direct, fewest, unknown, transient int
	byHops                                    map[int]int
	rows                                      []string
	removed, added                            int
	kinds                                     map[string]int
}

func (s rp488Summary) String() string {
	b := &strings.Builder{}
	fmt.Fprintf(b, "nodes with advert evidence in a region: %d — direct %d, fewest_hops %d %v, unknown %d; nodes with transient regions %d\n",
		s.nodes, s.direct, s.fewest, s.byHops, s.unknown, s.transient)
	fmt.Fprintf(b, "observation kinds: %v\n", s.kinds)
	fmt.Fprintf(b, "%-6s %8s %10s %8s %8s\n", "region", "current", "candidate", "removed", "added")
	for _, r := range s.rows {
		b.WriteString(r + "\n")
	}
	fmt.Fprintf(b, "memberships removed %d, added %d", s.removed, s.added)
	return b.String()
}

// rp488Report compares candidate placements with current per-region
// membership.
func rp488Report(agg *rp488Agg, pl map[string]rp488Placement, current func(string) []string, regions []string) rp488Summary {
	s := rp488Summary{byHops: map[int]int{}, kinds: map[string]int{}}
	for pk, p := range pl {
		s.nodes++
		switch p.Basis {
		case rp488BasisDirect:
			s.direct++
		case rp488BasisFewest:
			s.fewest++
			s.byHops[p.Hops]++
		default:
			s.unknown++
		}
		if len(p.Transient) > 0 {
			s.transient++
		}
		for _, st := range agg.Nodes[pk] {
			s.kinds["shared"] += st.Shared
			s.kinds["unknown"] += st.Unknown
			s.kinds["far"] += st.Far
		}
	}
	for _, r := range regions {
		cur := current(r)
		cand := rp488Members(pl, []string{r})
		in := map[string]bool{}
		for _, pk := range cand {
			in[pk] = true
		}
		removed := 0
		for _, pk := range cur {
			if !in[pk] {
				removed++
			}
			delete(in, pk)
		}
		s.removed += removed
		s.added += len(in)
		s.rows = append(s.rows, fmt.Sprintf("%-6s %8d %10d %8d %8d", r, len(cur), len(cand), removed, len(in)))
	}
	return s
}

func rp488Copy(t testing.TB, src string) string {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Skipf("fixture not available: %v", err)
	}
	defer in.Close()
	dst := filepath.Join(t.TempDir(), filepath.Base(src))
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return dst
}

func rp488DBRegions(t testing.TB, db *DB) []string {
	t.Helper()
	rows, err := db.conn.Query(`SELECT DISTINCT UPPER(TRIM(iata)) FROM observers WHERE iata IS NOT NULL AND TRIM(iata) != '' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// TestRP488_FixtureMeasurement runs current vs candidate on the e2e fixture,
// opened read-only through the server's own OpenDB. It logs the table posted
// on #488 and asserts the invariants (the candidate never adds a membership;
// every placed node has a basis), not the counts, so refreshing the fixture
// does not break it.
func TestRP488_FixtureMeasurement(t *testing.T) {
	path := rp488Copy(t, filepath.Join("..", "..", "test-fixtures", "e2e-fixture.db"))
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	regions := rp488DBRegions(t, db)
	if len(regions) == 0 {
		t.Skip("fixture has no observer regions")
	}
	ctx := context.Background()
	wm, err := db.maxObservationID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	agg := newRP488Agg()
	if _, err := rp488ScanAgg(ctx, db, 0, wm, false, agg); err != nil {
		t.Fatal(err)
	}
	pl := rp488Placements(agg, rp488DefaultParams)
	cur := func(r string) []string { return rp488CurrentMembers(t, db, []string{r}) }
	s := rp488Report(agg, pl, cur, regions)
	t.Logf("e2e fixture (obs raw_hex column: %v)\n%s", db.hasObsRawHex(), s)
	for _, maxHops := range []int{0, 1, 2, 3, 5, 7} {
		p := rp488DefaultParams
		p.MaxHops = maxHops
		ps := rp488Report(agg, rp488Placements(agg, p), cur, regions)
		t.Logf("maxHops=%d: direct %d, fewest %d, unknown %d, memberships removed %d", maxHops, ps.direct, ps.fewest, ps.unknown, ps.removed)
	}
	if s.added != 0 {
		t.Errorf("candidate added %d memberships", s.added)
	}
	if s.direct+s.fewest+s.unknown != s.nodes {
		t.Errorf("bases do not partition the nodes: %+v", s)
	}
	allCur := map[string]bool{}
	for _, r := range regions {
		for _, pk := range cur(r) {
			allCur[pk] = true
		}
	}
	if len(allCur) != s.nodes {
		t.Errorf("candidate saw %d nodes, current filter %d: the scans disagree on scope", s.nodes, len(allCur))
	}
}

// ---- large-database cost (env-gated) ----

func TestPerf_GenerateRegionPlacementDB_488(t *testing.T) {
	path := os.Getenv("CORESCOPE_PERF_488_GEN")
	if path == "" {
		t.Skip("set CORESCOPE_PERF_488_GEN=<db path> to generate the #488 perf database")
	}
	scale, _ := strconv.Atoi(os.Getenv("CORESCOPE_PERF_488_SCALE"))
	if scale <= 0 {
		scale = 60
	}
	filler, _ := strconv.Atoi(os.Getenv("CORESCOPE_PERF_488_FILLER"))
	if filler <= 0 {
		filler = 1_000_000
	}
	db := setupTestDB(t)
	defer db.Close()
	tx, err := db.conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	truth, _ := rp488SeedScenario(t, tx, scale, time.Now().Unix())
	// Non-advert traffic around the adverts, as on a live instance, so the
	// scans pay for skipping it.
	for i := range filler {
		raw := fmt.Sprintf("1502a1a2%064x", i)
		res, err := tx.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json)
			VALUES (?, ?, '2026-10-01T00:00:00Z', 1, 5, '{"type":"GRP_TXT"}')`, raw, fmt.Sprintf("fill-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		if _, err := tx.Exec(`INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp, raw_hex)
			VALUES (?, ?, '["A1","A2"]', ?, ?)`, id, 1+i%(len(rp488Regions)*rp488ObsPerRegion), time.Now().Unix()-int64(i), raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE INDEX IF NOT EXISTS idx_transmissions_payload_type ON transmissions(payload_type)`,
		`CREATE INDEX IF NOT EXISTS idx_observations_observer_idx ON observations(observer_idx)`,
		`ANALYZE`,
	} {
		if _, err := db.conn.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Remove(path)
	if _, err := db.conn.Exec(`VACUUM INTO ?`, path); err != nil {
		t.Fatal(err)
	}
	var obs int
	db.conn.QueryRow(`SELECT COUNT(*) FROM observations`).Scan(&obs)
	t.Logf("generated %s: %d nodes, %d observations in %v", path, len(truth), obs, time.Since(start).Round(time.Second))
}

func rp488Median(f func()) time.Duration {
	const runs = 7
	d := make([]time.Duration, runs)
	for i := range d {
		start := time.Now()
		f()
		d[i] = time.Since(start)
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[runs/2]
}

func rp488Heap() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// TestPerf_RegionPlacement488 times, on one database, medians of 7:
//   - current: cold full build per region (what each region set pays once),
//     warm hit, delta refresh over the last N observations;
//   - candidate: cold global aggregate + placements for every region,
//     placement-only recompute, delta scan + recompute, warm hit.
func TestPerf_RegionPlacement488(t *testing.T) {
	path := os.Getenv("CORESCOPE_PERF_488_DB")
	if path == "" {
		t.Skip("set CORESCOPE_PERF_488_DB=<db path> (see TestPerf_GenerateRegionPlacementDB_488)")
	}
	rp488PerfReport(t, path, 5000)
}

// TestRP488_FixtureCost runs the same timing on the e2e fixture, always.
func TestRP488_FixtureCost(t *testing.T) {
	rp488PerfReport(t, rp488Copy(t, filepath.Join("..", "..", "test-fixtures", "e2e-fixture.db")), 100)
}

func rp488PerfReport(t *testing.T, path string, deltaRows int64) {
	t.Helper()
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	regions := rp488DBRegions(t, db)
	wm, err := db.maxObservationID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lo := max(wm-deltaRows, 0)

	clear := func() {
		db.nodeRegionCacheMu.Lock()
		db.nodeRegionCache, db.nodeRegionUsed = nil, nil
		db.nodeRegionCacheMu.Unlock()
	}
	curCold := rp488Median(func() {
		clear()
		for _, r := range regions {
			if _, err := db.nodeRegionKeysJSON([]string{r}); err != nil {
				t.Fatal(err)
			}
		}
	})
	curWarm := rp488Median(func() {
		for _, r := range regions {
			if _, err := db.nodeRegionKeysJSON([]string{r}); err != nil {
				t.Fatal(err)
			}
		}
	})
	// Two delta windows: the newest deltaRows observations (whatever the
	// traffic mix is), and deltaRows ending at the newest ADVERT observation,
	// so every scanned row is classified.
	var advHi int64
	db.conn.QueryRow(`SELECT COALESCE(MAX(o.id), 0) FROM observations o
		JOIN transmissions t ON t.id = o.transmission_id WHERE t.payload_type = ?`, PayloadADVERT).Scan(&advHi)
	windows := [][2]int64{{lo, wm}, {max(advHi-deltaRows, 0), advHi}}
	// Same id window for both sides: the current refresh always scans up to
	// the newest id, so its delta is timed at the scan it runs
	// (scanNodeRegionKeys, CROSS JOIN), per region; the end-to-end refresh
	// is timed separately on the tail window.
	curDelta := make([]time.Duration, len(windows))
	for i, w := range windows {
		curDelta[i] = rp488Median(func() {
			for _, r := range regions {
				set := map[string]struct{}{}
				if _, err := db.scanNodeRegionKeys(ctx, []string{r}, w[0], w[1], true, set); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	curRefresh := rp488Median(func() {
		for _, r := range regions {
			key := nodeRegionKey([]string{r})
			e := *db.getNodeRegionEntry(key)
			e.watermark, e.refreshed = lo, time.Now().Add(-2*nodeRegionFreshTTL)
			db.setNodeRegionEntry(key, &e)
			if _, err := db.refreshNodeRegion(key, []string{r}); err != nil {
				t.Fatal(err)
			}
		}
	})
	clear()
	h0 := rp488Heap()
	for _, r := range regions {
		db.nodeRegionKeysJSON([]string{r})
	}
	curHeap := int64(rp488Heap()) - int64(h0)

	var agg *rp488Agg
	keys := map[string][]string{}
	derive := func(a *rp488Agg) {
		pl := rp488Placements(a, rp488DefaultParams)
		for _, r := range regions {
			keys[r] = rp488Members(pl, []string{r})
		}
	}
	candCold := rp488Median(func() {
		agg = newRP488Agg()
		if _, err := rp488ScanAgg(ctx, db, 0, wm, false, agg); err != nil {
			t.Fatal(err)
		}
		derive(agg)
	})
	candPlace := rp488Median(func() { derive(agg) })
	candDelta := make([]time.Duration, len(windows))
	for i, w := range windows {
		candDelta[i] = rp488Median(func() {
			if _, err := rp488ScanAgg(ctx, db, w[0], w[1], true, agg); err != nil {
				t.Fatal(err)
			}
			derive(agg)
		})
	}
	candWarm := rp488Median(func() {
		for _, r := range regions {
			_ = keys[r]
		}
	})
	agg = nil
	h1 := rp488Heap()
	agg = newRP488Agg()
	rp488ScanAgg(ctx, db, 0, wm, false, agg)
	candHeap := int64(rp488Heap()) - int64(h1)

	t.Logf("%s: %d observations, %d regions, %d nodes, %d (node, region) pairs", filepath.Base(path), wm, len(regions), len(agg.Nodes), agg.Pairs)
	t.Logf("current   cold (all regions) %v | warm %v | delta scan tail %v, advert-dense %v (%d obs) | tail refresh %v | heap %d KiB",
		curCold.Round(time.Microsecond), curWarm, curDelta[0].Round(time.Microsecond), curDelta[1].Round(time.Microsecond),
		deltaRows, curRefresh.Round(time.Microsecond), curHeap/1024)
	t.Logf("candidate cold (all regions) %v | placement %v | delta+placement tail %v, advert-dense %v | warm %v | heap %d KiB",
		candCold.Round(time.Microsecond), candPlace.Round(time.Microsecond), candDelta[0].Round(time.Microsecond),
		candDelta[1].Round(time.Microsecond), candWarm, candHeap/1024)
}
