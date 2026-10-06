package main

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A nonempty recompute is not necessarily complete evidence: the raw packet
// survives while node retention removes the coordinates used by its record.
func TestPingRecordDistanceSurvivesMissingGPS(t *testing.T) {
	for _, partial := range []bool{false, true} {
		name := "all_coordinates_missing"
		if partial {
			name = "nearer_station_still_positioned"
		}
		t.Run(name, func(t *testing.T) {
			fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 100})
			id := seedPingTrigger(t, fx.srv, "gpsloss000001", "#test", "sender", "2026-01-15T10:00:00Z")
			seedPingObservation(t, fx.srv, id, "pingobsa", 9, `[]`, `[]`, 1736935200)
			seedPingObservation(t, fx.srv, id, "pingobsb", 7, `["aa"]`, `["relay"]`, 1736935210)
			seedPingObservation(t, fx.srv, id, "pingobsc", 5, `["aa","bb"]`, `["relay","relay2"]`, 1736935220)
			settleEntry(t, fx)
			before, _ := fx.engine.index.Get(id)
			if before.FarthestKm == nil || *before.FarthestKm < 200 {
				t.Fatal("fixture lacks real farthest record")
			}
			sql := `DELETE FROM nodes WHERE public_key IN ('pingobsa','pingobsb','pingobsc')`
			if partial {
				sql = `DELETE FROM nodes WHERE public_key = 'pingobsb'`
			}
			if _, err := fx.srv.db.conn.Exec(sql); err != nil {
				t.Fatal(err)
			}
			fx.clock.Advance(time.Minute)
			snap, err := fx.engine.Cycle()
			if err != nil {
				t.Fatal(err)
			}
			after, _ := fx.engine.index.Get(id)
			if after.FarthestKm == nil || *after.FarthestKm != *before.FarthestKm || after.FarthestPubkey != before.FarthestPubkey {
				t.Errorf("missing GPS downgraded retained distance: before=%v/%s after=%v/%s", before.FarthestKm, before.FarthestPubkey, after.FarthestKm, after.FarthestPubkey)
			}
			if snap.FarthestPing == nil || *snap.FarthestPing.FarthestKm != *before.FarthestKm {
				t.Error("all-time distance record disappeared or shrank")
			}
		})
	}
}

// The path API deliberately exposes an airport fallback as ordinary
// coordinates. It is not evidence that the observer's former own GPS
// remains available, so it cannot justify shrinking a historical record.
func TestPingRecordDistanceSurvivesKnownIATAFallback(t *testing.T) {
	for _, onlyFarthestMissing := range []bool{false, true} {
		name := "both_airport_fallbacks"
		if onlyFarthestMissing {
			name = "farthest_airport_fallback"
		}
		t.Run(name, func(t *testing.T) {
			fx, _, before := seedRecordArchiveFixture(t)
			for _, statement := range []string{
				`UPDATE observers SET iata='AAR' WHERE id='pingobsa'`,
				`UPDATE observers SET iata='CPH' WHERE id='pingobsb'`,
			} {
				if _, err := fx.srv.db.conn.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			statement := `DELETE FROM nodes WHERE public_key IN ('pingobsa','pingobsb')`
			if onlyFarthestMissing {
				statement = `DELETE FROM nodes WHERE public_key='pingobsb'`
			}
			if _, err := fx.srv.db.conn.Exec(statement); err != nil {
				t.Fatal(err)
			}
			path, err := fx.srv.db.GetPacketPath(before.FarthestPing.Hash, EstimateMaxEdgeKm)
			if err != nil {
				t.Fatal(err)
			}
			fresh := fx.srv.buildPingScoreFromPath(pingTriggerRow{hash: before.FarthestPing.Hash}, path)
			if fresh.FarthestKm == nil || *fresh.FarthestKm >= *before.FarthestPing.FarthestKm {
				t.Fatal("fixture lacks a non-nil airport-based downgrade")
			}
			fx.clock.Advance(time.Minute)
			after, err := fx.engine.Cycle()
			if err != nil {
				t.Fatal(err)
			}
			if after.FarthestPing == nil || *after.FarthestPing.FarthestKm != *before.FarthestPing.FarthestKm {
				t.Fatal("airport fallback shrank historical GPS distance")
			}
			if !reflect.DeepEqual(before.pathArchives["allTime.farthestPing"], after.pathArchives["allTime.farthestPing"]) {
				t.Fatal("airport fallback replaced original record evidence")
			}
		})
	}
}

func seedRecordArchiveFixture(t *testing.T) (*engineFixture, int64, *PingScoresSnapshot) {
	t.Helper()
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 100, RetentionDuration: 30 * 24 * time.Hour})
	ts := fx.clock.Now().Add(-time.Hour)
	id := seedPingTrigger(t, fx.srv, "archiveping001", "#test", "sender", ts.UTC().Format(time.RFC3339))
	seedPingObservation(t, fx.srv, id, "pingobsa", 9, `[]`, `[]`, ts.Unix())
	seedPingObservation(t, fx.srv, id, "pingobsb", 7, `["aa"]`, `["relay"]`, ts.Unix()+10)
	seedPingObservation(t, fx.srv, id, "pingobsc", 5, `["aa","bb"]`, `["relay","relay2"]`, ts.Unix()+20)
	settleEntry(t, fx)
	snap, err := fx.engine.QuickSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snap.pathArchives["allTime.farthestPing"]; !ok {
		t.Fatal("farthest archive not captured")
	}
	return fx, id, snap
}

