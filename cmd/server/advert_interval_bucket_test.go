package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// #896 M1: GET /api/analytics/advert-intervals/bucket lists the nodes of one
// Flood or Zero-hop distribution bucket, from the published snapshot only,
// with the identities hidden at serve time withheld; the same serve-time
// filter keeps hidden identities out of the outliers.

const aabPath = "/api/analytics/advert-intervals/bucket"

func aabKey(n int) string { return fmt.Sprintf("896b%060x", n) }

// aabNamed is aaRow with a 64-hex key and a name of its own.
func aabNamed(n int, name string, regions []string, flood, zh AdvertIntervalEstimate) advertIntervalNodeRow {
	r := aaRow(aabKey(n), "repeater", regions, flood, zh)
	r.name = name
	return r
}

// aabSnapshot is the snapshot a pass over rows publishes.
func aabSnapshot(rows []advertIntervalNodeRow, at string) *advertIntervalSnapshot {
	sort.Slice(rows, func(i, j int) bool { return rows[i].pubkey < rows[j].pubkey })
	snap := &advertIntervalSnapshot{rows: rows, all: aggregateAdvertIntervals(rows, aaAll)}
	snap.all.Scan = AdvertIntervalScanInfo{GeneratedAt: at}
	return snap
}

// aabRec is an advert-intervals recomputer publishing a test snapshot and
// counting its passes.
type aabRec struct {
	rc     *analyticsRecomputer
	passes atomic.Int64
	snap   atomic.Pointer[advertIntervalSnapshot]
}

// aabPublish installs and starts an advert-intervals recomputer on srv whose
// passes publish snap, as StartAnalyticsRecomputers would.
func aabPublish(t testing.TB, srv *Server, snap *advertIntervalSnapshot, pauseWhenIdle bool) *aabRec {
	t.Helper()
	p := &aabRec{}
	p.snap.Store(snap)
	p.rc = newAnalyticsRecomputer("advert-intervals", time.Hour, func() interface{} {
		p.passes.Add(1)
		return p.snap.Load()
	})
	p.rc.pauseWhenIdle = pauseWhenIdle
	p.rc.Start()
	t.Cleanup(p.rc.Stop)
	srv.store.analyticsRecomputerMu.Lock()
	srv.store.recompAdvertIntervals = p.rc
	srv.store.analyticsRecomputerMu.Unlock()
	return p
}

// refresh publishes snap as the next pass.
func (p *aabRec) refresh(snap *advertIntervalSnapshot) {
	p.snap.Store(snap)
	p.rc.RecomputeNow()
}

func aabGet(t *testing.T, router http.Handler, q string, want int) (AdvertIntervalBucketResponse, string, http.Header) {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", aabPath+q, nil))
	if w.Code != want {
		t.Fatalf("GET %s = %d, want %d: %s", q, w.Code, want, w.Body.String())
	}
	var body AdvertIntervalBucketResponse
	if want == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{`"members":null`, `"regions":null`} {
			if strings.Contains(w.Body.String(), bad) {
				t.Fatalf("response has %s, lists must be []: %s", bad, w.Body.String())
			}
		}
	}
	return body, w.Body.String(), w.Header()
}

func aabKeys(members []AdvertIntervalBucketMember) []string {
	out := make([]string, len(members))
	for i, m := range members {
		out[i] = m.PublicKey
	}
	return out
}

// --- the shared bucket rule --------------------------------------------

