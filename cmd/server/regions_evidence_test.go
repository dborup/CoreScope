package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func latestRegions(t testing.TB, srv *Server) RegionsObservedResponse {
	t.Helper()
	w := httptest.NewRecorder()
	srv.handleRegionsObserved(w, httptest.NewRequest("GET", "/api/regions/observed?window=7d", nil))
	var out RegionsObservedResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("latest response %d: %s", w.Code, w.Body.String())
	}
	return out
}

func TestRegionsEvidenceRejectsPlannedPathsWithoutChangingGenericRelayInfo(t *testing.T) {
	srv, now, observed := regionsObservedFixture(t)
	planned := strings.Repeat("cd", 32)
	if _, err := srv.db.conn.Exec("INSERT INTO nodes(public_key,name,role) VALUES(?,'Planned only','repeater')", planned); err != nil {
		t.Fatal(err)
	}
	tx := routeTransmission(1, now.Truncate(time.Hour).Add(-time.Minute), `["`+observed+`"]`)
	tx.PathJSON = `["` + planned + `"]`
	tx.Observations = append(tx.Observations, routeObservation(tx.PathJSON, "17aabbccdd00"))
	srv.store.byPathHop[planned] = []*StoreTx{tx}
	srv.store.byPathHop[observed] = []*StoreTx{tx}
	wantGeneric := srv.store.GetRepeaterRelayInfo(planned, 24)
	recomputeRepeaterEnrichmentSafe(srv.store, 24)
	gotGeneric := srv.store.repeaterRelaySnapshot.info[planned]
	wantJSON, _ := json.Marshal(wantGeneric)
	gotJSON, _ := json.Marshal(gotGeneric)
	if string(wantJSON) != string(gotJSON) || gotGeneric.RelayCount24h != 1 {
		t.Fatalf("generic relay semantics changed: %s != %s", wantJSON, gotJSON)
	}
	history, latest := getRegionsHistory(t, srv), latestRegions(t, srv)
	if len(history.Nodes) != 1 || history.Nodes[0].PublicKey != observed || history.Nodes[0].Scopes[0].Buckets[0].Count != 1 {
		t.Fatalf("planned DIRECT path entered history: %+v", history.Nodes)
	}
	if len(latest.Nodes) != 1 || latest.Nodes[0].PublicKey != observed {
		t.Fatalf("planned DIRECT path entered Latest: %+v", latest.Nodes)
	}
}

func TestRegionsEvidenceOwnFloodSurvivesCanonicalDirectAndByNodeOnly(t *testing.T) {
	srv, now, key := regionsObservedFixture(t)
	tx := routeTransmission(1, now.Truncate(time.Hour).Add(-time.Minute), `["`+key+`"]`, `["`+key+`","`+key+`"]`)
	direct := 3
	tx.RouteType, tx.RawHex, tx.PathJSON = &direct, "17aabbccdd00", `["unknown-planned"]`
	srv.store.byNode[key] = []*StoreTx{tx, tx}
	before := srv.store.computeRepeaterRelayInfoMap(24)
	recomputeRepeaterEnrichmentSafe(srv.store, 24)
	if !reflect.DeepEqual(before, srv.store.repeaterRelaySnapshot.info) {
		t.Fatal("canonical direct changed generic relay map")
	}
	history, latest := getRegionsHistory(t, srv), latestRegions(t, srv)
	if len(history.Nodes) != 1 || len(latest.Nodes) != 1 || history.Nodes[0].Scopes[0].Buckets[0].Count != 1 {
		t.Fatalf("own singleton flood lost or loop/observation duplicated: history=%+v latest=%+v", history.Nodes, latest.Nodes)
	}
	if routes := getRegionsRoutes(t, srv); len(routes.Steps) != 0 {
		t.Fatal("singleton/loop membership invented a directed step")
	}
}

func TestRegionsEvidenceLatestIncludesCurrentHourButHistoryDoesNot(t *testing.T) {
	srv, now, key := regionsObservedFixture(t)
	when := now.Truncate(time.Hour)
	tx := routeTransmission(1, when, `["`+key+`"]`)
	srv.store.byPathHop[key] = []*StoreTx{tx}
	recomputeRepeaterEnrichmentSafe(srv.store, 24)
	if latest := latestRegions(t, srv); len(latest.Nodes) != 1 || latest.Nodes[0].Scopes[0].LastRelayed != when.Format(time.RFC3339Nano) {
		t.Fatalf("current hour missing from Latest: %+v", latest.Nodes)
	}
	if history := getRegionsHistory(t, srv); len(history.Nodes) != 0 {
		t.Fatal("unfinished current hour entered history")
	}
}

