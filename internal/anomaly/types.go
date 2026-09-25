package anomaly

import (
	"encoding/hex"
	"errors"
	"fmt"
)

// PayloadType is the 4-bit MeshCore payload type from the packet header
// (bits 2-5). The library treats it as an opaque grouping value; it never
// decodes payloads.
type PayloadType uint8

// MaxPayloadType is the largest valid PayloadType (4-bit field).
const MaxPayloadType PayloadType = 15

// PayloadTypeTrace is the MeshCore TRACE payload type. Its header path holds
// per-hop SNR bytes, not relay hashes, so a TRACE path is never used as route
// evidence.
const PayloadTypeTrace PayloadType = 9

// RouteClass is the header route type as observed. The zero value is
// RouteClassUnknown.
type RouteClass uint8

const (
	RouteClassUnknown RouteClass = iota
	RouteClassFlood
	RouteClassTransportFlood
	RouteClassDirect
	RouteClassTransportDirect
	// RouteClassMixed marks a transmission whose observations disagree on the
	// route class (for example a flood advert re-shared zero-hop). It is only
	// set by the Normalizer and is always low confidence.
	RouteClassMixed
)

var routeClassNames = [...]string{"unknown", "flood", "tflood", "direct", "tdirect", "mixed"}

func (c RouteClass) String() string {
	if int(c) < len(routeClassNames) {
		return routeClassNames[c]
	}
	return fmt.Sprintf("routeclass(%d)", uint8(c))
}

func (c RouteClass) valid() bool { return int(c) < len(routeClassNames) }

// isFlood reports whether packets of this class carry the relay path in
// order of forwarding (path[0] is the first relay after the originator).
func (c RouteClass) isFlood() bool {
	return c == RouteClassFlood || c == RouteClassTransportFlood
}

// RouteKind says what the first hop of a transmission means. Only
// RouteKindRelay has a first hop; every other kind is a separate
// low-confidence class and never names a relay.
type RouteKind uint8

const (
	RouteKindUnknown RouteKind = iota
	// RouteKindRelay: a flood observation with at least one hop. The first
	// hop is the first relay after the originator as seen on the wire. It is
	// observation evidence, not an identity of the originator.
	RouteKindRelay
	// RouteKindNoPath: a flood observation with zero hops. The observer heard
	// the packet directly; the observer is NOT assumed to be the sender.
	RouteKindNoPath
	// RouteKindDirect: a direct-routed observation. path[0] is the next hop
	// of a pre-set route, not a first relay, so no hop is kept.
	RouteKindDirect
)

var routeKindNames = [...]string{"unknown", "relay", "nopath", "direct"}

func (k RouteKind) String() string {
	if int(k) < len(routeKindNames) {
		return routeKindNames[k]
	}
	return fmt.Sprintf("routekind(%d)", uint8(k))
}

func (k RouteKind) valid() bool { return int(k) < len(routeKindNames) }

// MaxHopWidth is the widest hop hash MeshCore encodes (path_len bits 6-7).
const MaxHopWidth = 3

// Hop is one relay hash prefix together with its byte width. A 1-byte FD and
// the first byte of a 2-byte FD12 are different Hops: width is part of the
// value. The zero Hop means "no hop".
type Hop struct {
	w uint8
	b [MaxHopWidth]byte
}

// NewHop builds a Hop from 1 to 3 bytes.
func NewHop(b []byte) (Hop, error) {
	if len(b) < 1 || len(b) > MaxHopWidth {
		return Hop{}, fmt.Errorf("anomaly: hop width %d outside 1..%d", len(b), MaxHopWidth)
	}
	var h Hop
	h.w = uint8(len(b))
	copy(h.b[:], b)
	return h, nil
}

// Width returns the hop width in bytes (0 for the zero Hop).
func (h Hop) Width() int { return int(h.w) }

// IsZero reports whether h is the zero Hop.
func (h Hop) IsZero() bool { return h.w == 0 }

// Bytes returns a copy of the hop bytes.
func (h Hop) Bytes() []byte { return append([]byte(nil), h.b[:h.w]...) }

func (h Hop) encode() string {
	if h.w == 0 {
		return "-"
	}
	return fmt.Sprintf("%d:%s", h.w, hex.EncodeToString(h.b[:h.w]))
}

// MaxChannelHashLen bounds the opaque channel hash. MeshCore uses 1 byte
// today; the type allows up to 4 so a wider hash does not change the API.
const MaxChannelHashLen = 4

// ChannelHash is an opaque channel identifier as it appears on the wire. The
// library never interprets it and never relies on names such as "enc_XX" or
// on whether this instance could decrypt the channel. The zero value means
// "no channel" (for payload types without one), which is different from a
// 1-byte hash 0x00.
type ChannelHash struct {
	n uint8
	b [MaxChannelHashLen]byte
}

// NewChannelHash builds a ChannelHash from 1 to 4 bytes.
func NewChannelHash(b []byte) (ChannelHash, error) {
	if len(b) < 1 || len(b) > MaxChannelHashLen {
		return ChannelHash{}, fmt.Errorf("anomaly: channel hash length %d outside 1..%d", len(b), MaxChannelHashLen)
	}
	var c ChannelHash
	c.n = uint8(len(b))
	copy(c.b[:], b)
	return c, nil
}

