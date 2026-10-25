package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// Distinct three-byte prefixes, so every fixture hop resolves uniquely
// unless a test adds a deliberate collision.
const (
	transportPKA = "a10000000000000000000000000000000000000000000000000000000000000a"
	transportPKB = "b20000000000000000000000000000000000000000000000000000000000000b"
	transportPKC = "c30000000000000000000000000000000000000000000000000000000000000c"
	transportPKD = "d40000000000000000000000000000000000000000000000000000000000000d"
)

type transportFix struct {
	t   *testing.T
	s   *Server
	now time.Time
	tx  int
}

// transportFixture is auditFixture plus observers with IATA regions: rowid 1
// is CPH, rowid 2 is ARN. Observations carry observer_idx (v3 schema).
func transportFixture(t *testing.T) *transportFix {
	t.Helper()
	s, _, _ := auditFixture(t)
	for _, q := range []string{
		"DELETE FROM observers",
		"INSERT INTO observers(rowid,id,name,iata) VALUES(1,'obs-cph','cph','CPH')",
		"INSERT INTO observers(rowid,id,name,iata) VALUES(2,'obs-arn','arn','arn ')",
	} {
		if _, err := s.db.conn.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return &transportFix{t: t, s: s, now: time.Now().UTC()}
}

// node inserts a repeater; a nil scope is a node that was never asked for a
// declaration, "" is a confirmed empty declaration.
func (f *transportFix) node(pk, name string, scope *string) {
	f.t.Helper()
	if _, err := f.s.db.conn.Exec(`INSERT INTO nodes(public_key,name,role,configured_scope,configured_scope_at) VALUES(?,?,'repeater',?,?)`, pk, name, scope, f.now.Format(time.RFC3339)); err != nil {
		f.t.Fatal(err)
	}
}

// forward adds n new transmissions, each heard once by observer through path.
// scope nil + route 1 is unscoped flood; route 0 with a scope is transport flood.
func (f *transportFix) forward(n, route int, scope *string, path string, observer int, at time.Time) {
	f.t.Helper()
	raw := "0001020304"
	if route == 1 {
		raw = "01"
	}
	for i := 0; i < n; i++ {
		f.tx++
		if _, err := f.s.db.conn.Exec(`INSERT INTO transmissions(id,hash,raw_hex,first_seen,route_type,scope_name) VALUES(?,?,?,?,?,?)`, f.tx, fmt.Sprintf("h%d", f.tx), raw, at.Format(time.RFC3339), route, scope); err != nil {
			f.t.Fatal(err)
		}
		f.observe(f.tx, path, raw, observer, at)
	}
}

func (f *transportFix) observe(tx int, path, raw string, observer int, at time.Time) {
	f.t.Helper()
	if _, err := f.s.db.conn.Exec(`INSERT INTO observations(transmission_id,observer_idx,path_json,raw_hex,timestamp) VALUES(?,?,?,?,?)`, tx, observer, path, raw, at.Unix()); err != nil {
		f.t.Fatal(err)
	}
}

func (f *transportFix) get(query string) (*ScopeTransportResponse, int, string) {
	f.t.Helper()
	w := httptest.NewRecorder()
	f.s.handleScopeTransport(w, httptest.NewRequest("GET", "/api/scope-audit/transport?"+query, nil))
	if w.Code != 200 {
		return nil, w.Code, w.Body.String()
	}
	var r ScopeTransportResponse
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		f.t.Fatal(err)
	}
	return &r, w.Code, w.Body.String()
}

func (f *transportFix) mustGet(query string) *ScopeTransportResponse {
	f.t.Helper()
	r, code, body := f.get(query)
	if r == nil {
		f.t.Fatalf("GET %s: %d %s", query, code, body)
	}
	return r
}

func transportRows(r *ScopeTransportResponse) map[string]ScopeTransportRow {
	out := make(map[string]ScopeTransportRow, len(r.Rows))
	for _, row := range r.Rows {
		out[row.PublicKey] = row
	}
	return out
}

func strp(s string) *string { return &s }

