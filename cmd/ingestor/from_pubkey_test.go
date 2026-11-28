package main

// Tests for #1143: ingestor must populate transmissions.from_pubkey at
// write time (cheap — already parsing decoded_json) so attribution queries
// don't rely on JSON substring matches.

import (
	"database/sql"
	"testing"
)

func TestInsertTransmission_FromPubkeyPopulatedForAdvert(t *testing.T) {
	s, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const pk = "f7181c468dfe7c55aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	data := &PacketData{
		RawHex:         "AABBCC",
		Timestamp:      "2026-03-25T00:00:00Z",
		ObserverID:     "obs1",
		Hash:           "advert_hash_1143",
		RouteType:      1,
		PayloadType:    4, // ADVERT
		PayloadVersion: 0,
		PathJSON:       "[]",
		DecodedJSON:    `{"type":"ADVERT","pubKey":"` + pk + `","name":"X"}`,
		FromPubkey:     pk,
	}
	if _, err := s.InsertTransmission(data); err != nil {
		t.Fatal(err)
	}

	var got sql.NullString
	s.db.QueryRow("SELECT from_pubkey FROM transmissions WHERE hash = ?", data.Hash).Scan(&got)
	if !got.Valid || got.String != pk {
		t.Fatalf("from_pubkey = %v (valid=%v), want %q", got.String, got.Valid, pk)
	}
}

func TestInsertTransmission_FromPubkeyNullForNonAdvert(t *testing.T) {
	s, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	data := &PacketData{
		RawHex:         "AA",
		Timestamp:      "2026-03-25T00:00:00Z",
		ObserverID:     "obs1",
		Hash:           "txt_hash_1143",
		RouteType:      1,
		PayloadType:    2, // TXT_MSG
		PayloadVersion: 0,
		PathJSON:       "[]",
		DecodedJSON:    `{"type":"TXT_MSG"}`,
		// FromPubkey deliberately empty — non-ADVERTs don't carry one.
	}
	if _, err := s.InsertTransmission(data); err != nil {
		t.Fatal(err)
	}

	var got sql.NullString
	s.db.QueryRow("SELECT from_pubkey FROM transmissions WHERE hash = ?", data.Hash).Scan(&got)
	if got.Valid {
		t.Fatalf("from_pubkey for non-ADVERT must be NULL, got %q", got.String)
	}
}

func TestBuildPacketData_PopulatesFromPubkey(t *testing.T) {
	const pk = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	msg := &MQTTPacketMessage{Raw: "AA", Origin: "obs"}
	decoded := &DecodedPacket{
		Header:  Header{PayloadType: PayloadADVERT},
		Payload: Payload{Type: "ADVERT", PubKey: pk},
	}
	pd := BuildPacketData(msg, decoded, "obs", "", nil)
	if pd.FromPubkey != pk {
		t.Fatalf("BuildPacketData FromPubkey = %q, want %q", pd.FromPubkey, pk)
	}

	// Non-ADVERT: must not carry a pubkey.
	decoded2 := &DecodedPacket{
		Header:  Header{PayloadType: 2},
		Payload: Payload{Type: "TXT_MSG"},
	}
	pd2 := BuildPacketData(msg, decoded2, "obs", "", nil)
	if pd2.FromPubkey != "" {
		t.Fatalf("BuildPacketData FromPubkey for non-ADVERT = %q, want empty", pd2.FromPubkey)
	}
}

// The backfill is the second path that writes from_pubkey, for ADVERT rows
// ingested before #1143. The server's /api/nodes region filter matches on
// from_pubkey alone (PR #38), so a backfilled row must carry exactly the
// pubKey its decoded_json holds, or that node silently drops out of every
// region it was heard in. Rows without a pubkey get the "" sentinel, and
// non-ADVERTs are left alone.
func TestBackfillFromPubkey_CopiesDecodedPubKey_PR38(t *testing.T) {
	s, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const pk1 = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	const pk2 = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	rows := []struct {
		hash        string
		payloadType int
		decoded     string
	}{
		{"legacy-adv-1", 4, `{"type":"ADVERT","pubKey":"` + pk1 + `","name":"one"}`},
		{"legacy-adv-2", 4, `{"type":"ADVERT","pubKey":"` + pk2 + `","name":"two"}`},
		{"legacy-adv-nokey", 4, `{"type":"ADVERT","name":"no key"}`},
		{"legacy-adv-corrupt", 4, `NOT JSON`},
		{"legacy-txt", 2, `{"type":"TXT_MSG","pubKey":"` + pk1 + `"}`},
	}
	for _, r := range rows {
		if _, err := s.db.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, payload_version, decoded_json)
			VALUES ('00', ?, '2026-01-01T00:00:00Z', 1, ?, 0, ?)`, r.hash, r.payloadType, r.decoded); err != nil {
			t.Fatal(err)
		}
	}

	// A chunk of 2 makes the 4 ADVERTs span several batches.
	s.BackfillFromPubkey(2, 0, nil)

	want := map[string]sql.NullString{
		"legacy-adv-1":       {String: pk1, Valid: true},
		"legacy-adv-2":       {String: pk2, Valid: true},
		"legacy-adv-nokey":   {String: "", Valid: true},
		"legacy-adv-corrupt": {String: "", Valid: true},
		"legacy-txt":         {},
	}
	for hash, w := range want {
		var got sql.NullString
		if err := s.db.QueryRow(`SELECT from_pubkey FROM transmissions WHERE hash = ?`, hash).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != w {
			t.Errorf("%s: from_pubkey = %q (valid=%v), want %q (valid=%v)", hash, got.String, got.Valid, w.String, w.Valid)
		}
	}
}
