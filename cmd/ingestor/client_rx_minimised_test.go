package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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

// Payload types — firmware/src/Packet.h:19-32.
const (
	fwPayloadAdvert = 0x04
	fwPayloadGrpTxt = 0x05
	fwPayloadTrace  = 0x09
)

// fwPayloadTypeShift mirrors firmware PH_TYPE_SHIFT (src/Packet.h:9).
const fwPayloadTypeShift = 2

// fwHeader builds the 1-byte header per the mask/shift defines on
// firmware/src/Packet.h:8-12 (PH_ROUTE_MASK, PH_TYPE_SHIFT, PH_VER_SHIFT, read
// back by Packet::getRouteType/getPayloadType/getPayloadVer). Version is v1 (0).
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
	return testRxAtAgo(time.Minute)
}

// testRxAtAgo returns an rx_at the given duration in the past. Callers that need
// two packets to land in two distinct rows pick two distinct ages: the
// UNIQUE(rx_pubkey, heard_key, rx_at) index collapses them otherwise.
func testRxAtAgo(ago time.Duration) string {
	return time.Now().UTC().Add(-ago).Format(time.RFC3339)
}

// A GRP_TXT payload is channel hash(1) + MAC(2) + ciphertext — firmware
// src/Packet.h:24. Only used for the "whole packet" controls.
const grpTxtPayload = "7f" + "1234" + "deadbeefdeadbeefdeadbeefdeadbeef"

// A whole 0-hop advert fixture: the advert payload starts with the advertiser's
// 32-byte pubkey (firmware src/Mesh.cpp createAdvert), which is the heard key.
// advertPubkeyHex is the lowercased pubkey that advertPayload carries, i.e. the
// heard key of a 0-hop advert.
const advertPubkeyHex = "d818206d3aac152c8a91f89957e6d30ca51f36e28790228971c473b755f244f7"

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

// clientRxRow is one client_receptions row as the coverage tests read it.
type clientRxRow struct {
	heardKey string
	keylen   int
	src      string
	rxAt     string
}

