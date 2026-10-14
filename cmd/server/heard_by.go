package main

import (
	"context"
	"sort"
	"strings"

	"github.com/meshcore-analyzer/packetpath"
)

// Heard By (#76) splits the observers of a node's traffic into those that
// received the node's own transmission on air (direct) and the rest (relay).
//
// Only an ADVERT says who transmitted the copy an observer received, and only
// for the observation's own route type and path (firmware src/Mesh.cpp):
//   - a flood forwarder appends its own hash before retransmitting
//     (routeRecvPacket), so the last hop of a flood path is the node the
//     observer heard, and an empty flood path is the advertiser itself;
//   - sendZeroHop sends DIRECT / TRANSPORT_DIRECT with path_len 0, so an
//     empty direct path is the advertiser too;
//   - a direct forwarder removes itself from the path (removeSelfFromPath),
//     so a non-empty direct path names nobody that transmitted.
//
// DIRECT and FLOOD copies of one advert share a transmission (the content
// hash ignores the route bits), so the route type is read per observation,
// as the ingestor does (#308). A last hop counts only with the evidence the
// node's liveness already requires (#64, relayKeyMatcher): a full key or a
// wire prefix that is unique among relay-capable nodes, never a listener.

// HeardDirectObserver is an observer that received the node's own
// transmission. PacketCount, AvgSnr and AvgRssi cover those receptions only,
// so the signal values are the node's.
type HeardDirectObserver struct {
	ObserverID   string   `json:"observer_id"`
	ObserverName string   `json:"observer_name"`
	PacketCount  int      `json:"packetCount"`
	AvgSnr       *float64 `json:"avgSnr"`
	AvgRssi      *float64 `json:"avgRssi"`
	CanRelay     *bool    `json:"can_relay,omitempty"`

	// snrSum and snrN let stats.avgSnr be recomputed over the rows that stay
	// after the hidden filter (visibleHeardBy); they are not serialised.
	snrSum float64
	snrN   int
}

// HeardRelayObserver is an observer that saw the node's traffic, but no
// observation shows it received the node's own transmission. Its signal
// values belong to whichever node it heard, so none are reported.
// PacketCount is the number of the node's transmissions it saw.
type HeardRelayObserver struct {
	ObserverID   string `json:"observer_id"`
	ObserverName string `json:"observer_name"`
	PacketCount  int    `json:"packetCount"`
	CanRelay     *bool  `json:"can_relay,omitempty"`
}

// wireRouteFromRawHex encodes an observation frame's header route type for
// StoreObs.wireRoute: route type + 1, or 0 when the frame is missing or its
// header is not hex.
func wireRouteFromRawHex(rawHex string) uint8 {
	if rt, ok := packetpath.RouteTypeFromRawHex(rawHex); ok {
		return uint8(rt) + 1
	}
	return 0
}

// observationRouteType is the route type of one observation, by the rule of
// the ingestor's observationRouteType (#308): the observation's own frame
// header wins; without one, a transmission seen on both flood and direct
// routes is unknown; otherwise the transmission's route type.
func observationRouteType(o *StoreObs, tx *StoreTx) (int, bool) {
	if o.wireRoute != 0 {
		return int(o.wireRoute) - 1, true
	}
	if tx.routeMaskKnown && tx.routeMask&uint8(packetpath.RouteMaskFlood) != 0 && tx.routeMask&uint8(packetpath.RouteMaskDirect) != 0 {
		return 0, false
	}
	if tx.RouteType == nil {
		return 0, false
	}
	return *tx.RouteType, true
}

// pathLastHop returns the last hop and hop count of a path_json array. ok is
// false for a missing or unparseable path, which is never read as zero hop.
func pathLastHop(pathJSON string) (last string, hops int, ok bool) {
	if flatPathJSON(pathJSON) {
		visitPathJSONHops(pathJSON, func(token string) bool {
			last, hops = token, hops+1
			return true
		})
		return last, hops, true
	}
	parsed := parsePathJSON(pathJSON) // escapes or nulls; malformed is nil
	if len(parsed) == 0 {
		return "", 0, false
	}
	return parsed[len(parsed)-1], len(parsed), true
}

