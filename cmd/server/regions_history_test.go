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

// historyCell builds one aggregated membership cell the way the Regions
// collector does (#508): the hourly bucket index is derived from the
// transmission time, and a time before the horizon is out of range rather
// than truncated into bucket 0.
func historyCell(b *regionsHistoryBuilder, at time.Time, scope string) regionsMembershipNodeCell {
	hour := int32(at.Sub(b.history.start) / time.Hour)
	if at.Before(b.history.start) {
		hour = -1
	}
	return regionsMembershipNodeCell{scope: scope, hour: hour, count: 1, last: at}
}

func TestRegionsHistoryBucketsAndScopeProvenance(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 35, 0, 0, time.UTC)
	end := now.Truncate(time.Hour)
	start := end.Add(-21 * 24 * time.Hour)
	// Advert/TRACE payloads, planned direct paths, prefix-only evidence and
	// unparsable timestamps are excluded before capture and can no longer
	// reach this builder; TestRegionRoutesStrictObservationScopeAndBounds
	// pins that gate. What remains here is the horizon and the scope name.
	b := newRegionsHistoryBuilder(now)
	cells := []regionsMembershipNodeCell{historyCell(b, start, "#sample"), historyCell(b, end.Add(-time.Nanosecond), "#sample"),
		historyCell(b, end.Add(-time.Hour), "#sample"), historyCell(b, start.Add(-time.Nanosecond), "#old"),
		historyCell(b, end, "#unfinished"), historyCell(b, now.Add(time.Hour), "#future"),
		historyCell(b, end.Add(-time.Minute), "invalid")}
	b.add(strings.Repeat("ab", 32), cells)
	h := b.history
	if !h.start.Equal(start) || !h.end.Equal(end) || len(h.nodes) != 1 {
		t.Fatalf("history bounds/nodes: %+v", h)
	}
	scopes := h.nodes[strings.Repeat("ab", 32)]
	if len(scopes) != 1 || scopes[0].Name != "#sample" || len(scopes[0].Buckets) != 2 {
		t.Fatalf("scope provenance/bounds: %+v", scopes)
	}
	if scopes[0].Buckets[0].Index != 0 || scopes[0].Buckets[0].Count != 1 || scopes[0].Buckets[1].Index != 503 || scopes[0].Buckets[1].Count != 2 {
		t.Fatalf("bucket boundary counts: %+v", scopes[0].Buckets)
	}
	if scopes[0].Buckets[1].LastRelayed != end.Add(-time.Nanosecond).Format(time.RFC3339Nano) {
		t.Fatal("latest evidence was lost")
	}
}

func TestRegionsHistoryAdjacentSevenDayWindows(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 35, 0, 0, time.UTC)
	end := now.Truncate(time.Hour)
	b := newRegionsHistoryBuilder(now)
	cells := []regionsMembershipNodeCell{historyCell(b, end.Add(-14*24*time.Hour), "#sample"),
		historyCell(b, end.Add(-7*24*time.Hour), "#sample"), historyCell(b, end.Add(-time.Second), "#sample")}
	key := strings.Repeat("ab", 32)
	b.add(key, cells)
	previous, current := 0, 0
	for _, bucket := range b.history.nodes[key][0].Buckets {
		if bucket.Index >= 168 && bucket.Index < 336 {
			previous += bucket.Count
		}
		if bucket.Index >= 336 && bucket.Index < 504 {
			current += bucket.Count
		}
	}
	if previous != 1 || current != 2 {
		t.Fatalf("adjacent windows overlap or lose boundary: %d %d", previous, current)
	}
}

