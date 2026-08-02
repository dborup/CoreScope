package main

// Regression tests: getNodeClockSkewLocked() used to iterate every ADVERT
// transaction indexed under a pubkey in s.byNode WITHOUT checking whether
// the node actually originated (self-signed) that advert. byNode is an
// involvement index (indexResolvedPathHops): a transmission is also indexed
// under every relay-hop pubkey extracted from an observation's
// resolved_path. So a relay that merely forwarded a broken-clock node's
// advert inherited that node's skew as if it were its own -- producing
// fleet-wide false no_clock/bimodal classifications and bit-identical
// skew "clusters" across unrelated relays, plus a single relay showing a
// nonsense multi-day skew even though its own self-adverts are healthy,
// because a couple of relayed adverts from a broken originator landed in
// the tail of its small recent-window sample.
//
// The fix (txOriginatedBy in clock_skew.go) restricts the ADVERT filter in
// getNodeClockSkewLocked to self-originated adverts only (decoded pubKey
// == pubkey), leaving byNode itself untouched (other consumers still
// depend on the broader involvement index).

import (
	"testing"
	"time"
)

func TestTxOriginatedBy(t *testing.T) {
	pt := 4 // ADVERT
	selfTx := &StoreTx{
		Hash:        "self-1",
		PayloadType: &pt,
		DecodedJSON: `{"payload":{"timestamp":1700000000},"pubKey":"AABBCC"}`,
	}
	foreignTx := &StoreTx{
		Hash:        "foreign-1",
		PayloadType: &pt,
		DecodedJSON: `{"payload":{"timestamp":1700000000},"pubKey":"DDEEFF"}`,
	}
	noPubkeyTx := &StoreTx{
		Hash:        "nopk-1",
		PayloadType: &pt,
		DecodedJSON: `{"payload":{"timestamp":1700000000}}`,
	}
	mixedCaseTx := &StoreTx{
		Hash:        "case-1",
		PayloadType: &pt,
		DecodedJSON: `{"payload":{"timestamp":1700000000},"pubKey":"aabbcc"}`,
	}

	if !txOriginatedBy(selfTx, "AABBCC") {
		t.Error("expected self-signed advert to match its own pubkey")
	}
	if txOriginatedBy(foreignTx, "AABBCC") {
		t.Error("expected advert from a different originator NOT to match")
	}
	if txOriginatedBy(noPubkeyTx, "AABBCC") {
		t.Error("expected advert with no decoded pubKey NOT to match anything")
	}
	if !txOriginatedBy(mixedCaseTx, "AABBCC") {
		t.Error("expected case-insensitive pubkey match to succeed")
	}
}