func TestRegionsEvidenceFailsClosedOnOwnWireAndCanonicalScopeMismatch(t *testing.T) {
	for _, tc := range []struct {
		name, canonical, own string
		unprovable           bool
	}{
		{"missing-own-header", "14aabbccdd00", "", true},
		{"missing-own-codes", "14aabbccdd00", "14aa", true},
		{"mismatch-first-code", "14aabbccdd00", "14aabcccdd00", false},
		{"mismatch-second-code", "14aabbccdd00", "14aabbccde00", false},
		{"missing-canonical-codes", "14aa", "14aabbccdd00", true},
		{"zero-canonical-code", "140000ccdd00", "140000ccdd00", true},
		{"unscoped-own", "14aabbccdd00", "1500", false},
		{"direct-own", "14aabbccdd00", "17aabbccdd00", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, now, key := regionsObservedFixture(t)
			tx := routeTransmission(1, now.Truncate(time.Hour).Add(-time.Minute))
			tx.RawHex, tx.PathJSON = tc.canonical, `["`+key+`"]`
			tx.Observations = []*StoreObs{routeObservation(tx.PathJSON, tc.own)}
			srv.store.byPathHop[key] = []*StoreTx{tx}
			recomputeRepeaterEnrichmentSafe(srv.store, 24)
			if history, latest := getRegionsHistory(t, srv), latestRegions(t, srv); len(history.Nodes) != 0 || len(latest.Nodes) != 0 {
				t.Fatalf("unverified observation entered Regions: history=%+v latest=%+v", history.Nodes, latest.Nodes)
			}
			want := RegionsEvidenceCounts{Excluded: 1}
			if tc.unprovable {
				want = RegionsEvidenceCounts{Unprovable: 1}
			}
			if history, latest := getRegionsHistory(t, srv), latestRegions(t, srv); history.Coverage.Evidence.RegionsEvidenceCounts != want || latest.Coverage.Evidence.RegionsEvidenceCounts != want {
				t.Fatalf("attribution completeness confused with exclusion: history=%+v latest=%+v", history.Coverage, latest.Coverage)
			}
		})
	}
}

func TestRegionsEvidencePrefixAmbiguityAndScopeDedup(t *testing.T) {
	srv, now, key := regionsObservedFixture(t)
	other := "ab" + strings.Repeat("34", 31)
	if _, err := srv.db.conn.Exec("INSERT INTO nodes(public_key,name,role) VALUES(?,'Colliding relay','repeater')", other); err != nil {
		t.Fatal(err)
	}
	for i, scope := range []string{"#one", "#two"} {
		tx := routeTransmission(i+1, now.Truncate(time.Hour).Add(-time.Minute), `["ab","`+key+`","`+key+`"]`, `["`+key+`"]`)
		tx.ScopeName = scope
		srv.store.byNode[key] = append(srv.store.byNode[key], tx, tx)
	}
	recomputeRepeaterEnrichmentSafe(srv.store, 24)
	history := getRegionsHistory(t, srv)
	// An ambiguous hop qualifies directed steps (stepEvidence.unresolvedHops),
	// not the provenance of resolved relays' memberships.
	if len(history.Nodes) != 1 || history.Nodes[0].PublicKey != key || len(history.Nodes[0].Scopes) != 2 || history.Coverage.Evidence.Unprovable != 0 {
		t.Fatalf("ambiguous prefix broadened membership: %+v", history)
	}
	for _, scope := range history.Nodes[0].Scopes {
		if len(scope.Buckets) != 1 || scope.Buckets[0].Count != 1 {
			t.Fatalf("TX/node/scope dedup failed: %+v", scope)
		}
	}
}

