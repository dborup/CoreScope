package anomaly

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// TxID identifies one logical transmission (for CoreScope: the content
// hash). The library only compares it for equality and never interprets it.
type TxID string

// MaxIDLen bounds TxID and observation IDs, so a caller cannot make the
// dedup set hold arbitrarily large strings.
const MaxIDLen = 256

// MaxPayloadBytes is the MeshCore payload limit (MAX_PACKET_PAYLOAD).
const MaxPayloadBytes = 184

// Valid event times: [minTime, maxTime), i.e. after the Unix epoch and
// before 2200. Times outside this range are rejected so nanosecond
// arithmetic can never overflow; the epoch itself (a typical unset-clock
// value) is rejected so zero can mean "never" internally.
var (
	minTime = time.Unix(0, 1).UTC()
	maxTime = time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)
)

func validTime(t time.Time) bool { return !t.Before(minTime) && t.Before(maxTime) }

// Event is one FINAL, normalized logical transmission. The detector counts
// each Event.ID at most once (within the dedup horizon) and never changes an
// event after Observe. Callers either build events themselves, having already
// finished normalization, or use Normalizer, which finalizes events by an
// explicit settle policy.
type Event struct {
	ID TxID
	// Time is the data timestamp of the transmission: the earliest included
	// observation time. The detector never reads the wall clock.
	Time time.Time
	// FinalAt is when the event's evidence was complete (for Normalizer:
	// first arrival + settle window). The detector's decision clock never
	// runs behind the FinalAt of any event it was offered, so every
	// candidate that uses this event reports DecidedAt >= FinalAt and a
	// replay never claims an alarm earlier than the route evidence behind
	// it existed. Zero means FinalAt == Time.
	FinalAt     time.Time
	PayloadType PayloadType
	// Channel is opaque; zero for payload types without a channel.
	Channel ChannelHash
	// Decryption is metadata only; it never affects keys or counting.
	Decryption DecryptStatus
	RouteClass RouteClass
	RouteKind  RouteKind
	// FirstHop is set exactly when RouteKind == RouteKindRelay.
	FirstHop Hop
	// FullRoute is optional separate evidence; if set on a relay event its
	// first hop must equal FirstHop.
	FullRoute Route
	// WireBytes is the size of the reference frame in bytes (not hex
	// characters). 0 means unknown.
	WireBytes int
	// PayloadBytes is valid only when PayloadKnown is true.
	PayloadBytes int
	PayloadKnown bool
	Traffic      TrafficClass
	// Normalization evidence (0 for caller-built events).
	Observations         int
	ExcludedObservations int
	// Truncated is set when normalization cut this transmission's evidence
	// for capacity (fan-out cap or forced early finalization); its first hop
	// may then rest on incomplete route evidence.
	Truncated bool
}

func (e *Event) finalAt() time.Time {
	if e.FinalAt.IsZero() {
		return e.Time
	}
	return e.FinalAt
}

var errInvalidEvent = errors.New("anomaly: invalid event")

// kindFitsClass: a relay or no-path kind needs a flood class, a direct kind
// a direct class; RouteClassMixed (observations disagree) allows any kind.
func kindFitsClass(k RouteKind, c RouteClass) bool {
	switch k {
	case RouteKindRelay, RouteKindNoPath:
		return c.isFlood() || c == RouteClassMixed
	case RouteKindDirect:
		return c == RouteClassDirect || c == RouteClassTransportDirect || c == RouteClassMixed
	}
	return true
}

