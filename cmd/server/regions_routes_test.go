package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"
)

func routeObservation(path, raw string) *StoreObs {
	o := &StoreObs{PathJSON: path}
	cacheObservationWireEvidence(o, raw)
	return o
}

func routeTransmission(id int, when time.Time, paths ...string) *StoreTx {
	pt, rt := 5, 0
	tx := &StoreTx{ID: id, FirstSeen: when.Format(time.RFC3339Nano), PayloadType: &pt, RouteType: &rt, ScopeName: "#sample", RawHex: "14aabbccdd00"}
	for _, path := range paths {
		tx.Observations = append(tx.Observations, routeObservation(path, "14aabbccdd00"))
	}
	return tx
}

func TestRegionRoutesWireEvidenceAndLayout(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) == 8 && unsafe.Sizeof(StoreObs{}) != 200 {
		t.Fatalf("observation grew: %d", unsafe.Sizeof(StoreObs{}))
	}
	var layout StoreObs
	if unsafe.Offsetof(layout.transportKnown) != unsafe.Offsetof(layout.wireRoute)+1 || unsafe.Offsetof(layout.wireTransport)+unsafe.Sizeof(layout.wireTransport) > unsafe.Sizeof(layout) {
		t.Fatal("transport evidence no longer fits trailing padding")
	}
	for _, tc := range []struct {
		raw   string
		known bool
		route uint8
	}{
		{"14AABBCCDD00", true, 1}, {"17aabbccdd00", true, 4}, {"1500", false, 2}, {"", false, 0},
		{"14aa", false, 1}, {"14aabbzzdd00", false, 1}, {"xxaabbccdd00", false, 0},
	} {
		o := routeObservation("[]", tc.raw)
		if o.wireRoute != tc.route || o.transportKnown != tc.known {
			t.Fatalf("%q: route=%d known=%v", tc.raw, o.wireRoute, o.transportKnown)
		}
		if tc.known && o.wireTransport != 0xaabbccdd {
			t.Fatal("both transport codes were not retained")
		}
	}
}

func TestRegionRoutesDirectionDedupAndNoStitching(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 35, 0, 0, time.UTC)
	a, b, c := strings.Repeat("ab", 32), strings.Repeat("cd", 32), strings.Repeat("ef", 32)
	ab, ba := `["`+a+`","`+b+`"]`, `["`+b+`","`+a+`"]`
	tx := routeTransmission(1, now.Add(-2*time.Hour), ab, ab, ba, `["`+a+`","`+b+`","`+a+`","`+b+`"]`)
	collector := newRegionsRoutesCollector(now)
	collector.capture(tx)
	collector.capture(routeTransmission(2, now.Add(-2*time.Hour), `["`+b+`"]`, `["`+c+`"]`))
	got := collector.build(nil)
	if len(got.steps) != 2 || got.steps[0].From != a || got.steps[0].To != b || got.steps[1].From != b || got.steps[1].To != a {
		t.Fatalf("direction/stitching: %+v", got.steps)
	}
	for _, step := range got.steps {
		if len(step.Buckets) != 1 || step.Buckets[0].Count != 1 {
			t.Fatalf("duplicate observation/loop inflated count: %+v", step)
		}
	}
}

func TestRegionRoutesStrictObservationScopeAndBounds(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 35, 0, 0, time.UTC)
	a, b := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	path := `["` + a + `","` + b + `"]`
	collector := newRegionsRoutesCollector(now)
	end := now.Truncate(time.Hour)
	valid := routeTransmission(1, end.Add(-time.Nanosecond), path)
	direct := 3
	valid.RouteType = &direct
	valid.RawHex = "17aabbccdd00" // canonical direct does not invalidate an explicitly observed flood.
	collector.capture(valid)
	for i, raw := range []string{"", "14aabbccde00", "14aabb", "1500", "17aabbccdd00"} {
		tx := routeTransmission(i+2, end.Add(-time.Minute))
		tx.Observations = []*StoreObs{routeObservation(path, raw)}
		collector.capture(tx)
	}
	for i, when := range []time.Time{end, now.Add(time.Hour), end.Add(-21*24*time.Hour - time.Nanosecond)} {
		collector.capture(routeTransmission(i+10, when, path))
	}
	for i, pt := range []int{payloadTypeAdvert, 9} {
		tx := routeTransmission(i+20, end.Add(-time.Minute), path)
		tx.PayloadType = &pt
		collector.capture(tx)
	}
	got := collector.build(nil)
	if len(got.steps) != 1 || got.steps[0].Buckets[0].Count != 1 || got.steps[0].Buckets[0].Index != 503 {
		t.Fatalf("unsafe observation or time attribution: %+v", got.steps)
	}
	if e := got.evidence; !e.Incomplete || e.UnprovableObservations != 2 || e.ExcludedObservations != 3 || e.CappedObservations != 0 {
		t.Fatalf("missing evidence diagnostic: %+v", got.evidence)
	}
}

