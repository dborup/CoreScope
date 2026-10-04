package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// PR #190 review, finding 5: tests that kill the mutants the review found
// surviving the whole ingestor suite.

// PR #190 review, finding 5 (mutant m9): the backward walk breaks on a hop it
// cannot resolve. Hop 1 is ambiguous (both b2 nodes neighbour c3a); hop 0
// must not be anchored on c3a, two hops away, even though exactly one a1
// node neighbours it.
func TestObserverAnchor_BackwardWalkBreaksOnNilHop_188(t *testing.T) {
	idx := idx188(allNodes188...)
	g := graph188([2]string{obs188, c3a}, [2]string{c3a, b2a}, [2]string{c3a, b2b}, [2]string{c3a, a1a})
	got := resolveObservationPath([]string{"a1", "b2", "c3"}, "", obs188, routeFlood188, g, idx)
	wantPath188(t, got, "", "", c3a)
}

// PR #190 review, finding 5 (mutant m10, reported as practically
// equivalent): the backward walk excludes the nodes it has already resolved.
// Path c3b -> b2a -> c3a -> observer. Hop 0 has the prefix "c3" again; both
// c3 nodes neighbour b2a, but c3a is already hop 2, so c3b is the one
// candidate left. Without the exclusion hop 0 stays nil.
func TestObserverAnchor_BackwardWalkExcludesResolvedHops_188(t *testing.T) {
	idx := idx188(allNodes188...)
	g := graph188([2]string{obs188, c3a}, [2]string{c3a, b2a}, [2]string{b2a, c3b})
	got := resolveObservationPath([]string{"c3", "b2", "c3"}, "", obs188, routeFlood188, g, idx)
	wantPath188(t, got, c3b, b2a, c3a)
}

// Mutant m12: the backfill passes transmissions.from_pubkey as the forward
// anchor only for ADVERTs, as InsertTransmission does (buildPacketData sets
// FromPubkey only for ADVERTs). Two rows with the same path ["a1"] and the
// same from_pubkey, whose neighbour edge would pick a1a: the ADVERT resolves,
// the GRP_TXT row stays NULL. The observer has no a1 neighbour, so it
// anchors nothing.
func TestResolvedPathBackfill_FromPubkeyOnlyForAdverts_188(t *testing.T) {
	store := backfillFixture188(t, filepath.Join(t.TempDir(), "ingest.db"), true)
	defer store.Close()
	for _, pk := range []string{a1a, a1b} {
		if _, err := store.db.Exec(`INSERT INTO nodes (public_key, name, role) VALUES (?, ?, 'repeater')`, pk, pk[:4]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec(`INSERT INTO neighbor_edges (node_a, node_b, count, last_seen) VALUES (?, ?, 5, '2026-06-01T00:00:00Z')`, a1a, from188); err != nil {
		t.Fatal(err)
	}
	var obsIdx int64
	if err := store.db.QueryRow(`SELECT rowid FROM observers WHERE id = ?`, strings.ToUpper(obs188)).Scan(&obsIdx); err != nil {
		t.Fatal(err)
	}
	ids := map[int]int64{}
	for _, payloadType := range []int{int(payloadADVERT), 5} {
		res, err := store.db.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey) VALUES ('00', ?, '2026-06-01T00:00:00Z', 1, ?, '{}', ?)`,
			fmt.Sprintf("h188-from-%d", payloadType), payloadType, from188)
		if err != nil {
			t.Fatal(err)
		}
		txID, _ := res.LastInsertId()
		res, err = store.db.Exec(`INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp) VALUES (?, ?, '["a1"]', 1780272000)`, txID, obsIdx)
		if err != nil {
			t.Fatal(err)
		}
		ids[payloadType], _ = res.LastInsertId()
	}
	primeIndexAndGraph188(t, store)
	if _, err := store.RunResolvedPathBackfill(context.Background(), 10, 0); err != nil {
		t.Fatal(err)
	}
	advert := resolvedPathOf188(t, store, ids[int(payloadADVERT)])
	if !advert.Valid {
		t.Fatal("setup: the ADVERT row was not resolved from its from_pubkey")
	}
	wantPath188(t, unmarshalResolvedPathLocal(advert.String), a1a)
	if rp := resolvedPathOf188(t, store, ids[5]); rp.Valid {
		t.Fatalf("the GRP_TXT row was resolved from from_pubkey: %s", rp.String)
	}
}
