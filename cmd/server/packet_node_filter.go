package main

import "strings"

// Bounds on GET /api/packets?nodes= (#335); not enforced yet.
const (
	maxPacketNodeFilters          = 50
	maxPacketNodeFilterBytes      = 4096
	maxPacketNodeFilterRawEntries = 2 * maxPacketNodeFilters
)

// parsePacketNodeFilters is the handler's existing split/trim loop, moved
// unchanged so the #335 tests can call it.
func parsePacketNodeFilters(raw string) ([]string, error) {
	var cleaned []string
	for _, pk := range strings.Split(raw, ",") {
		pk = strings.TrimSpace(pk)
		if pk != "" {
			cleaned = append(cleaned, pk)
		}
	}
	return cleaned, nil
}
