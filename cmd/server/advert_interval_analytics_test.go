package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// #245 M2: mesh-wide advert interval analytics. The firmware facts behind
// the expectations are the ones M1 cites (advert_intervals.go):
//   - flood.advert.interval: whole hours 3-168, 0 = off
//     (src/helpers/CommonCLI.cpp:486-495).
//   - advert.interval: 60-240 minutes stored as mins/2, 0 = off
//     (src/helpers/CommonCLI.cpp:496-505); 2 min on an untouched new
//     install (examples/simple_repeater/MyMesh.cpp:903).
//   - a flood advert re-arms the zero-hop timer
//     (examples/simple_repeater/MyMesh.cpp:1294-1306).

// aaEst is an estimated class at intervalS with the given confidence and
// snapped flag.
func aaEst(intervalS int64, conf string, snapped bool) AdvertIntervalEstimate {
	v, raw := intervalS, intervalS
	return AdvertIntervalEstimate{IntervalS: &v, RawIntervalS: &raw, Snapped: snapped, Samples: 10, GapsUsed: 8,
		Confidence: conf, Status: advertIntervalEstimated}
}

// aaNone is a class with no estimate for status.
func aaNone(status string) AdvertIntervalEstimate {
	return AdvertIntervalEstimate{Confidence: advertConfidenceNone, Status: status}
}

func aaRow(pk, role string, regions []string, flood, zh AdvertIntervalEstimate) advertIntervalNodeRow {
	return advertIntervalNodeRow{pubkey: pk, name: "Node " + pk, role: role, regions: regions, flood: flood, zeroHop: zh}
}

func aaAll(*advertIntervalNodeRow) bool { return true }

