package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

// #881 M1: detector unit tests. A fake clock, fake node rows, a hand-built
// NeighborGraph and a hand-built relay snapshot; no SQL here (see
// moments_sql_test.go).

var momentsT0 = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func momentPK(i int) string { return fmt.Sprintf("%064x", i) }

type momentsHarness struct {
	d     *momentsDetector
	now   time.Time
	rows  []momentNodeRow
	reads []bool // full flag per read
	ready bool
	sent  []Moment
}

func enabledMomentsConfig(m MomentsConfig) *Config {
	m.Enabled = true
	return &Config{Moments: &m}
}

func newMomentsHarness(t testing.TB, cfg *Config) *momentsHarness {
	t.Helper()
	if cfg == nil {
		cfg = enabledMomentsConfig(MomentsConfig{})
	}
	h := &momentsHarness{now: momentsT0, ready: true}
	h.d = newMomentsDetector(cfg.momentsSettings(), momentsDeps{
		now: func() time.Time { return h.now },
		cfg: cfg,
		readNodes: func(_ context.Context, _ time.Time, full bool) ([]momentNodeRow, error) {
			h.reads = append(h.reads, full)
			return append([]momentNodeRow(nil), h.rows...), nil
		},
		storeReady: func() bool { return h.ready },
		maxEdgeKm:  500,
		broadcast:  func(m Moment) { h.sent = append(h.sent, m) },
	})
	return h
}

func (h *momentsHarness) tick(rows ...momentNodeRow) []Moment {
	h.rows = rows
	n := len(h.sent)
	h.d.tickNodes(context.Background())
	return h.sent[n:]
}

func (h *momentsHarness) graph(edges ...*NeighborEdge) []Moment {
	g := NewNeighborGraph()
	for _, e := range edges {
		g.edges[makeEdgeKey(e.NodeA, e.NodeB)] = e
	}
	n := len(h.sent)
	h.d.onGraph(g)
	return h.sent[n:]
}

func (h *momentsHarness) relay(counts map[string]int) []Moment {
	info := make(map[string]RepeaterRelayInfo, len(counts))
	for k, c := range counts {
		info[k] = RepeaterRelayInfo{relayCountRecent: c}
	}
	n := len(h.sent)
	h.d.onRelay(&repeaterRelaySnapshot{info: info})
	return h.sent[n:]
}

const rfc = time.RFC3339

func nodeRow(pk, role string, lastSeen, firstSeen time.Time) momentNodeRow {
	return momentNodeRow{PublicKey: pk, Name: "N-" + pk[60:], Role: role, Lat: 55.6, Lon: 12.5, HasPos: true,
		LastSeen: lastSeen.Format(rfc), FirstSeen: firstSeen.Format(rfc)}
}

func kinds(ms []Moment) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Kind
		if m.Node != nil {
			out[i] += ":" + m.Node.PublicKey[60:]
		}
		if m.More > 0 {
			out[i] += fmt.Sprintf("+%d", m.More)
		}
	}
	return out
}

func wantKinds(t *testing.T, got []Moment, want ...string) {
	t.Helper()
	if g := strings.Join(kinds(got), ","); g != strings.Join(want, ",") {
		t.Fatalf("moments = [%s], want [%s]", g, strings.Join(want, ","))
	}
}

// seeded returns a harness whose silent seed has run over rows.
func seeded(t *testing.T, cfg *Config, rows ...momentNodeRow) *momentsHarness {
	t.Helper()
	h := newMomentsHarness(t, cfg)
	wantKinds(t, h.tick(rows...))
	return h
}

// ── new_node ──────────────────────────────────────────────────────────────

func TestMomentsNewNodeFiresOnce_881(t *testing.T) {
	old := momentsT0.Add(-48 * time.Hour)
	h := seeded(t, nil, nodeRow(momentPK(1), "repeater", momentsT0, old))
	h.now = momentsT0.Add(15 * time.Second)
	b := nodeRow(momentPK(2), "repeater", h.now, h.now.Add(-time.Minute))
	got := h.tick(b)
	wantKinds(t, got, "new_node:0002")
	if n := got[0].Node; n.Name != "N-0002" || n.Role != "repeater" || n.Lat == nil || *n.Lat != 55.6 {
		t.Fatalf("node payload %+v", n)
	}
	h.now = h.now.Add(15 * time.Second)
	wantKinds(t, h.tick(b))
	b.LastSeen = h.now.Format(rfc)
	wantKinds(t, h.tick(b))
	// Even after the node map lost it (eviction), the key is not new twice.
	delete(h.d.nodes, momentPK(2))
	wantKinds(t, h.tick(b))
}