func TestRegionsHistoryBoundedScopesAndCells(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 35, 0, 0, time.UTC)
	b := newRegionsHistoryBuilder(now)
	b.remainingCells = 2
	var cells []regionsMembershipNodeCell
	for i := 0; i < 40; i++ {
		cells = append(cells, historyCell(b, b.history.end.Add(-time.Duration(i+1)*time.Hour), fmt.Sprintf("#s%02d", i)))
	}
	key := strings.Repeat("ab", 32)
	b.add(key, cells)
	if !b.history.scopesTruncated || !b.history.bucketsTruncated || len(b.history.nodes[key]) > 32 || b.remainingCells != 0 {
		t.Fatalf("bounds/flags: %+v", b.history)
	}
	for _, scope := range b.history.nodes[key] {
		if scope.Name == "#s39" {
			t.Fatal("old scope displaced recent scope")
		}
	}
	reversed := append([]regionsMembershipNodeCell(nil), cells...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	other := newRegionsHistoryBuilder(now)
	other.remainingCells = 2
	other.add(key, reversed)
	if !reflect.DeepEqual(b.history.nodes, other.history.nodes) {
		t.Fatal("cell budget depends on entry order")
	}
}

func getRegionsHistory(t testing.TB, srv *Server) RegionsHistoryResponse {
	t.Helper()
	w := httptest.NewRecorder()
	srv.handleRegionsHistory(w, httptest.NewRequest("GET", "/api/regions/history", nil))
	var got RegionsHistoryResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("history response %d: %s", w.Code, w.Body.String())
	}
	return got
}

func TestRegionsHistoryDedupCoverageAndWarmLock(t *testing.T) {
	srv, now, key := regionsObservedFixture(t)
	tx := routeTransmission(5, now.Add(-2*time.Hour), `["`+key+`","`+key+`"]`)
	tx.PathJSON = `["` + key + `","` + key + `"]`
	srv.store.byPathHop[key] = []*StoreTx{tx, tx}
	srv.store.byNode[key] = []*StoreTx{tx}
	recomputeRepeaterEnrichmentSafe(srv.store, 24)
	got := getRegionsHistory(t, srv)
	if len(got.Nodes) != 1 || len(got.Nodes[0].Scopes) != 1 || got.Nodes[0].Scopes[0].Buckets[0].Count != 1 {
		t.Fatalf("duplicate transmission counted: %+v", got.Nodes)
	}
	if got.Coverage.Start == nil || got.Coverage.Loading || got.HistoryEnd == got.GeneratedAt || got.BucketCount != 504 {
		t.Fatalf("paired metadata: %+v", got)
	}
	srv.store.mu.Lock()
	srv.store.oldestLoaded = now.Format(time.RFC3339)
	srv.store.backgroundLoadDone.Store(false)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		srv.handleRegionsHistory(w, httptest.NewRequest("GET", "/api/regions/history", nil))
		done <- w
	}()
	select {
	case w := <-done:
		if w.Code != 200 {
			t.Errorf("warm response: %d", w.Code)
		}
	case <-time.After(2 * time.Second):
		srv.store.mu.Unlock()
		t.Fatal("warm history request waits for packet lock")
	}
	srv.store.mu.Unlock()
	warm := getRegionsHistory(t, srv)
	if warm.Coverage.Start == nil || *warm.Coverage.Start != *got.Coverage.Start || warm.Coverage.Loading {
		t.Fatal("coverage changed independently of cached evidence")
	}
}

