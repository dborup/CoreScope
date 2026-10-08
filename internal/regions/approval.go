package regions

import (
	"errors"
	"unicode/utf8"
)

// ValidateCandidate accepts an exact OTA scope token, without normalizing
// case or adding '#': those bytes determine its HMAC key.
func ValidateCandidate(name string) error {
	if len(name) < 2 || len(name) > 30 || name[0] != '#' || !utf8.ValidString(name) {
		return errors.New("scope must start with # and be 2–30 valid UTF-8 bytes")
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		// MeshCore RegionMap::is_name_char accepts these exact byte classes.
		// Exclude DEL even though firmware accepts it, since it is an invisible
		// control character. The UTF-8 check above prevents malformed names.
		if !(c == '-' || c == '$' || c == '#' || (c >= '0' && c <= '9') || (c >= 'A' && c != 0x7f)) {
			return errors.New("scope contains a character not accepted by MeshCore firmware")
		}
	}
	return nil
}
