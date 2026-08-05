package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// --- test clock -------------------------------------------------------------

type testClock struct{ t time.Time }

func (c *testClock) Now() time.Time          { return c.t }
func (c *testClock) Advance(d time.Duration) { c.t = c.t.Add(d) }
func (c *testClock) Set(t time.Time)         { c.t = t }

// --- fixture ------------------------------------------------------------

type engineFixture struct {
	srv    *Server
	store  *PingScoreHistoryStore
	clock  *testClock
	config pingScoreHistoryEngineConfig
	engine *pingScoreHistoryEngine
}

// setupEngineFixture builds a Server+DB (reusing setupPingScoresFixture's
// observers/nodes -- pingobsa/b/c with known positions), a fresh temp-file
// history store, and an engine with an injectable clock.
//
// setupEngineFixture deliberately applies NO hidden defaults -- config is
// passed to newPingScoreHistoryEngine exactly as given. Per the Phase 4D
// review: a test fixture must never have defaults the production
// constructor doesn't also have, or a test could pass while exercising
// different behavior than production would for the same config. Every
// zero value (SettleDebounce, DeepSweepBatchSize, RetentionDuration,
// MaxEdgeKm) is itself a real, documented, valid behavior -- see
// pingScoreHistoryEngineConfig's own doc comment -- not a placeholder
// this fixture should silently replace. A test that wants
// defaultPingScoreHistoryEngineConfig's production defaults calls that
// function explicitly.
func setupEngineFixture(t *testing.T, config pingScoreHistoryEngineConfig) *engineFixture {
	t.Helper()
	srv, _ := setupPingScoresFixture(t)
	dir := t.TempDir()
	store, err := OpenPingScoreHistoryStore(filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatalf("open history store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	clock := &testClock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	engine, err := newPingScoreHistoryEngine(srv, store, clock.Now, config)
	if err != nil {
		t.Fatalf("newPingScoreHistoryEngine: %v", err)
	}
	return &engineFixture{srv: srv, store: store, clock: clock, config: config, engine: engine}
}

// reopenEngine rebuilds the engine from the store's current persisted
// state (simulating a process restart) -- used by tests that want to
// confirm persisted state independently of the in-memory index.
func (fx *engineFixture) reopenEngine(t *testing.T) {
	t.Helper()
	engine, err := newPingScoreHistoryEngine(fx.srv, fx.store, fx.clock.Now, fx.config)
	if err != nil {
		t.Fatalf("reopen engine: %v", err)
	}
	fx.engine = engine
}

func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// --- constructor tests ----------------------------------------------------

func TestNewPingScoreHistoryEngine_EmptyStore(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{})
	if fx.engine.index.Len() != 0 {
		t.Errorf("index.Len() = %d, want 0", fx.engine.index.Len())
	}
}

func TestNewPingScoreHistoryEngine_LoadsExistingEntries(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenPingScoreHistoryStore(filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	seed := PingScoreHistoryEntry{TxID: 5, Hash: "seed00001", Timestamp: "2026-01-01T00:00:00Z", StationCount: 1, ComputedAt: "2026-01-01T00:00:00Z"}
	if err := store.UpsertAndDelete([]PingScoreHistoryEntry{seed}, nil); err != nil {
		t.Fatal(err)
	}

	srv, _ := setupPingScoresFixture(t)
	clock := &testClock{t: time.Now()}
	engine, err := newPingScoreHistoryEngine(srv, store, clock.Now, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := engine.index.Get(5)
	if !ok || got.Hash != "seed00001" {
		t.Errorf("index.Get(5) = %+v, %v, want the seeded entry", got, ok)
	}
}

func TestNewPingScoreHistoryEngine_RejectsInvalidRelayJSON(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenPingScoreHistoryStore(filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Bypass the normal upsert path (which always writes valid JSON) to
	// simulate corruption -- direct SQL against the store's own table.
	if _, err := store.conn.Exec(`INSERT INTO ping_score_history_entries (tx_id, hash, timestamp, station_count, deepest_hops, relay_pubkeys_json, computed_at) VALUES (1, 'h', 't', 1, 0, 'not valid json', 'c')`); err != nil {
		t.Fatal(err)
	}

	srv, _ := setupPingScoresFixture(t)
	_, err = newPingScoreHistoryEngine(srv, store, time.Now, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 10})
	if err == nil {
		t.Fatal("want an error for invalid persisted relay_pubkeys_json")
	}
}

func TestNewPingScoreHistoryEngine_DoesNotMutatePersistedState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "h.db")
	store, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	seed := PingScoreHistoryEntry{TxID: 1, Hash: "h", Timestamp: "t", ComputedAt: "c"}
	if err := store.UpsertAndDelete([]PingScoreHistoryEntry{seed}, nil); err != nil {
		t.Fatal(err)
	}
	store.Close()

	// Reopen and construct an engine -- construction alone must not change
	// anything on disk.
	store2, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	srv, _ := setupPingScoresFixture(t)
	if _, err := newPingScoreHistoryEngine(srv, store2, time.Now, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 10}); err != nil {
		t.Fatal(err)
	}

	entries, err := store2.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Hash != "h" {
		t.Errorf("entries = %+v, want the single unmodified seeded row", entries)
	}
}

// --- basic functional cycles ----------------------------------------------

func TestCycle_EmptyTriggerList(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{})
	snap, err := fx.engine.Cycle()
	if err != nil {
		t.Fatal(err)
	}
	if snap == nil || snap.TotalPings != 0 {
		t.Errorf("snap = %+v, want TotalPings=0", snap)
	}
	if fx.engine.index.Len() != 0 {
		t.Errorf("index.Len() = %d, want 0", fx.engine.index.Len())
	}
}

func TestCycle_FirstBootstrap(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{})
	txID := seedPingTrigger(t, fx.srv, "bootstrap0001", "#b", "Alice", "2026-01-15T10:00:00Z")
	seedPingObservation(t, fx.srv, txID, "pingobsa", 9.0, `[]`, `[]`, 1736935200)
	seedPingObservation(t, fx.srv, txID, "pingobsb", 6.0, `["aa"]`, `["pkrelay1"]`, 1736935210)

	snap, err := fx.engine.Cycle()
	if err != nil {
		t.Fatal(err)
	}
	if snap.TotalPings != 1 {
		t.Errorf("TotalPings = %d, want 1", snap.TotalPings)
	}
	entry, ok := fx.engine.index.Get(txID)
	if !ok {
		t.Fatal("index missing the bootstrapped entry")
	}
	if entry.Unscorable {
		t.Error("entry.Unscorable = true, want false (this ping has real data)")
	}
	if entry.StationCount != 2 {
		t.Errorf("entry.StationCount = %d, want 2", entry.StationCount)
	}
	if entry.Settled {
		t.Error("entry.Settled = true, want false (brand-new entries start unsettled)")
	}
	if entry.StableSince == "" {
		t.Error("entry.StableSince is empty, want the cycle's now")
	}

	// Persisted, not just in-memory.
	persisted, err := fx.store.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 1 || persisted[0].TxID != txID {
		t.Errorf("persisted = %+v, want the one bootstrapped entry", persisted)
	}
}

func TestCycle_NewTriggerAfterBootstrap(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{})
	if _, err := fx.engine.Cycle(); err != nil { // empty first cycle
		t.Fatal(err)
	}

	txID := seedPingTrigger(t, fx.srv, "afterboot0001", "#a", "Bob", "2026-01-16T10:00:00Z")
	seedPingObservation(t, fx.srv, txID, "pingobsa", 9.0, `[]`, `[]`, 1737025200)

	fx.clock.Advance(time.Minute)
	snap, err := fx.engine.Cycle()
	if err != nil {
		t.Fatal(err)
	}
	if snap.TotalPings != 1 {
		t.Errorf("TotalPings = %d, want 1", snap.TotalPings)
	}
	if _, ok := fx.engine.index.Get(txID); !ok {
		t.Error("new trigger's entry missing from index after the second cycle")
	}
}

// --- settle state machine --------------------------------------------------

func TestCycle_UnchangedUnsettledEntryBeforeDebounce(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: 10 * time.Minute})
	txID := seedPingTrigger(t, fx.srv, "debounce00001", "#d", "S", "2026-01-15T10:00:00Z")
	seedPingObservation(t, fx.srv, txID, "pingobsa", 9.0, `[]`, `[]`, 1736935200)

	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	entry, _ := fx.engine.index.Get(txID)
	if entry.Settled {
		t.Fatal("Settled = true after the very first cycle, want false")
	}
	stableSince := entry.StableSince

	fx.clock.Advance(5 * time.Minute) // < 10-minute debounce
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	entry2, _ := fx.engine.index.Get(txID)
	if entry2.Settled {
		t.Error("Settled = true before the debounce elapsed, want false")
	}
	if entry2.StableSince != stableSince {
		t.Errorf("StableSince changed from %q to %q without a fingerprint change", stableSince, entry2.StableSince)
	}
}

func TestCycle_SettlesAfterDebounce(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: 10 * time.Minute})
	txID := seedPingTrigger(t, fx.srv, "settle000001", "#d", "S", "2026-01-15T10:00:00Z")
	seedPingObservation(t, fx.srv, txID, "pingobsa", 9.0, `[]`, `[]`, 1736935200)

	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	fx.clock.Advance(11 * time.Minute) // > 10-minute debounce, no new observations
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	entry, _ := fx.engine.index.Get(txID)
	if !entry.Settled {
		t.Error("Settled = false after the debounce elapsed with no fingerprint change, want true")
	}
}

