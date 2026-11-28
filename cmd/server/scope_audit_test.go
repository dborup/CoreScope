package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func auditFixture(t *testing.T) (*Server, func(string, string, string, string), func(int, int, *string, string, string, time.Time)) {
	t.Helper()
	s, _ := setupTestServer(t)
	for _, q := range []string{"ALTER TABLE nodes ADD COLUMN configured_scope TEXT", "ALTER TABLE nodes ADD COLUMN configured_scope_at TEXT", "ALTER TABLE transmissions ADD COLUMN scope_name TEXT", "DELETE FROM observations", "DELETE FROM transmissions", "DELETE FROM nodes"} {
		if _, err := s.db.conn.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s.db.detectSchema()
	node := func(pk, name, scope, at string) {
		t.Helper()
		if _, err := s.db.conn.Exec(`INSERT INTO nodes(public_key,name,role,configured_scope,configured_scope_at) VALUES(?,?,'repeater',?,?)`, pk, name, scope, at); err != nil {
			t.Fatal(err)
		}
	}
	packet := func(id, route int, scope *string, path, raw string, at time.Time) {
		t.Helper()
		if _, err := s.db.conn.Exec(`INSERT OR IGNORE INTO transmissions(id,hash,raw_hex,first_seen,route_type,scope_name) VALUES(?,?,?, ?,?,?)`, id, id, raw, at.UTC().Format(time.RFC3339), route, scope); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.conn.Exec(`INSERT INTO observations(transmission_id,path_json,raw_hex,timestamp) VALUES(?,?,?,?)`, id, path, raw, at.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	return s, node, packet
}
func TestScopeAuditFindings(t *testing.T) {
	s, node, packet := auditFixture(t)
	now := time.Now().UTC()
	at := now.Format(time.RFC3339)
	node(scopeContractMultiPK, "multi", "#dk, eu, #dk", at)
	node(scopeContractEmptyPK, "empty", "", at)
	node(scopeContractWildPK, "wild", "*", now.Add(-8*24*time.Hour).Format(time.RFC3339))
	dk := "dk"
	foreign := "#other"
	unknown := ""
	packet(1, 0, &dk, `["e1","e1","e3"]`, "0001020304", now)
	packet(1, 0, &dk, `["e1","e3"]`, "0001020304", now)
	packet(2, 0, &foreign, `["e1"]`, "0001020304", now)
	packet(3, 1, nil, `["e1"]`, "01", now)
	packet(4, 0, &unknown, `["e1"]`, "0001020304", now)
	packet(5, 2, &foreign, `["e1"]`, "02", now)
	resp, err := s.buildScopeAudit("24h", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Rows) != 3 {
		t.Fatalf("rows=%+v", resp.Rows)
	}
	byKey := make(map[string]ScopeAuditRow)
	for _, row := range resp.Rows {
		byKey[row.PublicKey] = row
	}
	multi := byKey[scopeContractMultiPK]
	if multi.PublicKey != scopeContractMultiPK || multi.Forwarded != 4 || multi.UnscopedObserved != 1 || multi.UnknownScopeObserved != 1 || !multi.WildcardContradiction || len(multi.NotObserved) != 1 || multi.NotObserved[0] != "eu" || len(multi.UndeclaredObserved) != 1 || multi.Status != "incomplete" {
		t.Fatalf("multi=%+v", multi)
	}
	if byKey[scopeContractWildPK].Status != "no-evidence" || !byKey[scopeContractWildPK].StaleDeclaration {
		t.Fatalf("wild=%+v", byKey[scopeContractWildPK])
	}
	if byKey[scopeContractEmptyPK].Status != "findings" || byKey[scopeContractEmptyPK].Forwarded != 1 {
		t.Fatalf("empty=%+v", byKey[scopeContractEmptyPK])
	}
}
func TestScopeAuditCollisionAndWindow(t *testing.T) {
	s, node, packet := auditFixture(t)
	now := time.Now().UTC()
	node(scopeContractMultiPK, "declared", "dk", now.Format(time.RFC3339))
	if _, err := s.db.conn.Exec(`INSERT INTO nodes(public_key,name,role) VALUES(?,'other','repeater')`, "e1ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"); err != nil {
		t.Fatal(err)
	}
	dk := "dk"
	packet(1, 0, &dk, `["e1"]`, "0001020304", now)
	packet(2, 0, &dk, `["`+scopeContractMultiPK+`"]`, "0001020304", now.Add(-2*time.Hour))
	r, err := s.buildScopeAudit("1h", now)
	if err != nil {
		t.Fatal(err)
	}
	if r.AmbiguousHops != 1 || r.Rows[0].Forwarded != 0 || !r.Rows[0].IncompleteEvidence || r.Rows[0].AmbiguousHops != 1 {
		t.Fatalf("1h=%+v", r)
	}
	r, err = s.buildScopeAudit("24h", now)
	if err != nil {
		t.Fatal(err)
	}
	if r.Rows[0].Forwarded != 1 {
		t.Fatalf("24h=%+v", r.Rows[0])
	}
}
func TestScopeAuditHTTPAndCache(t *testing.T) {
	s, node, _ := auditFixture(t)
	node(scopeContractMultiPK, "visible", "dk", time.Now().UTC().Format(time.RFC3339))
	get := func(window string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleScopeAudit(w, httptest.NewRequest("GET", "/api/scope-audit?window="+window, nil))
		return w
	}
	if w := get("bad"); w.Code != 400 {
		t.Fatalf("status=%d", w.Code)
	}
	w := get("24h")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var r ScopeAuditResponse
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 1 {
		t.Fatal(r)
	}
	s.db.conn.Exec(`UPDATE nodes SET configured_scope='eu' WHERE public_key=?`, scopeContractMultiPK)
	w = get("24h")
	json.Unmarshal(w.Body.Bytes(), &r)
	if r.Rows[0].ConfiguredScope != "dk" {
		t.Fatal("cache missed")
	}
	s.cfg.SetNodeBlacklist([]string{scopeContractMultiPK})
	w = get("24h")
	json.Unmarshal(w.Body.Bytes(), &r)
	if len(r.Rows) != 0 || r.Summary.Total != 0 {
		t.Fatalf("cached identity leaked: %s", w.Body.String())
	}
}

func TestScopeAuditObservationRouteAndScope(t *testing.T) {
	s, node, packet := auditFixture(t)
	now := time.Now().UTC()
	node(scopeContractMultiPK, "relay", "#dk,*", now.Format(time.RFC3339))
	dk := "dk"
	packet(1, 0, &dk, `["e1"]`, "0001020304", now)
	// Same payload crosses a scope boundary; canonical scope is no longer proof.
	packet(1, 0, &dk, `["e1"]`, "0005060304", now)
	packet(1, 0, &dk, `["e1"]`, "01", now)
	// Canonical flood transmission has a direct observation to a second target.
	node(scopeContractEmptyPK, "direct-only", "", now.Format(time.RFC3339))
	packet(1, 0, &dk, `["e3"]`, "02", now)
	// Random unknown hops are not collisions with known nodes.
	packet(2, 0, &dk, `["ff", "malformed"]`, "0001020304", now)
	r, err := s.buildScopeAudit("24h", now)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range r.Rows {
		if row.PublicKey == scopeContractMultiPK {
			if row.Forwarded != 1 || row.UnknownScopeObserved != 1 || row.UnscopedObserved != 1 || len(row.ObservedScopes) != 1 || row.WildcardContradiction {
				t.Fatalf("boundary evidence=%+v", row)
			}
		} else if row.Forwarded != 0 {
			t.Fatalf("direct path counted=%+v", row)
		}
	}
	if r.AmbiguousHops != 0 {
		t.Fatalf("unknown hops reported as collisions: %d", r.AmbiguousHops)
	}
}

func TestScopeAuditMissingDeclarationAndSevenDayWindow(t *testing.T) {
	s, node, packet := auditFixture(t)
	now := time.Now().UTC()
	node(scopeContractMultiPK, "relay", "dk", now.Format(time.RFC3339))
	if _, err := s.db.conn.Exec(`INSERT INTO nodes(public_key,name,role) VALUES(?,'not-declared','repeater')`, scopeContractNonePK); err != nil {
		t.Fatal(err)
	}
	dk := "dk"
	packet(1, 0, &dk, `["e1"]`, "0001020304", now.Add(-48*time.Hour))
	packet(2, 0, &dk, `["e1"]`, "0001020304", now.Add(-8*24*time.Hour))
	r, err := s.buildScopeAudit("7d", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 1 || r.Rows[0].Forwarded != 1 || r.Rows[0].Status != "consistent" {
		t.Fatalf("7d=%+v", r)
	}
}

func BenchmarkScopeAudit30KPackets2KNodes(b *testing.B) {
	db := setupTestDB(b)
	for _, q := range []string{"ALTER TABLE nodes ADD COLUMN configured_scope TEXT", "ALTER TABLE nodes ADD COLUMN configured_scope_at TEXT", "ALTER TABLE transmissions ADD COLUMN scope_name TEXT", "CREATE INDEX audit_obs_tx ON observations(transmission_id)", "CREATE INDEX audit_obs_ts ON observations(timestamp)"} {
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
	insertNode, err := tx.Prepare(`INSERT INTO nodes(public_key,name,role,configured_scope,configured_scope_at) VALUES(?,'relay','repeater','dk',?)`)
	if err != nil {
		b.Fatal(err)
	}
	keys := make([]string, 2000)
	for i := range keys {
		keys[i] = fmt.Sprintf("%06x%058x", i+1, 0)
		if _, err := insertNode.Exec(keys[i], stamp); err != nil {
			b.Fatal(err)
		}
	}
	insertNode.Close()
	insertTx, err := tx.Prepare(`INSERT INTO transmissions(id,hash,raw_hex,first_seen,route_type,scope_name) VALUES(?,?,'0001020304',?,0,'dk')`)
	if err != nil {
		b.Fatal(err)
	}
	insertObs, err := tx.Prepare(`INSERT INTO observations(transmission_id,path_json,raw_hex,timestamp) VALUES(?,?,'0001020304',?)`)
	if err != nil {
		b.Fatal(err)
	}
	for i := 1; i <= 30000; i++ {
		if _, err := insertTx.Exec(i, fmt.Sprint(i), stamp); err != nil {
			b.Fatal(err)
		}
		path := fmt.Sprintf(`["%s","%s","%s","%s"]`, keys[i%2000][:6], keys[(i+1)%2000][:6], keys[(i+2)%2000][:6], keys[(i+3)%2000][:6])
		for j := 0; j < 3; j++ {
			if _, err := insertObs.Exec(i, path, now.Unix()); err != nil {
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
	s := &Server{db: db, cfg: &Config{}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r, err := s.buildScopeAudit("24h", now)
		if err != nil {
			b.Fatal(err)
		}
		if len(r.Rows) != 2000 || r.Rows[0].Forwarded != 60 {
			b.Fatalf("unexpected audit: rows=%d forwarded=%d", len(r.Rows), r.Rows[0].Forwarded)
		}
	}
}

func TestScopeAuditCacheSingleflightExpiryAndErrors(t *testing.T) {
	var cache scopeAuditCache[ScopeAuditResponse]
	var fills atomic.Int32
	start := make(chan struct{})
	release := make(chan struct{})
	build := func() (*ScopeAuditResponse, error) {
		if fills.Add(1) == 1 {
			close(start)
		}
		<-release
		return &ScopeAuditResponse{Window: "24h"}, nil
	}
	var wg sync.WaitGroup
	results := make(chan *ScopeAuditResponse, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := cache.load("24h", build)
			if err != nil {
				t.Error(err)
			}
			results <- r
		}()
	}
	<-start
	close(release)
	wg.Wait()
	close(results)
	var first *ScopeAuditResponse
	for r := range results {
		if first == nil {
			first = r
		}
		if r != first {
			t.Fatal("concurrent callers received distinct builds")
		}
	}
	if fills.Load() != 1 {
		t.Fatalf("fills=%d want 1", fills.Load())
	}
	cache.mu.Lock()
	entry := cache.entries["24h"]
	entry.at = time.Now().Add(-scopeAuditTTL - time.Second)
	cache.entries["24h"] = entry
	cache.mu.Unlock()
	if _, err := cache.load("24h", func() (*ScopeAuditResponse, error) { fills.Add(1); return &ScopeAuditResponse{}, nil }); err != nil {
		t.Fatal(err)
	}
	if fills.Load() != 2 {
		t.Fatal("expired entry not refreshed")
	}
	for i := 0; i < 2; i++ {
		if _, err := cache.load("7d", func() (*ScopeAuditResponse, error) { fills.Add(1); return nil, fmt.Errorf("failed") }); err == nil {
			t.Fatal("expected error")
		}
	}
	if fills.Load() != 4 {
		t.Fatal("failed build was cached")
	}
}

func TestScopeAuditCachedHiddenNameAndUnsupportedSchema(t *testing.T) {
	s, node, _ := auditFixture(t)
	node(scopeContractMultiPK, "visible", "", time.Now().UTC().Format(time.RFC3339))
	s.cfg.SetHiddenNamePrefixes([]string{"private-"})
	get := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleScopeAudit(w, httptest.NewRequest("GET", "/api/scope-audit", nil))
		return w
	}
	if w := get(); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if _, err := s.db.conn.Exec(`UPDATE nodes SET name='private-fixture' WHERE public_key=?`, scopeContractMultiPK); err != nil {
		t.Fatal(err)
	}
	w := get()
	var r ScopeAuditResponse
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 0 {
		t.Fatalf("hidden rename leaked: %s", w.Body.String())
	}
	plain, _ := setupTestServer(t)
	w = httptest.NewRecorder()
	plain.handleScopeAudit(w, httptest.NewRequest("GET", "/api/scope-audit", nil))
	if w.Code != 500 {
		t.Fatalf("unsupported schema status=%d", w.Code)
	}
}

// The shared singleflight build must not die with the first caller's
// connection: a coalesced caller whose own context stays live must still
// get a response when another caller's context is canceled mid-build.
func TestScopeAuditHandlerSingleflightShareSurvivesFirstCallerDisconnect(t *testing.T) {
	s, node, packet := auditFixture(t)
	now := time.Now().UTC()
	node(scopeContractMultiPK, "relay", "dk", now.Format(time.RFC3339))
	dk := "dk"
	// Enough observations that the build takes long enough for both callers
	// to join the same singleflight call before the first's cancel lands.
	for i := 1; i <= 6000; i++ {
		packet(i, 0, &dk, `["e1","aa","bb","cc"]`, "0001020304", now)
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := httptest.NewRequest("GET", "/api/scope-audit?window=24h", nil).WithContext(cancelCtx)
		w := httptest.NewRecorder()
		s.handleScopeAudit(w, req)
		codes <- w.Code
	}()
	// Give caller A a head start so it reliably becomes the singleflight
	// leader (whose context drives the build) before caller B joins it.
	time.Sleep(2 * time.Millisecond)
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := httptest.NewRequest("GET", "/api/scope-audit?window=24h", nil)
		w := httptest.NewRecorder()
		s.handleScopeAudit(w, req)
		codes <- w.Code
	}()
	time.Sleep(2 * time.Millisecond)
	cancel() // simulate caller A's connection dropping mid-build
	wg.Wait()
	close(codes)
	sawLiveCallerSucceed := false
	for code := range codes {
		if code == 200 {
			sawLiveCallerSucceed = true
		}
	}
	if !sawLiveCallerSucceed {
		t.Fatal("a disconnecting caller's context cancellation took down the shared build for a still-connected caller")
	}
}

// A declared repeater that the observers table marks listener-only
// (can_relay=0, #1290) must not have its own hop token confirmed as a
// relay — the same exclusion resolveWithContext applies elsewhere.
func TestScopeAuditListenerOnlyNodeExcludedFromForwardedCount(t *testing.T) {
	s, node, packet := auditFixture(t)
	now := time.Now().UTC()
	at := now.Format(time.RFC3339)
	node(scopeContractMultiPK, "listener", "dk", at)
	if _, err := s.db.conn.Exec(`ALTER TABLE observers ADD COLUMN can_relay INTEGER DEFAULT 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.conn.Exec(
		`INSERT INTO observers(id,name,last_seen,first_seen,packet_count,can_relay) VALUES(?,'obs',?,?,1,0)`,
		scopeContractMultiPK, at, at); err != nil {
		t.Fatal(err)
	}
	dk := "dk"
	packet(1, 0, &dk, `["e1"]`, "0001020304", now)
	r, err := s.buildScopeAudit("24h", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 1 || r.Rows[0].Forwarded != 0 || r.Rows[0].Status != "no-evidence" {
		t.Fatalf("listener-only hop counted as a confirmed relay: %+v", r.Rows[0])
	}
}

// load rechecks the cache after entering singleflight (comment on scopeAuditCache.load):
// a fill that lands in the gap between the outer miss and entering singleflight
// must be reused instead of triggering a redundant rebuild.
func TestScopeAuditCacheRecheckInsideSingleflightReusesConcurrentFill(t *testing.T) {
	var cache scopeAuditCache[ScopeAuditResponse]
	want := &ScopeAuditResponse{Window: "fresh-from-concurrent-fill"}
	cache.recheckHook = func() {
		cache.mu.Lock()
		cache.entries = map[string]scopeAuditCacheEntry[ScopeAuditResponse]{"24h": {response: want, at: time.Now()}}
		cache.mu.Unlock()
	}
	var built bool
	got, err := cache.load("24h", func() (*ScopeAuditResponse, error) {
		built = true
		return &ScopeAuditResponse{Window: "should-not-be-used"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if built {
		t.Fatal("build ran despite a concurrent fill landing before the singleflight entry")
	}
	if got != want {
		t.Fatalf("got=%+v want=%+v", got, want)
	}
}

func TestScopeAuditNoDeclarationsSkipsObservationScan(t *testing.T) {
	s, _, _ := auditFixture(t)
	// If the cold path scans observations despite no declarations, this fails.
	if _, err := s.db.conn.Exec(`DROP TABLE observations`); err != nil {
		t.Fatal(err)
	}
	r, err := s.buildScopeAudit("24h", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 0 || r.Summary.Total != 0 || r.AmbiguousHops != 0 {
		t.Fatalf("empty audit=%+v", r)
	}
}