func TestRegionsHistoryArchivedMetadataAndVisibility(t *testing.T) {
	srv, now, key := regionsObservedFixture(t)
	seedRegionRelaySnapshot(t, srv, now, key)
	if _, err := srv.db.conn.Exec(`CREATE TABLE inactive_nodes(public_key TEXT PRIMARY KEY, name TEXT, role TEXT, lat REAL, lon REAL, foreign_advert INTEGER);
		INSERT INTO inactive_nodes SELECT public_key, 'Archived relay', role, lat, lon, foreign_advert FROM nodes`); err != nil {
		t.Fatal(err)
	}
	got := getRegionsHistory(t, srv)
	if len(got.Nodes) != 1 || got.Nodes[0].MetadataSource != "current" || got.Nodes[0].Name != "Synthetic relay" {
		t.Fatalf("current precedence: %+v", got.Nodes)
	}
	if _, err := srv.db.conn.Exec("UPDATE nodes SET role='companion'"); err != nil {
		t.Fatal(err)
	}
	if got := getRegionsHistory(t, srv); len(got.Nodes) != 0 {
		t.Fatal("archived repeater resurrected current companion")
	}
	if _, err := srv.db.conn.Exec("DELETE FROM nodes"); err != nil {
		t.Fatal(err)
	}
	got = getRegionsHistory(t, srv)
	if len(got.Nodes) != 1 || got.Nodes[0].MetadataSource != "archived" || got.Nodes[0].Name != "Archived relay" {
		t.Fatalf("archive fallback: %+v", got.Nodes)
	}
	srv.cfg.SetHiddenNamePrefixes([]string{"Hidden"})
	if _, err := srv.db.conn.Exec("INSERT INTO observers(id,name) VALUES(?, 'Hidden observer alias')", strings.ToUpper(key)); err != nil {
		t.Fatal(err)
	}
	if got := getRegionsHistory(t, srv); len(got.Nodes) != 0 {
		t.Fatal("archived identity observer alias leaked")
	}
	srv.cfg.SetHiddenNamePrefixes(nil)
	srv.cfg.SetNodeBlacklist([]string{key})
	if got := getRegionsHistory(t, srv); len(got.Nodes) != 0 {
		t.Fatal("warm snapshot blacklist leaked")
	}
	srv.cfg.SetNodeBlacklist(nil)
	srv.cfg.ObserverBlacklist = []string{key}
	if got := getRegionsHistory(t, srv); len(got.Nodes) != 0 {
		t.Fatal("observer blacklist leaked")
	}
	srv.cfg.ObserverBlacklist = nil
	srv.setGeoFilter(&GeoFilterConfig{Polygon: [][2]float64{{51, 4}, {51, 5}, {50, 5}, {50, 4}}})
	if got := getRegionsHistory(t, srv); len(got.Nodes) != 0 {
		t.Fatal("archive bypassed geographic filter")
	}
	if _, err := srv.db.conn.Exec("UPDATE inactive_nodes SET foreign_advert=1"); err != nil {
		t.Fatal(err)
	}
	if got := getRegionsHistory(t, srv); len(got.Nodes) != 1 {
		t.Fatal("archive lost foreign-node exception")
	}
	srv.setGeoFilter(nil)
	if _, err := srv.db.conn.Exec("UPDATE inactive_nodes SET lat=NULL,lon=NULL"); err != nil {
		t.Fatal(err)
	}
	got = getRegionsHistory(t, srv)
	if len(got.Nodes) != 1 || got.Nodes[0].Lat != nil || got.Nodes[0].PositionSource != "none" {
		t.Fatal("unpositioned archived relay lost")
	}
}

func TestRegionsHistoryNodeCapAndAvailability(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 35, 0, 0, time.UTC)
	b := newRegionsHistoryBuilder(now)
	cells := []regionsMembershipNodeCell{historyCell(b, b.history.end.Add(-time.Minute), "#sample")}
	for i := 0; i <= maxObservedRegionNodes; i++ {
		b.add(fmt.Sprintf("%064x", i+1), cells)
	}
	if len(b.history.nodes) != maxObservedRegionNodes || !b.history.truncated {
		t.Fatal("snapshot node bound not enforced")
	}
	srv, _, _ := regionsObservedFixture(t)
	w := httptest.NewRecorder()
	srv.handleRegionsHistory(w, httptest.NewRequest("GET", "/api/regions/history", nil))
	if w.Code != 503 || srv.store.repeaterRelayCache != nil {
		t.Fatal("warming history computed inline")
	}
	seedRegionRelaySnapshot(t, srv, now, strings.Repeat("ab", 32))
	if _, err := srv.db.conn.Exec("DROP TABLE nodes"); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	srv.handleRegionsHistory(w, httptest.NewRequest("GET", "/api/regions/history", nil))
	if w.Code != 500 {
		t.Fatalf("metadata error became empty success: %d", w.Code)
	}
}

func TestRegionsHistoryActualGlobalCellBudget(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 35, 0, 0, time.UTC)
	b := newRegionsHistoryBuilder(now)
	hours := make([]regionsMembershipNodeCell, 501)
	for i := range hours {
		hours[i] = historyCell(b, b.history.start.Add(time.Duration(i)*time.Hour), "#sample")
	}
	for i := 0; i < 500; i++ {
		b.add(fmt.Sprintf("%064x", i+1), hours)
	}
	cells := 0
	for _, scopes := range b.history.nodes {
		for _, scope := range scopes {
			cells += len(scope.Buckets)
		}
	}
	if cells != maxRegionsHistoryCells || b.remainingCells != 0 || !b.history.bucketsTruncated {
		t.Fatalf("global budget: cells=%d remaining=%d truncated=%v", cells, b.remainingCells, b.history.bucketsTruncated)
	}
}

