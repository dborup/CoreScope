package anomaly

import (
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
)

// Scope is the aggregation level a key and a rule work on.
type Scope uint8

const (
	scopeInvalid Scope = iota
	// ScopeStream: payload type + channel hash + route class + route kind +
	// first hop (with width). An observed route group, not an originator.
	ScopeStream
	// ScopeChannel: payload type + channel hash, all routes merged.
	ScopeChannel
	// ScopeRouteGroup: payload type + first hop (with width), all channels and
	// flood route classes merged. Only transmissions with RouteKindRelay and
	// a flood route class (not mixed or unknown) belong to a route group.
	ScopeRouteGroup
	// ScopeGlobal: payload type only. A global candidate is never attributed
	// to a stream, channel or route.
	ScopeGlobal
	numScopes
)

var scopeNames = [...]string{"invalid", "stream", "channel", "route", "global"}

func (s Scope) String() string {
	if int(s) < len(scopeNames) {
		return scopeNames[s]
	}
	return "scope(" + strconv.Itoa(int(s)) + ")"
}

func (s Scope) valid() bool { return s > scopeInvalid && s < numScopes }

// Key identifies one series at one scope. Key is comparable and usable as a
// map key. Fields that do not belong to the key's scope are always zero, so
// two keys are equal exactly when their canonical encodings are equal.
type Key struct {
	Scope       Scope
	PayloadType PayloadType
	Channel     ChannelHash
	RouteClass  RouteClass
	RouteKind   RouteKind
	FirstHop    Hop
}

// StreamKeyOf returns the stream key of a finalized event.
func StreamKeyOf(e Event) Key {
	k := Key{Scope: ScopeStream, PayloadType: e.PayloadType, Channel: e.Channel,
		RouteClass: e.RouteClass, RouteKind: e.RouteKind}
	if e.RouteKind == RouteKindRelay {
		k.FirstHop = e.FirstHop
	}
	return k
}

// ChannelKeyOf returns the channel key of a finalized event.
func ChannelKeyOf(e Event) Key {
	return Key{Scope: ScopeChannel, PayloadType: e.PayloadType, Channel: e.Channel}
}

// RouteGroupKeyOf returns the route-group key of a finalized event. ok is
// false unless the event has a relay first hop from a flood route class:
// no-path, direct, mixed and unknown transmissions never belong to a route
// group.
func RouteGroupKeyOf(e Event) (k Key, ok bool) {
	if e.RouteKind != RouteKindRelay || e.FirstHop.IsZero() || !e.RouteClass.isFlood() {
		return Key{}, false
	}
	return Key{Scope: ScopeRouteGroup, PayloadType: e.PayloadType, RouteKind: RouteKindRelay, FirstHop: e.FirstHop}, true
}

// GlobalKeyOf returns the global key (payload type only).
func GlobalKeyOf(e Event) Key {
	return Key{Scope: ScopeGlobal, PayloadType: e.PayloadType}
}

// keyOf maps an event to the key of the given scope.
func keyOf(s Scope, e Event) (Key, bool) {
	switch s {
	case ScopeStream:
		return StreamKeyOf(e), true
	case ScopeChannel:
		return ChannelKeyOf(e), true
	case ScopeRouteGroup:
		return RouteGroupKeyOf(e)
	case ScopeGlobal:
		return GlobalKeyOf(e), true
	}
	return Key{}, false
}

// Confidence says how much a candidate's key says about routing.
type Confidence uint8

const (
	// ConfidenceAggregate: channel or global scope; no route claim at all.
	ConfidenceAggregate Confidence = iota + 1
	// ConfidenceRouteObserved: a relay first hop was observed. It is still
	// only observation evidence and may be globally ambiguous.
	ConfidenceRouteObserved
	// ConfidenceLow: no path, direct, mixed or unknown route; or the
	// evidence itself is incomplete (for example a left-censored stream).
	ConfidenceLow
)

var confidenceNames = [...]string{"", "aggregate", "route_observed", "low"}

func (c Confidence) String() string {
	if int(c) < len(confidenceNames) && c > 0 {
		return confidenceNames[c]
	}
	return "confidence(" + strconv.Itoa(int(c)) + ")"
}

