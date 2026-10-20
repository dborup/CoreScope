package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unsafe"
)

// #508: every Regions capture and publication budget was a single global
// count, and real data (48 h retention, ~805k observations) exhausted two of
// them: the 250,000 path-variant capture budget, which reported `capped`
// membership evidence for 17 of 20 hours in #dk and withheld every rate, and
// the 5,000-row directed-step cap. These tests describe the redesigned
// budgets through the collector's public behaviour only, so they run on the
// pre-#508 tree as well.
//
// Shared fixture note: routeTransmission uses scope "#sample" and a canonical
// transport-FLOOD frame, so every observation below is eligible evidence and
// anything reported as excluded/unprovable/capped is a budget effect.

func stepCells(snapshot *regionsRouteSnapshot) (cells, count int) {
	for _, step := range snapshot.steps {
		cells += len(step.Buckets)
		for _, bucket := range step.Buckets {
			count += bucket.Count
		}
	}
	return cells, count
}

// capturedEvidenceBucket returns the capped count attributed to one completed
// hour of the collector's own scope.
func capturedEvidenceBucket(t *testing.T, snapshot *regionsRouteSnapshot, index int) RegionsEvidenceCounts {
	t.Helper()
	if snapshot.memberships == nil || snapshot.memberships.evidence == nil {
		t.Fatal("snapshot published no membership evidence")
	}
	for _, scope := range snapshot.memberships.evidence.history.Scopes {
		if scope.Name != "#sample" {
			continue
		}
		for _, bucket := range scope.Buckets {
			if bucket.Index == index {
				return bucket.RegionsEvidenceCounts
			}
		}
	}
	return RegionsEvidenceCounts{}
}

// Point 1: the capture budget is per hourly bucket, so a 168 h retention is
// not capped. 262,080 retained path references spread evenly over the horizon
// exceed the old 250,000 global budget while staying far below any single
// hour's share of it.
func TestRegionsRoutePathCaptureBudgetIsPerHour(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 35, 0, 0, time.UTC)
	collector := newRegionsRoutesCollector(now)
	a := strings.Repeat("ab", 32)
	paths := make([]string, 100)
	for i := range paths {
		paths[i] = fmt.Sprintf(`["%s","%064x"]`, a, i+1)
	}
	const perHour = 520
	id := 0
	for hour := 0; hour < regionsHistoryBucketCount; hour++ {
		at := collector.start.Add(time.Duration(hour)*time.Hour + time.Minute)
		for i := 0; i < perHour; i++ {
			id++
			collector.capture(routeTransmission(id, at, paths[i%len(paths)]))
		}
	}
	got := collector.build(nil)
	if got.evidence.SourceTruncated || got.evidence.CappedObservations != 0 {
		t.Fatalf("168 h of evenly spread capture was truncated: truncated=%v capped=%d",
			got.evidence.SourceTruncated, got.evidence.CappedObservations)
	}
	if e := got.memberships.evidence.history; e.Capped != 0 || e.Unprovable != 0 || e.Truncated {
		t.Fatalf("capture budget withheld membership rates: %+v", e)
	}
	cells, count := stepCells(got)
	if len(got.steps) != len(paths) || cells != len(paths)*regionsHistoryBucketCount || count != perHour*regionsHistoryBucketCount {
		t.Fatalf("steps=%d cells=%d appearances=%d", len(got.steps), cells, count)
	}
	if got.stepsTruncated || got.bucketsTruncated || got.evidence.CappedStepAppearances != 0 {
		t.Fatalf("publication budget truncated a 168 h snapshot: %+v", got.evidence)
	}
}

