// Package brokerurl removes credentials from MQTT broker URLs. The ingestor
// uses it for everything it logs or publishes in its stats file, and the
// server, which serves that file on public endpoints, applies it again to
// whatever an ingestor of any version wrote there.
//
// It deliberately does not use url.Parse. A password with an unescaped '/',
// '?' or '#' ends the authority for url.Parse, which then reports the user
// name as the host and the password as port, path or query; one with a bad
// %-escape makes it fail. Here everything up to the last '@' is user-info,
// and the query and fragment (which may carry a token) are dropped. A
// broker with an '@' in its path or query therefore shows only what
// follows that '@': showing too little is the safe side.
package brokerurl

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Marker replaces removed user-info in Mask and MaskText, so a reader can
// tell credentials were present, and that a host shown after it may be
// only what followed an '@' in the path or query. MaskSecrets replaces
// each masked span with it too.
const Marker = "****"

// Mask returns s with its user-info replaced by Marker and without query
// and fragment. A scheme ("tcp://") is kept when it is a valid URL scheme.
func Mask(s string) string {
	p := split(s, false)
	rest := p.rest
	if p.hasUserinfo {
		rest = Marker + "@" + rest
	}
	return join(p.scheme, rest)
}

// Secrets returns what Mask removes from s, and its parts, so that a
// caller can mask them where they appear without a URL around them (say,
// a password or a query token quoted in an error; see MaskSecrets): the
// user-info and, when it has a ':', the user name before it and the
// password after it; the query and each of its values; the fragment and
// each of its values. A value is what follows the first '=' of an
// '&'-separated part, or the whole part when it has no '='.
//
// s is read twice: as Mask reads it, with the user-info ending at the
// last '@', and as RFC 3986 does, with the user-info ending at the last
// '@' before the first '/', '?' or '#'. Either can be the one a library
// quoting a part used, and each hides parts from the other: an '@' in the
// query moves Mask's user-info past the password, and an unescaped '/' in
// the password moves RFC 3986's past it.
//
// Each part comes raw and, where it differs and is valid UTF-8, %-decoded
// as in a path and as in a query ('+' as space). Only one layer is
// decoded and nothing is re-encoded, so a value decoded twice, or quoted
// %-encoded when it was configured raw, is not listed; inside a URL,
// MaskText still masks it. Empty values and duplicates are left out.
func Secrets(s string) []string {
	raw := append(split(s, false).secretParts(), split(s, true).secretParts()...)
	var out []string
	seen := map[string]bool{"": true}
	for _, v := range raw {
		for _, d := range decoded(v) {
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	return out
}

// secretParts returns p's user-info, user name and password, query,
// fragment and their values, raw.
func (p parts) secretParts() []string {
	var out []string
	if p.userinfo != "" {
		out = append(out, p.userinfo)
		if user, pass, ok := strings.Cut(p.userinfo, ":"); ok {
			out = append(out, user, pass)
		}
	}
	for _, v := range []string{p.query, p.fragment} {
		if v != "" {
			out = append(out, v)
			out = append(out, values(v)...)
		}
	}
	return out
}

// values returns the value of each '&'-separated part of a query or
// fragment.
func values(q string) []string {
	var out []string
	for _, part := range strings.Split(q, "&") {
		if _, v, ok := strings.Cut(part, "="); ok {
			part = v
		}
		out = append(out, part)
	}
	return out
}

// decoded returns v and its %-decoded forms. A bad escape has none, and a
// form that is not valid UTF-8 is dropped: "%A6abc" decodes to a stray
// continuation byte that matches inside a rune of other text.
func decoded(v string) []string {
	out := []string{v}
	for _, unescape := range []func(string) (string, error){url.PathUnescape, url.QueryUnescape} {
		if d, err := unescape(v); err == nil && utf8.ValidString(d) {
			out = append(out, d)
		}
	}
	return out
}

// MinSecretLen is the length in runes below which MaskSecrets leaves a
// secret alone. A one- or two-character user name or password would mask
// that substring all over unrelated text, and the free text is all
// MaskSecrets sees: a URL that holds such a value is still masked whole
// by Mask and MaskText, and a longer secret containing it (such as the
// user-info "u:pw") still is by MaskSecrets.
const MinSecretLen = 3

// MaskSecrets replaces every occurrence in s of each of secrets (Secrets,
// or values a caller knows, such as a configured password) by Marker. It
// collects the match intervals of all secrets, including overlapping
// matches of one secret, merges those that overlap or touch, and replaces
// each merged interval once, so overlapping secrets leave no residue
// ("abcd" and "cdef" turn "abcdef" into one Marker) and a Marker is never
// matched again. Secrets shorter than MinSecretLen are skipped.
func MaskSecrets(s string, secrets ...string) string {
	type span struct{ start, end int }
	var spans []span
	for _, v := range secrets {
		if utf8.RuneCountInString(v) < MinSecretLen {
			continue
		}
		for i := 0; ; {
			j := strings.Index(s[i:], v)
			if j < 0 {
				break
			}
			spans = append(spans, span{i + j, i + j + len(v)})
			i += j + 1
		}
	}
	if len(spans) == 0 {
		return s
	}
	sort.Slice(spans, func(a, b int) bool { return spans[a].start < spans[b].start })
	var b strings.Builder
	last := 0
	for k := 0; k < len(spans); {
		start, end := spans[k].start, spans[k].end
		for k++; k < len(spans) && spans[k].start <= end; k++ {
			end = max(end, spans[k].end)
		}
		b.WriteString(s[last:start])
		b.WriteString(Marker)
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

// urlUserinfoRe matches a URL with user-info in free text: from the start of
// the token holding the scheme to the last '@' on the line and the rest of
// that token, so a password with whitespace is covered whole (and any text
// between the scheme and a later '@' is masked with it).
var urlUserinfoRe = regexp.MustCompile(`\S*[A-Za-z][A-Za-z0-9+.\-]*://[^\n]*@\S*`)

// maskTokenRe matches the whitespace-separated tokens that may hold a
// broker URL or user-info.
var maskTokenRe = regexp.MustCompile(`\S*(?:://|@)\S*`)

// schemeRe finds a URL scheme inside a token, e.g. after a quote.
var schemeRe = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.\-]*://`)

// MaskText masks broker URLs and user-info in free text (an error message,
// a source name) and leaves the rest alone: first every URL with user-info
// (urlUserinfoRe), then every remaining whitespace-separated token that
// contains "://" or "@". Without a scheme, user-info with whitespace in it
// cannot be told from text, and only its last part is masked; a string
// that is a broker URL goes through Mask instead.
func MaskText(s string) string {
	s = urlUserinfoRe.ReplaceAllStringFunc(s, maskSpan)
	return maskTokenRe.ReplaceAllStringFunc(s, maskSpan)
}

// maskSpan masks one match of MaskText. Punctuation before a scheme, such
// as the quote in `"wss://…"`, is kept; anything else before it may be
// user-info.
func maskSpan(m string) string {
	if loc := schemeRe.FindStringIndex(m); loc != nil && isPunct(m[:loc[0]]) {
		return m[:loc[0]] + Mask(m[loc[0]:])
	}
	return Mask(m)
}

// parts is a broker URL as split reads it.
type parts struct {
	scheme      string
	userinfo    string // everything before the last '@'
	hasUserinfo bool   // an '@' was present, even with empty user-info
	rest        string // host, port and path
	query       string // without '?'
	fragment    string // without '#'
}

// split reads s as Mask does, with the user-info ending at the last '@',
// or, with authority set, as RFC 3986 does, with the user-info ending at
// the last '@' before the first '/', '?' or '#' (see Secrets).
func split(s string, authority bool) parts {
	var p parts
	rest := s
	if i := strings.Index(rest, "://"); i > 0 && validScheme(rest[:i]) {
		p.scheme, rest = rest[:i], rest[i+len("://"):]
	}
	end := len(rest)
	if i := strings.IndexAny(rest, "/?#"); authority && i >= 0 {
		end = i
	}
	if i := strings.LastIndex(rest[:end], "@"); i >= 0 {
		p.userinfo, p.hasUserinfo, rest = rest[:i], true, rest[i+1:]
	}
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest, p.fragment = rest[:i], rest[i+1:]
	}
	if i := strings.IndexByte(rest, '?'); i >= 0 {
		rest, p.query = rest[:i], rest[i+1:]
	}
	p.rest = rest
	return p
}

func join(scheme, rest string) string {
	if scheme == "" {
		return rest
	}
	return scheme + "://" + rest
}

// validScheme reports whether s is an RFC 3986 scheme.
func validScheme(s string) bool {
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && (r >= '0' && r <= '9' || r == '+' || r == '-' || r == '.'):
		default:
			return false
		}
	}
	return s != ""
}

func isPunct(s string) bool {
	for _, r := range s {
		if r == '@' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r > 0x7f {
			return false
		}
	}
	return true
}