func TestMomentsNoPositionAtZeroZero_881(t *testing.T) {
	h := seeded(t, nil)
	h.now = momentsT0.Add(15 * time.Second)
	r := nodeRow(momentPK(2), "room", h.now, h.now)
	r.Lat, r.Lon = 0, 0
	got := h.tick(r)
	wantKinds(t, got, "new_node:0002")
	if got[0].Node.Lat != nil || got[0].Node.Lon != nil {
		t.Fatalf("(0,0) published as a position: %v,%v", *got[0].Node.Lat, *got[0].Node.Lon)
	}
}

func TestMomentsNewNodeRoles_881(t *testing.T) {
	for role, want := range map[string]bool{"repeater": true, "room": true, "sensor": true, "Room": true, "companion": false, "": false, "chat": false} {
		h := seeded(t, nil)
		h.now = momentsT0.Add(15 * time.Second)
		got := h.tick(nodeRow(momentPK(2), role, h.now, h.now))
		if (len(got) == 1) != want {
			t.Errorf("role %q: moments %v, want fire=%v", role, kinds(got), want)
		}
	}
}

func TestMomentsPrivacyFilters_881(t *testing.T) {
	pk := momentPK(2)
	cases := map[string]func(*Config){
		"node blacklist":     func(c *Config) { c.SetNodeBlacklist([]string{strings.ToUpper(pk)}) },
		"observer blacklist": func(c *Config) { c.ObserverBlacklist = []string{pk} },
		"hidden prefix":      func(c *Config) { c.SetHiddenNamePrefixes([]string{"N-00"}) },
		"geofilter":          nil,
		"control":            func(*Config) {},
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := enabledMomentsConfig(MomentsConfig{})
			if mut != nil {
				mut(cfg)
			}
			h := newMomentsHarness(t, cfg)
			if name == "geofilter" {
				h.d.geo = func() *GeoFilterConfig {
					return &GeoFilterConfig{LatMin: f64(50), LatMax: f64(51), LonMin: f64(0), LonMax: f64(1)}
				}
			}
			h.tick()
			h.now = momentsT0.Add(15 * time.Second)
			got := h.tick(nodeRow(pk, "repeater", h.now, h.now))
			if want := name == "control"; (len(got) == 1) != want {
				t.Fatalf("moments %v, want fire=%v", kinds(got), want)
			}
		})
	}
	// A foreign-tagged node passes the geofilter, as on /api/nodes.
	h := newMomentsHarness(t, nil)
	h.d.geo = func() *GeoFilterConfig {
		return &GeoFilterConfig{LatMin: f64(50), LatMax: f64(51), LonMin: f64(0), LonMax: f64(1)}
	}
	h.tick()
	h.now = momentsT0.Add(15 * time.Second)
	r := nodeRow(pk, "repeater", h.now, h.now)
	r.Foreign = true
	wantKinds(t, h.tick(r), "new_node:0002")
}

func TestMomentsRetiredKeyIsBackOnlineNotNew_881(t *testing.T) {
	h := seeded(t, nil)
	h.now = momentsT0.Add(15 * time.Second)
	r := nodeRow(momentPK(2), "repeater", h.now, h.now)
	r.Retired, r.RetiredLastSeen = true, h.now.Add(-30*time.Hour).Format(rfc)
	got := h.tick(r)
	wantKinds(t, got, "back_online:0002")
	if got[0].SilentHours == nil || *got[0].SilentHours != 30 {
		t.Fatalf("silentHours %v", got[0].SilentHours)
	}
	// A retired room is neither new nor back online.
	h = seeded(t, nil)
	h.now = momentsT0.Add(15 * time.Second)
	r = nodeRow(momentPK(3), "room", h.now, h.now)
	r.Retired, r.RetiredLastSeen = true, h.now.Add(-30*time.Hour).Format(rfc)
	wantKinds(t, h.tick(r))
}