// receptionsFor returns every coverage row for a companion, oldest rx_at first.
// Reading the rows individually (rather than only counting them) is what lets a
// test assert the row a single call wrote, instead of a total that a later call
// could also have produced on its own (#305).
func receptionsFor(t *testing.T, s *Store, rxPubkey string) []clientRxRow {
	t.Helper()
	rows, err := s.db.Query(
		`SELECT heard_key, heard_keylen, src, rx_at FROM client_receptions WHERE rx_pubkey=? ORDER BY rx_at`,
		rxPubkey)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []clientRxRow
	for rows.Next() {
		var r clientRxRow
		if err := rows.Scan(&r.heardKey, &r.keylen, &r.src, &r.rxAt); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
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
// changes nothing the HARD RULE reads. Issue #305 point 1: the minimised packet
// is asserted ON ITS OWN, under its own rx_at, before the whole twin is sent at
// all. The earlier shape sent both at the same rx_at and only counted rows, so
// the whole packet alone produced the one expected row and every "payload
// required" mutant stayed green.
func TestHandleClientPacketMinimisedAndWholeAgree(t *testing.T) {
	s := newTestStore(t)
	// Two distinct rx_at values: at one rx_at the UNIQUE(rx_pubkey, heard_key,
	// rx_at) index would merge the two calls into a single row, and then either
	// call could satisfy the assertion by itself.
	rxAtMinimised := testRxAtAgo(3 * time.Minute)
	rxAtWhole := testRxAtAgo(2 * time.Minute)
	hops := []string{"aabb", "ccdd"}
	minimised := fwPacket(fwRouteFlood, fwPayloadGrpTxt, "", hops, "")
	whole := fwPacket(fwRouteFlood, fwPayloadGrpTxt, "", hops, grpTxtPayload)

	// 1. The minimised packet, alone, must write its own row. A check that drops
	//    a payload-less non-advert fails right here.
	handleClientPacket(s, "test", testCompanionPK, clientMsg(minimised, rxAtMinimised), nil)
	got := receptionsFor(t, s, testCompanionPK)
	wantMinimised := clientRxRow{heardKey: "ccdd", keylen: 2, src: "rxlog", rxAt: rxAtMinimised}
	if len(got) != 1 || got[0] != wantMinimised {
		t.Fatalf("the minimised packet alone must write exactly %+v, got %d row(s): %+v",
			wantMinimised, len(got), got)
	}

	// 2. The whole twin, at its own rx_at, adds a second row with the same key.
	handleClientPacket(s, "test", testCompanionPK, clientMsg(whole, rxAtWhole), nil)
	got = receptionsFor(t, s, testCompanionPK)
	wantWhole := clientRxRow{heardKey: "ccdd", keylen: 2, src: "rxlog", rxAt: rxAtWhole}
	if len(got) != 2 || got[1] != wantWhole {
		t.Fatalf("the whole packet must add exactly %+v, got %d row(s): %+v",
			wantWhole, len(got), got)
	}
	if got[0].heardKey != got[1].heardKey || got[0].keylen != got[1].keylen || got[0].src != got[1].src {
		t.Fatalf("minimised and whole must agree on the heard node: %+v vs %+v", got[0], got[1])
	}

	// 3. Idempotency, asserted separately now that it no longer doubles as the
	//    agreement check: the whole packet re-sent at the minimised packet's
	//    rx_at collapses onto the existing row (ON CONFLICT DO NOTHING).
	handleClientPacket(s, "test", testCompanionPK, clientMsg(whole, rxAtMinimised), nil)
	if got = receptionsFor(t, s, testCompanionPK); len(got) != 2 {
		t.Fatalf("re-sending at an existing rx_at must not add a row, got %d: %+v", len(got), got)
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
		if key != advertPubkeyHex || keylen != 32 || src != "advert" {
			t.Fatalf("0-hop advert: got %q/%d/%q, want %q/32/advert", key, keylen, src, advertPubkeyHex)
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

// ---------------------------------------------------------------------------
// Issue #305 point 2: the three claims docs/client-rx-coverage.md makes about
// minimised raw packets, one test each. Every claim was re-checked against the
// firmware source before being pinned.
// ---------------------------------------------------------------------------

// TestHandleClientPacketMinimisedZeroHopAdvertDropped pins claim (a): a
// minimised 0-hop advert is dropped.
//
// Why it must be: firmware Mesh::createAdvert (src/Mesh.cpp) writes the
// advertiser's 32-byte pub_key as the FIRST payload field, followed by a 4-byte
// timestamp and a 64-byte signature (firmware docs/payloads.md, "Node
// advertisement"). A 0-hop advert has no path, so the only thing identifying the
// node that was heard lives in the payload — remove it and nothing attributable
// is left.
//
// Both on-wire 0-hop forms are covered: Mesh::sendZeroHop sets
// ROUTE_TYPE_DIRECT with path_len 0, while the first hop of a flooded advert is
// ROUTE_TYPE_FLOOD with path_len 0 (Mesh::sendFlood).
func TestHandleClientPacketMinimisedZeroHopAdvertDropped(t *testing.T) {
	for _, c := range []struct{ name, raw string }{
		{"zero-hop advert (DIRECT, sendZeroHop), payload removed",
			fwPacket(fwRouteDirect, fwPayloadAdvert, "", nil, "")},
		{"flooded advert with no hops yet (FLOOD), payload removed",
			fwPacket(fwRouteFlood, fwPayloadAdvert, "", nil, "")},
		// Keeping only the pubkey does not rescue it either: the advert payload
		// is pub_key(32) + timestamp(4) + signature(64), and decodeAdvert reports
		// a pubkey only once all 100 bytes are present.
		{"zero-hop advert truncated to the 32-byte pubkey",
			fwPacket(fwRouteDirect, fwPayloadAdvert, "", nil, advertPayload[:64])},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newTestStore(t)
			handleClientPacket(s, "test", testCompanionPK, clientMsg(c.raw, testRxAt()), nil)
			if n := countReceptions(t, s); n != 0 {
				t.Fatalf("minimised 0-hop advert %s must be dropped, got %d row(s)", c.raw, n)
			}
		})
	}
	// Control: the same advert sent whole IS accepted, so the subtests above are
	// asserting the missing payload rather than a broken fixture.
	t.Run("control: the same 0-hop advert whole is accepted", func(t *testing.T) {
		s := newTestStore(t)
		raw := fwPacket(fwRouteDirect, fwPayloadAdvert, "", nil, advertPayload)
		handleClientPacket(s, "test", testCompanionPK, clientMsg(raw, testRxAt()), nil)
		key, keylen, src := readOneReception(t, s, testCompanionPK)
		if key != advertPubkeyHex || keylen != 32 || src != "advert" {
			t.Fatalf("whole 0-hop advert: got %q/%d/%q, want %q/32/advert",
				key, keylen, src, advertPubkeyHex)
		}
	})
}

// TestHandleClientPacketMinimisedRelayedAdvertSurvives pins claim (b): a
// minimised RELAYED advert survives.
//
// Why it must be: once an advert has been forwarded it carries a path, and a
// FLOOD forwarder APPENDS its own hash to the end of that path — firmware
// Mesh::routeRecvPacket (src/Mesh.cpp) writes self_id's hash at
// path[hash_count * hash_size] and then bumps hash_count. So path[last] is the
// node that physically transmitted, and it is readable without the payload.
// Note src is "rxlog", not "advert": the key comes from the path, so the
// advertiser's own pubkey never enters into it.
func TestHandleClientPacketMinimisedRelayedAdvertSurvives(t *testing.T) {
	for _, c := range []struct {
		name, raw, wantKey string
		wantKeyLn          int
	}{
		{"relayed advert, 2-byte path, payload removed",
			fwPacket(fwRouteFlood, fwPayloadAdvert, "", []string{"aabb", "ccdd"}, ""), "ccdd", 2},
		{"relayed advert, 3-byte path, payload removed",
			fwPacket(fwRouteFlood, fwPayloadAdvert, "", []string{"aabbcc", "ddeeff"}, ""), "ddeeff", 3},
		{"relayed advert behind transport codes, payload removed",
			fwPacket(fwRouteTransportFlood, fwPayloadAdvert, "11223344", []string{"aabb", "ccdd"}, ""), "ccdd", 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newTestStore(t)
			handleClientPacket(s, "test", testCompanionPK, clientMsg(c.raw, testRxAt()), nil)
			key, keylen, src := readOneReception(t, s, testCompanionPK)
			if key != c.wantKey || keylen != c.wantKeyLn || src != "rxlog" {
				t.Fatalf("minimised relayed advert %s: got %q/%d/%q, want %q/%d/rxlog",
					c.raw, key, keylen, src, c.wantKey, c.wantKeyLn)
			}
		})
	}
}

// TestHandleClientPacketPartialPayloadTolerated pins claim (c): a partially kept
// payload is tolerated.
//
// Tolerated means the packet is still ingested and still attributed to
// path[last] — the remnant changes nothing about the coverage row. What it
// decodes to does depend on how much is left: a group text payload is channel
// hash (1) + cipher MAC (2) + ciphertext (firmware docs/payloads.md, "Group
// text message"), so only a remnant SHORTER than that 3-byte envelope is
// reported as undecodable; a 3-byte remnant still yields the channel hash and
// the MAC. That is why there is no point in keeping a partial payload: it can
// leak the channel hash and MAC without adding anything to coverage.
func TestHandleClientPacketPartialPayloadTolerated(t *testing.T) {
	hops := []string{"aabb", "ccdd"}
	for _, c := range []struct {
		name, payload    string
		wantPayloadError bool
	}{
		{"payload cut inside the channel hash + MAC envelope", "7f12", true},
		{"payload cut to exactly the channel hash + MAC", "7f1234", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw := fwPacket(fwRouteFlood, fwPayloadGrpTxt, "", hops, c.payload)
			s := newTestStore(t)
			handleClientPacket(s, "test", testCompanionPK, clientMsg(raw, testRxAt()), nil)
			key, keylen, src := readOneReception(t, s, testCompanionPK)
			if key != "ccdd" || keylen != 2 || src != "rxlog" {
				t.Fatalf("partial payload %q: got %q/%d/%q, want ccdd/2/rxlog — the same row the\n"+
					"fully minimised packet writes", c.payload, key, keylen, src)
			}
			// ...and the decoder's view of the remnant, which is the part of the
			// claim the doc had to be corrected on.
			d, err := DecodePacket(raw, nil, false)
			if err != nil {
				t.Fatalf("partial payload %q must still decode: %v", c.payload, err)
			}
			if c.wantPayloadError {
				if d.Payload.Error == "" {
					t.Fatalf("payload %q is shorter than the envelope, so it must be reported as\n"+
						"unusable, got %+v", c.payload, d.Payload)
				}
				return
			}
			if d.Payload.Error != "" || d.Payload.ChannelHashHex != "7F" || d.Payload.MAC != "1234" {
				t.Fatalf("payload %q is a complete envelope, so the channel hash and MAC must still\n"+
					"decode (that is the leak the doc warns about), got %+v", c.payload, d.Payload)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Issue #305 point 3: keep the firmware line citations in this file honest.
// ---------------------------------------------------------------------------

// fwLineRefs are the firmware citations the comments in THIS file make, with a
// pattern that the first and the last line of each cited range must match. Two
// things drift independently — the line numbers in the comments, and what those
// lines actually say — so both halves are checked below. The PAYLOAD_TYPE_*
// range is the one #305 point 3 reports: it was cited as 20-33 while the defines
// sit on 19-32.
var fwLineRefs = []struct {
	file     string // path inside the firmware checkout
	from, to int    // the cited line range, inclusive (from == to for a single line)
	firstRe  string // the first line of the range must match this
	lastRe   string // ...and the last line this
}{
	{"src/Packet.h", 8, 12, `^#define PH_ROUTE_MASK\b`, `^#define PH_VER_MASK\b`},
	{"src/Packet.h", 9, 9, `^#define PH_TYPE_SHIFT\b`, `^#define PH_TYPE_SHIFT\b`},
	{"src/Packet.h", 14, 17, `^#define ROUTE_TYPE_TRANSPORT_FLOOD\b`, `^#define ROUTE_TYPE_TRANSPORT_DIRECT\b`},
	{"src/Packet.h", 19, 32, `^#define PAYLOAD_TYPE_REQ\b`, `^#define PAYLOAD_TYPE_RAW_CUSTOM\b`},
	{"src/Packet.h", 24, 24, `^#define PAYLOAD_TYPE_GRP_TXT\b`, `^#define PAYLOAD_TYPE_GRP_TXT\b`},
}

// fwCiteRe matches a firmware citation in a comment: "src/Packet.h:19-32",
// "firmware/src/Packet.h:9".
var fwCiteRe = regexp.MustCompile(`(?:firmware/)?((?:src|docs)/[A-Za-z0-9_.]+\.(?:h|cpp|md)):(\d+)(?:-(\d+))?`)

func fwCiteString(file string, from, to int) string {
	if from == to {
		return fmt.Sprintf("%s:%d", file, from)
	}
	return fmt.Sprintf("%s:%d-%d", file, from, to)
}

// TestFirmwareLineCitationsAreAccurate checks the firmware line references in
// this file's comments. The first half (comments agree with fwLineRefs) needs
// nothing but the source and so runs everywhere, CI included. The second half
// reads the cited lines and needs the firmware checkout, which AGENTS.md keeps
// at firmware/ and gitignores — absent there, it skips.
func TestFirmwareLineCitationsAreAccurate(t *testing.T) {
	const self = "client_rx_minimised_test.go"
	srcBytes, err := os.ReadFile(self)
	if err != nil {
		t.Fatalf("reading %s: %v", self, err)
	}
	src := string(srcBytes)

	// Every citation the comments make must be one the table knows about, so a
	// range nobody checked cannot slip in.
	for _, m := range fwCiteRe.FindAllStringSubmatch(src, -1) {
		from, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatalf("citation %q: %v", m[0], err)
		}
		to := from
		if m[3] != "" {
			if to, err = strconv.Atoi(m[3]); err != nil {
				t.Fatalf("citation %q: %v", m[0], err)
			}
		}
		found := false
		for _, r := range fwLineRefs {
			if r.file == m[1] && r.from == from && r.to == to {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s cites %s, which fwLineRefs does not cover — add it together with the\n"+
				"patterns those lines must match, or fix the citation", self, m[0])
		}
	}
	// ...and conversely, the table may not keep checking a reference the comments
	// no longer make.
	for _, r := range fwLineRefs {
		if cite := fwCiteString(r.file, r.from, r.to); !strings.Contains(src, cite) {
			t.Errorf("fwLineRefs covers %s but no comment in %s cites it", cite, self)
		}
	}

	root := filepath.Join("..", "..", "firmware")
	if _, err := os.Stat(filepath.Join(root, "src", "Packet.h")); err != nil {
		t.Skip("firmware/ checkout absent; clone it per AGENTS.md to check the cited lines " +
			"(git clone --depth 1 https://github.com/meshcore-dev/MeshCore.git firmware)")
	}
	cache := map[string][]string{}
	for _, r := range fwLineRefs {
		lines, ok := cache[r.file]
		if !ok {
			b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(r.file)))
			if err != nil {
				t.Fatalf("reading firmware %s: %v", r.file, err)
			}
			lines = strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
			cache[r.file] = lines
		}
		cite := fwCiteString(r.file, r.from, r.to)
		if r.from < 1 || r.to < r.from || r.to > len(lines) {
			t.Errorf("%s is out of range: %s has %d lines", cite, r.file, len(lines))
			continue
		}
		for _, check := range []struct {
			what string
			line string
			pat  string
		}{
			{"first", lines[r.from-1], r.firstRe},
			{"last", lines[r.to-1], r.lastRe},
		} {
			if !regexp.MustCompile(check.pat).MatchString(check.line) {
				t.Errorf("%s: %s line does not match %s — got %q", cite, check.what, check.pat, check.line)
			}
		}
	}
}