func (k Key) confidence() Confidence {
	switch k.Scope {
	case ScopeChannel, ScopeGlobal:
		return ConfidenceAggregate
	case ScopeRouteGroup:
		return ConfidenceRouteObserved
	}
	if k.RouteKind == RouteKindRelay && k.RouteClass != RouteClassMixed && k.RouteClass != RouteClassUnknown {
		return ConfidenceRouteObserved
	}
	return ConfidenceLow
}

// Validate checks that k is canonical for its scope.
func (k Key) Validate() error {
	if !k.Scope.valid() {
		return errors.New("anomaly: key has invalid scope")
	}
	if k.PayloadType > MaxPayloadType {
		return errors.New("anomaly: key payload type out of range")
	}
	if !k.RouteClass.valid() || !k.RouteKind.valid() {
		return errors.New("anomaly: key route class or kind out of range")
	}
	if err := k.Channel.canonical(); err != nil {
		return err
	}
	if err := k.FirstHop.canonical(); err != nil {
		return err
	}
	switch k.Scope {
	case ScopeStream:
		if (k.RouteKind == RouteKindRelay) != !k.FirstHop.IsZero() {
			return errors.New("anomaly: stream key must have a first hop exactly when its route kind is relay")
		}
		if !kindFitsClass(k.RouteKind, k.RouteClass) {
			return errors.New("anomaly: stream key route kind inconsistent with its route class")
		}
	case ScopeChannel:
		if k.RouteClass != 0 || k.RouteKind != 0 || !k.FirstHop.IsZero() {
			return errors.New("anomaly: channel key must not carry route fields")
		}
	case ScopeRouteGroup:
		if !k.Channel.IsZero() || k.RouteClass != 0 || k.RouteKind != RouteKindRelay || k.FirstHop.IsZero() {
			return errors.New("anomaly: route key must carry only a relay first hop")
		}
	case ScopeGlobal:
		if !k.Channel.IsZero() || k.RouteClass != 0 || k.RouteKind != 0 || !k.FirstHop.IsZero() {
			return errors.New("anomaly: global key must carry only a payload type")
		}
	}
	return nil
}

func (h Hop) canonical() error {
	if h.w > MaxHopWidth {
		return errors.New("anomaly: hop width out of range")
	}
	for i := int(h.w); i < MaxHopWidth; i++ {
		if h.b[i] != 0 {
			return errors.New("anomaly: hop has bytes beyond its width")
		}
	}
	return nil
}

func (c ChannelHash) canonical() error {
	if c.n > MaxChannelHashLen {
		return errors.New("anomaly: channel hash length out of range")
	}
	for i := int(c.n); i < MaxChannelHashLen; i++ {
		if c.b[i] != 0 {
			return errors.New("anomaly: channel hash has bytes beyond its length")
		}
	}
	return nil
}

// String returns the canonical encoding, for example
//
//	v1|s=stream|pt=5|ch=1:ab|rc=flood|rk=relay|hop=1:5c
//
// Every field is always present, lengths are explicit and hex is lower case,
// so the encoding is injective: two distinct valid keys never encode to the
// same string, and a 1-byte hop can never be read as the prefix of a wider
// one. The encoding holds only values the caller supplied.
func (k Key) String() string {
	var b strings.Builder
	b.Grow(48)
	b.WriteString("v1|s=")
	b.WriteString(k.Scope.String())
	b.WriteString("|pt=")
	b.WriteString(strconv.Itoa(int(k.PayloadType)))
	b.WriteString("|ch=")
	b.WriteString(k.Channel.encode())
	b.WriteString("|rc=")
	b.WriteString(k.RouteClass.String())
	b.WriteString("|rk=")
	b.WriteString(k.RouteKind.String())
	b.WriteString("|hop=")
	b.WriteString(k.FirstHop.encode())
	return b.String()
}

var errBadKeyEncoding = errors.New("anomaly: not a canonical key encoding")