func TestRegionRoutesUnknownHopsBreakAdjacency(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 35, 0, 0, time.UTC)
	a, b := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	collector := newRegionsRoutesCollector(now)
	collector.capture(routeTransmission(1, now.Add(-time.Hour), `["`+a+`","unknown","`+b+`"]`))
	got := collector.build(nil)
	if len(got.steps) != 0 || got.evidence.UnresolvedHops != 1 || !got.evidence.Incomplete {
		t.Fatalf("bridged unknown hop: %+v", got)
	}
}

func TestRegionRoutesZeroScopeMalformedAndDifferentTransportCodes(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 35, 0, 0, time.UTC)
	a, b := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	path := `["` + a + `","` + b + `"]`
	collector := newRegionsRoutesCollector(now)
	zero := routeTransmission(1, now.Add(-time.Hour))
	zero.RawHex = "140000000000"
	zero.Observations = []*StoreObs{routeObservation(path, zero.RawHex)}
	collector.capture(zero)
	for i, raw := range []string{"14aabcccdd00", "14aabbccde00"} {
		tx := routeTransmission(i+2, now.Add(-time.Hour))
		tx.Observations = []*StoreObs{routeObservation(path, raw)}
		collector.capture(tx)
	}
	collector.capture(routeTransmission(4, now.Add(-time.Hour), `["`+a+`",null,"`+b+`"]`, `["`+a+`","`+b+`" BROKEN]`))
	got := collector.build(nil)
	if e := got.evidence; len(got.steps) != 0 || !e.Incomplete || e.UnprovableObservations != 2 || e.ExcludedObservations != 2 || e.UnresolvedHops != 1 {
		t.Fatalf("invalid scope/path accepted: %+v", got)
	}
	// A collided prefix remains unknown, including when one candidate is hidden
	// or a heuristic resolver would prefer a geographically close candidate.
	pm := buildPrefixMap([]nodeInfo{{PublicKey: a, Role: "repeater"}, {PublicKey: "ab" + strings.Repeat("34", 31), Role: "repeater"}, {PublicKey: b, Role: "repeater"}})
	collector = newRegionsRoutesCollector(now)
	collector.capture(routeTransmission(5, now.Add(-time.Hour), `["`+b+`","ab","`+a+`"]`))
	if got := collector.build(pm); len(got.steps) != 0 {
		t.Fatal("collision bridged")
	}
	pm.nonRelay = map[string]struct{}{a: {}}
	collector = newRegionsRoutesCollector(now)
	collector.capture(routeTransmission(6, now.Add(-time.Hour), path))
	if got := collector.build(pm); len(got.steps) != 0 {
		t.Fatal("listener-only identity accepted as relay")
	}
}

