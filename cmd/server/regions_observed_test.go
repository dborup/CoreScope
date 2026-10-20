package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func regionsObservedFixture(t testing.TB) (*Server, time.Time, string) {
	t.Helper()
	db := setupTestDB(t)
	t.Cleanup(func() { db.Close() })
	key := "ab" + strings.Repeat("12", 31)
	if _, err := db.conn.Exec("INSERT INTO nodes(public_key,name,role,lat,lon) VALUES(?, 'Synthetic relay', 'repeater', 55.2, 10.3)", key); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	store := NewPacketStore(db, nil)
	store.loaded, store.retentionHours = true, 168
	store.retainedHistoryFrom = now.Add(-8 * 24 * time.Hour)
	store.oldestLoaded = store.retainedHistoryFrom.Format(time.RFC3339Nano)
	store.packets = []*StoreTx{{FirstSeen: store.oldestLoaded}}
	store.backgroundLoadDone.Store(true)
	store.pathHopReady.Store(true)
	store.subpathReady.Store(true)
	srv := NewServer(db, &Config{}, NewHub())
	srv.store = store
	return srv, now, key
}

func seedRegionRelaySnapshot(t testing.TB, srv *Server, now time.Time, key string) {
	t.Helper()
	for i, d := range []time.Duration{30 * time.Minute, 12 * time.Hour, 72 * time.Hour} {
		tx := routeTransmission(i+1, now.Add(-d), `["`+key+`"]`)
		tx.ScopeName, tx.PathJSON = fmt.Sprintf("#scope%d", i), `["`+key+`"]`
		srv.store.byPathHop[key] = append(srv.store.byPathHop[key], tx)
	}
	recomputeRepeaterEnrichmentSafe(srv.store, 24)
}

func TestRegionsObservedWindowsAndCoverage(t *testing.T) {
	srv, now, key := regionsObservedFixture(t)
	seedRegionRelaySnapshot(t, srv, now, key)
	for _, tc := range []struct {
		window   string
		scopes   int
		complete bool
	}{{"1h", 1, true}, {"24h", 2, true}, {"7d", 3, true}, {"", 2, true}} {
		t.Run(tc.window, func(t *testing.T) {
			w := httptest.NewRecorder()
			srv.handleRegionsObserved(w, httptest.NewRequest("GET", "/api/regions/observed?window="+tc.window, nil))
			var got RegionsObservedResponse
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil {
				t.Fatalf("response %d: %s", w.Code, w.Body.String())
			}
			if len(got.Nodes) != 1 || len(got.Nodes[0].Scopes) != tc.scopes || !got.Coverage.Complete || got.Coverage.Start == nil {
				t.Fatalf("unexpected response: %+v", got)
			}
			if got.Nodes[0].PositionSource != "reported" || got.Nodes[0].Lat == nil || got.WindowEnd != got.GeneratedAt {
				t.Fatalf("position/time contract: %+v", got)
			}
		})
	}
}

