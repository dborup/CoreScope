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
	found := false
	forEachObservedRelayHop(tx, func(token string) bool {
		found = confirmedRelayKey(token, pm) == key
		return !found
	})
	return found
}

// forEachObservedRelayHop visits the display path hops, then the hops of every
// other raw observation path. The display observation is only the longest
// route, so a shorter observation can be the sole raw evidence for a relay that
// resolved indexing attached to the transmission. visit returns false to stop.
// Repeated identical paths are rescanned instead of deduplicated: scanning
// does not allocate, a per-transmission set would. Caller holds s.mu.
func forEachObservedRelayHop(tx *StoreTx, visit func(token string) bool) {
	for _, token := range txGetParsedPath(tx) {
		if !visit(token) {
			return
		}
	}
	for _, obs := range tx.Observations {
		if obs == nil || obs.PathJSON == tx.PathJSON {
			continue
		}
		if !visitPathJSONHops(obs.PathJSON, visit) {
			return
		}
	}
}

// visitPathJSONHops visits the hops of a path_json array with the same accept
// set as parsePathJSON, without reflection or allocation on the common flat
// string-array shape. Malformed input yields no hops, exactly like the display
// path; escapes and null elements fall back to encoding/json. Returns false if
// visit stopped early.
func visitPathJSONHops(pathJSON string, visit func(string) bool) bool {
	if !flatPathJSON(pathJSON) {
		if strings.IndexByte(pathJSON, '\\') >= 0 || strings.Contains(pathJSON, "null") {
			for _, token := range parsePathJSON(pathJSON) {
				if !visit(token) {
					return false
				}
			}
		}
		return true
	}
	for i := 0; ; {
		open := strings.IndexByte(pathJSON[i:], '"')
		if open < 0 {
			return true
		}
		start := i + open + 1
		end := start + strings.IndexByte(pathJSON[start:], '"')
		if !visit(pathJSON[start:end]) {
			return false
		}
		i = end + 1
	}
}

// flatPathJSON reports whether s is a JSON array of plain (escape-free)
// strings, allowing JSON whitespace. Validation precedes any visit so a
// malformed tail cannot leak earlier hops as evidence.
func flatPathJSON(s string) bool {
	i, n := 0, len(s)
	skip := func() {
		for i < n && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
			i++
		}
	}
	skip()
	if i == n || s[i] != '[' {
		return false
	}
	i++
	skip()
	if i < n && s[i] == ']' {
		i++
		skip()
		return i == n
	}
	for {
		if i == n || s[i] != '"' {
			return false
		}
		i++
		for i < n && s[i] != '"' {
			if s[i] == '\\' || s[i] < 0x20 {
				return false
			}
			i++
		}
		if i == n {
			return false
		}
		i++
		skip()
		if i == n {
			return false
		}
		if s[i] == ']' {
			i++
			skip()
			return i == n
		}
		if s[i] != ',' {
			return false
		}
		i++
		skip()
	}
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

func txIsInvalidAdvert(tx *StoreTx) bool {
	if tx.PayloadType == nil || *tx.PayloadType != payloadTypeAdvert {
		return false
	}
	valid, ok := tx.ParsedDecoded()["signatureValid"].(bool)
	return ok && !valid
}

func updateNodeActivity(tx *StoreTx, key string, pm *prefixMap, heard, advert *string) {
	if txIsInvalidAdvert(tx) {
		return
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

// updateIndexedRelayActivityLocked folds the relay evidence behind
// GetRepeaterRelayInfo into health activity. byNode only holds decoded and
// resolved-path membership, so a unique raw-prefix relay would otherwise be
// RelayActive while health stayed silent. Only heard changes: a relay is never
// an advert. Caller holds s.mu; cost is the node's own index buckets.
func (s *PacketStore) updateIndexedRelayActivityLocked(key string, pm *prefixMap, heard *string) {
	forEachConfirmedRelayTx(s.byPathHop, key, pm, nil, func(tx *StoreTx, _ bool) {
		if txIsInvalidAdvert(tx) {
			return
		}
		timestamp, valid := parseRelayTS(tx.FirstSeen)
		oldHeard, _ := parseRelayTS(*heard)
		if valid && timestamp.After(oldHeard) {
			*heard = tx.FirstSeen
		}
	})
}

func timestampPointer(ts string) *string {
	if ts == "" {
		return nil
	}
	return &ts
}
