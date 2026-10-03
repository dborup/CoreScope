package main

import (
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
)

// Issue #188: the observer anchors the last hop of a FLOOD path, and a
// backward walk resolves earlier hops from it. See resolveObservationPath.

// Pubkeys for 1-byte collisions: each prefix has an "a" and a "b" node.
const (
	obs188  = "0b5e000000000000000000000000000000000000000000000000000000000001"
	a1a     = "a1110000000000000000000000000000000000000000000000000000000000aa"
	a1b     = "a1220000000000000000000000000000000000000000000000000000000000bb"
	b2a     = "b2330000000000000000000000000000000000000000000000000000000000aa"
	b2b     = "b2440000000000000000000000000000000000000000000000000000000000bb"
	c3a     = "c3550000000000000000000000000000000000000000000000000000000000aa"
	c3b     = "c3660000000000000000000000000000000000000000000000000000000000bb"
	from188 = "f0f0000000000000000000000000000000000000000000000000000000000001"
)

const (
	routeTransportFlood188 = 0
	routeFlood188          = 1
	routeDirect188         = 2
)

func idx188(keys ...string) prefixIndex {
	idx := prefixIndex{}
	for _, pk := range keys {
		for _, n := range []int{2, 4, 6, 8, 12, 16} {
			idx[pk[:n]] = append(idx[pk[:n]], pk)
		}
	}
	return idx
}

func graph188(edges ...[2]string) *NeighborGraph {
	g := NewNeighborGraph()
	for _, e := range edges {
		g.AddEdge(e[0], e[1])
	}
	return g
}

func path188(rp []*string) string {
	parts := make([]string, len(rp))
	for i, p := range rp {
		if p == nil {
			parts[i] = "nil"
		} else {
			parts[i] = (*p)[:4]
		}
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func wantPath188(t *testing.T, got []*string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%s)", len(got), len(want), path188(got))
	}
	for i := range want {
		g := ""
		if got[i] != nil {
			g = *got[i]
		}
		if g != want[i] {
			t.Fatalf("hop %d = %q, want %q (path %s)", i, g, want[i], path188(got))
		}
	}
}

var allNodes188 = []string{obs188, a1a, a1b, b2a, b2b, c3a, c3b, from188}

func TestObserverAnchor_LastHopResolvesViaObserver_188(t *testing.T) {
	idx := idx188(allNodes188...)
	g := graph188([2]string{obs188, c3a})
	got := resolveObservationPath([]string{"b2", "c3"}, "", obs188, routeFlood188, g, idx)
	wantPath188(t, got, "", c3a)

	// The observer id arrives upper-case from the MQTT topic.
	got = resolveObservationPath([]string{"C3"}, "", strings.ToUpper(obs188), routeTransportFlood188, g, idx)
	wantPath188(t, got, c3a)
}

func TestObserverAnchor_BackwardWalkResolvesEarlierHops_188(t *testing.T) {
	idx := idx188(allNodes188...)
	g := graph188([2]string{obs188, c3a}, [2]string{c3a, b2a}, [2]string{b2a, a1a},
		// decoys that are adjacent to the wrong anchors
		[2]string{c3b, b2b}, [2]string{b2b, a1b})
	got := resolveObservationPath([]string{"a1", "b2", "c3"}, "", obs188, routeFlood188, g, idx)
	wantPath188(t, got, a1a, b2a, c3a)

	// The walk stops at the first ambiguous hop: hop 0 has no unique
	// neighbour of the resolved hop 1, so it stays nil.
	g2 := graph188([2]string{obs188, c3a}, [2]string{c3a, b2a}, [2]string{b2a, a1a}, [2]string{b2a, a1b})
	got = resolveObservationPath([]string{"a1", "b2", "c3"}, "", obs188, routeFlood188, g2, idx)
	wantPath188(t, got, "", b2a, c3a)
}

func TestObserverAnchor_TwoNeighborCandidatesStayNil_188(t *testing.T) {
	idx := idx188(allNodes188...)
	// Both c3 candidates are neighbours of the observer.
	g := graph188([2]string{obs188, c3a}, [2]string{obs188, c3b}, [2]string{c3a, b2a})
	got := resolveObservationPath([]string{"b2", "c3"}, "", obs188, routeFlood188, g, idx)
	wantPath188(t, got, "", "")

	// Last hop resolves, but both b2 candidates neighbour it.
	g = graph188([2]string{obs188, c3a}, [2]string{c3a, b2a}, [2]string{c3a, b2b})
	got = resolveObservationPath([]string{"b2", "c3"}, "", obs188, routeFlood188, g, idx)
	wantPath188(t, got, "", c3a)
}

