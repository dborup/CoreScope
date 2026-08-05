package main

import (
	"encoding/json"
	"testing"
	"time"
)

// --- shared equivalence helpers -------------------------------------------

// historyEntriesFromLiveScores computes each trigger's score via the SAME
// live path (computePingScore -> GetPacketPath) computeAllPingScores uses,
// then converts each into a PingScoreHistoryEntry via
// pingScoreHistoryEntryFromScore -- simulating "persist today's live
// computation, unchanged, as history" for the equivalence tests below.
func historyEntriesFromLiveScores(t *testing.T, srv *Server, triggers []pingTriggerRow, computedAt time.Time) []PingScoreHistoryEntry {
	t.Helper()
	entries := make([]PingScoreHistoryEntry, 0, len(triggers))
	for _, trigger := range triggers {
		score := srv.computePingScore(trigger)
		entries = append(entries, pingScoreHistoryEntryFromScore(trigger, score, observationFingerprint{}, PingScoreHistoryEntryState{}, computedAt))
	}
	return entries
}

// assertSnapshotsEqualIgnoringGeneratedAt compares only PUBLIC (JSON-visible)
// fields -- marshaling to JSON naturally excludes PingScore's unexported
// relayPubkeys/firstPubkey/firstName fields, which is exactly the "public
// fields only" comparison item 6 asks for (relayPubkeys in particular is
// built from live map iteration in one path and from sorted JSON in the
// other -- same SET, different internal order, which must not fail an
// otherwise-correct equivalence check).
func assertSnapshotsEqualIgnoringGeneratedAt(t *testing.T, want, got *PingScoresSnapshot) {
	t.Helper()
	if (want == nil) != (got == nil) {
		t.Fatalf("snapshot nilness differs: want=%v got=%v", want, got)
	}
	if want == nil {
		return
	}
	wantCopy, gotCopy := *want, *got
	wantCopy.GeneratedAt, gotCopy.GeneratedAt = "", ""
	wantJSON, err := json.MarshalIndent(wantCopy, "", "  ")
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	gotJSON, err := json.MarshalIndent(gotCopy, "", "  ")
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	if string(wantJSON) != string(gotJSON) {
		t.Errorf("snapshots differ (ignoring GeneratedAt):\n--- live (want) ---\n%s\n--- from-history (got) ---\n%s", wantJSON, gotJSON)
	}
}

// runEquivalenceCheck computes today's live snapshot, converts every
// trigger's live score into a history entry, rebuilds a snapshot purely
// from those entries, and asserts the two match on every public field.
func runEquivalenceCheck(t *testing.T, srv *Server) {
	t.Helper()
	triggers, err := srv.db.fetchPingTriggers()
	if err != nil {
		t.Fatalf("fetchPingTriggers: %v", err)
	}
	live := srv.computeAllPingScores()
	now := time.Now()

	entries := historyEntriesFromLiveScores(t, srv, triggers, now)
	fromHistory, err := srv.buildPingScoresSnapshotFromHistory(triggers, entries, now)
	if err != nil {
		t.Fatalf("buildPingScoresSnapshotFromHistory: %v", err)
	}
	assertSnapshotsEqualIgnoringGeneratedAt(t, live, fromHistory)
}

// --- equivalence: empty / single / multiple -------------------------------

func TestPingScoresHistoryEquivalence_EmptyHistory(t *testing.T) {
	srv, _ := setupPingScoresFixture(t)
	runEquivalenceCheck(t, srv)
}

func TestPingScoresHistoryEquivalence_OnePing(t *testing.T) {
	srv, _ := setupPingScoresFixture(t)
	txID := seedPingTrigger(t, srv, "eq1ping0000001", "#test", "Alice", "2026-01-15T10:00:00Z")
	seedPingObservation(t, srv, txID, "pingobsa", 9.0, `[]`, `[]`, 1736935200)
	seedPingObservation(t, srv, txID, "pingobsc", 4.0, `["aa","bb"]`, `["pkrelay1","pkrelay2"]`, 1736935260)
	runEquivalenceCheck(t, srv)
}

