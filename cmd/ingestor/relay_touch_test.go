package main

import (
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
// to that existing resolution point.

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
// resolvePathWithContext yields nil for ambiguous or unknown prefixes, and
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
