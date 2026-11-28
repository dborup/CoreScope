package packetcard

import "time"

// PacketCardInput is everything the card may show about one packet. The
// caller (cmd/server, #812 M2) fills it from getPacketPath after the path
// visibility rule (filterPingScorePath): a branch through a hidden node or
// observer is already gone, the first observation is nil when it is
// hidden, approximate positions are already stripped under a visibility
// policy, and Areas are recomputed from the visible positions only.
//
// The payload and route are numbers, named by the card itself, and there
// is deliberately no field for message text, a channel, a key, a sender or
// any other observer's name: the card cannot draw what it is never given
// (#812 privacy rules 1–4). The only names are FirstObserverName and, for
// an advert, AdvertName.
type PacketCardInput struct {
	// PayloadType is the firmware payload_type (PAYLOAD_TYPE_* in
	// Packet.h), named as packet-filter.js names it. Every type gets a
	// card (owner decision Q2, revised).
	PayloadType int
	// RouteType is the firmware route_type (ROUTE_TYPE_*); outside 0–3
	// its clause is dropped.
	RouteType int
	// Branches are the visible branches, one per observer, deepest first
	// as getPacketPath orders them. All of them are counted ("heard by");
	// at most MaxBranches are drawn.
	Branches []Branch
	// First is the earliest observation; nil when it is unknown or
	// hidden. Without it the card names no observer and draws no spread
	// or distance, which are measured from it.
	First *Branch
	// FirstObserverName is First's observer as the packet page names it,
	// drawn as "First heard by …" only with First (owner decision Q3,
	// revised). No other observer is ever named.
	FirstObserverName string
	// AdvertName is the advertising node's name, drawn only for an
	// ADVERT (privacy rule 2); the caller passes it only when the node is
	// not hidden.
	AdvertName string
	// Areas are the touched areas' boundaries, drawn as faint outlines
	// behind the path (owner decision Q4, revised). Their labels are not
	// drawn.
	Areas []Area
	// FirstHeard is when the packet was first heard; zero drops it.
	FirstHeard time.Time
	// AirtimeMs is the estimated flood airtime (estimatedAirtimeMs); nil
	// when unknown or when the filter removed anything.
	AirtimeMs *float64
}

// Branch is one observer's deepest observation of the packet.
type Branch struct {
	// Hops is the raw hop count of the observation.
	Hops int
	// Points are the positioned relays in path order; an unpositioned
	// relay is left out, and the line runs straight past it.
	Points []LatLon
	// Observer is where the observer is; nil when unknown.
	Observer *LatLon
	// SecondsAfterFirst and DistanceFromFirstKm are measured from First;
	// nil when unknown. Ignored when First is nil.
	SecondsAfterFirst   *float64
	DistanceFromFirstKm *float64
}

// LatLon is a position in degrees. A position outside ±90°/±180°, NaN or
// infinite is not drawn.
type LatLon struct {
	Lat, Lon float64
}

// Area is one touched area's boundary as a ring of vertices; BoxRing
// turns a bounding-box area into one.
type Area struct {
	Ring []LatLon
}

// BoxRing is the ring of a bounding-box area (TouchedAreaShape's
// LatMin/LatMax/LonMin/LonMax).
func BoxRing(latMin, latMax, lonMin, lonMax float64) []LatLon {
	return []LatLon{{latMin, lonMin}, {latMin, lonMax}, {latMax, lonMax}, {latMax, lonMin}}
}

// Options are the operator's display choices.
type Options struct {
	// Palette is the card's colours; nil draws the shipped dark palette.
	Palette *Palette
	// HideNames draws no names at all: no first observer, no advert
	// origin (the profile card's names setting).
	HideNames bool
	// Site is the site name in the footer, e.g. "meshview.dk".
	Site string
}

// Bounds on what one card draws, the same as the ping history archive's:
// beyond them, branches are left out (and counted in a "+N more" note)
// and points are decimated. Areas and their vertices are capped too.
const (
	MaxBranches     = 128
	MaxPoints       = 4096
	MaxAreas        = 64
	MaxAreaVertices = 4096
)