func TestOpenAPIRegionsHistorySchema(t *testing.T) {
	meta := routeDescriptions()["GET /api/regions/history"]
	if meta.Response["$ref"] != "#/components/schemas/RegionsHistoryResponse" {
		t.Fatal("history response schema missing")
	}
	schemas := componentSchemas()
	response := schemas["RegionsHistoryResponse"].(*openAPISchema)
	if response.Properties["nodes"].Items.Ref != "#/components/schemas/RegionsHistoryNode" || response.Properties["bucketCount"].Type != "integer" {
		t.Fatal("typed history contract missing")
	}
	node := schemas["RegionsHistoryNode"].(*openAPISchema)
	if !node.Properties["lat"].Nullable || len(node.Properties["metadataSource"].Enum) != 2 {
		t.Fatal("metadata uncertainty missing")
	}
	if !schemas["RegionsHistoryCoverage"].(*openAPISchema).Properties["start"].Nullable {
		t.Fatal("unknown coverage missing")
	}
}

func regionsHistoryBenchmarkFixture(b *testing.B) (*Server, time.Time) {
	srv, now, _ := regionsObservedFixture(b)
	end := now.Truncate(time.Hour)
	transaction, err := srv.db.conn.Begin()
	if err != nil {
		b.Fatal(err)
	}
	stmt, err := transaction.Prepare("INSERT INTO nodes(public_key,name,role,lat,lon) VALUES(?,'Synthetic history relay','repeater',55.2,10.3)")
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		key := fmt.Sprintf("%064x", i+1)
		if _, err := stmt.Exec(key); err != nil {
			b.Fatal(err)
		}
		for j := 0; j < 60; j++ {
			tx := routeTransmission(i*60+j+1, end.Add(-time.Duration(j*8+1)*time.Hour), `["`+key+`"]`)
			tx.PathJSON = `["` + key + `"]`
			srv.store.byPathHop[key] = append(srv.store.byPathHop[key], tx)
		}
	}
	stmt.Close()
	if err := transaction.Commit(); err != nil {
		b.Fatal(err)
	}
	return srv, now
}

func BenchmarkRegionsHistoryCompute30K(b *testing.B) {
	srv, _ := regionsHistoryBenchmarkFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = srv.store.computeRepeaterRelaySnapshot(24)
	}
}

func BenchmarkRegionsHistoryWarm30K(b *testing.B) {
	srv, _ := regionsHistoryBenchmarkFixture(b)
	recomputeRepeaterEnrichmentSafe(srv.store, 24)
	srv.store.byPathHop = nil
	response := getRegionsHistory(b, srv)
	encoded, err := json.Marshal(response)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		srv.handleRegionsHistory(w, httptest.NewRequest("GET", "/api/regions/history", nil))
		if w.Code != 200 {
			b.Fatal(w.Code)
		}
	}
	b.ReportMetric(float64(len(encoded)), "payload-bytes")
}

func BenchmarkRegionsHistoryCapPayload(b *testing.B) {
	response := RegionsHistoryResponse{Nodes: []RegionsHistoryNode{}}
	for i := 0; i < 500; i++ {
		buckets := make([]RegionsHistoryBucket, 500)
		for j := range buckets {
			buckets[j] = RegionsHistoryBucket{Index: j, Count: 12345, LastRelayed: "2026-10-16T11:59:59.123456789Z"}
		}
		response.Nodes = append(response.Nodes, RegionsHistoryNode{PublicKey: fmt.Sprintf("%064x", i), Name: "Synthetic benchmark", Role: "repeater", PositionSource: "reported", MetadataSource: "current", Scopes: []RegionsHistoryScope{{Name: "#sample", Buckets: buckets}}})
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := json.Marshal(response); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(encoded)), "payload-bytes")
}