func TestCycle_FingerprintChangesBeforeSettle(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: 10 * time.Minute})
	txID := seedPingTrigger(t, fx.srv, "fpchange0001", "#d", "S", "2026-01-15T10:00:00Z")
	seedPingObservation(t, fx.srv, txID, "pingobsa", 9.0, `[]`, `[]`, 1736935200)

	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	entry1, _ := fx.engine.index.Get(txID)
	if entry1.StationCount != 1 {
		t.Fatalf("StationCount = %d, want 1", entry1.StationCount)
	}

	fx.clock.Advance(3 * time.Minute) // still < debounce
	seedPingObservation(t, fx.srv, txID, "pingobsb", 6.0, `["aa"]`, `["pkrelay1"]`, 1736935210)
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	entry2, _ := fx.engine.index.Get(txID)
	if entry2.Settled {
		t.Error("Settled = true right after a fingerprint change, want false")
	}
	if entry2.StationCount != 2 {
		t.Errorf("StationCount = %d, want 2 (new observation picked up)", entry2.StationCount)
	}
	if entry2.StableSince != fx.clock.Now().UTC().Format(time.RFC3339) {
		t.Errorf("StableSince = %q, want reset to the current cycle's now", entry2.StableSince)
	}
}

// TestCycle_FingerprintChangesAfterSettle_ViaDeepSweep covers the review's
// "fingerprint ændres efter settle, hvis den er valgt til relevant
// kontrol" scenario: deep-sweep is the ONLY mechanism that re-examines a
// Settled entry's fingerprint at all, so a post-settle change is only ever
// caught when that entry happens to be selected for deep-sweep.
func TestCycle_FingerprintChangesAfterSettle_ViaDeepSweep(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: 10 * time.Minute, DeepSweepBatchSize: 100})
	txID := seedPingTrigger(t, fx.srv, "postsettle001", "#d", "S", "2026-01-15T10:00:00Z")
	seedPingObservation(t, fx.srv, txID, "pingobsa", 9.0, `[]`, `[]`, 1736935200)

	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	fx.clock.Advance(11 * time.Minute)
	if _, err := fx.engine.Cycle(); err != nil { // settles
		t.Fatal(err)
	}
	entry, _ := fx.engine.index.Get(txID)
	if !entry.Settled {
		t.Fatal("sanity check failed: entry did not settle")
	}

	// New observation arrives on an already-settled entry.
	seedPingObservation(t, fx.srv, txID, "pingobsc", 4.0, `["aa","bb"]`, `["pkrelay1","pkrelay2"]`, 1736935900)
	fx.clock.Advance(time.Minute) // DeepSweepBatchSize=100 covers this single entry, so it's swept every cycle
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	entry2, _ := fx.engine.index.Get(txID)
	if entry2.Settled {
		t.Error("Settled = true after deep-sweep found a changed fingerprint, want false")
	}
	if entry2.StationCount != 2 {
		t.Errorf("StationCount = %d, want 2 (pingobsa + pingobsc -- deep-sweep's recompute picked up the new observation)", entry2.StationCount)
	}
}

// --- deep-sweep rotation and path-change ------------------------------------

// settleEntry runs a bootstrap cycle for trigger, then advances the clock
// past the debounce and runs a second cycle so the resulting entry is
// Settled=true with LastDeepSweptAt still empty (it was a fingerprint
// candidate on the settle cycle, never a deep-sweep candidate).
func settleEntry(t *testing.T, fx *engineFixture) {
	t.Helper()
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	fx.clock.Advance(fx.config.SettleDebounce + time.Minute)
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
}

func TestCycle_DeterministicDeepSweepRotation(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: 10 * time.Minute, DeepSweepBatchSize: 2})
	txA := seedPingTrigger(t, fx.srv, "rotation000001", "#r", "S", "2026-01-15T10:00:00Z")
	seedPingObservation(t, fx.srv, txA, "pingobsa", 9.0, `[]`, `[]`, 1736935200)
	txB := seedPingTrigger(t, fx.srv, "rotation000002", "#r", "S", "2026-01-15T10:01:00Z")
	seedPingObservation(t, fx.srv, txB, "pingobsa", 9.0, `[]`, `[]`, 1736935260)
	txC := seedPingTrigger(t, fx.srv, "rotation000003", "#r", "S", "2026-01-15T10:02:00Z")
	seedPingObservation(t, fx.srv, txC, "pingobsa", 9.0, `[]`, `[]`, 1736935320)

	if _, err := fx.engine.Cycle(); err != nil { // bootstrap all 3
		t.Fatal(err)
	}
	fx.clock.Advance(11 * time.Minute)
	if _, err := fx.engine.Cycle(); err != nil { // settle all 3 (no deep-sweep yet: all were fingerprint candidates)
		t.Fatal(err)
	}
	for _, id := range []int64{txA, txB, txC} {
		e, _ := fx.engine.index.Get(id)
		if !e.Settled || e.LastDeepSweptAt != "" {
			t.Fatalf("sanity check failed for tx_id=%d: %+v, want Settled=true LastDeepSweptAt=\"\"", id, e)
		}
	}

	fx.clock.Advance(time.Minute)
	if _, err := fx.engine.Cycle(); err != nil { // batch=2, all tied on empty LastDeepSweptAt -> lowest 2 tx_ids first
		t.Fatal(err)
	}
	eA, _ := fx.engine.index.Get(txA)
	eB, _ := fx.engine.index.Get(txB)
	eC, _ := fx.engine.index.Get(txC)
	if eA.LastDeepSweptAt == "" || eB.LastDeepSweptAt == "" {
		t.Errorf("txA/txB should have been swept first (lowest tx_id tie-break): A=%q B=%q", eA.LastDeepSweptAt, eB.LastDeepSweptAt)
	}
	if eC.LastDeepSweptAt != "" {
		t.Errorf("txC.LastDeepSweptAt = %q, want empty (batch size 2, not yet its turn)", eC.LastDeepSweptAt)
	}

	fx.clock.Advance(time.Minute)
	if _, err := fx.engine.Cycle(); err != nil { // txC (empty) sorts first, then the lowest of {A,B} (tied, now non-empty)
		t.Fatal(err)
	}
	eA2, _ := fx.engine.index.Get(txA)
	eC2, _ := fx.engine.index.Get(txC)
	if eC2.LastDeepSweptAt == "" {
		t.Error("txC should have been swept on the second rotation (it had the oldest/empty LastDeepSweptAt)")
	}
	if eA2.LastDeepSweptAt == eA.LastDeepSweptAt {
		t.Error("txA should have been swept again (tie-broken by lowest tx_id among A/B), but LastDeepSweptAt didn't change")
	}
}

func TestCycle_DeepSweepPathChange(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: 10 * time.Minute, DeepSweepBatchSize: 100})
	txID := seedPingTrigger(t, fx.srv, "pathchange0001", "#p", "S", "2026-01-15T10:00:00Z")
	seedPingObservation(t, fx.srv, txID, "pingobsa", 9.0, `[]`, `[]`, 1736935200)
	seedPingObservation(t, fx.srv, txID, "pingobsb", 6.0, `["aa"]`, `["pkrelay1"]`, 1736935210)
	settleEntry(t, fx)

	entry1, _ := fx.engine.index.Get(txID)
	if entry1.FarthestKm == nil {
		t.Fatal("sanity check failed: FarthestKm is nil before the reposition")
	}
	before := *entry1.FarthestKm

	// Move pingobsb far away -- doesn't touch observations/transmissions
	// at all, so the fingerprint stays unchanged, but GetPacketPathsBulk's
	// recomputed distance must differ.
	if _, err := fx.srv.db.conn.Exec(`UPDATE nodes SET lat = 10.0, lon = 10.0 WHERE public_key = 'pingobsb'`); err != nil {
		t.Fatal(err)
	}

	fx.clock.Advance(time.Minute)
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	entry2, _ := fx.engine.index.Get(txID)
	if entry2.FarthestKm == nil || *entry2.FarthestKm == before {
		t.Errorf("FarthestKm = %v, want a DIFFERENT value from %v after the node reposition", entry2.FarthestKm, before)
	}
	if !entry2.Settled {
		t.Error("Settled = false after a path-only change (fingerprint unchanged), want true (unchanged)")
	}
}

// --- DataPruned proof --------------------------------------------------------

// setupSettledPingWithGoneData settles one ping, then deletes its
// underlying observations/transmission rows (simulating the ingestor
// having pruned the raw packet past its retention window) -- so a
// subsequent deep-sweep's GetPacketPathsBulk call finds nothing for it,
// while ping_triggers (and the history entry) still has the row.
func setupSettledPingWithGoneData(t *testing.T, fx *engineFixture, hash, firstSeen string) int64 {
	t.Helper()
	txID := seedPingTrigger(t, fx.srv, hash, "#g", "S", firstSeen)
	seedPingObservation(t, fx.srv, txID, "pingobsa", 9.0, `[]`, `[]`, mustUnixSeconds(t, firstSeen))
	settleEntry(t, fx)

	entry, _ := fx.engine.index.Get(txID)
	if entry.Unscorable || entry.StationCount == 0 {
		t.Fatalf("sanity check failed: entry not validly scored before data removal: %+v", entry)
	}

	if _, err := fx.srv.db.conn.Exec(`DELETE FROM observations WHERE transmission_id IN (SELECT id FROM transmissions WHERE hash = ?)`, hash); err != nil {
		t.Fatal(err)
	}
	return txID
}

func mustUnixSeconds(t *testing.T, rfc3339 string) int64 {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		t.Fatal(err)
	}
	return ts.Unix()
}

func TestCycle_EmptyDeepSweepBeforeRetention_NotDataPruned(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 100, RetentionDuration: 30 * 24 * time.Hour})
	firstSeen := fx.clock.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339) // recent, well within the 30-day retention
	txID := setupSettledPingWithGoneData(t, fx, "pruneearly0001", firstSeen)

	before, _ := fx.engine.index.Get(txID)
	fx.clock.Advance(2 * time.Minute)
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	after, _ := fx.engine.index.Get(txID)
	if after.DataPruned {
		t.Error("DataPruned = true, want false -- the trigger is not yet older than the retention window")
	}
	if after.StationCount != before.StationCount || after.DeepestHops != before.DeepestHops {
		t.Errorf("path facts changed on an empty deep-sweep result: before=%+v after=%+v", before, after)
	}
}

