package main

import "strings"

// NodeHealthStats keeps packet analytics separate from identity-safe activity.
// LastHeard is own advert or unambiguous relay evidence; LastAdvert is strictly
// the node's own ADVERT. Unknown timestamps serialize as null, never last_seen:
// the ingestor may refresh last_seen from a safely resolved relay hop.
type NodeHealthStats struct {
	TotalTransmissions int      `json:"totalTransmissions"`
	TotalObservations  int      `json:"totalObservations"`
	TotalPackets       int      `json:"totalPackets"`
	PacketsToday       int      `json:"packetsToday"`
	AvgSnr             *float64 `json:"avgSnr"`
	AvgHops            *int     `json:"avgHops,omitempty"`
	LastHeard          *string  `json:"lastHeard"`
	LastAdvert         *string  `json:"lastAdvert"`
}

// relayPrefixMapLocked reuses the existing bounded node cache. Missing node
// metadata fails closed for short hashes (e.g. during startup/schema failure).
// Caller holds s.mu. There is no per-node or per-packet SQL query here.
func (s *PacketStore) relayPrefixMapLocked() *prefixMap {
	if s.db == nil || s.db.conn == nil {
		return s.nodePM
	}
	_, pm := s.getCachedNodesAndPM()
	return pm
}

// confirmedRelayKey deliberately does not use graph/geo/fallback guesses.
// A full raw identity or a unique wire prefix is evidence; a colliding prefix
// remains unknown even if a heuristic previously indexed it under a full key.
func confirmedRelayKey(token string, pm *prefixMap) string {
	token = strings.ToLower(token)
	if !isHexLower(token) {
		return ""
	}
	key := ""
	if len(token) == 64 { // exact internal identity, not a wire prefix
		key = token
	} else if len(token) == 2 || len(token) == 4 || len(token) == 6 {
		key = uniqueResolve(pm, token)
	}
	if pm != nil {
		if _, listener := pm.nonRelay[key]; listener {
			return ""
		}
	}
	return key
}

func txHasConfirmedRelay(tx *StoreTx, key string, pm *prefixMap) bool {
	// Flood paths record already-observed hops. Direct paths list the
	// remaining intended route, not nodes that forwarded this observation.
	if !txHasObservedFloodPath(tx) {
		return false
	}
	for _, token := range txGetParsedPath(tx) {
		if confirmedRelayKey(token, pm) == key {
			return true
		}
	}
	return false
}

func txHasObservedFloodPath(tx *StoreTx) bool {
	return tx.RouteType != nil && (*tx.RouteType == 0 || *tx.RouteType == routeTypeFlood)
}

func txIsOwnAdvert(tx *StoreTx, key string) bool {
	if tx.PayloadType == nil || *tx.PayloadType != payloadTypeAdvert {
		return false
	}
	decoded := tx.ParsedDecoded()
	if valid, ok := decoded["signatureValid"].(bool); ok && !valid {
		return false
	}
	pubkey, _ := decoded["pubKey"].(string)
	return pubkey != "" && strings.EqualFold(pubkey, key)
}

func updateNodeActivity(tx *StoreTx, key string, pm *prefixMap, heard, advert *string) {
	if tx.PayloadType != nil && *tx.PayloadType == payloadTypeAdvert {
		if valid, ok := tx.ParsedDecoded()["signatureValid"].(bool); ok && !valid {
			return
		}
	}
	timestamp, valid := parseRelayTS(tx.FirstSeen)
	if !valid {
		return
	}
	ownAdvert := txIsOwnAdvert(tx, key)
	oldAdvert, _ := parseRelayTS(*advert)
	if ownAdvert && timestamp.After(oldAdvert) {
		*advert = tx.FirstSeen
	}
	oldHeard, _ := parseRelayTS(*heard)
	if (ownAdvert || txHasConfirmedRelay(tx, key, pm)) && timestamp.After(oldHeard) {
		*heard = tx.FirstSeen
	}
}

func timestampPointer(ts string) *string {
	if ts == "" {
		return nil
	}
	return &ts
}
