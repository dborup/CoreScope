package main

import (
	"strings"
	"testing"
	"time"
)

// #382: the node-detail help text says "Total Packets" counts distinct
// transmissions that involve the node (sender, recipient, resolved relay) in
// the in-memory packet window, and that the seen count sums the observations of
// those transmissions. Pin that to what /api/nodes/{pk}/health returns.
func TestNodeHealthPacketCountSemantics_382(t *testing.T) {
	pk := "a1b2c3" + strings.Repeat("11", 29)
	other := "d4e5f6" + strings.Repeat("22", 29)
	db, store := activityTestStore(t, pk, other)
	defer db.conn.Close()
	ts := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)

	// Originator: own advert heard by two observers.
	storeObservedTx(store, 1, payloadTypeAdvert, ts, `{"type":"ADVERT","pubKey":"`+pk+`"}`, `[]`, `[]`)
	// Destination: one transmission with three observations.
	storeObservedTx(store, 2, 2, ts, `{"type":"TXT_MSG","destPubKey":"`+pk+`"}`, `[]`, `[]`, `[]`)
	// Unrelated traffic must not be counted for this node.
	storeObservedTx(store, 3, payloadTypeAdvert, ts, `{"type":"ADVERT","pubKey":"`+other+`"}`, `[]`)

	health, err := store.GetNodeHealth(pk, time.Now().UTC())
	if err != nil || health == nil {
		t.Fatalf("health: %v %v", health, err)
	}
	stats := health["stats"].(NodeHealthStats)
	if stats.TotalTransmissions != 2 || stats.TotalPackets != 2 {
		t.Errorf("transmissions = %d, packets = %d; want 2 distinct transmissions", stats.TotalTransmissions, stats.TotalPackets)
	}
	if stats.TotalObservations != 5 {
		t.Errorf("observations = %d; want 5 (2 + 3), more than the transmission count", stats.TotalObservations)
	}
}