// ParseKey parses the canonical encoding produced by Key.String. It rejects
// every non-canonical form, so ParseKey(s) succeeds only if
// ParseKey(s).String() == s.
func ParseKey(s string) (Key, error) {
	parts := strings.Split(s, "|")
	if len(parts) != 7 || parts[0] != "v1" {
		return Key{}, errBadKeyEncoding
	}
	field := func(i int, name string) (string, bool) {
		p := parts[i]
		if !strings.HasPrefix(p, name+"=") {
			return "", false
		}
		return p[len(name)+1:], true
	}
	var k Key
	sv, ok := field(1, "s")
	if !ok {
		return Key{}, errBadKeyEncoding
	}
	for i, n := range scopeNames {
		if i > 0 && n == sv {
			k.Scope = Scope(i)
		}
	}
	pv, ok := field(2, "pt")
	if !ok {
		return Key{}, errBadKeyEncoding
	}
	pt, err := strconv.ParseUint(pv, 10, 8)
	if err != nil || strconv.FormatUint(pt, 10) != pv || pt > uint64(MaxPayloadType) {
		return Key{}, errBadKeyEncoding
	}
	k.PayloadType = PayloadType(pt)
	cv, ok := field(3, "ch")
	if !ok {
		return Key{}, errBadKeyEncoding
	}
	cb, err := decodeLenHex(cv, MaxChannelHashLen)
	if err != nil {
		return Key{}, errBadKeyEncoding
	}
	if cb != nil {
		k.Channel, _ = NewChannelHash(cb)
	}
	rv, ok := field(4, "rc")
	if !ok {
		return Key{}, errBadKeyEncoding
	}
	found := false
	for i, n := range routeClassNames {
		if n == rv {
			k.RouteClass, found = RouteClass(i), true
		}
	}
	if !found {
		return Key{}, errBadKeyEncoding
	}
	kv, ok := field(5, "rk")
	if !ok {
		return Key{}, errBadKeyEncoding
	}
	found = false
	for i, n := range routeKindNames {
		if n == kv {
			k.RouteKind, found = RouteKind(i), true
		}
	}
	if !found {
		return Key{}, errBadKeyEncoding
	}
	hv, ok := field(6, "hop")
	if !ok {
		return Key{}, errBadKeyEncoding
	}
	hb, err := decodeLenHex(hv, MaxHopWidth)
	if err != nil {
		return Key{}, errBadKeyEncoding
	}
	if hb != nil {
		k.FirstHop, _ = NewHop(hb)
	}
	if err := k.Validate(); err != nil {
		return Key{}, errBadKeyEncoding
	}
	if k.String() != s {
		return Key{}, errBadKeyEncoding
	}
	return k, nil
}

// decodeLenHex decodes "-" (nil) or "<n>:<2n lower-case hex>".
func decodeLenHex(v string, max int) ([]byte, error) {
	if v == "-" {
		return nil, nil
	}
	i := strings.IndexByte(v, ':')
	if i != 1 {
		return nil, errBadKeyEncoding
	}
	n := int(v[0] - '0')
	if n < 1 || n > max {
		return nil, errBadKeyEncoding
	}
	h := v[2:]
	if len(h) != 2*n || strings.ToLower(h) != h {
		return nil, errBadKeyEncoding
	}
	b, err := hex.DecodeString(h)
	if err != nil {
		return nil, errBadKeyEncoding
	}
	return b, nil
}

// less orders keys deterministically (by canonical encoding fields).
func (k Key) less(o Key) bool {
	if k.Scope != o.Scope {
		return k.Scope < o.Scope
	}
	if k.PayloadType != o.PayloadType {
		return k.PayloadType < o.PayloadType
	}
	if k.Channel.n != o.Channel.n {
		return k.Channel.n < o.Channel.n
	}
	if k.Channel.b != o.Channel.b {
		return string(k.Channel.b[:]) < string(o.Channel.b[:])
	}
	if k.RouteClass != o.RouteClass {
		return k.RouteClass < o.RouteClass
	}
	if k.RouteKind != o.RouteKind {
		return k.RouteKind < o.RouteKind
	}
	if k.FirstHop.w != o.FirstHop.w {
		return k.FirstHop.w < o.FirstHop.w
	}
	return string(k.FirstHop.b[:]) < string(o.FirstHop.b[:])
}
