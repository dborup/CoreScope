package packetpath

import (
	"encoding/json"
	"strings"
)

// PayloadADVERT is the payload type of a node advertisement (firmware
// src/Packet.h PAYLOAD_TYPE_ADVERT).
const PayloadADVERT = 0x04

// Frame limits from firmware src/MeshCore.h.
const (
	maxPathSize      = 64  // MAX_PATH_SIZE
	maxPacketPayload = 184 // MAX_PACKET_PAYLOAD
)

// AdvertKind is what one ADVERT observation says about where its originator
// is (#488).
type AdvertKind uint8

const (
	// AdvertUnknown: no usable hop count. A missing or malformed path, an
	// undecodable frame, a DIRECT route with a path (its length is the hops
	// still to go, Mesh.cpp removeSelfFromPath), a TRANSPORT_DIRECT advert
	// that is not a contact share (no firmware sender), or not an ADVERT.
	AdvertUnknown AdvertKind = iota
	// AdvertDirect: heard from the originator's own transmission. Adverts
	// leave the originator with hash count 0 (sendFlood/sendFloodScoped on
	// FLOOD/TRANSPORT_FLOOD, sendZeroHop on DIRECT) and every relay appends
	// one hash (Mesh.cpp routeRecvPacket).
	AdvertDirect
	// AdvertRelayed: a flood advert heard after Hops relays.
	AdvertRelayed
	// AdvertShared: another node's advert re-sent zero-hop by a companion
	// (BaseChatMesh::shareContactZeroHop: TRANSPORT_DIRECT, codes {0,0},
	// path_len 0). It locates the sharer, not the advertised node.
	AdvertShared
)

// AdvertHearing classifies one ADVERT observation.
type AdvertHearing struct {
	Kind AdvertKind
	Hops int // relays, for AdvertRelayed
	// Verified is true when the observation's own frame was classified,
	// false when only its path_json was available. On such a legacy row an
	// explicit '[]' cannot be told apart from a contact re-share, whose
	// path_json is '[]' too.
	Verified bool
}

// ClassifyAdvert classifies one ADVERT observation by its own frame
// (observations.raw_hex) when one is stored, else by its path_json. The
// frame wins because path_json '[]' is written both for zero hops and as the
// ingestor backfill's "attempted, decode failed" sentinel, and because a
// contact re-share is another observation of the original content hash
// (route bits are not hashed) with path_json '[]'.
func ClassifyAdvert(obsRawHex, pathJSON string) AdvertHearing {
	if strings.TrimSpace(obsRawHex) != "" {
		return ClassifyAdvertFrame(obsRawHex)
	}
	return ClassifyAdvertPathJSON(pathJSON)
}

// ClassifyAdvertFrame classifies a hex-encoded ADVERT frame. It reads the
// path byte through PathByte (offset 5 behind the transport codes on
// TRANSPORT_FLOOD/TRANSPORT_DIRECT) and rejects what Packet::readFrom
// rejects: the reserved hash size and hash_count*hash_size > MAX_PATH_SIZE
// (Packet::isValidPathLen), a path running past the frame, an empty payload,
// and a payload over MAX_PACKET_PAYLOAD. The result is always Verified. It
// does not allocate.
func ClassifyAdvertFrame(rawHex string) AdvertHearing {
	rawHex = strings.TrimSpace(rawHex)
	unknown := AdvertHearing{Kind: AdvertUnknown, Verified: true}
	header, ok := hexByteAt(rawHex, 0)
	if !ok || header>>2&0x0f != PayloadADVERT || !isHex(rawHex) {
		return unknown
	}
	route, pathByte, ok := PathByte(rawHex)
	if !ok {
		return unknown
	}
	size := int(pathByte>>6) + 1
	count := int(pathByte & 0x3f)
	if size == 4 || count*size > maxPathSize {
		return unknown
	}
	pathStart := 2
	if IsTransportRoute(route) {
		pathStart += 4
	}
	payload := len(rawHex)/2 - pathStart - count*size
	if payload < 1 || payload > maxPacketPayload {
		return unknown
	}
	switch route {
	case RouteFlood, RouteTransportFlood:
		if count == 0 {
			return AdvertHearing{Kind: AdvertDirect, Verified: true}
		}
		return AdvertHearing{Kind: AdvertRelayed, Hops: count, Verified: true}
	case RouteDirect:
		if count == 0 {
			return AdvertHearing{Kind: AdvertDirect, Verified: true}
		}
		return unknown
	default: // RouteTransportDirect
		if count == 0 && transportCodesZero(rawHex) {
			return AdvertHearing{Kind: AdvertShared, Verified: true}
		}
		return unknown
	}
}

// ClassifyAdvertPathJSON classifies an observation that has no frame of its
// own. A missing, empty, JSON-null or malformed path is unknown, not zero
// hops. The result is never Verified.
func ClassifyAdvertPathJSON(pathJSON string) AdvertHearing {
	s := strings.TrimSpace(pathJSON)
	if s == "[]" {
		return AdvertHearing{Kind: AdvertDirect}
	}
	if s == "" || s == "null" {
		return AdvertHearing{Kind: AdvertUnknown}
	}
	var hops []string
	if json.Unmarshal([]byte(s), &hops) != nil || hops == nil {
		return AdvertHearing{Kind: AdvertUnknown}
	}
	if len(hops) == 0 {
		return AdvertHearing{Kind: AdvertDirect}
	}
	return AdvertHearing{Kind: AdvertRelayed, Hops: len(hops)}
}

// transportCodesZero reports whether both 2-byte transport codes (bytes 1-4)
// are zero, the {0,0} "send this nowhere" codes of a contact share. The
// caller has checked that the frame is hex and long enough.
func transportCodesZero(rawHex string) bool {
	return strings.Trim(rawHex[2:10], "0") == ""
}

// isHex reports whether s is an even-length hex string.
func isHex(s string) bool {
	if len(s)%2 != 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if _, ok := hexNibble(s[i]); !ok {
			return false
		}
	}
	return true
}