// Point 1: one busy hour cannot starve the rest. 70,000 references in a single
// hour is above that hour's budget and far below the old global 250,000, so on
// the pre-#508 tree nothing was capped at all.
func TestRegionsRoutePathCaptureBudgetFairAcrossHours(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 35, 0, 0, time.UTC)
	collector := newRegionsRoutesCollector(now)
	a, b, c := strings.Repeat("ab", 32), fmt.Sprintf("%064x", 1), fmt.Sprintf("%064x", 2)
	const overfull = 70000
	for i := 0; i < overfull; i++ {
		collector.capture(routeTransmission(i+1, collector.start.Add(time.Minute), `["`+a+`","`+b+`"]`))
	}
	collector.capture(routeTransmission(overfull+1, collector.start.Add(time.Hour+time.Minute), `["`+a+`","`+c+`"]`))
	got := collector.build(nil)
	if !got.evidence.SourceTruncated {
		t.Fatal("a single hour was allowed to spend more than its capture budget")
	}
	if hot := capturedEvidenceBucket(t, got, 0); hot.Capped == 0 {
		t.Fatal("the overfull hour's omitted references were not reported as capped evidence")
	}
	if cold := capturedEvidenceBucket(t, got, 1); cold != (RegionsEvidenceCounts{}) {
		t.Fatalf("a busy hour qualified another hour's evidence: %+v", cold)
	}
	found := false
	for _, step := range got.steps {
		if step.To == c {
			found = len(step.Buckets) == 1 && step.Buckets[0].Index == 1 && step.Buckets[0].Count == 1
		}
	}
	if !found {
		t.Fatalf("the quiet hour lost its own evidence: %+v", got.steps)
	}
}

// Point 2: memberships are aggregated per (relay, scope, hour) instead of
// retaining one entry per (relay, transmission), so the 1,000,000-entry
// budget no longer scales with observations. 20,400 transmissions x 64 relays
// is 1,305,600 memberships in 1,536 cells.
func TestRegionsMembershipAggregatesPerRelayScopeHour(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 35, 0, 0, time.UTC)
	collector := newRegionsRoutesCollector(now)
	keys := make([]string, 64)
	for i := range keys {
		keys[i] = fmt.Sprintf("%064x", i+1)
	}
	path := `["` + strings.Join(keys, `","`) + `"]`
	const hours, perHour = 24, 850
	id := 0
	for hour := 0; hour < hours; hour++ {
		at := collector.start.Add(time.Duration(hour)*time.Hour + time.Minute)
		for i := 0; i < perHour; i++ {
			id++
			collector.capture(routeTransmission(id, at, path))
		}
	}
	got := collector.build(nil)
	if e := got.memberships.evidence.history; e.Capped != 0 || e.Unprovable != 0 || e.Truncated {
		t.Fatalf("1.3M memberships exhausted the membership budget: %+v", e)
	}
	if len(got.memberships.nodes) != len(keys) {
		t.Fatalf("retained %d of %d relay identities", len(got.memberships.nodes), len(keys))
	}
	history := got.history
	if len(history.nodes) != len(keys) || history.truncated || history.bucketsTruncated || history.scopesTruncated {
		t.Fatalf("history dropped aggregated memberships: nodes=%d %+v", len(history.nodes), history)
	}
	for _, key := range keys {
		scopes := history.nodes[key]
		if len(scopes) != 1 {
			t.Fatalf("%s retained %d scopes", key[:8], len(scopes))
		}
		if len(scopes[0].Buckets) != hours {
			t.Fatalf("%s retained %d hourly buckets, want %d", key[:8], len(scopes[0].Buckets), hours)
		}
		for _, bucket := range scopes[0].Buckets {
			if bucket.Count != perHour {
				t.Fatalf("%s bucket %d counted %d distinct transmissions, want %d", key[:8], bucket.Index, bucket.Count, perHour)
			}
		}
	}
}