func aaBuckets(b []AdvertIntervalBucket) string {
	parts := make([]string, 0, len(b))
	for _, x := range b {
		s := fmt.Sprintf("%d:%d", x.IntervalS, x.Nodes)
		if !x.Snapped {
			s += "~"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

func aaGroup(t *testing.T, groups []AdvertIntervalGroup, key string) AdvertIntervalGroup {
	t.Helper()
	for _, g := range groups {
		if g.Key == key {
			return g
		}
	}
	t.Fatalf("no group %q in %+v", key, groups)
	return AdvertIntervalGroup{}
}

// Distribution by role: only medium/high estimates are bucketed, by their
// snapped firmware value; each status is counted once per class.
func TestAdvertIntervalAnalytics_ByRole(t *testing.T) {
	rows := []advertIntervalNodeRow{
		aaRow("r1", "repeater", nil, aaEst(47*3600, advertConfidenceHigh, true), aaEst(7200, advertConfidenceHigh, true)),
		aaRow("r2", "repeater", nil, aaEst(47*3600, advertConfidenceMedium, true), aaNone(advertIntervalNoneObserved)),
		aaRow("r3", "Repeater", nil, aaEst(12*3600, advertConfidenceHigh, true), aaEst(14400, advertConfidenceMedium, true)),
		aaRow("c1", "companion", nil, aaNone(advertIntervalNoneObserved), aaNone(advertIntervalNoneObserved)),
		aaRow("m1", "room", nil, aaEst(47*3600, advertConfidenceHigh, true), aaEst(7200, advertConfidenceHigh, true)),
		aaRow("u1", "", nil, aaNone(advertIntervalTooFew), aaNone(advertIntervalNoneObserved)),
	}
	got := aggregateAdvertIntervals(rows, aaAll)
	if got.Nodes != 6 || got.Window != nodeAdvertRouteLimit || got.MinConfidence != advertConfidenceMedium {
		t.Fatalf("nodes/window/min_confidence = %d/%d/%s", got.Nodes, got.Window, got.MinConfidence)
	}
	if b := aaBuckets(got.Flood.Buckets); b != "43200:1 169200:3" {
		t.Errorf("flood buckets = %s, want 12 h x1 and 47 h x3", b)
	}
	if b := aaBuckets(got.ZeroHop.Buckets); b != "7200:2 14400:1" {
		t.Errorf("zero-hop buckets = %s", b)
	}
	if got.Flood.Estimated != 4 || got.Flood.NotObserved != 1 || got.Flood.TooFew != 1 {
		t.Errorf("flood summary = %+v", got.Flood)
	}
	if got.ZeroHop.Estimated != 3 || got.ZeroHop.NotObserved != 3 {
		t.Errorf("zero-hop summary = %+v", got.ZeroHop)
	}
	keys := []string{}
	for _, g := range got.ByRole {
		keys = append(keys, fmt.Sprintf("%s:%d", g.Key, g.Nodes))
	}
	// Largest population first, then by name; roles are normalized.
	if k := strings.Join(keys, " "); k != "repeater:3 companion:1 room:1 unknown:1" {
		t.Fatalf("by_role = %s", k)
	}
	rep := aaGroup(t, got.ByRole, "repeater")
	if b := aaBuckets(rep.Flood.Buckets); b != "43200:1 169200:2" || rep.ZeroHop.NotObserved != 1 {
		t.Errorf("repeater group = %s, zero-hop %+v", b, rep.ZeroHop)
	}
	if c := aaGroup(t, got.ByRole, "companion"); c.ZeroHop.NotObserved != 1 || c.Flood.NotObserved != 1 || len(c.Flood.Buckets) != 0 {
		t.Errorf("companion group = %+v", c)
	}
}

// Distribution by region: a node heard in two regions counts in both, one
// heard in none under "unknown"; groups are capped.
func TestAdvertIntervalAnalytics_ByRegion(t *testing.T) {
	rows := []advertIntervalNodeRow{
		aaRow("a", "repeater", []string{"CPH", "ODE"}, aaEst(47*3600, advertConfidenceHigh, true), aaNone(advertIntervalNoneObserved)),
		aaRow("b", "repeater", []string{"CPH"}, aaEst(12*3600, advertConfidenceHigh, true), aaEst(7200, advertConfidenceHigh, true)),
		aaRow("c", "repeater", nil, aaEst(12*3600, advertConfidenceHigh, true), aaNone(advertIntervalNoneObserved)),
	}
	got := aggregateAdvertIntervals(rows, aaAll)
	keys := []string{}
	for _, g := range got.ByRegion {
		keys = append(keys, fmt.Sprintf("%s:%d", g.Key, g.Nodes))
	}
	if k := strings.Join(keys, " "); k != "CPH:2 ODE:1 unknown:1" {
		t.Fatalf("by_region = %s", k)
	}
	cph := aaGroup(t, got.ByRegion, "CPH")
	if b := aaBuckets(cph.Flood.Buckets); b != "43200:1 169200:1" || cph.ZeroHop.NotObserved != 1 || cph.ZeroHop.Estimated != 1 {
		t.Errorf("CPH = %s %+v", b, cph.ZeroHop)
	}
	if got.Nodes != 3 {
		t.Errorf("nodes = %d: a node in two regions is still one node", got.Nodes)
	}
	if got.RegionsTruncated {
		t.Error("3 groups must not be truncated")
	}

	var many []advertIntervalNodeRow
	for i := 0; i < advertIntervalRegionLimit+5; i++ {
		many = append(many, aaRow(fmt.Sprintf("n%03d", i), "repeater", []string{fmt.Sprintf("R%03d", i)}, aaNone(advertIntervalTooFew), aaNone(advertIntervalTooFew)))
	}
	got = aggregateAdvertIntervals(many, aaAll)
	if len(got.ByRegion) != advertIntervalRegionLimit || !got.RegionsTruncated {
		t.Fatalf("by_region = %d groups, truncated %v; want %d, true", len(got.ByRegion), got.RegionsTruncated, advertIntervalRegionLimit)
	}
}

// Insufficient data never reaches the distribution: too few adverts,
// irregular adverts and low confidence are counted beside it.
func TestAdvertIntervalAnalytics_InsufficientData(t *testing.T) {
	rows := []advertIntervalNodeRow{
		aaRow("a", "repeater", nil, aaNone(advertIntervalTooFew), aaNone(advertIntervalTooFew)),
		aaRow("b", "repeater", nil, aaNone(advertIntervalIrregular), aaNone(advertIntervalIrregular)),
		aaRow("c", "repeater", nil, aaEst(47*3600, advertConfidenceLow, true), aaEst(7200, advertConfidenceLow, true)),
		aaRow("d", "repeater", nil, aaNone(advertIntervalNoneObserved), aaNone(advertIntervalNoneObserved)),
	}
	got := aggregateAdvertIntervals(rows, aaAll)
	for name, c := range map[string]AdvertIntervalClassSummary{"flood": got.Flood, "zero_hop": got.ZeroHop} {
		if c.Estimated != 0 || c.LowConfidence != 1 || c.Irregular != 1 || c.TooFew != 1 || c.NotObserved != 1 || len(c.Buckets) != 0 {
			t.Errorf("%s = %+v", name, c)
		}
	}
	// The real estimator: two adverts are too few, three regular ones are
	// estimated (low confidence: only two fitting gaps).
	two := estimateAdvertInterval(aiSeries(12*time.Hour, 0, 1), advertIntervalFlood)
	three := estimateAdvertInterval(aiSeries(12*time.Hour, 0, 1, 2), advertIntervalFlood)
	got = aggregateAdvertIntervals([]advertIntervalNodeRow{
		aaRow("e", "repeater", nil, two, aaNone(advertIntervalNoneObserved)),
		aaRow("f", "repeater", nil, three, aaNone(advertIntervalNoneObserved)),
	}, aaAll)
	if got.Flood.TooFew != 1 || got.Flood.LowConfidence != 1 || got.Flood.Estimated != 0 {
		t.Fatalf("estimator states: %+v (two=%+v three=%+v)", got.Flood, two, three)
	}
}

// Values outside the settable range are not snapped; they are bucketed by
// the class's unit (hours / minutes) and flagged unsnapped.
func TestAdvertIntervalAnalytics_SnappedBuckets(t *testing.T) {
	// The estimator snaps 46.8 h to 47 h and 119 min to 120 min.
	flood := estimateAdvertInterval(aiSeries(time.Duration(46.8*float64(time.Hour)), aiInts(0, 9)...), advertIntervalFlood)
	zh := estimateAdvertInterval(aiSeries(119*time.Minute, aiInts(0, 9)...), advertIntervalZeroHop)
	// 300 h: outside 3-168 h, kept as estimated but unsnapped.
	far := estimateAdvertInterval(aiSeries(300*time.Hour, aiInts(0, 9)...), advertIntervalFlood)
	if far.Snapped || far.Status != advertIntervalEstimated {
		t.Fatalf("300 h must be estimated and unsnapped: %+v", far)
	}
	got := aggregateAdvertIntervals([]advertIntervalNodeRow{
		aaRow("a", "repeater", nil, flood, zh),
		aaRow("b", "repeater", nil, far, aaNone(advertIntervalNoneObserved)),
	}, aaAll)
	if b := aaBuckets(got.Flood.Buckets); b != "169200:1 1080000:1~" {
		t.Fatalf("flood buckets = %s", b)
	}
	if b := aaBuckets(got.ZeroHop.Buckets); b != "7200:1" {
		t.Fatalf("zero-hop buckets = %s", b)
	}
	if v := advertIntervalBucketValue(aaEst(4000, advertConfidenceHigh, false), advertIntervalZeroHop); v != 4020 {
		t.Errorf("unsnapped zero-hop rounds to the minute: %d", v)
	}
	if v := advertIntervalBucketValue(aaEst(1081000, advertConfidenceHigh, false), advertIntervalFlood); v != 1080000 {
		t.Errorf("unsnapped flood rounds to the hour: %d", v)
	}
}

// Outliers: adverts more frequent than the firmware's shortest setting, and
// the 2-minute new-install zero-hop default. Sorted most frequent first,
// capped, with the full count.
func TestAdvertIntervalAnalytics_Outliers(t *testing.T) {
	def := aaRow("def", "repeater", []string{"CPH"}, aaEst(47*3600, advertConfidenceHigh, true), aaEst(120, advertConfidenceHigh, true))
	def.zeroHopGapS = 120
	def.floodGapS = 47 * 3600
	fastFlood := aaRow("ff", "companion", nil, aaNone(advertIntervalIrregular), aaNone(advertIntervalNoneObserved))
	fastFlood.floodGapS = 600 // flood every 10 min: manual or scripted
	fastZH := aaRow("fz", "repeater", nil, aaNone(advertIntervalIrregular), aaNone(advertIntervalIrregular))
	fastZH.zeroHopGapS = 1200 // 20 min: below advert.interval's 60 min floor
	edge := aaRow("edge", "repeater", nil, aaEst(3*3600, advertConfidenceHigh, true), aaEst(3600, advertConfidenceHigh, true))
	edge.floodGapS, edge.zeroHopGapS = 3*3600*(1-advertIntervalTol), 3600*(1-advertIntervalTol)
	normal := aaRow("ok", "repeater", nil, aaEst(12*3600, advertConfidenceHigh, true), aaEst(7200, advertConfidenceHigh, true))
	normal.floodGapS, normal.zeroHopGapS = 12*3600, 7200

	got := aggregateAdvertIntervals([]advertIntervalNodeRow{normal, fastZH, def, edge, fastFlood}, aaAll)
	var seen []string
	for _, o := range got.Outliers {
		seen = append(seen, fmt.Sprintf("%s/%s/%s/%d", o.PublicKey, o.Class, o.Reason, o.MedianGapS))
	}
	want := "def/zero_hop/zero_hop_new_install_default/120 ff/flood/flood_faster_than_settable/600 fz/zero_hop/zero_hop_faster_than_settable/1200"
	if s := strings.Join(seen, " "); s != want {
		t.Fatalf("outliers = %s\nwant       %s", s, want)
	}
	if got.OutliersTotal != 3 {
		t.Fatalf("outliers_total = %d", got.OutliersTotal)
	}
	if o := got.Outliers[0]; o.Name != "Node def" || o.Role != "repeater" || o.IntervalS == nil || *o.IntervalS != 120 || len(o.Regions) != 1 {
		t.Fatalf("outlier row = %+v", o)
	}
	if raw, _ := json.Marshal(got.Outliers[1]); !strings.Contains(string(raw), `"regions":[]`) || !strings.Contains(string(raw), `"interval_s":null`) {
		t.Fatalf("outlier without regions or estimate: %s", raw)
	}

	var many []advertIntervalNodeRow
	for i := 0; i < advertIntervalOutlierLimit+7; i++ {
		r := aaRow(fmt.Sprintf("n%03d", i), "repeater", nil, aaNone(advertIntervalIrregular), aaNone(advertIntervalNoneObserved))
		r.floodGapS = float64(60 + i)
		many = append(many, r)
	}
	got = aggregateAdvertIntervals(many, aaAll)
	if len(got.Outliers) != advertIntervalOutlierLimit || got.OutliersTotal != advertIntervalOutlierLimit+7 {
		t.Fatalf("outliers = %d of %d", len(got.Outliers), got.OutliersTotal)
	}
	if got.Outliers[0].MedianGapS != 60 || got.Outliers[len(got.Outliers)-1].MedianGapS != int64(60+advertIntervalOutlierLimit-1) {
		t.Fatalf("most frequent first: %d .. %d", got.Outliers[0].MedianGapS, got.Outliers[len(got.Outliers)-1].MedianGapS)
	}
}

// advertMedianGap: the median gap of the class's sampled adverts, in any
// order; zero below the estimator's minimum samples.
func TestAdvertMedianGap(t *testing.T) {
	s := aiGaps(10*time.Minute, 10*time.Minute, 12*time.Hour, 10*time.Minute)
	s[0], s[3] = s[3], s[0]
	if g := advertMedianGap(s); g != 600 {
		t.Fatalf("median gap = %v, want 600 (not the mean)", g)
	}
	if g := advertMedianGap(aiGaps(time.Minute)); g != 0 {
		t.Fatalf("two adverts: %v, want 0", g)
	}
}

// The flood timer re-arms the zero-hop timer (MyMesh.cpp:1294-1306), the
// #247 review's F7 and N4: a zero-hop estimate equal to the flood interval
// is paced by flood, and with flood <= 240 min an unobserved zero-hop may be
// suppressed by it rather than off.
func TestAdvertIntervalAnalytics_FloodReArm(t *testing.T) {
	rows := []advertIntervalNodeRow{
		aaRow("paced", "repeater", nil, aaEst(3*3600, advertConfidenceHigh, true), aaEst(10800, advertConfidenceHigh, false)),
		aaRow("paced4", "repeater", nil, aaEst(4*3600, advertConfidenceMedium, true), aaEst(14400, advertConfidenceHigh, true)),
		aaRow("suppressed", "repeater", nil, aaEst(4*3600, advertConfidenceHigh, true), aaNone(advertIntervalNoneObserved)),
		aaRow("off", "repeater", nil, aaEst(5*3600, advertConfidenceHigh, true), aaNone(advertIntervalNoneObserved)),
		aaRow("normal", "repeater", nil, aaEst(12*3600, advertConfidenceHigh, true), aaEst(7200, advertConfidenceHigh, true)),
		aaRow("lowconf", "repeater", nil, aaEst(3*3600, advertConfidenceLow, true), aaNone(advertIntervalNoneObserved)),
	}
	got := aggregateAdvertIntervals(rows, aaAll)
	if got.FloodReArm.ZeroHopPacedByFlood != 2 || got.FloodReArm.ZeroHopMaybeSuppressed != 1 {
		t.Fatalf("flood_re_arm = %+v, want paced 2, maybe suppressed 1", got.FloodReArm)
	}
}

// The request filter keeps only matching nodes in every part of the
// response; region codes match case-insensitively, as a comma list.
func TestAdvertIntervalAnalytics_RegionAreaFilter(t *testing.T) {
	rows := []advertIntervalNodeRow{
		aaRow("a", "repeater", []string{"CPH"}, aaEst(47*3600, advertConfidenceHigh, true), aaNone(advertIntervalNoneObserved)),
		aaRow("b", "repeater", []string{"ODE"}, aaEst(12*3600, advertConfidenceHigh, true), aaEst(7200, advertConfidenceHigh, true)),
		aaRow("c", "companion", nil, aaNone(advertIntervalNoneObserved), aaNone(advertIntervalNoneObserved)),
	}
	rows[1].zeroHopGapS = 120
	keep := advertIntervalFilter("cph", nil)
	got := aggregateAdvertIntervals(rows, keep)
	if got.Nodes != 1 || aaBuckets(got.Flood.Buckets) != "169200:1" || len(got.Outliers) != 0 || len(got.ByRegion) != 1 {
		t.Fatalf("region=cph: %+v", got)
	}
	got = aggregateAdvertIntervals(rows, advertIntervalFilter(" ode , xyz", nil))
	if got.Nodes != 1 || len(got.Outliers) != 1 {
		t.Fatalf("region=ode,xyz: nodes %d outliers %d", got.Nodes, len(got.Outliers))
	}
	got = aggregateAdvertIntervals(rows, advertIntervalFilter("", map[string]bool{"c": true, "b": true}))
	if got.Nodes != 2 || aaGroup(t, got.ByRole, "companion").Nodes != 1 {
		t.Fatalf("area {b,c}: %+v", got)
	}
	got = aggregateAdvertIntervals(rows, advertIntervalFilter("CPH", map[string]bool{"b": true}))
	if got.Nodes != 0 || len(got.ByRole) != 0 || got.Flood.Buckets == nil || got.Outliers == nil {
		t.Fatalf("region and area both apply, and empty lists stay []: %+v", got)
	}
}

// --- DB scan -------------------------------------------------------------

type aaDB struct {
	t  *testing.T
	db *DB
}

func newAADB(t *testing.T) *aaDB {
	db := setupTestDB(t)
	narAddRouteMask(t, db)
	for _, o := range []struct{ id, iata string }{{"o-cph", "CPH"}, {"o-ode", " ode "}, {"o-none", ""}} {
		if _, err := db.conn.Exec(`INSERT INTO observers (id, name, iata, last_seen, first_seen) VALUES (?, ?, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
			o.id, "Observer "+o.id, o.iata); err != nil {
			t.Fatal(err)
		}
	}
	return &aaDB{t, db}
}

func (a *aaDB) node(pk, name, role string) {
	if _, err := a.db.conn.Exec(`INSERT INTO nodes (public_key, name, role, last_seen, first_seen) VALUES (?, ?, ?, '2026-09-01T00:00:00Z', '2026-01-01T00:00:00Z')`, pk, name, role); err != nil {
		a.t.Fatal(err)
	}
}

// advert inserts one advert of pk heard at heard by the observers named,
// sender clock heard - 2 s.
func (a *aaDB) advert(pk, hash string, routeType int, mask interface{}, heard time.Time, observers ...string) int64 {
	res, err := a.db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey, route_mask)
		VALUES ('1100', ?, ?, ?, 4, ?, ?, ?)`, hash, heard.UTC().Format(time.RFC3339), routeType,
		fmt.Sprintf(`{"type":"ADVERT","pubKey":%q,"timestamp":%d}`, pk, heard.Unix()-2), pk, mask)
	if err != nil {
		a.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	for _, o := range observers {
		if _, err := a.db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, timestamp) VALUES (?, (SELECT rowid FROM observers WHERE id = ?), ?)`,
			id, o, heard.Unix()); err != nil {
			a.t.Fatal(err)
		}
	}
	return id
}

const (
	aaFlood   = 0b0010
	aaZeroHop = 0b0100
	aaMixed   = 0b0110
)

// The scan keeps the newest nodeAdvertRouteLimit flood and zero-hop adverts
// per node, classified like node detail (mixed and unknown left out), and
// the regions of the observers that heard those adverts.
func TestScanAdvertIntervalSamples(t *testing.T) {
	a := newAADB(t)
	const pk = "245b000000000000000000000000000000000000000000000000000000000001"
	base := aiBase
	for i := 0; i < 25; i++ { // 25 flood adverts, newest 20 kept
		obs := []string{}
		if i == 24 {
			obs = []string{"o-ode"} // the newest flood advert, heard in ODE
		}
		if i == 0 {
			obs = []string{"o-cph"} // the oldest: dropped, so CPH must not count
		}
		a.advert(pk, fmt.Sprintf("f%02d", i), 1, aaFlood, base.Add(time.Duration(i)*12*time.Hour), obs...)
	}
	for i := 0; i < 4; i++ {
		a.advert(pk, fmt.Sprintf("z%d", i), 2, aaZeroHop, base.Add(time.Duration(i)*2*time.Hour+time.Minute), "o-none")
	}
	a.advert(pk, "mixed", 2, aaMixed, base.Add(time.Hour), "o-cph")
	a.advert(pk, "legacy-zh", 2, nil, base.Add(30*time.Hour)) // no mask yet: route_type 2 = zero-hop

	scan, err := a.db.scanAdvertIntervalSamples(advertIntervalScanRowCap)
	if err != nil {
		t.Fatal(err)
	}
	n := scan.nodes[pk]
	if n == nil {
		t.Fatalf("node missing: %v", scan.nodes)
	}
	if len(n.flood) != nodeAdvertRouteLimit || len(n.zeroHop) != 5 {
		t.Fatalf("flood %d, zero-hop %d; want %d and 5 (mixed left out, legacy route_type counted)", len(n.flood), len(n.zeroHop), nodeAdvertRouteLimit)
	}
	oldest := n.flood[0].heard
	for _, s := range n.flood {
		if s.heard.Before(oldest) {
			oldest = s.heard
		}
	}
	if want := base.Add(5 * 12 * time.Hour); !oldest.Equal(want) {
		t.Fatalf("oldest kept flood = %v, want %v (newest 20)", oldest, want)
	}
	if n.flood[0].senderTS == 0 {
		t.Fatal("sender clock not read")
	}
	if strings.Join(n.regions, ",") != "ODE" {
		t.Fatalf("regions = %v, want [ODE] (observers of the kept adverts, trimmed, upper-case)", n.regions)
	}
	if scan.rows != 31 || scan.truncated {
		t.Fatalf("rows %d truncated %v", scan.rows, scan.truncated)
	}

	capped, err := a.db.scanAdvertIntervalSamples(10)
	if err != nil {
		t.Fatal(err)
	}
	if !capped.truncated || capped.rows != 10 {
		t.Fatalf("row cap: rows %d truncated %v", capped.rows, capped.truncated)
	}
}

// Without the route_mask column the scan classifies by route_type alone,
// as node detail does.
func TestScanAdvertIntervalSamples_NoRouteMaskColumn(t *testing.T) {
	db := setupTestDB(t)
	const pk = "245b000000000000000000000000000000000000000000000000000000000002"
	for i := 0; i < 3; i++ {
		heard := aiBase.Add(time.Duration(i) * time.Hour)
		if _, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey)
			VALUES ('1100', ?, ?, 1, 4, '{"type":"ADVERT"}', ?)`, fmt.Sprint("h", i), heard.Format(time.RFC3339), pk); err != nil {
			t.Fatal(err)
		}
	}
	scan, err := db.scanAdvertIntervalSamples(advertIntervalScanRowCap)
	if err != nil {
		t.Fatal(err)
	}
	if n := scan.nodes[pk]; n == nil || len(n.flood) != 3 || len(n.zeroHop) != 0 {
		t.Fatalf("legacy schema: %+v", scan.nodes[pk])
	}
}

// The analytics estimate of a node is exactly node detail's (M1): same
// rows, same classifier, same estimator.
func TestAdvertIntervalAnalytics_MatchesNodeDetail(t *testing.T) {
	srv, router := setupTestServer(t)
	a := &aaDB{t, srv.db}
	narAddRouteMask(t, srv.db)
	const pk = "245b000000000000000000000000000000000000000000000000000000000003"
	a.node(pk, "Consistency Node", "repeater")
	now := time.Now().UTC().Truncate(time.Second)
	// Flood every 12 h with a missed advert and a manual one; zero-hop 120 min.
	for i, h := range []float64{1, 13, 25, 49, 61, 66, 73, 85} {
		a.advert(pk, fmt.Sprintf("cf%d", i), 1, aaFlood, now.Add(-time.Duration(h*float64(time.Hour))))
	}
	for i := 0; i < 7; i++ {
		a.advert(pk, fmt.Sprintf("cz%d", i), 2, aaZeroHop, now.Add(-time.Duration(30+120*i)*time.Minute))
	}
	srv.store.InvalidateNodeCache()
	var detail struct {
		Intervals *NodeAdvertIntervals `json:"advertIntervals"`
	}
	_, raw := narGetNode(t, router, pk+narIncludeQuery, 200)
	if err := json.Unmarshal([]byte(raw), &detail); err != nil || detail.Intervals == nil {
		t.Fatalf("node detail: %v %s", err, raw)
	}
	snap := srv.store.computeAdvertIntervalAnalytics()
	var row *advertIntervalNodeRow
	for i := range snap.rows {
		if snap.rows[i].pubkey == pk {
			row = &snap.rows[i]
		}
	}
	if row == nil {
		t.Fatal("node missing from the analytics rows")
	}
	if !reflect.DeepEqual(row.flood, detail.Intervals.Flood) || !reflect.DeepEqual(row.zeroHop, detail.Intervals.ZeroHop) {
		t.Fatalf("analytics != node detail\nflood    %+v\n         %+v\nzero-hop %+v\n         %+v",
			row.flood, detail.Intervals.Flood, row.zeroHop, detail.Intervals.ZeroHop)
	}
	if row.role != "repeater" || row.name != "Consistency Node" || row.flood.Status != advertIntervalEstimated || row.zeroHop.Status != advertIntervalEstimated {
		t.Fatalf("row = %+v", row)
	}
}

// Nodes without a nodes row (aged out) and hidden identities are left out.
func TestAdvertIntervalAnalytics_RosterAndHidden(t *testing.T) {
	srv, _ := setupTestServer(t)
	a := &aaDB{t, srv.db}
	narAddRouteMask(t, srv.db)
	const visible = "245b000000000000000000000000000000000000000000000000000000000010"
	const hiddenName = "245b000000000000000000000000000000000000000000000000000000000011"
	const blacklisted = "245b000000000000000000000000000000000000000000000000000000000012"
	const gone = "245b000000000000000000000000000000000000000000000000000000000013"
	a.node(visible, "Visible", "repeater")
	a.node(hiddenName, "zz-secret", "repeater")
	a.node(blacklisted, "Black", "repeater")
	for i, pk := range []string{visible, hiddenName, blacklisted, gone} {
		a.advert(pk, fmt.Sprint("ra", i), 1, aaFlood, aiBase.Add(time.Duration(i)*time.Minute))
	}
	srv.store.InvalidateNodeCache()
	srv.store.config.HiddenNamePrefixes = []string{"zz-"}
	srv.store.config.NodeBlacklist = []string{blacklisted}
	snap := srv.store.computeAdvertIntervalAnalytics()
	got := map[string]bool{}
	for _, r := range snap.rows {
		got[r.pubkey] = true
	}
	if !got[visible] || got[hiddenName] || got[blacklisted] || got[gone] {
		t.Fatalf("rows = %v: want the visible node only (of these)", got)
	}
}

// --- API and recomputer --------------------------------------------------

func aaGet(t *testing.T, router http.Handler, q string, want int) AdvertIntervalAnalyticsResponse {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/advert-intervals"+q, nil))
	if w.Code != want {
		t.Fatalf("GET %s = %d, want %d: %s", q, w.Code, want, w.Body.String())
	}
	var body AdvertIntervalAnalyticsResponse
	if want == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{`"flood"`, `"zero_hop"`, `"by_role"`, `"by_region"`, `"outliers"`, `"flood_re_arm"`, `"scan"`} {
			if !strings.Contains(w.Body.String(), key) {
				t.Fatalf("response lacks %s: %s", key, w.Body.String())
			}
		}
		for _, key := range []string{`"buckets":null`, `"by_role":null`, `"by_region":null`, `"outliers":null`, `"regions":null`} {
			if strings.Contains(w.Body.String(), key) {
				t.Fatalf("response has %s, lists must be []: %s", key, w.Body.String())
			}
		}
	}
	return body
}

func TestHandleAnalyticsAdvertIntervals(t *testing.T) {
	srv, router := setupTestServer(t)
	a := &aaDB{t, srv.db}
	narAddRouteMask(t, srv.db)
	const cph = "245b000000000000000000000000000000000000000000000000000000000020"
	const other = "245b000000000000000000000000000000000000000000000000000000000021"
	a.node(cph, "Region Node", "repeater")
	a.node(other, "Elsewhere", "companion")
	if _, err := srv.db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES ('o-cph', 'Observer CPH', 'CPH')`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		a.advert(cph, fmt.Sprint("hf", i), 1, aaFlood, aiBase.Add(time.Duration(i)*12*time.Hour), "o-cph")
		a.advert(cph, fmt.Sprint("hz", i), 2, aaZeroHop, aiBase.Add(time.Duration(i)*2*time.Minute+time.Hour), "o-cph")
	}
	a.advert(other, "ho", 1, aaFlood, aiBase, "obs2") // SFO
	srv.cfg.Areas = map[string]AreaEntry{"fyn": {}}
	srv.store.InvalidateNodeCache()

	all := aaGet(t, router, "", 200)
	if all.Nodes < 2 {
		t.Fatalf("nodes = %d", all.Nodes)
	}
	got := aaGet(t, router, "?region=cph", 200)
	if got.Nodes != 1 || aaBuckets(got.Flood.Buckets) != "43200:1" || aaBuckets(got.ZeroHop.Buckets) != "120:1" {
		t.Fatalf("region=cph: nodes %d flood %s zero-hop %s", got.Nodes, aaBuckets(got.Flood.Buckets), aaBuckets(got.ZeroHop.Buckets))
	}
	if len(got.Outliers) != 1 || got.Outliers[0].Reason != advertOutlierZeroHopDefault || got.Outliers[0].PublicKey != cph {
		t.Fatalf("region=cph outliers = %+v", got.Outliers)
	}
	if got.Scan.GeneratedAt == "" || got.Scan.Truncated || got.Scan.Rows < 17 {
		t.Fatalf("scan = %+v", got.Scan)
	}
	if sfo := aaGet(t, router, "?region=SFO", 200); sfo.Nodes < 1 || len(sfo.Outliers) != 0 {
		t.Fatalf("region=SFO: %+v", sfo)
	}
	aaGet(t, router, "?area=nope", 400)
	aaGet(t, router, "?region="+strings.Repeat("X", analyticsFilterMaxLen+1), 400)
}

// One recomputer per analytics cycle (pauseWhenIdle-aware like the others);
// requests, filtered or not, read its snapshot and never recompute.
func TestAdvertIntervalAnalytics_Recomputer(t *testing.T) {
	srv, router := setupTestServer(t)
	stop := srv.store.StartAnalyticsRecomputers(time.Hour)
	defer stop()
	rc := srv.store.recomputer(&srv.store.recompAdvertIntervals)
	if rc == nil || rc.name != "advert-intervals" {
		t.Fatal("advert-intervals recomputer not started")
	}
	if _, ok := rc.Load().(*advertIntervalSnapshot); !ok {
		t.Fatalf("snapshot type %T", rc.Load())
	}
	runs := rc.ComputeRuns()
	// A node that advertises after the pass is not in any response until
	// the next pass: requests read the snapshot, they do not recompute.
	a := &aaDB{t, srv.db}
	narAddRouteMask(t, srv.db)
	const late = "245b000000000000000000000000000000000000000000000000000000000030"
	a.node(late, "Late Node", "repeater")
	if _, err := srv.db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES ('o-late', 'Late', 'LTE')`); err != nil {
		t.Fatal(err)
	}
	a.advert(late, "late-1", 1, aaFlood, aiBase, "o-late")
	srv.store.InvalidateNodeCache()
	for _, q := range []string{"", "?region=LTE", "?region=SFO"} {
		if got := aaGet(t, router, q, 200); q == "?region=LTE" && got.Nodes != 0 {
			t.Fatalf("%s served a fresh compute (%d nodes), not the snapshot", q, got.Nodes)
		}
	}
	if got := rc.ComputeRuns(); got != runs {
		t.Fatalf("requests recomputed: %d -> %d runs", runs, got)
	}
	rc.RecomputeNow()
	if got := aaGet(t, router, "?region=LTE", 200); got.Nodes != 1 {
		t.Fatalf("after the next pass region=LTE has %d nodes, want 1", got.Nodes)
	}
	if v := (&Config{Analytics: &AnalyticsConfig{RecomputeIntervalSeconds: map[string]int{"advertIntervals": 90}}}).AnalyticsRecomputeIntervals(); v.AdvertIntervals != 90*time.Second {
		t.Fatalf("config key advertIntervals = %v", v.AdvertIntervals)
	}
}

// #495: the compute never takes the packet store lock - it reads the
// database and the node cache only - so a held store write lock (ingest)
// cannot stall it, and it cannot stall ingest.
func TestAdvertIntervalAnalytics_OutsideStoreLock(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.store.getCachedNodesAndPM() // warm the node cache, as the recomputer finds it
	srv.store.mu.Lock()
	done := make(chan *advertIntervalSnapshot, 1)
	go func() { done <- srv.store.computeAdvertIntervalAnalytics() }()
	select {
	case snap := <-done:
		srv.store.mu.Unlock()
		if snap == nil {
			t.Fatal("nil snapshot")
		}
	case <-time.After(5 * time.Second):
		srv.store.mu.Unlock()
		<-done
		t.Fatal("computeAdvertIntervalAnalytics waited on the store lock")
	}
}

// Perf proof (AGENTS.md rule 0): a filtered request aggregates the cached
// rows of a 5000-node mesh, most of them outliers, well inside a request
// budget: 25 ms (about 4 ms measured), 250 ms under -race (about 56 ms
// measured with -race -cover, as CI runs it).
func TestAdvertIntervalAnalytics_AggregatePerf(t *testing.T) {
	budget := 25 * time.Millisecond
	if raceBuild {
		budget = 250 * time.Millisecond
	}
	rows := aaPerfRows(5000)
	keep := advertIntervalFilter("R03", nil)
	start := time.Now()
	const n = 20
	for i := 0; i < n; i++ {
		aggregateAdvertIntervals(rows, keep)
		aggregateAdvertIntervals(rows, aaAll)
	}
	per := time.Since(start) / (2 * n)
	t.Logf("aggregate 5000 nodes: %v per call", per)
	if per > budget {
		t.Fatalf("aggregate 5000 nodes took %v per call, budget %v", per, budget)
	}
}

func aaPerfRows(n int) []advertIntervalNodeRow {
	roles := []string{"repeater", "companion", "room", "sensor"}
	rows := make([]advertIntervalNodeRow, n)
	for i := range rows {
		rows[i] = aaRow(fmt.Sprintf("p%05d", i), roles[i%4], []string{fmt.Sprintf("R%02d", i%40), fmt.Sprintf("R%02d", (i+1)%40)},
			aaEst(int64(3+i%166)*3600, advertConfidenceHigh, true), aaEst(int64(60+2*(i%91))*60, advertConfidenceMedium, true))
		rows[i].floodGapS, rows[i].zeroHopGapS = float64(i%7000), float64(i%4000)
	}
	return rows
}

func BenchmarkAggregateAdvertIntervals5000(b *testing.B) {
	rows := aaPerfRows(5000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		aggregateAdvertIntervals(rows, aaAll)
	}
}

// Perf proof for the per-cycle scan: 2000 nodes x 40 adverts (80000 rows),
// each heard by two observers.
func BenchmarkComputeAdvertIntervalAnalytics(b *testing.B) {
	db := setupTestDB(b)
	if _, err := db.conn.Exec(`ALTER TABLE transmissions ADD COLUMN route_mask INTEGER`); err != nil {
		b.Fatal(err)
	}
	db.hasRouteMaskFlag.forceTrue()
	db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES ('b1', 'B1', 'CPH'), ('b2', 'B2', 'ODE')`)
	tx, _ := db.conn.Begin()
	for n := 0; n < 2000; n++ {
		pk := fmt.Sprintf("%064x", n+1)
		tx.Exec(`INSERT INTO nodes (public_key, name, role) VALUES (?, ?, 'repeater')`, pk, fmt.Sprint("N", n))
		for i := 0; i < 40; i++ {
			rt, mask, step := 1, aaFlood, 12*time.Hour
			if i%2 == 1 {
				rt, mask, step = 2, aaZeroHop, 2*time.Hour
			}
			heard := aiBase.Add(time.Duration(i/2) * step)
			res, err := tx.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey, route_mask)
				VALUES ('1100', ?, ?, ?, 4, ?, ?, ?)`, fmt.Sprintf("%d-%d", n, i), heard.Format(time.RFC3339), rt,
				fmt.Sprintf(`{"type":"ADVERT","pubKey":%q,"timestamp":%d}`, pk, heard.Unix()), pk, mask)
			if err != nil {
				b.Fatal(err)
			}
			id, _ := res.LastInsertId()
			tx.Exec(`INSERT INTO observations (transmission_id, observer_idx, timestamp) VALUES (?, 1, 0), (?, 2, 0)`, id, id)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	s := NewPacketStore(db, nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snap := s.computeAdvertIntervalAnalytics()
		if len(snap.rows) != 2000 {
			b.Fatalf("rows = %d", len(snap.rows))
		}
	}
}