func TestRegionRoutesTransportCachedOnEveryObservationPath(t *testing.T) {
	db := routeMaskServerDB(t)
	defer db.Close()
	rmSeed(t, db)
	for id, raw := range map[int]string{1: "14aabbccdd00", 2: "17aabbccde00", 3: "14aabbccdf00", 4: "1600"} {
		if _, err := db.conn.Exec("UPDATE observations SET raw_hex=? WHERE id=?", raw, id); err != nil {
			t.Fatal(err)
		}
	}
	type evidence struct {
		Route     uint8
		Transport uint32
		Known     bool
	}
	snapshot := func(s *PacketStore) map[int]evidence {
		s.mu.RLock()
		defer s.mu.RUnlock()
		out := map[int]evidence{}
		for _, tx := range s.packets {
			for _, o := range tx.Observations {
				out[o.ID] = evidence{o.wireRoute, o.wireTransport, o.transportKnown}
				if o.RawHex != "" {
					t.Fatal("duplicate raw frame retained")
				}
			}
		}
		return out
	}
	want := map[int]evidence{1: {1, 0xaabbccdd, true}, 2: {4, 0xaabbccde, true}, 3: {1, 0xaabbccdf, true}, 4: {3, 0, false}}
	load := NewPacketStore(db, nil)
	if err := load.Load(); err != nil {
		t.Fatal(err)
	}
	chunked := NewPacketStore(db, nil)
	if err := chunked.LoadChunked(1); err != nil {
		t.Fatal(err)
	}
	window := NewPacketStore(db, nil)
	if err := window.loadChunk(time.Now().Add(-48*time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]*PacketStore{"Load": load, "LoadChunked": chunked, "loadChunk": window} {
		if got := snapshot(s); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	stamp := time.Now().UTC().Add(-30 * time.Minute)
	rmInsertTx(t, db, 4, "route-live", 0, 1, stamp.Format(time.RFC3339))
	rmInsertObs(t, db, 5, 4, 1, `[]`, "14aabbcce000", stamp.Unix())
	load.IngestNewFromDB(3, 100)
	rmInsertObs(t, db, 6, 2, 2, `["ab"]`, "14aabbcce100", stamp.Unix())
	rmInsertObs(t, db, 7, 2, 1, `["cd"]`, "", stamp.Unix())
	load.IngestNewObservations(5, 100)
	want[5], want[6], want[7] = evidence{1, 0xaabbcce0, true}, evidence{1, 0xaabbcce1, true}, evidence{0, 0, false}
	if got := snapshot(load); !reflect.DeepEqual(got, want) {
		t.Fatalf("live hydration: %+v", got)
	}
}

func regionsRoutesFixture(t testing.TB) (*Server, time.Time, string, string) {
	srv, now, a := regionsObservedFixture(t)
	b := strings.Repeat("cd", 32)
	if _, err := srv.db.conn.Exec("INSERT INTO nodes(public_key,name,role,lat,lon) VALUES(?,'Second synthetic relay','repeater',55.3,10.4)", b); err != nil {
		t.Fatal(err)
	}
	path := `["` + a + `","` + b + `"]`
	tx := routeTransmission(1, now.Add(-2*time.Hour), path)
	// Canonical direct with an independently observed flood must work in both
	// strict history memberships and directed steps.
	direct := 3
	tx.RouteType = &direct
	tx.RawHex = "17aabbccdd00"
	tx.PathJSON = path
	srv.store.byPathHop[a] = []*StoreTx{tx}
	srv.store.byPathHop[b] = []*StoreTx{tx}
	recomputeRepeaterEnrichmentSafe(srv.store, 24)
	return srv, now, a, b
}

func getRegionsRoutes(t testing.TB, srv *Server) RegionsRoutesResponse {
	t.Helper()
	w := httptest.NewRecorder()
	srv.handleRegionsRoutes(w, httptest.NewRequest("GET", "/api/regions/routes", nil))
	var out RegionsRoutesResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("routes response %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing fresh visibility policy")
	}
	return out
}

func TestRegionsRoutesPairedMetadataVisibilityAndWarmLock(t *testing.T) {
	srv, _, a, b := regionsRoutesFixture(t)
	got := getRegionsRoutes(t, srv)
	if len(got.Nodes) != 2 || len(got.StepNodes) != 2 || len(got.Steps) != 1 || got.Steps[0].From != a || got.Steps[0].To != b || got.Steps[0].Buckets[0].Count != 1 {
		t.Fatalf("canonical direct scoped flood lost: %+v", got)
	}
	srv.store.mu.Lock()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		srv.handleRegionsRoutes(w, httptest.NewRequest("GET", "/api/regions/routes", nil))
		done <- w
	}()
	select {
	case w := <-done:
		if w.Code != 200 {
			t.Errorf("warm response=%d", w.Code)
		}
	case <-time.After(2 * time.Second):
		srv.store.mu.Unlock()
		t.Fatal("warm routes acquired packet lock")
	}
	srv.store.mu.Unlock()
	srv.cfg.SetNodeBlacklist([]string{a})
	if hidden := getRegionsRoutes(t, srv); len(hidden.Steps) != 0 || len(hidden.StepNodes) != 1 {
		t.Fatal("hidden endpoint or step leaked")
	}
	srv.cfg.SetNodeBlacklist(nil)
	srv.cfg.SetHiddenNamePrefixes([]string{"Hidden"})
	if _, err := srv.db.conn.Exec("INSERT INTO observers(id,name) VALUES(?,'Hidden alias')", strings.ToUpper(b)); err != nil {
		t.Fatal(err)
	}
	if hidden := getRegionsRoutes(t, srv); len(hidden.Steps) != 0 || len(hidden.StepNodes) != 1 {
		t.Fatal("observer alias endpoint leaked")
	}
	srv.cfg.SetHiddenNamePrefixes(nil)
	if _, err := srv.db.conn.Exec(`CREATE TABLE inactive_nodes(public_key TEXT PRIMARY KEY,name TEXT,role TEXT,lat REAL,lon REAL,foreign_advert INTEGER);
		INSERT INTO inactive_nodes SELECT public_key,name,role,lat,lon,foreign_advert FROM nodes; DELETE FROM nodes`); err != nil {
		t.Fatal(err)
	}
	archived := getRegionsRoutes(t, srv)
	if len(archived.Steps) != 1 || len(archived.StepNodes) != 2 || archived.StepNodes[0].MetadataSource != "archived" {
		t.Fatal("archived endpoint metadata lost")
	}
	if _, err := srv.db.conn.Exec("INSERT INTO nodes(public_key,name,role) VALUES(?,'Current companion','companion')", a); err != nil {
		t.Fatal(err)
	}
	if current := getRegionsRoutes(t, srv); len(current.Steps) != 0 || len(current.StepNodes) != 1 || current.StepEvidence.MissingEndpointSteps != 1 {
		t.Fatal("archive restored an endpoint whose current role is companion")
	}
}

