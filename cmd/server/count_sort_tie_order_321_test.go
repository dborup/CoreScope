package main

import (
	"fmt"
	"sort"
	"testing"
	"time"
)

// Issue #321: four more count-only sorts (plus the hash-collision list) are
// built by ranging over a map and were sorted on the count alone. Go map
// iteration order is random, so entries with equal counts came out in a
// different order on each call, and where the list is capped (peerInteractions
// at 20) it was random *which* tied entries made the cut. Same bug class and
// same fix as rankSubpaths (#256/#293) and GetSubpathDetail (#319): give every
// comparator a final, unique tie-break key.
//
// Each test recomputes its list tieOrderRuns times (see
// analytics_tie_order_256_test.go) and requires the same, fully specified
// order every time, so the failure before the fix is deterministic rather
// than flaky.

// scrambledKeys returns n keys with the given prefix whose insertion order is
// deliberately not their sorted order, so a comparator that leaves ties to map
// iteration cannot accidentally produce the expected sequence.
func scrambledKeys(prefix string, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("%s-%04x", prefix, (i*37)%256+0x1000))
	}
	return out
}

// newDisplayArrayAccumulator builds an accumulator in display-array mode with
// the observer and peer detail maps the two node-analytics lists are ranked
// from. finalizeDisplayArrays only reads those maps for these two lists.
func newDisplayArrayAccumulator(obs map[string]int, peers map[string]int) *nodeAnalyticsAccumulator {
	a := newNodeAnalyticsAccumulator("node-321", true)
	for id, n := range obs {
		a.obsDetail[id] = &nodeAnalyticsObsAccum{
			name:  "Name-" + id,
			count: n,
			first: "2026-05-01T12:00:00Z",
			last:  "2026-05-01T13:00:00Z",
		}
	}
	for key, n := range peers {
		a.peerDetail[key] = &nodeAnalyticsPeerAccum{
			key: key, name: "Peer-" + key, count: n,
			lastContact: "2026-05-01T12:00:00Z",
		}
	}
	return a
}

func TestNodeAnalyticsObserverCoverageTieOrder_321(t *testing.T) {
	// observerCoverage has no cap, so only the order was random. 12 observers
	// tie on 4 packets, one is seen 9 times and one twice.
	tied := scrambledKeys("obs", 12)
	obs := map[string]int{"obs-high": 9, "obs-low": 2}
	for _, id := range tied {
		obs[id] = 4
	}
	a := newDisplayArrayAccumulator(obs, nil)

	sorted := append([]string(nil), tied...)
	sort.Strings(sorted)
	want := append([]string{"obs-high"}, sorted...)
	want = append(want, "obs-low")
	assertSameEveryRun(t, "observerCoverage observer_id", want, func() interface{} {
		_, _, _, coverage, _, _, _ := a.finalizeDisplayArrays()
		out := []string{}
		for _, e := range coverage {
			out = append(out, e.ObserverID.(string))
		}
		return out
	})
}

func TestNodeAnalyticsPeerInteractionsTieOrder_321(t *testing.T) {
	// peerInteractions is capped at 20, so the tie decided *which* peers the
	// node-analytics page showed, not just their order. 30 peers tie on 3
	// messages and one has 9, so 19 of the 30 tied peers make the cut.
	tied := scrambledKeys("peer", 30)
	peers := map[string]int{"peer-high": 9}
	for _, key := range tied {
		peers[key] = 3
	}
	a := newDisplayArrayAccumulator(nil, peers)

	sorted := append([]string(nil), tied...)
	sort.Strings(sorted)
	want := append([]string{"peer-high"}, sorted[:19]...)
	assertSameEveryRun(t, "peerInteractions peer_key (top 20)", want, func() interface{} {
		_, _, _, _, _, peerInteractions, _ := a.finalizeDisplayArrays()
		out := []string{}
		for _, e := range peerInteractions {
			out = append(out, e.PeerKey)
		}
		return out
	})
	if len(want) != 20 {
		t.Fatalf("expected the cap to bind at 20, want has %d entries", len(want))
	}
}

