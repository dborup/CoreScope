// Package regions holds the region-name normalization rules shared between
// cmd/ingestor (which derives HMAC keys from hashRegions) and cmd/server
// (which only needs the names to diff configured regions against observed
// scope_name values for region-utilization analytics). Keeping this in one
// place avoids the two normalizing hashRegions independently and drifting
// apart on the trim/prefix/dedupe rules.
package regions

import (
	"crypto/sha256"
	"strings"
)

// Normalize applies the hashRegions name convention to a single raw config
// entry: trim whitespace, ensure a leading "#", and reject blank entries.
// Returns ok=false for an entry that normalizes to nothing.
func Normalize(raw string) (name string, ok bool) {
	name = strings.TrimSpace(raw)
	if name == "" {
		return "", false
	}
	if !strings.HasPrefix(name, "#") {
		name = "#" + name
	}
	return name, true
}

// NormalizeNames normalizes and deduplicates a raw hashRegions list,
// preserving first-seen order.
func NormalizeNames(raw []string) []string {
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		name, ok := Normalize(r)
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// Key derives a region's 16-byte transport key from its exact name, as
// MeshCore's TransportKeyStore::getAutoKeyFor does: SHA-256 of the name
// bytes, truncated to TransportKey.key. Configured and admin-approved names
// both use it, so the two can never derive different keys.
func Key(name string) []byte {
	h := sha256.Sum256([]byte(name))
	return h[:16]
}
