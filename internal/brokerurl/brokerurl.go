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
	"regexp"
	"strings"
)

// Marker replaces removed user-info in Mask and MaskText, so a reader can
// tell credentials were present, and that a host shown after it may be
// only what followed an '@' in the path or query.
const Marker = "****"

// Mask returns s with its user-info replaced by Marker and without query
// and fragment. A scheme ("tcp://") is kept when it is a valid URL scheme.
func Mask(s string) string {
	p := split(s)
	rest := p.rest
	if p.hasUserinfo {
		rest = Marker + "@" + rest
	}
	return join(p.scheme, rest)
}

// Secrets returns what Mask removes from s, so that a caller can mask the
// same values where they appear without a URL around them (say, a query
// token quoted in an error): the user-info, the query and the fragment,
// each only when non-empty.
func Secrets(s string) []string {
	p := split(s)
	var out []string
	for _, v := range []string{p.userinfo, p.query, p.fragment} {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
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

// parts is a broker URL as Mask reads it.
type parts struct {
	scheme      string
	userinfo    string // everything before the last '@'
	hasUserinfo bool   // an '@' was present, even with empty user-info
	rest        string // host, port and path
	query       string // without '?'
	fragment    string // without '#'
}

func split(s string) parts {
	var p parts
	rest := s
	if i := strings.Index(rest, "://"); i > 0 && validScheme(rest[:i]) {
		p.scheme, rest = rest[:i], rest[i+len("://"):]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
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