func TestScopeTransportListsNeverQueriedAndEmptyDeclarations(t *testing.T) {
	f := transportFixture(t)
	dk, eu := "dk", "eu"
	f.node(transportPKA, "never-asked", nil)
	f.node(transportPKB, "empty", strp(""))
	f.node(transportPKC, "declared", strp("#dk,eu"))
	f.forward(6, 0, &dk, `["a10000"]`, 1, f.now)
	f.forward(6, 1, nil, `["b20000"]`, 1, f.now)
	f.forward(6, 0, &eu, `["c30000"]`, 1, f.now)

	r := f.mustGet("window=24h&scope=%23dk")
	if r.Scope != "dk" || len(r.Rows) != 3 || r.Summary.Total != 3 {
		t.Fatalf("scope=%q rows=%d summary=%+v", r.Scope, len(r.Rows), r.Summary)
	}
	rows := transportRows(r)
	a, b, c := rows[transportPKA], rows[transportPKB], rows[transportPKC]
	// Never asked is not the same as declaring nothing.
	if a.Declaration != nil || a.Evidence != "carrier" || a.Carrier == nil || *a.Carrier != 6 || *a.NonCarrier != 0 || a.Forwarded != 6 {
		t.Fatalf("never-asked=%+v", a)
	}
	if b.Declaration == nil || b.Declaration.ConfiguredScope != "" || len(b.Declaration.DeclaredScopes) != 0 || b.Declaration.DeclaresScope == nil || *b.Declaration.DeclaresScope {
		t.Fatalf("empty declaration=%+v", b.Declaration)
	}
	if b.Evidence != "not-seen-carrying" || *b.Carrier != 0 || *b.NonCarrier != 6 || b.Unscoped != 6 {
		t.Fatalf("empty=%+v", b)
	}
	// Declaring the scope but not being seen carrying it is still transport
	// evidence only; it is not turned into a configuration verdict.
	if c.Declaration == nil || !*c.Declaration.DeclaresScope || c.Evidence != "not-seen-carrying" || len(c.ObservedScopes) != 1 || c.ObservedScopes[0].Name != "eu" {
		t.Fatalf("declared=%+v", c)
	}
	if r.Summary.Carrier != 1 || r.Summary.NotSeenCarrying != 2 || r.Summary.NeverQueried != 1 || r.Summary.Declared != 2 {
		t.Fatalf("summary=%+v", r.Summary)
	}
	if r.Population.Transmissions != 18 || r.Population.ScopeTransmissions == nil || *r.Population.ScopeTransmissions != 6 {
		t.Fatalf("population=%+v", r.Population)
	}
	if len(r.Scopes) != 2 || r.Scopes[0].Count != 6 {
		t.Fatalf("scopes=%+v", r.Scopes)
	}
	// The declaration audit keeps its own population: declared nodes only.
	audit, err := f.s.buildScopeAudit("24h", f.now)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit.Rows) != 2 {
		t.Fatalf("declaration audit rows=%+v", audit.Rows)
	}
}

func TestScopeTransportWithoutScopeListsForwardersOnly(t *testing.T) {
	f := transportFixture(t)
	dk := "dk"
	f.node(transportPKA, "busy", nil)
	f.node(transportPKB, "quiet", nil)
	f.forward(3, 0, &dk, `["a10000"]`, 1, f.now)
	// Older than the window: the quiet repeater is not a forwarder in 24h.
	f.forward(3, 0, &dk, `["b20000"]`, 1, f.now.Add(-48*time.Hour))
	r := f.mustGet("window=24h")
	if len(r.Rows) != 1 || r.Rows[0].PublicKey != transportPKA || r.Rows[0].Evidence != "forwarding" || r.Rows[0].Carrier != nil || r.Population.ScopeTransmissions != nil {
		t.Fatalf("rows=%+v population=%+v", r.Rows, r.Population)
	}
	if r := f.mustGet("window=7d"); len(r.Rows) != 2 {
		t.Fatalf("7d rows=%+v", r.Rows)
	}
}

