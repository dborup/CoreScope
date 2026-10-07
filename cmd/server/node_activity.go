package main

import (
	"strings"
	"time"
)

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
	return txConfirmsRelay(tx, newRelayKeyMatcher(key, pm))
}

// relayPrefixHexLens are the supported wire hash sizes (1/2/3 bytes).
var relayPrefixHexLens = [...]int{2, 4, 6}

// relayKeyMatcher answers confirmedRelayKey(token, pm) == key for one key
// without per-token allocation or prefix-map lookups: uniqueness of the key's
// own wire prefixes is decided once. Build one per key per request.
type relayKeyMatcher struct {
	key      string
	unique   [3]bool // indexed like relayPrefixHexLens
	listener bool
}

func newRelayKeyMatcher(key string, pm *prefixMap) relayKeyMatcher {
	m := relayKeyMatcher{key: key}
	if pm == nil {
		return m
	}
	_, m.listener = pm.nonRelay[key]
	for i, l := range relayPrefixHexLens {
		if len(key) < l || !isHexLower(key[:l]) {
			continue
		}
		cands := pm.m[key[:l]]
		m.unique[i] = len(cands) == 1 && strings.ToLower(cands[0].PublicKey) == key
	}
	return m
}

// possible reports whether any token can confirm key; listener-only nodes
// and empty keys never relay.
func (m relayKeyMatcher) possible() bool {
	return m.key != "" && !m.listener
}

func (m relayKeyMatcher) uniquePrefix(hexLen int) bool {
	for i, l := range relayPrefixHexLens {
		if l == hexLen {
			return m.unique[i]
		}
	}
	return false
}

func (m relayKeyMatcher) matches(token string) bool {
	if !m.possible() {
		return false
	}
	switch len(token) {
	case 64: // exact internal identity, not a wire prefix
		return len(m.key) == 64 && hexFoldEqual(token, m.key)
	case 2, 4, 6:
		return m.uniquePrefix(len(token)) && hexFoldEqual(token, m.key[:len(token)])
	}
	return false
}

// hexFoldEqual reports whether token equals the lowercase hex string lower,
// ignoring ASCII case, i.e. strings.ToLower(token) == lower && isHexLower(lower).
func hexFoldEqual(token, lower string) bool {
	if len(token) != len(lower) {
		return false
	}
	for i := 0; i < len(lower); i++ {
		c, want := token[i], lower[i]
		if !((want >= '0' && want <= '9') || (want >= 'a' && want <= 'f')) {
			return false
		}
		if c != want && !(want >= 'a' && c == want-'a'+'A') {
			return false
		}
	}
	return true
}

// txConfirmsRelay reports whether any raw observed flood path of tx names
// m.key with identity-safe evidence.
func txConfirmsRelay(tx *StoreTx, m relayKeyMatcher) bool {
	// Flood paths record already-observed hops. Direct paths list the
	// remaining intended route, not nodes that forwarded this observation.
	if !m.possible() || !txHasObservedFloodPath(tx) {
		return false
	}
	found := false
	forEachObservedRelayHop(tx, func(token string) bool {
		found = m.matches(token)
		return !found
	})
	return found
}

// relayTokenResolver memoizes confirmedRelayKey per distinct raw token for one
// request, so repeated hops cost a map lookup instead of ToLower + resolve.
// Not safe for concurrent use.
type relayTokenResolver struct {
	pm    *prefixMap
	cache map[string]string
}

func newRelayTokenResolver(pm *prefixMap) *relayTokenResolver {
	return &relayTokenResolver{pm: pm, cache: make(map[string]string, 1024)}
}

func (r *relayTokenResolver) key(token string) string {
	if key, ok := r.cache[token]; ok {
		return key
	}
	key := confirmedRelayKey(token, r.pm)
	r.cache[token] = key
	return key
}

// forEachObservedRelayHop visits the display path hops, then the hops of every
// other raw observation path. The display observation is only the longest
// route, so a shorter observation can be the sole raw evidence for a relay that
// resolved indexing attached to the transmission. visit returns false to stop.
// Repeated identical paths are rescanned instead of deduplicated: the scanner
// itself does not allocate (visit may), a per-transmission set would. Caller
// holds s.mu.
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

// updateNodeActivity is the single-transmission form of node activity: own
// advert, then relay evidence. Health endpoints use the two parts separately
// so each relay candidate is evaluated once per node.
func updateNodeActivity(tx *StoreTx, key string, pm *prefixMap, heard, advert *string) {
	updateOwnAdvertActivity(tx, key, heard, advert)
	heardAt, _ := parseRelayTS(*heard)
	updateRelayActivity(tx, newRelayKeyMatcher(key, pm), heard, &heardAt)
}

// updateOwnAdvertActivity advances advert and heard for a valid own ADVERT.
func updateOwnAdvertActivity(tx *StoreTx, key string, heard, advert *string) {
	if !txIsOwnAdvert(tx, key) {
		return
	}
	timestamp, valid := parseRelayTS(tx.FirstSeen)
	if !valid {
		return
	}
	if oldAdvert, _ := parseRelayTS(*advert); timestamp.After(oldAdvert) {
		*advert = tx.FirstSeen
	}
	if oldHeard, _ := parseRelayTS(*heard); timestamp.After(oldHeard) {
		*heard = tx.FirstSeen
	}
}

// updateRelayActivity advances heard (and its parsed heardAt) when tx is newer
// and confirms relay evidence. The timestamp gate runs first: path evidence
// can only matter for a newer transmission. Returns whether the evidence was
// evaluated. Never touches advert.
func updateRelayActivity(tx *StoreTx, m relayKeyMatcher, heard *string, heardAt *time.Time) bool {
	timestamp, valid := parseRelayTS(tx.FirstSeen)
	if !valid || !timestamp.After(*heardAt) {
		return false
	}
	if txIsInvalidAdvert(tx) || !txConfirmsRelay(tx, m) {
		return true
	}
	*heard, *heardAt = tx.FirstSeen, timestamp
	return true
}

// updateIndexedRelayActivityLocked folds the relay evidence behind
// GetRepeaterRelayInfo into health activity, over the same candidates
// (byPathHop full key, byNode, unique raw prefixes). Only heard changes: a
// relay is never an advert. seen is request-local scratch (cleared here) so a
// transmission's path evidence is evaluated at most once per node; candidates
// not newer than heard are skipped before any path work. Caller holds s.mu.
func (s *PacketStore) updateIndexedRelayActivityLocked(key string, pm *prefixMap, heard *string, seen map[int]struct{}) {
	m := newRelayKeyMatcher(key, pm)
	if !m.possible() {
		return
	}
	clear(seen)
	heardAt, _ := parseRelayTS(*heard)
	forEachRelayCandidate(s.byPathHop, s.byNode, m, func(tx *StoreTx, _ bool) {
		if _, done := seen[tx.ID]; done {
			return
		}
		if updateRelayActivity(tx, m, heard, &heardAt) {
			seen[tx.ID] = struct{}{}
		}
	})
}

func timestampPointer(ts string) *string {
	if ts == "" {
		return nil
	}
	return &ts
}
