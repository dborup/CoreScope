package main

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNodeRegionCacheReplacementAtCapacityKeepsOtherKeys(t *testing.T) {
	db := &DB{}
	for i := range nodeRegionMaxEntries {
		key := fmt.Sprintf("R%02d", i)
		db.setNodeRegionEntry(key, &nodeRegionEntry{keysJSON: `["` + key + `"]`})
	}
	db.setNodeRegionEntry("R00", &nodeRegionEntry{keysJSON: `["updated"]`})
	if got := len(db.nodeRegionCache); got != nodeRegionMaxEntries {
		t.Fatalf("replacing one of %d entries left %d", nodeRegionMaxEntries, got)
	}
	if got := db.getNodeRegionEntry("R01"); got == nil || got.keysJSON != `["R01"]` {
		t.Fatalf("unrelated cache entry lost on replacement: %#v", got)
	}
	if got := db.getNodeRegionEntry("R00").keysJSON; got != `["updated"]` {
		t.Fatalf("replacement missing: %q", got)
	}
}

func TestNodeRegionCachePortMembershipAndReuse(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	seedTestData(t, db)
	var scans atomic.Int32
	db.nodeRegionQueryHook = func() error { scans.Add(1); return nil }
	for _, region := range []string{"SJC", "sjc", "SJC,SJC"} {
		nodes, total, _, err := db.GetNodes(10, 0, "", "", "", "", "", region)
		if err != nil {
			t.Fatal(err)
		}
		if total != 1 || len(nodes) != 1 || nodes[0]["public_key"] != "aabbccdd11223344" {
			t.Fatalf("region %q: total=%d nodes=%v", region, total, nodes)
		}
	}
	if got := scans.Load(); got != 1 {
		t.Fatalf("same region should scan once, got %d", got)
	}
}

// The delta refresh obeys the same from_pubkey contract as the full scan
// (PR #38): an ADVERT the ingestor left with a NULL from_pubkey does not
// place its node in a region, and a row whose decoded_json is not valid JSON
// does not fail the refresh — which, behind this cache, would leave the entry
// stale for every later request too, not just the one that triggered it.
func TestNodeRegionCacheDeltaFollowsFromPubkeyContract_176(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	seedTestData(t, db)
	// The helper installs a trigger that copies decoded_json.pubKey into
	// from_pubkey; drop it so the test writes from_pubkey itself, the way the
	// real write paths do.
	if _, err := db.conn.Exec(`DROP TRIGGER test_from_pubkey_advert`); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := db.GetNodes(10, 0, "", "", "", "", "", "SJC"); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.conn.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, pk := range []string{"nullkey000000001", "stampedkey000002"} {
		mustExec(`INSERT INTO nodes(public_key,name,role,last_seen,first_seen) VALUES (?,?,'repeater',?,?)`,
			pk, "n-"+pk[:4], now, now)
	}
	// An ADVERT with a pubkey in decoded_json the backfill has not stamped,
	// an ADVERT that is stamped, and an ADVERT whose decoded_json is corrupt.
	mustExec(`INSERT INTO transmissions(raw_hex,hash,first_seen,route_type,payload_type,decoded_json,from_pubkey)
		VALUES ('AA','d-null',?,1,4,'{"pubKey":"nullkey000000001"}',NULL)`, now)
	mustExec(`INSERT INTO transmissions(raw_hex,hash,first_seen,route_type,payload_type,decoded_json,from_pubkey)
		VALUES ('BB','d-stamped',?,1,4,'{"pubKey":"stampedkey000002"}','stampedkey000002')`, now)
	mustExec(`INSERT INTO transmissions(raw_hex,hash,first_seen,route_type,payload_type,decoded_json,from_pubkey)
		VALUES ('CC','d-corrupt',?,1,4,'NOT JSON',NULL)`, now)
	for _, hash := range []string{"d-null", "d-stamped", "d-corrupt"} {
		mustExec(`INSERT INTO observations(transmission_id,observer_idx,timestamp)
			VALUES ((SELECT id FROM transmissions WHERE hash=?),1,?)`, hash, time.Now().Unix())
	}

	ageNodeRegionEntry(t, db, "SJC", 2*nodeRegionFreshTTL, 0)
	if _, err := db.refreshNodeRegion("SJC", []string{"SJC"}); err != nil {
		t.Fatalf("delta refresh over a corrupt ADVERT: %v", err)
	}
	keys := db.getNodeRegionEntry("SJC").keysJSON
	if !strings.Contains(keys, "stampedkey000002") {
		t.Errorf("stamped ADVERT missing from cached set: %s", keys)
	}
	if strings.Contains(keys, "nullkey000000001") {
		t.Errorf("NULL from_pubkey ADVERT placed its node in the region: %s", keys)
	}
}

func TestNodeRegionCachePortFullRebuildDropsPrunedRegion(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	seedTestData(t, db)
	if _, _, _, err := db.GetNodes(10, 0, "", "", "", "", "", "SJC"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.Exec(`DELETE FROM observations WHERE observer_idx=1`); err != nil {
		t.Fatal(err)
	}
	prev := db.getNodeRegionEntry("SJC")
	aged := *prev
	aged.refreshed = time.Now().Add(-2 * nodeRegionFreshTTL)
	aged.built = time.Now().Add(-2 * nodeRegionRebuildInterval)
	db.setNodeRegionEntry("SJC", &aged)
	if _, err := db.refreshNodeRegion("SJC", []string{"SJC"}); err != nil {
		t.Fatal(err)
	}
	nodes, total, _, err := db.GetNodes(10, 0, "", "", "", "", "", "SJC")
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 || len(nodes) != 0 {
		t.Fatalf("full rebuild retained pruned membership: total=%d nodes=%v", total, nodes)
	}
}