func TestMomentsSeedWaitsForStoreAndIsSilent_881(t *testing.T) {
	h := newMomentsHarness(t, nil)
	h.ready = false
	fresh := nodeRow(momentPK(2), "repeater", momentsT0, momentsT0)
	wantKinds(t, h.tick(fresh))
	if len(h.reads) != 0 {
		t.Fatalf("read %d times before the store loaded", len(h.reads))
	}
	h.ready = true
	wantKinds(t, h.tick(fresh)) // the seed: silent even for a fresh node
	h.now = momentsT0.Add(15 * time.Second)
	wantKinds(t, h.tick(fresh))
	if len(h.reads) != 2 || !h.reads[0] || h.reads[1] {
		t.Fatalf("reads (full?) = %v, want [true false]", h.reads)
	}
	// The record and busiest detectors are silent before the node seed and
	// on their own first look.
	h2 := newMomentsHarness(t, nil)
	wantKinds(t, h2.graph(linkEdge(momentPK(1), momentPK(2), 3, momentsT0)))
	wantKinds(t, h2.relay(map[string]int{momentPK(1): 999}))
	if h2.d.recSeeded || h2.d.busySeeded {
		t.Fatal("record/busiest seeded before the nodes")
	}
}

func TestMomentsNewNodeFreshness_881(t *testing.T) {
	for _, c := range []struct {
		age  time.Duration
		fire bool
	}{{9 * time.Minute, true}, {10 * time.Minute, true}, {10*time.Minute + time.Second, false}, {2 * time.Hour, false}} {
		h := seeded(t, nil)
		h.now = momentsT0.Add(15 * time.Second)
		got := h.tick(nodeRow(momentPK(2), "repeater", h.now, h.now.Add(-c.age)))
		if (len(got) == 1) != c.fire {
			t.Errorf("first_seen %v old: %v, want fire=%v", c.age, kinds(got), c.fire)
		}
	}
	// A stale last_seen is not an event either.
	h := seeded(t, nil)
	h.now = momentsT0.Add(15 * time.Second)
	wantKinds(t, h.tick(nodeRow(momentPK(2), "repeater", h.now.Add(-11*time.Minute), h.now)))
}

func TestMomentsNewNodeCoalesces_881(t *testing.T) {
	h := seeded(t, nil)
	h.now = momentsT0.Add(15 * time.Second)
	var rows []momentNodeRow
	for i := 5; i >= 1; i-- { // node 5 is the newest first_seen
		rows = append(rows, nodeRow(momentPK(i), "repeater", h.now, h.now.Add(-time.Duration(6-i)*time.Second)))
	}
	got := h.tick(rows...)
	wantKinds(t, got, "new_node:0001+4")
	// The folded nodes are on cooldown too: none of them fires later.
	h.now = h.now.Add(2 * time.Minute)
	wantKinds(t, h.tick(rows...))

	// Within the cap, one moment each.
	h = seeded(t, nil)
	h.now = momentsT0.Add(15 * time.Second)
	wantKinds(t, h.tick(rows[2:]...), "new_node:0001", "new_node:0002", "new_node:0003")
	// The cap is per minute: a fourth node in the same minute is folded.
	h.now = h.now.Add(15 * time.Second)
	wantKinds(t, h.tick(rows[1]))
	h.now = h.now.Add(time.Minute)
	wantKinds(t, h.tick(rows[0]), "new_node:0005")
}

// ── back_online ───────────────────────────────────────────────────────────

func TestMomentsBackOnlineThreshold_881(t *testing.T) {
	pk := momentPK(7)
	for _, c := range []struct {
		gap  time.Duration
		fire bool
	}{{24*time.Hour + time.Second, true}, {24 * time.Hour, true}, {23*time.Hour + 59*time.Minute, false}} {
		h := seeded(t, nil, nodeRow(pk, "repeater", momentsT0, momentsT0.Add(-90*24*time.Hour)))
		h.now = momentsT0.Add(c.gap)
		got := h.tick(nodeRow(pk, "repeater", h.now, momentsT0.Add(-90*24*time.Hour)))
		if (len(got) == 1) != c.fire {
			t.Errorf("gap %v: %v, want fire=%v", c.gap, kinds(got), c.fire)
		}
	}
}

