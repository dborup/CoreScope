package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

func TestPingScorePathLiveRecord(t *testing.T) {
	srv, router := setupPingScoresFixture(t)
	hash := "pingpathlive01"
	txID := seedPingTrigger(t, srv, hash, "#test", "Sender", "2026-01-15T10:00:00Z")
	seedPingObservation(t, srv, txID, "pingobsa", 9, `[]`, `[]`, 1736935200)
	seedPingObservation(t, srv, txID, "pingobsb", 6, `["aa"]`, `["pkrelay1"]`, 1736935210)
	srv.pingScores.Store(srv.computeAllPingScores())
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest("GET", "/api/ping-scores/"+hash+"/path?record=allTime.farthestPing", nil))
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var response struct {
		Status string              `json:"status"`
		Path   *PacketPathResponse `json:"path"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "live" || response.Path == nil || len(response.Path.Branches) != 2 {
		t.Fatalf("response=%+v, want live path with two branches", response)
	}
}

func TestPingScorePathMissingOldDataIsExplicit(t *testing.T) {
	srv, router := setupPingScoresFixture(t)
	srv.pingScores.Store(&PingScoresSnapshot{TotalPings: 1, FarthestPing: &PingScore{
		Hash: "oldrecord", Timestamp: "2026-01-15T10:00:00Z", StationCount: 2,
	}})
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest("GET", "/api/ping-scores/oldrecord/path?record=allTime.farthestPing", nil))
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var response struct {
		Status string              `json:"status"`
		Reason string              `json:"reason"`
		Path   *PacketPathResponse `json:"path"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "unavailable" || response.Reason != "raw_data_expired_before_capture" || response.Path != nil {
		t.Fatalf("response=%+v, want explicit unavailable without an empty map", response)
	}
}

func TestPingScorePathInitializingIsNotNoPings(t *testing.T) {
	_, router := setupPingScoresFixture(t)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest("GET", "/api/ping-scores/oldrecord/path?record=allTime.farthestPing", nil))
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var response struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "initializing" {
		t.Fatalf("status=%q, want initializing", response.Status)
	}
}

func testPingScorePathArchive(t *testing.T, srv *Server) *PingScoresSnapshot {
	t.Helper()
	lat, lon, farLat, distance, seconds, airtime := 56.0, 10.0, 57.0, 111.0, 20.0, 50.0
	first := PacketPathBranch{Observer: &PacketPathObserver{PublicKey: "pingobsa", Name: "Saved A", Lat: &lat, Lon: &lon}}
	far := PacketPathBranch{Hops: 1, Points: []PacketPathPoint{{PublicKey: "RELAY", Name: "Saved relay", Lat: &lat, Lon: &lon}}, Observer: &PacketPathObserver{PublicKey: "PINGOBSB", Name: "Saved B", Lat: &farLat, Lon: &lon}, DistanceFromFirstKm: &distance, SecondsAfterFirst: &seconds}
	path := PacketPathResponse{Hash: "archive01", Branches: []PacketPathBranch{far, first}, First: &first, EstimatedAirtimeMs: &airtime, AirtimeRelayCount: 1, TouchedAreas: []TouchedAreaShape{{Label: "Obsolete area"}}}
	score := &PingScore{Hash: path.Hash, Timestamp: "2026-01-15T10:00:00Z", StationCount: 2, FarthestKm: &distance}
	snap := &PingScoresSnapshot{TotalPings: 1, FarthestPing: score, pathArchives: map[string]PingScorePathArchive{
		"allTime.farthestPing": {RecordKey: "allTime.farthestPing", Hash: path.Hash, Timestamp: score.Timestamp, CapturedAt: "2026-01-15T10:05:00Z", Path: path},
	}}
	srv.pingScores.Store(snap)
	return snap
}