func TestRegionRoutesSourceAndRowBounds(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 35, 0, 0, time.UTC)
	a, b := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	path := `["` + a + `","` + b + `"]`
	tx := routeTransmission(1, now.Add(-time.Hour))
	for i := 0; i < maxRegionsRoutePathsPerTx+1; i++ {
		tx.Observations = append(tx.Observations, routeObservation(strings.Repeat(" ", i)+path, "14aabbccdd00"))
	}
	collector := newRegionsRoutesCollector(now)
	collector.capture(tx)
	if collector.pathCount != maxRegionsRoutePathsPerTx || !collector.evidence.SourceTruncated {
		t.Fatal("per-transmission source cap missing")
	}
	if got := collector.build(nil); len(got.steps) != 1 || got.steps[0].Buckets[0].Count != 1 || !got.evidence.Incomplete {
		t.Fatal("source bound altered per-transmission dedup")
	}
	collector = newRegionsRoutesCollector(now)
	collector.pathCount = maxRegionsRoutePaths
	collector.capture(routeTransmission(2, now.Add(-time.Hour), path))
	if len(collector.txs) != 0 || !collector.evidence.SourceTruncated {
		t.Fatal("global path budget missing")
	}
	collector = newRegionsRoutesCollector(now)
	for i := 0; i < maxRegionsRouteSteps+1; i++ {
		to := fmt.Sprintf("%064x", i+1)
		collector.txs = append(collector.txs, regionsRouteTx{id: i + 1, when: now.Add(-time.Hour), scope: "#sample", paths: []regionsRoutePath{{path: `["` + a + `","` + to + `"]`, observations: 1}}})
	}
	got := collector.build(nil)
	if len(got.steps) != maxRegionsRouteSteps || !got.stepsTruncated || !got.evidence.Incomplete {
		t.Fatal("step row budget missing")
	}
	for i, j := 0, len(collector.txs)-1; i < j; i, j = i+1, j-1 {
		collector.txs[i], collector.txs[j] = collector.txs[j], collector.txs[i]
	}
	if other := collector.build(nil); !reflect.DeepEqual(got.steps, other.steps) {
		t.Fatal("row selection depends on map/source encounter order")
	}
}

func TestRegionRoutesActualGlobalCellBound(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 35, 0, 0, time.UTC)
	collector := newRegionsRoutesCollector(now)
	a := strings.Repeat("ab", 32)
	id := 0
	for i := 0; i < 200; i++ {
		path := `["` + a + `","` + fmt.Sprintf("%064x", i+1) + `"]`
		for hour := 0; hour < 501; hour++ {
			id++
			collector.txs = append(collector.txs, regionsRouteTx{id: id, when: collector.start.Add(time.Duration(hour) * time.Hour), scope: "#sample", paths: []regionsRoutePath{{path: path, observations: 1}}})
		}
	}
	got := collector.build(nil)
	cells := 0
	for _, step := range got.steps {
		cells += len(step.Buckets)
	}
	if cells != maxRegionsRouteCells || !got.bucketsTruncated || !got.evidence.Incomplete {
		t.Fatalf("cell limit=%d truncated=%v", cells, got.bucketsTruncated)
	}
}

func TestOpenAPIRegionsRoutesSchemas(t *testing.T) {
	if routeDescriptions()["GET /api/regions/routes"].Response["$ref"] != "#/components/schemas/RegionsRoutesResponse" {
		t.Fatal("missing route response schema")
	}
	schemas := componentSchemas()
	response := schemas["RegionsRoutesResponse"].(*openAPISchema)
	if response.Properties["nodes"].Items.Ref != "#/components/schemas/RegionsHistoryNode" || response.Properties["stepNodes"].Items.Ref != "#/components/schemas/RegionsRouteNode" || response.Properties["steps"].Items.Ref != "#/components/schemas/RegionsRouteStep" {
		t.Fatal("missing paired response shape")
	}
}