// Point 3: directed steps are bounded by estimated published payload bytes
// instead of a fixed 5,000-row / 100,000-cell cap, both of which were reached
// on real data at 48 h retention.
func TestRegionsRouteStepsBudgetedByPayloadBytes(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 35, 0, 0, time.UTC)
	collector := newRegionsRoutesCollector(now)
	a := strings.Repeat("ab", 32)
	const rows = 6000
	at := collector.start.Add(500 * time.Hour)
	for i := 0; i < rows; i++ {
		collector.capture(routeTransmission(i+1, at, fmt.Sprintf(`["%s","%064x"]`, a, i+1)))
	}
	got := collector.build(nil)
	if len(got.steps) != rows || got.stepsTruncated || got.bucketsTruncated || got.evidence.CappedStepAppearances != 0 {
		t.Fatalf("%d of %d directed step rows published (truncated=%v capped=%d)",
			len(got.steps), rows, got.stepsTruncated, got.evidence.CappedStepAppearances)
	}

	// A cell-heavy snapshot still has a payload guarantee, now expressed in
	// bytes: 600 rows x 450 populated hours is 270,000 cells, above the byte
	// budget and far above the retired 100,000-cell cap.
	collector = newRegionsRoutesCollector(now)
	paths := make([]string, 600)
	for i := range paths {
		paths[i] = fmt.Sprintf(`["%s","%064x"]`, a, i+1)
	}
	id := 0
	for hour := 0; hour < 450; hour++ {
		when := collector.start.Add(time.Duration(hour)*time.Hour + time.Minute)
		for i := range paths {
			id++
			collector.capture(routeTransmission(id, when, paths[i]))
		}
	}
	got = collector.build(nil)
	cells, _ := stepCells(got)
	if cells < 200000 {
		t.Fatalf("published %d hourly step cells: the byte budget must admit more than the retired cell cap", cells)
	}
	// 180 bytes per published row and 75 per bucket, against a 16 MiB budget.
	if payload := len(got.steps)*180 + cells*75; payload > 16<<20 {
		t.Fatalf("estimated step payload %d bytes exceeds the published budget", payload)
	}
	if !got.stepsTruncated && !got.bucketsTruncated {
		t.Fatal("the payload budget was not enforced")
	}
	if got.evidence.CappedStepAppearances == 0 || !got.evidence.Incomplete {
		t.Fatalf("payload truncation was not disclosed: %+v", got.evidence)
	}
	// #507: directed-step caps are step appearances only.
	if e := got.memberships.evidence.history; e.Capped != 0 || e.Truncated {
		t.Fatalf("the step payload budget polluted membership coverage evidence: %+v", e)
	}
}