func TestCycle_EmptyDeepSweepAfterRetention_DataPruned(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 100, RetentionDuration: 30 * 24 * time.Hour})
	firstSeen := fx.clock.Now().Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339) // older than the 30-day retention
	txID := setupSettledPingWithGoneData(t, fx, "pruneexpired01", firstSeen)

	before, _ := fx.engine.index.Get(txID)
	fx.clock.Advance(2 * time.Minute)
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	after, _ := fx.engine.index.Get(txID)
	if !after.DataPruned {
		t.Error("DataPruned = false, want true -- trigger is older than retention and its data is genuinely gone")
	}
	if after.StationCount != before.StationCount || after.DeepestHops != before.DeepestHops || after.Unscorable {
		t.Errorf("existing valid path facts not preserved when DataPruned was set: before=%+v after=%+v", before, after)
	}

	// Still contributes to the all-time snapshot.
	snap, err := fx.srv.buildPingScoresSnapshotFromHistory(mustFetchTriggers(t, fx), fx.engine.index.Entries(), fx.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	if snap.MostHopsPing != nil && snap.MostHopsPing.Hash == "pruneexpired01" {
		found = true
	}
	if !found {
		t.Errorf("DataPruned entry not contributing to the all-time snapshot: %+v", snap.MostHopsPing)
	}

	// Excluded from future deep-sweeps.
	fx.clock.Advance(time.Minute)
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	after2, _ := fx.engine.index.Get(txID)
	if after2.LastDeepSweptAt != after.LastDeepSweptAt {
		t.Error("entry was deep-swept AGAIN after becoming DataPruned -- must be excluded from future deep-sweeps")
	}
}

func mustFetchTriggers(t *testing.T, fx *engineFixture) []pingTriggerRow {
	t.Helper()
	triggers, err := fx.srv.db.fetchPingTriggers()
	if err != nil {
		t.Fatal(err)
	}
	return triggers
}

// TestCycle_NeverScorablePing_NotDataPruned_BootstrapIntegrityRecorded
// covers a trigger that is brand-new to the index AND already older than
// retention AND has no observations at all -- must never become
// DataPruned (there is no prior valid score to preserve), but IS recorded
// via bootstrap-integrity, since it will never be scorable.
func TestCycle_NeverScorablePing_NotDataPruned_BootstrapIntegrityRecorded(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 100, RetentionDuration: 30 * 24 * time.Hour})
	oldFirstSeen := fx.clock.Now().Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)
	txID := seedPingTrigger(t, fx.srv, "neverscorable1", "#n", "S", oldFirstSeen)
	// Deliberately NO observations seeded -- this trigger can never be scored.

	snap, err := fx.engine.Cycle()
	if err != nil {
		t.Fatal(err)
	}
	if snap.TotalPings != 1 {
		t.Errorf("TotalPings = %d, want 1", snap.TotalPings)
	}

	entry, ok := fx.engine.index.Get(txID)
	if !ok {
		t.Fatal("index missing the never-scorable trigger's entry")
	}
	if !entry.Unscorable {
		t.Error("Unscorable = false, want true")
	}
	if entry.DataPruned {
		t.Error("DataPruned = true, want false -- there was never a valid prior score to preserve")
	}

	integrity, err := fx.store.LoadIntegrity()
	if err != nil {
		t.Fatal(err)
	}
	if integrity == nil || integrity.Status != "initial-backfill-incomplete" {
		t.Fatalf("LoadIntegrity() = %+v, want status=initial-backfill-incomplete", integrity)
	}
	if integrity.UnreconstructableCount != 1 {
		t.Errorf("UnreconstructableCount = %d, want 1", integrity.UnreconstructableCount)
	}
	if integrity.TotalTriggers != 1 {
		t.Errorf("TotalTriggers = %d, want 1", integrity.TotalTriggers)
	}
}

func TestCycle_NormalEmptyDatabase_NoBootstrapIntegrityRecorded(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 100, RetentionDuration: 30 * 24 * time.Hour})
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	integrity, err := fx.store.LoadIntegrity()
	if err != nil {
		t.Fatal(err)
	}
	if integrity != nil {
		t.Errorf("LoadIntegrity() = %+v, want nil -- an empty database with no historical loss must not be marked degraded", integrity)
	}
}

func TestCycle_PreviousAbnormalIntegrityNeverAutoCleared(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 100, RetentionDuration: 30 * 24 * time.Hour})
	seeded := PingScoreHistoryIntegrity{Status: "initial-backfill-incomplete", DetectedAt: "2026-01-01T00:00:00Z", UnreconstructableCount: 5}
	if err := fx.store.StoreIntegrity(seeded); err != nil {
		t.Fatal(err)
	}

	// A normal cycle with nothing abnormal to report this time.
	txID := seedPingTrigger(t, fx.srv, "normalcycle001", "#n", "S", fx.clock.Now().UTC().Format(time.RFC3339))
	seedPingObservation(t, fx.srv, txID, "pingobsa", 9.0, `[]`, `[]`, fx.clock.Now().Unix())
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}

	integrity, err := fx.store.LoadIntegrity()
	if err != nil {
		t.Fatal(err)
	}
	if integrity == nil || integrity.Status != "initial-backfill-incomplete" || integrity.UnreconstructableCount != 5 {
		t.Errorf("LoadIntegrity() = %+v, want the ORIGINAL abnormal status untouched", integrity)
	}
}

// --- airtime locking ---------------------------------------------------------

func TestCycle_AirtimeLocking(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: 10 * time.Minute, DeepSweepBatchSize: 100})
	txID := seedPingTrigger(t, fx.srv, "airtimelock001", "#a", "S", "2026-01-15T10:00:00Z")
	seedPingObservation(t, fx.srv, txID, "pingobsa", 9.0, `[]`, `[]`, 1736935200)
	seedPingObservation(t, fx.srv, txID, "pingobsb", 6.0, `["aa"]`, `["pkrelay1"]`, 1736935210)
	if err := fx.srv.store.Load(); err != nil {
		t.Fatalf("reload store: %v", err)
	}
	if !fx.srv.store.WaitIndexesReady(5 * time.Second) {
		t.Fatal("background indexes never became ready after reload")
	}

	settleEntry(t, fx)
	entry1, _ := fx.engine.index.Get(txID)
	if entry1.AirtimeMs == nil || *entry1.AirtimeMs <= 0 {
		t.Fatalf("sanity check failed: AirtimeMs = %v, want a real positive value", entry1.AirtimeMs)
	}
	lockedAirtime := *entry1.AirtimeMs
	lockedRelayCount := entry1.RelayCount

	// Reposition pingobsb -- changes FarthestKm on the next deep-sweep,
	// but must NOT touch the already-locked AirtimeMs/RelayCount.
	if _, err := fx.srv.db.conn.Exec(`UPDATE nodes SET lat = 10.0, lon = 10.0 WHERE public_key = 'pingobsb'`); err != nil {
		t.Fatal(err)
	}
	fx.clock.Advance(time.Minute)
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	entry2, _ := fx.engine.index.Get(txID)
	if entry2.AirtimeMs == nil || *entry2.AirtimeMs != lockedAirtime {
		t.Errorf("AirtimeMs = %v, want it to stay locked at %v", entry2.AirtimeMs, lockedAirtime)
	}
	if entry2.RelayCount != lockedRelayCount {
		t.Errorf("RelayCount = %d, want it to stay locked at %d", entry2.RelayCount, lockedRelayCount)
	}
	if entry2.FarthestKm == nil || entry1.FarthestKm == nil || *entry2.FarthestKm == *entry1.FarthestKm {
		t.Fatalf("sanity check failed: FarthestKm didn't change after the reposition (before=%v after=%v)", entry1.FarthestKm, entry2.FarthestKm)
	}

	// KmPerSecondAirtime must change at MATERIALIZATION since FarthestKm
	// changed, even though AirtimeMs itself stayed locked.
	score1, err := materializePingScoreFromHistoryEntry(entry1)
	if err != nil {
		t.Fatal(err)
	}
	score2, err := materializePingScoreFromHistoryEntry(entry2)
	if err != nil {
		t.Fatal(err)
	}
	if score1.KmPerSecondAirtime == nil || score2.KmPerSecondAirtime == nil || *score1.KmPerSecondAirtime == *score2.KmPerSecondAirtime {
		t.Errorf("KmPerSecondAirtime should differ (derived fresh from a changed FarthestKm): before=%v after=%v", score1.KmPerSecondAirtime, score2.KmPerSecondAirtime)
	}
}

// --- invalidation / deletion / scale -----------------------------------------