// --- docs ---------------------------------------------------------------

func TestOpenAPI_AdvertIntervalAnalytics(t *testing.T) {
	spec := fetchSpec(t)
	schemas := asMap(t, asMap(t, spec["components"], "components")["schemas"], "schemas")
	for name, v := range map[string]interface{}{
		"AdvertIntervalAnalyticsResponse": AdvertIntervalAnalyticsResponse{},
		"AdvertIntervalClassSummary":      AdvertIntervalClassSummary{},
		"AdvertIntervalBucket":            AdvertIntervalBucket{},
		"AdvertIntervalGroup":             AdvertIntervalGroup{},
		"AdvertIntervalOutlier":           AdvertIntervalOutlier{},
		"AdvertIntervalFloodReArm":        AdvertIntervalFloodReArm{},
		"AdvertIntervalScanInfo":          AdvertIntervalScanInfo{},
	} {
		p := asMap(t, asMap(t, schemas[name], name)["properties"], name+".properties")
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			tag := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
			if _, ok := p[tag]; !ok && tag != "" && tag != "-" {
				t.Errorf("%s.%s is emitted but not documented", name, tag)
			}
		}
	}
	paths := asMap(t, spec["paths"], "paths")
	if _, ok := paths["/api/analytics/advert-intervals"]; !ok {
		t.Fatal("GET /api/analytics/advert-intervals is not documented")
	}
	raw, err := os.ReadFile("../../docs/api-spec.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "GET /api/analytics/advert-intervals") {
		t.Fatal("docs/api-spec.md does not document GET /api/analytics/advert-intervals")
	}
}