// Point 4: the retained route/membership snapshot is measurable, because
// publishing it raised staging RSS from ~1.55 to ~1.72 GiB and production
// hosts are smaller.
func TestPerfMemRegionsSnapshotMemory(t *testing.T) {
	srv, now, key := regionsObservedFixture(t)
	other := "cd" + strings.Repeat("34", 31)
	at := now.Truncate(time.Hour).Add(-time.Hour)
	srv.store.byPathHop[key] = []*StoreTx{routeTransmission(1, at, `["`+key+`","`+other+`"]`)}
	recomputeRepeaterEnrichmentSafe(srv.store, 24)

	w := httptest.NewRecorder()
	srv.handlePerf(w, httptest.NewRequest("GET", "/api/perf?mem=1", nil))
	if w.Code != 200 {
		t.Fatalf("perf mem=1 status %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		MemoryBreakdown *struct {
			TotalTx int `json:"totalTx"`
		} `json:"memoryBreakdown"`
		RegionsSnapshot *struct {
			Nodes                     int     `json:"nodes"`
			Scopes                    int     `json:"scopes"`
			HistoryNodes              int     `json:"historyNodes"`
			HistoryCells              int     `json:"historyCells"`
			Steps                     int     `json:"steps"`
			StepCells                 int     `json:"stepCells"`
			EvidenceCells             int     `json:"evidenceCells"`
			EvidenceBytes             int64   `json:"evidenceBytes"`
			MembershipsBytes          int64   `json:"membershipsBytes"`
			HistoryBytes              int64   `json:"historyBytes"`
			StepsBytes                int64   `json:"stepsBytes"`
			TotalBytes                int64   `json:"totalBytes"`
			TotalMB                   float64 `json:"totalMB"`
			EstimatedStepPayloadBytes int64   `json:"estimatedStepPayloadBytes"`
			GeneratedAt               string  `json:"generatedAt"`
		} `json:"regionsSnapshot"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.MemoryBreakdown == nil {
		t.Fatal("mem=1 returned no store memory breakdown")
	}
	r := body.RegionsSnapshot
	if r == nil {
		t.Fatal("mem=1 returned no retained Regions route/membership snapshot size")
	}
	if r.Nodes != 2 || r.Scopes != 2 || r.HistoryNodes != 2 || r.HistoryCells != 2 || r.Steps != 1 || r.StepCells != 1 {
		t.Fatalf("snapshot shape was not measured: %+v", *r)
	}
	if r.MembershipsBytes <= 0 || r.HistoryBytes <= 0 || r.StepsBytes <= 0 || r.EstimatedStepPayloadBytes <= 0 {
		t.Fatalf("retained sizes were not measured: %+v", *r)
	}
	if r.TotalBytes != r.MembershipsBytes+r.HistoryBytes+r.StepsBytes+r.EvidenceBytes || r.TotalMB < 0 {
		t.Fatalf("retained total does not account for its parts: %+v", *r)
	}
	if r.GeneratedAt == "" {
		t.Fatal("the measured snapshot is not identified by its generation time")
	}
}

// ---------------------------------------------------------------------------
// #510 round 2. The first round replaced the global 250,000-reference budget
// with a per-hour one, but left the interning table (131,072 entries) as a
// single global ceiling over the whole 504 h horizon: once full, every new
// path string was refused and reported as capped evidence in whichever hour
// the transmission fell in. Real path diversity is high — e2e-fixture.db, real
// captured traffic, holds 376 distinct path_json values in 399 path-carrying
// observations (94 %) — so ordinary diversity reached that ceiling before the
// per-hour budget did, which is the same starvation #508 reported at a lower
// ceiling. These tests pin the redesign: the table is a dedup cache that falls
// back to un-interned retention, and the only refusal left is per hour.

// shuffledDistinctPathTxs builds one transmission per distinct path, spread
// evenly over the last `hours` buckets, and shuffles them into the order
// production capture sees (s.byPathHop is walked by map iteration). Paths are
// distinct 5-hop chains drawn from a small relay pool, which is how real
// diversity looks: a few hundred relays, very many distinct chains.
func shuffledDistinctPathTxs(start time.Time, hours, perHour, relays int) []*StoreTx {
	keys := make([]string, relays)
	for i := range keys {
		keys[i] = fmt.Sprintf("%064x", i+1)
	}
	txs := make([]*StoreTx, 0, hours*perHour)
	id, n := 0, 0
	for hour := regionsHistoryBucketCount - hours; hour < regionsHistoryBucketCount; hour++ {
		at := start.Add(time.Duration(hour)*time.Hour + time.Minute)
		for i := 0; i < perHour; i++ {
			id, n = id+1, n+1
			hops := make([]string, 5)
			for h, d := 0, n; h < len(hops); h, d = h+1, d/relays {
				hops[h] = keys[d%relays]
			}
			txs = append(txs, routeTransmission(id, at, `["`+strings.Join(hops, `","`)+`"]`))
		}
	}
	rand.New(rand.NewSource(1)).Shuffle(len(txs), func(i, j int) { txs[i], txs[j] = txs[j], txs[i] })
	return txs
}

// F1/F2: 168 h at one distinct path per transmission must not be capped by any
// capture budget. 168,000 distinct chains is above the retired 131,072-entry
// interning table and is only 6 % of the rate real data showed (805k
// observations in 48 h is about 16,800 per hour), so this is a floor on real
// diversity, not a flood.
func TestRegionsRoutePathCaptureBudgetAtRealPathDiversity(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 35, 0, 0, time.UTC)
	collector := newRegionsRoutesCollector(now)
	const hours, perHour, relays = 168, 1000, 20
	for _, tx := range shuffledDistinctPathTxs(collector.start, hours, perHour, relays) {
		collector.capture(tx)
	}
	got := collector.build(nil)
	if got.evidence.SourceTruncated || got.evidence.CappedObservations != 0 {
		t.Fatalf("%d distinct paths over %d h were capped: truncated=%v capped=%d",
			hours*perHour, hours, got.evidence.SourceTruncated, got.evidence.CappedObservations)
	}
	if e := got.memberships.evidence.history; e.Capped != 0 || e.Unprovable != 0 || e.Truncated {
		t.Fatalf("path diversity withheld membership rates: %+v", e)
	}
	// Every one of the 168 hours keeps its own clean evidence: a diverse hour
	// must not qualify any other hour, whatever the capture order was.
	for hour := regionsHistoryBucketCount - hours; hour < regionsHistoryBucketCount; hour++ {
		if counts := capturedEvidenceBucket(t, got, hour); counts != (RegionsEvidenceCounts{}) {
			t.Fatalf("hour %d reported %+v, want clean evidence", hour, counts)
		}
	}
	if got.stepsTruncated || got.bucketsTruncated || got.evidence.CappedStepAppearances != 0 {
		t.Fatalf("publication budget truncated the snapshot: %+v", got.evidence)
	}
	if got.capture.PathVariants != hours*perHour || got.capture.PathReferences != hours*perHour {
		t.Fatalf("capture diagnostic lost the retained shape: %+v", got.capture)
	}
	if got.capture.MaxHourPathReferences != perHour {
		t.Fatalf("per-hour reference high-water mark was %d, want %d", got.capture.MaxHourPathReferences, perHour)
	}
	// Each hour's 1,000 chains cover the whole relay pool, so every relay keeps
	// one aggregated membership cell in each of the 168 hours.
	for key, scopes := range got.history.nodes {
		if len(scopes) != 1 || len(scopes[0].Buckets) != hours {
			t.Fatalf("%s retained %d scopes and %d hourly buckets, want 1 and %d",
				key[:8], len(scopes), len(scopes[0].Buckets), hours)
		}
	}
	if len(got.history.nodes) != relays {
		t.Fatalf("retained %d of %d relay identities", len(got.history.nodes), relays)
	}
}

// F1/F4 (M5): the interning table is a bounded dedup cache, not a budget. Past
// it, a path is retained un-interned — it costs a path slot and a reference
// instead of a map entry — and no observation is refused.
func TestRegionsRoutePathInterningFallsBackUnInterned(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 35, 0, 0, time.UTC)
	collector := newRegionsRoutesCollector(now)
	collector.internBound = 4
	a := strings.Repeat("ab", 32)
	const paths = 40
	at := collector.start.Add(500 * time.Hour)
	for i := 0; i < paths; i++ {
		collector.capture(routeTransmission(i+1, at, fmt.Sprintf(`["%s","%064x"]`, a, i+1)))
	}
	// Repeating one of the interned paths must still share its entry.
	collector.capture(routeTransmission(paths+1, at, fmt.Sprintf(`["%s","%064x"]`, a, 1)))
	got := collector.build(nil)
	if got.evidence.SourceTruncated || got.evidence.CappedObservations != 0 {
		t.Fatalf("a full interning table refused paths: truncated=%v capped=%d",
			got.evidence.SourceTruncated, got.evidence.CappedObservations)
	}
	if got.capture.PathVariantsInterned != collector.internBound {
		t.Fatalf("interned %d paths against a bound of %d", got.capture.PathVariantsInterned, collector.internBound)
	}
	if got.capture.PathVariants != paths || got.capture.PathReferences != paths+1 {
		t.Fatalf("un-interned retention lost paths: %+v", got.capture)
	}
	if len(got.steps) != paths {
		t.Fatalf("published %d of %d directed step rows", len(got.steps), paths)
	}
	for _, step := range got.steps {
		want := 1
		if step.To == fmt.Sprintf("%064x", 1) {
			want = 2
		}
		if len(step.Buckets) != 1 || step.Buckets[0].Count != want {
			t.Fatalf("%s..%s published %+v, want one bucket counting %d", step.From[:8], step.To[:8], step.Buckets, want)
		}
	}
}

// F2/F6: the per-hour reference budget must have headroom over the rate real
// data showed, so only a flood reaches it, and the cost model the disclosed
// transient ceiling rests on has to stay true. #508 reported about 805,000
// observations in 48 h of retention — about 16,800 per hour across all scopes
// — and every one of them could be transport-scoped with a distinct path.
func TestRegionsRouteCaptureBudgetHeadroomOverMeasuredRate(t *testing.T) {
	const measuredObservationsPerHour = 805000 / 48
	if maxRegionsRoutePathRefsPerHour < 2*measuredObservationsPerHour {
		t.Fatalf("per-hour capture budget %d leaves no headroom over the measured %d observations/hour",
			maxRegionsRoutePathRefsPerHour, measuredObservationsPerHour)
	}
	// The documented per-row capture cost: 8 bytes per retained reference, 24
	// for a retained path (header plus resolver slot) and 72 per retained
	// transmission, against the ~100 bytes per observation and ~1,000 bytes
	// per transmission the packet store already holds.
	if got := int64(unsafe.Sizeof(regionsRoutePathRef{})); got != regionsRoutePathRefBytes {
		t.Fatalf("a retained reference is %d bytes, documented as %d", got, regionsRoutePathRefBytes)
	}
	if got := int64(unsafe.Sizeof(regionsRouteTx{})); got != regionsRouteTxBytes {
		t.Fatalf("a retained transmission header is %d bytes, documented as %d", got, regionsRouteTxBytes)
	}
	slot := int64(unsafe.Sizeof("")) + int64(unsafe.Sizeof((*regionsResolvedPath)(nil)))
	if slot != regionsRoutePathSlotBytes {
		t.Fatalf("a retained path slot is %d bytes, documented as %d", slot, regionsRoutePathSlotBytes)
	}
	// The reference ceiling this implies stays a disclosed figure (#510 F6).
	refs := int64(maxRegionsRoutePathRefsPerHour) * (regionsHistoryBucketCount + 1)
	if bytes := refs * regionsRoutePathRefBytes; bytes > 320<<20 {
		t.Fatalf("transient reference ceiling grew to %d bytes; document or lower it", bytes)
	}
}

// F4 (M6): the published step-endpoint ceiling is enforced, and steps whose
// endpoints it omitted are disclosed instead of dropped silently.
func TestRegionsRouteStepEndpointCeilingDisclosed(t *testing.T) {
	srv, _, _, _ := regionsRoutesFixture(t)
	if full := getRegionsRoutes(t, srv); len(full.StepNodes) != 2 || len(full.Steps) != 1 || full.StepEvidence.MissingEndpointSteps != 0 {
		t.Fatalf("fixture did not publish both endpoints: %+v", full.StepEvidence)
	}
	srv.stepEndpointLimit = 1
	got := getRegionsRoutes(t, srv)
	if len(got.StepNodes) != 1 {
		t.Fatalf("published %d step endpoints against a ceiling of 1", len(got.StepNodes))
	}
	if len(got.Steps) != 0 || got.StepEvidence.MissingEndpointSteps != 1 || !got.StepEvidence.Incomplete {
		t.Fatalf("the endpoint ceiling was not disclosed: steps=%d %+v", len(got.Steps), got.StepEvidence)
	}
}

// F3: the capture budgets that decide `capped` must be observable before they
// cap, so the diagnostic reports their fill against their ceilings.
func TestRegionsSnapshotCaptureHeadroomMeasured(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 35, 0, 0, time.UTC)
	collector := newRegionsRoutesCollector(now)
	a := strings.Repeat("ab", 32)
	at := collector.start.Add(500 * time.Hour)
	for i := 0; i < 3; i++ {
		collector.capture(routeTransmission(i+1, at, fmt.Sprintf(`["%s","%064x"]`, a, i+1)))
	}
	snapshot := collector.build(nil)
	measured := measureRegionsSnapshot(&repeaterRelaySnapshot{generatedAt: now,
		regionMemberships: snapshot.memberships, history: snapshot.history, routes: snapshot})
	c := measured.Capture
	if c.PathVariants != 3 || c.PathVariantsInterned != 3 || c.PathReferences != 3 || c.Transmissions != 3 {
		t.Fatalf("capture fill was not reported: %+v", c)
	}
	if c.MaxHourPathReferences != 3 || c.HourPathReferenceBudget != maxRegionsRoutePathRefsPerHour {
		t.Fatalf("per-hour reference headroom was not reported: %+v", c)
	}
	if c.PathVariantInternBudget != maxRegionsRoutePathVariants {
		t.Fatalf("interning-table ceiling was not reported: %+v", c)
	}
	// Four relays in one hour and one scope: four aggregated membership cells.
	if c.MembershipCells != 4 || c.MembershipCellBudget != maxRegionsMembershipCells ||
		c.MembershipNodes != 4 || c.MembershipNodeBudget != maxRegionsMembershipNodes {
		t.Fatalf("membership headroom was not reported: %+v", c)
	}
	if c.EstimatedBytes <= 0 {
		t.Fatalf("transient capture cost was not estimated: %+v", c)
	}
}

// F5/F4 (M10): regionsSnapshot.truncated is budget truncation only. Unresolved
// hops and unprovable observations also set evidence.Incomplete, which is
// almost always true on real data, so it is reported separately.
func TestRegionsSnapshotTruncatedIsBudgetTruncationOnly(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 35, 0, 0, time.UTC)
	measure := func(snapshot *regionsRouteSnapshot) *RegionsSnapshotMemory {
		return measureRegionsSnapshot(&repeaterRelaySnapshot{generatedAt: now,
			regionMemberships: snapshot.memberships, history: snapshot.history, routes: snapshot})
	}
	a, b := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	// An unresolved hop token qualifies the evidence without spending any
	// budget: nothing was truncated.
	collector := newRegionsRoutesCollector(now)
	collector.capture(routeTransmission(1, collector.start.Add(500*time.Hour), `["`+a+`","BE","`+b+`"]`))
	incomplete := collector.build(nil)
	if !incomplete.evidence.Incomplete || incomplete.evidence.UnresolvedHops == 0 {
		t.Fatalf("fixture did not produce an unresolved hop: %+v", incomplete.evidence)
	}
	if got := measure(incomplete); got.Truncated || !got.EvidenceIncomplete {
		t.Fatalf("ordinary incompleteness was reported as budget truncation: truncated=%v evidenceIncomplete=%v",
			got.Truncated, got.EvidenceIncomplete)
	}
	// A spent publication budget is budget truncation.
	collector = newRegionsRoutesCollector(now)
	collector.stepBytes = regionsRouteStepRowBytes + regionsRouteStepCellBytes
	for i := 0; i < 3; i++ {
		collector.capture(routeTransmission(i+1, collector.start.Add(500*time.Hour), fmt.Sprintf(`["%s","%064x"]`, a, i+1)))
	}
	truncated := collector.build(nil)
	if !truncated.stepsTruncated {
		t.Fatal("fixture did not spend the publication budget")
	}
	if got := measure(truncated); !got.Truncated || !got.EvidenceIncomplete {
		t.Fatalf("publication truncation was not reported: truncated=%v evidenceIncomplete=%v",
			got.Truncated, got.EvidenceIncomplete)
	}
	// So is a spent capture budget, even though the published steps fit.
	collector = newRegionsRoutesCollector(now)
	collector.hourRefs[500] = maxRegionsRoutePathRefsPerHour
	collector.capture(routeTransmission(1, collector.start.Add(500*time.Hour), `["`+a+`","`+b+`"]`))
	collector.capture(routeTransmission(2, collector.start.Add(501*time.Hour), `["`+a+`","`+b+`"]`))
	capped := collector.build(nil)
	if !capped.evidence.SourceTruncated {
		t.Fatal("fixture did not spend the capture budget")
	}
	if got := measure(capped); !got.Truncated {
		t.Fatalf("capture truncation was not reported: %+v", got)
	}
}
