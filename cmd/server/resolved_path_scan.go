package main

import "strings"

// In-place reading of persisted resolved_path values (#533).
//
// The ingestor writes observations.resolved_path with json.Marshal of a
// []*string: a compact array of full pubkeys and nulls, e.g.
// ["a1…",null,"b2…"]. Load, the chunk merge and live ingest read one per
// observation, 2.5M at startup on the large instance, and decoding them with
// encoding/json was 17% of Load's allocations (pprof, see the PR
// description).
//
// scanResolvedPathPubkeys reads that compact form directly. It accepts only
// what json.Unmarshal would return unchanged: strings of printable ASCII
// without escapes, and null. Anything else -- whitespace, an escape, a byte
// outside 0x20..0x7e (which the decoder would reject or rewrite to U+FFFD), a
// number, a nested value, trailing data -- makes it report false, and the
// caller decodes with encoding/json as before. So the result never differs
// from extractResolvedPubkeys(unmarshalResolvedPath(s)): same strings
// verbatim (no case folding or validation, as before), same order, duplicates
// kept, null and "" dropped (FuzzPersistedRelayPath_533).
//
// The pubkeys are substrings of s. Callers that retain them must copy them
// (decodePersistedRelayPath does), or one map key keeps the whole column
// value alive.
//
// No string is shared through the #489 pool: every pubkey the store retains
// from here is a byNode/nodeHashes/byPathHop map key, which already holds one
// copy per distinct pubkey, so a pool would add a lock and a lookup per hop
// and save no retained bytes.

// scanResolvedPathPubkeys appends the non-empty pubkeys of the compact
// resolved_path s to dst. It returns dst unchanged and false when s is not
// in the compact form.
func scanResolvedPathPubkeys(dst []string, s string) ([]string, bool) {
	n := len(s)
	if n < 2 || s[0] != '[' || s[n-1] != ']' {
		return dst, false
	}
	start := len(dst)
	if n == 2 {
		return dst, true
	}
	i := 1
	for {
		switch {
		case s[i] == '"':
			j := i + 1
			for j < n && s[j] != '"' {
				if c := s[j]; c < 0x20 || c > 0x7e || c == '\\' {
					return dst[:start], false
				}
				j++
			}
			if j == n {
				return dst[:start], false
			}
			if j > i+1 {
				dst = append(dst, s[i+1:j])
			}
			i = j + 1
		case strings.HasPrefix(s[i:], "null"):
			i += 4
		default:
			return dst[:start], false
		}
		if i == n-1 {
			return dst, true
		}
		if s[i] != ',' || i+1 >= n-1 {
			return dst[:start], false
		}
		i++
	}
}
