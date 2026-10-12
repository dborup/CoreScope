package main

import (
	"fmt"
	"strings"
)

// Bounds on GET /api/packets?nodes= (#335). Every distinct entry costs one
// SQLite lookup (resolveNodePubkey), and the in-memory path runs those lookups
// while holding the packet store's read lock, so the list is checked before any
// lookup, query or lock. No UI page sends nodes= (the Packets page uses node=).
const (
	maxPacketNodeFilters = 50
	// MeshCore.h: PUB_KEY_SIZE 32 = 64 hex chars, longer than any advert name
	// (MAX_ADVERT_DATA_SIZE 32). 50 full keys joined by ", " are 3,298 bytes.
	maxPacketNodeFilterBytes = 4096
	// Empty and duplicate entries collapse, but each still costs a split and a
	// trim, so the raw list is bounded too.
	maxPacketNodeFilterRawEntries = 2 * maxPacketNodeFilters
)

// parsePacketNodeFilters trims entries, drops empty ones and collapses exact
// duplicates, keeping first-seen order. Entries match a public key or name
// exactly, so case variants stay distinct. An all-empty list yields no entries.
func parsePacketNodeFilters(raw string) ([]string, error) {
	if len(raw) > maxPacketNodeFilterBytes {
		return nil, fmt.Errorf("nodes must be at most %d characters", maxPacketNodeFilterBytes)
	}
	if strings.Count(raw, ",") >= maxPacketNodeFilterRawEntries {
		return nil, fmt.Errorf("nodes accepts at most %d comma-separated entries", maxPacketNodeFilterRawEntries)
	}
	parts := strings.Split(raw, ",")
	seen := make(map[string]struct{}, len(parts))
	cleaned := make([]string, 0, len(parts))
	for _, pk := range parts {
		pk = strings.TrimSpace(pk)
		if pk == "" {
			continue
		}
		if _, dup := seen[pk]; dup {
			continue
		}
		if len(cleaned) == maxPacketNodeFilters {
			return nil, fmt.Errorf("nodes accepts at most %d distinct entries", maxPacketNodeFilters)
		}
		seen[pk] = struct{}{}
		cleaned = append(cleaned, pk)
	}
	return cleaned, nil
}