func TestCycle_ChangedHashTimestampInvalidation(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: 10 * time.Minute, DeepSweepBatchSize: 100})
	txID := seedPingTrigger(t, fx.srv, "invalidate0001", "#i", "S", "2026-01-15T10:00:00Z")
	seedPingObservation(t, fx.srv, txID, "pingobsa", 9.0, `[]`, `[]`, 1736935200)
	seedPingObservation(t, fx.srv, txID, "pingobsb", 6.0, `["aa"]`, `["pkrelay1"]`, 1736935210)
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	before, _ := fx.engine.index.Get(txID)
	if before.StationCount != 2 {
		t.Fatalf("sanity check failed: StationCount = %d, want 2", before.StationCount)
	}

	// Simulate the same tx_id being reused for a completely different
	// packet: new hash, new (unrelated) transmission+observation, and
	// ping_triggers updated to match.
	newHash := "invalidate0002"
	if _, err := fx.srv.db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		VALUES ('BB', ?, '2026-01-16T10:00:00Z', 1, 5, '{}', '#i')`, newHash); err != nil {
		t.Fatal(err)
	}
	var newTxRowID int64
	if err := fx.srv.db.conn.QueryRow(`SELECT id FROM transmissions WHERE hash = ?`, newHash).Scan(&newTxRowID); err != nil {
		t.Fatal(err)
	}
	seedPingObservation(t, fx.srv, newTxRowID, "pingobsc", 4.0, `[]`, `[]`, 1737025200)
	if _, err := fx.srv.db.conn.Exec(`UPDATE ping_triggers SET hash = ?, first_seen = ? WHERE tx_id = ?`, newHash, "2026-01-16T10:00:00Z", txID); err != nil {
		t.Fatal(err)
	}

	fx.clock.Advance(time.Minute)
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	after, _ := fx.engine.index.Get(txID)
	if after.Hash != newHash || after.Timestamp != "2026-01-16T10:00:00Z" {
		t.Errorf("entry not updated to the new hash/timestamp: %+v", after)
	}
	if after.StationCount != 1 {
		t.Errorf("StationCount = %d, want 1 (pingobsc only -- old path facts from the previous hash must NOT be reused)", after.StationCount)
	}
	if after.Settled {
		t.Error("Settled = true right after invalidation, want false (treated as freshly discovered)")
	}
}

func TestCycle_TriggerDeletion(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{})
	txID := seedPingTrigger(t, fx.srv, "deletion000001", "#d", "S", "2026-01-15T10:00:00Z")
	seedPingObservation(t, fx.srv, txID, "pingobsa", 9.0, `[]`, `[]`, 1736935200)
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	if _, ok := fx.engine.index.Get(txID); !ok {
		t.Fatal("sanity check failed: entry not present after bootstrap")
	}

	if _, err := fx.srv.db.conn.Exec(`DELETE FROM ping_triggers WHERE tx_id = ?`, txID); err != nil {
		t.Fatal(err)
	}
	fx.clock.Advance(time.Minute)
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	if _, ok := fx.engine.index.Get(txID); ok {
		t.Error("entry still present in index after its trigger was deleted")
	}
	persisted, err := fx.store.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 0 {
		t.Errorf("persisted entries = %+v, want empty (deleted from the store too)", persisted)
	}
}

func TestCycle_MoreThan499Triggers(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{})
	const n = 520
	for i := 0; i < n; i++ {
		h := fmtHash(i)
		txID := seedPingTrigger(t, fx.srv, h, "#m", "S", "2026-01-15T10:00:00Z")
		seedPingObservation(t, fx.srv, txID, "pingobsa", 9.0, `[]`, `[]`, 1736935200+int64(i))
	}
	snap, err := fx.engine.Cycle()
	if err != nil {
		t.Fatal(err)
	}
	if snap.TotalPings != n {
		t.Errorf("TotalPings = %d, want %d", snap.TotalPings, n)
	}
	if fx.engine.index.Len() != n {
		t.Errorf("index.Len() = %d, want %d", fx.engine.index.Len(), n)
	}
	persisted, err := fx.store.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) != n {
		t.Errorf("persisted entries = %d, want %d", len(persisted), n)
	}
}

func fmtHash(i int) string {
	const hexdigits = "0123456789abcdef"
	b := make([]byte, 16)
	for j := 15; j >= 0; j-- {
		b[j] = hexdigits[i%16]
		i /= 16
	}
	return string(b)
}

// --- equivalence + determinism -----------------------------------------------

// TestCycle_SnapshotEquivalenceWithOldRecomputer covers a fully
// reconstructable fixture (nothing pruned, nothing invalidated): the
// engine's Cycle() output must match computeAllPingScores' own output on
// every public field.
func TestCycle_SnapshotEquivalenceWithOldRecomputer(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: 10 * time.Minute, DeepSweepBatchSize: 100})
	tx1 := seedPingTrigger(t, fx.srv, "equiveng000001", "#e", "Alice", "2026-01-15T10:00:00Z")
	seedPingObservation(t, fx.srv, tx1, "pingobsa", 9.0, `[]`, `[]`, 1736935200)
	seedPingObservation(t, fx.srv, tx1, "pingobsb", 6.0, `["aa"]`, `["pkrelay1"]`, 1736935210)
	tx2 := seedPingTrigger(t, fx.srv, "equiveng000002", "#e", "Bob", "2026-01-16T10:00:00Z")
	seedPingObservation(t, fx.srv, tx2, "pingobsc", 4.0, `["aa","bb"]`, `["pkrelay1","pkrelay2"]`, 1737025200)

	snap, err := fx.engine.Cycle()
	if err != nil {
		t.Fatal(err)
	}
	live := fx.srv.computeAllPingScores()
	assertSnapshotsEqualIgnoringGeneratedAt(t, live, snap)
}

func TestCycle_DeterministicRepeatedCalls(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: 10 * time.Minute, DeepSweepBatchSize: 100})
	tx1 := seedPingTrigger(t, fx.srv, "determeng000001", "#d", "Alice", "2026-01-15T10:00:00Z")
	seedPingObservation(t, fx.srv, tx1, "pingobsa", 9.0, `[]`, `[]`, 1736935200)
	seedPingObservation(t, fx.srv, tx1, "pingobsb", 6.0, `["aa"]`, `["pkrelay1"]`, 1736935210)

	snap1, err := fx.engine.Cycle()
	if err != nil {
		t.Fatal(err)
	}
	// Second cycle: no new data, clock unchanged -- must be a no-op
	// producing byte-identical output.
	snap2, err := fx.engine.Cycle()
	if err != nil {
		t.Fatal(err)
	}
	j1 := mustJSON(t, snap1)
	j2 := mustJSON(t, snap2)
	if j1 != j2 {
		t.Errorf("repeated cycles with identical inputs/now produced different snapshots:\n1: %s\n2: %s", j1, j2)
	}
}

// ============================================================================
// Error injection / all-or-nothing guarantees (section 9)
//
// Cycle() only ever reassigns e.index as its LAST statement, strictly
// after UpsertDeleteAndIntegrity has committed -- every error path returns
// before that line by construction, so "index unchanged on any error" is
// structural, not incidental. These tests prove it holds for a
// representative failure at each LAYER Cycle touches: the live query path
// (fetchPingTriggers, the fingerprint bulk query, the path bulk query --
// reusing Phase 4B's countingConn/fail-after-N driver), locally-injected
// bad data (materialization, trigger/entry mismatch), and the persistence
// layer itself (a genuine read-only reconnect, not a simulated error).
// ============================================================================

// setupEngineFaultFixture builds an engine whose *Server.db is wired
// through Phase 4B's fault-injecting counting driver (setupPacketPathCountingDB),
// extended with a ping_triggers table -- so setBulkTestFailAfterQueries can
// deterministically fail the Nth query Cycle() issues against the main DB,
// and bulkTestQueryLog() can be inspected to prove which queries a cycle
// did or didn't issue. config is passed through exactly as given -- no
// hidden defaults (same rule as setupEngineFixture).
func setupEngineFaultFixture(t *testing.T, config pingScoreHistoryEngineConfig) *engineFixture {
	t.Helper()
	db := setupPacketPathCountingDB(t)
	if _, err := db.conn.Exec(`CREATE TABLE ping_triggers (
		tx_id INTEGER PRIMARY KEY, hash TEXT NOT NULL, channel_hash TEXT, sender TEXT, first_seen TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(db, &Config{Port: 3000}, NewHub())

	dir := t.TempDir()
	store, err := OpenPingScoreHistoryStore(filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	clock := &testClock{t: time.Now()}
	engine, err := newPingScoreHistoryEngine(srv, store, clock.Now, config)
	if err != nil {
		t.Fatal(err)
	}
	return &engineFixture{srv: srv, store: store, clock: clock, config: config, engine: engine}
}

// seedFaultTrigger inserts a trigger (+ matching node/observer/transmission/
// observation) directly against the fault fixture's minimal schema.
func seedFaultTrigger(t *testing.T, fx *engineFixture, txID int64, hash string) {
	t.Helper()
	db := fx.srv.db
	if _, err := db.conn.Exec(`INSERT INTO ping_triggers (tx_id, hash, channel_hash, sender, first_seen) VALUES (?,?,?,?,?)`,
		txID, hash, "#f", "S", "2026-01-15T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.Exec(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash) VALUES (?, 'AA', ?, '2026-01-15T10:00:00Z', 1, 5, '{}', '#f')`, txID, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.Exec(`INSERT INTO observers (id, name, iata) VALUES (?, ?, NULL)`, "obs"+hash, "Obs"+hash); err != nil {
		t.Fatal(err)
	}
	var obsRowID int64
	if err := db.conn.QueryRow(`SELECT rowid FROM observers WHERE id = ?`, "obs"+hash).Scan(&obsRowID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, resolved_path, timestamp) VALUES (?, ?, 9.0, -88, '[]', '[]', 100)`, txID, obsRowID); err != nil {
		t.Fatal(err)
	}
}

// engineStateSnapshot captures everything an all-or-nothing failure must
// leave unchanged, for a before/after comparison.
type engineStateSnapshot struct {
	indexEntries     []PingScoreHistoryEntry
	persistedEntries []PingScoreHistoryEntry
	integrity        *PingScoreHistoryIntegrity
}

func captureEngineState(t *testing.T, fx *engineFixture) engineStateSnapshot {
	t.Helper()
	persisted, err := fx.store.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	integrity, err := fx.store.LoadIntegrity()
	if err != nil {
		t.Fatal(err)
	}
	return engineStateSnapshot{
		indexEntries:     fx.engine.index.Entries(),
		persistedEntries: persisted,
		integrity:        integrity,
	}
}

func assertEngineStateUnchanged(t *testing.T, fx *engineFixture, before engineStateSnapshot) {
	t.Helper()
	after := captureEngineState(t, fx)
	if !reflect.DeepEqual(before.indexEntries, after.indexEntries) {
		t.Errorf("in-memory index changed after a failed cycle:\n before: %+v\n after:  %+v", before.indexEntries, after.indexEntries)
	}
	if !reflect.DeepEqual(before.persistedEntries, after.persistedEntries) {
		t.Errorf("persisted entries changed after a failed cycle:\n before: %+v\n after:  %+v", before.persistedEntries, after.persistedEntries)
	}
	if !reflect.DeepEqual(before.integrity, after.integrity) {
		t.Errorf("integrity metadata changed after a failed cycle:\n before: %+v\n after:  %+v", before.integrity, after.integrity)
	}
}

func TestCycle_FetchTriggersFailure_LeavesEverythingUnchanged(t *testing.T) {
	fx := setupEngineFaultFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 10, MaxEdgeKm: EstimateMaxEdgeKm})
	seedFaultTrigger(t, fx, 1, "faultfetch0001")
	if _, err := fx.engine.Cycle(); err != nil { // seed one real entry first
		t.Fatal(err)
	}
	before := captureEngineState(t, fx)

	seedFaultTrigger(t, fx, 2, "faultfetch0002")
	resetBulkTestQueryLog()
	setBulkTestFailAfterQueries(0) // fail the very first query (fetchPingTriggers)
	defer clearBulkTestFailAfterQueries()

	snap, err := fx.engine.Cycle()
	if err == nil {
		t.Fatal("want an error from the injected fetchPingTriggers failure")
	}
	if snap != nil {
		t.Errorf("snap = %+v, want nil alongside the error", snap)
	}
	if !strings.Contains(err.Error(), "fetch triggers") {
		t.Errorf("err = %v, want it to identify the fetch-triggers step", err)
	}
	assertEngineStateUnchanged(t, fx, before)
}

func TestCycle_FingerprintQueryFailure_LeavesEverythingUnchanged(t *testing.T) {
	fx := setupEngineFaultFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 10, MaxEdgeKm: EstimateMaxEdgeKm})
	seedFaultTrigger(t, fx, 1, "faultfp000001")
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	fx.clock.Advance(30 * time.Second) // still < 1-minute debounce -- entry 1 stays a fingerprint candidate
	before := captureEngineState(t, fx)

	resetBulkTestQueryLog()
	// Query #1 = fetchPingTriggers (succeeds). Query #2 = the fingerprint
	// bulk query for entry 1 -- fail there.
	setBulkTestFailAfterQueries(1)
	defer clearBulkTestFailAfterQueries()

	snap, err := fx.engine.Cycle()
	if err == nil {
		t.Fatal("want an error from the injected fingerprint query failure")
	}
	if snap != nil {
		t.Errorf("snap = %+v, want nil alongside the error", snap)
	}
	assertEngineStateUnchanged(t, fx, before)
}

func TestCycle_BulkPathQueryFailure_LeavesEverythingUnchanged(t *testing.T) {
	fx := setupEngineFaultFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 10, MaxEdgeKm: EstimateMaxEdgeKm})
	// A brand-new trigger goes straight to the bulk-path query without any
	// fingerprint check (ToCompute path) -- query #1 = fetchPingTriggers,
	// query #2 = observationFingerprintsBulk for this new tx_id's
	// fingerprint (needed so the fresh entry's persisted fingerprint is
	// accurate -- see Cycle's doc comment), query #3 = GetPacketPathsBulk.
	seedFaultTrigger(t, fx, 1, "faultbulk00001")
	before := captureEngineState(t, fx)

	resetBulkTestQueryLog()
	setBulkTestFailAfterQueries(2)
	defer clearBulkTestFailAfterQueries()

	snap, err := fx.engine.Cycle()
	if err == nil {
		t.Fatal("want an error from the injected bulk-path query failure")
	}
	if snap != nil {
		t.Errorf("snap = %+v, want nil alongside the error", snap)
	}
	assertEngineStateUnchanged(t, fx, before)
}

// TestCycle_MaterializationFailureDuringSnapshot_LeavesEverythingUnchanged
// covers the SAME failure mode as above but arranges for it to surface
// specifically from step 8 (buildPingScoresSnapshotFromHistory), not
// engine construction: the corrupted entry is injected into the engine's
// in-memory index directly (simulating an entry that became invalid
// between a clean load and this cycle, e.g. via disk-level corruption of
// only the store file, not the process's own memory), while the SEPARATE
// persisted row on disk stays valid so the store-level assertions below
// have a meaningful, unrelated baseline to check.
func TestCycle_MaterializationFailureDuringSnapshot_LeavesEverythingUnchanged(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{})
	txID := seedPingTrigger(t, fx.srv, "faultmat200001", "#f", "S", "2026-01-15T10:00:00Z")
	seedPingObservation(t, fx.srv, txID, "pingobsa", 9.0, `[]`, `[]`, 1736935200)
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	before := captureEngineState(t, fx)

	corrupted := before.indexEntries[0]
	corrupted.RelayPubkeysJSON = "not valid json"
	fx.engine.index.Upsert(corrupted) // in-memory only -- the persisted row is untouched

	snap, err := fx.engine.Cycle()
	if err == nil {
		t.Fatal("want an error from the corrupted in-memory entry surfacing during snapshot materialization")
	}
	if snap != nil {
		t.Errorf("snap = %+v, want nil alongside the error", snap)
	}
	// The persisted store (never touched by the in-memory corruption
	// above) and integrity metadata must still be exactly as they were.
	after := captureEngineState(t, fx)
	if !reflect.DeepEqual(before.persistedEntries, after.persistedEntries) {
		t.Errorf("persisted entries changed after a failed cycle:\n before: %+v\n after:  %+v", before.persistedEntries, after.persistedEntries)
	}
	if !reflect.DeepEqual(before.integrity, after.integrity) {
		t.Errorf("integrity metadata changed after a failed cycle:\n before: %+v\n after:  %+v", before.integrity, after.integrity)
	}
}

func TestCycle_PersistFailure_LeavesEverythingUnchanged(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{})
	txID := seedPingTrigger(t, fx.srv, "faultpersist01", "#f", "S", "2026-01-15T10:00:00Z")
	seedPingObservation(t, fx.srv, txID, "pingobsa", 9.0, `[]`, `[]`, 1736935200)
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	before := captureEngineState(t, fx)

	// Force the NEXT persist attempt to fail genuinely: swap the store's
	// connection for a physically read-only one against the same file.
	roConn, err := openPingScoreHistoryConn(fx.store.path, false)
	if err != nil {
		t.Fatal(err)
	}
	originalConn := fx.store.conn
	fx.store.conn = roConn
	fx.store.readOnly = true
	t.Cleanup(func() {
		roConn.Close()
		fx.store.conn = originalConn
		fx.store.readOnly = false
	})

	tx2 := seedPingTrigger(t, fx.srv, "faultpersist02", "#f", "S", "2026-01-16T10:00:00Z")
	seedPingObservation(t, fx.srv, tx2, "pingobsb", 9.0, `[]`, `[]`, 1737025200)
	fx.clock.Advance(time.Minute)

	snap, err := fx.engine.Cycle()
	if err == nil {
		t.Fatal("want an error from the read-only history store connection")
	}
	if snap != nil {
		t.Errorf("snap = %+v, want nil alongside the error", snap)
	}

	// Restore a writable connection to inspect persisted state.
	fx.store.conn = originalConn
	fx.store.readOnly = false
	assertEngineStateUnchanged(t, fx, before)
}

// ============================================================================
// Fix round 2 (review of commit 1dbcfb33): permanent-unreconstructable
// exclusion from deep-sweep, genuinely-initial bootstrap-integrity,
// hash-normalization at bulk-result lookup, and constructor validation.
// ============================================================================

// --- Fix 1: isPermanentlyUnreconstructable ----------------------------------

func TestIsPermanentlyUnreconstructable(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	const retention = 30 * 24 * time.Hour
	oldFirstSeen := now.Add(-40 * 24 * time.Hour).Format(time.RFC3339)
	youngFirstSeen := now.Add(-1 * time.Hour).Format(time.RFC3339)

	cases := []struct {
		name      string
		entry     PingScoreHistoryEntry
		trigger   pingTriggerRow
		retention time.Duration
		want      bool
	}{
		{"unscorable + old + retention set -> true", PingScoreHistoryEntry{Unscorable: true}, pingTriggerRow{firstSeen: oldFirstSeen}, retention, true},
		{"not unscorable -> false (has a prior valid score, DataPruned's territory)", PingScoreHistoryEntry{Unscorable: false}, pingTriggerRow{firstSeen: oldFirstSeen}, retention, false},
		{"retention=0 -> false (disabled)", PingScoreHistoryEntry{Unscorable: true}, pingTriggerRow{firstSeen: oldFirstSeen}, 0, false},
		{"retention negative -> false (disabled)", PingScoreHistoryEntry{Unscorable: true}, pingTriggerRow{firstSeen: oldFirstSeen}, -time.Hour, false},
		{"invalid timestamp -> false (can't judge age)", PingScoreHistoryEntry{Unscorable: true}, pingTriggerRow{firstSeen: "not-a-timestamp"}, retention, false},
		{"missing timestamp -> false (can't judge age)", PingScoreHistoryEntry{Unscorable: true}, pingTriggerRow{firstSeen: ""}, retention, false},
		{"young unscorable -> false (still eligible for future deep-sweep)", PingScoreHistoryEntry{Unscorable: true}, pingTriggerRow{firstSeen: youngFirstSeen}, retention, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isPermanentlyUnreconstructable(c.entry, c.trigger, now, c.retention)
			if got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestCycle_OldNeverScorableEntry_SettlesButNeverDeepSwept(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 100, RetentionDuration: 30 * 24 * time.Hour})
	oldFirstSeen := fx.clock.Now().Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)
	txID := seedPingTrigger(t, fx.srv, "zombietest0001", "#z", "S", oldFirstSeen)
	// Deliberately no observations -- permanently unscorable.

	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	entry1, _ := fx.engine.index.Get(txID)
	if !entry1.Unscorable {
		t.Fatal("sanity check failed: entry should be Unscorable")
	}
	if entry1.Settled {
		t.Fatal("sanity check failed: entry should start unsettled")
	}

	fx.clock.Advance(2 * time.Minute) // > 1-minute debounce, fingerprint stays 0/0 (no observations ever)
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	entry2, _ := fx.engine.index.Get(txID)
	if !entry2.Settled {
		t.Error("Settled = false after the debounce elapsed, want true -- settling must still happen for a never-scorable entry")
	}

	// Now Settled=true, Unscorable=true, past retention -- must NEVER be
	// deep-swept, across several further cycles.
	for i := 0; i < 3; i++ {
		fx.clock.Advance(time.Minute)
		if _, err := fx.engine.Cycle(); err != nil {
			t.Fatal(err)
		}
		entry, _ := fx.engine.index.Get(txID)
		if entry.LastDeepSweptAt != "" {
			t.Fatalf("cycle %d: LastDeepSweptAt = %q, want empty forever -- a permanently unreconstructable entry must never be deep-swept", i, entry.LastDeepSweptAt)
		}
		if entry.DataPruned {
			t.Fatalf("cycle %d: DataPruned = true, want false -- never had a valid score to preserve", i)
		}
	}

	// Still counted, still visible, still explained by bootstrap-integrity.
	entry, _ := fx.engine.index.Get(txID)
	if !entry.Unscorable {
		t.Error("Unscorable = false, want true")
	}
	integrity, err := fx.store.LoadIntegrity()
	if err != nil {
		t.Fatal(err)
	}
	if integrity == nil || integrity.Status != "initial-backfill-incomplete" {
		t.Errorf("LoadIntegrity() = %+v, want an initial-backfill-incomplete record explaining this entry", integrity)
	}
}

// TestCycle_PermanentlyUnreconstructableEntry_NoBulkPathQueryIssued proves
// the exclusion at the query level: once an entry is settled and
// permanently unreconstructable, a later cycle issues NO GetPacketPathsBulk
// call carrying its hash, even though it's the ONLY entry that would
// otherwise be deep-sweep eligible.
func TestCycle_PermanentlyUnreconstructableEntry_NoBulkPathQueryIssued(t *testing.T) {
	fx := setupEngineFaultFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 10, MaxEdgeKm: EstimateMaxEdgeKm, RetentionDuration: 30 * 24 * time.Hour})
	oldFirstSeen := fx.clock.Now().Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)
	hash := "zombiequery0001"
	if _, err := fx.srv.db.conn.Exec(`INSERT INTO ping_triggers (tx_id, hash, channel_hash, sender, first_seen) VALUES (1, ?, '#z', 'S', ?)`, hash, oldFirstSeen); err != nil {
		t.Fatal(err)
	}
	// Deliberately no transmission/observation rows at all -- unscorable by construction.

	if _, err := fx.engine.Cycle(); err != nil { // bootstrap: Unscorable, unsettled
		t.Fatal(err)
	}
	fx.clock.Advance(2 * time.Minute)
	if _, err := fx.engine.Cycle(); err != nil { // settles
		t.Fatal(err)
	}
	entry, _ := fx.engine.index.Get(1)
	if !entry.Settled || !entry.Unscorable {
		t.Fatalf("sanity check failed: entry = %+v, want Settled=true Unscorable=true", entry)
	}

	resetBulkTestQueryLog()
	fx.clock.Advance(time.Minute)
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	for _, q := range bulkTestQueryLog() {
		if strings.Contains(q.sql, "GetPacketPathsBulk") || (strings.Contains(strings.ToLower(q.sql), "packet path bulk")) {
			t.Errorf("unexpected packet-path bulk query issued for a permanently unreconstructable entry: %s", q.sql)
		}
	}
	// More directly: GetPacketPathsBulk's own query text always contains
	// "FROM observations o" / "JOIN transmissions t" for the v3 path-fetch
	// shape (see ping_score_bulk.go) -- confirm none of the logged queries
	// reference observations/transmissions at all this cycle, since the
	// only entry in the index is excluded from every path-fetch category.
	for _, q := range bulkTestQueryLog() {
		if strings.Contains(q.sql, "FROM observations o") {
			t.Errorf("a packet-path query was issued this cycle even though the only entry is permanently unreconstructable: %s", q.sql)
		}
	}
}

func TestCycle_YoungUnscorableEntry_StillDeepSweptAfterSettle(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 100, RetentionDuration: 30 * 24 * time.Hour})
	recentFirstSeen := fx.clock.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339) // well within the 30-day retention
	txID := seedPingTrigger(t, fx.srv, "youngzombie0001", "#y", "S", recentFirstSeen)
	// No observations -- unscorable, but too young to be permanent.

	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	fx.clock.Advance(2 * time.Minute)
	if _, err := fx.engine.Cycle(); err != nil { // settles
		t.Fatal(err)
	}
	entry, _ := fx.engine.index.Get(txID)
	if !entry.Settled {
		t.Fatal("sanity check failed: entry did not settle")
	}

	fx.clock.Advance(time.Minute)
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	after, _ := fx.engine.index.Get(txID)
	if after.LastDeepSweptAt == "" {
		t.Error("LastDeepSweptAt is empty, want a real timestamp -- a YOUNG unscorable entry must still be deep-sweep eligible in case data eventually arrives")
	}
}

// TestCycle_DataPrunedEntry_UnaffectedByPermanentUnreconstructableCheck
// proves the two exclusions are independent: a DataPruned entry is
// excluded from deep-sweep by its OWN existing rule (DataPruned=true),
// never by isPermanentlyUnreconstructable (which requires Unscorable==true
// -- mutually exclusive with DataPruned by construction).
func TestCycle_DataPrunedEntry_UnaffectedByPermanentUnreconstructableCheck(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 100, RetentionDuration: 30 * 24 * time.Hour})
	firstSeen := fx.clock.Now().Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)
	txID := setupSettledPingWithGoneData(t, fx, "datapruned0chk1", firstSeen)

	fx.clock.Advance(2 * time.Minute)
	if _, err := fx.engine.Cycle(); err != nil { // triggers DataPruned
		t.Fatal(err)
	}
	entry, _ := fx.engine.index.Get(txID)
	if !entry.DataPruned {
		t.Fatal("sanity check failed: entry did not become DataPruned")
	}
	if entry.Unscorable {
		t.Fatal("sanity check failed: a DataPruned entry must not be Unscorable (it has preserved valid facts)")
	}
	// isPermanentlyUnreconstructable requires Unscorable==true, so it can
	// never fire for this entry regardless of age -- confirm directly.
	trigger := pingTriggerRow{firstSeen: firstSeen}
	if isPermanentlyUnreconstructable(entry, trigger, fx.clock.Now(), fx.config.RetentionDuration) {
		t.Error("isPermanentlyUnreconstructable = true for a DataPruned (not Unscorable) entry, want false")
	}
}

// --- Fix 2: genuinely-initial bootstrap-integrity ---------------------------

func TestBuildInitialBootstrapIntegrity_MissingEntryNotCountedAsScored(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{RetentionDuration: 30 * 24 * time.Hour})
	now := fx.clock.Now()
	oldFirstSeen := now.Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)
	triggers := []pingTriggerRow{
		{txID: 1, hash: "h1", firstSeen: oldFirstSeen}, // no matching entry at all
		{txID: 2, hash: "h2", firstSeen: oldFirstSeen}, // Unscorable, past retention
	}
	idx := newPingScoreHistoryIndex([]PingScoreHistoryEntry{
		{TxID: 2, Hash: "h2", Timestamp: oldFirstSeen, Unscorable: true},
	})
	integrity := fx.engine.buildInitialBootstrapIntegrity(triggers, idx, now)
	if integrity == nil {
		t.Fatal("want a non-nil integrity record")
	}
	if integrity.ScoredCount != 0 {
		t.Errorf("ScoredCount = %d, want 0 -- a missing candidate entry must never be counted as scored", integrity.ScoredCount)
	}
	if integrity.UnreconstructableCount != 1 {
		t.Errorf("UnreconstructableCount = %d, want 1", integrity.UnreconstructableCount)
	}
	if integrity.TotalTriggers != 2 {
		t.Errorf("TotalTriggers = %d, want 2", integrity.TotalTriggers)
	}
}

func TestCycle_GenuineBootstrap_MixedScoredAndUnreconstructableGivesCorrectCounts(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 10, RetentionDuration: 30 * 24 * time.Hour})
	now := fx.clock.Now()
	oldFirstSeen := now.Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)
	recentFirstSeen := now.Add(-1 * time.Hour).UTC().Format(time.RFC3339)

	// 2 scorable triggers (real data).
	tx1 := seedPingTrigger(t, fx.srv, "bootmix000001", "#b", "S", recentFirstSeen)
	seedPingObservation(t, fx.srv, tx1, "pingobsa", 9.0, `[]`, `[]`, now.Add(-1*time.Hour).Unix())
	tx2 := seedPingTrigger(t, fx.srv, "bootmix000002", "#b", "S", recentFirstSeen)
	seedPingObservation(t, fx.srv, tx2, "pingobsb", 9.0, `[]`, `[]`, now.Add(-1*time.Hour).Unix())

	// 1 permanently unreconstructable trigger (old, no data).
	seedPingTrigger(t, fx.srv, "bootmix000003", "#b", "S", oldFirstSeen)

	// 1 young unscorable trigger (no data, but too young to judge -- must
	// NOT be counted as unreconstructable).
	seedPingTrigger(t, fx.srv, "bootmix000004", "#b", "S", recentFirstSeen)

	snap, err := fx.engine.Cycle()
	if err != nil {
		t.Fatal(err)
	}
	if snap.TotalPings != 4 {
		t.Errorf("TotalPings = %d, want 4", snap.TotalPings)
	}

	integrity, err := fx.store.LoadIntegrity()
	if err != nil {
		t.Fatal(err)
	}
	if integrity == nil {
		t.Fatal("LoadIntegrity() = nil, want a record (1 unreconstructable trigger)")
	}
	if integrity.Status != "initial-backfill-incomplete" {
		t.Errorf("Status = %q, want initial-backfill-incomplete", integrity.Status)
	}
	if integrity.TotalTriggers != 4 {
		t.Errorf("TotalTriggers = %d, want 4", integrity.TotalTriggers)
	}
	if integrity.ScoredCount != 2 {
		t.Errorf("ScoredCount = %d, want 2", integrity.ScoredCount)
	}
	if integrity.UnreconstructableCount != 1 {
		t.Errorf("UnreconstructableCount = %d, want 1 (only the OLD unscorable trigger, not the young one)", integrity.UnreconstructableCount)
	}
}

func TestCycle_SecondCycleDoesNotChangeBootstrapRecord(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 10, RetentionDuration: 30 * 24 * time.Hour})
	oldFirstSeen := fx.clock.Now().Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)
	seedPingTrigger(t, fx.srv, "secondcycle001", "#s", "S", oldFirstSeen)

	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	first, err := fx.store.LoadIntegrity()
	if err != nil {
		t.Fatal(err)
	}
	if first == nil {
		t.Fatal("want a record after bootstrap")
	}

	fx.clock.Advance(time.Minute)
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	second, err := fx.store.LoadIntegrity()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("integrity changed on the second cycle:\n first:  %+v\n second: %+v", first, second)
	}
}

// TestCycle_LaterOldUnscorableTrigger_DoesNotOverwriteBootstrapRecord is
// the key regression test for the review's finding: a SECOND, later,
// old-and-never-scorable trigger must NOT be mistaken for a new bootstrap
// just because it's ALSO in that cycle's ToCompute and ALSO ends up
// Unscorable+past-retention.
func TestCycle_LaterOldUnscorableTrigger_DoesNotOverwriteBootstrapRecord(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 10, RetentionDuration: 30 * 24 * time.Hour})
	oldFirstSeen := fx.clock.Now().Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)
	seedPingTrigger(t, fx.srv, "regressiontest1", "#r", "S", oldFirstSeen)

	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	first, err := fx.store.LoadIntegrity()
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || first.TotalTriggers != 1 || first.UnreconstructableCount != 1 {
		t.Fatalf("sanity check failed: first = %+v", first)
	}

	fx.clock.Advance(time.Hour)
	anotherOldFirstSeen := fx.clock.Now().Add(-50 * 24 * time.Hour).UTC().Format(time.RFC3339)
	seedPingTrigger(t, fx.srv, "regressiontest2", "#r", "S", anotherOldFirstSeen)
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}

	second, err := fx.store.LoadIntegrity()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("bootstrap-integrity record was overwritten by a later ordinary trigger:\n first:  %+v\n second: %+v", first, second)
	}
}

func TestCycle_IntegrityLoadFailure_LeavesEverythingUnchanged(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 10})
	txID := seedPingTrigger(t, fx.srv, "intfail0000001", "#i", "S", "2026-01-15T10:00:00Z")
	seedPingObservation(t, fx.srv, txID, "pingobsa", 9.0, `[]`, `[]`, 1736935200)
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	before := captureEngineState(t, fx)

	// Corrupt the integrity table so a later LoadIntegrity call fails at
	// the query level (a genuine failure, not simulated).
	if _, err := fx.store.conn.Exec(`DROP TABLE ping_score_history_integrity`); err != nil {
		t.Fatal(err)
	}

	tx2 := seedPingTrigger(t, fx.srv, "intfail0000002", "#i", "S", "2026-01-16T10:00:00Z")
	seedPingObservation(t, fx.srv, tx2, "pingobsb", 9.0, `[]`, `[]`, 1737025200)
	fx.clock.Advance(time.Minute)

	snap, err := fx.engine.Cycle()
	if err == nil {
		t.Fatal("want an error from the corrupted integrity table")
	}
	if snap != nil {
		t.Errorf("snap = %+v, want nil alongside the error", snap)
	}
	if !reflect.DeepEqual(before.indexEntries, fx.engine.index.Entries()) {
		t.Error("in-memory index changed after a failed cycle")
	}
	persistedAfter, err := fx.store.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.persistedEntries, persistedAfter) {
		t.Error("persisted entries changed after a failed cycle")
	}
}

// TestCycle_BootstrapPersistFailure_NoPartialEntriesOrIntegrity proves the
// bootstrap's entries AND its integrity record commit as one transaction:
// forcing the persist step to fail leaves NEITHER behind.
func TestCycle_BootstrapPersistFailure_NoPartialEntriesOrIntegrity(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 10, RetentionDuration: 30 * 24 * time.Hour})
	oldFirstSeen := fx.clock.Now().Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)
	seedPingTrigger(t, fx.srv, "bootfail0000001", "#b", "S", oldFirstSeen)

	roConn, err := openPingScoreHistoryConn(fx.store.path, false)
	if err != nil {
		t.Fatal(err)
	}
	fx.store.conn = roConn
	fx.store.readOnly = true

	snap, err := fx.engine.Cycle()
	if err == nil {
		t.Fatal("want an error from the read-only history store connection during bootstrap")
	}
	if snap != nil {
		t.Errorf("snap = %+v, want nil", snap)
	}
	if fx.engine.index.Len() != 0 {
		t.Errorf("index.Len() = %d, want 0 -- a failed bootstrap must leave the index empty", fx.engine.index.Len())
	}
	roConn.Close()

	rwConn, err := openPingScoreHistoryConn(fx.store.path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer rwConn.Close()
	fx.store.conn = rwConn
	fx.store.readOnly = false

	entries, err := fx.store.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("persisted entries = %+v, want empty -- failed bootstrap must not partially persist", entries)
	}
	integrity, err := fx.store.LoadIntegrity()
	if err != nil {
		t.Fatal(err)
	}
	if integrity != nil {
		t.Errorf("integrity = %+v, want nil -- failed bootstrap must not partially persist integrity either", integrity)
	}
}

// --- Fix 3: hash normalization at bulk-result lookup ------------------------

// seedMixedCaseTrigger inserts a transmission with hash stored LOWERCASE
// (the only form GetPacketPath/GetPacketPathsBulk's `t.hash = LOWER(?)`
// queries can ever match -- both callsites lowercase their query input,
// so a mixed-case-STORED transmissions.hash could never be found at all,
// mixed case or not) paired with a ping_triggers row whose hash is a
// DIFFERENT casing of the exact same value. This models a real, possible
// divergence between how the transmission decoder and the ping-trigger
// detector each stored the identical packet hash -- exactly the case
// pathResultFor's normalization exists to bridge, without ever touching
// the persisted entry.Hash identity itself.
func seedMixedCaseTrigger(t *testing.T, srv *Server, lowerHash, mixedHash, channelHash, sender, firstSeen string) int64 {
	t.Helper()
	res, err := srv.db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		VALUES ('AABBCCDDEE', ?, ?, 1, 5, '{"type":"CHAN","channel":"'||?||'","text":"'||?||': ping","sender":"'||?||'"}', ?)`,
		lowerHash, firstSeen, channelHash, sender, sender, channelHash)
	if err != nil {
		t.Fatalf("insert tx: %v", err)
	}
	txID, _ := res.LastInsertId()
	if _, err := srv.db.conn.Exec(`INSERT INTO ping_triggers (tx_id, hash, channel_hash, sender, first_seen) VALUES (?, ?, ?, ?, ?)`,
		txID, mixedHash, channelHash, sender, firstSeen); err != nil {
		t.Fatalf("insert ping_trigger: %v", err)
	}
	return txID
}