// A relay ("RELAY001") only ever transmits healthy self-adverts (skew ~0s).
// It also forwards adverts from a broken-clock originator ("BROKENORIG",
// skew ~+100,500s) -- those transmissions are indexed under RELAY001 in
// byNode too, exactly as indexResolvedPathHops does for real relay-hop
// traffic. Before the fix, these foreign adverts polluted RELAY001's skew
// stream and could flip its severity to no_clock/bimodal even though every
// advert RELAY001 itself sent is fine.
func TestRelayDoesNotInheritOriginatorSkew(t *testing.T) {
	ps := NewPacketStore(nil, nil)
	pt := 4 // ADVERT
	baseObs := int64(1700000000)

	var relayTxs []*StoreTx

	// RELAY001's own healthy adverts: skew ~0s.
	for i := 0; i < 5; i++ {
		obsTS := baseObs + int64(i)*300
		advTS := obsTS - 2 // 2s behind, trivially healthy
		tx := &StoreTx{
			Hash:        "relay-self-" + formatInt64(int64(i)),
			PayloadType: &pt,
			DecodedJSON: `{"payload":{"timestamp":` + formatInt64(advTS) + `},"pubKey":"RELAY001"}`,
			Observations: []*StoreObs{
				{ObserverID: "obs1", Timestamp: time.Unix(obsTS, 0).UTC().Format(time.RFC3339)},
			},
		}
		relayTxs = append(relayTxs, tx)
	}

	// BROKENORIG's adverts: skew ~+100,500s. Indexed under RELAY001 too
	// (simulating indexResolvedPathHops adding the transmission under the
	// relay-hop pubkey), but their decoded pubKey is BROKENORIG, not
	// RELAY001.
	for i := 0; i < 5; i++ {
		obsTS := baseObs + int64(100+i)*300
		advTS := obsTS + 100500
		tx := &StoreTx{
			Hash:        "relayed-foreign-" + formatInt64(int64(i)),
			PayloadType: &pt,
			DecodedJSON: `{"payload":{"timestamp":` + formatInt64(advTS) + `},"pubKey":"BROKENORIG"}`,
			Observations: []*StoreObs{
				{ObserverID: "obs1", Timestamp: time.Unix(obsTS, 0).UTC().Format(time.RFC3339)},
			},
		}
		relayTxs = append(relayTxs, tx)
	}

	ps.mu.Lock()
	ps.byNode["RELAY001"] = relayTxs
	for _, tx := range relayTxs {
		ps.byPayloadType[4] = append(ps.byPayloadType[4], tx)
	}
	ps.clockSkew.computeInterval = 0
	ps.mu.Unlock()

	r := ps.GetNodeClockSkew("RELAY001")
	if r == nil {
		t.Fatal("expected clock skew result for RELAY001")
	}
	if r.Severity != SkewOK {
		t.Errorf("severity = %v, want ok -- relay's own adverts are healthy; "+
			"relayed foreign adverts must not be attributed to it", r.Severity)
	}
	if abs(r.RecentMedianSkewSec) > 5 {
		t.Errorf("recentMedianSkewSec = %v, want ~0 (only RELAY001's own adverts "+
			"should count, not the relayed +100.5k s originator adverts)", r.RecentMedianSkewSec)
	}
	if r.RecentSampleCount != 5 {
		t.Errorf("recentSampleCount = %d, want 5 (relayed adverts must be excluded)", r.RecentSampleCount)
	}
}

// Five distinct relay pubkeys never self-advert at all -- they only forward
// a single broken-clock originator's advert. Before the fix, each relay's
// GetNodeClockSkew would report the SAME bit-identical skew as the
// originator (a false "cluster"). After the fix, a node with zero
// self-originated adverts must report no clock-skew data at all (nil).
func TestPureRelaysReportNoSkew(t *testing.T) {
	ps := NewPacketStore(nil, nil)
	pt := 4 // ADVERT
	baseObs := int64(1700000000)

	const originator = "BROKENORIG2"
	obsTS := baseObs
	advTS := obsTS + 201000

	originatorTx := &StoreTx{
		Hash:        "originator-advert-1",
		PayloadType: &pt,
		DecodedJSON: `{"payload":{"timestamp":` + formatInt64(advTS) + `},"pubKey":"` + originator + `"}`,
		Observations: []*StoreObs{
			{ObserverID: "obs1", Timestamp: time.Unix(obsTS, 0).UTC().Format(time.RFC3339)},
		},
	}

	relayKeys := []string{"RLY-A", "RLY-B", "RLY-C", "RLY-D", "RLY-E"}

	ps.mu.Lock()
	ps.byNode[originator] = []*StoreTx{originatorTx}
	for _, rk := range relayKeys {
		ps.byNode[rk] = []*StoreTx{originatorTx}
	}
	ps.byPayloadType[4] = []*StoreTx{originatorTx}
	ps.clockSkew.computeInterval = 0
	ps.mu.Unlock()

	origResult := ps.GetNodeClockSkew(originator)
	if origResult == nil {
		t.Fatal("expected clock skew result for the originator")
	}
	if abs(origResult.RecentMedianSkewSec-201000) > 5 {
		t.Errorf("originator recentMedianSkewSec = %v, want ~201000", origResult.RecentMedianSkewSec)
	}

	for _, rk := range relayKeys {
		r := ps.GetNodeClockSkew(rk)
		if r != nil {
			t.Errorf("relay %s: expected nil (no self-originated adverts), "+
				"got skew=%v -- this is the bit-identical false cluster the fix must prevent",
				rk, r.RecentMedianSkewSec)
		}
	}
}