func TestMomentsBackOnlineRepeaterOnly_881(t *testing.T) {
	for _, role := range []string{"room", "sensor", "companion"} {
		pk := momentPK(7)
		h := seeded(t, nil, nodeRow(pk, role, momentsT0, momentsT0.Add(-90*24*time.Hour)))
		h.now = momentsT0.Add(48 * time.Hour)
		wantKinds(t, h.tick(nodeRow(pk, role, h.now, momentsT0.Add(-90*24*time.Hour))))
	}
}

func TestMomentsBackOnlineCooldown_881(t *testing.T) {
	pk := momentPK(7)
	first := momentsT0.Add(-90 * 24 * time.Hour)
	h := seeded(t, enabledMomentsConfig(MomentsConfig{BackOnlineHours: 1}), nodeRow(pk, "repeater", momentsT0, first))
	h.now = momentsT0.Add(2 * time.Hour)
	wantKinds(t, h.tick(nodeRow(pk, "repeater", h.now, first)), "back_online:0007")
	h.now = h.now.Add(2 * time.Hour) // silent 2 h again, inside the 24 h cooldown
	wantKinds(t, h.tick(nodeRow(pk, "repeater", h.now, first)))
	h.now = h.now.Add(22 * time.Hour) // 24 h after the moment, silent 22 h
	wantKinds(t, h.tick(nodeRow(pk, "repeater", h.now, first)), "back_online:0007")
}

func TestMomentsTimestampFormats_881(t *testing.T) {
	const sqliteFmt = "2006-01-02 15:04:05"
	pk := momentPK(7)
	first := momentsT0.Add(-90 * 24 * time.Hour)
	for _, c := range []struct{ prev, cur string }{{sqliteFmt, rfc}, {rfc, sqliteFmt}, {sqliteFmt, sqliteFmt}, {rfc, "2006-01-02T15:04:05.000Z"}} {
		seed := nodeRow(pk, "repeater", momentsT0, first)
		seed.LastSeen = momentsT0.Format(c.prev)
		h := seeded(t, nil, seed)
		h.now = momentsT0.Add(25 * time.Hour)
		cur := nodeRow(pk, "repeater", h.now, first)
		cur.LastSeen = h.now.Format(c.cur)
		got := h.tick(cur)
		wantKinds(t, got, "back_online:0007")
		if *got[0].SilentHours != 25 {
			t.Fatalf("%s → %s: silentHours %v", c.prev, c.cur, *got[0].SilentHours)
		}
	}
	// first_seen in SQLite format.
	h := seeded(t, nil)
	h.now = momentsT0.Add(15 * time.Second)
	r := nodeRow(momentPK(2), "room", h.now, h.now)
	r.FirstSeen, r.LastSeen = h.now.Format(sqliteFmt), h.now.Format(sqliteFmt)
	wantKinds(t, h.tick(r), "new_node:0002")
}

// ── record_link ───────────────────────────────────────────────────────────

// linkNodes places nodes at lat 55, lon 12 + i·step so distances are exact
// haversine values.
func linkRow(pk, role string, lat, lon float64) momentNodeRow {
	r := nodeRow(pk, role, momentsT0, momentsT0.Add(-90*24*time.Hour))
	r.Lat, r.Lon = lat, lon
	return r
}

func linkEdge(a, b string, count int, last time.Time) *NeighborEdge {
	return &NeighborEdge{NodeA: a, NodeB: b, Count: count, LastSeen: last}
}

// latForKm returns the latitude km north of 55°N on the same meridian.
func latForKm(km float64) float64 { return 55 + km/111.19492664455873 }

func recordHarness(t *testing.T, cfg *Config, extra ...momentNodeRow) *momentsHarness {
	rows := []momentNodeRow{
		linkRow(momentPK(1), "repeater", 55, 12), linkRow(momentPK(2), "repeater", latForKm(10), 12),
		linkRow(momentPK(3), "repeater", 55, 13), linkRow(momentPK(4), "repeater", latForKm(10.4), 13),
		linkRow(momentPK(5), "repeater", 55, 14), linkRow(momentPK(6), "room", latForKm(10.6), 14),
		linkRow(momentPK(7), "repeater", 55, 15), linkRow(momentPK(8), "repeater", latForKm(10), 15),
	}
	return seeded(t, cfg, append(rows, extra...)...)
}