// Validate checks the event contract. Error messages name the violated
// field only; they never echo IDs, hashes or routes.
func (e *Event) Validate() error {
	fail := func(what string) error { return fmt.Errorf("%w: %s", errInvalidEvent, what) }
	if len(e.ID) == 0 || len(e.ID) > MaxIDLen {
		return fail("ID length")
	}
	if !validTime(e.Time) {
		return fail("Time out of range")
	}
	if !e.FinalAt.IsZero() && (!validTime(e.FinalAt) || e.FinalAt.Before(e.Time)) {
		return fail("FinalAt before Time or out of range")
	}
	if e.PayloadType > MaxPayloadType {
		return fail("PayloadType")
	}
	if !e.RouteClass.valid() || !e.RouteKind.valid() || !e.Decryption.valid() || !e.Traffic.valid() {
		return fail("enum field out of range")
	}
	if e.Channel.canonical() != nil || e.FirstHop.canonical() != nil {
		return fail("non-canonical channel or hop")
	}
	if (e.RouteKind == RouteKindRelay) == e.FirstHop.IsZero() {
		return fail("FirstHop must be set exactly for RouteKindRelay")
	}
	if e.RouteKind == RouteKindRelay && e.PayloadType == PayloadTypeTrace {
		return fail("TRACE paths are not route evidence")
	}
	if !kindFitsClass(e.RouteKind, e.RouteClass) {
		return fail("RouteKind inconsistent with RouteClass")
	}
	if e.FullRoute.Width() != 0 {
		if e.RouteKind != RouteKindRelay || e.FullRoute.First() != e.FirstHop {
			return fail("FullRoute inconsistent with FirstHop")
		}
	}
	if e.WireBytes < 0 || e.WireBytes > MaxWireBytes {
		return fail("WireBytes")
	}
	if e.PayloadKnown {
		if e.PayloadBytes < 0 || e.PayloadBytes > MaxPayloadBytes {
			return fail("PayloadBytes")
		}
		if e.WireBytes > 0 && e.PayloadBytes > e.WireBytes-2 {
			return fail("PayloadBytes larger than the frame allows")
		}
	} else if e.PayloadBytes != 0 {
		return fail("PayloadBytes set without PayloadKnown")
	}
	if e.Observations < 0 || e.ExcludedObservations < 0 {
		return fail("observation counts")
	}
	return nil
}

// Observation is one reception of a transmission by one observer. The
// observer itself is deliberately absent: it is never evidence of the
// sender or of the first relay.
type Observation struct {
	TxID TxID
	// ObsID is a stable identifier unique within the transmission (for
	// CoreScope: the observation row id). It breaks timestamp ties.
	ObsID string
	// Time is the observation's data timestamp.
	Time time.Time
	// ArrivedAt is when the caller received the observation. Arrival order
	// drives finalization; it may differ from Time. Zero means ArrivedAt ==
	// Time.
	ArrivedAt    time.Time
	PayloadType  PayloadType
	Channel      ChannelHash
	Decryption   DecryptStatus
	RouteClass   RouteClass
	Path         Route
	WireBytes    int
	PayloadBytes int
	PayloadKnown bool
	Traffic      TrafficClass
}

func (o *Observation) arrived() time.Time {
	if o.ArrivedAt.IsZero() {
		return o.Time
	}
	return o.ArrivedAt
}

// kind classifies the observation's route evidence.
func (o *Observation) kind() RouteKind {
	if o.PayloadType == PayloadTypeTrace {
		return RouteKindUnknown
	}
	switch {
	case o.RouteClass.isFlood() && o.Path.Hops() > 0:
		return RouteKindRelay
	case o.RouteClass.isFlood():
		return RouteKindNoPath
	case o.RouteClass == RouteClassDirect || o.RouteClass == RouteClassTransportDirect:
		return RouteKindDirect
	}
	return RouteKindUnknown
}

var errInvalidObservation = errors.New("anomaly: invalid observation")

// Validate checks one observation. Messages never echo field values.
func (o *Observation) Validate() error {
	fail := func(what string) error { return fmt.Errorf("%w: %s", errInvalidObservation, what) }
	if len(o.TxID) == 0 || len(o.TxID) > MaxIDLen || len(o.ObsID) == 0 || len(o.ObsID) > MaxIDLen {
		return fail("ID length")
	}
	if !validTime(o.Time) || (!o.ArrivedAt.IsZero() && !validTime(o.ArrivedAt)) {
		return fail("time out of range")
	}
	if o.PayloadType > MaxPayloadType || !o.RouteClass.valid() || o.RouteClass == RouteClassMixed ||
		!o.Decryption.valid() || !o.Traffic.valid() {
		return fail("enum field out of range")
	}
	if o.Channel.canonical() != nil {
		return fail("non-canonical channel")
	}
	if o.WireBytes < 0 || o.WireBytes > MaxWireBytes {
		return fail("WireBytes")
	}
	if o.PayloadKnown && (o.PayloadBytes < 0 || o.PayloadBytes > MaxPayloadBytes) {
		return fail("PayloadBytes")
	}
	if !o.PayloadKnown && o.PayloadBytes != 0 {
		return fail("PayloadBytes set without PayloadKnown")
	}
	if !fitsFrame(o, o.WireBytes) {
		return fail("PayloadBytes larger than the frame allows")
	}
	if o.Path.w > MaxHopWidth || (o.Path.w == 0) != (len(o.Path.hops) == 0) || len(o.Path.hops) > MaxPathBytes {
		return fail("path")
	}
	return nil
}

