package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The relay-aware last_seen touch.
//
// History: the server had touchRelayLastSeen (cmd/server/store.go), which
// called TouchNodeLastSeen -> UPDATE nodes SET last_seen. Since the server
// opens SQLite with mode=ro, that UPDATE has failed with "attempt to write
// a readonly database" on every call, and the error was discarded at the
// call site. Net effect: nodes.last_seen has tracked ADVERT arrivals only,
// and relay participation has never refreshed it.
//
// The writer lives in the ingestor, which already resolves hop prefixes to
// full pubkeys for observations.resolved_path. These tests pin the touch
// to the unique-prefix resolution (resolvePath), the server's rule for relay
// evidence; the contextual resolution is for display only (#897).

// helper: seed a node so the prefix index can resolve a hop to it.
func seedRelayNode(t *testing.T, s *Store, pubkey, name, lastSeen string) {
	t.Helper()
	if err := s.UpsertNode(pubkey, name, "repeater", nil, nil, lastSeen); err != nil {
		t.Fatalf("seed node %s: %v", name, err)
	}
}

func nodeLastSeen(t *testing.T, s *Store, pubkey string) string {
	t.Helper()
	var ls string
	if err := s.db.QueryRow(`SELECT COALESCE(last_seen,'') FROM nodes WHERE public_key=?`, pubkey).Scan(&ls); err != nil {
		t.Fatalf("read last_seen for %s: %v", pubkey, err)
	}
	return ls
}

// TestTouchRelayNodes_AdvancesLastSeen is the core regression: a node that
// appears as a resolved relay hop must have its last_seen advanced, even
// though it sent no ADVERT of its own.
func TestTouchRelayNodes_AdvancesLastSeen(t *testing.T) {
	store := newTestStore(t)

	const relay = "aa11223344556677889900aabbccddeeff00112233445566778899aabbccddee"
	seedRelayNode(t, store, relay, "RelayOnly", "2026-07-01T00:00:00Z")

	rxTime := "2026-07-10T12:00:00Z"
	store.touchRelayNodesLocked([]string{relay}, rxTime)

	got := nodeLastSeen(t, store, relay)
	if got != rxTime {
		t.Errorf("last_seen = %q, want %q -- relay participation did not refresh the node", got, rxTime)
	}
	if n := store.Stats.RelayTouches.Load(); n != 1 {
		t.Errorf("RelayTouches = %d, want 1", n)
	}
}

// TestTouchRelayNodes_NeverGoesBackwards guards the monotonic invariant.
// Out-of-order ingest (a late observation with an older rxTime) must not
// rewind a node's last_seen.
func TestTouchRelayNodes_NeverGoesBackwards(t *testing.T) {
	store := newTestStore(t)

	const relay = "bb11223344556677889900aabbccddeeff00112233445566778899aabbccddee"
	seedRelayNode(t, store, relay, "Backbone", "2026-07-10T12:00:00Z")

	store.touchRelayNodesLocked([]string{relay}, "2026-07-09T00:00:00Z")

	if got := nodeLastSeen(t, store, relay); got != "2026-07-10T12:00:00Z" {
		t.Errorf("last_seen went backwards: got %q, want 2026-07-10T12:00:00Z", got)
	}
}