func TestRegionsObservedAvailabilityAndVisibility(t *testing.T) {
	srv, now, key := regionsObservedFixture(t)
	w := httptest.NewRecorder()
	srv.handleRegionsObserved(w, httptest.NewRequest("GET", "/api/regions/observed", nil))
	if w.Code != 503 || srv.store.repeaterRelayCache != nil {
		t.Fatalf("unavailable snapshot must not compute inline: %d", w.Code)
	}
	seedRegionRelaySnapshot(t, srv, now, key)
	for _, value := range []string{"2h", "0", "168", "24H"} {
		w = httptest.NewRecorder()
		srv.handleRegionsObserved(w, httptest.NewRequest("GET", "/api/regions/observed?window="+value, nil))
		if w.Code != 400 {
			t.Fatalf("window %q: %d", value, w.Code)
		}
	}
	srv.cfg.SetNodeBlacklist([]string{key})
	w = httptest.NewRecorder()
	srv.handleRegionsObserved(w, httptest.NewRequest("GET", "/api/regions/observed", nil))
	var got RegionsObservedResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Nodes) != 0 {
		t.Fatal("blacklist was not applied to warm snapshot")
	}
	srv.cfg.SetNodeBlacklist(nil)
	srv.cfg.ObserverBlacklist = []string{key}
	w = httptest.NewRecorder()
	srv.handleRegionsObserved(w, httptest.NewRequest("GET", "/api/regions/observed", nil))
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Nodes) != 0 {
		t.Fatal("observer blacklist was not applied")
	}
	srv.cfg.ObserverBlacklist = nil
	srv.cfg.SetHiddenNamePrefixes([]string{"Hidden"})
	if _, err := srv.db.conn.Exec("INSERT INTO observers(id,name) VALUES(?, 'Hidden observer alias')", strings.ToUpper(key)); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	srv.handleRegionsObserved(w, httptest.NewRequest("GET", "/api/regions/observed", nil))
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Nodes) != 0 {
		t.Fatal("hidden observer alias was not applied")
	}
	if _, err := srv.db.conn.Exec("UPDATE observers SET name='Visible observer'"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.db.conn.Exec("UPDATE nodes SET name='Hidden renamed relay' WHERE public_key=?", key); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	srv.handleRegionsObserved(w, httptest.NewRequest("GET", "/api/regions/observed", nil))
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Nodes) != 0 {
		t.Fatal("current hidden name was not applied")
	}
	srv.cfg.SetHiddenNamePrefixes(nil)
	if _, err := srv.db.conn.Exec("UPDATE nodes SET lat=NULL,lon=NULL WHERE public_key=?", key); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	srv.handleRegionsObserved(w, httptest.NewRequest("GET", "/api/regions/observed", nil))
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Nodes) != 1 || got.Nodes[0].Lat != nil || got.Nodes[0].Lon != nil || got.Nodes[0].PositionSource != "none" {
		t.Fatalf("unpositioned relay lost: %+v", got)
	}
	if _, err := srv.db.conn.Exec("DROP TABLE nodes"); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	srv.handleRegionsObserved(w, httptest.NewRequest("GET", "/api/regions/observed", nil))
	if w.Code != 500 {
		t.Fatalf("metadata failure must not become empty success: %d", w.Code)
	}
}

func TestRegionsObservedPrivateScopeEvidence(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	entries := []relayEntry{{ts: now.Add(time.Minute).Format(time.RFC3339), scope: "#valid"}, {ts: now.Add(-time.Minute).Format(time.RFC3339), scope: "#valid"}, {ts: now.Add(-time.Minute).Format(time.RFC3339), scope: "#prefix", fromPrefix: true}, {ts: now.Add(-time.Minute).Format(time.RFC3339), scope: "#advert", pt: payloadTypeAdvert}}
	info := computeRelayInfoFromEntriesAt(entries, 24, now)
	if len(info.regionScopes) != 1 || info.regionScopes[0].name != "#valid" || !info.regionScopes[0].when.Equal(now.Add(-time.Minute)) {
		t.Fatalf("unsafe scope evidence: %+v", info.regionScopes)
	}
	if info.LastRelayed != entries[0].ts {
		t.Fatal("existing future-dated relay semantics changed")
	}
	for i := 0; i < 40; i++ {
		entries = append(entries, relayEntry{ts: now.Add(-time.Duration(i+2) * time.Minute).Format(time.RFC3339), scope: fmt.Sprintf("#a%02d", i)})
	}
	info = computeRelayInfoFromEntriesAt(entries, 24, now)
	if len(info.regionScopes) != 32 || !info.regionScopesTruncated || info.regionScopes[0].name != "#valid" {
		t.Fatalf("newest scope bound not retained: %+v", info.regionScopes)
	}
	encoded, _ := json.Marshal(info)
	if strings.Contains(string(encoded), "regionScopes") {
		t.Fatal("private scope evidence leaked into existing API")
	}
}