func TestPingRecordArchivesSurviveRetentionAndRestart(t *testing.T) {
	fx, id, before := seedRecordArchiveFixture(t)
	if len(before.pathArchives) > 10 {
		t.Fatal("too many archives")
	}
	old := before.pathArchives["allTime.farthestPing"]
	if _, err := fx.srv.db.conn.Exec(`DELETE FROM observations WHERE transmission_id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.srv.db.conn.Exec(`DELETE FROM transmissions WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	fx.clock.Advance(40 * 24 * time.Hour)
	after, err := fx.engine.Cycle()
	if err != nil {
		t.Fatal(err)
	}
	if after.FarthestPing == nil || !reflect.DeepEqual(old, after.pathArchives["allTime.farthestPing"]) {
		t.Fatal("retention lost or reinterpreted record path")
	}
	for key := range after.pathArchives {
		if strings.HasPrefix(key, "thisWeek.") {
			t.Fatal("superseded weekly archive remains")
		}
	}
	fx.reopenEngine(t)
	restarted, err := fx.engine.QuickSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(old, restarted.pathArchives["allTime.farthestPing"]) {
		t.Fatal("restart lost archived positions")
	}
	live, err := fx.srv.db.GetPacketPath(old.Hash, EstimateMaxEdgeKm)
	if err != nil || len(live.Branches) != 0 {
		t.Fatal("fixture did not lose live path")
	}
}

func TestPingRecordArchivesKeepIndependentEvidenceForSameHash(t *testing.T) {
	fx, id, before := seedRecordArchiveFixture(t)
	old := before.pathArchives["allTime.farthestPing"]
	if _, err := fx.srv.db.conn.Exec(`DELETE FROM nodes WHERE public_key='pingobsb'`); err != nil {
		t.Fatal(err)
	}
	seedPingObservation(t, fx.srv, id, "pingobsc", 5, `["aa","bb","cc"]`, `["relay","relay2","relay3"]`, fx.clock.Now().Unix())
	fx.clock.Advance(time.Minute)
	after, err := fx.engine.Cycle()
	if err != nil {
		t.Fatal(err)
	}
	f := after.pathArchives["allTime.farthestPing"]
	h := after.pathArchives["allTime.mostHopsPing"]
	if !reflect.DeepEqual(f, old) {
		t.Fatal("poorer GPS overwrote farthest record archive")
	}
	if f.Hash != h.Hash || h.Path.Branches[0].Hops != 3 || h.CapturedAt == f.CapturedAt {
		t.Fatal("same-hash records failed to keep their independent evidence")
	}
}

func TestPingRecordArchiveCommitFailureLeavesLastPublishedAndPersistedState(t *testing.T) {
	fx, _, before := seedRecordArchiveFixture(t)
	fx.srv.pingScores.Store(before)
	oldEntries, err := fx.store.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	// A deferred FK causes a real final Commit failure, after archive and
	// score INSERTs succeeded. It exercises rollback, not just validation.
	for _, statement := range []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE review_archive_parent (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE review_archive_commit_guard (id INTEGER REFERENCES review_archive_parent(id) DEFERRABLE INITIALLY DEFERRED)`,
		`CREATE TRIGGER review_archive_commit_fail AFTER INSERT ON ping_score_path_archives BEGIN INSERT INTO review_archive_commit_guard VALUES (123); END`,
	} {
		if _, err := fx.store.conn.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fx.srv.db.conn.Exec(`UPDATE nodes SET lat=56.01,lon=10.01 WHERE public_key='pingobsb'`); err != nil {
		t.Fatal(err)
	}
	fx.clock.Advance(time.Minute)
	after, err := fx.engine.Cycle()
	if err == nil || after != nil {
		t.Fatal("expected cycle failure at commit")
	}
	persisted, err := fx.store.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(oldEntries, persisted) || fx.srv.pingScores.Load() != before {
		t.Fatal("failed commit changed scores")
	}
	archives, err := fx.store.LoadPathArchives()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(archives, before.pathArchives) || !reflect.DeepEqual(fx.engine.pathArchives, before.pathArchives) {
		t.Fatal("failed commit replaced old archive evidence")
	}
}

func TestPingRecordPathArchiveBoundsAndOversizedLoad(t *testing.T) {
	fx, _, snap := seedRecordArchiveFixture(t)
	a := snap.pathArchives["allTime.farthestPing"]
	for _, name := range []string{"branches", "points", "bytes"} {
		t.Run(name, func(t *testing.T) {
			b, _ := json.Marshal(a.Path)
			var path PacketPathResponse
			if err := json.Unmarshal(b, &path); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "branches":
				path.Branches = make([]PacketPathBranch, pingScorePathArchiveMaxBranches+1)
			case "points":
				path.Branches[0].Points = make([]PacketPathPoint, pingScorePathArchiveMaxPoints+1)
			case "bytes":
				path.Branches[0].Observer.Name = strings.Repeat("x", pingScorePathArchiveMaxBytes)
			}
			if _, err := boundedPingPathJSON(&path); err == nil {
				t.Fatal("oversized path was accepted")
			}
		})
	}
	if _, err := fx.store.conn.Exec(`PRAGMA ignore_check_constraints=ON`); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.store.conn.Exec(`UPDATE ping_score_path_archives SET path_json=? WHERE record_key='allTime.farthestPing'`, strings.Repeat("x", pingScorePathArchiveMaxBytes+1)); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.store.LoadPathArchives(); err == nil || !strings.Contains(err.Error(), "oversized") {
		t.Fatalf("oversized persisted JSON must be rejected before parsing: %v", err)
	}
}

func TestPingRecordPathArchivesDeduplicateExtraBulkFetch(t *testing.T) {
	fx := setupEngineFaultFixture(t, pingScoreHistoryEngineConfig{})
	seedFaultTrigger(t, fx, 1, "bulkarchive01")
	snap, err := fx.srv.buildPingScoresSnapshotFromHistory([]pingTriggerRow{{txID: 1, hash: "bulkarchive01", firstSeen: "2026-01-15T10:00:00Z"}}, []PingScoreHistoryEntry{{TxID: 1, Hash: "bulkarchive01", Timestamp: "2026-01-15T10:00:00Z", StationCount: 1, DeepestHops: 0, DeepestPubkey: "obsbulkarchive01"}}, fx.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	resetBulkTestQueryLog()
	archives, _, err := fx.engine.pathsForRecords(snap, nil, nil, fx.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	queries := 0
	for _, q := range bulkTestQueryLog() {
		if strings.Contains(q.sql, "FROM observations o") {
			queries++
			if q.argCount != 1 {
				t.Fatalf("same record hash fetched multiple times: %d args", q.argCount)
			}
		}
	}
	if queries != 1 || len(archives) != 2 {
		t.Fatalf("want one bulk fetch and two slots, got %d queries/%d archives", queries, len(archives))
	}
}

func TestPingScoreHistoryV2ToV3PreservesExistingScores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := applyPingScoreHistoryV1(tx); err != nil {
		t.Fatal(err)
	}
	if err := applyPingScoreHistoryV2(tx); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO _meta VALUES ('schema_version','2')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO ping_score_history_entries(tx_id,hash,timestamp,station_count,deepest_hops,first_pubkey,farthest_km,farthest_pubkey,computed_at) VALUES (1,'older-record','2026-01-01T00:00:00Z',20,8,'legacy-first',100,'legacy-farthest','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	entries, err := store.LoadAll()
	if err != nil || len(entries) != 1 || entries[0].StationCount != 20 {
		t.Fatalf("v2 score not preserved: %v %+v", err, entries)
	}
	score, err := materializePingScoreFromHistoryEntry(entries[0])
	if err != nil || entries[0].DistanceFirstPubkey != "" || score.distanceFirstPubkey != "legacy-first" {
		t.Fatal("legacy nullable distance-origin fallback was not preserved")
	}
	archives, err := store.LoadPathArchives()
	if err != nil || len(archives) != 0 {
		t.Fatal("migration fabricated old paths")
	}
	version, err := store.readSchemaVersion()
	if err != nil || version != 3 {
		t.Fatal("archive schema not installed")
	}
}

func TestPingRecordAlreadyPrunedWithoutArchiveRemainsUnavailable(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{})
	_, err := fx.srv.db.conn.Exec(`INSERT INTO ping_triggers(tx_id,hash,first_seen) VALUES(900,'prunedrecord01','2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	e := PingScoreHistoryEntry{TxID: 900, Hash: "prunedrecord01", Timestamp: "2026-01-01T00:00:00Z", StationCount: 20, DeepestHops: 8, ComputedAt: fx.clock.Now().UTC().Format(time.RFC3339)}
	if err := fx.store.UpsertAndDelete([]PingScoreHistoryEntry{e}, nil); err != nil {
		t.Fatal(err)
	}
	fx.reopenEngine(t)
	snap, err := fx.engine.Cycle()
	if err != nil {
		t.Fatal(err)
	}
	if snap.MostHopsPing == nil || len(snap.pathArchives) != 0 {
		t.Fatal("lost retained score or invented historical route")
	}
}

func TestPingRecordDistanceAllowsGenuineGPSCorrection(t *testing.T) {
	fx := setupEngineFixture(t, pingScoreHistoryEngineConfig{SettleDebounce: time.Minute, DeepSweepBatchSize: 100})
	id := seedPingTrigger(t, fx.srv, "gpscorrect001", "#test", "sender", "2026-01-15T10:00:00Z")
	seedPingObservation(t, fx.srv, id, "pingobsa", 9, `[]`, `[]`, 1736935200)
	seedPingObservation(t, fx.srv, id, "pingobsb", 7, `["aa"]`, `["relay"]`, 1736935210)
	settleEntry(t, fx)
	before, _ := fx.engine.index.Get(id)
	if _, err := fx.srv.db.conn.Exec(`UPDATE nodes SET lat = 56.01, lon = 10.01 WHERE public_key = 'pingobsb'`); err != nil {
		t.Fatal(err)
	}
	fx.clock.Advance(time.Minute)
	if _, err := fx.engine.Cycle(); err != nil {
		t.Fatal(err)
	}
	after, _ := fx.engine.index.Get(id)
	if after.FarthestKm == nil || *after.FarthestKm >= *before.FarthestKm {
		t.Error("a genuine positioned correction was incorrectly frozen")
	}
}

// First-hearer credit is a live observation fact, distinct from the
// origin landmark for an old retained distance. Preserving the latter
// must not pin the observer leaderboard to a station no longer first.
func TestPingRecordMissingGPSKeepsDistanceOriginNotFirstHearer(t *testing.T) {
	fx, id, before := seedRecordArchiveFixture(t)
	old := before.pathArchives["allTime.farthestPing"]
	if _, err := fx.srv.db.conn.Exec(`DELETE FROM nodes WHERE public_key='pingobsb'`); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.srv.db.conn.Exec(`INSERT INTO observers (id,name) VALUES ('pingobsnew','EarlierObserver')`); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.srv.db.conn.Exec(`INSERT INTO nodes (public_key,name,lat,lon) VALUES ('pingobsnew','EarlierObserver',56.02,10.02)`); err != nil {
		t.Fatal(err)
	}
	ts, err := time.Parse(time.RFC3339, before.FarthestPing.Timestamp)
	if err != nil {
		t.Fatal(err)
	}
	seedPingObservation(t, fx.srv, id, "pingobsnew", 10, `[]`, `[]`, ts.Unix()-20)
	fx.clock.Advance(time.Minute)
	after, err := fx.engine.Cycle()
	if err != nil {
		t.Fatal(err)
	}
	check := func(snap *PingScoresSnapshot) {
		t.Helper()
		entry, _ := fx.engine.index.Get(id)
		if entry.FirstPubkey != "pingobsnew" || snap.FarthestPing.firstPubkey != "pingobsnew" {
			t.Errorf("old distance origin incorrectly credited as first hearer: entry=%s, score=%s", entry.FirstPubkey, snap.FarthestPing.firstPubkey)
		}
		if entry.DistanceFirstPubkey != "pingobsa" || snap.FarthestPing.distanceFirstPubkey != "pingobsa" {
			t.Errorf("retained distance origin changed: entry=%s, score=%s", entry.DistanceFirstPubkey, snap.FarthestPing.distanceFirstPubkey)
		}
		if len(snap.ObserverLeaderboard) != 1 || snap.ObserverLeaderboard[0].Pubkey != "pingobsnew" || snap.ObserverLeaderboard[0].Count != 1 {
			t.Errorf("first-hearer leaderboard must credit new earlier observation: %+v", snap.ObserverLeaderboard)
		}
		if *snap.FarthestPing.FarthestKm != *before.FarthestPing.FarthestKm || !reflect.DeepEqual(old, snap.pathArchives["allTime.farthestPing"]) {
			t.Error("retained distance lost its original landmark evidence")
		}
	}
	check(after)
	// A physical sidecar close/reopen, not merely rebuilding its in-memory
	// index, proves the two identities survive persistence independently.
	path := fx.store.path
	if err := fx.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fx.store = store
	fx.reopenEngine(t)
	restarted, err := fx.engine.QuickSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	check(restarted)
}

func TestPingRecordPathArchiveIsImmutableAndChecksLandmarks(t *testing.T) {
	fx, _, snap := seedRecordArchiveFixture(t)
	a := snap.pathArchives["allTime.farthestPing"]
	b, _ := json.Marshal(a.Path)
	var source PacketPathResponse
	if err := json.Unmarshal(b, &source); err != nil {
		t.Fatal(err)
	}
	next, ok := newPingScorePathArchive(a.RecordKey, snap.FarthestPing, &source, fx.clock.Now())
	if !ok {
		t.Fatal("valid matching path rejected")
	}
	source.Branches[0].Observer.Name = "mutated source"
	source.First.Observer.PublicKey = "other origin"
	if !reflect.DeepEqual(next.Path, a.Path) {
		t.Fatal("archive retained a mutable reference to the live response")
	}
	if _, ok := newPingScorePathArchive(a.RecordKey, snap.FarthestPing, &source, fx.clock.Now()); ok {
		t.Fatal("same numeric distance with a different first landmark accepted")
	}
}

func TestPingRecordGenuineCorrectionReplacesArchive(t *testing.T) {
	fx, _, before := seedRecordArchiveFixture(t)
	old := before.pathArchives["allTime.farthestPing"]
	if _, err := fx.srv.db.conn.Exec(`UPDATE nodes SET lat=56.01,lon=10.01 WHERE public_key='pingobsb'`); err != nil {
		t.Fatal(err)
	}
	// Do not advance the clock: a real geometry correction within one
	// RFC3339 second must still be persisted, not mistaken for unchanged.
	after, err := fx.engine.Cycle()
	if err != nil {
		t.Fatal(err)
	}
	next, ok := after.pathArchives["allTime.farthestPing"]
	if !ok || *after.FarthestPing.FarthestKm >= *before.FarthestPing.FarthestKm || reflect.DeepEqual(old.Path, next.Path) {
		t.Fatal("genuine coordinate correction did not replace metric and archive")
	}
	stored, err := fx.store.LoadPathArchives()
	if err != nil || !reflect.DeepEqual(next, stored["allTime.farthestPing"]) {
		t.Fatalf("same-second corrected archive not persisted: %v", err)
	}
}

func TestPingRecordPathArchiveSlotBoundAndInvalidWriteRollback(t *testing.T) {
	fx, _, snap := seedRecordArchiveFixture(t)
	old := snap.pathArchives["allTime.farthestPing"]
	full := make(map[string]PingScorePathArchive)
	for _, prefix := range []string{"allTime", "thisWeek"} {
		for _, kind := range pingScoreRecordKinds {
			a := old
			a.RecordKey = prefix + "." + kind
			full[a.RecordKey] = a
		}
	}
	if err := fx.store.upsertDeleteMetadataAndArchives(nil, nil, nil, nil, nil, full); err != nil {
		t.Fatal(err)
	}
	stored, err := fx.store.LoadPathArchives()
	if err != nil || len(stored) != pingScorePathArchiveMaxRecords {
		t.Fatalf("ten-slot archive failed: %v / %d", err, len(stored))
	}
	full["extra"] = old
	if err := fx.store.upsertDeleteMetadataAndArchives(nil, nil, nil, nil, nil, full); err == nil {
		t.Fatal("eleventh archive accepted")
	}
	delete(full, "extra")
	bad := old
	bad.RecordKey = "invalid.slot"
	delete(full, "allTime.farthestPing")
	full[bad.RecordKey] = bad
	if err := fx.store.upsertDeleteMetadataAndArchives(nil, nil, nil, nil, nil, full); err == nil {
		t.Fatal("invalid slot accepted")
	}
	after, err := fx.store.LoadPathArchives()
	if err != nil || !reflect.DeepEqual(stored, after) {
		t.Fatalf("invalid write removed old committed archives: %v", err)
	}
}

func TestPingRecordFutureSchemaLoadsArchivesReadOnly(t *testing.T) {
	fx, _, snap := seedRecordArchiveFixture(t)
	if _, err := fx.store.conn.Exec(`UPDATE _meta SET value='999' WHERE key='schema_version'`); err != nil {
		t.Fatal(err)
	}
	path := fx.store.path
	if err := fx.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	archives, err := store.LoadPathArchives()
	if err != nil || !store.ReadOnly() || !reflect.DeepEqual(archives, snap.pathArchives) {
		t.Fatalf("future schema archive read failed: %v", err)
	}
	if err := store.upsertDeleteMetadataAndArchives(nil, nil, nil, nil, nil, map[string]PingScorePathArchive{}); err == nil {
		t.Fatal("future schema permitted archive write")
	}
	if _, err := store.conn.Exec(`DELETE FROM ping_score_path_archives`); err == nil {
		t.Fatal("future schema connection is physically writable")
	}
}

func TestPingRecordFutureLegacySchemaUsesReadOnlyDistanceOriginFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future-legacy.db")
	conn, err := openPingScoreHistoryConn(path, true)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := applyPingScoreHistoryV1(tx); err != nil {
		t.Fatal(err)
	}
	if err := applyPingScoreHistoryV2(tx); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO _meta VALUES ('schema_version','999')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO ping_score_history_entries(tx_id,hash,timestamp,station_count,deepest_hops,first_pubkey,farthest_km,computed_at) VALUES (1,'future-legacy','2026-01-01T00:00:00Z',2,1,'legacy-first',10,'2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenPingScoreHistoryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	entries, err := store.LoadAll()
	if err != nil || !store.ReadOnly() || len(entries) != 1 {
		t.Fatalf("future legacy score read failed: %v", err)
	}
	score, err := materializePingScoreFromHistoryEntry(entries[0])
	if err != nil || score.distanceFirstPubkey != "legacy-first" || score.firstPubkey != "legacy-first" {
		t.Fatal("future legacy distance origin not materialized")
	}
	archives, err := store.LoadPathArchives()
	if err != nil || len(archives) != 0 {
		t.Fatal("future legacy archive absence should be unavailable, not a migration")
	}
	if _, err := store.conn.Exec(`ALTER TABLE ping_score_history_entries ADD COLUMN distance_first_pubkey TEXT`); err == nil {
		t.Fatal("future legacy connection is physically writable")
	}
}

func BenchmarkPingRecordPathArchiveCapture(b *testing.B) {
	path := PacketPathResponse{Hash: "bench-ping"}
	for i := 0; i < 16; i++ {
		branch := PacketPathBranch{Hops: 16, Observer: &PacketPathObserver{PublicKey: strings.Repeat("a", 64), Name: "observer"}}
		for j := 0; j < 16; j++ {
			branch.Points = append(branch.Points, PacketPathPoint{PublicKey: strings.Repeat("b", 64), Name: "repeater", Role: "repeater"})
		}
		path.Branches = append(path.Branches, branch)
	}
	score := &PingScore{Hash: path.Hash, StationCount: len(path.Branches), Timestamp: "2026-01-15T10:00:00Z"}
	now := time.Date(2026, 1, 15, 11, 0, 0, 0, time.UTC)
	data, err := boundedPingPathJSON(&path)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(len(data)), "archive-bytes")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := newPingScorePathArchive("allTime.widestSpreadPing", score, &path, now); !ok {
			b.Fatal("representative bounded archive rejected")
		}
	}
}