// The observer anchor only holds for flood routing: a DIRECT packet's path is
// the remaining planned route (each forwarder strips itself from the front,
// firmware Mesh.cpp removeSelfFromPath), and a TRACE path carries SNR bytes.
func TestObserverAnchor_DirectRouteIsNotAnchored_188(t *testing.T) {
	idx := idx188(allNodes188...)
	g := graph188([2]string{obs188, c3a})
	for _, rt := range []int{routeDirect188, 3} {
		got := resolveObservationPath([]string{"c3"}, "", obs188, rt, g, idx)
		wantPath188(t, got, "")
	}
}

func TestObserverAnchor_UnknownObserverHasNoEffect_188(t *testing.T) {
	idx := idx188(allNodes188...)
	g := graph188([2]string{obs188, c3a})
	for _, o := range []string{"", "companion", "not-a-node"} {
		got := resolveObservationPath([]string{"c3"}, "", o, routeFlood188, g, idx)
		wantPath188(t, got, "")
	}
	// No graph: nothing to anchor on.
	got := resolveObservationPath([]string{"c3"}, "", obs188, routeFlood188, nil, idx)
	wantPath188(t, got, "")
}

// An ADVERT resolved by the forward chain keeps exactly that result; the
// backward walk only fills hops the forward chain left nil.
func TestObserverAnchor_AdvertForwardResultUnchanged_188(t *testing.T) {
	idx := idx188(allNodes188...)
	g := graph188([2]string{from188, a1a}, [2]string{a1a, b2a}, [2]string{b2a, c3a}, [2]string{obs188, c3a})
	hops := []string{"a1", "b2", "c3"}
	fwd := resolvePathWithContext(hops, from188, g, idx)
	wantPath188(t, fwd, a1a, b2a, c3a)
	got := resolveObservationPath(hops, from188, obs188, routeFlood188, g, idx)
	wantPath188(t, got, a1a, b2a, c3a)

	// Forward resolves hop 0 only; the backward walk fills hops 1 and 2.
	g = graph188([2]string{from188, a1a}, [2]string{obs188, c3a}, [2]string{c3a, b2a})
	got = resolveObservationPath(hops, from188, obs188, routeFlood188, g, idx)
	wantPath188(t, got, a1a, b2a, c3a)
}

// Forward and backward never contradict each other in one row: when they
// resolve the same hop differently, or would put one node at two positions,
// the backward result is dropped for the whole row.
func TestObserverAnchor_ForwardBackwardMeetOrNil_188(t *testing.T) {
	idx := idx188(allNodes188...)
	hops := []string{"a1", "b2", "c3"}

	t.Run("agree", func(t *testing.T) {
		// Forward: a1a -> b2a, then c3 is ambiguous (both neighbour b2a).
		// Backward: observer -> c3a -> b2a -> a1a, agreeing on hops 0-1.
		g := graph188([2]string{from188, a1a}, [2]string{a1a, b2a}, [2]string{b2a, c3a}, [2]string{b2a, c3b},
			[2]string{obs188, c3a})
		fwd := resolvePathWithContext(hops, from188, g, idx)
		wantPath188(t, fwd, a1a, b2a, "")
		got := resolveObservationPath(hops, from188, obs188, routeFlood188, g, idx)
		wantPath188(t, got, a1a, b2a, c3a)
	})
	t.Run("disagree on a hop keeps the forward result", func(t *testing.T) {
		// Forward: a1a -> b2a, then c3 has no neighbour of b2a.
		// Backward: observer -> c3b -> b2b. Hop 1 conflicts (b2a vs b2b).
		g := graph188([2]string{from188, a1a}, [2]string{a1a, b2a},
			[2]string{obs188, c3b}, [2]string{c3b, b2b})
		fwd := resolvePathWithContext(hops, from188, g, idx)
		wantPath188(t, fwd, a1a, b2a, "")
		got := resolveObservationPath(hops, from188, obs188, routeFlood188, g, idx)
		wantPath188(t, got, a1a, b2a, "")
	})
	t.Run("same node at two positions keeps the forward result", func(t *testing.T) {
		// A path that names the same prefix twice: forward puts c3a at
		// hop 0, backward puts c3a at hop 1.
		g := graph188([2]string{from188, c3a}, [2]string{obs188, c3a})
		got := resolveObservationPath([]string{"c3", "c3"}, from188, obs188, routeFlood188, g, idx)
		wantPath188(t, got, c3a, "")
	})
}