func TestScopeTransportLowAmbiguousAndIncompleteEvidence(t *testing.T) {
	f := transportFixture(t)
	dk, eu, unknown := "dk", "eu", ""
	f.node(transportPKA, "carrier", nil)
	f.node(transportPKB, "low", nil)
	f.node(transportPKC, "collides-1", nil)
	f.node("c3ffff0000000000000000000000000000000000000000000000000000000000", "collides-2", nil)
	f.node(transportPKD, "unknown-scope", nil)
	f.forward(6, 0, &dk, `["a10000"]`, 1, f.now)
	f.forward(2, 0, &eu, `["b20000"]`, 1, f.now)
	// c3 is unique at 3 bytes but ambiguous at 1 byte: the dk hop "c3" could
	// be either node, so neither may be judged as not carrying dk.
	f.forward(6, 0, &eu, `["c30000"]`, 1, f.now)
	f.forward(1, 0, &dk, `["c3"]`, 1, f.now)
	f.forward(6, 0, &eu, `["d40000"]`, 1, f.now)
	f.forward(1, 0, &unknown, `["d40000"]`, 1, f.now)

	r := f.mustGet("window=24h&scope=dk")
	rows := transportRows(r)
	want := map[string]string{transportPKA: "carrier", transportPKB: "low-evidence", transportPKC: "ambiguous", transportPKD: "incomplete"}
	for pk, evidence := range want {
		if rows[pk].Evidence != evidence {
			t.Fatalf("%s evidence=%q want %q (%+v)", rows[pk].Name, rows[pk].Evidence, evidence, rows[pk])
		}
	}
	if len(rows) != 4 {
		t.Fatalf("an ambiguous-only candidate must not be listed as a forwarder: %+v", r.Rows)
	}
	c := rows[transportPKC]
	if c.AmbiguousHops != 1 || c.AmbiguousScopeHops == nil || *c.AmbiguousScopeHops != 1 {
		t.Fatalf("ambiguous counts=%+v", c)
	}
	if r.Population.AmbiguousHops != 1 || r.Summary.Ambiguous != 1 || r.Summary.LowEvidence != 1 || r.Summary.Incomplete != 1 || r.Summary.NotSeenCarrying != 0 {
		t.Fatalf("summary=%+v population=%+v", r.Summary, r.Population)
	}
	// The same ambiguous dk hop says nothing about eu, so eu judgements stay.
	r = f.mustGet("window=24h&scope=eu")
	if got := transportRows(r)[transportPKA].Evidence; got != "not-seen-carrying" {
		t.Fatalf("eu evidence for dk-only carrier=%q", got)
	}
	// A scope nobody was seen carrying in the population cannot be judged.
	r = f.mustGet("window=24h&scope=nowhere")
	if *r.Population.ScopeTransmissions != 0 || r.Summary.NoScopeTraffic != 3 || r.Summary.NotSeenCarrying != 0 {
		t.Fatalf("no traffic summary=%+v", r.Summary)
	}
	// Scope names are case-sensitive in firmware (RegionMap::findByName, strcmp).
	if r := f.mustGet("window=24h&scope=DK"); *r.Population.ScopeTransmissions != 0 {
		t.Fatalf("scope matched case-insensitively: %+v", r.Population)
	}
}

