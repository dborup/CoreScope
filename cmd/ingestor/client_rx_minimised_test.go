package main

import (
	"fmt"
	"testing"
	"time"
)

// Minimised-raw-packet contract for the mobile client RX topic (issue #284).
//
// A privacy-conscious companion uploader may strip the payload from a
// non-advert packet before publishing to meshcore/client/{PUBLIC_KEY}/packets,
// because coverage only needs what the HARD RULE reads: the header, the
// transport codes (TRANSPORT_* routes) and the path. Adverts are sent whole —
// the heard key for a 0-hop advert IS the advertiser's pubkey, which lives in
// the payload.
//
// These tests pin that the real handleClientPacket path accepts the minimised
// form, so a future "payload required" check cannot silently drop coverage.
//
// Wire format (firmware/docs/packet_format.md, src/Packet.h, src/Packet.cpp):
//
//	[header][transport_codes(4, TRANSPORT_* only)][path_length][path][payload]
//
//	header      0bVVPPPPRR — RR route (PH_ROUTE_MASK 0x03), PPPP payload type
//	            (PH_TYPE_SHIFT 2), VV payload version (PH_VER_SHIFT 6)
//	path_length bits 0-5 hash_count (0-63), bits 6-7 hash_size-1 (0b11 reserved)
//	path        hash_count * hash_size bytes
//	payload     up to MAX_PACKET_PAYLOAD=184 bytes — ABSENT in a minimised packet

// Route types — firmware/src/Packet.h:14-17.
const (
	fwRouteTransportFlood  = 0x00
	fwRouteFlood           = 0x01
	fwRouteDirect          = 0x02
	fwRouteTransportDirect = 0x03
)

// Payload types — firmware/src/Packet.h:20-33.
const (
	fwPayloadAdvert = 0x04
	fwPayloadGrpTxt = 0x05
	fwPayloadTrace  = 0x09
)

// fwPayloadTypeShift mirrors firmware PH_TYPE_SHIFT (src/Packet.h:9).
const fwPayloadTypeShift = 2

// fwHeader builds the 1-byte header per firmware/src/Packet.h:8-12
// (getRouteType/getPayloadType/getPayloadVer). Payload version is v1 (0).
func fwHeader(routeType, payloadType int) string {
	return fmt.Sprintf("%02x", (payloadType<<fwPayloadTypeShift)|routeType)
}

// fwPathLen builds the path_length byte: bits 0-5 hash_count, bits 6-7
// hash_size-1 (firmware/docs/packet_format.md "Path Length Encoding").
func fwPathLen(hashSize, hashCount int) string {
	return fmt.Sprintf("%02x", ((hashSize-1)<<6)|hashCount)
}

// fwPacket assembles a raw packet hex string. transportCodes is the 4-byte hex
// for TRANSPORT_* routes (""), hops are the path hashes (all the same size),
// and payload is "" for a minimised packet.
func fwPacket(routeType, payloadType int, transportCodes string, hops []string, payload string) string {
	if len(hops) == 0 {
		return fwHeader(routeType, payloadType) + transportCodes + fwPathLen(1, 0) + payload
	}
	hashSize := len(hops[0]) / 2
	raw := fwHeader(routeType, payloadType) + transportCodes + fwPathLen(hashSize, len(hops))
	for _, h := range hops {
		raw += h
	}
	return raw + payload
}

// clientMsg builds the MQTT payload documented in docs/client-rx-coverage.md.
// rx_at is kept inside resolveRxTime's accepted window so it is stored verbatim.
func clientMsg(rawHex, rxAt string) map[string]interface{} {
	return map[string]interface{}{
		"raw": rawHex, "direction": "rx", "timestamp": rxAt,
		"origin": "MyMob", "SNR": -7.0, "RSSI": -92.0,
		"gps": map[string]interface{}{"lat": 51.05, "lon": 3.72, "acc_m": 8.0},
	}
}

func testRxAt() string {
	return time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
}

// A GRP_TXT payload is channel hash(1) + MAC(2) + ciphertext — firmware
// src/Packet.h:24. Only used for the "whole packet" controls.
const grpTxtPayload = "7f" + "1234" + "deadbeefdeadbeefdeadbeefdeadbeef"