func TestRegionsEvidenceMembershipBudgetIndependentOfSteps(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 35, 0, 0, time.UTC)
	collector := newRegionsRoutesCollector(now)
	a := strings.Repeat("ab", 32)
	// One row plus its first bucket fits; the second row does not.
	collector.stepBytes = regionsRouteStepRowBytes + regionsRouteStepCellBytes
	for i := 0; i < 2; i++ {
		key := fmt.Sprintf("%064x", i+1)
		collector.capture(routeTransmission(i+1, now.Add(-time.Hour), `["`+a+`","`+key+`"]`))
	}
	got := collector.build(nil)
	last := fmt.Sprintf("%064x", 2)
	if len(got.steps) != 1 || !got.stepsTruncated || len(got.memberships.nodes[last].scopes) != 1 ||
		got.memberships.evidence.history.Capped != 0 || got.memberships.evidence.truncated || got.evidence.CappedStepAppearances != 1 {
		t.Fatal("directed-step payload budget suppressed independent valid membership or polluted its evidence")
	}
	b := newRegionsMembershipBuilder()
	b.remaining = 1
	if !b.add(a, "#one", 0, now) || b.add(a, "#two", 0, now) {
		t.Fatal("independent membership cell cap was silent")
	}
	// Aggregation: a further transmission in an admitted cell costs no budget.
	if !b.add(a, "#one", 0, now.Add(time.Minute)) {
		t.Fatal("an aggregated cell spent the membership budget twice")
	}
	if bounded, _ := b.build(now, nil); len(b.cells) != 1 || len(bounded.nodes[a].scopes) != 1 {
		t.Fatal("independent membership cell cap was not applied")
	}
	// The node cap is counted as capped evidence in the omitted membership's
	// own hour and scope only.
	collector = newRegionsRoutesCollector(now)
	for i := 0; i < maxRegionsMembershipNodes; i++ {
		collector.capture(routeTransmission(i+1, now.Add(-time.Hour), fmt.Sprintf(`["%064x"]`, i+1)))
	}
	collector.capture(routeTransmission(0, now.Add(-2*time.Hour), `["`+a+`"]`)) // Admitted first: lowest ID.
	got = collector.build(nil)
	evidence := got.memberships.evidence.history
	if len(got.memberships.nodes) != maxRegionsMembershipNodes || evidence.Capped != 1 || len(evidence.Scopes) != 1 ||
		!reflect.DeepEqual(evidence.Scopes[0].Buckets, []RegionsEvidenceBucket{{Index: regionsHistoryBucketCount - 1, RegionsEvidenceCounts: RegionsEvidenceCounts{Capped: 1}}}) ||
		got.evidence.CappedObservations != 0 {
		t.Fatalf("membership node cap must be capped evidence in its own bucket: %+v / %+v", evidence, got.evidence)
	}
}

// The JSON contract is decoded into test-local types so these assertions
// describe the public API independently of the server's Go structs.
type regionsEvidenceBucketJSON struct {
	Index                        int
	Excluded, Unprovable, Capped int
}

type regionsEvidenceScopeJSON struct {
	Name                         string
	Excluded, Unprovable, Capped int
	Buckets                      []regionsEvidenceBucketJSON
}

type regionsEvidenceJSON struct {
	Excluded, Unprovable, Capped int
	Truncated                    bool
	Scopes                       []regionsEvidenceScopeJSON
}

type regionsStepEvidenceJSON struct {
	ExcludedObservations, UnprovableObservations, CappedObservations int
	UnresolvedHops, CappedStepAppearances                            int
	SourceTruncated, Incomplete                                      bool
}

type regionsEvidenceBody struct {
	Coverage struct {
		Evidence *regionsEvidenceJSON
	}
	StepsTruncated bool
	StepEvidence   *regionsStepEvidenceJSON
}

func regionsEvidenceResponse(t testing.TB, srv *Server, target string) regionsEvidenceBody {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", target, nil)
	switch {
	case strings.HasPrefix(target, "/api/regions/observed"):
		srv.handleRegionsObserved(w, r)
	case strings.HasPrefix(target, "/api/regions/history"):
		srv.handleRegionsHistory(w, r)
	default:
		srv.handleRegionsRoutes(w, r)
	}
	var out regionsEvidenceBody
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("%s: %d %s", target, w.Code, w.Body.String())
	}
	if out.Coverage.Evidence == nil {
		t.Fatalf("%s: coverage.evidence missing: %s", target, w.Body.String())
	}
	return out
}