func TestScopeTransportEvidenceClassification(t *testing.T) {
	cases := []struct {
		name                                               string
		forwarded, carrier, ambScope, unknown, legacy, pop int
		want                                               string
	}{
		{"carrier wins over caveats", 1, 1, 3, 3, 3, 1, "carrier"},
		{"ambiguous scope hop", 9, 0, 1, 1, 0, 5, "ambiguous"},
		{"unknown scope", 9, 0, 0, 1, 0, 5, "incomplete"},
		{"legacy frames", 9, 0, 0, 0, 1, 5, "incomplete"},
		{"no scope traffic", 9, 0, 0, 0, 0, 0, "no-scope-traffic"},
		{"below threshold", scopeTransportMinForwarded - 1, 0, 0, 0, 0, 5, "low-evidence"},
		{"at threshold", scopeTransportMinForwarded, 0, 0, 0, 0, 5, "not-seen-carrying"},
	}
	for _, c := range cases {
		if got := classifyScopeTransport(c.forwarded, c.carrier, c.ambScope, c.unknown, c.legacy, c.pop); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	// Over every input combination the outcome stays an evidence label; none
	// is a configuration verdict.
	allowed := map[string]bool{"carrier": true, "ambiguous": true, "incomplete": true, "no-scope-traffic": true, "low-evidence": true, "not-seen-carrying": true}
	for fw := 0; fw <= scopeTransportMinForwarded+1; fw++ {
		for mask := 0; mask < 32; mask++ {
			bit := func(i int) int { return mask >> i & 1 }
			if got := classifyScopeTransport(fw, bit(0)*fw, bit(1), bit(2), bit(3), bit(4)); !allowed[got] {
				t.Fatalf("classify(%d, %05b)=%q", fw, mask, got)
			}
		}
	}
}

func TestScopeTransportRegionFilterAppliesToCountsRowsAndDetails(t *testing.T) {
	f := transportFixture(t)
	dk, eu := "dk", "eu"
	f.node(transportPKA, "heard-in-cph", nil)
	f.node(transportPKB, "heard-in-arn", nil)
	f.node(transportPKC, "both", nil)
	f.forward(5, 0, &dk, `["a10000"]`, 1, f.now)
	f.forward(5, 0, &dk, `["b20000"]`, 2, f.now)
	// C carries dk in ARN and eu in CPH: the details differ per region.
	f.forward(5, 0, &eu, `["c30000"]`, 1, f.now)
	f.forward(5, 0, &dk, `["c30000"]`, 2, f.now)

	cph := f.mustGet("window=24h&scope=dk&region=cph,%20")
	rows := transportRows(cph)
	if len(cph.Region) != 1 || cph.Region[0] != "CPH" || cph.Population.RegionObservers == nil || *cph.Population.RegionObservers != 1 {
		t.Fatalf("region echo=%v observers=%v", cph.Region, cph.Population.RegionObservers)
	}
	if len(rows) != 2 || rows[transportPKB].PublicKey != "" || cph.Summary.Total != 2 || cph.Population.Transmissions != 10 || *cph.Population.ScopeTransmissions != 5 {
		t.Fatalf("CPH rows=%+v summary=%+v population=%+v", cph.Rows, cph.Summary, cph.Population)
	}
	if c := rows[transportPKC]; c.Evidence != "not-seen-carrying" || len(c.ObservedScopes) != 1 || c.ObservedScopes[0].Name != "eu" || c.Forwarded != 5 {
		t.Fatalf("CPH details for C=%+v", c)
	}
	// The IATA comparison trims and upper-cases stored codes ("arn ").
	arn := f.mustGet("window=24h&scope=dk&region=ARN")
	if c := transportRows(arn)[transportPKC]; c.Evidence != "carrier" || len(arn.Rows) != 2 {
		t.Fatalf("ARN rows=%+v", arn.Rows)
	}
	all := f.mustGet("window=24h&scope=dk&region=All")
	if len(all.Rows) != 3 || len(all.Region) != 0 || all.Population.RegionObservers != nil || all.Population.Transmissions != 20 {
		t.Fatalf("All rows=%d region=%v population=%+v", len(all.Rows), all.Region, all.Population)
	}
	// A region with no observer matches nothing instead of widening to all.
	none := f.mustGet("window=24h&scope=dk&region=XXX")
	if len(none.Rows) != 0 || *none.Population.RegionObservers != 0 || none.Population.Transmissions != 0 {
		t.Fatalf("empty region=%+v", none)
	}
	// Blacklisted observers are not part of a region's population.
	f.s.cfg.ObserverBlacklist = []string{"obs-cph"} // read once, lazily
	if r := f.mustGet("window=1h&scope=dk&region=CPH"); *r.Population.RegionObservers != 0 || len(r.Rows) != 0 {
		t.Fatalf("blacklisted observer counted: %+v", r.Population)
	}
}

func TestScopeTransportCacheReusesOneScanAcrossScopes(t *testing.T) {
	f := transportFixture(t)
	dk := "dk"
	f.node(transportPKA, "relay", nil)
	f.forward(5, 0, &dk, `["a10000"]`, 1, f.now)
	builds := 0
	f.s.scopeTransport.buildHook = func() { builds++ }
	f.mustGet("window=24h&scope=dk")
	// New traffic is invisible until the TTL: other scopes reuse the scan.
	f.node(transportPKB, "late", nil)
	f.forward(5, 0, &dk, `["b20000"]`, 1, f.now)
	for _, q := range []string{"window=24h&scope=eu", "window=24h", "window=24h&scope=%23dk&region=all"} {
		if r := f.mustGet(q); len(r.Rows) != 1 {
			t.Fatalf("%s rows=%d: scan was not reused", q, len(r.Rows))
		}
	}
	if builds != 1 {
		t.Fatalf("builds=%d want 1", builds)
	}
	// Window and region are part of the key, normalised.
	f.mustGet("window=7d")
	f.mustGet("window=24h&region=cph")
	f.mustGet("window=24h&region=CPH,%20cph")
	if builds != 3 {
		t.Fatalf("builds=%d want 3", builds)
	}
	// The cache is bounded: many region keys cannot grow it without limit.
	for i := 0; i < 3*scopeTransportCacheMax; i++ {
		f.mustGet(fmt.Sprintf("window=1h&region=R%02d", i))
	}
	f.s.scopeTransport.mu.Lock()
	n := len(f.s.scopeTransport.entries)
	f.s.scopeTransport.mu.Unlock()
	if n > scopeTransportCacheMax {
		t.Fatalf("cache entries=%d max %d", n, scopeTransportCacheMax)
	}
}

func TestScopeTransportVisibilityAppliesToRowsAndSummary(t *testing.T) {
	f := transportFixture(t)
	dk := "dk"
	f.node(transportPKA, "visible", nil)
	f.node(transportPKB, "private-relay", nil)
	f.node(transportPKC, "blocked", nil)
	f.forward(5, 0, &dk, `["a10000","b20000","c30000"]`, 1, f.now)
	f.s.cfg.SetHiddenNamePrefixes([]string{"private-"})
	f.s.cfg.SetNodeBlacklist([]string{transportPKC})
	r := f.mustGet("window=24h&scope=dk")
	if len(r.Rows) != 1 || r.Rows[0].PublicKey != transportPKA || r.Summary.Total != 1 || r.Summary.Carrier != 1 {
		t.Fatalf("hidden identities leaked: rows=%+v summary=%+v", r.Rows, r.Summary)
	}
}

func TestScopeTransportValidationCoverageAndLegacyFrames(t *testing.T) {
	f := transportFixture(t)
	dk := "dk"
	f.node(transportPKA, "relay", nil)
	f.forward(5, 0, &dk, `["a10000"]`, 1, f.now.Add(-2*time.Hour))
	// A legacy observation without its own frame header.
	f.forward(1, 0, &dk, `["a10000"]`, 1, f.now)
	if _, err := f.s.db.conn.Exec(`UPDATE observations SET raw_hex=NULL WHERE transmission_id=?`, f.tx); err != nil {
		t.Fatal(err)
	}
	codes := make([]string, scopeTransportMaxRegionCodes+1)
	for i := range codes {
		codes[i] = fmt.Sprintf("R%02d", i)
	}
	for _, q := range []string{"window=2d", "region=" + strings.Join(codes, ","), "scope=*", "scope=" + strings.Repeat("x", scopeTransportMaxScopeLen+1)} {
		if _, code, _ := f.get(q); code != 400 {
			t.Fatalf("%s status=%d", q, code)
		}
	}
	r := f.mustGet("window=7d&scope=dk")
	if r.Population.LegacyObservations != 1 || r.Rows[0].LegacyObservations != 1 || r.Rows[0].Evidence != "carrier" {
		t.Fatalf("legacy=%+v row=%+v", r.Population, r.Rows[0])
	}
	// Captures start two hours ago: a 7d window is not fully covered, 1h is.
	if r.Coverage.OldestObservation == nil || r.Coverage.Complete {
		t.Fatalf("7d coverage=%+v", r.Coverage)
	}
	if r := f.mustGet("window=1h"); !r.Coverage.Complete {
		t.Fatalf("1h coverage=%+v", r.Coverage)
	}
}

// Transport evidence is read from SQLite only, never through PacketStore.mu,
// so a writer holding the store lock cannot stall it and it cannot stall ingest.
func TestScopeTransportDoesNotTakePacketStoreLock(t *testing.T) {
	f := transportFixture(t)
	dk := "dk"
	f.node(transportPKA, "relay", nil)
	f.forward(5, 0, &dk, `["a10000"]`, 1, f.now)
	if f.s.store == nil {
		t.Fatal("fixture has no store")
	}
	f.s.store.mu.Lock()
	defer f.s.store.mu.Unlock()
	done := make(chan *ScopeTransportResponse, 1)
	go func() { r, _, _ := f.get("window=24h&scope=dk"); done <- r }()
	select {
	case r := <-done:
		if r == nil || len(r.Rows) != 1 {
			t.Fatalf("response=%+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("transport view blocked on PacketStore.mu")
	}
}

func TestScopeTransportRoutedAndDocumented(t *testing.T) {
	f := transportFixture(t)
	f.node(transportPKA, "relay", nil)
	router := mux.NewRouter()
	f.s.RegisterRoutes(router)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/scope-audit/transport?window=1h", nil))
	if w.Code != 200 {
		t.Fatalf("route status=%d %s", w.Code, w.Body.String())
	}
	if _, ok := routeDescriptions()["GET /api/scope-audit/transport"]; !ok {
		t.Fatal("transport endpoint missing from OpenAPI descriptions")
	}
}

func seedTransportBenchmark(b *testing.B) *Server {
	b.Helper()
	db := setupTestDB(b)
	for _, q := range []string{"ALTER TABLE nodes ADD COLUMN configured_scope TEXT", "ALTER TABLE nodes ADD COLUMN configured_scope_at TEXT", "ALTER TABLE transmissions ADD COLUMN scope_name TEXT", "CREATE INDEX audit_obs_tx ON observations(transmission_id)", "CREATE INDEX audit_obs_ts ON observations(timestamp)",
		"INSERT INTO observers(rowid,id,iata) VALUES(1,'o1','CPH'),(2,'o2','ARN'),(3,'o3','OSL')"} {
		if _, err := db.conn.Exec(q); err != nil {
			b.Fatal(err)
		}
	}
	now := time.Now().UTC()
	stamp := now.Format(time.RFC3339)
	tx, err := db.conn.Begin()
	if err != nil {
		b.Fatal(err)
	}
	insertNode, _ := tx.Prepare(`INSERT INTO nodes(public_key,name,role,configured_scope,configured_scope_at) VALUES(?,'relay','repeater',?,?)`)
	keys := make([]string, 2000)
	for i := range keys {
		keys[i] = fmt.Sprintf("%06x%058x", i+1, 0)
		// One in ten nodes was ever asked for a declaration.
		var scope interface{}
		if i%10 == 0 {
			scope = "dk"
		}
		if _, err := insertNode.Exec(keys[i], scope, stamp); err != nil {
			b.Fatal(err)
		}
	}
	insertNode.Close()
	insertTx, _ := tx.Prepare(`INSERT INTO transmissions(id,hash,raw_hex,first_seen,route_type,scope_name) VALUES(?,?,'0001020304',?,0,?)`)
	insertObs, _ := tx.Prepare(`INSERT INTO observations(transmission_id,observer_idx,path_json,raw_hex,timestamp) VALUES(?,?,?,'0001020304',?)`)
	scopes := []string{"dk", "se", "no", "eu"}
	for i := 1; i <= 30000; i++ {
		if _, err := insertTx.Exec(i, fmt.Sprint(i), stamp, scopes[i%len(scopes)]); err != nil {
			b.Fatal(err)
		}
		path := fmt.Sprintf(`["%s","%s","%s","%s"]`, keys[i%2000][:6], keys[(i+1)%2000][:6], keys[(i+2)%2000][:6], keys[(i+3)%2000][:6])
		for j := 0; j < 3; j++ {
			if _, err := insertObs.Exec(i, j+1, path, now.Unix()); err != nil {
				b.Fatal(err)
			}
		}
	}
	insertTx.Close()
	insertObs.Close()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	db.detectSchema()
	return &Server{db: db, cfg: &Config{}}
}

// Cold: one uncached scan (30K transmissions, 90K observations, 2K nodes).
func BenchmarkScopeTransportCold30KPackets2KNodes(b *testing.B) {
	s := seedTransportBenchmark(b)
	now := time.Now().UTC()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scan, err := s.buildScopeTransportScan(scopeTransportKey{window: "24h"}, now)
		if err != nil {
			b.Fatal(err)
		}
		if len(scan.rows) != 2000 {
			b.Fatalf("rows=%d", len(scan.rows))
		}
	}
}

// Cold with an observer-region filter (one of three observers).
func BenchmarkScopeTransportColdRegion30KPackets2KNodes(b *testing.B) {
	s := seedTransportBenchmark(b)
	now := time.Now().UTC()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.buildScopeTransportScan(scopeTransportKey{window: "24h", region: "CPH"}, now); err != nil {
			b.Fatal(err)
		}
	}
}

// Warm: an HTTP request served from the cached scan for one scope.
func BenchmarkScopeTransportWarm30KPackets2KNodes(b *testing.B) {
	s := seedTransportBenchmark(b)
	req := func() {
		w := httptest.NewRecorder()
		s.handleScopeTransport(w, httptest.NewRequest("GET", "/api/scope-audit/transport?window=24h&scope=dk", nil))
		if w.Code != 200 {
			b.Fatal(w.Body.String())
		}
	}
	req()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req()
	}
}

// Retained heap of one cached scan: what the bounded cache holds per key.
func BenchmarkScopeTransportRetainedHeap(b *testing.B) {
	s := seedTransportBenchmark(b)
	now := time.Now().UTC()
	var before, after runtime.MemStats
	for i := 0; i < b.N; i++ {
		runtime.GC()
		runtime.ReadMemStats(&before)
		scan, err := s.buildScopeTransportScan(scopeTransportKey{window: "24h"}, now)
		if err != nil {
			b.Fatal(err)
		}
		runtime.GC()
		runtime.ReadMemStats(&after)
		b.ReportMetric(float64(after.HeapAlloc)-float64(before.HeapAlloc), "retained-B")
		runtime.KeepAlive(scan)
	}
}