func TestPingScoresHistoryEquivalence_MultiplePings(t *testing.T) {
	srv, _ := setupPingScoresFixture(t)
	tx1 := seedPingTrigger(t, srv, "eqmulti000001", "#a", "Alice", "2026-01-15T10:00:00Z")
	seedPingObservation(t, srv, tx1, "pingobsa", 9.0, `[]`, `[]`, 1736935200)
	seedPingObservation(t, srv, tx1, "pingobsb", 6.0, `["aa"]`, `["pkrelay1"]`, 1736935210)
	seedPingObservation(t, srv, tx1, "pingobsc", 4.0, `["aa","bb"]`, `["pkrelay1","pkrelay2"]`, 1736935260)

	tx2 := seedPingTrigger(t, srv, "eqmulti000002", "#b", "Bob", "2026-01-16T10:00:00Z")
	seedPingObservation(t, srv, tx2, "pingobsb", 9.0, `[]`, `[]`, 1737025200)

	tx3 := seedPingTrigger(t, srv, "eqmulti000003", "#a", "Alice", "2026-01-17T10:00:00Z")
	seedPingObservation(t, srv, tx3, "pingobsc", 5.0, `["aa","bb","cc"]`, `["pkrelay2","pkrelay3","pkrelay1"]`, 1737111600)
	seedPingObservation(t, srv, tx3, "pingobsa", 8.0, `[]`, `[]`, 1737111605)

	runEquivalenceCheck(t, srv)
}

// --- equivalence: all-time records -----------------------------------------

func TestPingScoresHistoryEquivalence_AllTimeRecords(t *testing.T) {
	srv, _ := setupPingScoresFixture(t)
	// Farthest winner: obs B, ~230km.
	txF := seedPingTrigger(t, srv, "eqrec0000001", "#r", "S1", "2026-01-10T10:00:00Z")
	seedPingObservation(t, srv, txF, "pingobsa", 9.0, `[]`, `[]`, 1736503200)
	seedPingObservation(t, srv, txF, "pingobsb", 6.0, `["aa"]`, `["pkr1"]`, 1736503210)

	// MostHops winner: 3-hop branch.
	txH := seedPingTrigger(t, srv, "eqrec0000002", "#r", "S2", "2026-01-11T10:00:00Z")
	seedPingObservation(t, srv, txH, "pingobsa", 9.0, `[]`, `[]`, 1736589600)
	seedPingObservation(t, srv, txH, "pingobsc", 4.0, `["aa","bb","cc"]`, `["pkr1","pkr2","pkr3"]`, 1736589660)

	// WidestSpread winner: all 3 observers.
	txW := seedPingTrigger(t, srv, "eqrec0000003", "#r", "S3", "2026-01-12T10:00:00Z")
	seedPingObservation(t, srv, txW, "pingobsa", 9.0, `[]`, `[]`, 1736676000)
	seedPingObservation(t, srv, txW, "pingobsb", 6.0, `["aa"]`, `["pkr1"]`, 1736676010)
	seedPingObservation(t, srv, txW, "pingobsc", 4.0, `["aa","bb"]`, `["pkr1","pkr2"]`, 1736676070)

	// A lone-station ping (StationCount=1) that must NOT win FastestSpread.
	txLone := seedPingTrigger(t, srv, "eqrec0000004", "#r", "S4", "2026-01-13T10:00:00Z")
	seedPingObservation(t, srv, txLone, "pingobsa", 9.0, `[]`, `[]`, 1736762400)

	runEquivalenceCheck(t, srv)
}

// --- equivalence: ThisWeek ---------------------------------------------------