func TestMomentsRecordSeedEpsilonAndTies_881(t *testing.T) {
	h := recordHarness(t, nil)
	ab := linkEdge(momentPK(1), momentPK(2), 3, momentsT0)
	wantKinds(t, h.graph(ab)) // silent seed: record 10 km
	if !h.d.record.same(momentPK(1), momentPK(2)) {
		t.Fatalf("seed record %+v", h.d.record)
	}
	tie := linkEdge(momentPK(7), momentPK(8), 3, momentsT0)
	wantKinds(t, h.graph(ab, tie)) // equal distance
	cd := linkEdge(momentPK(3), momentPK(4), 3, momentsT0)
	wantKinds(t, h.graph(ab, cd)) // +0.4 km < ε 0.5
	ef := linkEdge(momentPK(5), momentPK(6), 3, momentsT0)
	got := h.graph(ab, cd, ef) // +0.6 km
	wantKinds(t, got, "record_link")
	m := got[0]
	if m.Link == nil || m.Link.DistanceKm != 10.6 || m.PreviousKm == nil || *m.PreviousKm != 10 {
		t.Fatalf("record payload %+v prev %v", m.Link, m.PreviousKm)
	}
	if m.Link.A.PublicKey != momentPK(5) || m.Link.B.Role != "room" || m.Link.B.Lat == nil {
		t.Fatalf("link endpoints %+v", m.Link)
	}
	wantKinds(t, h.graph(ab, cd, ef)) // same record again: silent
}

func TestMomentsRecordExclusions_881(t *testing.T) {
	far := func(pk string) momentNodeRow { return linkRow(pk, "repeater", latForKm(100), 12) }
	base := linkEdge(momentPK(1), momentPK(2), 3, momentsT0)
	cases := []struct {
		name string
		row  momentNodeRow
		edge func(*NeighborEdge)
		cfg  func(*Config)
		// noMax disables MaxEdgeKm, so only the case's own rule can exclude.
		noMax bool
		fire  bool
	}{
		{name: "control", row: far(momentPK(20)), fire: true},
		{name: "count 1", row: far(momentPK(20)), edge: func(e *NeighborEdge) { e.Count = 1 }},
		{name: "ambiguous", row: far(momentPK(20)), edge: func(e *NeighborEdge) { e.Ambiguous = true }},
		{name: "unresolved", row: far(momentPK(20)), edge: func(e *NeighborEdge) { e.NodeB = "" }},
		{name: "outside window", row: far(momentPK(20)), edge: func(e *NeighborEdge) { e.LastSeen = momentsT0.Add(-25 * time.Hour) }},
		{name: "companion", row: linkRow(momentPK(20), "companion", latForKm(100), 12)},
		{name: "unknown role", row: linkRow(momentPK(20), "", latForKm(100), 12)},
		{name: "no GPS", row: func() momentNodeRow { r := far(momentPK(20)); r.HasPos = false; return r }()},
		{name: "0,0", row: linkRow(momentPK(20), "repeater", 0, 0), noMax: true},
		{name: "control without MaxEdgeKm", row: linkRow(momentPK(20), "repeater", 1, 1), noMax: true, fire: true},
		{name: "over MaxEdgeKm", row: linkRow(momentPK(20), "repeater", latForKm(600), 12)},
		{name: "blacklisted", row: far(momentPK(20)), cfg: func(c *Config) { c.SetNodeBlacklist([]string{momentPK(20)}) }},
		{name: "hidden name", row: far(momentPK(20)), cfg: func(c *Config) { c.SetHiddenNamePrefixes([]string{"N-0014"}) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := enabledMomentsConfig(MomentsConfig{})
			if c.cfg != nil {
				c.cfg(cfg)
			}
			h := recordHarness(t, cfg, c.row)
			if c.noMax {
				h.d.maxEdgeKm = 0
			}
			h.graph(base)
			e := linkEdge(momentPK(1), momentPK(20), 3, momentsT0)
			if c.edge != nil {
				c.edge(e)
			}
			got := h.graph(base, e)
			if (len(got) == 1) != c.fire {
				t.Fatalf("moments %v, want fire=%v", kinds(got), c.fire)
			}
		})
	}
	// Below recordMinKm is never a record, even the first.
	h := seeded(t, enabledMomentsConfig(MomentsConfig{RecordMinKm: 20}), linkRow(momentPK(1), "repeater", 55, 12), linkRow(momentPK(2), "repeater", latForKm(10), 12))
	h.graph()
	wantKinds(t, h.graph(linkEdge(momentPK(1), momentPK(2), 3, momentsT0)))
}