// heardDirect reports whether observation o of the ADVERT tx shows that the
// observer received the node's own transmission. own is whether the node is
// the advertiser; m matches the node as a relay hop.
func heardDirect(o *StoreObs, tx *StoreTx, own bool, m relayKeyMatcher) bool {
	rt, ok := observationRouteType(o, tx)
	if !ok {
		return false
	}
	last, hops, ok := pathLastHop(o.PathJSON)
	if !ok {
		return false
	}
	switch rt {
	case RouteTransportFlood, RouteFlood:
		if hops == 0 {
			return own
		}
		return m.matches(last)
	case RouteDirect, RouteTransportDirect:
		return hops == 0 && own
	}
	return false
}

type heardByRow struct {
	id, name        string
	last            *StoreTx // last transmission counted in packets
	packets         int
	direct          int
	snrSum, rssiSum float64
	snrN, rssiN     int
}

// heardByAccumulator builds one node's Heard By lists from every observation
// of the node's transmissions in one pass: O(observations), one map lookup
// per observation. reset reuses its storage for the next node, so a bulk
// request allocates rows only while the largest observer set grows.
type heardByAccumulator struct {
	pm     *prefixMap
	key    string // lowercase pubkey
	m      relayKeyMatcher
	index  map[string]int // observer id -> rows index
	rows   []heardByRow
	snrSum float64
	snrN   int
}

func newHeardByAccumulator(pubkey string, pm *prefixMap) *heardByAccumulator {
	a := &heardByAccumulator{pm: pm, index: make(map[string]int)}
	a.reset(pubkey)
	return a
}

func (a *heardByAccumulator) reset(pubkey string) {
	a.key = strings.ToLower(pubkey)
	a.m = newRelayKeyMatcher(a.key, a.pm)
	clear(a.index)
	a.rows = a.rows[:0]
	a.snrSum, a.snrN = 0, 0
}

// advertOrigin returns whether tx is an ADVERT whose signature is not known
// to be invalid (txIsInvalidAdvert), and its advertiser, from one decode.
func advertOrigin(tx *StoreTx) (pubkey string, ok bool) {
	if tx.PayloadType == nil || *tx.PayloadType != PayloadADVERT {
		return "", false
	}
	decoded := tx.ParsedDecoded()
	if valid, isBool := decoded["signatureValid"].(bool); isBool && !valid {
		return "", false
	}
	pubkey, _ = decoded["pubKey"].(string)
	return pubkey, true
}

func (a *heardByAccumulator) add(tx *StoreTx) {
	origin, advert := advertOrigin(tx)
	own := advert && origin != "" && strings.EqualFold(origin, a.key)
	for _, o := range tx.Observations {
		if o == nil || o.ObserverID == "" {
			continue
		}
		i, seen := a.index[o.ObserverID]
		if !seen {
			i = len(a.rows)
			a.rows = append(a.rows, heardByRow{id: o.ObserverID, name: o.ObserverName})
			a.index[o.ObserverID] = i
		}
		r := &a.rows[i]
		if r.last != tx {
			r.last = tx
			r.packets++
		}
		// A node never receives its own transmission.
		if !advert || strings.EqualFold(o.ObserverID, a.key) || !heardDirect(o, tx, own, a.m) {
			continue
		}
		r.direct++
		if o.SNR != nil {
			r.snrSum += *o.SNR
			r.snrN++
			a.snrSum += *o.SNR
			a.snrN++
		}
		if o.RSSI != nil {
			r.rssiSum += *o.RSSI
			r.rssiN++
		}
	}
}

func meanOrNil(sum float64, n int) *float64 {
	if n == 0 {
		return nil
	}
	v := sum / float64(n)
	return &v
}

// avgSnr is the mean SNR over the node's direct receptions.
func (a *heardByAccumulator) avgSnr() *float64 { return meanOrNil(a.snrSum, a.snrN) }

