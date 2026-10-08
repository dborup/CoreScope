package main

// PR #176 review follow-ups for the region membership cache:
//
//   - the cache is bounded by evicting one entry, not by clearing the map,
//     and a caller-supplied region set cannot grow a stored key without
//     limit (AGENTS.md rule 0: no unbounded data structures);
//   - the freshness contract the cache documents is the freshness it
//     delivers: additions within nodeRegionFreshTTL, removals and observer
//     IATA changes within nodeRegionRebuildInterval, and nothing older than
//     nodeRegionMaxStale served at all;
//   - a cached region set returns exactly the nodes the uncached subquery
//     returns, for several region sets.

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// ageNodeRegionEntry rewrites the cached entry for key with its refresh and
// build times pushed back, so a test can stand where a 30-second-old or
// 30-minute-old entry stands without waiting. The entry is immutable once
// stored, so this stores a copy.
func ageNodeRegionEntry(t *testing.T, db *DB, key string, refreshedAgo, builtAgo time.Duration) *nodeRegionEntry {
	t.Helper()
	prev := db.getNodeRegionEntry(key)
	if prev == nil {
		t.Fatalf("no cache entry for %q to age", key)
	}
	aged := *prev
	aged.refreshed = time.Now().Add(-refreshedAgo)
	if builtAgo > 0 {
		aged.built = time.Now().Add(-builtAgo)
	}
	db.setNodeRegionEntry(key, &aged)
	return &aged
}

// waitForNodeRegionRefresh waits for the background refresh kicked by a
// stale read to replace the entry, and fails if it never does.
func waitForNodeRegionRefresh(t *testing.T, db *DB, key string, was time.Time) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e := db.getNodeRegionEntry(key); e != nil && e.refreshed.After(was) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no background refresh of %q within 5s of a stale read", key)
}