func TestRetainedHistoryCoverageForRegions(t *testing.T) {
	srv, now, _ := regionsObservedFixture(t)
	s := srv.store
	s.retentionHours = 12
	if floor := s.retainedHistoryCoverageStartLocked(now); !floor.Equal(now.Add(-12 * time.Hour)) {
		t.Fatalf("known partial floor=%v", floor)
	}
	if floor := s.nodeActivityCoverageStartLocked(now); !floor.IsZero() {
		t.Fatal("existing24h wrapper changed")
	}
	s.retentionHours = 168
	s.maxMemoryMB = 1
	s.packets[0].FirstSeen = now.Add(-2 * time.Hour).Format(time.RFC3339)
	if floor := s.retainedHistoryCoverageStartLocked(now); !floor.Equal(now.Add(-2 * time.Hour)) {
		t.Fatalf("eviction floor=%v", floor)
	}
	verified := s.retainedHistoryFrom
	s.retainedHistoryFrom = time.Time{}
	if floor := s.retainedHistoryCoverageStartLocked(now); !floor.IsZero() {
		t.Fatal("partial load must not claim complete coverage")
	}
	s.retainedHistoryFrom = verified
	s.backgroundLoadFailed.Store(true)
	if floor := s.retainedHistoryCoverageStartLocked(now); !floor.IsZero() {
		t.Fatal("failed load must not claim coverage")
	}
	s.backgroundLoadFailed.Store(false)
	s.backgroundLoadDone.Store(false)
	if floor := s.retainedHistoryCoverageStartLocked(now); !floor.IsZero() {
		t.Fatal("unfinished load must not claim coverage")
	}
	s.backgroundLoadDone.Store(true)
	s.retentionHours, s.hotStartupHours, s.maxMemoryMB = 0, 2, 0
	s.oldestLoaded = now.Add(-2 * time.Hour).Format(time.RFC3339)
	s.retainedHistoryFrom = now.Add(-2 * time.Hour)
	if floor := s.retainedHistoryCoverageStartLocked(now); !floor.Equal(now.Add(-2 * time.Hour)) {
		t.Fatalf("known hot-window coverage=%v", floor)
	}
	s.maxMemoryMB = 1
	s.retainedHistoryFrom = time.Time{}
	if floor := s.retainedHistoryCoverageStartLocked(now); !floor.IsZero() {
		t.Fatalf("capped hot startup without exact row coverage must remain unknown, got %v", floor)
	}
}

func TestOpenAPIRegionsObservedSchema(t *testing.T) {
	meta := routeDescriptions()["GET /api/regions/observed"]
	if meta.Response["$ref"] != "#/components/schemas/RegionsObservedResponse" {
		t.Fatal("route lacks its response schema")
	}
	schemas := componentSchemas()
	response, ok := schemas["RegionsObservedResponse"].(*openAPISchema)
	if !ok || response.Properties["nodes"].Items.Ref != "#/components/schemas/RegionsObservedNode" {
		t.Fatal("bulk response schema missing")
	}
	coverage := schemas["RegionsObservedCoverage"].(*openAPISchema)
	if !coverage.Properties["start"].Nullable || coverage.Properties["loading"].Type != "boolean" {
		t.Fatal("coverage null/loading contract missing")
	}
	node := schemas["RegionsObservedNode"].(*openAPISchema)
	if !node.Properties["lat"].Nullable || len(node.Properties["positionSource"].Enum) != 2 {
		t.Fatal("position uncertainty contract missing")
	}
}

