package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

// Point 1: one busy hour cannot starve the rest. 20,000 references in a single
// hour is above that hour's budget and far below the old global 250,000, so on
// the pre-#508 tree nothing was capped at all.
func TestRegionsRoutePathCaptureBudgetFairAcrossHours(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 35, 0, 0, time.UTC)
	collector := newRegionsRoutesCollector(now)
	a, b, c := strings.Repeat("ab", 32), fmt.Sprintf("%064x", 1), fmt.Sprintf("%064x", 2)
	const overfull = 20000
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
