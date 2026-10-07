package main

import (
	"encoding/json"
	"log"
)

// Privacy filtering for /api/analytics/topology (issue #91).
//
// computeAnalyticsTopology builds its result from maps and slices of maps.
// The previous filter type-asserted the Topology* structs instead
// ([]TopRepeater, map[string]*ObserverReach, ...), every assertion missed, and
// blacklisted nodes were returned unfiltered. This filter works on the shape
// the store actually produces, accepts the other shapes the same data can take
// (JSON-decoded []interface{}, the typed structs), and fails closed on
// anything else: a part or entry it cannot read is dropped, never passed on.
//
// The store hands the handler its shared cached result — the recomputer
// snapshot or a topoCache entry — so nothing here writes to its input. Parts
// are rebuilt only when something in them is hidden; with nothing to hide the
// input is returned as is.

// topologyListParts are the list-valued parts that carry node pubkeys.
// perObserverReach (an object of observers → rings → nodes) is handled
// separately.
var topologyListParts = []string{"topRepeaters", "topPairs", "bestPathList", "multiObsNodes"}

// filterBlacklistedFromTopology returns data with every entry that refers to a
// blacklisted node (Config.IsBlacklisted: trimmed, case-insensitive) or to a
// name hidden by HiddenNamePrefixes (#1181) removed from topRepeaters,
// topPairs (either side), bestPathList, multiObsNodes and
// perObserverReach{}.rings[].nodes[]. The input is never modified.
func (s *Server) filterBlacklistedFromTopology(data map[string]interface{}) map[string]interface{} {
	if data == nil || s == nil || s.cfg == nil {
		return data
	}
	var out map[string]interface{} // copy-on-write top level
	set := func(key string, v interface{}) {
		if out == nil {
			out = make(map[string]interface{}, len(data))
			for k, val := range data {
				out[k] = val
			}
		}
		out[key] = v
	}
	for _, key := range topologyListParts {
		v, ok := data[key]
		if !ok {
			continue
		}
		hidden := (*Server).topologyNodeHidden
		if key == "topPairs" {
			hidden = (*Server).topologyPairHidden
		}
		if nv, changed := s.filterTopologyList(key, v, hidden); changed {
			set(key, nv)
		}
	}
	if v, ok := data["perObserverReach"]; ok {
		if nv, changed := s.filterTopologyReach(v); changed {
			set("perObserverReach", nv)
		}
	}
	if out == nil {
		return data
	}
	return out
}

// topologyPubkeyHidden: a string pubkey is checked against the blacklist; an
// absent (nil) one is an unresolved hop and has nothing to check; anything
// else cannot be checked and is treated as hidden.
func (s *Server) topologyPubkeyHidden(v interface{}) bool {
	switch pk := v.(type) {
	case nil:
		return false
	case string:
		return s.cfg.IsBlacklisted(pk)
	default:
		return true
	}
}

// topologyNameHidden applies HiddenNamePrefixes; a name of an unexpected type
// cannot be checked and is treated as hidden.
func (s *Server) topologyNameHidden(v interface{}) bool {
	switch name := v.(type) {
	case nil:
		return false
	case string:
		return s.cfg.IsNameHidden(name)
	default:
		return true
	}
}

// topRepeaters, bestPathList, multiObsNodes and perObserverReach nodes. The
// entry carries the node's resolved name, so nothing is looked up per entry
// (an isPubkeyHidden database query per bestPathList entry was up to 50
// queries a request).
func (s *Server) topologyNodeHidden(e map[string]interface{}) bool {
	return s.topologyPubkeyHidden(e["pubkey"]) || s.topologyNameHidden(e["name"])
}

// topPairs: the pair goes if either side is hidden.
func (s *Server) topologyPairHidden(e map[string]interface{}) bool {
	return s.topologyPubkeyHidden(e["pubkeyA"]) || s.topologyPubkeyHidden(e["pubkeyB"]) ||
		s.topologyNameHidden(e["nameA"]) || s.topologyNameHidden(e["nameB"])
}