func TestCycle_MixedCaseTriggerHashScoredCorrectly(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: 10 * time.Minute, DeepSweepBatchSize: 100})
	mixedHash := "MiXedCaseHash01"
	txID := seedMixedCaseTrigger(t, fx.srv, "mixedcasehash01", mixedHash, "#m", "S", "2026-01-15T10:00:00Z")
	seedPingObservation(t, fx.srv, txID, "pingobsa", 9.0, `[]`, `[]`, 1736935200)
	seedPingObservation(t, fx.srv, txID, "pingobsb", 6.0, `["aa"]`, `["pkrelay1"]`, 1736935210)

	snap, err := fx.engine.Cycle()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := fx.engine.index.Get(txID)
	if !ok {
		t.Fatal("index missing entry for mixed-case hash trigger")
	}
	if entry.Unscorable {
		t.Error("Unscorable = true, want false -- GetPacketPathsBulk's result must be found despite hash casing")
	}
	if entry.StationCount != 2 {
		t.Errorf("StationCount = %d, want 2", entry.StationCount)
	}
	if entry.Hash != mixedHash {
		t.Errorf("entry.Hash = %q, want the ORIGINAL mixed-case value %q preserved -- only the map lookup is normalized, never the persisted field", entry.Hash, mixedHash)
	}
	if snap.TotalPings != 1 {
		t.Errorf("TotalPings = %d, want 1", snap.TotalPings)
	}
}

