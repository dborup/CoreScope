package packetpath

// Route type constants (header bits 1-0).
const (
	RouteTransportFlood  = 0
	RouteFlood           = 1
	RouteDirect          = 2
	RouteTransportDirect = 3
)

// PayloadTRACE is the payload type constant for TRACE packets.
const PayloadTRACE = 0x09

// IsTransportRoute returns true for TRANSPORT_FLOOD (0) and TRANSPORT_DIRECT (3).
func IsTransportRoute(routeType int) bool {
	return routeType == RouteTransportFlood || routeType == RouteTransportDirect
}

// PathBytesAreHops returns true when the raw_hex header path bytes represent
// route hop hashes (the normal case). Returns false for packet types where
// header path bytes are repurposed (e.g. TRACE uses them for SNR values).
func PathBytesAreHops(payloadType byte) bool {
	return payloadType != PayloadTRACE
}

// Route mask bits for transmissions.route_mask (#89): bit r is set when a
// frame carrying raw route type r (header bits 1-0) was observed for the
// transmission's content hash. The mask is monotonic: bits are OR-ed in and
// never cleared.
const (
	RouteMaskFlood  = int64(1)<<RouteTransportFlood | int64(1)<<RouteFlood   // routes 0 and 1
	RouteMaskDirect = int64(1)<<RouteDirect | int64(1)<<RouteTransportDirect // routes 2 and 3
	RouteMaskAll    = RouteMaskFlood | RouteMaskDirect
)

// RouteTypeFromHeader returns the route type carried in the low two bits of
// a packet header byte (firmware src/Packet.h PH_ROUTE_MASK).
func RouteTypeFromHeader(header byte) int {
	return int(header & 0x03)
}

// RouteTypeFromRawHex returns the route type of a hex-encoded frame's header
// byte. ok is false when the header byte is missing or not valid hex.
func RouteTypeFromRawHex(rawHex string) (routeType int, ok bool) {
	if len(rawHex) < 2 {
		return 0, false
	}
	var header byte
	for _, c := range []byte(rawHex[:2]) {
		var v byte
		switch {
		case c >= '0' && c <= '9':
			v = c - '0'
		case c >= 'a' && c <= 'f':
			v = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			v = c - 'A' + 10
		default:
			return 0, false
		}
		header = header<<4 | v
	}
	return RouteTypeFromHeader(header), true
}

// RouteMaskBit returns the transmissions.route_mask bit for a raw route type,
// or 0 for values outside 0..3 (which must not set any bit).
func RouteMaskBit(routeType int) int64 {
	if routeType < RouteTransportFlood || routeType > RouteTransportDirect {
		return 0
	}
	return int64(1) << uint(routeType)
}