// topologyEntries reads a list part as its entries. ok is false when v is not
// a list of objects. native is true when v is already []map[string]interface{}
// (the store's shape) or nil; other readable shapes are converted, the typed
// structs via a JSON round trip.
func topologyEntries(v interface{}) (entries []map[string]interface{}, native, ok bool) {
	switch l := v.(type) {
	case nil:
		return nil, true, true
	case []map[string]interface{}:
		return l, true, true
	case []interface{}:
		entries = make([]map[string]interface{}, 0, len(l))
		for _, e := range l {
			m, isMap := e.(map[string]interface{})
			if !isMap {
				return nil, false, false
			}
			entries = append(entries, m)
		}
		return entries, false, true
	}
	// A local target, not the named result: taking the result's address
	// would move it to the heap on every call, the native path included.
	var converted []map[string]interface{}
	b, err := json.Marshal(v)
	if err != nil || json.Unmarshal(b, &converted) != nil {
		return nil, false, false
	}
	for _, e := range converted {
		if e == nil {
			return nil, false, false
		}
	}
	return converted, false, true
}

// filterTopologyList returns the part without hidden entries. changed is false
// (and v is returned as is) when there is nothing to remove. A part that is not
// a list of objects is replaced by an empty list — fail closed.
//
// hidden is a method expression rather than a bound method value, so passing
// it does not allocate on the per-request path.
func (s *Server) filterTopologyList(key string, v interface{}, hidden func(*Server, map[string]interface{}) bool) (interface{}, bool) {
	entries, _, ok := topologyEntries(v)
	if !ok {
		log.Printf("[privacy] topology: %s has an unexpected shape (%T); dropped", key, v)
		return []map[string]interface{}{}, true
	}
	first := -1
	for i, e := range entries {
		if hidden(s, e) {
			first = i
			break
		}
	}
	if first < 0 {
		return v, false
	}
	kept := make([]map[string]interface{}, first, len(entries)-1)
	copy(kept, entries[:first])
	for _, e := range entries[first+1:] {
		if !hidden(s, e) {
			kept = append(kept, e)
		}
	}
	return kept, true
}

// filterTopologyReach filters perObserverReach: observer id → {observer_name,
// rings: [{hops, nodes: [...]}]}. Observers, rings and nodes are copied only
// where something changes (nothing is allocated when nothing is hidden);
// anything unreadable is dropped.
func (s *Server) filterTopologyReach(v interface{}) (interface{}, bool) {
	var observers map[string]interface{}
	converted := false
	switch m := v.(type) {
	case nil:
		return v, false
	case map[string]interface{}:
		observers = m
	default: // map[string]*ObserverReach or an unknown shape
		var decoded map[string]interface{} // local: see topologyEntries
		b, err := json.Marshal(v)
		if err != nil || json.Unmarshal(b, &decoded) != nil {
			log.Printf("[privacy] topology: perObserverReach has an unexpected shape (%T); dropped", v)
			return map[string]interface{}{}, true
		}
		observers = decoded
		converted = true
	}
	var out map[string]interface{} // copy-on-write
	touch := func() {
		if out == nil {
			out = make(map[string]interface{}, len(observers))
			for k, val := range observers {
				out[k] = val
			}
		}
	}
	if converted {
		touch()
	}
	for id, ov := range observers {
		obs, ok := ov.(map[string]interface{})
		if !ok {
			touch()
			delete(out, id)
			continue
		}
		rings, ringsChanged, ok := s.filterReachRings(obs["rings"])
		if !ok {
			log.Printf("[privacy] topology: perObserverReach[%s].rings has an unexpected shape; dropped", id)
			touch()
			delete(out, id)
			continue
		}
		if !ringsChanged {
			continue
		}
		cp := make(map[string]interface{}, len(obs))
		for k, val := range obs {
			cp[k] = val
		}
		cp["rings"] = rings
		touch()
		out[id] = cp
	}
	if out == nil {
		return v, false
	}
	return out, true
}

// filterReachRings filters the nodes of each ring; rings are copied only when
// their nodes change, and a non-native (converted) ring list is always
// returned as the converted copy.
func (s *Server) filterReachRings(v interface{}) (rings interface{}, changed, ok bool) {
	entries, native, ok := topologyEntries(v)
	if !ok {
		return nil, false, false
	}
	var out []map[string]interface{} // copy-on-write
	for i, ring := range entries {
		nodes, nodesChanged := s.filterTopologyList("perObserverReach nodes", ring["nodes"], (*Server).topologyNodeHidden)
		if !nodesChanged {
			continue
		}
		if out == nil {
			out = make([]map[string]interface{}, len(entries))
			copy(out, entries)
		}
		cp := make(map[string]interface{}, len(ring))
		for k, val := range ring {
			cp[k] = val
		}
		cp["nodes"] = nodes
		out[i] = cp
	}
	switch {
	case out != nil:
		return out, true, true
	case !native:
		return entries, true, true
	default:
		return v, false, true
	}
}