// TestTouchRelayNodes_Debounces pins the write-amplification guard. The
// ingest path is hot; a backbone repeater appears in thousands of paths per
// hour and must not produce one UPDATE per observation.
func TestTouchRelayNodes_Debounces(t *testing.T) {
	store := newTestStore(t)

	const relay = "cc11223344556677889900aabbccddeeff00112233445566778899aabbccddee"
	seedRelayNode(t, store, relay, "Chatty", "2026-07-01T00:00:00Z")

	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	store.touchRelayNodesLocked([]string{relay}, base.Format(time.RFC3339))
	// Second hit two minutes later -- inside the debounce window, no write.
	store.touchRelayNodesLocked([]string{relay}, base.Add(2*time.Minute).Format(time.RFC3339))

	if n := store.Stats.RelayTouches.Load(); n != 1 {
		t.Errorf("RelayTouches = %d, want 1 (second touch should be debounced)", n)
	}
	if got := nodeLastSeen(t, store, relay); got != base.Format(time.RFC3339) {
		t.Errorf("last_seen = %q, want %q", got, base.Format(time.RFC3339))
	}

	// Past the debounce window the write goes through again.
	later := base.Add(6 * time.Minute)
	store.touchRelayNodesLocked([]string{relay}, later.Format(time.RFC3339))
	if n := store.Stats.RelayTouches.Load(); n != 2 {
		t.Errorf("RelayTouches = %d, want 2 after debounce window elapsed", n)
	}
	if got := nodeLastSeen(t, store, relay); got != later.Format(time.RFC3339) {
		t.Errorf("last_seen = %q, want %q", got, later.Format(time.RFC3339))
	}
}

// TestTouchRelayNodes_IgnoresEmptyAndUnknown covers the unresolved-hop case:
// resolvePath yields nil for ambiguous or unknown prefixes, and
// unknown pubkeys must not create rows.
func TestTouchRelayNodes_IgnoresEmptyAndUnknown(t *testing.T) {
	store := newTestStore(t)

	store.touchRelayNodesLocked(nil, "2026-07-10T12:00:00Z")
	store.touchRelayNodesLocked([]string{}, "2026-07-10T12:00:00Z")
	store.touchRelayNodesLocked([]string{""}, "2026-07-10T12:00:00Z")
	store.touchRelayNodesLocked([]string{"ff99887766554433221100ffeeddccbbaa99887766554433221100ffeeddccbb"}, "2026-07-10T12:00:00Z")

	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("nodes count = %d, want 0 -- touch must never insert rows", count)
	}
	if n := store.Stats.RelayTouches.Load(); n != 0 {
		t.Errorf("RelayTouches = %d, want 0", n)
	}
}

// TestTouchRelayNodes_MalformedTimestamp: rxTime that does not parse must be
// a no-op rather than writing a garbage timestamp into the node directory.
func TestTouchRelayNodes_MalformedTimestamp(t *testing.T) {
	store := newTestStore(t)

	const relay = "dd11223344556677889900aabbccddeeff00112233445566778899aabbccddee"
	seedRelayNode(t, store, relay, "Fine", "2026-07-01T00:00:00Z")

	store.touchRelayNodesLocked([]string{relay}, "not-a-timestamp")

	if got := nodeLastSeen(t, store, relay); got != "2026-07-01T00:00:00Z" {
		t.Errorf("last_seen = %q, want it unchanged on malformed rxTime", got)
	}
	if n := store.Stats.RelayTouches.Load(); n != 0 {
		t.Errorf("RelayTouches = %d, want 0", n)
	}
}

// TestResolvedPubkeys_DedupsAndDropsNil covers the flattening helper
// touchRelayNodesLocked's caller uses (resolved_path.go).
func TestResolvedPubkeys_DedupsAndDropsNil(t *testing.T) {
	pk1 := "aaaa"
	pk2 := "bbbb"
	empty := ""
	rp := []*string{&pk1, nil, &pk2, &pk1, &empty}
	got := resolvedPubkeys(rp)
	if len(got) != 2 || got[0] != "aaaa" || got[1] != "bbbb" {
		t.Errorf("resolvedPubkeys = %v, want [aaaa bbbb] (dedup + nil/empty dropped)", got)
	}
	if got := resolvedPubkeys(nil); got != nil {
		t.Errorf("resolvedPubkeys(nil) = %v, want nil", got)
	}
}