// F1 (PR #507 review): one unprovable observation anywhere must not make every
// hour and scope incomplete. A realistic mix of valid, excluded (known direct,
// unscoped, mismatched code), unprovable (unknown header/codes) and capped
// observations is attributed to the transmission's own hour and stored scope.
func TestRegionsEvidenceIncompletenessIsPerBucketAndScope(t *testing.T) {
	srv, now, key := regionsObservedFixture(t)
	hour := now.Truncate(time.Hour)
	at := func(hoursAgo int) time.Time { return hour.Add(-time.Duration(hoursAgo)*time.Hour + time.Minute) }
	index := func(hoursAgo int) int { return regionsHistoryBucketCount - hoursAgo }
	valid := `["` + key + `"]`
	mixed := routeTransmission(1, at(3), valid, valid)
	mixed.Observations = append(mixed.Observations,
		routeObservation(valid, "17aabbccdd00"), // known TRANSPORT_DIRECT: planned path
		routeObservation(valid, "1500"),         // known unscoped FLOOD
		routeObservation(valid, "14aabcccdd00")) // own scope code differs from the stored frame
	unknown := routeTransmission(2, at(2), valid)
	unknown.Observations = append(unknown.Observations, routeObservation(valid, ""))
	other := routeTransmission(3, at(3), valid)
	other.ScopeName = "#other"
	other.Observations = append(other.Observations, routeObservation(valid, "14aa"))
	oversize := routeTransmission(4, at(1), valid)
	oversize.Observations = append(oversize.Observations, routeObservation(`["`+strings.Repeat("a", maxRegionsRoutePathBytes)+`"]`, "14aabbccdd00"))
	srv.store.byPathHop[key] = []*StoreTx{mixed, unknown, other, oversize}
	recomputeRepeaterEnrichmentSafe(srv.store, 24)

	history := getRegionsHistory(t, srv)
	if len(history.Nodes) != 1 || len(history.Nodes[0].Scopes) != 2 {
		t.Fatalf("valid observations must still count beside excluded/unprovable ones: %+v", history.Nodes)
	}
	want := regionsEvidenceJSON{Excluded: 3, Unprovable: 2, Capped: 1, Scopes: []regionsEvidenceScopeJSON{
		{Name: "#other", Unprovable: 1, Buckets: []regionsEvidenceBucketJSON{{Index: index(3), Unprovable: 1}}},
		{Name: "#sample", Excluded: 3, Unprovable: 1, Capped: 1, Buckets: []regionsEvidenceBucketJSON{
			{Index: index(3), Excluded: 3}, {Index: index(2), Unprovable: 1}, {Index: index(1), Capped: 1}}},
	}}
	for _, target := range []string{"/api/regions/history", "/api/regions/routes"} {
		if got := *regionsEvidenceResponse(t, srv, target).Coverage.Evidence; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s evidence is not per bucket/scope:\n got %+v\nwant %+v", target, got, want)
		}
	}
	// Latest summarises the requested rolling window, without bucket detail.
	latest := *regionsEvidenceResponse(t, srv, "/api/regions/observed?window=7d").Coverage.Evidence
	wantLatest := regionsEvidenceJSON{Excluded: 3, Unprovable: 2, Capped: 1, Scopes: []regionsEvidenceScopeJSON{
		{Name: "#other", Unprovable: 1}, {Name: "#sample", Excluded: 3, Unprovable: 1, Capped: 1}}}
	if !reflect.DeepEqual(latest, wantLatest) {
		t.Fatalf("7d Latest evidence:\n got %+v\nwant %+v", latest, wantLatest)
	}
	lastHour := *regionsEvidenceResponse(t, srv, "/api/regions/observed?window=1h").Coverage.Evidence
	if want := (regionsEvidenceJSON{Capped: 1, Scopes: []regionsEvidenceScopeJSON{{Name: "#sample", Capped: 1}}}); !reflect.DeepEqual(lastHour, want) {
		t.Fatalf("1h Latest evidence must exclude older hours:\n got %+v\nwant %+v", lastHour, want)
	}
	steps := regionsEvidenceResponse(t, srv, "/api/regions/routes").StepEvidence
	if steps == nil || steps.ExcludedObservations != 3 || steps.UnprovableObservations != 2 || steps.CappedObservations != 1 {
		t.Fatalf("step evidence must split excluded/unprovable/capped: %+v", steps)
	}
}

// Only known non-eligible observations: nothing is incomplete.
func TestRegionsEvidenceExcludedOnlyIsComplete(t *testing.T) {
	srv, now, key := regionsObservedFixture(t)
	valid := `["` + key + `"]`
	tx := routeTransmission(1, now.Truncate(time.Hour).Add(-time.Minute), valid)
	tx.Observations = append(tx.Observations, routeObservation(valid, "17aabbccdd00"), routeObservation(valid, "14aabbccde00"))
	srv.store.byPathHop[key] = []*StoreTx{tx}
	recomputeRepeaterEnrichmentSafe(srv.store, 24)
	got := *regionsEvidenceResponse(t, srv, "/api/regions/history").Coverage.Evidence
	if got.Excluded != 2 || got.Unprovable != 0 || got.Capped != 0 || got.Truncated {
		t.Fatalf("identified exclusions were reported as incomplete: %+v", got)
	}
}