func TestPingScorePathArchivePrivacyAndImmutability(t *testing.T) {
	for _, tc := range []struct {
		name, excluded string
		configure      func(*testing.T, *Server, *PingScoresSnapshot)
		firstHidden    bool
	}{
		{"node blacklist case insensitive", "RELAY", func(_ *testing.T, s *Server, _ *PingScoresSnapshot) { s.cfg.SetNodeBlacklist([]string{"relay"}) }, false},
		{"observer blacklist case insensitive", "PINGOBSB", func(_ *testing.T, s *Server, _ *PingScoresSnapshot) { s.cfg.ObserverBlacklist = []string{"pingobsb"} }, false},
		{"saved hidden name", "Saved B", func(_ *testing.T, s *Server, _ *PingScoresSnapshot) { s.cfg.SetHiddenNamePrefixes([]string{"Saved B"}) }, false},
		{"current hidden observer name", "PINGOBSB", func(t *testing.T, s *Server, _ *PingScoresSnapshot) {
			s.cfg.SetHiddenNamePrefixes([]string{"Secret"})
			if _, err := s.db.conn.Exec(`UPDATE observers SET name = 'Secret now' WHERE id = 'pingobsb'`); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"current hidden first node", "pingobsa", func(t *testing.T, s *Server, _ *PingScoresSnapshot) {
			s.cfg.SetHiddenNamePrefixes([]string{"Secret"})
			if _, err := s.db.conn.Exec(`UPDATE nodes SET name = 'Secret now' WHERE public_key = 'pingobsa'`); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"inactive hidden first node", "pingobsa", func(t *testing.T, s *Server, _ *PingScoresSnapshot) {
			s.cfg.SetHiddenNamePrefixes([]string{"Secret"})
			if _, err := s.db.conn.Exec(`CREATE TABLE IF NOT EXISTS inactive_nodes (public_key TEXT PRIMARY KEY, name TEXT); INSERT INTO inactive_nodes(public_key,name) VALUES('pingobsa','Secret retired'); DELETE FROM nodes WHERE public_key='pingobsa'`); err != nil {
				t.Fatal(err)
			}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, router := setupPingScoresFixture(t)
			snap := testPingScorePathArchive(t, srv)
			tc.configure(t, srv, snap)
			before := mustJSON(t, snap.pathArchives)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, httptest.NewRequest("GET", "/api/ping-scores/archive01/path?record=allTime.farthestPing", nil))
			if rr.Code != 200 {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			var response PingScorePathResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Status != "archived" || response.CapturedAt != "2026-01-15T10:05:00Z" || response.Path == nil || len(response.Path.Branches) != 1 {
				t.Fatalf("unexpected response: %s", rr.Body.String())
			}
			if strings.Contains(rr.Body.String(), tc.excluded) || strings.Contains(rr.Body.String(), "Obsolete area") {
				t.Fatalf("hidden/stale evidence leaked: %s", rr.Body.String())
			}
			if response.Path.EstimatedAirtimeMs != nil || response.Path.AirtimeRelayCount != 0 {
				t.Fatal("hidden relay aggregate leaked")
			}
			if tc.firstHidden && (response.Path.First != nil || response.Path.Branches[0].DistanceFromFirstKm != nil || response.Path.Branches[0].SecondsAfterFirst != nil) {
				t.Fatal("hidden origin leaked through relative metrics")
			}
			if got := mustJSON(t, snap.pathArchives); got != before {
				t.Fatal("request changed shared path archive")
			}
		})
	}
}

func TestPingScorePathVisibilityLookupFailsClosed(t *testing.T) {
	srv, router := setupPingScoresFixture(t)
	testPingScorePathArchive(t, srv)
	srv.cfg.SetHiddenNamePrefixes([]string{"Secret"})
	if _, err := srv.db.conn.Exec(`DROP TABLE observers`); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest("GET", "/api/ping-scores/archive01/path?record=allTime.farthestPing", nil))
	if rr.Code != 500 || strings.Contains(rr.Body.String(), "Saved") {
		t.Fatalf("status=%d body=%s, want closed failure", rr.Code, rr.Body.String())
	}
}

func TestPingScorePathRejectsOtherSlotsAndStaleLinks(t *testing.T) {
	srv, router := setupPingScoresFixture(t)
	testPingScorePathArchive(t, srv)
	for _, tc := range []struct {
		path string
		want int
	}{
		{"archive01/path", 400},
		{"archive01/path?record=anything.farthestPing", 400},
		{"archive01/path?record=allTime.mostHopsPing", 404},
		{"older/path?record=allTime.farthestPing", 404},
	} {
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, httptest.NewRequest("GET", "/api/ping-scores/"+tc.path, nil))
		if rr.Code != tc.want || strings.Contains(rr.Body.String(), "Saved") {
			t.Fatalf("%s: status=%d body=%s", tc.path, rr.Code, rr.Body.String())
		}
	}
}