// TestInsertTransmission_TouchesOnlyFloodRelays pins the route/payload gate
// in front of the touch (#623, upstream 2308).
//
// Only a flood path lists relays that already transmitted the packet: each
// forwarder appends its own hash (Mesh.cpp routeRecvPacket). A direct route's
// path is the hops still to come -- every forwarder strips itself from the
// front (Mesh.cpp removeSelfFromPath), the same reason deriveHeardKey in
// client_reception.go refuses direct paths. And a TRACE's path_json is the
// route it was asked to take, because its header path bytes are per-hop SNR
// (packetpath.PathBytesAreHops). So tracing or direct-messaging a switched-off
// repeater must not mark it heard.
func TestInsertTransmission_TouchesOnlyFloodRelays(t *testing.T) {
	cases := []struct {
		name        string
		routeType   int
		payloadType int
		wantTouch   bool
	}{
		{"transport flood TXT_MSG", 0, 2, true},
		{"flood TXT_MSG", 1, 2, true},
		{"direct TXT_MSG", 2, 2, false},
		{"transport direct TXT_MSG", 3, 2, false},
		{"direct TRACE", 2, 9, false},
		// A flood-routed TRACE is not something the firmware sends
		// (sendFlood refuses TRACE, Mesh.cpp:638), but the payload type
		// alone already disqualifies the path: its bytes are SNR, so the
		// hops never prove a transmission.
		{"flood TRACE", 1, 9, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)

			const relay = "4b6091e5d29f53738c0dbe013cb4aeb738955447a92465d9445e18d4099d940e"
			const before = "2026-07-01T00:00:00Z"
			seedRelayNode(t, store, relay, "Switched off", before)
			if err := store.RefreshPrefixIndex(); err != nil {
				t.Fatal(err)
			}
			if err := store.UpsertObserver("fixture-observer", "Fixture observer", "", nil); err != nil {
				t.Fatal(err)
			}

			rxTime := "2026-07-10T12:00:00Z"
			data := &PacketData{
				Hash:        "touch-" + tc.name,
				RouteType:   tc.routeType,
				PayloadType: tc.payloadType,
				PathJSON:    `["4b60"]`,
				ObserverID:  "fixture-observer",
				Timestamp:   rxTime,
			}
			if _, err := store.InsertTransmission(data); err != nil {
				t.Fatal(err)
			}

			want := before
			wantTouches := int64(0)
			if tc.wantTouch {
				want, wantTouches = rxTime, 1
			}
			if got := nodeLastSeen(t, store, relay); got != want {
				t.Errorf("last_seen = %q, want %q", got, want)
			}
			if n := store.Stats.RelayTouches.Load(); n != wantTouches {
				t.Errorf("RelayTouches = %d, want %d", n, wantTouches)
			}

			// The gate is on the touch only: resolved_path stays written
			// for every observation, so hop analytics are unchanged.
			var resolved string
			if err := store.db.QueryRow(
				`SELECT COALESCE(o.resolved_path,'') FROM observations o
				 JOIN transmissions t ON t.id = o.transmission_id
				 WHERE t.hash = ?`, data.Hash,
			).Scan(&resolved); err != nil {
				t.Fatal(err)
			}
			if resolved != `["`+relay+`"]` {
				t.Errorf("resolved_path = %q, want the resolved hop regardless of route", resolved)
			}
		})
	}
}

// floodACK builds one observation of a FLOOD ACK (header 0x0d) whose path
// holds hops of width bytes each, followed by the 4-byte ACK payload.
func floodACK(hash, observer, rxTime string, width int, hops ...string) *PacketData {
	return floodPacket(hash, observer, rxTime, 3, width, hops...)
}

