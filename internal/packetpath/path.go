// Package packetpath provides shared helpers for extracting path hops from
// raw MeshCore packet hex bytes.
package packetpath

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// DecodePathFromRawHex extracts the header path hops directly from raw hex bytes.
// This is the authoritative path that matches what's in raw_hex, as opposed to
// decoded.Path.Hops which may be overwritten for TRACE packets (issue #886).
//
// WARNING: This function returns the literal header path bytes regardless of
// payload type. For TRACE packets these bytes are SNR values, NOT hop hashes.
// Callers that may receive TRACE packets MUST check PathBytesAreHops(payloadType)
// first, or use the safer DecodeHopsForPayload wrapper.
func DecodePathFromRawHex(rawHex string) ([]string, error) {
	buf, err := hex.DecodeString(rawHex)
	if err != nil || len(buf) < 2 {
		return nil, fmt.Errorf("invalid or too-short hex")
	}

	headerByte := buf[0]
	offset := 1
	if IsTransportRoute(int(headerByte & 0x03)) {
		if len(buf) < offset+4 {
			return nil, fmt.Errorf("too short for transport codes")
		}
		offset += 4
	}
	if offset >= len(buf) {
		return nil, fmt.Errorf("too short for path byte")
	}

	pathByte := buf[offset]
	offset++

	hashSize := int(pathByte>>6) + 1
	hashCount := int(pathByte & 0x3F)

	hops := make([]string, 0, hashCount)
	for i := 0; i < hashCount; i++ {
		start := offset + i*hashSize
		end := start + hashSize
		if end > len(buf) {
			break
		}
		hops = append(hops, strings.ToUpper(hex.EncodeToString(buf[start:end])))
	}
	return hops, nil
}

// SenderHashSize reads the width chosen for this frame from the path byte.
// Floods encode the width even with zero relay hops. A direct 0x00 path byte
// is sendZeroHop's marker and carries no width; TRACE path bytes are SNR.
// Zero means the width cannot be established from the supplied header.
func SenderHashSize(rawHex string) int {
	header, ok := hexByteAt(rawHex, 0)
	if !ok || !PathBytesAreHops(header>>2&0x0f) {
		return 0
	}
	route, pathByte, ok := PathByte(rawHex)
	if !ok || pathByte>>6 == 3 {
		return 0
	}
	if pathByte == 0 && (route == RouteDirect || route == RouteTransportDirect) {
		return 0
	}
	return int(pathByte>>6) + 1
}

// PathByte returns a hex-encoded frame's route type and its path_length byte,
// read after the 4 transport-code bytes on TRANSPORT_FLOOD/TRANSPORT_DIRECT
// (offset 5) and right after the header otherwise (offset 1): firmware
// docs/packet_format.md, src/Packet.cpp Packet::readFrom. ok is false when
// the header or path byte is missing or not valid hex. It does not allocate.
func PathByte(rawHex string) (routeType int, pathByte byte, ok bool) {
	header, ok := hexByteAt(rawHex, 0)
	if !ok {
		return 0, 0, false
	}
	routeType = RouteTypeFromHeader(header)
	offset := 1
	if IsTransportRoute(routeType) {
		offset += 4
	}
	pathByte, ok = hexByteAt(rawHex, offset)
	return routeType, pathByte, ok
}

// hexByteAt decodes byte offset of a hex string without allocating.
func hexByteAt(rawHex string, offset int) (byte, bool) {
	start := offset * 2
	if len(rawHex) < start+2 {
		return 0, false
	}
	hi, ok1 := hexNibble(rawHex[start])
	lo, ok2 := hexNibble(rawHex[start+1])
	return hi<<4 | lo, ok1 && ok2
}

func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// DecodeHopsForPayload returns the header path hops only when the payload type's
// header bytes are actually route hops (i.e. PathBytesAreHops(payloadType) is true).
// For TRACE packets it returns (nil, ErrPayloadHasNoHeaderHops) so the caller is
// forced to source hops from the decoded payload instead.
//
// Prefer this over DecodePathFromRawHex when the payload type is known.
func DecodeHopsForPayload(rawHex string, payloadType byte) ([]string, error) {
	if !PathBytesAreHops(payloadType) {
		return nil, ErrPayloadHasNoHeaderHops
	}
	return DecodePathFromRawHex(rawHex)
}

// ErrPayloadHasNoHeaderHops is returned by DecodeHopsForPayload when the
// payload type repurposes the raw_hex header path bytes (e.g. TRACE → SNR values).
var ErrPayloadHasNoHeaderHops = errPayloadHasNoHeaderHops{}

type errPayloadHasNoHeaderHops struct{}

func (errPayloadHasNoHeaderHops) Error() string {
	return "payload type repurposes header path bytes; source hops from decoded payload"
}
