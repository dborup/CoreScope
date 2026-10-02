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
	db.nodeRegionQueryHook = func() { scans.Add(1) }
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

func TestNodeRegionCachePortLegacyBackfillAndRefresh(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	seedTestData(t, db)
	_, _, _, err := db.GetNodes(10, 0, "", "", "", "", "", "SJC")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = db.conn.Exec(`INSERT INTO nodes(public_key,name,role,last_seen,first_seen) VALUES ('legacykey','Legacy','repeater',?,?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.conn.Exec(`INSERT INTO transmissions(raw_hex,hash,first_seen,route_type,payload_type,decoded_json) VALUES ('AA','legacy-hash',?,1,4,'{"pubKey":"legacykey"}')`, now)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an old row not yet processed by the asynchronous backfill.
	_, err = db.conn.Exec(`UPDATE transmissions SET from_pubkey=NULL WHERE hash='legacy-hash'`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.conn.Exec(`INSERT INTO observations(transmission_id,observer_idx,timestamp) VALUES ((SELECT id FROM transmissions WHERE hash='legacy-hash'),1,?)`, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	prev := db.getNodeRegionEntry("SJC")
	aged := *prev
	aged.refreshed = time.Now().Add(-2 * nodeRegionFreshTTL)
	db.setNodeRegionEntry("SJC", &aged)
	if _, err := db.refreshNodeRegion("SJC", []string{"SJC"}); err != nil {
		t.Fatal(err)
	}
	nodes, total, _, err := db.GetNodes(10, 0, "", "", "", "", "", "SJC")
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(nodes) != 2 {
		t.Fatalf("legacy row missing after delta refresh: total=%d nodes=%v", total, nodes)
	}
	if !strings.Contains(db.getNodeRegionEntry("SJC").keysJSON, "legacykey") {
		t.Fatal("legacy key absent from cached set")
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