// Len returns the hash length in bytes (0 = no channel).
func (c ChannelHash) Len() int { return int(c.n) }

// IsZero reports whether c is "no channel".
func (c ChannelHash) IsZero() bool { return c.n == 0 }

// Bytes returns a copy of the hash bytes.
func (c ChannelHash) Bytes() []byte { return append([]byte(nil), c.b[:c.n]...) }

func (c ChannelHash) encode() string {
	if c.n == 0 {
		return "-"
	}
	return fmt.Sprintf("%d:%s", c.n, hex.EncodeToString(c.b[:c.n]))
}

// MaxPathBytes is the MeshCore limit on path bytes (MAX_PATH_SIZE).
const MaxPathBytes = 64

// MaxWireBytes is the MeshCore maximum frame size (MAX_TRANS_UNIT).
const MaxWireBytes = 255

// Route is the full ordered route of one observation: hop width plus the hop
// bytes in forwarding order. It is evidence only and never part of a stream
// key, because full routes fragment far more than first hops.
type Route struct {
	w    uint8
	hops []byte
}

// NewRoute builds a Route from a hop width (1..3) and the concatenated hop
// bytes. An empty route (no hops) is written as NewRoute(0, nil).
func NewRoute(width int, hops []byte) (Route, error) {
	if len(hops) == 0 {
		if width != 0 {
			return Route{}, errors.New("anomaly: empty route must have width 0")
		}
		return Route{}, nil
	}
	if width < 1 || width > MaxHopWidth {
		return Route{}, fmt.Errorf("anomaly: route hop width %d outside 1..%d", width, MaxHopWidth)
	}
	if len(hops)%width != 0 {
		return Route{}, errors.New("anomaly: route length is not a multiple of its hop width")
	}
	if len(hops) > MaxPathBytes {
		return Route{}, fmt.Errorf("anomaly: route longer than %d bytes", MaxPathBytes)
	}
	return Route{w: uint8(width), hops: append([]byte(nil), hops...)}, nil
}

// Width returns the hop width (0 for an empty route).
func (r Route) Width() int { return int(r.w) }

// Hops returns the number of hops.
func (r Route) Hops() int {
	if r.w == 0 {
		return 0
	}
	return len(r.hops) / int(r.w)
}

// Bytes returns a copy of the concatenated hop bytes.
func (r Route) Bytes() []byte { return append([]byte(nil), r.hops...) }

// First returns the first hop (zero Hop for an empty route).
func (r Route) First() Hop {
	if r.w == 0 {
		return Hop{}
	}
	var h Hop
	h.w = r.w
	copy(h.b[:], r.hops[:r.w])
	return h
}

func (r Route) equal(o Route) bool {
	if r.w != o.w || len(r.hops) != len(o.hops) {
		return false
	}
	for i := range r.hops {
		if r.hops[i] != o.hops[i] {
			return false
		}
	}
	return true
}

// DecryptStatus records whether the caller's instance decrypted the payload.
// It is metadata only: it is never part of a key and never changes counting.
// "Not decrypted here" is a property of this instance's keys, not of the
// radio traffic.
type DecryptStatus uint8

const (
	DecryptUnknown DecryptStatus = iota
	DecryptDecrypted
	DecryptNotDecrypted
)

func (d DecryptStatus) valid() bool { return d <= DecryptNotDecrypted }

// TrafficClass is a caller-supplied label. The library never infers it and
// never hardcodes channel names; a caller that knows a channel is expected
// automation (by its own configuration) labels those events.
type TrafficClass uint8

const (
	// TrafficUnclassified: no label supplied.
	TrafficUnclassified TrafficClass = iota
	// TrafficExpected: the caller asserts this is expected automated traffic.
	TrafficExpected
	// TrafficMonitored: the caller wants this traffic evaluated and never
	// suppressed.
	TrafficMonitored
)

func (t TrafficClass) valid() bool { return t <= TrafficMonitored }

// TrafficLabel summarizes the caller labels of the events behind a candidate.
type TrafficLabel uint8

const (
	LabelUnclassified TrafficLabel = iota // no event carried a label
	LabelExpected                         // every event was TrafficExpected
	LabelMixed                            // some expected, some not
	LabelMonitored                        // at least one event was TrafficMonitored
)

var labelNames = [...]string{"unclassified", "expected", "mixed", "monitored"}

func (l TrafficLabel) String() string {
	if int(l) < len(labelNames) {
		return labelNames[l]
	}
	return fmt.Sprintf("label(%d)", uint8(l))
}

// mergeLabel combines the labels of two sets of events.
func mergeLabel(a, b TrafficLabel) TrafficLabel {
	switch {
	case a == b:
		return a
	case a == LabelMonitored || b == LabelMonitored:
		return LabelMonitored
	case a == LabelExpected || b == LabelExpected || a == LabelMixed || b == LabelMixed:
		return LabelMixed
	}
	return LabelUnclassified
}

func labelFromCounts(total, expected, monitored uint64) TrafficLabel {
	switch {
	case monitored > 0:
		return LabelMonitored
	case total > 0 && expected == total:
		return LabelExpected
	case expected > 0:
		return LabelMixed
	default:
		return LabelUnclassified
	}
}