func TestPingScorePathApproximatePrivateContributorsNotExposed(t *testing.T) {
	srv, router := setupPingScoresFixture(t)
	snap := testPingScorePathArchive(t, srv)
	srv.cfg.SetHiddenNamePrefixes([]string{"Secret"})
	a := snap.pathArchives["allTime.farthestPing"]
	for i := range a.Path.Branches {
		a.Path.Branches[i].Observer.Approx = true
		for j := range a.Path.Branches[i].Points {
			a.Path.Branches[i].Points[j].Approx = true
		}
	}
	a.Path.First.Observer.Approx = true
	snap.pathArchives["allTime.farthestPing"] = a
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest("GET", "/api/ping-scores/archive01/path?record=allTime.farthestPing", nil))
	var response PingScorePathResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if rr.Code != 200 || response.Status != "unavailable" || response.Reason != "privacy_filtered" || response.Path != nil {
		t.Fatalf("private approximation leaked: %s", rr.Body.String())
	}
}

func TestPingScorePathRecordedGeometrySurvivesGPSLossAndRealRestart(t *testing.T) {
	for _, mode := range []string{"GPS loss", "packet retention"} {
		t.Run(mode, func(t *testing.T) {
			fx, id, before := seedRecordArchiveFixture(t)
			if mode == "GPS loss" {
				if _, err := fx.srv.db.conn.Exec(`DELETE FROM nodes WHERE public_key='pingobsb'`); err != nil {
					t.Fatal(err)
				}
				fx.clock.Advance(2 * time.Minute)
			} else {
				if _, err := fx.srv.db.conn.Exec(`DELETE FROM observations WHERE transmission_id=?; DELETE FROM transmissions WHERE id=?`, id, id); err != nil {
					t.Fatal(err)
				}
				fx.clock.Advance(40 * 24 * time.Hour)
			}
			if _, err := fx.engine.Cycle(); err != nil {
				t.Fatal(err)
			}
			var seq int
			var name, file string
			if err := fx.store.conn.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &file); err != nil {
				t.Fatal(err)
			}
			if err := fx.store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err := OpenPingScoreHistoryStore(file)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			engine, err := newPingScoreHistoryEngine(fx.srv, store, fx.clock.Now, fx.config)
			if err != nil {
				t.Fatal(err)
			}
			snap, err := engine.QuickSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			fx.srv.pingScores.Store(snap)
			router := mux.NewRouter()
			fx.srv.RegisterRoutes(router)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, httptest.NewRequest("GET", "/api/ping-scores/"+before.FarthestPing.Hash+"/path?record=allTime.farthestPing", nil))
			var response PingScorePathResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if rr.Code != 200 || response.Status != "archived" || response.Path == nil {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			max := 0.0
			for _, b := range response.Path.Branches {
				if b.DistanceFromFirstKm != nil && *b.DistanceFromFirstKm > max {
					max = *b.DistanceFromFirstKm
				}
			}
			if max != *before.FarthestPing.FarthestKm || *snap.FarthestPing.FarthestKm != max {
				t.Fatalf("record/map mismatch after restart: map=%v card=%v old=%v", max, *snap.FarthestPing.FarthestKm, *before.FarthestPing.FarthestKm)
			}
		})
	}
}

func TestPingScorePathNoArchiveCannotShowSmallerLiveGeometry(t *testing.T) {
	fx, _, _ := seedRecordArchiveFixture(t)
	// Simulate rollout onto existing score history after old GPS is gone:
	// there was no route archive at the time of the original measurement.
	fx.engine.pathArchives = nil
	if _, err := fx.store.conn.Exec(`DELETE FROM ping_score_path_archives`); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.srv.db.conn.Exec(`DELETE FROM nodes WHERE public_key='pingobsb'`); err != nil {
		t.Fatal(err)
	}
	fx.clock.Advance(2 * time.Minute)
	snap, err := fx.engine.Cycle()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := snap.pathArchives["allTime.farthestPing"]; exists {
		t.Fatal("incoherent archive unexpectedly created")
	}
	fx.srv.pingScores.Store(snap)
	router := mux.NewRouter()
	fx.srv.RegisterRoutes(router)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest("GET", "/api/ping-scores/"+snap.FarthestPing.Hash+"/path?record=allTime.farthestPing", nil))
	var response PingScorePathResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if rr.Code != 200 || response.Status != "unavailable" || response.Reason != "record_evidence_unavailable" || response.Path != nil {
		t.Fatalf("smaller map misrepresents retained record: %s", rr.Body.String())
	}
}