// Invariant over random graphs and paths: every hop the forward chain
// resolved is kept, every resolved hop matches its prefix, and no node
// appears twice in a row.
func TestObserverAnchor_NeverContradictsForward_Random_188(t *testing.T) {
	rng := rand.New(rand.NewSource(188))
	var keys []string
	for p := 0; p < 6; p++ {
		for v := 0; v < 3; v++ {
			keys = append(keys, fmt.Sprintf("%02x%02x", 0xa0+p, v)+strings.Repeat("0", 60))
		}
	}
	keys = append(keys, obs188, from188)
	idx := idx188(keys...)
	resolvedSome := 0
	for iter := 0; iter < 2000; iter++ {
		g := NewNeighborGraph()
		for e := 0; e < 25; e++ {
			g.AddEdge(keys[rng.Intn(len(keys))], keys[rng.Intn(len(keys))])
		}
		n := 1 + rng.Intn(5)
		hops := make([]string, n)
		for i := range hops {
			hops[i] = fmt.Sprintf("%02x", 0xa0+rng.Intn(6))
		}
		from := ""
		if rng.Intn(2) == 0 {
			from = from188
		}
		fwd := resolvePathWithContext(hops, from, g, idx)
		got := resolveObservationPath(hops, from, obs188, routeFlood188, g, idx)
		seen := map[string]bool{}
		for i := range hops {
			if fwd[i] != nil && (got[i] == nil || *got[i] != *fwd[i]) {
				t.Fatalf("iter %d: hop %d changed from forward %s to %s", iter, i, path188(fwd), path188(got))
			}
			if got[i] == nil {
				continue
			}
			if !strings.HasPrefix(*got[i], hops[i]) {
				t.Fatalf("iter %d: hop %d %q does not match prefix %q", iter, i, *got[i], hops[i])
			}
			if seen[*got[i]] {
				t.Fatalf("iter %d: %s appears twice in %s", iter, (*got[i])[:4], path188(got))
			}
			seen[*got[i]] = true
			if fwd[i] == nil {
				resolvedSome++
			}
		}
	}
	if resolvedSome == 0 {
		t.Fatal("the backward walk never resolved a hop; the invariant was not exercised")
	}
}

func TestObserverAnchor_HashSizes_188(t *testing.T) {
	for _, size := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("%d-byte", size), func(t *testing.T) {
			// Two relays that share the first `size` bytes.
			shared := strings.Repeat("d4", size)
			x := shared + "11" + strings.Repeat("0", 62-2*size)
			y := shared + "22" + strings.Repeat("0", 62-2*size)
			idx := idx188(x, y, obs188)
			g := graph188([2]string{obs188, y})
			got := resolveObservationPath([]string{shared}, "", obs188, routeFlood188, g, idx)
			wantPath188(t, got, y)
			if fwd := resolvePathWithContext([]string{shared}, "", g, idx); fwd[0] != nil {
				t.Fatalf("setup: the forward chain already resolves %s", shared)
			}
		})
	}
}

// End to end through InsertTransmission: a non-advert flood packet with a
// 1-byte path is stored with its last hop resolved from the observer's
// neighbour edge. On master the row is NULL.
func TestInsertTransmission_ObserverAnchorResolvesLastHop_188(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "ingest.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.backfillWg.Wait()

	for _, n := range []struct{ pk, role string }{{c3a, "repeater"}, {c3b, "repeater"}, {b2a, "repeater"}, {b2b, "room"}} {
		if _, err := store.db.Exec(`INSERT INTO nodes (public_key, name, role) VALUES (?, ?, ?)`, n.pk, n.pk[:4], n.role); err != nil {
			t.Fatal(err)
		}
	}
	observerID := strings.ToUpper(obs188) // as it arrives in the MQTT topic
	if err := store.UpsertObserver(observerID, "observer-188", "", nil); err != nil {
		t.Fatal(err)
	}
	// The neighbour builder writes observer<->last-hop edges lower-cased.
	if _, err := store.db.Exec(`INSERT INTO neighbor_edges (node_a, node_b, count, last_seen) VALUES (?, ?, 5, '2026-06-01T00:00:00Z')`, obs188, c3a); err != nil {
		t.Fatal(err)
	}
	if err := store.RefreshPrefixIndex(); err != nil {
		t.Fatal(err)
	}
	if err := store.RefreshNeighborGraph(); err != nil {
		t.Fatal(err)
	}
	pkt := &PacketData{
		RawHex:      "c0ffee",
		Timestamp:   "2026-06-01T00:00:00Z",
		ObserverID:  observerID,
		Hash:        "h-188-anchor",
		RouteType:   routeFlood188,
		PayloadType: 5, // GRP_TXT: no fromPubkey
		PathJSON:    `["b2","c3"]`,
		DecodedJSON: "{}",
	}
	if _, err := store.InsertTransmission(pkt); err != nil {
		t.Fatal(err)
	}
	var rp sql.NullString
	if err := store.db.QueryRow(`SELECT resolved_path FROM observations WHERE transmission_id = (SELECT id FROM transmissions WHERE hash = ?)`, pkt.Hash).Scan(&rp); err != nil {
		t.Fatal(err)
	}
	if !rp.Valid {
		t.Fatal("resolved_path is NULL: the observer did not anchor the last hop")
	}
	wantPath188(t, unmarshalResolvedPathLocal(rp.String), "", c3a)
}