func TestMomentsRecordExpiryIsSilent_881(t *testing.T) {
	h := recordHarness(t, nil,
		linkRow(momentPK(20), "repeater", latForKm(100), 12),
		linkRow(momentPK(21), "repeater", latForKm(50), 12),
		linkRow(momentPK(22), "repeater", latForKm(60), 12))
	long := linkEdge(momentPK(1), momentPK(20), 3, momentsT0)
	mid := linkEdge(momentPK(1), momentPK(21), 3, momentsT0)
	h.graph(long, mid) // seed: record 100 km
	h.now = momentsT0.Add(24*time.Hour + time.Minute)
	mid.LastSeen = h.now
	wantKinds(t, h.graph(long, mid)) // 100 km left the window: silent drop to 50 km
	if !h.d.record.same(momentPK(1), momentPK(21)) {
		t.Fatalf("record after expiry %+v", h.d.record)
	}
	wantKinds(t, h.graph(mid))
	wantKinds(t, h.graph()) // nothing qualifies: silent
	wantKinds(t, h.graph(mid), "record_link")
	got := h.graph(mid, linkEdge(momentPK(1), momentPK(22), 3, h.now))
	if len(got) != 0 {
		t.Fatalf("kind cap: %v", kinds(got)) // 1 per 10 min
	}
	h.now = h.now.Add(10 * time.Minute)
	got = h.graph(mid, linkEdge(momentPK(1), momentPK(22), 3, h.now))
	wantKinds(t, got, "record_link")
	if *got[0].PreviousKm != 50 {
		t.Fatalf("previousKm %v: the capped beat must not move the record", *got[0].PreviousKm)
	}
}

// ── busiest_repeater ──────────────────────────────────────────────────────

func busyHarness(t *testing.T) *momentsHarness {
	return seeded(t, nil,
		nodeRow(momentPK(1), "repeater", momentsT0, momentsT0.Add(-time.Hour*2000)),
		nodeRow(momentPK(2), "repeater", momentsT0, momentsT0.Add(-time.Hour*2000)),
		nodeRow(momentPK(3), "room", momentsT0, momentsT0.Add(-time.Hour*2000)))
}

func TestMomentsBusiestFloorAndRole_881(t *testing.T) {
	h := busyHarness(t)
	wantKinds(t, h.relay(map[string]int{momentPK(1): 30})) // seed
	h.now = momentsT0.Add(2 * time.Hour)
	wantKinds(t, h.relay(map[string]int{momentPK(2): 19, momentPK(3): 500}))
	got := h.relay(map[string]int{momentPK(2): 20, momentPK(3): 500, "ab": 900})
	wantKinds(t, got, "busiest_repeater:0002")
	if *got[0].Relays != 20 || *got[0].WindowMinutes != 10 {
		t.Fatalf("payload relays=%d window=%d", *got[0].Relays, *got[0].WindowMinutes)
	}
	// Loading snapshots are ignored.
	h.now = h.now.Add(2 * time.Hour)
	h.d.onRelay(&repeaterRelaySnapshot{loading: true, info: map[string]RepeaterRelayInfo{momentPK(1): {relayCountRecent: 99}}})
	wantKinds(t, h.sent[1:])
}

func TestMomentsBusiestLeaderCooldownAndKindCap_881(t *testing.T) {
	h := busyHarness(t)
	wantKinds(t, h.relay(map[string]int{momentPK(1): 30})) // seed leader 1
	h.now = momentsT0.Add(5 * time.Minute)
	wantKinds(t, h.relay(map[string]int{momentPK(1): 40})) // same leader < 60 min
	got := h.relay(map[string]int{momentPK(1): 40, momentPK(2): 50})
	wantKinds(t, got, "busiest_repeater:0002") // a new leader
	h.now = h.now.Add(5 * time.Minute)
	wantKinds(t, h.relay(map[string]int{momentPK(1): 90})) // leader change inside the 15 min kind cap
	h.now = h.now.Add(10 * time.Minute)
	wantKinds(t, h.relay(map[string]int{momentPK(2): 90})) // 2 still on its 60 min cooldown
	wantKinds(t, h.relay(map[string]int{momentPK(1): 90})) // 1 still on the seed cooldown (60 min)
	h.now = momentsT0.Add(61 * time.Minute)
	wantKinds(t, h.relay(map[string]int{momentPK(1): 90}), "busiest_repeater:0001")
}