// floodPacket builds one FLOOD observation of payloadType whose path holds
// hops of width bytes each, followed by a 4-byte placeholder payload.
func floodPacket(hash, observer, rxTime string, payloadType, width int, hops ...string) *PacketData {
	pathByte := byte(width-1)<<6 | byte(len(hops))
	quoted := make([]string, len(hops))
	for i, h := range hops {
		quoted[i] = `"` + h + `"`
	}
	return &PacketData{
		Hash: hash, RouteType: 1, PayloadType: payloadType,
		RawHex:     fmt.Sprintf("%02x%02x%s11223344", payloadType<<2|1, pathByte, strings.Join(hops, "")),
		PathJSON:   "[" + strings.Join(quoted, ",") + "]",
		ObserverID: observer, Timestamp: rxTime,
	}
}

func storedResolvedPath(t *testing.T, s *Store, hash string) string {
	t.Helper()
	var rp string
	if err := s.db.QueryRow(
		`SELECT COALESCE(o.resolved_path,'') FROM observations o
		 JOIN transmissions t ON t.id = o.transmission_id
		 WHERE t.hash = ?`, hash,
	).Scan(&rp); err != nil {
		t.Fatal(err)
	}
	return rp
}

// TestInsertTransmission_AdjacencyDoesNotRefreshCollidingRelays (#897,
// upstream 2334/2329): adjacency settles a colliding prefix for the displayed
// path, but it does not prove which of the colliding relays transmitted. The
// touch takes the server's rule (confirmedRelayKey in
// cmd/server/node_activity.go): only a prefix that names one relay refreshes
// last_seen. The path mixes a unique hop with a colliding one, at every wire
// hash size.
func TestInsertTransmission_AdjacencyDoesNotRefreshCollidingRelays(t *testing.T) {
	for width := 1; width <= 3; width++ {
		t.Run(fmt.Sprintf("%d-byte hops", width), func(t *testing.T) {
			store := newTestStore(t)
			uniquePfx := strings.Repeat("aa", width)
			collidingPfx := strings.Repeat("bb", width)
			unique := uniquePfx + strings.Repeat("11", 32-width)
			adjacent := collidingPfx + strings.Repeat("22", 32-width)
			other := collidingPfx + strings.Repeat("33", 32-width)
			const before = "2026-07-01T00:00:00Z"
			const rxTime = "2026-07-10T12:00:00Z"
			for _, pk := range []string{unique, adjacent, other} {
				seedRelayNode(t, store, pk, "Fixture relay", before)
			}
			if err := store.RefreshPrefixIndex(); err != nil {
				t.Fatal(err)
			}
			graph := NewNeighborGraph()
			graph.AddEdge(unique, adjacent)
			store.neighborGraph.store(graph)
			if err := store.UpsertObserver("fixture-observer", "Fixture observer", "", nil); err != nil {
				t.Fatal(err)
			}

			data := floodACK("relay-certainty", "fixture-observer", rxTime, width, uniquePfx, collidingPfx)
			if _, err := store.InsertTransmission(data); err != nil {
				t.Fatal(err)
			}

			// The displayed path keeps the contextual resolution.
			if got, want := storedResolvedPath(t, store, data.Hash), `["`+unique+`","`+adjacent+`"]`; got != want {
				t.Fatalf("resolved_path = %s, want %s", got, want)
			}
			if got := nodeLastSeen(t, store, unique); got != rxTime {
				t.Errorf("unique relay last_seen = %q, want %q", got, rxTime)
			}
			for _, pk := range []string{adjacent, other} {
				if got := nodeLastSeen(t, store, pk); got != before {
					t.Errorf("colliding relay %s last_seen = %q, want unchanged %q", pk[:8], got, before)
				}
			}
			if got := store.Stats.RelayTouches.Load(); got != 1 {
				t.Errorf("RelayTouches = %d, want 1 (the unique hop only)", got)
			}
		})
	}
}