// advertIntervalBucketOf is the histogram's rule: counted estimates only,
// keyed by the bucket value and snapped.
func TestAdvertIntervalBucketOf(t *testing.T) {
	nilInterval := aaEst(10800, advertConfidenceHigh, true)
	nilInterval.IntervalS = nil
	for _, c := range []struct {
		name  string
		e     AdvertIntervalEstimate
		class advertIntervalClass
		want  advertIntervalBucketKey
		ok    bool
	}{
		{"flood snapped", aaEst(10800, advertConfidenceHigh, true), advertIntervalFlood, advertIntervalBucketKey{10800, true}, true},
		{"flood unsnapped rounds to the hour", aaEst(10500, advertConfidenceMedium, false), advertIntervalFlood, advertIntervalBucketKey{10800, false}, true},
		{"zero-hop unsnapped rounds to the minute", aaEst(7290, advertConfidenceHigh, false), advertIntervalZeroHop, advertIntervalBucketKey{7320, false}, true},
		{"zero-hop snapped", aaEst(120, advertConfidenceHigh, true), advertIntervalZeroHop, advertIntervalBucketKey{120, true}, true},
		{"low confidence", aaEst(10800, advertConfidenceLow, true), advertIntervalFlood, advertIntervalBucketKey{}, false},
		{"irregular", aaNone(advertIntervalIrregular), advertIntervalFlood, advertIntervalBucketKey{}, false},
		{"too few", aaNone(advertIntervalTooFew), advertIntervalFlood, advertIntervalBucketKey{}, false},
		{"not observed", aaNone(advertIntervalNoneObserved), advertIntervalZeroHop, advertIntervalBucketKey{}, false},
		{"no interval", nilInterval, advertIntervalFlood, advertIntervalBucketKey{}, false},
	} {
		got, ok := advertIntervalBucketOf(c.e, c.class)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got %+v %v, want %+v %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// aabMixedRows is aaPerfRows plus rows the rule must keep apart: a snapped
// and an unsnapped estimate with the same rounded value per class, and
// every estimate that is not counted.
func aabMixedRows() []advertIntervalNodeRow {
	rows := aaPerfRows(500)
	nilInterval := aaEst(10800, advertConfidenceHigh, true)
	nilInterval.IntervalS = nil
	extra := []advertIntervalNodeRow{
		aaRow("x01", "repeater", []string{"R03"}, aaEst(10800, advertConfidenceHigh, true), aaEst(7200, advertConfidenceHigh, true)),
		aaRow("x02", "repeater", []string{"R03"}, aaEst(10500, advertConfidenceMedium, false), aaEst(7210, advertConfidenceHigh, false)),
		aaRow("x03", "room", []string{"R07"}, aaEst(11000, advertConfidenceHigh, false), aaEst(7190, advertConfidenceMedium, false)),
		aaRow("x04", "repeater", []string{"R03"}, aaEst(10800, advertConfidenceLow, true), aaEst(7200, advertConfidenceLow, true)),
		aaRow("x05", "repeater", nil, aaNone(advertIntervalIrregular), aaNone(advertIntervalTooFew)),
		aaRow("x06", "repeater", nil, aaNone(advertIntervalNoneObserved), nilInterval),
	}
	return append(rows, extra...)
}

// Property: for every bucket of every filter, the members are exactly the
// bucket's count, every member is in the bucket and kept by the filter,
// and the buckets together hold the class's estimated nodes once each.
func TestAdvertIntervalBucket_MatchesHistogram(t *testing.T) {
	rows := aabMixedRows()
	area := map[string]bool{}
	for i := 0; i < len(rows); i += 3 {
		area[rows[i].pubkey] = true
	}
	filters := map[string]func(*advertIntervalNodeRow) bool{
		"all":          advertIntervalFilter("", nil),
		"region":       advertIntervalFilter("R03", nil),
		"multi-region": advertIntervalFilter("r01, R07", nil),
		"area":         advertIntervalFilter("", area),
		"region+area":  advertIntervalFilter("R03", area),
	}
	for name, keep := range filters {
		agg := aggregateAdvertIntervals(rows, keep)
		for _, c := range []struct {
			class advertIntervalClass
			sum   AdvertIntervalClassSummary
		}{{advertIntervalFlood, agg.Flood}, {advertIntervalZeroHop, agg.ZeroHop}} {
			seen := map[string]bool{}
			total := 0
			for _, b := range c.sum.Buckets {
				key := advertIntervalBucketKey{b.IntervalS, b.Snapped}
				members := advertIntervalBucketMembers(rows, keep, c.class, key)
				if len(members) != b.Nodes {
					t.Fatalf("%s class %d bucket %+v: %d members, histogram %d", name, c.class, b, len(members), b.Nodes)
				}
				for _, m := range members {
					if k, ok := advertIntervalBucketOf(m.estimate(c.class), c.class); !ok || k != key || !keep(m) {
						t.Fatalf("%s: member %s is not in bucket %+v", name, m.pubkey, key)
					}
					if seen[m.pubkey] {
						t.Fatalf("%s: %s in two buckets", name, m.pubkey)
					}
					seen[m.pubkey] = true
				}
				total += len(members)
			}
			if total != c.sum.Estimated {
				t.Fatalf("%s class %d: buckets hold %d members, estimated %d", name, c.class, total, c.sum.Estimated)
			}
		}
	}
	// The collisions are real buckets of their own.
	if got := advertIntervalBucketMembers(rows, aaAll, advertIntervalFlood, advertIntervalBucketKey{10800, false}); len(got) != 2 {
		t.Fatalf("unsnapped flood 3 h: %d members, want x02 and x03", len(got))
	}
	if got := advertIntervalBucketMembers(rows, aaAll, advertIntervalZeroHop, advertIntervalBucketKey{7200, false}); len(got) != 2 {
		t.Fatalf("unsnapped zero-hop 120 min: %d members, want x02 and x03", len(got))
	}
}

// --- the endpoint -------------------------------------------------------

const aabAt = "2026-11-21T10:00:00Z"

// aabRows is a small mesh: three nodes in the snapped Flood 3 h bucket
// (two in CPH), one unsnapped Flood 3 h node, and estimates that are not
// counted.
func aabRows() []advertIntervalNodeRow {
	return []advertIntervalNodeRow{
		aabNamed(1, "Charlie Relay", []string{"CPH"}, aaEst(10800, advertConfidenceHigh, true), aaEst(7200, advertConfidenceMedium, true)),
		aabNamed(2, "alpha relay", []string{"CPH", "ODE"}, aaEst(10800, advertConfidenceMedium, true), aaNone(advertIntervalNoneObserved)),
		aabNamed(3, "Bravo Relay", []string{"ODE"}, aaEst(10800, advertConfidenceHigh, true), aaEst(7200, advertConfidenceHigh, true)),
		aabNamed(4, "Unsnapped Relay", []string{"CPH"}, aaEst(10500, advertConfidenceHigh, false), aaEst(7210, advertConfidenceHigh, false)),
		aabNamed(5, "Low Relay", []string{"CPH"}, aaEst(10800, advertConfidenceLow, true), aaNone(advertIntervalTooFew)),
		aabNamed(6, "Irregular Relay", nil, aaNone(advertIntervalIrregular), aaNone(advertIntervalNoneObserved)),
	}
}

func TestHandleAdvertIntervalBucket(t *testing.T) {
	srv, router := setupTestServer(t)
	snap := aabSnapshot(aabRows(), aabAt)
	aabPublish(t, srv, snap, false)

	got, raw, hdr := aabGet(t, router, "?class=flood&interval_s=10800&snapped=true", 200)
	if got.Class != "flood" || got.IntervalS != 10800 || !got.Snapped || got.GeneratedAt != aabAt {
		t.Fatalf("echo: %+v", got)
	}
	if got.Nodes != 3 || got.Visible != 3 || got.Withheld != 0 || got.Offset != 0 || got.Limit != 50 {
		t.Fatalf("counts: %+v", got)
	}
	// The count is the histogram's.
	for _, b := range snap.all.Flood.Buckets {
		if b.IntervalS == 10800 && b.Snapped && b.Nodes != got.Nodes {
			t.Fatalf("histogram %d, bucket %d", b.Nodes, got.Nodes)
		}
	}
	if want := []string{aabKey(2), aabKey(3), aabKey(1)}; !reflect.DeepEqual(aabKeys(got.Members), want) {
		t.Fatalf("order by name: %v, want %v", aabKeys(got.Members), want)
	}
	m := got.Members[0]
	if m.Name != "alpha relay" || m.Role != "repeater" || strings.Join(m.Regions, ",") != "CPH,ODE" ||
		m.IntervalS != 10800 || !m.Snapped || m.Confidence != advertConfidenceMedium || m.Samples != 10 || m.GapsUsed != 8 {
		t.Fatalf("member: %+v", m)
	}
	if hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", hdr.Get("Cache-Control"))
	}
	if strings.Contains(raw, aabKey(5)) || strings.Contains(raw, aabKey(4)) {
		t.Fatalf("low-confidence or unsnapped node in the snapped bucket: %s", raw)
	}

	// Snapped and unsnapped with the same rounded value are two lists; a
	// member keeps its own estimate.
	un, _, _ := aabGet(t, router, "?class=flood&interval_s=10800&snapped=false", 200)
	if un.Nodes != 1 || len(un.Members) != 1 || un.Members[0].PublicKey != aabKey(4) || un.Members[0].IntervalS != 10500 || un.Members[0].Snapped {
		t.Fatalf("unsnapped flood 3 h: %+v", un)
	}
	zh, _, _ := aabGet(t, router, "?class=zero_hop&interval_s=7200&snapped=true", 200)
	if want := []string{aabKey(3), aabKey(1)}; !reflect.DeepEqual(aabKeys(zh.Members), want) {
		t.Fatalf("zero-hop 120 min: %v, want %v", aabKeys(zh.Members), want)
	}

	// Region: the histogram's filter, one or several codes.
	cph, _, _ := aabGet(t, router, "?class=flood&interval_s=10800&snapped=true&region=cph", 200)
	if want := []string{aabKey(2), aabKey(1)}; !reflect.DeepEqual(aabKeys(cph.Members), want) || cph.Nodes != 2 {
		t.Fatalf("region=cph: %+v", cph)
	}
	both, _, _ := aabGet(t, router, "?class=flood&interval_s=10800&snapped=true&region=CPH,ODE", 200)
	if both.Nodes != 3 {
		t.Fatalf("region=CPH,ODE: %+v", both)
	}

	// A bucket the snapshot does not hold is empty, not an error.
	none, raw, _ := aabGet(t, router, "?class=zero_hop&interval_s=999&snapped=true", 200)
	if none.Nodes != 0 || none.Visible != 0 || len(none.Members) != 0 || !strings.Contains(raw, `"members":[]`) {
		t.Fatalf("unknown bucket: %s", raw)
	}
}

// Area: nodes positioned in the area, as the histogram's area filter.
func TestHandleAdvertIntervalBucket_Area(t *testing.T) {
	srv, router := setupTestServer(t)
	for _, n := range []struct {
		pk       string
		lat, lon float64
	}{{aabKey(1), 55.4, 10.4}, {aabKey(3), 40.0, -120.0}} {
		if _, err := srv.db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon) VALUES (?, 'n', 'repeater', ?, ?)`, n.pk, n.lat, n.lon); err != nil {
			t.Fatal(err)
		}
	}
	lo, hi, wlo, whi := 55.0, 56.0, 10.0, 11.0
	srv.cfg.Areas = map[string]AreaEntry{"fyn": {Label: "Fyn", LatMin: &lo, LatMax: &hi, LonMin: &wlo, LonMax: &whi}}
	aabPublish(t, srv, aabSnapshot(aabRows(), aabAt), false)
	got, _, _ := aabGet(t, router, "?class=flood&interval_s=10800&snapped=true&area=fyn", 200)
	if want := []string{aabKey(1)}; !reflect.DeepEqual(aabKeys(got.Members), want) || got.Nodes != 1 {
		t.Fatalf("area=fyn: %+v", got)
	}
	aabGet(t, router, "?class=flood&interval_s=10800&snapped=true&area=nope", 400)
}

// Stable paging: one total order (name case-insensitively, then name, then
// key), offset/limit bounds, an offset past the end is an empty page.
func TestAdvertIntervalBucket_Pagination(t *testing.T) {
	srv, router := setupTestServer(t)
	f := aaEst(43200, advertConfidenceHigh, true)
	none := aaNone(advertIntervalNoneObserved)
	rows := []advertIntervalNodeRow{
		aabNamed(10, "bravo", nil, f, none), aabNamed(11, "Alpha", nil, f, none), aabNamed(13, "alpha", nil, f, none),
		aabNamed(12, "alpha", nil, f, none), aabNamed(14, "", nil, f, none), aabNamed(15, "Charlie", nil, f, none),
	}
	aabPublish(t, srv, aabSnapshot(rows, aabAt), false)
	want := []string{aabKey(14), aabKey(11), aabKey(12), aabKey(13), aabKey(10), aabKey(15)}
	q := "?class=flood&interval_s=43200&snapped=true"
	all, _, _ := aabGet(t, router, q+"&limit=200", 200)
	if !reflect.DeepEqual(aabKeys(all.Members), want) {
		t.Fatalf("order: %v, want %v", aabKeys(all.Members), want)
	}
	var paged []string
	for off := 0; off < 6; off += 4 {
		p, _, _ := aabGet(t, router, fmt.Sprintf("%s&limit=4&offset=%d", q, off), 200)
		if p.Visible != 6 || p.Nodes != 6 || p.Offset != off || p.Limit != 4 {
			t.Fatalf("page %d: %+v", off, p)
		}
		paged = append(paged, aabKeys(p.Members)...)
	}
	if !reflect.DeepEqual(paged, want) {
		t.Fatalf("pages: %v, want %v", paged, want)
	}
	for _, off := range []int{6, 1000000} {
		end, raw, _ := aabGet(t, router, fmt.Sprintf("%s&offset=%d", q, off), 200)
		if len(end.Members) != 0 || end.Visible != 6 || !strings.Contains(raw, `"members":[]`) {
			t.Fatalf("offset %d: %s", off, raw)
		}
	}
	if big, _, _ := aabGet(t, router, q+"&limit=500", 200); big.Limit != 200 {
		t.Fatalf("limit=500 serves %d, want the 200 cap", big.Limit)
	}
	if dflt, _, _ := aabGet(t, router, q, 200); dflt.Limit != 50 {
		t.Fatalf("default limit %d", dflt.Limit)
	}
}

// A list never mixes two snapshots: snapshot= names the one the client
// paged from; once the published snapshot differs the answer is 409 with
// the current time, and the counts follow the new snapshot.
func TestAdvertIntervalBucket_SnapshotConsistency(t *testing.T) {
	srv, router := setupTestServer(t)
	p := aabPublish(t, srv, aabSnapshot(aabRows(), aabAt), false)
	q := "?class=flood&interval_s=10800&snapped=true"
	first, _, _ := aabGet(t, router, q+"&snapshot="+aabAt, 200)
	if first.Nodes != 3 {
		t.Fatalf("first: %+v", first)
	}
	const later = "2026-11-21T10:05:00Z"
	rows := append(aabRows(), aabNamed(7, "Delta Relay", []string{"CPH"}, aaEst(10800, advertConfidenceHigh, true), aaNone(advertIntervalNoneObserved)))
	p.refresh(aabSnapshot(rows, later))

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", aabPath+q+"&offset=2&snapshot="+aabAt, nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("stale snapshot= = %d, want 409: %s", w.Code, w.Body.String())
	}
	var conflict AdvertIntervalBucketConflict
	if err := json.Unmarshal(w.Body.Bytes(), &conflict); err != nil || conflict.GeneratedAt != later || conflict.Error == "" {
		t.Fatalf("409 body: %s (%v)", w.Body.String(), err)
	}
	if strings.Contains(w.Body.String(), "members") {
		t.Fatalf("409 carries a list: %s", w.Body.String())
	}
	now, _, _ := aabGet(t, router, q, 200)
	if now.Nodes != 4 || now.GeneratedAt != later {
		t.Fatalf("after the refresh: %+v", now)
	}
	if again, _, _ := aabGet(t, router, q+"&snapshot="+later, 200); again.Nodes != 4 {
		t.Fatalf("current snapshot=: %+v", again)
	}
}

// Opening a bucket never computes: it reads the published snapshot (no
// pass, no DB scan); before the first snapshot it answers 503 instead of
// scanning as the histogram's startup fallback does.
func TestAdvertIntervalBucket_NoScan(t *testing.T) {
	srv, router := setupTestServer(t)
	// A node a scan would find: a 503 below can not be a scan that came
	// back empty.
	a := &aaDB{t, srv.db}
	narAddRouteMask(t, srv.db)
	const pk = "896b0000000000000000000000000000000000000000000000000000000000ff"
	a.node(pk, "Scan Me", "repeater")
	for i := 0; i < 8; i++ {
		a.advert(pk, fmt.Sprint("ns", i), 1, aaFlood, aiBase.Add(time.Duration(i)*3*time.Hour))
	}
	srv.store.InvalidateNodeCache()
	q := "?class=flood&interval_s=10800&snapped=true"

	// No recomputer: 503, never the compute-on-miss fallback.
	_, raw, hdr := aabGet(t, router, q, http.StatusServiceUnavailable)
	if strings.Contains(raw, pk) || strings.Contains(raw, "members") || hdr.Get("Retry-After") == "" {
		t.Fatalf("503 without a snapshot: %s %v", raw, hdr)
	}
	// A recomputer that has not published yet: 503, and no pass ran.
	idle := newAnalyticsRecomputer("advert-intervals", time.Hour, func() interface{} { return srv.store.computeAdvertIntervalAnalytics() })
	srv.store.analyticsRecomputerMu.Lock()
	srv.store.recompAdvertIntervals = idle
	srv.store.analyticsRecomputerMu.Unlock()
	aabGet(t, router, q, http.StatusServiceUnavailable)
	if idle.ComputeRuns() != 0 {
		t.Fatalf("a bucket request ran %d passes", idle.ComputeRuns())
	}

	// Published: requests read it and never compute.
	p := aabPublish(t, srv, aabSnapshot(aabRows(), aabAt), false)
	before := p.passes.Load()
	for _, extra := range []string{"", "&region=CPH", "&limit=1&offset=1", "&snapshot=" + aabAt} {
		if got, _, _ := aabGet(t, router, q+extra, 200); got.GeneratedAt != aabAt {
			t.Fatalf("%s: %+v", extra, got)
		}
	}
	if p.passes.Load() != before || p.rc.ComputeRuns() != before {
		t.Fatalf("bucket requests computed: %d -> %d passes", before, p.passes.Load())
	}
}

// With analytics.pauseWhenIdle on, a bucket is labelled like the histogram
// (#492).
func TestAdvertIntervalBucket_SnapshotHeaders(t *testing.T) {
	srv, router := setupTestServer(t)
	aabPublish(t, srv, aabSnapshot(aabRows(), aabAt), true)
	_, _, hdr := aabGet(t, router, "?class=flood&interval_s=10800&snapped=true", 200)
	if hdr.Get(analyticsSnapshotHeader) == "" || hdr.Get(analyticsSnapshotAgeHeader) == "" {
		t.Fatalf("#492 headers missing: %v", hdr)
	}
}

func TestAdvertIntervalBucket_Validation(t *testing.T) {
	srv, router := setupTestServer(t)
	srv.cfg.Areas = map[string]AreaEntry{"fyn": {}}
	ok := "class=flood&interval_s=10800&snapped=true"
	// Invalid parameters are rejected before the snapshot is looked at.
	aabGet(t, router, "?class=mixed&interval_s=10800&snapped=true", 400)
	aabPublish(t, srv, aabSnapshot(aabRows(), aabAt), false)
	for _, q := range []string{
		"interval_s=10800&snapped=true", "class=&interval_s=10800&snapped=true", "class=FLOOD&interval_s=10800&snapped=true",
		"class=mixed&interval_s=10800&snapped=true",
		"class=flood&snapped=true", "class=flood&interval_s=abc&snapped=true", "class=flood&interval_s=0&snapped=true",
		"class=flood&interval_s=-3600&snapped=true", "class=flood&interval_s=1.5&snapped=true",
		"class=flood&interval_s=99999999999999999999&snapped=true",
		"class=flood&interval_s=10800", "class=flood&interval_s=10800&snapped=yes", "class=flood&interval_s=10800&snapped=1",
		ok + "&limit=0", ok + "&limit=-1", ok + "&limit=x", ok + "&offset=-1", ok + "&offset=x",
		ok + "&region=" + strings.Repeat("X", analyticsFilterMaxLen+1), ok + "&area=nope",
	} {
		aabGet(t, router, "?"+q, 400)
	}
	// An unsnapped estimate can lie above the longest setting (168 h): its
	// bucket is valid.
	aabGet(t, router, "?class=flood&interval_s=1080000&snapped=false", 200)
	aabGet(t, router, "?"+ok+"&area=fyn", 200)
}

// --- visibility -----------------------------------------------------------

// Every identity the instance hides at serve time is withheld: counted in
// withheld, absent from the raw response - key, snapshot name and alias.
// The snapshot only left out identities hidden by node name when its pass
// ran, so these all reach the bucket's rows.
func TestAdvertIntervalBucket_Visibility(t *testing.T) {
	const target = 20
	pk := aabKey(target)
	cases := []struct {
		name  string
		names []string // must not appear in the response
		setup func(t *testing.T, srv *Server)
	}{
		{"node blacklist", nil, func(t *testing.T, srv *Server) { srv.cfg.SetNodeBlacklist([]string{strings.ToUpper(pk)}) }},
		{"observer blacklist", nil, func(t *testing.T, srv *Server) { srv.cfg.ObserverBlacklist = []string{pk} }},
		{"node renamed into a hidden prefix", []string{"zz-renamed"}, func(t *testing.T, srv *Server) {
			srv.cfg.SetHiddenNamePrefixes([]string{"zz-"})
			mustExec(t, srv.db, `INSERT INTO nodes (public_key, name, role) VALUES (?, 'zz-renamed', 'repeater')`, pk)
		}},
		{"observer alias in another case (#286)", []string{"zz-alias"}, func(t *testing.T, srv *Server) {
			srv.cfg.SetHiddenNamePrefixes([]string{"zz-"})
			mustExec(t, srv.db, `INSERT INTO nodes (public_key, name, role) VALUES (?, 'Target Relay', 'repeater')`, pk)
			mustExec(t, srv.db, `INSERT INTO observers (id, name) VALUES (?, 'zz-alias')`, strings.ToUpper(pk))
		}},
		{"inactive node name", []string{"zz-gone"}, func(t *testing.T, srv *Server) {
			srv.cfg.SetHiddenNamePrefixes([]string{"zz-"})
			mustExec(t, srv.db, `CREATE TABLE IF NOT EXISTS inactive_nodes (public_key TEXT PRIMARY KEY, name TEXT)`)
			mustExec(t, srv.db, `INSERT INTO inactive_nodes (public_key, name) VALUES (?, 'zz-gone')`, pk)
		}},
		{"hidden snapshot name (fail closed)", nil, func(t *testing.T, srv *Server) {
			srv.cfg.SetHiddenNamePrefixes([]string{"Target"})
			mustExec(t, srv.db, `INSERT INTO nodes (public_key, name, role) VALUES (?, 'Renamed Out', 'repeater')`, pk)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, router := setupTestServer(t)
			c.setup(t, srv)
			rows := append(aabRows(), aabNamed(target, "Target Relay", []string{"CPH"}, aaEst(10800, advertConfidenceHigh, true), aaNone(advertIntervalNoneObserved)))
			aabPublish(t, srv, aabSnapshot(rows, aabAt), false)
			for _, q := range []string{"", "&region=CPH", "&limit=1", "&limit=1&offset=1"} {
				got, raw, _ := aabGet(t, router, "?class=flood&interval_s=10800&snapped=true"+q, 200)
				wantNodes := 4
				if q == "&region=CPH" {
					wantNodes = 3
				}
				if got.Nodes != wantNodes || got.Withheld != 1 || got.Visible != wantNodes-1 {
					t.Fatalf("%s: nodes %d visible %d withheld %d", q, got.Nodes, got.Visible, got.Withheld)
				}
				for _, s := range append([]string{pk, strings.ToUpper(pk), "Target Relay"}, c.names...) {
					if strings.Contains(raw, s) {
						t.Fatalf("%s: response contains %q: %s", q, s, raw)
					}
				}
			}
			// Paging runs over the visible members only.
			p1, _, _ := aabGet(t, router, "?class=flood&interval_s=10800&snapped=true&limit=2", 200)
			p2, _, _ := aabGet(t, router, "?class=flood&interval_s=10800&snapped=true&limit=2&offset=2", 200)
			if len(p1.Members) != 2 || len(p2.Members) != 1 {
				t.Fatalf("pages over visible members: %d + %d", len(p1.Members), len(p2.Members))
			}
		})
	}
}

// Without any hiding rule configured the bucket costs no identity query.
func TestAdvertIntervalBucket_NoRulesNoLookup(t *testing.T) {
	srv, router := setupTestServer(t)
	queries := 0
	srv.db.identityNamesQueryHook = func([]string) { queries++ }
	aabPublish(t, srv, aabSnapshot(aabRows(), aabAt), false)
	got, _, _ := aabGet(t, router, "?class=flood&interval_s=10800&snapped=true", 200)
	if got.Withheld != 0 || got.Visible != 3 || queries != 0 {
		t.Fatalf("withheld %d visible %d, %d identity queries", got.Withheld, got.Visible, queries)
	}
	// With a rule, one bulk query for the whole bucket - not one per member
	// or per page.
	srv.cfg.SetHiddenNamePrefixes([]string{"zz-"})
	aabGet(t, router, "?class=flood&interval_s=10800&snapped=true&limit=1", 200)
	if queries != 1 {
		t.Fatalf("%d identity queries for one bucket request, want 1", queries)
	}
}

// A failed identity lookup fails closed: 500 and no list.
func TestAdvertIntervalBucket_LookupFailsClosed(t *testing.T) {
	srv, router := setupTestServer(t)
	srv.cfg.SetHiddenNamePrefixes([]string{"zz-"})
	aabPublish(t, srv, aabSnapshot(aabRows(), aabAt), false)
	mustExec(t, srv.db, `ALTER TABLE observers RENAME TO observers_gone`)
	_, raw, _ := aabGet(t, router, "?class=flood&interval_s=10800&snapped=true", 500)
	if strings.Contains(raw, "members") || strings.Contains(raw, aabKey(1)) || strings.Contains(raw, "Relay") {
		t.Fatalf("500 leaks the list: %s", raw)
	}
}

// --- outliers (#896 §2.4) -------------------------------------------------

// aabOutlierServer is a server whose snapshot holds two outliers on the
// 2-minute zero-hop default: "Visible Relay", known as an observer too
// under the hidden alias "zz-alias" (the key in another case), and
// "Plain Relay". The snapshot's pass only checked node names, so both rows
// are in it.
func aabOutlierServer(t *testing.T) (*Server, http.Handler, string, string) {
	srv, router := setupTestServer(t)
	a := &aaDB{t, srv.db}
	narAddRouteMask(t, srv.db)
	const hidden = "896c0000000000000000000000000000000000000000000000000000000000a1"
	const plain = "896c0000000000000000000000000000000000000000000000000000000000a2"
	a.node(hidden, "Visible Relay", "repeater")
	a.node(plain, "Plain Relay", "repeater")
	mustExec(t, srv.db, `INSERT INTO observers (id, name, iata) VALUES ('o-896', 'Observer 896', 'CPH'), (?, 'zz-alias', NULL)`, strings.ToUpper(hidden))
	for i := 0; i < 8; i++ {
		a.advert(hidden, fmt.Sprint("oh", i), 2, aaZeroHop, aiBase.Add(time.Duration(i)*2*time.Minute), "o-896")
		a.advert(plain, fmt.Sprint("op", i), 2, aaZeroHop, aiBase.Add(time.Duration(i)*2*time.Minute+time.Hour), "o-896")
	}
	srv.store.InvalidateNodeCache()
	srv.cfg.SetHiddenNamePrefixes([]string{"zz-"})
	snap := srv.store.computeAdvertIntervalAnalytics()
	if len(snap.all.Outliers) != 2 {
		t.Fatalf("fixture: snapshot outliers %+v", snap.all.Outliers)
	}
	aabPublish(t, srv, snap, false)
	return srv, router, hidden, plain
}

// An outlier whose identity is hidden only through an observer alias is
// withheld from the outliers at serve time: the raw response holds neither
// its key nor its names, filtered or not; the counts stay as they are.
func TestAdvertIntervalAnalytics_OutliersWithholdHidden(t *testing.T) {
	_, router, hidden, plain := aabOutlierServer(t)
	for _, q := range []string{"", "?region=CPH"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/advert-intervals"+q, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", q, w.Code, w.Body.String())
		}
		raw := w.Body.String()
		for _, s := range []string{hidden, strings.ToUpper(hidden), "Visible Relay", "zz-alias"} {
			if strings.Contains(raw, s) {
				t.Fatalf("%s: response contains %q: %s", q, s, raw)
			}
		}
		var got AdvertIntervalAnalyticsResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Outliers) != 1 || got.Outliers[0].PublicKey != plain || got.OutliersWithheld != 1 || got.OutliersTotal != 2 {
			t.Fatalf("%s: outliers %+v withheld %d total %d", q, got.Outliers, got.OutliersWithheld, got.OutliersTotal)
		}
		if got.ZeroHop.Estimated != 2 {
			t.Fatalf("%s: histogram counts changed: %+v", q, got.ZeroHop)
		}
	}
}

// The outliers fail closed too, and the published snapshot is never edited.
func TestAdvertIntervalAnalytics_OutliersFailClosed(t *testing.T) {
	srv, router, hidden, _ := aabOutlierServer(t)
	mustExec(t, srv.db, `ALTER TABLE observers RENAME TO observers_gone`)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/advert-intervals", nil))
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), hidden) || strings.Contains(w.Body.String(), "Plain Relay") {
		t.Fatalf("lookup failure: %d %s", w.Code, w.Body.String())
	}
	snap, _ := srv.store.recomputer(&srv.store.recompAdvertIntervals).Peek().(*advertIntervalSnapshot)
	if snap == nil || len(snap.all.Outliers) != 2 || snap.all.OutliersWithheld != 0 {
		t.Fatalf("snapshot edited: %+v", snap)
	}
}

// Serving a filtered copy never edits the shared snapshot either: not its
// count, and not the rows in its backing array (filtering in place would
// overwrite the hidden row with the visible one).
func TestAdvertIntervalAnalytics_OutliersSnapshotUntouched(t *testing.T) {
	srv, router, hidden, plain := aabOutlierServer(t)
	for i := 0; i < 2; i++ {
		aaGet(t, router, "", 200)
	}
	snap, _ := srv.store.recomputer(&srv.store.recompAdvertIntervals).Peek().(*advertIntervalSnapshot)
	got := []string{}
	for _, o := range snap.all.Outliers {
		got = append(got, o.PublicKey)
	}
	if want := []string{hidden, plain}; !reflect.DeepEqual(got, want) || snap.all.OutliersWithheld != 0 {
		t.Fatalf("snapshot edited: outliers %v (want %v), withheld %d", got, want, snap.all.OutliersWithheld)
	}
}

// --- perf -----------------------------------------------------------------

// aabOneBucketRows is a 5000-node mesh with every node in the Flood 3 h
// bucket: the worst case for one bucket request.
func aabOneBucketRows(n int) []advertIntervalNodeRow {
	rows := aaPerfRows(n)
	for i := range rows {
		rows[i].pubkey = fmt.Sprintf("%064x", i+1)
		rows[i].name = fmt.Sprintf("Node %05d", (i*7919)%n)
		rows[i].flood = aaEst(10800, advertConfidenceHigh, true)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].pubkey < rows[j].pubkey })
	return rows
}

// Perf proof (AGENTS.md rule 0): membership, sort and one 200-row page of a
// 5000-member bucket, as the handler computes it after the identity
// lookup. Budget 25 ms (250 ms under -race), the filtered histogram's.
func TestAdvertIntervalBucket_Perf(t *testing.T) {
	budget := 25 * time.Millisecond
	if raceBuild {
		budget = 250 * time.Millisecond
	}
	rows := aabOneBucketRows(5000)
	q := advertIntervalBucketQuery{class: advertIntervalFlood, className: advertClassFlood,
		key: advertIntervalBucketKey{10800, true}, limit: advertIntervalBucketMaxLimit}
	start := time.Now()
	const n = 20
	var resp AdvertIntervalBucketResponse
	for i := 0; i < n; i++ {
		members := advertIntervalBucketMembers(rows, advertIntervalFilter("", nil), q.class, q.key)
		resp = advertIntervalBucketPage(members, nil, q, aabAt)
	}
	per := time.Since(start) / n
	t.Logf("bucket of 5000 nodes, 200-row page: %v per call", per)
	if resp.Nodes != 5000 || len(resp.Members) != 200 {
		t.Fatalf("nodes %d members %d", resp.Nodes, len(resp.Members))
	}
	if per > budget {
		t.Fatalf("bucket of 5000 nodes took %v per call, budget %v", per, budget)
	}
}

// BenchmarkAdvertIntervalBucket5000: "page" is membership + sort + one
// 50-row page; "request" is the whole request with a hidden-name prefix
// configured, so it includes the one bulk identity lookup of 5000 keys.
func BenchmarkAdvertIntervalBucket5000(b *testing.B) {
	rows := aabOneBucketRows(5000)
	b.Run("page", func(b *testing.B) {
		q := advertIntervalBucketQuery{class: advertIntervalFlood, className: advertClassFlood,
			key: advertIntervalBucketKey{10800, true}, limit: advertIntervalBucketDefaultLimit}
		keep := advertIntervalFilter("", nil)
		for i := 0; i < b.N; i++ {
			advertIntervalBucketPage(advertIntervalBucketMembers(rows, keep, q.class, q.key), nil, q, aabAt)
		}
	})
	b.Run("request", func(b *testing.B) {
		db := setupTestDB(b)
		tx, _ := db.conn.Begin()
		for _, r := range rows {
			tx.Exec(`INSERT INTO nodes (public_key, name, role) VALUES (?, ?, 'repeater')`, r.pubkey, r.name)
		}
		if err := tx.Commit(); err != nil {
			b.Fatal(err)
		}
		cfg := &Config{Port: 3000}
		cfg.SetHiddenNamePrefixes([]string{"zz-"})
		srv := NewServer(db, cfg, NewHub())
		srv.store = NewPacketStore(db, nil)
		srv.store.config = cfg
		aabPublish(b, srv, aabSnapshot(append([]advertIntervalNodeRow(nil), rows...), aabAt), false)
		req := httptest.NewRequest("GET", aabPath+"?class=flood&interval_s=10800&snapped=true", nil)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			w := httptest.NewRecorder()
			srv.handleAnalyticsAdvertIntervalBucket(w, req)
			if w.Code != 200 {
				b.Fatalf("%d %s", w.Code, w.Body.String())
			}
		}
	})
}

// --- docs ---------------------------------------------------------------

func TestOpenAPI_AdvertIntervalBucket(t *testing.T) {
	spec := fetchSpec(t)
	schemas := asMap(t, asMap(t, spec["components"], "components")["schemas"], "schemas")
	for name, v := range map[string]interface{}{
		"AdvertIntervalBucketResponse": AdvertIntervalBucketResponse{},
		"AdvertIntervalBucketMember":   AdvertIntervalBucketMember{},
		"AdvertIntervalBucketConflict": AdvertIntervalBucketConflict{},
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
	get := asMap(t, asMap(t, paths[aabPath], aabPath)["get"], "get")
	params := map[string]bool{}
	for _, p := range get["parameters"].([]interface{}) {
		params[p.(map[string]interface{})["name"].(string)] = true
	}
	for _, name := range []string{"class", "interval_s", "snapped", "region", "area", "offset", "limit", "snapshot"} {
		if !params[name] {
			t.Errorf("parameter %s is not documented", name)
		}
	}
	responses := asMap(t, get["responses"], "responses")
	for _, code := range []string{"200", "409", "500", "503"} {
		if _, ok := responses[code]; !ok {
			t.Errorf("response %s is not documented", code)
		}
	}
	raw, err := os.ReadFile("../../docs/api-spec.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"GET /api/analytics/advert-intervals/bucket", "outliers_withheld"} {
		if !strings.Contains(string(raw), s) {
			t.Errorf("docs/api-spec.md does not document %s", s)
		}
	}
}