func TestCycle_MixedCaseHash_SingleBulkScoreMatches(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: 10 * time.Minute, DeepSweepBatchSize: 100})
	mixedHash := "MiXedBulkSingle1"
	txID := seedMixedCaseTrigger(t, fx.srv, "mixedbulksingle1", mixedHash, "#m", "S", "2026-01-15T10:00:00Z")
	seedPingObservation(t, fx.srv, txID, "pingobsa", 9.0, `[]`, `[]`, 1736935200)
	seedPingObservation(t, fx.srv, txID, "pingobsb", 6.0, `["aa"]`, `["pkrelay1"]`, 1736935210)

	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	entry, _ := fx.engine.index.Get(txID)
	bulkScore, err := materializePingScoreFromHistoryEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	if bulkScore == nil {
		t.Fatal("bulkScore = nil, want a real score")
	}

	trigger := pingTriggerRow{txID: txID, hash: mixedHash, channelHash: "#m", sender: "S", firstSeen: "2026-01-15T10:00:00Z"}
	singleScore := fx.srv.computePingScore(trigger)
	if singleScore == nil {
		t.Fatal("computePingScore(mixed-case hash) returned nil")
	}
	if bulkScore.StationCount != singleScore.StationCount || bulkScore.DeepestHops != singleScore.DeepestHops {
		t.Errorf("bulk = %+v, single = %+v, want matching path facts", bulkScore, singleScore)
	}
}

