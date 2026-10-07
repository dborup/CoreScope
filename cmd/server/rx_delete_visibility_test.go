package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestClientRxDeleteRemovesContributorFromServerViews is the server-side half
// of #330: once the ingestor has erased one contributor's client_receptions
// and client_observers rows, the read-only server's leaderboard and the
// per-observer `?rx=` coverage must no longer surface that contributor, while
// a second contributor is untouched. The server performs no writes — the
// delete is modelled here exactly as the ingestor leaves the tables.
func TestClientRxDeleteRemovesContributorFromServerViews(t *testing.T) {
	db := seedCoverageDB(t)
	recent := time.Now().UTC().Format(time.RFC3339)

	// Two contributors, each heard several distinct nodes at a point in the bbox.
	const victim = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa11"
	const keep = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb22"
	for i := 0; i < 3; i++ {
		mustExecDB(t, db, fmt.Sprintf(
			`INSERT INTO client_receptions (rx_pubkey,heard_key,heard_keylen,snr,lat,lon,rx_at,ingested_at,src)
			 VALUES ('%s','aabb%02d',3,-6,51.05,3.72,'%s','t','rxlog')`, victim, i, recent))
	}
	mustExecDB(t, db, fmt.Sprintf(
		`INSERT INTO client_receptions (rx_pubkey,heard_key,heard_keylen,snr,lat,lon,rx_at,ingested_at,src)
		 VALUES ('%s','ccdd01',3,-6,51.05,3.72,'%s','t','rxlog')`, keep, recent))
	mustExecDB(t, db, fmt.Sprintf(`INSERT INTO client_observers (pubkey,name,last_seen) VALUES ('%s','Victim','%s')`, victim, recent))
	mustExecDB(t, db, fmt.Sprintf(`INSERT INTO client_observers (pubkey,name,last_seen) VALUES ('%s','Keeper','%s')`, keep, recent))

	srv := &Server{db: db, cfg: &Config{ClientRxCoverage: &ClientRxCoverageConfig{Enabled: true}}}
	bb := bbox{MinLat: 50, MinLon: 3, MaxLat: 52, MaxLon: 4}

	// Precondition: both contributors are visible.
	lb, err := srv.rxLeaderboard(context.Background(), 7, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !leaderboardHas(lb, victim) || !leaderboardHas(lb, keep) {
		t.Fatalf("precondition: both contributors must appear, got %+v", lb)
	}
	if rows, _ := srv.queryCoverageFiltered("", victim, 7, bb); len(rows) != 3 {
		t.Fatalf("precondition: victim coverage want 3, got %d", len(rows))
	}

	// Apply the delete exactly as the ingestor does (= match, both tables).
	mustExecDB(t, db, fmt.Sprintf(`DELETE FROM client_receptions WHERE rx_pubkey='%s'`, victim))
	mustExecDB(t, db, fmt.Sprintf(`DELETE FROM client_observers WHERE pubkey='%s'`, victim))

	// Postcondition: the victim is gone from both views; the keeper remains.
	lb, err = srv.rxLeaderboard(context.Background(), 7, 10)
	if err != nil {
		t.Fatal(err)
	}
	if leaderboardHas(lb, victim) {
		t.Fatalf("victim must be gone from leaderboard, got %+v", lb)
	}
	if !leaderboardHas(lb, keep) {
		t.Fatalf("keeper must remain in leaderboard, got %+v", lb)
	}
	if rows, _ := srv.queryCoverageFiltered("", victim, 7, bb); len(rows) != 0 {
		t.Fatalf("victim ?rx= coverage must be empty, got %d", len(rows))
	}
	if rows, _ := srv.queryCoverageFiltered("", keep, 7, bb); len(rows) != 1 {
		t.Fatalf("keeper ?rx= coverage must remain, got %d", len(rows))
	}
}

func leaderboardHas(obs []LeaderObserver, pubkey string) bool {
	for _, o := range obs {
		if o.Pubkey == pubkey {
			return true
		}
	}
	return false
}