// F3 (PR #507 review): the #508 directed-step cap is counted as capped step
// appearances in stepEvidence, separately from observation-level evidence, and
// never makes membership/history coverage incomplete.
func TestRegionsEvidenceStepCapCountedSeparately(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 35, 0, 0, time.UTC)
	a := strings.Repeat("ab", 32)
	collector := newRegionsRoutesCollector(now)
	// Budget for one row and its first bucket only, so a second directed step
	// is omitted by the published payload budget rather than by evidence.
	collector.stepBytes = regionsRouteStepRowBytes + regionsRouteStepCellBytes
	for i := 0; i < 2; i++ {
		collector.capture(routeTransmission(i+1, now.Add(-time.Hour), fmt.Sprintf(`["%s","%064x"]`, a, i+1)))
	}
	got := collector.build(nil)
	if e := got.memberships.evidence.history; e.Excluded != 0 || e.Unprovable != 0 || e.Capped != 0 || e.Truncated || len(e.Scopes) != 0 {
		t.Fatalf("directed-step payload budget leaked into membership coverage evidence: %+v", e)
	}
	steps := got.evidence
	if !got.stepsTruncated || steps.CappedStepAppearances != 1 || steps.CappedObservations != 0 || steps.UnprovableObservations != 0 || !steps.Incomplete {
		t.Fatalf("the step payload budget must be counted as capped step appearances only: truncated=%v %+v", got.stepsTruncated, steps)
	}
	// Both relays keep their membership; only the publication was truncated.
	if len(got.memberships.nodes) != 3 || len(got.history.nodes) != 3 {
		t.Fatalf("payload truncation dropped memberships: %d/%d", len(got.memberships.nodes), len(got.history.nodes))
	}
}

// F2 (PR #507 review): a relay appearance is one transmission per distinct
// relay on ANY observer's own attributable flood path (the union across
// observers), not only the canonical display path. Repeated observers,
// identical paths and loops count once.
func TestRegionsEvidenceAppearancesUnionAcrossObservers(t *testing.T) {
	srv, now, a := regionsObservedFixture(t)
	b, c, d := "b1"+strings.Repeat("12", 31), "c1"+strings.Repeat("12", 31), "d1"+strings.Repeat("12", 31)
	for _, key := range []string{b, c, d} {
		if _, err := srv.db.conn.Exec("INSERT INTO nodes(public_key,name,role) VALUES(?,'Relay','repeater')", key); err != nil {
			t.Fatal(err)
		}
	}
	display := `["` + a + `","` + b + `"]`
	tx := routeTransmission(1, now.Truncate(time.Hour).Add(-time.Minute), display, `["`+c+`","`+d+`"]`, display, `["`+b+`","`+a+`","`+b+`"]`)
	tx.PathJSON = display
	srv.store.byPathHop[a] = []*StoreTx{tx}
	srv.store.byPathHop[b] = []*StoreTx{tx}
	recomputeRepeaterEnrichmentSafe(srv.store, 24)
	history := getRegionsHistory(t, srv)
	got := map[string]int{}
	total := 0
	for _, node := range history.Nodes {
		for _, scope := range node.Scopes {
			for _, bucket := range scope.Buckets {
				got[node.PublicKey] += bucket.Count
				total += bucket.Count
			}
		}
	}
	if want := map[string]int{a: 1, b: 1, c: 1, d: 1}; !reflect.DeepEqual(got, want) || total != 4 {
		t.Fatalf("relay appearances must be the union across observers, once per relay: got %v (total %d)", got, total)
	}
}

// Worst-case evidence publication: every scope x hour cell of the budget.
func BenchmarkRegionsEvidenceCapPayload(b *testing.B) {
	builder := newRegionsEvidenceBuilder()
	for i := 0; !builder.truncated; i++ {
		builder.add(fmt.Sprintf("#scope%03d", i/(regionsHistoryBucketCount+1)), i%(regionsHistoryBucketCount+1), RegionsEvidenceCounts{Excluded: 12345, Unprovable: 1, Capped: 1})
	}
	snapshot := builder.build(time.Now().UTC().Truncate(time.Hour).Add(-regionsHistoryBucketCount * time.Hour))
	encoded, err := json.Marshal(snapshot.history)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = snapshot.window(snapshot.start, snapshot.start.Add(regionsHistoryBucketCount*time.Hour))
	}
	b.ReportMetric(float64(len(encoded)), "payload-bytes")
	b.ReportMetric(float64(len(builder.cells)), "cells")
}