func TestRegionsObservedCappedHotStartupCoverageIntegration(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	now := time.Now().UTC().Truncate(time.Second)
	transaction, err := db.conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	statement, err := transaction.Prepare("INSERT INTO transmissions(id,raw_hex,hash,first_seen,route_type,payload_type,decoded_json) VALUES(?,'0100',?,?,1,5,'{}')")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5000; i++ {
		when := now.Add(-110*time.Minute + time.Duration(i)*time.Second).Format(time.RFC3339)
		if _, err := statement.Exec(i, fmt.Sprintf("region-hot-%d", i), when); err != nil {
			t.Fatal(err)
		}
	}
	statement.Close()
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	store := NewPacketStore(db, &PacketStoreConfig{RetentionHours: 0, HotStartupHours: 2, MaxMemoryMB: 1})
	if err := store.RunStartupLoad(512); err != nil {
		t.Fatal(err)
	}
	if !store.WaitIndexesReady(5 * time.Second) {
		t.Fatal("indexes did not finish")
	}
	if len(store.packets) >= 5000 || len(store.packets) == 0 {
		t.Fatalf("fixture did not hit memory cap: %d packets", len(store.packets))
	}
	// LoadChunked is ID-ordered. The capped sample contains the old part of
	// the hot window while newer rows exist only in the DB: its old first row
	// cannot establish that the requested last hour was actually loaded.
	latest, ok := parseRelayTS(store.packets[len(store.packets)-1].FirstSeen)
	if !ok || !latest.Before(now.Add(-time.Hour)) {
		t.Fatalf("fixture did not omit the newer interval: latest=%v", latest)
	}
	if !store.backgroundLoadDone.Load() || store.LoadCoverageRatio() != 0 {
		t.Fatal("fixture did not reproduce unlimited-retention hot-start terminal state")
	}
	srv := NewServer(db, &Config{}, NewHub())
	srv.store = store
	recomputeRepeaterEnrichmentSafe(store, 24)
	w := httptest.NewRecorder()
	srv.handleRegionsObserved(w, httptest.NewRequest("GET", "/api/regions/observed?window=1h", nil))
	var got RegionsObservedResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("response %d: %s", w.Code, w.Body.String())
	}
	if got.Coverage.Start != nil || got.Coverage.Complete || got.Coverage.Loading {
		t.Fatalf("incomplete capped startup falsely established coverage: %+v", got.Coverage)
	}
}

func TestRegionsObservedExactBoundsAndPairedSnapshot(t *testing.T) {
	srv, now, key := regionsObservedFixture(t)
	seedRegionRelaySnapshot(t, srv, now, key)
	snapshot := srv.store.repeaterRelaySnapshot
	end, start := snapshot.generatedAt, snapshot.generatedAt.Add(-time.Hour)
	info := snapshot.regionMemberships.nodes[key]
	info.scopes = []regionScopeRelay{{"#start", start}, {"#end", end}, {"#before", start.Add(-time.Nanosecond)}, {"#future", end.Add(time.Nanosecond)}}
	snapshot.regionMemberships.nodes[key] = info
	// A later change to live history must not get paired with older cached
	// relay evidence. The endpoint also must not wait for the packet lock.
	srv.store.mu.Lock()
	srv.store.oldestLoaded = end.Format(time.RFC3339Nano)
	srv.store.backgroundLoadDone.Store(false)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		srv.handleRegionsObserved(w, httptest.NewRequest("GET", "/api/regions/observed?window=1h", nil))
		done <- w
	}()
	var w *httptest.ResponseRecorder
	select {
	case w = <-done:
	case <-time.After(2 * time.Second):
		srv.store.mu.Unlock()
		t.Fatal("warm request acquired the packet lock")
	}
	srv.store.mu.Unlock()
	var got RegionsObservedResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("response: %d %s", w.Code, w.Body.String())
	}
	if got.GeneratedAt != end.Format(time.RFC3339Nano) || !got.Coverage.Complete || got.Coverage.Loading {
		t.Fatalf("snapshot metadata torn: %+v", got)
	}
	if len(got.Nodes) != 1 || len(got.Nodes[0].Scopes) != 2 || got.Nodes[0].Scopes[0].Name != "#end" || got.Nodes[0].Scopes[1].Name != "#start" {
		t.Fatalf("bounds: %+v", got.Nodes)
	}
}

