package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// GET /api/nodes/{pubkey}/declared-regions (#126): the read-only consumer of
// the ingestor's client_declared_regions table. Contract: docs/client-regions.md.

const (
	declTarget     = "bb11223344556677889900aabbccddeeff00112233445566778899aabbccddaa"
	declCollectorA = "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2"
	declCollectorB = "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"
)

// declaredRegionsDDL mirrors the ingestor's CREATE TABLE (cmd/ingestor/db.go).
const declaredRegionsDDL = `CREATE TABLE client_declared_regions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	collector_pubkey TEXT NOT NULL, target_pubkey TEXT NOT NULL,
	regions_csv TEXT NOT NULL, truncated INTEGER NOT NULL DEFAULT 0,
	reported_at TEXT NOT NULL, ingested_at TEXT NOT NULL, repeater_clock INTEGER,
	lat REAL, lon REAL, pos_acc_m REAL, mqtt_source TEXT NOT NULL DEFAULT '',
	UNIQUE(target_pubkey, reported_at, collector_pubkey))`

func regionsOnServerCfg() *Config {
	return &Config{ClientRegions: &ClientRegionsConfig{Enabled: true}}
}

func seedDeclaredDB(t *testing.T) *DB {
	t.Helper()
	db := setupTestDBv2(t)
	mustExecDB(t, db, declaredRegionsDDL)
	db.hasClientDeclaredRegionsFlag.forceTrue()
	return db
}

func insertDeclared(t *testing.T, conn *sql.DB, collector, reportedAt, csv string, clock interface{}, lat interface{}) {
	t.Helper()
	if _, err := conn.Exec(`INSERT INTO client_declared_regions
		(collector_pubkey, target_pubkey, regions_csv, reported_at, ingested_at, repeater_clock, lat, lon, pos_acc_m)
		VALUES (?, ?, ?, ?, '2026-10-14T12:00:00.000Z', ?, ?, ?, ?)`,
		collector, declTarget, csv, reportedAt, clock, lat, lat, lat); err != nil {
		t.Fatalf("insert report: %v", err)
	}
}

func serveDeclared(srv *Server, pubkey string) *httptest.ResponseRecorder {
	router := mux.NewRouter()
	router.HandleFunc("/api/nodes/{pubkey}/declared-regions", srv.handleNodeDeclaredRegions).Methods("GET")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest("GET", "/api/nodes/"+pubkey+"/declared-regions", nil))
	return rr
}