func nodeRegionPublicKeys(t *testing.T, db *DB, region string) []string {
	t.Helper()
	nodes, total, _, err := db.GetNodes(10000, 0, "", "", "", "", "", region)
	if err != nil {
		t.Fatalf("GetNodes(region=%q): %v", region, err)
	}
	if total != len(nodes) {
		t.Fatalf("GetNodes(region=%q): total=%d but %d rows", region, total, len(nodes))
	}
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		pk, _ := n["public_key"].(string)
		out = append(out, pk)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- bounded

// Filling the cache past nodeRegionMaxEntries must evict exactly one entry —
// the least recently used one — and leave every other entry in place.
//
// The bound used to be enforced by replacing the whole map, so the 33rd
// distinct region set dropped the 32 before it. ?region= is a query
// parameter: a client walking 33 region sets would have emptied the cache on
// every request, and every request after it would have paid a full
// observation scan (~10s on a 1.4GB database), serialised behind
// nodeRegionFullMu. That is the exact collapse this cache exists to prevent,
// reachable from the outside.
func TestNodeRegionCacheEvictsOneLRUEntryAtCapacity_176(t *testing.T) {
	db := &DB{}
	for i := range nodeRegionMaxEntries {
		key := fmt.Sprintf("R%02d", i)
		db.setNodeRegionEntry(key, &nodeRegionEntry{keysJSON: `["` + key + `"]`})
	}
	// R00 is the least recently *written*; read it so R01 becomes the victim.
	// That also locks the LRU order against a plain insertion-order policy.
	if got := db.getNodeRegionEntry("R00"); got == nil {
		t.Fatal("R00 missing before eviction")
	}

	db.setNodeRegionEntry("NEW", &nodeRegionEntry{keysJSON: `["NEW"]`})

	if got := len(db.nodeRegionCache); got != nodeRegionMaxEntries {
		t.Fatalf("cache holds %d entries, want the bound %d", got, nodeRegionMaxEntries)
	}
	if got := db.getNodeRegionEntry("R01"); got != nil {
		t.Errorf("least recently used entry R01 survived: %#v", got)
	}
	var lost []string
	for i := range nodeRegionMaxEntries {
		key := fmt.Sprintf("R%02d", i)
		if key == "R01" {
			continue
		}
		if e := db.nodeRegionCache[key]; e == nil || e.keysJSON != `["`+key+`"]` {
			lost = append(lost, key)
		}
	}
	if len(lost) > 0 {
		t.Errorf("eviction of one entry also dropped %d others: %v", len(lost), lost)
	}
	if e := db.getNodeRegionEntry("NEW"); e == nil || e.keysJSON != `["NEW"]` {
		t.Errorf("new entry not stored: %#v", e)
	}
	if got, want := len(db.nodeRegionUsed), len(db.nodeRegionCache); got != want {
		t.Errorf("LRU bookkeeping holds %d keys for %d entries", got, want)
	}
}

// Repeated eviction keeps the bound: 4x the capacity in distinct region sets
// never grows the cache, and the most recent ones are the ones kept.
func TestNodeRegionCacheStaysBoundedUnderChurn_176(t *testing.T) {
	db := &DB{}
	const churn = 4 * nodeRegionMaxEntries
	for i := range churn {
		key := fmt.Sprintf("Q%03d", i)
		db.setNodeRegionEntry(key, &nodeRegionEntry{keysJSON: `["` + key + `"]`})
		if got := len(db.nodeRegionCache); got > nodeRegionMaxEntries {
			t.Fatalf("after %d inserts the cache holds %d entries, bound is %d",
				i+1, got, nodeRegionMaxEntries)
		}
	}
	if got := len(db.nodeRegionCache); got != nodeRegionMaxEntries {
		t.Fatalf("cache holds %d entries after churn, want %d", got, nodeRegionMaxEntries)
	}
	for i := churn - nodeRegionMaxEntries; i < churn; i++ {
		key := fmt.Sprintf("Q%03d", i)
		if db.nodeRegionCache[key] == nil {
			t.Errorf("most recent entry %s was evicted", key)
		}
	}
}

// normalizeRegionCodes does not limit how many codes ?region= may carry, so
// the stored key must be bounded on its own. A long set is keyed by its
// digest; distinct long sets still get distinct keys, and the same long set
// still shares one entry.
func TestNodeRegionCacheLongRegionSetKeyIsBounded_176(t *testing.T) {
	long := make([]string, 0, 2000)
	for i := range 2000 {
		long = append(long, fmt.Sprintf("X%04d", i))
	}
	key := nodeRegionKey(long)
	if len(key) > nodeRegionMaxKeyBytes {
		t.Fatalf("key for %d codes is %d bytes, bound is %d", len(long), len(key), nodeRegionMaxKeyBytes)
	}
	if !strings.HasPrefix(key, "sha256:") {
		t.Fatalf("long key %q is not a digest", key)
	}
	// Order and duplicates still collapse to one identity.
	shuffled := slices.Clone(long)
	slices.Reverse(shuffled)
	shuffled = append(shuffled, long[0], long[1])
	if got := nodeRegionKey(shuffled); got != key {
		t.Errorf("same region set keyed twice: %q vs %q", got, key)
	}
	other := nodeRegionKey(append(slices.Clone(long), "ZZZZ"))
	if other == key {
		t.Error("two different long region sets share one cache key")
	}
	// A short set is still stored verbatim, so the log line and the tests
	// above keep reading region codes.
	if got := nodeRegionKey([]string{"SJC", "SFO"}); got != "SFO,SJC" {
		t.Errorf("short key = %q, want SFO,SJC", got)
	}
}

// ---------------------------------------------------------------- freshness

// seedRegionFreshnessDB returns a v3 test DB with one observer in SJC, one
// in SFO, and one node heard in SJC.
func seedRegionFreshnessDB(t *testing.T) *DB {
	t.Helper()
	db := setupTestDB(t)
	t.Cleanup(func() { db.Close() })
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.conn.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO observers (rowid, id, name, iata) VALUES (1,'obs-sjc','SJC obs','SJC'), (2,'obs-sfo','SFO obs','SFO')`)
	regionSeedNode(t, db, "aa00000000000001", 1)
	return db
}

// regionSeedNode inserts a node, a stamped ADVERT for it and one observation
// by the given observer rowid, and returns the node's public key.
func regionSeedNode(t *testing.T, db *DB, pubkey string, observerIdx int) string {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.conn.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO nodes (public_key, name, role, last_seen, first_seen)
		VALUES (?, ?, 'repeater', ?, ?)`, pubkey, "n-"+pubkey[:4], now, now)
	mustExec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey)
		VALUES ('00', ?, ?, 1, 4, ?, ?)`, "adv-"+pubkey, now,
		`{"type":"ADVERT","pubKey":"`+pubkey+`"}`, pubkey)
	mustExec(`INSERT INTO observations (transmission_id, observer_idx, timestamp)
		VALUES ((SELECT id FROM transmissions WHERE hash=?), ?, ?)`, "adv-"+pubkey, observerIdx, time.Now().Unix())
	return pubkey
}

// A node first heard after the entry was built appears within
// nodeRegionFreshTTL: the read at the TTL boundary is still answered from the
// cache (that is the contract), it kicks exactly one refresh, and the next
// read has the new node. Nothing waits longer than the promised 30 seconds
// plus one delta scan.
func TestNodeRegionCacheAdditionVisibleWithinFreshTTL_176(t *testing.T) {
	db := seedRegionFreshnessDB(t)
	if got := nodeRegionPublicKeys(t, db, "SJC"); !slices.Equal(got, []string{"aa00000000000001"}) {
		t.Fatalf("cold SJC = %v", got)
	}

	newKey := regionSeedNode(t, db, "bb00000000000002", 1)

	// Inside the TTL the cached set is served unchanged — the cache is real.
	if got := nodeRegionPublicKeys(t, db, "SJC"); slices.Contains(got, newKey) {
		t.Fatalf("fresh entry already reflects a node added after it was built: %v", got)
	}

	aged := ageNodeRegionEntry(t, db, "SJC", nodeRegionFreshTTL, 0)
	if got := nodeRegionPublicKeys(t, db, "SJC"); slices.Contains(got, newKey) {
		t.Fatalf("read at the TTL boundary blocked on a refresh instead of serving stale: %v", got)
	}
	waitForNodeRegionRefresh(t, db, "SJC", aged.refreshed)

	got := nodeRegionPublicKeys(t, db, "SJC")
	if !slices.Equal(got, []string{"aa00000000000001", newKey}) {
		t.Fatalf("node added %v after the entry was built is still missing: %v",
			nodeRegionFreshTTL, got)
	}
	if e := db.getNodeRegionEntry("SJC"); !e.fresh() {
		t.Error("entry not fresh after its refresh landed")
	}
}

// A node removed from a region — its observations pruned by retention, or its
// observer's IATA changed — is gone within nodeRegionRebuildInterval and not
// before: the delta scan cannot see a removal, so the documented 30 minutes
// is what governs. Both removal shapes are checked.
func TestNodeRegionCacheRemovalGoneWithinRebuildInterval_176(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove func(t *testing.T, db *DB)
	}{
		{"retention pruned the observations", func(t *testing.T, db *DB) {
			if _, err := db.conn.Exec(`DELETE FROM observations WHERE observer_idx = 1`); err != nil {
				t.Fatal(err)
			}
		}},
		{"observer moved to another region", func(t *testing.T, db *DB) {
			if _, err := db.conn.Exec(`UPDATE observers SET iata='LAX' WHERE rowid=1`); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := seedRegionFreshnessDB(t)
			if got := nodeRegionPublicKeys(t, db, "SJC"); len(got) != 1 {
				t.Fatalf("cold SJC = %v", got)
			}
			tc.remove(t, db)

			// Stale, but not yet due a rebuild: the delta refresh runs and
			// must not drop the node. This is the documented behaviour, and
			// it is what makes the rebuild interval the real bound.
			aged := ageNodeRegionEntry(t, db, "SJC", nodeRegionFreshTTL, nodeRegionRebuildInterval/2)
			nodeRegionPublicKeys(t, db, "SJC")
			waitForNodeRegionRefresh(t, db, "SJC", aged.refreshed)
			if got := nodeRegionPublicKeys(t, db, "SJC"); len(got) != 1 {
				t.Fatalf("delta refresh changed membership: %v", got)
			}

			// At the rebuild interval the refresh is a full rebuild.
			aged = ageNodeRegionEntry(t, db, "SJC", nodeRegionFreshTTL, nodeRegionRebuildInterval)
			nodeRegionPublicKeys(t, db, "SJC")
			waitForNodeRegionRefresh(t, db, "SJC", aged.refreshed)
			if got := nodeRegionPublicKeys(t, db, "SJC"); len(got) != 0 {
				t.Fatalf("node still in the region %v after it was removed: %v",
					nodeRegionRebuildInterval, got)
			}
		})
	}
}

// Past nodeRegionMaxStale an entry is not served blind: the caller waits for
// the refresh, so one call — not two — returns membership inside the
// documented age. Without the wait, membership of unbounded age is served
// whenever refreshes have been failing, with only a log line to show it.
func TestNodeRegionCacheDoesNotServeBeyondMaxStale_176(t *testing.T) {
	db := seedRegionFreshnessDB(t)
	if got := nodeRegionPublicKeys(t, db, "SJC"); len(got) != 1 {
		t.Fatalf("cold SJC = %v", got)
	}
	newKey := regionSeedNode(t, db, "bb00000000000002", 1)
	ageNodeRegionEntry(t, db, "SJC", nodeRegionMaxStale, nodeRegionMaxStale)

	got := nodeRegionPublicKeys(t, db, "SJC")
	if !slices.Contains(got, newKey) {
		t.Fatalf("entry %v old was served without waiting for its refresh: %v",
			nodeRegionMaxStale, got)
	}
}

// ...and a refresh that fails does not turn an over-stale entry into a 500:
// the stale set is served, the failure is logged, and the scan was attempted
// before the call returned.
func TestNodeRegionCacheOverStaleFallsBackWhenRefreshFails_176(t *testing.T) {
	db := seedRegionFreshnessDB(t)
	want := nodeRegionPublicKeys(t, db, "SJC")
	if len(want) != 1 {
		t.Fatalf("cold SJC = %v", want)
	}
	regionSeedNode(t, db, "bb00000000000002", 1)
	ageNodeRegionEntry(t, db, "SJC", nodeRegionMaxStale, nodeRegionMaxStale)

	scans := 0
	db.nodeRegionQueryHook = func() error { scans++; return fmt.Errorf("observation scan unavailable") }
	got := nodeRegionPublicKeys(t, db, "SJC")
	if scans != 1 {
		t.Errorf("over-stale read attempted %d scans before returning, want 1", scans)
	}
	if !slices.Equal(got, want) {
		t.Errorf("failed refresh of an over-stale entry returned %v, want the stale set %v", got, want)
	}
}

// ---------------------------------------------------------------- oracle

// regionNodesUncached is the oracle: the node set the region filter returned
// before this cache existed, built from the uncached subquery (#38's form) so
// the cache is compared to something it cannot be derived from.
func regionNodesUncached(t *testing.T, db *DB, region string) []string {
	t.Helper()
	codes := normalizeRegionCodes(region)
	if len(codes) == 0 {
		t.Fatalf("region %q normalizes to no codes", region)
	}
	joinCond := "obs.rowid = o.observer_idx"
	if !db.isV3() {
		joinCond = "obs.id = o.observer_id"
	}
	args := make([]any, len(codes))
	for i, c := range codes {
		args[i] = c
	}
	q := `SELECT public_key FROM nodes WHERE public_key IN (
		SELECT DISTINCT t.from_pubkey
		FROM transmissions t
		JOIN observations o ON o.transmission_id = t.id
		JOIN observers obs ON ` + joinCond + `
		WHERE t.payload_type = 4
		AND UPPER(TRIM(obs.iata)) IN (?` + strings.Repeat(",?", len(codes)-1) + `)
	)`
	rows, err := db.conn.Query(q, args...)
	if err != nil {
		t.Fatalf("oracle query for %q: %v", region, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var pk string
		if err := rows.Scan(&pk); err != nil {
			t.Fatal(err)
		}
		out = append(out, pk)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// The cached region filter returns exactly the uncached node set, for single
// regions, multi-region sets, a set written in mixed case with padding and
// duplicates, and a region no observer carries — before and after new
// ADVERTs arrive. Every set is queried twice, so the second answer comes
// from the cache rather than a fresh scan.
func TestNodeRegionCacheMatchesUncachedForManyRegionSets_176(t *testing.T) {
	db := seedRegionFreshnessDB(t)
	regionSeedNode(t, db, "bb00000000000002", 2)
	regionSeedNode(t, db, "cc00000000000003", 1)
	// cc is heard in both regions.
	if _, err := db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, timestamp)
		VALUES ((SELECT id FROM transmissions WHERE hash='adv-cc00000000000003'), 2, ?)`, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	sets := []string{"SJC", "SFO", "SJC,SFO", " sfo , SJC , sjc ", "LAX", "LAX,SJC"}
	check := func(stage string) {
		t.Helper()
		nonEmpty := 0
		for _, region := range sets {
			want := regionNodesUncached(t, db, region)
			for pass := range 2 {
				got := nodeRegionPublicKeys(t, db, region)
				if !slices.Equal(got, want) {
					t.Errorf("%s: region %q pass %d: cached %v, uncached %v",
						stage, region, pass, got, want)
				}
			}
			if len(want) > 0 {
				nonEmpty++
			}
		}
		if nonEmpty < 3 {
			t.Fatalf("%s: only %d region sets matched any node; the comparison is vacuous",
				stage, nonEmpty)
		}
	}
	check("initial")

	// New ADVERTs land, every entry is taken past its TTL, and each set's
	// refresh is awaited before it is compared again.
	regionSeedNode(t, db, "dd00000000000004", 2)
	for _, region := range sets {
		key := nodeRegionKey(normalizeRegionCodes(region))
		aged := ageNodeRegionEntry(t, db, key, 2*nodeRegionFreshTTL, 0)
		nodeRegionPublicKeys(t, db, region)
		waitForNodeRegionRefresh(t, db, key, aged.refreshed)
	}
	check("after new adverts")
}