func regionsRoutesBenchmarkFixture(b *testing.B) *Server {
	srv, now, _ := regionsObservedFixture(b)
	keys := make([]string, 2000)
	transaction, err := srv.db.conn.Begin()
	if err != nil {
		b.Fatal(err)
	}
	stmt, err := transaction.Prepare("INSERT INTO nodes(public_key,name,role,lat,lon) VALUES(?,'Synthetic route relay','repeater',55.2,10.3)")
	if err != nil {
		b.Fatal(err)
	}
	for i := range keys {
		keys[i] = fmt.Sprintf("%064x", i+1)
		if _, err := stmt.Exec(keys[i]); err != nil {
			b.Fatal(err)
		}
	}
	stmt.Close()
	if err := transaction.Commit(); err != nil {
		b.Fatal(err)
	}
	end := now.Truncate(time.Hour)
	for i := 0; i < 30000; i++ {
		a, b, c := keys[i%len(keys)], keys[(i+1)%len(keys)], keys[(i+2)%len(keys)]
		path := `["` + a + `","` + b + `","` + c + `"]`
		tx := routeTransmission(i+1, end.Add(-time.Duration(i%168+1)*time.Hour), path, path)
		tx.PathJSON = path
		for _, key := range []string{a, b, c} {
			srv.store.byPathHop[key] = append(srv.store.byPathHop[key], tx)
		}
	}
	return srv
}

func BenchmarkRegionsRoutesCompute30K2K(b *testing.B) {
	srv := regionsRoutesBenchmarkFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = srv.store.computeRepeaterRelaySnapshot(24)
	}
}

func BenchmarkRegionsRoutesWarm30K2K(b *testing.B) {
	srv := regionsRoutesBenchmarkFixture(b)
	recomputeRepeaterEnrichmentSafe(srv.store, 24)
	response := getRegionsRoutes(b, srv)
	encoded, err := json.Marshal(response)
	if err != nil {
		b.Fatal(err)
	}
	srv.store.byPathHop = nil
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		srv.handleRegionsRoutes(w, httptest.NewRequest("GET", "/api/regions/routes", nil))
		if w.Code != 200 {
			b.Fatal(w.Code)
		}
	}
	b.ReportMetric(float64(len(encoded)), "payload-bytes")
	b.ReportMetric(float64(len(response.Steps)), "steps")
}

func BenchmarkRegionsRoutesCapture30K2K(b *testing.B) {
	srv := regionsRoutesBenchmarkFixture(b)
	type evidence struct {
		tx   *StoreTx
		when time.Time
	}
	items := make([]evidence, 0, 30000)
	seen := make(map[int]bool)
	for _, list := range srv.store.byPathHop {
		for _, tx := range list {
			if !seen[tx.ID] {
				seen[tx.ID] = true
				when, _ := parseRelayTS(tx.FirstSeen)
				items = append(items, evidence{tx, when})
			}
		}
	}
	now := time.Now().UTC()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		collector := newRegionsRoutesCollector(now)
		for _, item := range items {
			collector.captureAt(item.tx, item.when, true)
		}
	}
}

func BenchmarkRegionsRoutesCapPayload(b *testing.B) {
	response := RegionsRoutesResponse{Steps: make([]RegionsRouteStep, 5000), StepNodes: []RegionsRouteNode{}}
	for i := range response.Steps {
		buckets := make([]RegionsHistoryBucket, 20)
		for j := range buckets {
			buckets[j] = RegionsHistoryBucket{Index: 480 + j, Count: 12345, LastRelayed: "2026-10-16T11:59:59.123456789Z"}
		}
		response.Steps[i] = RegionsRouteStep{From: fmt.Sprintf("%064x", i), To: fmt.Sprintf("%064x", i+1), Scope: "#sample", Buckets: buckets}
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

func TestRegionsRoutesUnavailable(t *testing.T) {
	srv, _, _ := regionsObservedFixture(t)
	w := httptest.NewRecorder()
	srv.handleRegionsRoutes(w, httptest.NewRequest("GET", "/api/regions/routes", nil))
	if w.Code != 503 || srv.store.repeaterRelayCache != nil {
		t.Fatal("cold routes computed inline")
	}
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Error == "" {
		t.Fatal("missing typed error response")
	}
}