// TestInsertTransmission_ObserverAnchorDoesNotRefreshCollidingRelay covers
// the other contextual resolution in this fork: a flood path's last hop is
// settled backwards from the observer (#188, resolveObservationPath). That
// is display evidence too, not proof of who transmitted.
func TestInsertTransmission_ObserverAnchorDoesNotRefreshCollidingRelay(t *testing.T) {
	store := newTestStore(t)
	adjacent := "bb" + strings.Repeat("22", 31)
	other := "bb" + strings.Repeat("33", 31)
	observer := "cc" + strings.Repeat("44", 31)
	const before = "2026-07-01T00:00:00Z"
	const rxTime = "2026-07-10T12:00:00Z"
	for _, pk := range []string{adjacent, other} {
		seedRelayNode(t, store, pk, "Fixture relay", before)
	}
	if err := store.RefreshPrefixIndex(); err != nil {
		t.Fatal(err)
	}
	graph := NewNeighborGraph()
	graph.AddEdge(observer, adjacent)
	store.neighborGraph.store(graph)
	if err := store.UpsertObserver(observer, "Fixture observer", "", nil); err != nil {
		t.Fatal(err)
	}

	data := floodACK("observer-anchor", observer, rxTime, 1, "bb")
	if _, err := store.InsertTransmission(data); err != nil {
		t.Fatal(err)
	}

	if got, want := storedResolvedPath(t, store, data.Hash), `["`+adjacent+`"]`; got != want {
		t.Fatalf("resolved_path = %s, want %s", got, want)
	}
	for _, pk := range []string{adjacent, other} {
		if got := nodeLastSeen(t, store, pk); got != before {
			t.Errorf("colliding relay %s last_seen = %q, want unchanged %q", pk[:8], got, before)
		}
	}
	if got := store.Stats.RelayTouches.Load(); got != 0 {
		t.Errorf("RelayTouches = %d, want 0", got)
	}
}

// TestInsertTransmission_ListenerRoleDoesNotMakePrefixAmbiguous pins the
// candidate set the touch shares with the server: only relay roles are prefix
// candidates (isRelayRole here, canAppearInPath in buildPrefixMap there), so
// a companion sharing a repeater's prefix leaves that prefix unique and the
// repeater is refreshed, as the server's uniqueResolve would confirm it.
func TestInsertTransmission_ListenerRoleDoesNotMakePrefixAmbiguous(t *testing.T) {
	store := newTestStore(t)
	repeater := "dd" + strings.Repeat("55", 31)
	companion := "dd" + strings.Repeat("66", 31)
	const before = "2026-07-01T00:00:00Z"
	const rxTime = "2026-07-10T12:00:00Z"
	seedRelayNode(t, store, repeater, "Fixture relay", before)
	if err := store.UpsertNode(companion, "Fixture companion", "companion", nil, nil, before); err != nil {
		t.Fatal(err)
	}
	if err := store.RefreshPrefixIndex(); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertObserver("fixture-observer", "Fixture observer", "", nil); err != nil {
		t.Fatal(err)
	}

	if _, err := store.InsertTransmission(floodACK("listener-role", "fixture-observer", rxTime, 1, "dd")); err != nil {
		t.Fatal(err)
	}

	if got := nodeLastSeen(t, store, repeater); got != rxTime {
		t.Errorf("repeater last_seen = %q, want %q", got, rxTime)
	}
	if got := nodeLastSeen(t, store, companion); got != before {
		t.Errorf("companion last_seen = %q, want unchanged %q", got, before)
	}
}

