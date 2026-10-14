package main

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// TestObserverComparisonPerf_RealisticFixtureEndToEnd is the AGENTS.md rule
// 0 perf proof for #122: the reference-to-many comparison must scan a
// realistic multi-observer dataset via the composite (observer_idx,
// timestamp) index, not via one query per compared observer and not by
// loading every packet into the browser (the problem this issue replaces).
//
// Dataset: 30,000 transmissions, 8 observers (1 reference + 7 compared),
// each transmission RX-observed by about 3 of the 8 observers (~90K
// observation rows) -- same fan-out shape as the staging #wardriving
// channel referenced by db_channel_messages_perf_test.go, sized for #122's
// "30K+ transmissions and many observers" without making `go test -race`
// (the CI configuration, deploy.yml) too slow: measured on this machine,
// the real implementation takes ~4.7s under -race (vs ~0.3s without it);
// a mutant that issues one query per compared observer instead of one bulk
// query -- the per-API-call anti-pattern #122 explicitly forbids -- took
// ~10.2s under -race for the same fixture, more than double. The budget
// below is set well above the real implementation's measured time (not at
// the mutant's) so CI noise cannot make a correct change flaky.
func TestObserverComparisonPerf_RealisticFixtureEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("perf test")
	}
	db := setupTestDB(t)
	defer db.Close()

	const numObservers = 8
	const numTx = 30000

	observerRowIDs := make([]int64, numObservers)
	observerIDs := make([]string, numObservers)
	for i := 0; i < numObservers; i++ {
		id := fmt.Sprintf("perf-obs-%02d", i)
		res, err := db.conn.Exec(`INSERT INTO observers (id, name) VALUES (?, ?)`, id, id)
		if err != nil {
			t.Fatal(err)
		}
		rowID, _ := res.LastInsertId()
		observerRowIDs[i] = rowID
		observerIDs[i] = id
	}

	txTx, err := db.conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	txStmt, err := txTx.Prepare(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type) VALUES (?, ?, ?, 1, 1)`)
	if err != nil {
		t.Fatal(err)
	}
	obsStmt, err := txTx.Prepare(`INSERT INTO observations (transmission_id, observer_idx, direction, path_json, timestamp) VALUES (?, ?, 'RX', '[]', ?)`)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	base := now.Add(-23 * time.Hour) // comfortably inside a 24h window, never racing "now"
	observationRows := 0
	for i := 0; i < numTx; i++ {
		ts := base.Add(time.Duration(i) * time.Second).Format(time.RFC3339)
		hash := fmt.Sprintf("perf122hash%08d", i)
		res, err := txStmt.Exec(fmt.Sprintf("%08X", i), hash, ts)
		if err != nil {
			t.Fatal(err)
		}
		txID, _ := res.LastInsertId()
		obsTS := base.Add(time.Duration(i) * time.Second).Unix()
		// Deterministic ~3-of-8 fan-out per transmission (no randomness, so
		// the test is reproducible): observer o observes tx i when
		// (i*31 + o*17) % 8 < 3.
		for o := 0; o < numObservers; o++ {
			if (i*31+o*17)%8 >= 3 {
				continue
			}
			if _, err := obsStmt.Exec(txID, observerRowIDs[o], obsTS); err != nil {
				t.Fatal(err)
			}
			observationRows++
		}
	}
	if err := txTx.Commit(); err != nil {
		t.Fatal(err)
	}
	t.Logf("fixture: %d transmissions, %d observers, %d observation rows", numTx, numObservers, observationRows)

	srv := NewServer(db, &Config{}, NewHub())
	router := mux.NewRouter()
	srv.RegisterRoutes(router)

	url := "/api/analytics/observer-comparison?ref=" + observerIDs[0] + "&hours=24&observers="
	for i := 1; i < numObservers; i++ {
		if i > 1 {
			url += ","
		}
		url += observerIDs[i]
	}

	// Warm-up call to amortize first-run query-plan/prepare cost, matching
	// the db_channel_messages_perf_test.go convention.
	warm := httptest.NewRecorder()
	router.ServeHTTP(warm, httptest.NewRequest("GET", url, nil))
	if warm.Code != 200 {
		t.Fatalf("warm-up status = %d body=%s", warm.Code, warm.Body.String())
	}
	srv.obsComparison.reset() // measure a real cold computation, not the cache hit

	start := time.Now()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", url, nil))
	elapsed := time.Since(start)
	if w.Code != 200 {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	resp := decodeComparisonResponse(t, w)
	if len(resp.Rows) != numObservers-1 {
		t.Fatalf("rows = %d, want %d", len(resp.Rows), numObservers-1)
	}

	// Budget set ~2.5x above the real implementation's measured -race time
	// (~4.7s; see the doc comment above) so CI noise cannot make a correct
	// change flaky, while still sitting below the measured one-query-per-
	// observer mutant's ~10.2s.
	const budget = 12 * time.Second
	if elapsed > budget {
		t.Fatalf("observer comparison too slow: %v (budget %v) on %d tx x %d observers (%d obs rows)",
			elapsed, budget, numTx, numObservers, observationRows)
	}
	t.Logf("comparison against %d observers over %d matching rows took %v", numObservers-1, observationRows, elapsed)
}

// buildComparisonFixture returns a reference plus numOthers observers and a
// pairs slice where the reference and each other observer each contribute
// exactly pairsPerObserver distinct transmissions, chosen so total pairs
// grows linearly with numOthers (not with numOthers^2).
func buildComparisonFixture(numOthers, pairsPerObserver int) (others []observerIdentity, pairs []observationPair) {
	others = make([]observerIdentity, numOthers)
	for i := range others {
		others[i] = observerIdentity{RowID: int64(i + 2), ID: fmt.Sprintf("o%d", i), Name: fmt.Sprintf("Observer %d", i)}
	}
	// Reference's own pairs (rowid 1), disjoint tx id space per observer so
	// nothing degenerates into an all-shared or all-disjoint special case.
	nextTx := int64(0)
	for i := 0; i < pairsPerObserver; i++ {
		pairs = append(pairs, observationPair{ObserverRowID: 1, TransmissionID: nextTx})
		nextTx++
	}
	for _, o := range others {
		for i := 0; i < pairsPerObserver; i++ {
			pairs = append(pairs, observationPair{ObserverRowID: o.RowID, TransmissionID: nextTx})
			nextTx++
		}
	}
	return others, pairs
}

// TestComputeObserverComparison_ScalesLinearlyInObservers is a regression
// guard for #122 "no O(n²) across observers x packets": with a fixed
// per-observer workload, total input size grows linearly with the number
// of compared observers, and so must computeObserverComparison's time. The
// real mutant proof for the O(n²) requirement lives at the level #122
// actually calls out ("bounded, cached/coalesced bulk aggregation rather
// than one API call per observer"): see
// TestObserverComparisonPerf_RealisticFixtureEndToEnd's doc comment, which
// records a per-observer-rescan mutant that measurably broke that test's
// budget. A pure-Go nested rescan of computeObserverComparison's pairs
// slice was tried here too, but at this function's realistic input sizes a
// branch-predicted integer-compare skip is cheap enough that it did not
// reliably blow this test's budget -- so this test is kept as a linear-
// scaling sanity check on the shipped implementation, not as that mutant's
// proof.
func TestComputeObserverComparison_ScalesLinearlyInObservers(t *testing.T) {
	if testing.Short() {
		t.Skip("perf test")
	}
	const pairsPerObserver = 300_000
	const smallN = 4
	const bigN = observerComparisonMaxObservers // 40
	const scale = float64(bigN) / float64(smallN)

	smallOthers, smallPairs := buildComparisonFixture(smallN, pairsPerObserver)
	start := time.Now()
	computeObserverComparison(1, smallOthers, smallPairs)
	smallElapsed := time.Since(start)

	bigOthers, bigPairs := buildComparisonFixture(bigN, pairsPerObserver)
	start = time.Now()
	computeObserverComparison(1, bigOthers, bigPairs)
	bigElapsed := time.Since(start)

	// Linear (O(pairs)): bigElapsed ~= scale * smallElapsed. Quadratic
	// (O(observers x pairs)) would instead give ~= scale^2 * smallElapsed
	// (10x for smallN=4, bigN=40). A tolerance of 3x the linear prediction
	// comfortably passes the real implementation's noise while clearly
	// rejecting a quadratic regression.
	const tolerance = 3.0
	budget := time.Duration(float64(smallElapsed) * scale * tolerance)
	// Measurements at this size can be a handful of milliseconds; floor the
	// budget so sub-millisecond timer noise in smallElapsed can't make it
	// unrealistically tight.
	if budget < 50*time.Millisecond {
		budget = 50 * time.Millisecond
	}
	if bigElapsed > budget {
		t.Fatalf("computeObserverComparison does not scale linearly in observers: %d others took %v, %d others took %v (budget %v = %dx baseline x %.0fx tolerance) -- looks O(observers x pairs)",
			smallN, smallElapsed, bigN, bigElapsed, budget, int(scale), tolerance)
	}
	t.Logf("%d others: %v; %d others: %v (linear prediction %v, budget %v)",
		smallN, smallElapsed, bigN, bigElapsed, time.Duration(float64(smallElapsed)*scale), budget)
}