func TestPingScoresHistoryEquivalence_ThisWeek(t *testing.T) {
	srv, _ := setupPingScoresFixture(t)
	recent := time.Now().Add(-2 * time.Hour)
	old := time.Now().AddDate(0, 0, -20) // well outside the 7-day window

	txRecent := seedPingTrigger(t, srv, "eqweek0000001", "#w", "Recent", recent.Format(time.RFC3339))
	seedPingObservation(t, srv, txRecent, "pingobsa", 9.0, `[]`, `[]`, recent.Unix())
	seedPingObservation(t, srv, txRecent, "pingobsb", 6.0, `["aa"]`, `["pkr1"]`, recent.Unix()+10)

	txOld := seedPingTrigger(t, srv, "eqweek0000002", "#w", "Old", old.Format(time.RFC3339))
	seedPingObservation(t, srv, txOld, "pingobsa", 9.0, `[]`, `[]`, old.Unix())
	seedPingObservation(t, srv, txOld, "pingobsc", 4.0, `["aa","bb","cc"]`, `["pkr1","pkr2","pkr3"]`, old.Unix()+30)

	runEquivalenceCheck(t, srv)
}

func TestPingScoresHistoryEquivalence_ThisWeekNilWhenNoRecentPings(t *testing.T) {
	srv, _ := setupPingScoresFixture(t)
	old := time.Now().AddDate(0, 0, -20)
	tx := seedPingTrigger(t, srv, "eqweeknil00001", "#w", "Old", old.Format(time.RFC3339))
	seedPingObservation(t, srv, tx, "pingobsa", 9.0, `[]`, `[]`, old.Unix())
	runEquivalenceCheck(t, srv)
}

// --- equivalence: sender 30-day cutoff ---------------------------------------

func TestPingScoresHistoryEquivalence_SenderCutoff(t *testing.T) {
	srv, _ := setupPingScoresFixture(t)
	recent := time.Now().Add(-1 * time.Hour)
	old := time.Now().AddDate(0, 0, -40) // outside 30 days, inside... no, also outside all-time relevance windows

	txRecent := seedPingTrigger(t, srv, "eqsender000001", "#s", "RecentSender", recent.Format(time.RFC3339))
	seedPingObservation(t, srv, txRecent, "pingobsa", 9.0, `[]`, `[]`, recent.Unix())

	txOld := seedPingTrigger(t, srv, "eqsender000002", "#s", "OldSender", old.Format(time.RFC3339))
	seedPingObservation(t, srv, txOld, "pingobsb", 9.0, `[]`, `[]`, old.Unix())

	runEquivalenceCheck(t, srv)
}

// --- equivalence: relay / observer leaderboards, duplicates -----------------

func TestPingScoresHistoryEquivalence_RelayAndObserverLeaderboards(t *testing.T) {
	srv, _ := setupPingScoresFixture(t)
	// pkrelay1 appears in both branches of ping 1 -- must count once for
	// that ping (relay-leaderboard within-ping dedup), plus once more in
	// ping 2 (across-ping accumulation).
	tx1 := seedPingTrigger(t, srv, "eqlb0000000001", "#l", "Alice", "2026-01-20T10:00:00Z")
	seedPingObservation(t, srv, tx1, "pingobsa", 9.0, `[]`, `[]`, 1737367200)
	seedPingObservation(t, srv, tx1, "pingobsb", 6.0, `["aa"]`, `["pkrelay1"]`, 1737367210)
	seedPingObservation(t, srv, tx1, "pingobsc", 4.0, `["aa"]`, `["pkrelay1"]`, 1737367220)

	tx2 := seedPingTrigger(t, srv, "eqlb0000000002", "#l", "Bob", "2026-01-21T10:00:00Z")
	seedPingObservation(t, srv, tx2, "pingobsa", 9.0, `[]`, `[]`, 1737453600)
	seedPingObservation(t, srv, tx2, "pingobsb", 6.0, `["aa"]`, `["pkrelay1"]`, 1737453610)

	runEquivalenceCheck(t, srv)
}

// --- equivalence: nil FarthestKm/AirtimeMs, derived KmPerSecondAirtime -----