// newHealthTieStore seeds one node row plus in-memory packets so that each
// observer in counts heard the node exactly that many times.
func newHealthTieStore(t *testing.T, pubkey string, counts map[string]int) *PacketStore {
	t.Helper()
	db := setupTestDBv2(t)
	mustExecDB(t, db, `INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen)
		VALUES ('`+pubkey+`', 'Node 321', 'repeater', 0, 0, '2026-05-01T13:00:00Z', '2026-05-01T12:00:00Z')`)
	s := newTestStoreWithDB(t, db, &Config{})
	snr := 5.0
	n := 0
	for obsID, c := range counts {
		for i := 0; i < c; i++ {
			n++
			s.byNode[pubkey] = append(s.byNode[pubkey], &StoreTx{
				ID: n, Hash: fmt.Sprintf("hl%d", n), FirstSeen: "2026-05-01T12:00:00Z",
				SNR: &snr, ObservationCount: 1,
				ObserverID: obsID, ObserverName: "Name-" + obsID,
			})
		}
	}
	return s
}

// healthTieCounts is the shared shape for both health endpoints: 12 observers
// tie on 4 packets, one is heard 9 times and one twice.
func healthTieCounts() (counts map[string]int, want []string) {
	tied := scrambledKeys("obs", 12)
	counts = map[string]int{"obs-high": 9, "obs-low": 2}
	for _, id := range tied {
		counts[id] = 4
	}
	sorted := append([]string(nil), tied...)
	sort.Strings(sorted)
	want = append([]string{"obs-high"}, sorted...)
	want = append(want, "obs-low")
	return counts, want
}

func TestNodeHealthObserverRowsTieOrder_321(t *testing.T) {
	const pubkey = "aa11bb22cc33dd44"
	counts, want := healthTieCounts()
	s := newHealthTieStore(t, pubkey, counts)

	assertSameEveryRun(t, "GetNodeHealth observers observer_id", want, func() interface{} {
		res, err := s.GetNodeHealth(pubkey)
		if err != nil {
			t.Fatalf("GetNodeHealth: %v", err)
		}
		return mapField(res["observers"], "observer_id")
	})
}

func TestBulkHealthObserverRowsTieOrder_321(t *testing.T) {
	const pubkey = "bb11cc22dd33ee44"
	counts, want := healthTieCounts()
	s := newHealthTieStore(t, pubkey, counts)

	assertSameEveryRun(t, "GetBulkHealth observers observer_id", want, func() interface{} {
		rows := s.GetBulkHealth(10, "", "")
		if len(rows) != 1 {
			t.Fatalf("expected 1 bulk-health row, got %d", len(rows))
		}
		return mapField(rows[0]["observers"], "observer_id")
	})
}

func TestHashCollisionsTieOrder_321(t *testing.T) {
	// Six 2-byte prefixes, each shared by exactly two coordinate-less
	// repeaters. Every entry therefore ties on both existing sort keys —
	// classification ("incomplete") and Appearances (2) — so the order was
	// pure map iteration order.
	db := setupTestDBv2(t)
	prefixes := []string{"1000", "1025", "104a", "106f", "1094", "10b9"}
	var adverts []*StoreTx
	now := time.Now().UTC()
	for _, p := range prefixes {
		for j := 0; j < 2; j++ {
			pk := fmt.Sprintf("%s%02dcafebabe00", p, j)
			mustExecDB(t, db, `INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen)
				VALUES ('`+pk+`', 'R-`+pk+`', 'repeater', 0, 0, '2026-05-01T13:00:00Z', '2026-05-01T12:00:00Z')`)
			// Path byte 0x41 declares 2-byte hashes, so HashSize == 2.
			adv := makeHashSizeAdvert(pk, "R-"+pk, now.Add(-time.Duration(j)*time.Minute).Format(time.RFC3339))
			adverts = append(adverts, adv)
		}
	}
	s := newTestStoreWithDB(t, db, &Config{})
	s.byPayloadType[4] = adverts

	want := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		want = append(want, fmt.Sprintf("%04X", mustParseHex(t, p)))
	}
	sort.Strings(want)
	assertSameEveryRun(t, "hash collisions prefix (2-byte)", want, func() interface{} {
		s.hashSizeInfoCache = nil
		res := s.computeHashCollisions("", "")
		bySize := res["by_size"].(map[string]interface{})
		two := bySize["2"].(map[string]interface{})
		out := []string{}
		for _, c := range two["collisions"].([]collisionEntry) {
			out = append(out, c.Prefix)
		}
		return out
	})
}

func mustParseHex(t *testing.T, s string) int {
	t.Helper()
	var v int
	if _, err := fmt.Sscanf(s, "%x", &v); err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}
