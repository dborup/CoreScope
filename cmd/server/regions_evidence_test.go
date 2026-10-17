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
	for _, tc := range []struct{ name, canonical, own string }{
		{"missing-own-header", "14aabbccdd00", ""},
		{"missing-own-codes", "14aabbccdd00", "14aa"},
		{"mismatch-first-code", "14aabbccdd00", "14aabcccdd00"},
		{"mismatch-second-code", "14aabbccdd00", "14aabbccde00"},
		{"missing-canonical-codes", "14aa", "14aabbccdd00"},
		{"zero-canonical-code", "140000ccdd00", "140000ccdd00"},
		{"unscoped-own", "14aabbccdd00", "1500"},
		{"direct-own", "14aabbccdd00", "17aabbccdd00"},
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
			wantIncomplete := tc.name != "direct-own" && tc.name != "unscoped-own"
			if history, latest := getRegionsHistory(t, srv), latestRegions(t, srv); history.Coverage.EvidenceIncomplete != wantIncomplete || latest.Coverage.EvidenceIncomplete != wantIncomplete {
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
	if len(history.Nodes) != 1 || history.Nodes[0].PublicKey != key || len(history.Nodes[0].Scopes) != 2 || !history.Coverage.EvidenceIncomplete {
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
	for i := 0; i < maxRegionsRouteSteps+1; i++ {
		key := fmt.Sprintf("%064x", i+1)
		collector.capture(routeTransmission(i+1, now.Add(-time.Hour), `["`+a+`","`+key+`"]`))
	}
	got := collector.build(nil)
	last := fmt.Sprintf("%064x", maxRegionsRouteSteps+1)
	if len(got.steps) != maxRegionsRouteSteps || !got.stepsTruncated || len(got.memberships.nodes[last].scopes) != 1 || got.memberships.incomplete {
		t.Fatal("directed-step cap suppressed independent valid membership or polluted its evidence flag")
	}
	b := newRegionsMembershipBuilder()
	b.remaining = 1
	b.add(a, regionsRouteTx{when: now, scope: "#one"})
	b.add(a, regionsRouteTx{when: now, scope: "#two"})
	bounded, _ := b.build(now, false)
	if !bounded.incomplete || len(b.entries[a]) != 1 || len(bounded.nodes[a].scopes) != 1 {
		t.Fatal("independent membership entry cap was silent")
	}
	b = newRegionsMembershipBuilder()
	for i := 0; i <= maxRegionsMembershipNodes; i++ {
		b.add(fmt.Sprintf("%064x", i+1), regionsRouteTx{when: now, scope: "#one"})
	}
	if len(b.entries) != maxRegionsMembershipNodes || !b.incomplete {
		t.Fatal("membership node scratch space is unbounded")
	}
}
