package main

import (
	"sort"
	"time"
)

const (
	maxRegionsMembershipEntries = 1000000
	maxRegionsMembershipNodes   = 10000
)

// eligibleRegionsObservation is the single provenance gate for Regions
// memberships and directed steps. Firmware builds flood paths, but consumes
// direct paths as intended remaining hops (Mesh.cpp routeRecvPacket and
// removeSelfFromPath). Canonical transport codes only associate the own frame
// with its stored scope; they do not independently authenticate the scope name.
// A known direct/unscoped observation is deliberately excluded, not missing
// evidence. Unknown headers or unmatched scoped flood frames are incomplete.
func eligibleRegionsObservation(obs *StoreObs, canonical uint32, known bool) (eligible, incomplete bool) {
	if obs.wireRoute == 0 {
		return false, true
	}
	if obs.wireRoute != 1 { // Explicit ROUTE_TYPE_TRANSPORT_FLOOD, encoded +1.
		return false, false
	}
	if !known || !obs.transportKnown || obs.wireTransport != canonical {
		return false, true
	}
	return true, false
}

type regionsNodeMembership struct {
	scopes          []regionScopeRelay
	scopesTruncated bool
}

// Kept separate from generic relay/liveness info: Regions must neither inherit
// its canonical-route heuristics nor broaden its existing public semantics.
type regionsMembershipSnapshot struct {
	nodes      map[string]regionsNodeMembership
	incomplete bool
}

type regionsMembershipBuilder struct {
	entries    map[string][]relayEntry
	remaining  int
	incomplete bool
}

func newRegionsMembershipBuilder() *regionsMembershipBuilder {
	return &regionsMembershipBuilder{entries: make(map[string][]relayEntry), remaining: maxRegionsMembershipEntries}
}

// retainLatestRegionScope keeps the newest 32 names with bounded scratch
// storage. Both Latest and hourly history use this exact tie-breaking rule.
func retainLatestRegionScope(latest map[string]time.Time, name string, when time.Time) (truncated bool) {
	if have, exists := latest[name]; exists {
		if when.After(have) {
			latest[name] = when
		}
		return false
	}
	if len(latest) < maxTransportedScopes {
		latest[name] = when
		return false
	}
	var oldestName string
	var oldest time.Time
	for scope, stamp := range latest {
		if oldestName == "" || stamp.Before(oldest) || (stamp.Equal(oldest) && scope > oldestName) {
			oldestName, oldest = scope, stamp
		}
	}
	if when.After(oldest) || (when.Equal(oldest) && name < oldestName) {
		delete(latest, oldestName)
		latest[name] = when
	}
	return true
}

// Called once per resolved node per distinct transmission, outside the packet
// lock. This independent budget is never spent by directed edges or edge cells.
func (b *regionsMembershipBuilder) add(key string, tx regionsRouteTx) {
	if b.remaining == 0 {
		b.incomplete = true
		return
	}
	if _, exists := b.entries[key]; !exists && len(b.entries) >= maxRegionsMembershipNodes {
		b.incomplete = true
		return
	}
	// Only time/scope are consumed below; payload exclusion was already made
	// before capture. No canonical/direct relay-entry fields are reused.
	b.entries[key] = append(b.entries[key], relayEntry{t: tx.when, parsed: true, valid: true, scope: tx.scope})
	b.remaining--
}

func (b *regionsMembershipBuilder) build(now time.Time, incomplete bool) (*regionsMembershipSnapshot, *regionsRelayHistory) {
	out := &regionsMembershipSnapshot{nodes: make(map[string]regionsNodeMembership, len(b.entries)), incomplete: incomplete || b.incomplete}
	history := newRegionsHistoryBuilder(now)
	keys := make([]string, 0, len(b.entries))
	for key := range b.entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		entries := b.entries[key]
		// At most 32 newest scopes per key, with bounded scratch space even
		// if an adversary creates a new scope for every retained transmission.
		latest := make(map[string]time.Time, maxTransportedScopes)
		truncated := false
		for _, e := range entries {
			truncated = retainLatestRegionScope(latest, e.scope, e.t) || truncated
		}
		scopes, _ := newestRegionScopeRelays(latest)
		out.nodes[key] = regionsNodeMembership{scopes: scopes, scopesTruncated: truncated}
		history.add(key, entries)
	}
	return out, history.history
}