// TestCycle_MixedCaseHash_MismatchDetectionStillWorks confirms the
// trigger/entry mismatch check (buildPingScoresSnapshotFromHistory, exact-
// string comparison) still correctly fires for a GENUINELY different hash
// even when mixed-case hashes are in play throughout -- proving the
// case-insensitive pathResults lookup fix didn't accidentally make the
// persisted-identity comparison case-insensitive too.
func TestCycle_MixedCaseHash_MismatchDetectionStillWorks(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: 10 * time.Minute, DeepSweepBatchSize: 100})
	mixedHash := "MixedMismatch001"
	txID := seedPingTrigger(t, fx.srv, mixedHash, "#m", "S", "2026-01-15T10:00:00Z")
	seedPingObservation(t, fx.srv, txID, "pingobsa", 9.0, `[]`, `[]`, 1736935200)
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	staleEntry, _ := fx.engine.index.Get(txID)

	triggers := []pingTriggerRow{{txID: txID, hash: "DifferentHash001", firstSeen: "2026-01-15T10:00:00Z"}}
	_, err := fx.srv.buildPingScoresSnapshotFromHistory(triggers, []PingScoreHistoryEntry{staleEntry}, fx.clock.Now())
	var mismatchErr *PingScoreHistoryMismatchError
	if !errors.As(err, &mismatchErr) {
		t.Fatalf("err = %v, want a *PingScoreHistoryMismatchError for the genuinely different hash", err)
	}

	// Sanity: the SAME mixed-case hash on both sides is NOT a mismatch.
	sameTriggers := []pingTriggerRow{{txID: txID, hash: mixedHash, firstSeen: "2026-01-15T10:00:00Z"}}
	if _, err := fx.srv.buildPingScoresSnapshotFromHistory(sameTriggers, []PingScoreHistoryEntry{staleEntry}, fx.clock.Now()); err != nil {
		t.Errorf("unexpected error for a matching (identically-cased) hash: %v", err)
	}
}