// TestPingScoresHistoryEquivalence_LoneStationHasNilAirtime covers a
// single-observer, 0-hop ping (no relays at all): FarthestKm is a real,
// non-nil 0.0 (a station's distance from itself, the reference point --
// see buildPacketPathResponseFromReduction's First-branch handling), but
// AirtimeMs/RelayCount/KmPerSecondAirtime are genuinely nil (no relay
// airtime to estimate). Both paths must agree on this exactly.
func TestPingScoresHistoryEquivalence_LoneStationHasNilAirtime(t *testing.T) {
	srv, _ := setupPingScoresFixture(t)
	tx := seedPingTrigger(t, srv, "eqnilfar0000001", "#n", "Solo", "2026-01-22T10:00:00Z")
	seedPingObservation(t, srv, tx, "pingobsa", 9.0, `[]`, `[]`, 1737540000)
	runEquivalenceCheck(t, srv)

	// Direct assertion that this scenario really exercises what it claims
	// to (not vacuously passing because the ping was excluded for other
	// reasons).
	score := srv.computePingScore(pingTriggerRow{txID: tx, hash: "eqnilfar0000001", channelHash: "#n", sender: "Solo", firstSeen: "2026-01-22T10:00:00Z"})
	if score == nil {
		t.Fatal("computePingScore returned nil")
	}
	if score.FarthestKm == nil || *score.FarthestKm != 0 {
		t.Errorf("FarthestKm = %v, want a real 0.0 (distance from itself)", score.FarthestKm)
	}
	if score.AirtimeMs != nil || score.KmPerSecondAirtime != nil {
		t.Errorf("expected nil AirtimeMs/KmPerSecondAirtime for a relay-less ping, got %+v", score)
	}
}

