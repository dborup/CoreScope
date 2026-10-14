package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// ---- test fixtures --------------------------------------------------

// newObsComparisonTestServer returns a fresh v3-schema server with an empty
// DB (no seedTestData) so each test controls its own observers/
// transmissions/observations precisely.
func newObsComparisonTestServer(t *testing.T) (*Server, *mux.Router, *DB) {
	t.Helper()
	db := setupTestDB(t)
	srv := NewServer(db, &Config{}, NewHub())
	router := mux.NewRouter()
	srv.RegisterRoutes(router)
	return srv, router, db
}

func insertComparisonObserver(t *testing.T, db *DB, id, name string, inactive int) int64 {
	t.Helper()
	res, err := db.conn.Exec(`INSERT INTO observers (id, name, inactive) VALUES (?, ?, ?)`, id, name, inactive)
	if err != nil {
		t.Fatalf("insert observer %s: %v", id, err)
	}
	rowID, _ := res.LastInsertId()
	return rowID
}

func insertComparisonTx(t *testing.T, db *DB, hash, firstSeen string) int64 {
	t.Helper()
	res, err := db.conn.Exec(
		`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type) VALUES ('AA', ?, ?, 1, 1)`,
		hash, firstSeen)
	if err != nil {
		t.Fatalf("insert transmission %s: %v", hash, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func insertComparisonObs(t *testing.T, db *DB, txID, observerRowID int64, direction string, ts int64, pathJSON string) {
	t.Helper()
	_, err := db.conn.Exec(
		`INSERT INTO observations (transmission_id, observer_idx, direction, path_json, timestamp) VALUES (?, ?, ?, ?, ?)`,
		txID, observerRowID, direction, pathJSON, ts)
	if err != nil {
		t.Fatalf("insert observation tx=%d observer=%d: %v", txID, observerRowID, err)
	}
}

// ---- computeObserverComparison (pure) --------------------------------

func TestComputeObserverComparison_EmptySets(t *testing.T) {
	others := []observerIdentity{{RowID: 2, ID: "other", Name: "Other"}}
	refTotal, rows := computeObserverComparison(1, others, nil)
	if refTotal != 0 {
		t.Fatalf("refTotal = %d, want 0", refTotal)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.Total != 0 || r.Shared != 0 || r.ReferenceOnly != 0 || r.OtherOnly != 0 {
		t.Fatalf("expected all-zero counts, got %+v", r)
	}
	if r.ReferenceCoveragePct != nil {
		t.Fatalf("ReferenceCoveragePct = %v, want nil (zero denominator)", *r.ReferenceCoveragePct)
	}
	if r.OtherCoveragePct != nil {
		t.Fatalf("OtherCoveragePct = %v, want nil (zero denominator)", *r.OtherCoveragePct)
	}
}

func TestComputeObserverComparison_ZeroOverlap(t *testing.T) {
	// Reference saw tx 1,2; other saw disjoint tx 3,4.
	pairs := []observationPair{
		{ObserverRowID: 1, TransmissionID: 1},
		{ObserverRowID: 1, TransmissionID: 2},
		{ObserverRowID: 2, TransmissionID: 3},
		{ObserverRowID: 2, TransmissionID: 4},
	}
	others := []observerIdentity{{RowID: 2, ID: "other", Name: "Other"}}
	refTotal, rows := computeObserverComparison(1, others, pairs)
	if refTotal != 2 {
		t.Fatalf("refTotal = %d, want 2", refTotal)
	}
	r := rows[0]
	if r.Total != 2 || r.Shared != 0 || r.ReferenceOnly != 2 || r.OtherOnly != 2 {
		t.Fatalf("got %+v", r)
	}
	if r.ReferenceCoveragePct == nil || *r.ReferenceCoveragePct != 0 {
		t.Fatalf("ReferenceCoveragePct = %v, want 0", r.ReferenceCoveragePct)
	}
	if r.OtherCoveragePct == nil || *r.OtherCoveragePct != 0 {
		t.Fatalf("OtherCoveragePct = %v, want 0", r.OtherCoveragePct)
	}
}

func TestComputeObserverComparison_PerfectOverlap(t *testing.T) {
	pairs := []observationPair{
		{ObserverRowID: 1, TransmissionID: 1},
		{ObserverRowID: 1, TransmissionID: 2},
		{ObserverRowID: 2, TransmissionID: 1},
		{ObserverRowID: 2, TransmissionID: 2},
	}
	others := []observerIdentity{{RowID: 2, ID: "other", Name: "Other"}}
	refTotal, rows := computeObserverComparison(1, others, pairs)
	if refTotal != 2 {
		t.Fatalf("refTotal = %d, want 2", refTotal)
	}
	r := rows[0]
	if r.Total != 2 || r.Shared != 2 || r.ReferenceOnly != 0 || r.OtherOnly != 0 {
		t.Fatalf("got %+v", r)
	}
	if r.ReferenceCoveragePct == nil || *r.ReferenceCoveragePct != 100 {
		t.Fatalf("ReferenceCoveragePct = %v, want 100", r.ReferenceCoveragePct)
	}
	if r.OtherCoveragePct == nil || *r.OtherCoveragePct != 100 {
		t.Fatalf("OtherCoveragePct = %v, want 100", r.OtherCoveragePct)
	}
}

// TestComputeObserverComparison_PartialAsymmetric mirrors the issue's own
// worked example: reference = 1000, other = 800, shared = 600 -> 60% of
// reference traffic, 75% of other traffic. Scaled down 100x for a tiny
// fixture; the percentages must match exactly.
func TestComputeObserverComparison_PartialAsymmetric(t *testing.T) {
	var pairs []observationPair
	// Reference sees tx 1..10 (10 total).
	for tx := int64(1); tx <= 10; tx++ {
		pairs = append(pairs, observationPair{ObserverRowID: 1, TransmissionID: tx})
	}
	// Other sees tx 1..6 (shared, 6) plus tx 100,101 (other-only, 2) = 8 total.
	for tx := int64(1); tx <= 6; tx++ {
		pairs = append(pairs, observationPair{ObserverRowID: 2, TransmissionID: tx})
	}
	pairs = append(pairs,
		observationPair{ObserverRowID: 2, TransmissionID: 100},
		observationPair{ObserverRowID: 2, TransmissionID: 101},
	)

	others := []observerIdentity{{RowID: 2, ID: "other", Name: "Other"}}
	refTotal, rows := computeObserverComparison(1, others, pairs)
	if refTotal != 10 {
		t.Fatalf("refTotal = %d, want 10", refTotal)
	}
	r := rows[0]
	if r.Total != 8 {
		t.Fatalf("Total = %d, want 8", r.Total)
	}
	if r.Shared != 6 {
		t.Fatalf("Shared = %d, want 6", r.Shared)
	}
	if r.ReferenceOnly != 4 {
		t.Fatalf("ReferenceOnly = %d, want 4 (1000-600 scaled: 10-6)", r.ReferenceOnly)
	}
	if r.OtherOnly != 2 {
		t.Fatalf("OtherOnly = %d, want 2 (800-600 scaled: 8-6)", r.OtherOnly)
	}
	if r.ReferenceCoveragePct == nil || *r.ReferenceCoveragePct != 60 {
		t.Fatalf("ReferenceCoveragePct = %v, want 60 (matches issue's 60%% example)", r.ReferenceCoveragePct)
	}
	if r.OtherCoveragePct == nil || *r.OtherCoveragePct != 75 {
		t.Fatalf("OtherCoveragePct = %v, want 75 (matches issue's 75%% example)", r.OtherCoveragePct)
	}
}

func TestComputeObserverComparison_RefTotalZero_PctNil(t *testing.T) {
	// Reference has no RX pairs at all in the window; other has some.
	pairs := []observationPair{
		{ObserverRowID: 2, TransmissionID: 1},
	}
	others := []observerIdentity{{RowID: 2, ID: "other", Name: "Other"}}
	refTotal, rows := computeObserverComparison(1, others, pairs)
	if refTotal != 0 {
		t.Fatalf("refTotal = %d, want 0", refTotal)
	}
	r := rows[0]
	if r.ReferenceCoveragePct != nil {
		t.Fatalf("ReferenceCoveragePct = %v, want nil when |R|=0 (unavailable, not 0%%)", *r.ReferenceCoveragePct)
	}
	if r.OtherCoveragePct == nil || *r.OtherCoveragePct != 0 {
		t.Fatalf("OtherCoveragePct = %v, want 0 (|O|=1, |S|=0)", r.OtherCoveragePct)
	}
	if r.OtherOnly != 1 {
		t.Fatalf("OtherOnly = %d, want 1", r.OtherOnly)
	}
}

func TestComputeObserverComparison_OtherTotalZero_PctNil(t *testing.T) {
	pairs := []observationPair{
		{ObserverRowID: 1, TransmissionID: 1},
	}
	others := []observerIdentity{{RowID: 2, ID: "other", Name: "Other"}}
	refTotal, rows := computeObserverComparison(1, others, pairs)
	if refTotal != 1 {
		t.Fatalf("refTotal = %d, want 1", refTotal)
	}
	r := rows[0]
	if r.OtherCoveragePct != nil {
		t.Fatalf("OtherCoveragePct = %v, want nil when |O|=0", *r.OtherCoveragePct)
	}
	if r.ReferenceCoveragePct == nil || *r.ReferenceCoveragePct != 0 {
		t.Fatalf("ReferenceCoveragePct = %v, want 0", r.ReferenceCoveragePct)
	}
}

func TestComputeObserverComparison_MultipleOthersSortedByName(t *testing.T) {
	pairs := []observationPair{
		{ObserverRowID: 1, TransmissionID: 1},
		{ObserverRowID: 1, TransmissionID: 2},
		{ObserverRowID: 2, TransmissionID: 1}, // "Zed"
		{ObserverRowID: 3, TransmissionID: 2}, // "Alpha"
	}
	others := []observerIdentity{
		{RowID: 2, ID: "obsZ", Name: "Zed"},
		{RowID: 3, ID: "obsA", Name: "Alpha"},
	}
	_, rows := computeObserverComparison(1, others, pairs)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].ObserverName != "Alpha" || rows[1].ObserverName != "Zed" {
		t.Fatalf("rows not sorted by name: %+v", rows)
	}
}

// ---- parseObserverComparisonObserverIDs ------------------------------

func TestParseObserverComparisonObserverIDs(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		ref     string
		want    []string
		wantErr bool
	}{
		{"empty", "", "ref", nil, false},
		{"single", "a", "ref", []string{"a"}, false},
		{"dedupe and trim", " a, b ,a,b", "ref", []string{"a", "b"}, false},
		{"ref in list", "a,ref,b", "ref", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseObserverComparisonObserverIDs(c.raw, c.ref)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
		})
	}
}

// ---- observerComparisonCacheKey ---------------------------------------

func TestObserverComparisonCacheKey_OrderIndependent(t *testing.T) {
	a := []observerIdentity{{ID: "b"}, {ID: "a"}, {ID: "c"}}
	b := []observerIdentity{{ID: "c"}, {ID: "a"}, {ID: "b"}}
	k1 := observerComparisonCacheKey("ref", 24, a)
	k2 := observerComparisonCacheKey("ref", 24, b)
	if k1 != k2 {
		t.Fatalf("keys differ by input order: %q vs %q", k1, k2)
	}
}

func TestObserverComparisonCacheKey_DifferentHoursDiffer(t *testing.T) {
	others := []observerIdentity{{ID: "a"}}
	if observerComparisonCacheKey("ref", 1, others) == observerComparisonCacheKey("ref", 24, others) {
		t.Fatal("keys for different windows must differ")
	}
}

func TestObserverComparisonCacheKey_LongKeyHashed(t *testing.T) {
	others := make([]observerIdentity, 0, 40)
	for i := 0; i < 40; i++ {
		others = append(others, observerIdentity{ID: fmt.Sprintf("observer-with-a-fairly-long-id-%03d", i)})
	}
	key := observerComparisonCacheKey("reference-observer-id", 24, others)
	if len(key) > observerComparisonCacheMaxKeyBytes {
		t.Fatalf("key not bounded: %d bytes", len(key))
	}
	if !strings.HasPrefix(key, "sha256:") {
		t.Fatalf("expected a hashed key, got %q", key)
	}
}

// ---- observerComparisonCache (ceiling + TTL + singleflight) ----------

func TestObserverComparisonCache_TTLExpiry(t *testing.T) {
	c := &observerComparisonCache{}
	now := time.Now()
	c.put("k", &ObserverComparisonResponse{}, now)
	if _, ok := c.get("k", now.Add(observerComparisonCacheTTL-time.Millisecond)); !ok {
		t.Fatal("expected a hit just before TTL expiry")
	}
	if _, ok := c.get("k", now.Add(observerComparisonCacheTTL+time.Millisecond)); ok {
		t.Fatal("expected a miss just after TTL expiry")
	}
}

// TestObserverComparisonCache_CeilingEvictsNearestExpiry is the #430
// pattern's mutant target: fill the cache to its ceiling with entries at
// staggered expiries, then insert one more NEW key. Exactly the entry
// nearest expiry must be evicted; a refresh of an EXISTING key must never
// evict anything else.
func TestObserverComparisonCache_CeilingEvictsNearestExpiry(t *testing.T) {
	c := &observerComparisonCache{}
	base := time.Now()
	for i := 0; i < observerComparisonCacheMaxKeys; i++ {
		key := fmt.Sprintf("k%03d", i)
		// Staggered expiry: k000 expires soonest.
		c.entries = ensureEntries(c.entries)
		c.put(key, &ObserverComparisonResponse{}, base.Add(time.Duration(i)*time.Second-observerComparisonCacheTTL))
	}
	if c.len() != observerComparisonCacheMaxKeys {
		t.Fatalf("len = %d, want %d", c.len(), observerComparisonCacheMaxKeys)
	}

	// Refreshing an existing key must not evict any other entry.
	c.put("k005", &ObserverComparisonResponse{}, base)
	if c.len() != observerComparisonCacheMaxKeys {
		t.Fatalf("refreshing an existing key changed len to %d", c.len())
	}

	// A brand new key past the ceiling evicts exactly the one nearest
	// expiry (k000, since none have expired relative to `base`).
	c.put("new-key", &ObserverComparisonResponse{}, base)
	if c.len() != observerComparisonCacheMaxKeys {
		t.Fatalf("len after eviction = %d, want %d (ceiling held)", c.len(), observerComparisonCacheMaxKeys)
	}
	if _, ok := c.get("k000", base); ok {
		t.Fatal("expected k000 (nearest expiry) to be evicted")
	}
	if _, ok := c.get("k001", base); !ok {
		t.Fatal("k001 should still be present -- only the single nearest-expiry victim is evicted")
	}
	if _, ok := c.get("new-key", base); !ok {
		t.Fatal("new-key should have been inserted")
	}
}

func ensureEntries(m map[string]*observerComparisonCacheEntry) map[string]*observerComparisonCacheEntry {
	if m == nil {
		return make(map[string]*observerComparisonCacheEntry)
	}
	return m
}

func TestObserverComparisonCache_SingleflightCollapsesConcurrentMisses(t *testing.T) {
	c := &observerComparisonCache{}
	var computeCalls atomic.Int32
	results, errs := callConcurrently(16, func(i int) (interface{}, error) {
		return c.load("same-key", time.Now(), func() (*ObserverComparisonResponse, error) {
			computeCalls.Add(1)
			time.Sleep(20 * time.Millisecond) // widen the race window
			return &ObserverComparisonResponse{Reference: ObserverComparisonReference{ID: "ref"}}, nil
		})
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
	}
	if n := computeCalls.Load(); n != 1 {
		t.Fatalf("compute ran %d times, want 1 (singleflight should collapse concurrent misses)", n)
	}
	for i, r := range results {
		resp := r.(*ObserverComparisonResponse)
		if resp.Reference.ID != "ref" {
			t.Fatalf("call %d got unexpected result: %+v", i, resp)
		}
	}
}

// ---- handler: request validation (#430 "reject before work") --------

func TestHandleObserverComparison_MissingRef(t *testing.T) {
	_, router, _ := newObsComparisonTestServer(t)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/observer-comparison?hours=24&observers=a", nil))
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleObserverComparison_InvalidHours(t *testing.T) {
	_, router, _ := newObsComparisonTestServer(t)
	for _, hours := range []string{"", "0", "5", "48", "-1", "abc"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/observer-comparison?ref=r&hours="+hours+"&observers=a", nil))
		if w.Code != 400 {
			t.Fatalf("hours=%q: status = %d, want 400", hours, w.Code)
		}
	}
}

func TestHandleObserverComparison_MissingObservers(t *testing.T) {
	_, router, _ := newObsComparisonTestServer(t)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/observer-comparison?ref=r&hours=24", nil))
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleObserverComparison_RefInObservers(t *testing.T) {
	_, router, _ := newObsComparisonTestServer(t)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/observer-comparison?ref=r&hours=24&observers=a,r,b", nil))
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleObserverComparison_TooManyObservers(t *testing.T) {
	_, router, _ := newObsComparisonTestServer(t)
	ids := make([]string, observerComparisonMaxObservers+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("o%d", i)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/observer-comparison?ref=r&hours=24&observers="+strings.Join(ids, ","), nil))
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleObserverComparison_ObserversParamTooLong(t *testing.T) {
	_, router, _ := newObsComparisonTestServer(t)
	huge := strings.Repeat("x", observerComparisonObserversParamMaxLen+1)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/observer-comparison?ref=r&hours=24&observers="+huge, nil))
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleObserverComparison_RefUnknown_404(t *testing.T) {
	_, router, db := newObsComparisonTestServer(t)
	insertComparisonObserver(t, db, "real", "Real", 0)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/observer-comparison?ref=nope&hours=24&observers=real", nil))
	if w.Code != 404 {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleObserverComparison_V3SchemaGuard(t *testing.T) {
	db := setupTestDBv2(t) // observer_id column, not observer_idx -- isV3() is false
	srv := NewServer(db, &Config{}, NewHub())
	router := mux.NewRouter()
	srv.RegisterRoutes(router)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/observer-comparison?ref=r&hours=24&observers=a", nil))
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400 for non-v3 schema; body=%s", w.Code, w.Body.String())
	}
}

// ---- handler: end-to-end correctness ---------------------------------

func decodeComparisonResponse(t *testing.T, w *httptest.ResponseRecorder) ObserverComparisonResponse {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp ObserverComparisonResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	return resp
}

// TestHandleObserverComparison_RXOnly is the core correctness requirement:
// TX and unknown-direction observations must never silently count as RX.
func TestHandleObserverComparison_RXOnly(t *testing.T) {
	_, router, db := newObsComparisonTestServer(t)
	refIdx := insertComparisonObserver(t, db, "ref", "Ref", 0)
	otherIdx := insertComparisonObserver(t, db, "other", "Other", 0)

	now := time.Now().UTC()
	firstSeen := now.Format(time.RFC3339)
	ts := now.Add(-time.Minute).Unix() // safely inside the window, never racing the handler's own time.Now()

	txRX := insertComparisonTx(t, db, "rx-tx", firstSeen)
	txTX := insertComparisonTx(t, db, "tx-tx", firstSeen)
	txUnknown := insertComparisonTx(t, db, "unknown-dir-tx", firstSeen)
	txEmpty := insertComparisonTx(t, db, "empty-dir-tx", firstSeen)
	txLowerRX := insertComparisonTx(t, db, "lower-rx-tx", firstSeen)

	insertComparisonObs(t, db, txRX, refIdx, "RX", ts, "[]")
	insertComparisonObs(t, db, txRX, otherIdx, "RX", ts, "[]")

	insertComparisonObs(t, db, txTX, refIdx, "TX", ts, "[]")
	insertComparisonObs(t, db, txTX, otherIdx, "TX", ts, "[]")

	insertComparisonObs(t, db, txUnknown, refIdx, "WEIRD", ts, "[]")
	insertComparisonObs(t, db, txUnknown, otherIdx, "WEIRD", ts, "[]")

	insertComparisonObs(t, db, txEmpty, refIdx, "", ts, "[]")
	insertComparisonObs(t, db, txEmpty, otherIdx, "", ts, "[]")

	insertComparisonObs(t, db, txLowerRX, refIdx, "rx", ts, "[]")
	insertComparisonObs(t, db, txLowerRX, otherIdx, "rx", ts, "[]")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/observer-comparison?ref=ref&hours=1&observers=other", nil))
	resp := decodeComparisonResponse(t, w)

	// Only the "RX" and "rx" rows count: refTotal=2, shared=2.
	if resp.Reference.Total != 2 {
		t.Fatalf("Reference.Total = %d, want 2 (TX/unknown/empty direction must be excluded)", resp.Reference.Total)
	}
	if len(resp.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(resp.Rows))
	}
	if resp.Rows[0].Shared != 2 || resp.Rows[0].Total != 2 {
		t.Fatalf("got %+v, want Total=2 Shared=2", resp.Rows[0])
	}
}

// TestHandleObserverComparison_DedupPerTransmission: #122 "count each
// transmission_id once per observer, even when multiple paths/observations
// exist." Two observation rows for the same (tx, observer) with different
// paths must count as one.
func TestHandleObserverComparison_DedupPerTransmission(t *testing.T) {
	_, router, db := newObsComparisonTestServer(t)
	refIdx := insertComparisonObserver(t, db, "ref", "Ref", 0)
	otherIdx := insertComparisonObserver(t, db, "other", "Other", 0)

	now := time.Now().UTC()
	firstSeen := now.Format(time.RFC3339)
	ts := now.Add(-time.Minute).Unix() // safely inside the window, never racing the handler's own time.Now()

	tx := insertComparisonTx(t, db, "multi-path-tx", firstSeen)
	// Reference observes the same transmission via two different paths.
	insertComparisonObs(t, db, tx, refIdx, "RX", ts, `["aa","bb"]`)
	insertComparisonObs(t, db, tx, refIdx, "RX", ts, `["cc"]`)
	insertComparisonObs(t, db, tx, otherIdx, "RX", ts, "[]")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/observer-comparison?ref=ref&hours=1&observers=other", nil))
	resp := decodeComparisonResponse(t, w)

	if resp.Reference.Total != 1 {
		t.Fatalf("Reference.Total = %d, want 1 (same transmission via 2 paths must count once)", resp.Reference.Total)
	}
	if resp.Rows[0].Shared != 1 {
		t.Fatalf("Shared = %d, want 1", resp.Rows[0].Shared)
	}
}

// TestHandleObserverComparison_WindowUsesObservationTimestamp: #122 "Use
// one explicit UTC interval ... based on observation timestamps, not only
// transmission first_seen." A transmission first seen long before the
// window, but re-observed by the reference inside the window, must count;
// one whose only observation falls outside the window must not.
func TestHandleObserverComparison_WindowUsesObservationTimestamp(t *testing.T) {
	_, router, db := newObsComparisonTestServer(t)
	refIdx := insertComparisonObserver(t, db, "ref", "Ref", 0)
	otherIdx := insertComparisonObserver(t, db, "other", "Other", 0)

	now := time.Now().UTC()
	oldFirstSeen := now.Add(-10 * 24 * time.Hour).Format(time.RFC3339)
	insideTS := now.Add(-30 * time.Minute).Unix()
	outsideTS := now.Add(-2 * time.Hour).Unix() // outside a 1h window

	txReobserved := insertComparisonTx(t, db, "old-tx-reobserved", oldFirstSeen)
	insertComparisonObs(t, db, txReobserved, refIdx, "RX", insideTS, "[]")
	insertComparisonObs(t, db, txReobserved, otherIdx, "RX", insideTS, "[]")

	txOutside := insertComparisonTx(t, db, "recent-tx-outside-window", now.Format(time.RFC3339))
	insertComparisonObs(t, db, txOutside, refIdx, "RX", outsideTS, "[]")
	insertComparisonObs(t, db, txOutside, otherIdx, "RX", outsideTS, "[]")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/observer-comparison?ref=ref&hours=1&observers=other", nil))
	resp := decodeComparisonResponse(t, w)

	if resp.Reference.Total != 1 {
		t.Fatalf("Reference.Total = %d, want 1 (only the re-observed tx is inside the window)", resp.Reference.Total)
	}
	if resp.Rows[0].Shared != 1 {
		t.Fatalf("Shared = %d, want 1", resp.Rows[0].Shared)
	}
}

func TestHandleObserverComparison_UnknownObserverSilentlyExcluded(t *testing.T) {
	_, router, db := newObsComparisonTestServer(t)
	refIdx := insertComparisonObserver(t, db, "ref", "Ref", 0)
	otherIdx := insertComparisonObserver(t, db, "other", "Other", 0)
	now := time.Now().UTC()
	ts := now.Add(-time.Minute).Unix() // safely inside the window, never racing the handler's own time.Now()
	tx := insertComparisonTx(t, db, "tx1", now.Format(time.RFC3339))
	insertComparisonObs(t, db, tx, refIdx, "RX", ts, "[]")
	insertComparisonObs(t, db, tx, otherIdx, "RX", ts, "[]")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/observer-comparison?ref=ref&hours=1&observers=other,does-not-exist", nil))
	resp := decodeComparisonResponse(t, w)
	if len(resp.Rows) != 1 {
		t.Fatalf("rows = %d, want 1 (unknown id silently excluded)", len(resp.Rows))
	}
	if resp.Rows[0].ObserverID != "other" {
		t.Fatalf("got %+v", resp.Rows[0])
	}
}

func TestHandleObserverComparison_InactiveObserverExcluded(t *testing.T) {
	_, router, db := newObsComparisonTestServer(t)
	refIdx := insertComparisonObserver(t, db, "ref", "Ref", 0)
	insertComparisonObserver(t, db, "inactive-one", "InactiveOne", 1)
	otherIdx := insertComparisonObserver(t, db, "other", "Other", 0)
	now := time.Now().UTC()
	ts := now.Add(-time.Minute).Unix() // safely inside the window, never racing the handler's own time.Now()
	tx := insertComparisonTx(t, db, "tx1", now.Format(time.RFC3339))
	insertComparisonObs(t, db, tx, refIdx, "RX", ts, "[]")
	insertComparisonObs(t, db, tx, otherIdx, "RX", ts, "[]")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/observer-comparison?ref=ref&hours=1&observers=other,inactive-one", nil))
	resp := decodeComparisonResponse(t, w)
	if len(resp.Rows) != 1 || resp.Rows[0].ObserverID != "other" {
		t.Fatalf("expected the inactive observer excluded, got %+v", resp.Rows)
	}
}

func TestHandleObserverComparison_BlacklistedObserverExcluded(t *testing.T) {
	db := setupTestDB(t)
	cfg := &Config{ObserverBlacklist: []string{"blacklisted-one"}}
	srv := NewServer(db, cfg, NewHub())
	router := mux.NewRouter()
	srv.RegisterRoutes(router)

	refIdx := insertComparisonObserver(t, db, "ref", "Ref", 0)
	insertComparisonObserver(t, db, "blacklisted-one", "BlacklistedOne", 0)
	otherIdx := insertComparisonObserver(t, db, "other", "Other", 0)
	now := time.Now().UTC()
	ts := now.Add(-time.Minute).Unix() // safely inside the window, never racing the handler's own time.Now()
	tx := insertComparisonTx(t, db, "tx1", now.Format(time.RFC3339))
	insertComparisonObs(t, db, tx, refIdx, "RX", ts, "[]")
	insertComparisonObs(t, db, tx, otherIdx, "RX", ts, "[]")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/observer-comparison?ref=ref&hours=1&observers=other,blacklisted-one", nil))
	resp := decodeComparisonResponse(t, w)
	if len(resp.Rows) != 1 || resp.Rows[0].ObserverID != "other" {
		t.Fatalf("expected the blacklisted observer excluded, got %+v", resp.Rows)
	}
}

func TestHandleObserverComparison_RefBlacklisted_404(t *testing.T) {
	db := setupTestDB(t)
	cfg := &Config{ObserverBlacklist: []string{"ref"}}
	srv := NewServer(db, cfg, NewHub())
	router := mux.NewRouter()
	srv.RegisterRoutes(router)
	insertComparisonObserver(t, db, "ref", "Ref", 0)
	insertComparisonObserver(t, db, "other", "Other", 0)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/observer-comparison?ref=ref&hours=1&observers=other", nil))
	if w.Code != 404 {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleObserverComparison_AllOthersIneligible_400(t *testing.T) {
	_, router, db := newObsComparisonTestServer(t)
	insertComparisonObserver(t, db, "ref", "Ref", 0)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/observer-comparison?ref=ref&hours=1&observers=nope1,nope2", nil))
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleObserverComparison_WindowEchoed asserts the response's window
// matches the requested hours and End-Start equals exactly that duration
// (the exact [start,end) boundary itself is covered at the DB-scan level in
// TestScanObserverComparisonPairs_ExactBoundaries, which does not depend on
// wall-clock timing).
func TestHandleObserverComparison_WindowEchoed(t *testing.T) {
	_, router, db := newObsComparisonTestServer(t)
	refIdx := insertComparisonObserver(t, db, "ref", "Ref", 0)
	otherIdx := insertComparisonObserver(t, db, "other", "Other", 0)
	now := time.Now().UTC()
	tx := insertComparisonTx(t, db, "tx1", now.Format(time.RFC3339))
	insertComparisonObs(t, db, tx, refIdx, "RX", now.Unix(), "[]")
	insertComparisonObs(t, db, tx, otherIdx, "RX", now.Unix(), "[]")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/observer-comparison?ref=ref&hours=6&observers=other", nil))
	resp := decodeComparisonResponse(t, w)

	if resp.Window.Hours != 6 {
		t.Fatalf("Window.Hours = %d, want 6", resp.Window.Hours)
	}
	start, err := time.Parse(time.RFC3339, resp.Window.Start)
	if err != nil {
		t.Fatalf("Start not RFC3339: %v", err)
	}
	end, err := time.Parse(time.RFC3339, resp.Window.End)
	if err != nil {
		t.Fatalf("End not RFC3339: %v", err)
	}
	if got := end.Sub(start); got != 6*time.Hour {
		t.Fatalf("End-Start = %v, want 6h", got)
	}
}

// ---- scanObserverComparisonPairs (DB-level, exact boundaries) --------

// TestScanObserverComparisonPairs_ExactBoundaries verifies the half-open
// [start, end) interval precisely, without depending on wall-clock timing:
// start is itself included, end itself is excluded.
func TestScanObserverComparisonPairs_ExactBoundaries(t *testing.T) {
	db := setupTestDB(t)
	refIdx := insertComparisonObserver(t, db, "ref", "Ref", 0)

	start := time.Unix(1_700_000_000, 0).UTC()
	end := start.Add(time.Hour)

	txAtStart := insertComparisonTx(t, db, "at-start", start.Format(time.RFC3339))
	insertComparisonObs(t, db, txAtStart, refIdx, "RX", start.Unix(), "[]")

	txBeforeStart := insertComparisonTx(t, db, "before-start", start.Format(time.RFC3339))
	insertComparisonObs(t, db, txBeforeStart, refIdx, "RX", start.Unix()-1, "[]")

	txAtEnd := insertComparisonTx(t, db, "at-end", start.Format(time.RFC3339))
	insertComparisonObs(t, db, txAtEnd, refIdx, "RX", end.Unix(), "[]")

	txJustBeforeEnd := insertComparisonTx(t, db, "just-before-end", start.Format(time.RFC3339))
	insertComparisonObs(t, db, txJustBeforeEnd, refIdx, "RX", end.Unix()-1, "[]")

	pairs, err := db.scanObserverComparisonPairs([]int64{refIdx}, start, end)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	got := map[int64]bool{}
	for _, p := range pairs {
		got[p.TransmissionID] = true
	}
	if !got[txAtStart] {
		t.Error("expected tx at exactly `start` to be included (inclusive lower bound)")
	}
	if got[txBeforeStart] {
		t.Error("expected tx before `start` to be excluded")
	}
	if got[txAtEnd] {
		t.Error("expected tx at exactly `end` to be excluded (exclusive upper bound)")
	}
	if !got[txJustBeforeEnd] {
		t.Error("expected tx just before `end` to be included")
	}
}

func TestScanObserverComparisonPairs_ScanRowLimit(t *testing.T) {
	db := setupTestDB(t)
	refIdx := insertComparisonObserver(t, db, "ref", "Ref", 0)
	now := time.Now().UTC()

	old := observerComparisonScanRowLimit
	observerComparisonScanRowLimit = 3
	t.Cleanup(func() { observerComparisonScanRowLimit = old })
	for i := 0; i < 5; i++ {
		tx := insertComparisonTx(t, db, fmt.Sprintf("tx%d", i), now.Format(time.RFC3339))
		insertComparisonObs(t, db, tx, refIdx, "RX", now.Unix(), "[]")
	}
	_, err := db.scanObserverComparisonPairs([]int64{refIdx}, now.Add(-time.Hour), now.Add(time.Hour))
	if err == nil {
		t.Fatal("expected an explicit error when the scan row limit is exceeded, got nil (silent truncation is forbidden)")
	}
}

// PR #450 review: a single over-long id (ref or inside observers) must be
// rejected as 400 before any DB lookup, not fall through to a 404 / lookup.
func TestHandleObserverComparison_OverlongIDRejectedBeforeWork(t *testing.T) {
	_, router, _ := newObsComparisonTestServer(t)
	long := strings.Repeat("x", observerComparisonIDMaxLen+1)
	for name, q := range map[string]string{
		"ref":      "ref=" + long + "&hours=24&observers=a",
		"observer": "ref=r&hours=24&observers=a," + long,
	} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/api/analytics/observer-comparison?"+q, nil))
		if w.Code != 400 {
			t.Fatalf("%s: status = %d, want 400; body=%s", name, w.Code, w.Body.String())
		}
	}
}

// #122 "Inspect query plans": the window scan must be driven by the
// (observer_idx, timestamp) index, never a full table scan of observations.
func TestScanObserverComparisonPairs_QueryPlanUsesObserverTimestampIndex(t *testing.T) {
	db := setupTestDB(t)
	// The composite index is built by the ingestor's async migration
	// obs_observer_ts_idx_v1 (cmd/ingestor/db.go), not by the server or the
	// test schema; create it with the same DDL so the plan reflects a
	// migrated production database.
	if _, err := db.conn.Exec(`CREATE INDEX IF NOT EXISTS idx_observations_observer_idx_timestamp ON observations(observer_idx, timestamp)`); err != nil {
		t.Fatal(err)
	}
	refIdx := insertComparisonObserver(t, db, "ref", "Ref", 0)
	now := time.Now().UTC()
	tx := insertComparisonTx(t, db, "tx-plan", now.Format(time.RFC3339))
	insertComparisonObs(t, db, tx, refIdx, "RX", now.Unix(), "[]")

	rows, err := db.conn.Query(`EXPLAIN QUERY PLAN `+observerComparisonScanSQL("?"),
		refIdx, now.Add(-time.Hour).Unix(), now.Unix()+1)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	joined := strings.Join(plan, " | ")
	if !strings.Contains(joined, "SEARCH") || !strings.Contains(joined, "idx_observations_observer_idx_timestamp") {
		t.Fatalf("scan must SEARCH observations via the (observer_idx, timestamp) index, got plan: %s", joined)
	}
	if strings.Contains(joined, "SCAN observations") {
		t.Fatalf("full table scan of observations: %s", joined)
	}
}
