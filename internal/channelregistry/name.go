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
	ErrNameInvisible   = errors.New("channel name contains invisible formatting characters")
)

// NormalizeName turns user input into the exact channel name whose bytes the
// hashtag key is derived from (SHA-256("#name")[:16]).
//
// It mirrors how the ingestor treats hashChannels entries: surrounding
// whitespace is trimmed and a missing '#' is added. Nothing else is changed —
// case and Unicode form are kept byte-for-byte, because the firmware derives
// the key from the exact bytes, so "#Test" and "#test" are different channels.
//
// The firmware imposes no character set (it only limits the length), so
// neither do we beyond what could never be a working or honest name: invalid
// UTF-8, control characters (a NUL would truncate the name on a radio) and the
// line/paragraph separators U+2028/U+2029, bidirectional overrides and other
// invisible formatting characters (which make a name display as something it
// is not, or two different channels look identical), and a body that starts
// or ends with whitespace. The invisible-character rule is CoreScope's own
// presentation rule, not a firmware one; the frontend and the ingestor apply
// the same rule.
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
		if unicode.IsControl(r) || isBidiControl(r) || isLineBreakSeparator(r) {
			return "", ErrNameControl
		}
		if isInvisibleFormat(r) {
			return "", ErrNameInvisible
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

// isLineBreakSeparator reports U+2028 LINE SEPARATOR and U+2029 PARAGRAPH
// SEPARATOR. They are not Cf (they are Zl/Zp), but a line break inside a
// channel name breaks every single-line place the name is shown.
func isLineBreakSeparator(r rune) bool {
	return r == 0x2028 || r == 0x2029
}

// isInvisibleFormat reports Unicode format characters (category Cf) that
// render as nothing: zero-width space U+200B, BOM U+FEFF, soft hyphen U+00AD,
// the tag characters U+E0000–U+E007F and the like. They would let two
// different channel names (different keys) look identical. The exceptions
// are what emoji need: ZERO WIDTH JOINER U+200D and the variation selectors
// U+FE00–U+FE0F and U+E0100–U+E01EF (those are Mn, not Cf, today; they are
// listed so the rule stays explicit if that ever changes). Emoji tag
// sequences, such as the subdivision flags, are rejected with the tags.
func isInvisibleFormat(r rune) bool {
	switch {
	case r == 0x200D:
		return false
	case r >= 0xFE00 && r <= 0xFE0F, r >= 0xE0100 && r <= 0xE01EF:
		return false
	}
	return unicode.Is(unicode.Cf, r)
}