func decodeDeclared(t *testing.T, rr *httptest.ResponseRecorder) DeclaredRegionsResponse {
	t.Helper()
	if rr.Code != 200 {
		t.Fatalf("status %d, body %s", rr.Code, rr.Body.String())
	}
	var resp DeclaredRegionsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func TestClientRegionsServerGateDefaultOff(t *testing.T) {
	var nilCfg *Config
	if nilCfg.ClientRegionsEnabled() || (&Config{}).ClientRegionsEnabled() || (&Config{ClientRegions: &ClientRegionsConfig{}}).ClientRegionsEnabled() {
		t.Fatal("clientRegions must default off")
	}
	if !regionsOnServerCfg().ClientRegionsEnabled() {
		t.Fatal("enabled:true must be on")
	}
}

func TestDeclaredRegionsEndpointGate(t *testing.T) {
	db := seedDeclaredDB(t)
	for name, cfg := range map[string]*Config{
		"absent":                   {},
		"coverage on, regions off": {ClientRxCoverage: &ClientRxCoverageConfig{Enabled: true}},
	} {
		if rr := serveDeclared(&Server{db: db, cfg: cfg}, declTarget); rr.Code != 404 {
			t.Errorf("%s: status %d, want 404", name, rr.Code)
		}
	}
	if rr := serveDeclared(&Server{db: db, cfg: regionsOnServerCfg()}, declTarget); rr.Code != 200 {
		t.Fatalf("gate on: status %d, want 200", rr.Code)
	}
}

func TestDeclaredRegionsEndpointRejectsAndHides(t *testing.T) {
	srv := &Server{db: seedDeclaredDB(t), cfg: regionsOnServerCfg()}
	for _, bad := range []string{declTarget[:12], declTarget + "aa", "zz" + declTarget[2:]} {
		if rr := serveDeclared(srv, bad); rr.Code != 400 {
			t.Errorf("pubkey %q: status %d, want 400", bad, rr.Code)
		}
	}
	if rr := serveDeclared(srv, strings.ToUpper(declTarget)); rr.Code != 200 {
		t.Errorf("uppercase pubkey: status %d, want 200", rr.Code)
	}
	srv.cfg.SetNodeBlacklist([]string{declTarget})
	if rr := serveDeclared(srv, declTarget); rr.Code != 404 {
		t.Errorf("blacklisted node: status %d, want 404", rr.Code)
	}
}

// Newest reported_at first whatever the insert order; collectors are opaque
// per-response numbers; keys and positions are never served.
func TestDeclaredRegionsEndpointShapeAndPrivacy(t *testing.T) {
	db := seedDeclaredDB(t)
	insertDeclared(t, db.conn, declCollectorA, "2026-10-14T11:00:00.000Z", "*,#be", 1760439600, 51.2)
	insertDeclared(t, db.conn, declCollectorB, "2026-10-12T09:00:00.000Z", "", nil, nil) // arrived late, older
	insertDeclared(t, db.conn, declCollectorA, "2026-10-13T08:00:00.000Z", "#be", nil, nil)
	rr := serveDeclared(&Server{db: db, cfg: regionsOnServerCfg()}, declTarget)
	resp := decodeDeclared(t, rr)

	if resp.Pubkey != declTarget || resp.Collectors != 2 || resp.Truncated || len(resp.Reports) != 3 {
		t.Fatalf("envelope = %+v", resp)
	}
	var order []string
	for _, r := range resp.Reports {
		order = append(order, fmt.Sprintf("%s/c%d/%s", r.ReportedAt, r.Collector, strings.Join(r.Regions, "+")))
	}
	want := "2026-10-14T11:00:00.000Z/c1/*+#be 2026-10-13T08:00:00.000Z/c1/#be 2026-10-12T09:00:00.000Z/c2/"
	if strings.Join(order, " ") != want {
		t.Fatalf("reports:\n got %s\nwant %s", strings.Join(order, " "), want)
	}
	first, empty := resp.Reports[0], resp.Reports[2]
	if first.RepeaterClock == nil || *first.RepeaterClock != 1760439600 || !first.HasPosition || first.Empty {
		t.Errorf("first report = %+v", first)
	}
	if !empty.Empty || empty.Regions == nil || len(empty.Regions) != 0 || empty.RepeaterClock != nil || empty.HasPosition {
		t.Errorf("empty answer = %+v, want empty:true, regions:[], repeater_clock:null", empty)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"regions":[]`) || !strings.Contains(body, `"repeater_clock":null`) {
		t.Errorf("explicit empty list / missing clock must serialise as [] / null: %s", body)
	}
	for _, leak := range []string{declCollectorA, declCollectorB, declCollectorA[:16], "51.2", "lat", "acc"} {
		if strings.Contains(body, leak) {
			t.Errorf("response leaks %q: %s", leak, body)
		}
	}
}

func TestDeclaredRegionsEndpointLimit(t *testing.T) {
	db := seedDeclaredDB(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i <= declaredRegionsAPILimit; i++ {
		insertDeclared(t, db.conn, declCollectorA, base.Add(time.Duration(i)*time.Minute).Format("2006-01-02T15:04:05.000Z"), "#be", nil, nil)
	}
	resp := decodeDeclared(t, serveDeclared(&Server{db: db, cfg: regionsOnServerCfg()}, declTarget))
	if len(resp.Reports) != declaredRegionsAPILimit || !resp.Truncated {
		t.Fatalf("got %d reports truncated=%v, want %d and truncated", len(resp.Reports), resp.Truncated, declaredRegionsAPILimit)
	}
	if resp.Reports[0].ReportedAt != base.Add(declaredRegionsAPILimit*time.Minute).Format("2006-01-02T15:04:05.000Z") {
		t.Fatalf("the limit must keep the newest reports, first = %s", resp.Reports[0].ReportedAt)
	}
}

// The ingestor creates client_declared_regions; the server, started at the
// same time, may probe first. It must neither fail nor cache "absent" for
// its lifetime.
func TestDeclaredRegionsTableCreatedAfterServerStartup(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "race.db")
	rw, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	rw.SetMaxOpenConns(1)
	defer rw.Close()
	if _, err := rw.Exec(`CREATE TABLE nodes (public_key TEXT PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}

	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := &Server{db: db, cfg: regionsOnServerCfg()}

	if db.hasClientDeclaredRegions() {
		t.Fatal("precondition: the table does not exist at server startup")
	}
	if resp := decodeDeclared(t, serveDeclared(srv, declTarget)); len(resp.Reports) != 0 || resp.Reports == nil {
		t.Fatalf("table absent: want 200 with reports:[], got %+v", resp)
	}

	// The ingestor's schema lands after the server's warm probe.
	if _, err := rw.Exec(declaredRegionsDDL); err != nil {
		t.Fatal(err)
	}
	insertDeclared(t, rw, declCollectorA, "2026-10-14T11:00:00.000Z", "#be", nil, nil)

	db.detectSchema() // one healSchemaFlags tick
	if !db.hasClientDeclaredRegions() {
		t.Fatal("detectSchema must latch the table once it exists")
	}
	if resp := decodeDeclared(t, serveDeclared(srv, declTarget)); len(resp.Reports) != 1 {
		t.Fatalf("after the table appeared: want 1 report without a restart, got %+v", resp)
	}
}

// The background healer must keep probing for the table: with every other
// flag already true it latches the new one and then exits.
func TestHealSchemaFlagsLatchesDeclaredRegionsTable(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	mustExecDB(t, db, declaredRegionsDDL)
	for _, f := range []*schemaFlag{&db.isV3Flag, &db.hasResolvedPathFlag, &db.hasObsRawHexFlag, &db.hasScopeNameFlag,
		&db.hasDefaultScopeFlag, &db.hasConfiguredScopeFlag, &db.hasDefaultScopeConfirmedAtFlag,
		&db.hasMultibyteSupColsFlag, &db.hasLastSeenFlag, &db.hasRouteMaskFlag, &db.hasProposalApprovedByFlag} {
		f.forceTrue()
	}
	db.schemaHealerStop = make(chan struct{})
	done := make(chan struct{})
	go func() { db.healSchemaFlags(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("healSchemaFlags did not latch client_declared_regions within two ticks")
	}
	if !db.hasClientDeclaredRegions() {
		t.Fatal("the healer exited without latching the table flag")
	}
}