func TestRegionsObservedPositions(t *testing.T) {
	valid := func(f float64) sql.NullFloat64 { return sql.NullFloat64{Float64: f, Valid: true} }
	for _, tc := range []struct {
		name     string
		lat, lon sql.NullFloat64
		want     bool
	}{
		{"reported", valid(55), valid(10), true}, {"equator", valid(0), valid(10), true},
		{"sentinel", valid(0), valid(0), false}, {"missing", sql.NullFloat64{}, valid(10), false},
		{"range", valid(91), valid(10), false}, {"nan", valid(math.NaN()), valid(10), false}, {"infinite", valid(55), valid(math.Inf(1)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reportedRegionPosition(tc.lat, tc.lon); got != tc.want {
				t.Fatalf("valid=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestRegionsObservedGeoFilterForeignException(t *testing.T) {
	srv, now, key := regionsObservedFixture(t)
	seedRegionRelaySnapshot(t, srv, now, key)
	srv.setGeoFilter(&GeoFilterConfig{Polygon: [][2]float64{{51, 4}, {51, 5}, {50, 5}, {50, 4}}})
	for _, foreign := range []bool{false, true} {
		if _, err := srv.db.conn.Exec("UPDATE nodes SET foreign_advert=? WHERE public_key=?", foreign, key); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		srv.handleRegionsObserved(w, httptest.NewRequest("GET", "/api/regions/observed", nil))
		var got RegionsObservedResponse
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if (len(got.Nodes) == 1) != foreign {
			t.Fatalf("foreign=%v: %+v", foreign, got)
		}
	}
}

func TestRegionsObservedDeterministicLimitAfterVisibility(t *testing.T) {
	srv, now, key := regionsObservedFixture(t)
	seedRegionRelaySnapshot(t, srv, now, key)
	if _, err := srv.db.conn.Exec("DELETE FROM nodes"); err != nil {
		t.Fatal(err)
	}
	transaction, err := srv.db.conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := transaction.Prepare("INSERT INTO nodes(public_key,name,role) VALUES(?,?,'repeater')")
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	info := srv.store.repeaterRelaySnapshot.regionMemberships.nodes[key]
	for i := 0; i < maxObservedRegionNodes+2; i++ {
		pk, name := fmt.Sprintf("%064x", i+1), "Visible"
		if i < 2 {
			name = "Hidden synthetic"
		}
		if _, err := stmt.Exec(pk, name); err != nil {
			t.Fatal(err)
		}
		srv.store.repeaterRelaySnapshot.regionMemberships.nodes[pk] = info
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	srv.cfg.SetHiddenNamePrefixes([]string{"Hidden"})
	for _, hidden := range []bool{true, false} {
		if !hidden {
			srv.cfg.SetHiddenNamePrefixes(nil)
		}
		w := httptest.NewRecorder()
		srv.handleRegionsObserved(w, httptest.NewRequest("GET", "/api/regions/observed", nil))
		var got RegionsObservedResponse
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil {
			t.Fatalf("response %d", w.Code)
		}
		if len(got.Nodes) != maxObservedRegionNodes || got.Truncated == hidden {
			t.Fatalf("hidden=%v nodes=%d truncated=%v", hidden, len(got.Nodes), got.Truncated)
		}
		for i := 1; i < len(got.Nodes); i++ {
			if got.Nodes[i-1].PublicKey >= got.Nodes[i].PublicKey {
				t.Fatal("unstable node order")
			}
		}
	}
}

func BenchmarkRegionsObservedWarm(b *testing.B) {
	srv, now, key := regionsObservedFixture(b)
	seedRegionRelaySnapshot(b, srv, now, key)
	transaction, err := srv.db.conn.Begin()
	if err != nil {
		b.Fatal(err)
	}
	stmt, err := transaction.Prepare("INSERT INTO nodes(public_key,name,role,lat,lon) VALUES(?,'Synthetic benchmark relay','repeater',55.2,10.3)")
	if err != nil {
		b.Fatal(err)
	}
	info := srv.store.repeaterRelaySnapshot.regionMemberships.nodes[key]
	for i := 0; i < 2000; i++ {
		pk := fmt.Sprintf("%064x", i+1)
		if _, err := stmt.Exec(pk); err != nil {
			b.Fatal(err)
		}
		srv.store.repeaterRelaySnapshot.regionMemberships.nodes[pk] = info
	}
	stmt.Close()
	if err := transaction.Commit(); err != nil {
		b.Fatal(err)
	}
	// The packet index is deliberately unavailable after snapshot publication:
	// warm responses must read the cache, never scan packets or rebuild indexes.
	srv.store.byPathHop = nil
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		srv.handleRegionsObserved(w, httptest.NewRequest("GET", "/api/regions/observed?window=7d", nil))
		if w.Code != 200 {
			b.Fatal(w.Code)
		}
	}
}