// ── limiter ───────────────────────────────────────────────────────────────

func TestMomentsLimiter_881(t *testing.T) {
	h := newMomentsHarness(t, enabledMomentsConfig(MomentsConfig{MaxPerMinute: 6}))
	d, now := h.d, momentsT0
	// Global burst 3.
	for i := 0; i < 3; i++ {
		if !d.allow(MomentKindNewNode, now) {
			t.Fatalf("burst %d refused", i)
		}
	}
	if d.allow(MomentKindBackOnline, now) {
		t.Fatal("4th moment passed the global burst")
	}
	if !d.allow(MomentKindBackOnline, now.Add(10*time.Second)) { // 6/min = 1 per 10 s
		t.Fatal("token not refilled after 10 s")
	}
	if d.allow(MomentKindBackOnline, now.Add(15*time.Second)) {
		t.Fatal("refill faster than maxPerMinute")
	}
	// Per-kind caps hold with tokens to spare.
	now = now.Add(time.Hour)
	if !d.allow(MomentKindRecordLink, now) || d.allow(MomentKindRecordLink, now.Add(9*time.Minute)) || !d.allow(MomentKindRecordLink, now.Add(10*time.Minute)) {
		t.Fatal("record_link cap is not 1 per 10 min")
	}
	if !d.allow(MomentKindBusiest, now) || d.allow(MomentKindBusiest, now.Add(14*time.Minute)) || !d.allow(MomentKindBusiest, now.Add(15*time.Minute)) {
		t.Fatal("busiest cap is not 1 per 15 min")
	}
	now = now.Add(time.Hour)
	if !d.allow(MomentKindBackOnline, now) || !d.allow(MomentKindBackOnline, now) || d.allow(MomentKindBackOnline, now) {
		t.Fatal("back_online cap is not 2 per min")
	}
	if !d.allow(MomentKindNewNode, now.Add(time.Minute)) {
		t.Fatal("new_node refused after its minute")
	}

	// Overflow is dropped and logged once per throttle interval.
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	d.noteDropped(2, now)
	d.noteDropped(5, now.Add(time.Minute))
	if c := strings.Count(buf.String(), "[moments] rate limit"); c != 1 || !strings.Contains(buf.String(), "dropped 2 ") {
		t.Fatalf("drop log %d lines: %q", c, buf.String())
	}
	d.noteDropped(1, now.Add(momentsDropLogEvery))
	if !strings.Contains(buf.String(), "dropped 6 ") {
		t.Fatalf("second drop line must report the growth: %q", buf.String())
	}
}

// The global bucket drops moments the per-kind cap would allow.
func TestMomentsGlobalBucketDropsOverflow_881(t *testing.T) {
	h := seeded(t, enabledMomentsConfig(MomentsConfig{MaxPerMinute: 1}),
		nodeRow(momentPK(7), "repeater", momentsT0, momentsT0.Add(-2000*time.Hour)),
		nodeRow(momentPK(8), "repeater", momentsT0, momentsT0.Add(-2000*time.Hour)))
	h.now = momentsT0.Add(25 * time.Hour)
	rows := []momentNodeRow{
		nodeRow(momentPK(1), "repeater", h.now, h.now), nodeRow(momentPK(2), "room", h.now, h.now),
		nodeRow(momentPK(7), "repeater", h.now, momentsT0), nodeRow(momentPK(8), "repeater", h.now, momentsT0),
	}
	got := h.tick(rows...)
	if len(got) != 3 {
		t.Fatalf("burst 3: got %v", kinds(got))
	}
}

// ── bounded state ─────────────────────────────────────────────────────────