// TestResolveObservationPath_ContextNullsUniqueHops reproduces the staging
// finding on PR #899: the unique-prefix touch refreshes some relays that the
// contextual resolver did not. The only branch where resolveHopWithContext
// returns nil for a prefix with exactly one relay candidate is its exclude
// set, which holds the advert originator, the hops already resolved on the
// path, and, for the last hop of a flood path, the observer. Those rules are
// about a hop's position in the displayed path, not about who the prefix
// names, so resolvePath (the touch, and the server's uniqueResolve) still
// names the node.
func TestResolveObservationPath_ContextNullsUniqueHops(t *testing.T) {
	relay := "aa" + strings.Repeat("11", 31)
	other := "cc" + strings.Repeat("33", 31)
	idx := prefixIndex{"aa": {relay}, "cc": {other}}
	cases := []struct {
		name     string
		hops     []string
		from     string
		observer string
		nulled   int // index of the hop the contextual resolver leaves nil
	}{
		// A repeater observer that reports its own transmissions (the bridge
		// default is logTx, MeshCore src/helpers/CommonCLI.h bridge_pkt_src)
		// reports a forwarded flood with its own hash appended last
		// (Mesh.cpp routeRecvPacket, Dispatcher.cpp logTx after the send).
		{"last hop is the observer", []string{"cc", "aa"}, "", relay, 1},
		// The originator of an ADVERT echoed back to it, or a node outside
		// the nodes table that shares its prefix.
		{"hop is the advert originator", []string{"aa", "cc"}, relay, "", 0},
		// A loop: the second position repeats a node already resolved.
		{"node already on the path", []string{"aa", "cc", "aa"}, "", "", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := resolveObservationPath(tc.hops, tc.from, tc.observer, routeTypeFlood, NewNeighborGraph(), idx)
			uniq := resolvePath(tc.hops, idx)
			if ctx[tc.nulled] != nil {
				t.Errorf("contextual hop %d = %s, want nil", tc.nulled, *ctx[tc.nulled])
			}
			if uniq[tc.nulled] == nil || *uniq[tc.nulled] != relay {
				t.Errorf("unique-prefix hop %d = %v, want %s", tc.nulled, uniq[tc.nulled], relay)
			}
		})
	}
}

// TestInsertTransmission_UniqueHopNulledByContextStillRefreshes pins the
// decision on PR #899: liveness follows the server's confirmedRelayKey, which
// takes a unique prefix wherever it sits in the path. In the two cases where
// the contextual exclusion changes the touch set, the node is alive at rxTime
// anyway: it is the observer reporting this frame, or the originator of this
// advert. The stored resolved_path keeps the hop nil.
func TestInsertTransmission_UniqueHopNulledByContextStillRefreshes(t *testing.T) {
	relay := "aa" + strings.Repeat("11", 31)
	other := "cc" + strings.Repeat("33", 31)
	const before = "2026-07-01T00:00:00Z"
	const rxTime = "2026-07-10T12:00:00Z"
	cases := []struct {
		name     string
		data     *PacketData
		resolved string
	}{
		{
			name:     "last hop is the observer",
			data:     floodACK("observer-own-tx", relay, rxTime, 1, "cc", "aa"),
			resolved: `["` + other + `",null]`,
		},
		{
			name: "hop is the advert originator",
			data: func() *PacketData {
				d := floodPacket("originator-echo", "fixture-observer", rxTime, PayloadADVERT, 1, "aa", "cc")
				d.FromPubkey = relay
				return d
			}(),
			resolved: `[null,"` + other + `"]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			for _, pk := range []string{relay, other} {
				seedRelayNode(t, store, pk, "Fixture relay", before)
			}
			if err := store.RefreshPrefixIndex(); err != nil {
				t.Fatal(err)
			}
			if err := store.UpsertObserver(tc.data.ObserverID, "Fixture observer", "", nil); err != nil {
				t.Fatal(err)
			}

			if _, err := store.InsertTransmission(tc.data); err != nil {
				t.Fatal(err)
			}

			if got := storedResolvedPath(t, store, tc.data.Hash); got != tc.resolved {
				t.Fatalf("resolved_path = %s, want %s", got, tc.resolved)
			}
			for _, pk := range []string{relay, other} {
				if got := nodeLastSeen(t, store, pk); got != rxTime {
					t.Errorf("relay %s last_seen = %q, want %q", pk[:8], got, rxTime)
				}
			}
			if got := store.Stats.RelayTouches.Load(); got != 2 {
				t.Errorf("RelayTouches = %d, want 2", got)
			}
		})
	}
}
