// Package channelregistry is the shared contract for publicly suggested
// hashtag channels: name validation, limits, the request/result file queue
// between the read-only server and the writer-owning ingestor, and the
// read-only SQL helpers both processes use.
//
// Write path (see cmd/ingestor/channel_proposals.go): the server never
// touches SQLite for writes. It validates a request, writes one atomic
// command file into the queue directory next to the database, and the
// ingestor applies it and writes a result file the server reads back.
package channelregistry

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxNameBytes is the longest channel name the firmware can hold, in bytes,
// including the leading '#'. MeshCore stores a channel name in
// ChannelDetails.name (char[32]) via StrHelper::strncpy, which keeps room for
// the terminating NUL, so anything past 31 bytes would be silently truncated
// on a radio and derive a different key than the one we derive here.
const MaxNameBytes = 31

// Name validation errors. The messages are shown to users as-is.
var (
	ErrNameEmpty       = errors.New("channel name is empty")
	ErrNameTooLong     = fmt.Errorf("channel name must be at most %d bytes including the #", MaxNameBytes)
	ErrNameInvalidUTF8 = errors.New("channel name is not valid UTF-8")
	ErrNameControl     = errors.New("channel name contains control or direction-override characters")
	ErrNameSpace       = errors.New("channel name must not start or end with a space")
)

// NormalizeName turns user input into the exact channel name whose bytes the
// hashtag key is derived from (SHA-256("#name")[:16]).
//
// It mirrors how the ingestor treats hashChannels entries: surrounding
// whitespace is trimmed and a missing '#' is added. Nothing else is changed —
// case and Unicode form are kept byte-for-byte, because the firmware derives
// the key from the exact bytes, so "#Test" and "#test" are different channels.
//
// The firmware imposes no character set, so neither do we beyond what could
// never be a working or honest name: invalid UTF-8, control characters (a NUL
// would truncate the name on a radio), bidirectional overrides (which make a
// name display as something it is not), and a body that starts or ends with
// whitespace.
func NormalizeName(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if !utf8.ValidString(s) {
		return "", ErrNameInvalidUTF8
	}
	if !strings.HasPrefix(s, "#") {
		s = "#" + s
	}
	body := s[1:]
	if body == "" {
		return "", ErrNameEmpty
	}
	if len(s) > MaxNameBytes {
		return "", ErrNameTooLong
	}
	for _, r := range s {
		if unicode.IsControl(r) || isBidiControl(r) {
			return "", ErrNameControl
		}
	}
	first, _ := utf8.DecodeRuneInString(body)
	last, _ := utf8.DecodeLastRuneInString(body)
	if unicode.IsSpace(first) || unicode.IsSpace(last) {
		return "", ErrNameSpace
	}
	return s, nil
}

// isBidiControl reports the explicit directional formatting characters
// (Unicode Bidi_Control). Other format characters such as the zero-width
// joiner stay allowed: emoji sequences need them.
func isBidiControl(r rune) bool {
	switch {
	case r == 0x061C, r == 0x200E, r == 0x200F:
		return true
	case r >= 0x202A && r <= 0x202E:
		return true
	case r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}
