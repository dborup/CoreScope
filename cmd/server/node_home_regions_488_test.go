package main

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/meshcore-analyzer/packetpath"
)

// #488 M1: computeNodeHomeRegions (#804) classifies each ADVERT observation by
// its own frame (observations.raw_hex) with packetpath.ClassifyAdvert.
// Firmware (MeshCore a366955):
//   - src/Mesh.cpp:637/664 sendFlood originates FLOOD/TRANSPORT_FLOOD with
//     hash count 0; routeRecvPacket (:344-350) appends one hash per relay, so
//     hash count 0 on a flood advert is the originator's own transmission.
//   - src/Mesh.cpp:717 sendZeroHop: DIRECT, path_len 0.
//   - src/helpers/BaseChatMesh.cpp:539-551 shareContactZeroHop re-sends ANOTHER
//     node's stored advert through sendZeroHop(packet, {0,0}) (Mesh.cpp:728):
//     TRANSPORT_DIRECT, codes {0,0}, path_len 0. It says where the sharing
//     companion is, not the advertised node.
//   - src/Packet.cpp:65-86 readFrom: the path byte follows 4 transport-code
//     bytes on routes 0 and 3.

// rp488HomeDB has observers AAA (rowid 1), BBB (2), CCC (3) and CCC2 (4, also
// region CCC), and stores observations.raw_hex.
func rp488HomeDB(t *testing.T) *DB {
	t.Helper()
	db := setupTestDB(t)
	db.hasObsRawHexFlag.forceTrue()
	t.Cleanup(func() { db.Close() })
	for i, o := range []struct{ id, iata string }{{"obs-AAA", "AAA"}, {"obs-BBB", "BBB"}, {"obs-CCC", "CCC"}, {"obs-CCC2", "CCC"}} {
		if _, err := db.conn.Exec(`INSERT INTO observers (rowid, id, name, iata, last_seen, first_seen)
			VALUES (?, ?, ?, ?, '2026-10-01T00:00:00Z', '2026-01-01T00:00:00Z')`, i+1, o.id, o.id, o.iata); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// rp488Hear is one observation: observer rowid, path_json (nil = NULL) and the
// observation's own frame (nil = NULL, a legacy row).
type rp488Hear struct {
	obs       int
	path, raw interface{}
}

var rp488TxSeq int

// rp488Advert stores one ADVERT transmission by pk whose first-copy frame is
// txRaw, heard as given.
func rp488Advert(t *testing.T, db *DB, pk, txRaw string, hears ...rp488Hear) {
	t.Helper()
	rp488TxSeq++
	ts := time.Now().UTC().Add(-time.Hour)
	res, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json)
		VALUES (?, ?, ?, ?, 4, ?)`, txRaw, fmt.Sprintf("h488-%s-%d", pk, rp488TxSeq), ts.Format(time.RFC3339),
		int(txRaw[1]-'0')&3, `{"type":"ADVERT","pubKey":"`+pk+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	txID, _ := res.LastInsertId()
	for _, h := range hears {
		if _, err := db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp, raw_hex)
			VALUES (?, ?, ?, ?, ?)`, txID, h.obs, h.path, ts.Unix(), h.raw); err != nil {
			t.Fatal(err)
		}
	}
}

func rp488Homes(t *testing.T, db *DB) map[string]string {
	t.Helper()
	store := NewPacketStore(db, nil)
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	obsIATA := store.observerIATAs()
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.computeNodeHomeRegions(obsIATA)
}

const rp488HomePayload = "aabbccddeeff0011"

// TestRP488_CurrentHomeRegionParsing (flipped from the characterization in
// draft PR #601): a TRANSPORT_DIRECT {0,0} contact re-share is not the
// advertised node's home, while zero-hop FLOOD and TRANSPORT_FLOOD adverts
// are direct evidence (path byte at offset 5 on the transport route).
func TestRP488_CurrentHomeRegionParsing(t *testing.T) {
	db := rp488HomeDB(t)
	shared := "130000000000" + rp488HomePayload
	floodZero := "1100" + rp488HomePayload
	transportFloodZero := "101a2b3c4d00" + rp488HomePayload
	rp488Advert(t, db, "cccc000000000003", shared, rp488Hear{1, "[]", shared})
	rp488Advert(t, db, "dddd000000000004", floodZero, rp488Hear{2, "[]", floodZero})
	rp488Advert(t, db, "eeee000000000005", transportFloodZero, rp488Hear{3, "[]", transportFloodZero})

	home := rp488Homes(t, db)
	if got, ok := home["cccc000000000003"]; ok {
		t.Errorf("shared contact placed in %q; a re-share is no evidence of the advertised node's home", got)
	}
	if got := home["dddd000000000004"]; got != "BBB" {
		t.Errorf("zero-hop flood advert home = %q, want BBB", got)
	}
	if got := home["eeee000000000005"]; got != "CCC" {
		t.Errorf("zero-hop transport flood advert home = %q, want CCC (path byte at offset 5)", got)
	}
}

// The transmission's first-copy frame does not decide for its other
// observations: a content hash is shared by every copy (route bits are not
// hashed, Packet::calculatePacketHash), so each observation is classified
// by its own frame.
func TestNodeHomeRegions488_PerObservationFrame(t *testing.T) {
	db := rp488HomeDB(t)
	direct := "1200" + rp488HomePayload
	floodZero := "1100" + rp488HomePayload
	relayed := "1102a1a2" + rp488HomePayload
	shared := "130000000000" + rp488HomePayload

	// First copy says zero-hop DIRECT, but the only observation heard it
	// after two relays.
	rp488Advert(t, db, "aaaa000000000001", direct, rp488Hear{1, `["A1","A2"]`, relayed})
	// First copy is a relayed flood, the observation heard it zero-hop.
	rp488Advert(t, db, "bbbb000000000002", relayed, rp488Hear{2, "[]", floodZero})
	// First copy is a re-share heard by two CCC observers; one AAA observer
	// heard the originator's own zero-hop flood. The share must not outvote it.
	rp488Advert(t, db, "cccc000000000003", shared,
		rp488Hear{3, "[]", shared}, rp488Hear{4, "[]", shared}, rp488Hear{1, "[]", floodZero})
	// The backfill's "attempted, decode failed" '[]' next to a relayed frame.
	rp488Advert(t, db, "dddd000000000004", relayed, rp488Hear{2, "[]", relayed})
	// A DIRECT advert with a path counts the hops still to go (Mesh.cpp
	// removeSelfFromPath), not the hops travelled: no evidence.
	rp488Advert(t, db, "eeee000000000005", "1202a1a2"+rp488HomePayload, rp488Hear{3, `["A1","A2"]`, "1202a1a2" + rp488HomePayload})

	got := rp488Homes(t, db)
	want := map[string]string{"bbbb000000000002": "BBB", "cccc000000000003": "AAA"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("home regions = %v, want %v", got, want)
	}
}

// Owner decision on #488: an observation without its own frame (a legacy
// row) counts an explicit '[]' path only as fallback evidence. It places a
// node that has no verified direct hearing, and never outranks one.
func TestNodeHomeRegions488_LegacyEmptyPathIsFallbackOnly(t *testing.T) {
	db := rp488HomeDB(t)
	floodZero := "1100" + rp488HomePayload
	// One verified zero-hop hearing in AAA, three legacy '[]' rows in BBB/CCC.
	rp488Advert(t, db, "aaaa000000000001", floodZero,
		rp488Hear{1, "[]", floodZero}, rp488Hear{2, "[]", nil}, rp488Hear{3, "[]", nil}, rp488Hear{4, "[]", nil})
	// Legacy only: the plurality of '[]' rows decides (CCC 2, BBB 1).
	rp488Advert(t, db, "bbbb000000000002", floodZero,
		rp488Hear{2, "[]", nil}, rp488Hear{3, "[]", nil}, rp488Hear{4, "[]", nil})
	// Legacy rows with a missing, JSON-null or relayed path are no evidence.
	rp488Advert(t, db, "cccc000000000003", floodZero,
		rp488Hear{1, nil, nil}, rp488Hear{2, "null", nil}, rp488Hear{3, `["A1"]`, nil})
	// A verified re-share does not make the legacy fallback lose: the
	// share is no evidence at all.
	rp488Advert(t, db, "dddd000000000004", floodZero,
		rp488Hear{1, "[]", "130000000000" + rp488HomePayload}, rp488Hear{2, "[]", nil})

	got := rp488Homes(t, db)
	want := map[string]string{"aaaa000000000001": "AAA", "bbbb000000000002": "CCC", "dddd000000000004": "BBB"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("home regions = %v, want %v", got, want)
	}
}

// Among verified direct regions the plurality wins, ties go to the
// lexically smallest code (unchanged #804 rule).
func TestNodeHomeRegions488_VerifiedPluralityAndTie(t *testing.T) {
	db := rp488HomeDB(t)
	f := "1100" + rp488HomePayload
	rp488Advert(t, db, "aaaa000000000001", f, rp488Hear{1, "[]", f}, rp488Hear{3, "[]", f}, rp488Hear{4, "[]", f})
	rp488Advert(t, db, "bbbb000000000002", f, rp488Hear{3, "[]", f}, rp488Hear{2, "[]", f})
	got := rp488Homes(t, db)
	want := map[string]string{"aaaa000000000001": "CCC", "bbbb000000000002": "BBB"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("home regions = %v, want %v", got, want)
	}
}

// Every path that materialises an observation records its frame's advert
// class: cold Load, chunked load, loadChunk, IngestNewFromDB,
// IngestNewObservations. advertEv.code is 0 without a frame, else 1+kind;
// advertEv.hops keeps a relayed frame's hop count (#488 M2, review N2).
func TestAdvertEvidence_SetOnEveryObservationPath_488(t *testing.T) {
	snapshot := func(s *PacketStore) map[int]advertEvidence {
		s.mu.RLock()
		defer s.mu.RUnlock()
		out := map[int]advertEvidence{}
		for _, tx := range s.packets {
			for _, o := range tx.Observations {
				out[o.ID] = o.advertEv
			}
		}
		return out
	}
	ev := func(k packetpath.AdvertKind) advertEvidence { return advertEvidence{code: uint8(k) + 1} }
	db := routeMaskServerDB(t)
	now := time.Now().UTC()
	fs := now.Add(-time.Hour).Format(time.RFC3339)
	ts := now.Add(-time.Hour).Unix()
	rmInsertTx(t, db, 1, "mixed", 3, 0b1010, fs)
	rmInsertObs(t, db, 1, 1, 1, `[]`, "1100aabb", ts)
	rmInsertObs(t, db, 2, 1, 2, `[]`, "130000000000aabb", ts)
	rmInsertTx(t, db, 2, "flood", 1, 0b0010, fs)
	rmInsertObs(t, db, 3, 2, 1, `["A1"]`, "1101a1aabb", ts)
	rmInsertTx(t, db, 3, "legacy", 2, nil, fs)
	rmInsertObs(t, db, 4, 3, 2, `[]`, "", ts)
	relayed := func(hops uint8) advertEvidence {
		return advertEvidence{code: uint8(packetpath.AdvertRelayed) + 1, hops: hops}
	}
	want := map[int]advertEvidence{1: ev(packetpath.AdvertDirect), 2: ev(packetpath.AdvertShared), 3: relayed(1), 4: {}}

	load := NewPacketStore(db, nil)
	if err := load.Load(); err != nil {
		t.Fatal(err)
	}
	chunked := NewPacketStore(db, nil)
	if err := chunked.LoadChunked(1); err != nil {
		t.Fatal(err)
	}
	window := NewPacketStore(db, nil)
	if err := window.loadChunk(time.Now().Add(-48*time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]*PacketStore{"Load": load, "LoadChunked": chunked, "loadChunk": window} {
		if got := snapshot(s); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: advertEv %v, want %v", name, got, want)
		}
	}
	live := now.Add(-30 * time.Minute)
	rmInsertTx(t, db, 4, "new-tx", 0, 0b0001, live.Format(time.RFC3339))
	rmInsertObs(t, db, 5, 4, 1, `[]`, "101a2b3c4d00aabb", live.Unix())
	load.IngestNewFromDB(3, 100)
	rmInsertObs(t, db, 6, 2, 2, `["A1","A2"]`, "1202a1a2aabb", live.Unix())
	rmInsertObs(t, db, 7, 2, 1, `[]`, "", live.Unix())
	rmInsertObs(t, db, 8, 2, 2, `["A1","A2","A3"]`, "1103a1a2a3aabb", live.Unix())
	load.IngestNewObservations(5, 100)
	want[5], want[6], want[7], want[8] = ev(packetpath.AdvertDirect), ev(packetpath.AdvertUnknown), advertEvidence{}, relayed(3)
	if got := snapshot(load); !reflect.DeepEqual(got, want) {
		t.Fatalf("live ingest: advertEv %v, want %v", got, want)
	}
}