// lists returns both lists, most packets first and then by observer id
// (#321: total order). canRelay may be nil.
func (a *heardByAccumulator) lists(canRelay func(id string) *bool) ([]HeardDirectObserver, []HeardRelayObserver) {
	nDirect := 0
	for i := range a.rows {
		if a.rows[i].direct > 0 {
			nDirect++
		}
	}
	direct := make([]HeardDirectObserver, 0, nDirect)
	relay := make([]HeardRelayObserver, 0, len(a.rows)-nDirect)
	for i := range a.rows {
		r := &a.rows[i]
		var cr *bool
		if canRelay != nil {
			cr = canRelay(r.id)
		}
		if r.direct > 0 {
			direct = append(direct, HeardDirectObserver{ObserverID: r.id, ObserverName: r.name, PacketCount: r.direct,
				AvgSnr: meanOrNil(r.snrSum, r.snrN), AvgRssi: meanOrNil(r.rssiSum, r.rssiN), CanRelay: cr,
				snrSum: r.snrSum, snrN: r.snrN})
		} else {
			relay = append(relay, HeardRelayObserver{ObserverID: r.id, ObserverName: r.name, PacketCount: r.packets, CanRelay: cr})
		}
	}
	sort.Slice(direct, func(i, j int) bool {
		if direct[i].PacketCount != direct[j].PacketCount {
			return direct[i].PacketCount > direct[j].PacketCount
		}
		return direct[i].ObserverID < direct[j].ObserverID
	})
	sort.Slice(relay, func(i, j int) bool {
		if relay[i].PacketCount != relay[j].PacketCount {
			return relay[i].PacketCount > relay[j].PacketCount
		}
		return relay[i].ObserverID < relay[j].ObserverID
	})
	return direct, relay
}

// heardByHiddenFilter returns the predicate that hides an observer row whose
// identity is hidden (identityHidden: node or observer blacklist, or a
// recorded or current name with a hidden prefix). The current names of all
// ids are read in one query; a failed read is returned so callers fail closed.
func (s *Server) heardByHiddenFilter(ctx context.Context, ids []string) (func(id, name string) bool, error) {
	var live map[string][]string
	if s.cfg != nil && len(s.cfg.EnforcedHiddenNamePrefixes()) > 0 {
		var err error
		if live, err = s.hiddenIdentityNames(ctx, ids); err != nil {
			return nil, err
		}
	}
	return func(id, name string) bool {
		return identityHidden(s.cfg, id, name) || (live != nil && identityHidden(s.cfg, id, live[strings.ToLower(id)]...))
	}, nil
}

func heardByIDs(ids []string, direct []HeardDirectObserver, relay []HeardRelayObserver) []string {
	for _, o := range direct {
		ids = append(ids, o.ObserverID)
	}
	for _, o := range relay {
		ids = append(ids, o.ObserverID)
	}
	return ids
}

func visibleHeardBy(direct []HeardDirectObserver, relay []HeardRelayObserver, hidden func(id, name string) bool) ([]HeardDirectObserver, []HeardRelayObserver) {
	d := make([]HeardDirectObserver, 0, len(direct))
	for _, o := range direct {
		if !hidden(o.ObserverID, o.ObserverName) {
			d = append(d, o)
		}
	}
	r := make([]HeardRelayObserver, 0, len(relay))
	for _, o := range relay {
		if !hidden(o.ObserverID, o.ObserverName) {
			r = append(r, o)
		}
	}
	return d, r
}

// applyVisibleHeardBy removes hidden observers from entry's Heard By lists
// and, when any direct row went, recomputes stats.avgSnr over the rows that
// stay, so a hidden observer's SNR never outlives its row. entry is a
// GetNodeHealth result or one GetBulkHealth row.
func applyVisibleHeardBy(entry map[string]interface{}, hidden func(id, name string) bool) {
	direct, _ := entry["directObservers"].([]HeardDirectObserver)
	relay, _ := entry["relayObservers"].([]HeardRelayObserver)
	visible, relay := visibleHeardBy(direct, relay, hidden)
	entry["directObservers"], entry["relayObservers"] = visible, relay
	stats, ok := entry["stats"].(NodeHealthStats)
	if !ok || len(visible) == len(direct) {
		return
	}
	var sum float64
	var n int
	for _, o := range visible {
		sum += o.snrSum
		n += o.snrN
	}
	stats.AvgSnr = meanOrNil(sum, n)
	entry["stats"] = stats
}