// compatible reports whether two observations can belong to one
// transmission: a content hash covers the payload, so payload type, channel
// and (when both know it) payload size must agree.
func compatible(a, b *Observation) bool {
	return a.PayloadType == b.PayloadType && a.Channel == b.Channel &&
		(!a.PayloadKnown || !b.PayloadKnown || a.PayloadBytes == b.PayloadBytes) &&
		fitsFrame(a, b.WireBytes) && fitsFrame(b, a.WireBytes)
}

// fitsFrame reports whether o's known payload fits a frame of w bytes
// (header and path length bytes at least; w == 0 is unknown).
func fitsFrame(o *Observation, w int) bool {
	return !o.PayloadKnown || w == 0 || o.PayloadBytes <= w-2
}

func sameObservation(a, b *Observation) bool {
	return a.TxID == b.TxID && a.ObsID == b.ObsID && a.Time.Equal(b.Time) &&
		a.arrived().Equal(b.arrived()) && a.PayloadType == b.PayloadType && a.Channel == b.Channel &&
		a.PayloadKnown == b.PayloadKnown && a.PayloadBytes == b.PayloadBytes &&
		a.Decryption == b.Decryption && a.RouteClass == b.RouteClass &&
		a.Path.equal(b.Path) && a.WireBytes == b.WireBytes && a.Traffic == b.Traffic
}

// obsLess orders observations by data time, then by ObsID. This is the
// order used to pick the reference observation.
func obsLess(a, b *Observation) bool {
	if !a.Time.Equal(b.Time) {
		return a.Time.Before(b.Time)
	}
	return a.ObsID < b.ObsID
}

// buildEvent turns the included observations of one transmission into a
// final Event. obs must be non-empty, validated, share TxID and payload
// fields, and hold no duplicate ObsID. It does not modify obs.
func buildEvent(obs []*Observation, finalAt time.Time, excluded int) Event {
	sorted := make([]*Observation, len(obs))
	copy(sorted, obs)
	sort.Slice(sorted, func(i, j int) bool { return obsLess(sorted[i], sorted[j]) })

	e := Event{
		ID:                   sorted[0].TxID,
		Time:                 sorted[0].Time,
		PayloadType:          sorted[0].PayloadType,
		Channel:              sorted[0].Channel,
		Observations:         len(sorted),
		ExcludedObservations: excluded,
	}
	// Reference observation: the earliest (Time, ObsID) with a relay path;
	// else the earliest no-path flood; else the earliest direct; else the
	// earliest overall. Hop width is kept as received.
	var ref *Observation
	for _, want := range []RouteKind{RouteKindRelay, RouteKindNoPath, RouteKindDirect} {
		for _, o := range sorted {
			if o.kind() == want {
				ref = o
				break
			}
		}
		if ref != nil {
			break
		}
	}
	if ref == nil {
		ref = sorted[0]
	}
	e.RouteKind = ref.kind()
	e.RouteClass = ref.RouteClass
	e.WireBytes = ref.WireBytes
	if e.RouteKind == RouteKindRelay {
		e.FirstHop = ref.Path.First()
		e.FullRoute = Route{w: ref.Path.w, hops: append([]byte(nil), ref.Path.hops...)}
	}
	var classes uint8
	for _, o := range sorted {
		if o.RouteClass != RouteClassUnknown {
			classes |= 1 << o.RouteClass
		}
		switch o.Decryption {
		case DecryptDecrypted:
			e.Decryption = DecryptDecrypted
		case DecryptNotDecrypted:
			if e.Decryption != DecryptDecrypted {
				e.Decryption = DecryptNotDecrypted
			}
		}
		if o.Traffic > e.Traffic {
			e.Traffic = o.Traffic
		}
		if o.PayloadKnown && !e.PayloadKnown {
			e.PayloadBytes, e.PayloadKnown = o.PayloadBytes, true
		}
	}
	if classes&(classes-1) != 0 {
		// Observations disagree on the route class. Keep the reference
		// hop as evidence but mark the class mixed (low confidence).
		e.RouteClass = RouteClassMixed
	}
	e.FinalAt = finalAt
	if e.FinalAt.Before(e.Time) {
		e.FinalAt = e.Time
	}
	return e
}