// --- Fix 4: constructor input/config validation -----------------------------

func TestNewPingScoreHistoryEngine_RejectsNilServer(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenPingScoreHistoryStore(filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = newPingScoreHistoryEngine(nil, store, time.Now, pingScoreHistoryEngineConfig{})
	if err == nil {
		t.Fatal("want an error for nil server")
	}
}

func TestNewPingScoreHistoryEngine_RejectsNilServerDB(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenPingScoreHistoryStore(filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	srv := &Server{} // db is nil
	_, err = newPingScoreHistoryEngine(srv, store, time.Now, pingScoreHistoryEngineConfig{})
	if err == nil {
		t.Fatal("want an error for nil server.db")
	}
}

func TestNewPingScoreHistoryEngine_RejectsNilStore(t *testing.T) {
	srv, _ := setupPingScoresFixture(t)
	_, err := newPingScoreHistoryEngine(srv, nil, time.Now, pingScoreHistoryEngineConfig{})
	if err == nil {
		t.Fatal("want an error for nil store")
	}
}

func TestNewPingScoreHistoryEngine_RejectsNilNow(t *testing.T) {
	srv, _ := setupPingScoresFixture(t)
	dir := t.TempDir()
	store, err := OpenPingScoreHistoryStore(filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = newPingScoreHistoryEngine(srv, store, nil, pingScoreHistoryEngineConfig{})
	if err == nil {
		t.Fatal("want an error for nil now")
	}
}

func TestNewPingScoreHistoryEngine_RejectsNegativeConfig(t *testing.T) {
	srv, _ := setupPingScoresFixture(t)
	cases := []struct {
		name   string
		config pingScoreHistoryEngineConfig
	}{
		{"negative SettleDebounce", pingScoreHistoryEngineConfig{SettleDebounce: -time.Minute}},
		{"negative DeepSweepBatchSize", pingScoreHistoryEngineConfig{DeepSweepBatchSize: -1}},
		{"negative RetentionDuration", pingScoreHistoryEngineConfig{RetentionDuration: -time.Hour}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := OpenPingScoreHistoryStore(filepath.Join(dir, "h.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			_, err = newPingScoreHistoryEngine(srv, store, time.Now, c.config)
			if err == nil {
				t.Fatalf("want an error for %s", c.name)
			}
		})
	}
}

// TestNewPingScoreHistoryEngine_AllowsNegativeMaxEdgeKm confirms MaxEdgeKm
// is NOT subject to the same negative-rejects-as-error rule as the other
// three fields -- it mirrors nearestPositionedNeighbor's own <=0-disables-
// the-filter convention, a meaningful value, not an error.
func TestNewPingScoreHistoryEngine_AllowsNegativeMaxEdgeKm(t *testing.T) {
	srv, _ := setupPingScoresFixture(t)
	dir := t.TempDir()
	store, err := OpenPingScoreHistoryStore(filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := newPingScoreHistoryEngine(srv, store, time.Now, pingScoreHistoryEngineConfig{MaxEdgeKm: -5}); err != nil {
		t.Fatalf("MaxEdgeKm<0 should be accepted (disables the geo-filter): %v", err)
	}
}

func TestNewPingScoreHistoryEngine_ConstructorFailureDoesNotMutateStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "h.db")
	store, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	seed := PingScoreHistoryEntry{TxID: 1, Hash: "h", Timestamp: "t", ComputedAt: "c"}
	if err := store.UpsertAndDelete([]PingScoreHistoryEntry{seed}, nil); err != nil {
		t.Fatal(err)
	}
	store.Close()

	store2, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	srv, _ := setupPingScoresFixture(t)
	_, err = newPingScoreHistoryEngine(srv, store2, time.Now, pingScoreHistoryEngineConfig{SettleDebounce: -1})
	if err == nil {
		t.Fatal("want a constructor error")
	}

	entries, err := store2.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Hash != "h" {
		t.Errorf("store mutated by a failed constructor call: %+v", entries)
	}
}

// --- Fix 4: zero-value semantics, explicitly tested -------------------------

func TestCycle_ZeroSettleDebounce_SettlesAtNextUnchangedCheck(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: 0, DeepSweepBatchSize: 10})
	txID := seedPingTrigger(t, fx.srv, "zerodebounce01", "#z", "S", "2026-01-15T10:00:00Z")
	seedPingObservation(t, fx.srv, txID, "pingobsa", 9.0, `[]`, `[]`, 1736935200)

	if _, err := fx.engine.Cycle(); err != nil { // bootstrap: unsettled
		t.Fatal(err)
	}
	entry1, _ := fx.engine.index.Get(txID)
	if entry1.Settled {
		t.Fatal("Settled = true immediately at bootstrap, want false")
	}

	fx.clock.Advance(time.Second) // any positive advance satisfies debounce=0
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	entry2, _ := fx.engine.index.Get(txID)
	if !entry2.Settled {
		t.Error("Settled = false at the next unchanged check with SettleDebounce=0, want true")
	}
}

func TestCycle_ZeroDeepSweepBatchSize_DisablesDeepSweep(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 0})
	txID := seedPingTrigger(t, fx.srv, "zerobatch000001", "#z", "S", "2026-01-15T10:00:00Z")
	seedPingObservation(t, fx.srv, txID, "pingobsa", 9.0, `[]`, `[]`, 1736935200)
	settleEntry(t, fx)

	before, _ := fx.engine.index.Get(txID)
	if !before.Settled {
		t.Fatal("sanity check failed: entry not settled")
	}

	fx.clock.Advance(time.Minute)
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	after, _ := fx.engine.index.Get(txID)
	if after.LastDeepSweptAt != "" {
		t.Errorf("LastDeepSweptAt = %q, want empty -- DeepSweepBatchSize=0 must disable deep-sweep entirely", after.LastDeepSweptAt)
	}
}

func TestCycle_ZeroRetentionDuration_DisablesDataPrunedAndBootstrapIntegrity(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 10, RetentionDuration: 0})
	oldFirstSeen := fx.clock.Now().Add(-100 * 24 * time.Hour).UTC().Format(time.RFC3339)
	seedPingTrigger(t, fx.srv, "zeroretent0001", "#z", "S", oldFirstSeen) // old, never scorable

	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	integrity, err := fx.store.LoadIntegrity()
	if err != nil {
		t.Fatal(err)
	}
	if integrity != nil {
		t.Errorf("integrity = %+v, want nil -- RetentionDuration=0 must disable bootstrap-integrity detection entirely", integrity)
	}
}