// TestBuildPingScoresSnapshotFromHistory_DerivedKmPerSecondAirtime
// constructs an entry directly (bypassing the live GetPacketPath path, to
// isolate this specific derivation) with both FarthestKm and a positive
// AirtimeMs persisted, and confirms the resulting snapshot record carries
// the correctly-derived KmPerSecondAirtime -- not merely that
// materializePingScoreFromHistoryEntry computes it in isolation
// (TestMaterializePingScoreFromHistoryEntry_FullRoundTrip already covers
// that unit-level), but that it actually surfaces through record selection
// into MostEfficientPing.
func TestBuildPingScoresSnapshotFromHistory_DerivedKmPerSecondAirtime(t *testing.T) {
	srv, _ := setupPingScoresFixture(t)
	triggers := []pingTriggerRow{
		{txID: 1, hash: "kmpersec0000001", firstSeen: "2026-01-01T00:00:00Z"},
	}
	entries := []PingScoreHistoryEntry{
		{TxID: 1, Hash: "kmpersec0000001", StationCount: 2, DeepestHops: 1, FarthestKm: f64(150), AirtimeMs: f64(500), RelayCount: 1},
	}
	snap, err := srv.buildPingScoresSnapshotFromHistory(triggers, entries, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if snap.MostEfficientPing == nil {
		t.Fatal("MostEfficientPing = nil, want the derived-efficiency winner")
	}
	// 150km / (500ms/1000) = 300 km/s.
	if snap.MostEfficientPing.KmPerSecondAirtime == nil || *snap.MostEfficientPing.KmPerSecondAirtime != 300 {
		t.Errorf("KmPerSecondAirtime = %v, want 300", snap.MostEfficientPing.KmPerSecondAirtime)
	}
}

// --- unscorable / pruned / missing-pairing scenarios -------------------------

// TestBuildPingScoresSnapshotFromHistory_UnscorableEntrySkippedButCounted
// covers a trigger whose persisted entry is Unscorable=true (this cycle's
// -- or every cycle's -- GetPacketPath produced nothing usable): it must
// be excluded from every record/leaderboard, yet still counted in
// TotalPings (which reflects the live trigger table, not "how many were
// actually scored").
func TestBuildPingScoresSnapshotFromHistory_UnscorableEntrySkippedButCounted(t *testing.T) {
	srv, _ := setupPingScoresFixture(t)
	triggers := []pingTriggerRow{
		{txID: 1, hash: "unscore00001", channelHash: "#u", sender: "S", firstSeen: "2026-01-01T00:00:00Z"},
	}
	entries := []PingScoreHistoryEntry{
		{TxID: 1, Hash: "unscore00001", Unscorable: true},
	}
	snap, err := srv.buildPingScoresSnapshotFromHistory(triggers, entries, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if snap.TotalPings != 1 {
		t.Errorf("TotalPings = %d, want 1", snap.TotalPings)
	}
	if snap.FarthestPing != nil || snap.MostHopsPing != nil || snap.WidestSpreadPing != nil {
		t.Errorf("expected no records from a single unscorable entry, got %+v", snap)
	}
	if len(snap.RelayLeaderboard) != 0 || len(snap.ObserverLeaderboard) != 0 || len(snap.SenderLeaderboard) != 0 {
		t.Errorf("expected empty leaderboards, got %+v", snap)
	}
}

// TestBuildPingScoresSnapshotFromHistory_TriggerWithoutHistoryEntry covers
// a fresh trigger that has never been scored/persisted yet -- must be
// silently excluded (not an error), still counted in TotalPings.
func TestBuildPingScoresSnapshotFromHistory_TriggerWithoutHistoryEntry(t *testing.T) {
	srv, _ := setupPingScoresFixture(t)
	triggers := []pingTriggerRow{
		{txID: 1, hash: "notyetpersisted", firstSeen: "2026-01-01T00:00:00Z"},
	}
	snap, err := srv.buildPingScoresSnapshotFromHistory(triggers, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if snap.TotalPings != 1 {
		t.Errorf("TotalPings = %d, want 1", snap.TotalPings)
	}
	if snap.FarthestPing != nil {
		t.Errorf("FarthestPing = %+v, want nil", snap.FarthestPing)
	}
}

// TestBuildPingScoresSnapshotFromHistory_HistoryEntryWithoutTrigger covers
// a persisted entry for a tx_id ping_triggers no longer has a row for
// (e.g. pruned upstream, not yet reconciled out of the store) -- must be
// silently excluded, and must NOT inflate TotalPings.
func TestBuildPingScoresSnapshotFromHistory_HistoryEntryWithoutTrigger(t *testing.T) {
	srv, _ := setupPingScoresFixture(t)
	entries := []PingScoreHistoryEntry{
		{TxID: 99, Hash: "orphaned00001", StationCount: 5, DeepestHops: 3, Timestamp: "2026-01-01T00:00:00Z"},
	}
	snap, err := srv.buildPingScoresSnapshotFromHistory(nil, entries, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if snap.TotalPings != 0 {
		t.Errorf("TotalPings = %d, want 0 -- an orphaned entry must not inflate the live trigger count", snap.TotalPings)
	}
	if snap.MostHopsPing != nil {
		t.Errorf("MostHopsPing = %+v, want nil -- an entry with no matching trigger must be excluded entirely", snap.MostHopsPing)
	}
}

// TestBuildPingScoresSnapshotFromHistory_ChangedTriggerHashDoesNotCorrupt
// is a robustness check, not a correctness contract: reconciling a
// changed hash/timestamp (recomputing it) is planPingScoreHistoryReconcile's
// job, run BEFORE building a snapshot in the intended flow -- this test
// only confirms that calling buildPingScoresSnapshotFromHistory directly
// with a stale (unreconciled) entry doesn't crash or produce a
// field-mixed/corrupted result: the entry's OWN persisted hash/timestamp
// are shown (not silently substituted with the trigger's), since deciding
// what to do about the mismatch belongs to reconciliation, not this
// function.
func TestBuildPingScoresSnapshotFromHistory_ChangedTriggerHashDoesNotCorrupt(t *testing.T) {
	srv, _ := setupPingScoresFixture(t)
	triggers := []pingTriggerRow{
		{txID: 1, hash: "newhash0000001", firstSeen: "2026-01-01T00:00:00Z"},
	}
	entries := []PingScoreHistoryEntry{
		{TxID: 1, Hash: "oldhash0000001", StationCount: 1, DeepestHops: 0, Timestamp: "2026-01-01T00:00:00Z"},
	}
	snap, err := srv.buildPingScoresSnapshotFromHistory(triggers, entries, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if snap.TotalPings != 1 {
		t.Errorf("TotalPings = %d, want 1", snap.TotalPings)
	}
	if snap.MostHopsPing == nil || snap.MostHopsPing.Hash != "oldhash0000001" {
		t.Errorf("MostHopsPing = %+v, want the entry's own (stale) hash -- reconciliation, not this function, decides what to do about the mismatch", snap.MostHopsPing)
	}
}

// TestBuildPingScoresSnapshotFromHistory_InvalidTimestampExcludedFromWeekAndSender
// mirrors computeAllPingScores' existing fail-toward-stale convention: an
// unparseable timestamp is treated as too old, not defaulted to included.
func TestBuildPingScoresSnapshotFromHistory_InvalidTimestampExcludedFromWeekAndSender(t *testing.T) {
	srv, _ := setupPingScoresFixture(t)
	triggers := []pingTriggerRow{
		{txID: 1, hash: "badts0000001", sender: "BadTS", firstSeen: "not-a-timestamp"},
	}
	entries := []PingScoreHistoryEntry{
		{TxID: 1, Hash: "badts0000001", Sender: "BadTS", Timestamp: "not-a-timestamp", StationCount: 1, DeepestHops: 0},
	}
	snap, err := srv.buildPingScoresSnapshotFromHistory(triggers, entries, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if snap.ThisWeek != nil {
		t.Errorf("ThisWeek = %+v, want nil -- an unparseable timestamp must be excluded, not treated as recent", snap.ThisWeek)
	}
	if len(snap.SenderLeaderboard) != 0 {
		t.Errorf("SenderLeaderboard = %+v, want empty -- an unparseable timestamp must be excluded, not treated as recent", snap.SenderLeaderboard)
	}
	// All-time records are NOT timestamp-gated -- still included.
	if snap.MostHopsPing == nil {
		t.Error("MostHopsPing = nil, want the ping (all-time records don't depend on a parseable timestamp)")
	}
}

// --- name enrichment: missing names, rename-between-persist-and-build ------

// TestBuildPingScoresSnapshotFromHistory_MissingNamesFallBackToPubkey is
// deliberately NOT an equivalence-with-live test: computePingScore's live
// DeepestName is whatever observers.name literally contains, even if that
// is an empty string (no fallback exists in that path today, and this
// phase must not change computeAllPingScores/computePingScore to add one).
// buildPingScoresSnapshotFromHistory's OWN name-enrichment, per this
// phase's spec, IS explicitly best-effort with a pubkey fallback -- so for
// an observer with no persisted name at all, the two paths intentionally
// differ (live: "", history-based: the pubkey) by design, not by bug. This
// test asserts the history-based function's own documented behavior
// directly, rather than comparing it against live for this one field.
func TestBuildPingScoresSnapshotFromHistory_MissingNamesFallBackToPubkey(t *testing.T) {
	srv, _ := setupPingScoresFixture(t)
	// "pingobsnoname" has no row in `observers` at all -- can't resolve a name.
	triggers := []pingTriggerRow{
		{txID: 1, hash: "noname0000001", firstSeen: "2026-01-01T00:00:00Z"},
	}
	entries := []PingScoreHistoryEntry{
		{TxID: 1, Hash: "noname0000001", StationCount: 1, DeepestHops: 0, DeepestPubkey: "pingobsnoname", FirstPubkey: "pingobsnoname"},
	}
	snap, err := srv.buildPingScoresSnapshotFromHistory(triggers, entries, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if snap.MostHopsPing == nil {
		t.Fatal("MostHopsPing = nil")
	}
	if snap.MostHopsPing.DeepestName != "pingobsnoname" {
		t.Errorf("DeepestName = %q, want the pubkey fallback %q", snap.MostHopsPing.DeepestName, "pingobsnoname")
	}
}

// TestBuildPingScoresSnapshotFromHistory_NodeRenameReflectsFreshAtSnapshotTime
// proves names are resolved FRESH at snapshot-build time, never frozen at
// persist time: an entry is built from a score computed BEFORE a rename,
// then the observer is renamed, then the snapshot is built from that
// (unchanged) history entry -- the NEW name must appear, not the one that
// was current when the entry was created.
func TestBuildPingScoresSnapshotFromHistory_NodeRenameReflectsFreshAtSnapshotTime(t *testing.T) {
	srv, _ := setupPingScoresFixture(t)
	txID := seedPingTrigger(t, srv, "rename0000001", "#r", "S", "2026-01-01T00:00:00Z")
	seedPingObservation(t, srv, txID, "pingobsa", 9.0, `[]`, `[]`, 1735689600)

	trigger := pingTriggerRow{txID: txID, hash: "rename0000001", channelHash: "#r", sender: "S", firstSeen: "2026-01-01T00:00:00Z"}
	score := srv.computePingScore(trigger)
	if score == nil || score.firstName != "ObsA" {
		t.Fatalf("sanity check failed: score = %+v, want firstName=ObsA before rename", score)
	}
	entry := pingScoreHistoryEntryFromScore(trigger, score, observationFingerprint{}, PingScoreHistoryEntryState{}, time.Now())

	if _, err := srv.db.conn.Exec(`UPDATE observers SET name = 'ObsA Renamed' WHERE id = 'pingobsa'`); err != nil {
		t.Fatalf("rename observer: %v", err)
	}

	snap, err := srv.buildPingScoresSnapshotFromHistory([]pingTriggerRow{trigger}, []PingScoreHistoryEntry{entry}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.ObserverLeaderboard) != 1 || snap.ObserverLeaderboard[0].Name != "ObsA Renamed" {
		t.Errorf("ObserverLeaderboard = %+v, want the RENAMED name (looked up fresh, not frozen from persist time)", snap.ObserverLeaderboard)
	}
}

// --- determinism -------------------------------------------------------------

func TestBuildPingScoresSnapshotFromHistory_DeterministicRepeatedCalls(t *testing.T) {
	srv, _ := setupPingScoresFixture(t)
	tx1 := seedPingTrigger(t, srv, "determ0000001", "#d", "Alice", "2026-01-15T10:00:00Z")
	seedPingObservation(t, srv, tx1, "pingobsa", 9.0, `[]`, `[]`, 1736935200)
	seedPingObservation(t, srv, tx1, "pingobsb", 6.0, `["aa"]`, `["pkrelay1"]`, 1736935210)
	seedPingObservation(t, srv, tx1, "pingobsc", 4.0, `["aa","bb"]`, `["pkrelay1","pkrelay2"]`, 1736935260)

	triggers, err := srv.db.fetchPingTriggers()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	entries := historyEntriesFromLiveScores(t, srv, triggers, now)

	snap1, err := srv.buildPingScoresSnapshotFromHistory(triggers, entries, now)
	if err != nil {
		t.Fatal(err)
	}
	snap2, err := srv.buildPingScoresSnapshotFromHistory(triggers, entries, now)
	if err != nil {
		t.Fatal(err)
	}
	j1, _ := json.Marshal(snap1)
	j2, _ := json.Marshal(snap2)
	if string(j1) != string(j2) {
		t.Errorf("repeated calls with identical inputs produced different output:\n1: %s\n2: %s", j1, j2)
	}
	// Also confirm entries themselves were never mutated by the build.
	entries2 := historyEntriesFromLiveScores(t, srv, triggers, now)
	j3, _ := json.Marshal(entries)
	j4, _ := json.Marshal(entries2)
	if string(j3) != string(j4) {
		t.Error("input entries appear to have been mutated by buildPingScoresSnapshotFromHistory")
	}
}