// A whole 0-hop advert fixture: the advert payload starts with the advertiser's
// 32-byte pubkey (firmware src/Mesh.cpp createAdvert), which is the heard key.
const advertPayload = "D818206D3AAC152C8A91F89957E6D30CA51F36E28790228971C473B755F244F718754CF5EE4A2FD58D944466E42CDED140C66D0CC590183E32BAF40F112BE8F3F2BDF6012B4B2793C52F1D36F69EE054D9A05593286F78453E56C0EC4A3EB95DDA2A7543FCCC00B939CACC009278603902FC12BCF84B706120526F6F6620536F6C6172"

// readOneReception returns the single coverage row for a companion, or fails.
func readOneReception(t *testing.T, s *Store, rxPubkey string) (heardKey string, keylen int, src string) {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM client_receptions WHERE rx_pubkey=?`, rxPubkey).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 client_receptions row, got %d", n)
	}
	if err := s.db.QueryRow(`SELECT heard_key, heard_keylen, src FROM client_receptions WHERE rx_pubkey=?`, rxPubkey).
		Scan(&heardKey, &keylen, &src); err != nil {
		t.Fatal(err)
	}
	return heardKey, keylen, src
}

func countReceptions(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM client_receptions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestHandleClientPacketMinimisedNonAdvertAccepted is issue #284 case 1+2+3:
// a FLOOD / TRANSPORT_FLOOD non-advert with the payload removed must still
// yield one coverage row keyed on path[last], and the whole packet (control)
// must yield the very same key. A mutant that requires a payload for
// non-adverts fails the "minimised" rows here.
func TestHandleClientPacketMinimisedNonAdvertAccepted(t *testing.T) {
	const tc = "11223344" // transport_code_1 + transport_code_2, 2 bytes each

	for _, c := range []struct {
		name      string
		raw       string
		wantKey   string
		wantKeyLn int
	}{
		{"flood 2-byte path, payload removed",
			fwPacket(fwRouteFlood, fwPayloadGrpTxt, "", []string{"aabb", "ccdd"}, ""), "ccdd", 2},
		{"flood 3-byte path, payload removed",
			fwPacket(fwRouteFlood, fwPayloadGrpTxt, "", []string{"aabbcc", "ddeeff"}, ""), "ddeeff", 3},
		{"transport_flood 2-byte path, codes kept, payload removed",
			fwPacket(fwRouteTransportFlood, fwPayloadGrpTxt, tc, []string{"aabb", "ccdd"}, ""), "ccdd", 2},
		{"transport_flood 3-byte path, codes kept, payload removed",
			fwPacket(fwRouteTransportFlood, fwPayloadGrpTxt, tc, []string{"aabbcc", "ddeeff"}, ""), "ddeeff", 3},
		// Controls: the same packets whole must resolve to the same heard key.
		{"control: flood 2-byte path, whole",
			fwPacket(fwRouteFlood, fwPayloadGrpTxt, "", []string{"aabb", "ccdd"}, grpTxtPayload), "ccdd", 2},
		{"control: flood 3-byte path, whole",
			fwPacket(fwRouteFlood, fwPayloadGrpTxt, "", []string{"aabbcc", "ddeeff"}, grpTxtPayload), "ddeeff", 3},
		{"control: transport_flood 2-byte path, whole",
			fwPacket(fwRouteTransportFlood, fwPayloadGrpTxt, tc, []string{"aabb", "ccdd"}, grpTxtPayload), "ccdd", 2},
		{"control: transport_flood 3-byte path, whole",
			fwPacket(fwRouteTransportFlood, fwPayloadGrpTxt, tc, []string{"aabbcc", "ddeeff"}, grpTxtPayload), "ddeeff", 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newTestStore(t)
			handleClientPacket(s, "test", testCompanionPK, clientMsg(c.raw, testRxAt()), nil)
			key, keylen, src := readOneReception(t, s, testCompanionPK)
			if key != c.wantKey || keylen != c.wantKeyLn || src != "rxlog" {
				t.Fatalf("raw %s: got heard_key=%q keylen=%d src=%q, want %q/%d/rxlog",
					c.raw, key, keylen, src, c.wantKey, c.wantKeyLn)
			}
		})
	}
}

// TestHandleClientPacketMinimisedAndWholeAgree pins that stripping the payload
// changes nothing the HARD RULE reads: the minimised and the whole form of the
// same packet produce the identical heard key, and (idempotency) the same
// rx_at+heard_key collapses to one row.
func TestHandleClientPacketMinimisedAndWholeAgree(t *testing.T) {
	s := newTestStore(t)
	rxAt := testRxAt()
	hops := []string{"aabb", "ccdd"}
	minimised := fwPacket(fwRouteFlood, fwPayloadGrpTxt, "", hops, "")
	whole := fwPacket(fwRouteFlood, fwPayloadGrpTxt, "", hops, grpTxtPayload)

	handleClientPacket(s, "test", testCompanionPK, clientMsg(minimised, rxAt), nil)
	handleClientPacket(s, "test", testCompanionPK, clientMsg(whole, rxAt), nil)

	key, keylen, src := readOneReception(t, s, testCompanionPK)
	if key != "ccdd" || keylen != 2 || src != "rxlog" {
		t.Fatalf("minimised and whole must agree: got %q/%d/%q", key, keylen, src)
	}
}

// TestHandleClientPacketWholeAdvertsAccepted is issue #284 case 4: adverts are
// uploaded whole, because the 0-hop heard key is the advertiser's pubkey from
// the payload. A relayed advert still follows the HARD RULE (path[last]).
func TestHandleClientPacketWholeAdvertsAccepted(t *testing.T) {
	t.Run("0-hop advert, whole", func(t *testing.T) {
		s := newTestStore(t)
		raw := fwPacket(fwRouteFlood, fwPayloadAdvert, "", nil, advertPayload)
		handleClientPacket(s, "test", testCompanionPK, clientMsg(raw, testRxAt()), nil)
		key, keylen, src := readOneReception(t, s, testCompanionPK)
		wantKey := "d818206d3aac152c8a91f89957e6d30ca51f36e28790228971c473b755f244f7"
		if key != wantKey || keylen != 32 || src != "advert" {
			t.Fatalf("0-hop advert: got %q/%d/%q, want %q/32/advert", key, keylen, src, wantKey)
		}
	})
	t.Run("relayed advert, whole", func(t *testing.T) {
		s := newTestStore(t)
		raw := fwPacket(fwRouteFlood, fwPayloadAdvert, "", []string{"aabb", "ccdd"}, advertPayload)
		handleClientPacket(s, "test", testCompanionPK, clientMsg(raw, testRxAt()), nil)
		key, keylen, src := readOneReception(t, s, testCompanionPK)
		if key != "ccdd" || keylen != 2 || src != "rxlog" {
			t.Fatalf("relayed advert: got %q/%d/%q, want ccdd/2/rxlog", key, keylen, src)
		}
	})
}

// TestHandleClientPacketMinimisedNotAttributable is issue #284 case 5: a
// minimised packet does not loosen the HARD RULE. DIRECT routes consume the
// next hop from the FRONT (firmware Mesh.cpp removeSelfFromPath), so path[last]
// is not who was heard; TRACE repurposes the header path bytes as per-hop SNR.
// Neither may produce a row, with or without a payload.
func TestHandleClientPacketMinimisedNotAttributable(t *testing.T) {
	const tc = "11223344"
	for _, c := range []struct{ name, raw string }{
		{"direct, payload removed",
			fwPacket(fwRouteDirect, fwPayloadGrpTxt, "", []string{"aabb", "ccdd"}, "")},
		{"transport_direct, payload removed",
			fwPacket(fwRouteTransportDirect, fwPayloadGrpTxt, tc, []string{"aabb", "ccdd"}, "")},
		{"flood trace, payload removed",
			fwPacket(fwRouteFlood, fwPayloadTrace, "", []string{"aabb", "ccdd"}, "")},
		{"direct trace, payload removed",
			fwPacket(fwRouteDirect, fwPayloadTrace, "", []string{"aabb", "ccdd"}, "")},
		{"transport_flood trace, payload removed",
			fwPacket(fwRouteTransportFlood, fwPayloadTrace, tc, []string{"aabb", "ccdd"}, "")},
		// A non-advert with no path has nothing attributable left once the
		// payload is gone.
		{"flood non-advert, no path, payload removed",
			fwPacket(fwRouteFlood, fwPayloadGrpTxt, "", nil, "")},
		// 1-byte hashes stay excluded (collision-prone), minimised or not.
		{"flood 1-byte path, payload removed",
			fwPacket(fwRouteFlood, fwPayloadGrpTxt, "", []string{"aa", "bb"}, "")},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newTestStore(t)
			handleClientPacket(s, "test", testCompanionPK, clientMsg(c.raw, testRxAt()), nil)
			if n := countReceptions(t, s); n != 0 {
				t.Fatalf("raw %s must not be attributable, got %d rows", c.raw, n)
			}
		})
	}
}

// TestHandleClientPacketMalformedPathLenDropped is issue #284 case 6: a
// wire-supplied path_length byte may claim more path bytes than the (minimised)
// buffer holds. DecodePacket must reject it instead of slicing out of range —
// without the bounds check in DecodePacket this panics (see #1211).
func TestHandleClientPacketMalformedPathLenDropped(t *testing.T) {
	// path_length 0x4A = hash_size 2, hash_count 10 → 20 claimed path bytes.
	overclaim := fwHeader(fwRouteFlood, fwPayloadGrpTxt) + "4a" + "aabb"
	overclaimTransport := fwHeader(fwRouteTransportFlood, fwPayloadGrpTxt) + "11223344" + "4a" + "aabb"
	// hash_size code 0b11 is reserved (firmware Packet::isValidPathLen).
	reservedHashSize := fwHeader(fwRouteFlood, fwPayloadGrpTxt) + "c2" + "aabbccdd"

	for _, c := range []struct{ name, raw string }{
		{"path_length claims 20 bytes, 2 present", overclaim},
		{"path_length claims 20 bytes behind transport codes", overclaimTransport},
		{"reserved hash_size 4", reservedHashSize},
		{"header only, path byte removed too", fwHeader(fwRouteFlood, fwPayloadGrpTxt)},
		{"path byte present, path bytes missing entirely",
			fwHeader(fwRouteFlood, fwPayloadGrpTxt) + "42"},
		{"transport codes truncated", fwHeader(fwRouteTransportFlood, fwPayloadGrpTxt) + "1122"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newTestStore(t)
			// A panic here fails the test (the ingestor's own recover() is in
			// the MQTT handler, one frame above handleClientPacket).
			handleClientPacket(s, "test", testCompanionPK, clientMsg(c.raw, testRxAt()), nil)
			if n := countReceptions(t, s); n != 0 {
				t.Fatalf("malformed raw %s must be dropped, got %d rows", c.raw, n)
			}
		})
	}
}

// TestDecodePacketMinimisedKeepsHeaderPathContract pins the decoder contract the
// client path relies on, and that it is unchanged for whole packets: a minimised
// packet decodes to the same header/transport/path as its whole twin, reports the
// payload as unusable rather than failing the packet, and a whole packet still
// decodes its payload. The observer path is not involved.
func TestDecodePacketMinimisedKeepsHeaderPathContract(t *testing.T) {
	const tc = "11223344"
	hops := []string{"aabbcc", "ddeeff"}
	minimised := fwPacket(fwRouteTransportFlood, fwPayloadGrpTxt, tc, hops, "")
	whole := fwPacket(fwRouteTransportFlood, fwPayloadGrpTxt, tc, hops, grpTxtPayload)

	dm, err := DecodePacket(minimised, nil, false)
	if err != nil {
		t.Fatalf("minimised packet must decode, got %v", err)
	}
	dw, err := DecodePacket(whole, nil, false)
	if err != nil {
		t.Fatalf("whole packet must decode, got %v", err)
	}
	if dm.Header != dw.Header {
		t.Fatalf("header differs: minimised %+v vs whole %+v", dm.Header, dw.Header)
	}
	if dm.TransportCodes == nil || dw.TransportCodes == nil || *dm.TransportCodes != *dw.TransportCodes {
		t.Fatalf("transport codes differ: %+v vs %+v", dm.TransportCodes, dw.TransportCodes)
	}
	if dm.Path.HashSize != 3 || dm.Path.HashCount != 2 || len(dm.Path.Hops) != 2 || dm.Path.Hops[1] != "DDEEFF" {
		t.Fatalf("minimised path not decoded: %+v", dm.Path)
	}
	if dm.Path.HashSize != dw.Path.HashSize || dm.Path.HashCount != dw.Path.HashCount || dm.Path.Hops[1] != dw.Path.Hops[1] {
		t.Fatalf("path differs: minimised %+v vs whole %+v", dm.Path, dw.Path)
	}
	// The absent payload is reported, not fatal — that is what makes the
	// minimised form ingestible.
	if dm.Payload.Error == "" {
		t.Fatalf("minimised payload should be flagged as unusable, got %+v", dm.Payload)
	}
	// Full-packet decoding is untouched: the GRP_TXT envelope still parses.
	if dw.Payload.Error != "" || dw.Payload.ChannelHashHex != "7F" || dw.Payload.MAC != "1234" {
		t.Fatalf("whole GRP_TXT payload must still decode: %+v", dw.Payload)
	}
}