func TestMomentsStateBounded_881(t *testing.T) {
	h := seeded(t, nil, nodeRow(momentPK(1), "repeater", momentsT0, momentsT0))
	old := momentsT0.Add(-1000 * time.Hour)
	const keys = 100000
	rows := make([]momentNodeRow, 0, 1000)
	for i := 0; i < keys; i++ {
		rows = append(rows, nodeRow(momentPK(1000+i), "repeater", old.Add(time.Duration(i)*time.Second), old))
		if len(rows) == cap(rows) {
			h.now = h.now.Add(15 * time.Second)
			wantKinds(t, h.tick(rows...))
			rows = rows[:0]
			if len(h.d.nodes) > h.d.nodeCap {
				t.Fatalf("node map %d > cap %d", len(h.d.nodes), h.d.nodeCap)
			}
		}
	}
	if h.d.nodeCap != momentsNodeMapMin || len(h.d.nodes) > momentsNodeMapMin {
		t.Fatalf("node map %d, cap %d", len(h.d.nodes), h.d.nodeCap)
	}
	// Eviction drops the longest-silent nodes, not recent ones.
	if _, ok := h.d.nodes[momentPK(1000+keys-1)]; !ok {
		t.Fatal("the newest node was evicted")
	}
	if _, ok := h.d.nodes[momentPK(1000)]; ok {
		t.Fatal("the oldest node survived eviction")
	}
	if _, ok := h.d.nodes[momentPK(1)]; !ok {
		t.Fatal("the recently seen seed node was evicted")
	}

	for i := 0; i < keys; i++ {
		h.d.setCooldown(MomentKindNewNode, momentPK(i), momentsT0.Add(time.Duration(i)*time.Second))
		if len(h.d.cooldown) > momentsCooldownCap {
			t.Fatalf("cooldown map %d > %d", len(h.d.cooldown), momentsCooldownCap)
		}
	}
	if !h.d.onCooldown(MomentKindNewNode, momentPK(keys-1), momentsT0) || h.d.onCooldown(MomentKindNewNode, momentPK(0), momentsT0.Add(-time.Hour)) {
		t.Fatal("cooldown eviction kept the wrong entries")
	}

	for i := 0; i < 1000; i++ {
		h.d.publish([]Moment{{ID: fmt.Sprint(i)}})
	}
	if len(h.d.ring) != 50 || h.d.ring[49].ID != "999" || h.d.ring[0].ID != "950" {
		t.Fatalf("ring len %d first %q last %q", len(h.d.ring), h.d.ring[0].ID, h.d.ring[len(h.d.ring)-1].ID)
	}
	// The node map cap grows with a large seed.
	big := newMomentsHarness(t, nil)
	seed := make([]momentNodeRow, 15000)
	for i := range seed {
		seed[i] = nodeRow(momentPK(i), "repeater", momentsT0, old)
	}
	big.tick(seed...)
	if big.d.nodeCap != 30000 {
		t.Fatalf("cap after a 15000-node seed = %d, want 30000", big.d.nodeCap)
	}
}

// ── config ────────────────────────────────────────────────────────────────

func TestMomentsConfigDefaultsAndClamps_881(t *testing.T) {
	if (&Config{}).MomentsEnabled() || (*Config)(nil).MomentsEnabled() {
		t.Fatal("moments on without config")
	}
	s := (&Config{}).momentsSettings()
	want := momentsSettings{backOnline: 24 * time.Hour, recordWindow: 24 * time.Hour, recordMinKm: 5, busyWindow: 10 * time.Minute, busyMinRelays: 20, maxPerMinute: 6, historySize: 50}
	if s != want {
		t.Fatalf("defaults %+v", s)
	}
	cfg, logged := loadConfigLogged(t, `{"moments":{"enabled":true,"backOnlineHours":0.5,"recordWindowHours":1000,"recordMinKm":-1,"busyWindowMinutes":5000,"busyMinRelays":0,"maxPerMinute":100,"historySize":9999}}`)
	s = cfg.momentsSettings()
	want = momentsSettings{enabled: true, backOnline: time.Hour, recordWindow: 168 * time.Hour, recordMinKm: 0.1, busyWindow: 1440 * time.Minute, busyMinRelays: 20, maxPerMinute: 60, historySize: 500}
	if s != want {
		t.Fatalf("clamped %+v", s)
	}
	if n := strings.Count(logged, "[config] moments."); n != 6 {
		t.Fatalf("%d clamp lines, want 6:\n%s", n, logged)
	}
	if cl := cfg.momentsClientConfig(); cl == nil || !cl.Enabled || cl.HistorySize != 500 {
		t.Fatalf("client block %+v", cl)
	}
	if (&Config{Moments: &MomentsConfig{}}).momentsClientConfig() != nil {
		t.Fatal("client block present while off")
	}
}
