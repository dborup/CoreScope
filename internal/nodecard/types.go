package nodecard

import (
	"errors"
	"time"
)

// NodeCardInput is everything the card may show about one node. The caller
// (cmd/server, #811 M3) fills it from the snapshots the node page already
// reads and passes only nodes the visibility rule (identityHidden) lets
// through. There is deliberately no field for a position, a map or packet
// content: the card cannot draw what it is never given (#811 plan, Q3).
//
// Every optional value is a pointer or a zero time: absent drops its
// clause, it is never drawn as 0.
type NodeCardInput struct {
	// PubKey is the node's public key in hex. Only its first 8 characters
	// are ever drawn, in the fallback title when the name is hidden or
	// cannot be drawn ("Node 3f2a91c0"); the full key is in the link.
	PubKey string
	// Name is the node's advertised name, as the node page shows it.
	Name string
	// Role is the node's role, as the API names it: "repeater", "room" or
	// "sensor". Any other role is refused with ErrRoleNotCarded (owner
	// decision Q3: companions get the plain site card).
	Role string
	// Area is the configured area the node is in (owner decision Q5), ""
	// when it is outside every area.
	Area string
	// LastHeard is when the node was last heard (last_heard || last_seen);
	// zero when unknown.
	LastHeard time.Time
	// Relay is the relay block (#697), drawn for repeaters and rooms only;
	// nil when there is none.
	Relay *RelayStats
	// Neighbors is the neighbour count; nil when unknown.
	Neighbors *int
	// FirstSeen is nodes.first_seen, the node page's "First Seen"; zero
	// when unknown (#857).
	FirstSeen time.Time
	// AvgHops is the health stats' avgHops: the rounded mean path length
	// of the node's transmissions in the in-memory window; nil when the
	// window holds none (the node page hides the row then, #857).
	AvgHops *int
	// OwnScope is nodes.default_scope as the node page reads it: nil for
	// NULL (no row drawn), "" for the legacy empty value ("unknown
	// scope"), else the scope ("#dk", or "*" for no default region).
	OwnScope *string
	// OwnScopeConfirmed is a non-empty default_scope_confirmed_at: the
	// scope was self-reported by the firmware, drawn as "confirmed".
	OwnScopeConfirmed bool
	// TransportedScopes are the region scopes a repeater or room has
	// carried (relay info TransportedScopes); drawn for those roles only.
	TransportedScopes []string
	// FloodInterval and ZeroHopInterval are the estimated advert
	// intervals (advertIntervals.flood / .zero_hop); nil when there is
	// no estimate.
	FloodInterval, ZeroHopInterval *AdvertInterval
	// AsOf is the moment the values were read. Required: the footer says
	// "as of" it, and LastHeard is drawn relative to it, never to the
	// wall clock, so the same input always gives the same card.
	AsOf time.Time
}

// RelayStats mirror the RepeaterRelayInfo fields the card shows. Each one
// is optional on its own.
type RelayStats struct {
	Relayed24h *int `json:"relayCount24h,omitempty"`
	Relayed1h  *int `json:"relayCount1h,omitempty"`
	// AirtimePct24h and AirtimePeakPct are seen airtime, a lower bound,
	// in percent (EstAirtimePct24h, EstAirtimePeakPct).
	AirtimePct24h  *float64 `json:"estAirtimePct24h,omitempty"`
	AirtimePeakPct *float64 `json:"estAirtimePeakPct,omitempty"`
}

// AdvertInterval is one estimated advert interval (#245): only an
// estimate with status "estimated" is passed; the card draws it at medium
// or high confidence only (#857: a low-confidence estimate is dropped).
type AdvertInterval struct {
	Seconds    int64
	Confidence string // "high", "medium" or "low"
	// Snapped is false when the estimate is outside the values the
	// firmware can be set to.
	Snapped bool
}

// Options are the operator's display choices.
type Options struct {
	// Palette is the card's colours; nil draws the shipped dark palette.
	Palette *Palette
	// HideNames draws "Node <8 hex>" instead of the node's name (the
	// profile card's names setting, owner decision Q2).
	HideNames bool
	// Site is the site name in the footer, e.g. "meshview.dk".
	Site string
}

var (
	// ErrRoleNotCarded is returned for a role that has no node card:
	// companions (usually people's phones) and anything unknown.
	ErrRoleNotCarded = errors.New("nodecard: no card for this role")
	// ErrNoAsOf is returned when NodeCardInput.AsOf is zero.
	ErrNoAsOf = errors.New("nodecard: AsOf is required")
)
