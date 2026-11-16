package main

import (
	"io"
	"net/url"
	"regexp"
	"strings"
)

// redacted replaces a secret in the output.
const redacted = "REDACTED"

// secretParamRe is a query parameter that carries a key in any upstream
// URL style: Dataforsyningen's token, Stadia's api_key, Mapbox's
// access_token. Its value is redacted whether or not it is a known token.
var secretParamRe = regexp.MustCompile(`(?i)([?&;](?:token|api_?key|access_token|key)=)[^&#\s"']*`)

// redactor removes the configured tokens, raw and URL-escaped, and the
// value of every key-carrying query parameter from text.
type redactor struct{ forms []string }

func newRedactor(secrets ...string) *redactor {
	r := &redactor{}
	for _, s := range secrets {
		if s == "" {
			continue
		}
		r.forms = append(r.forms, s, url.QueryEscape(s), url.PathEscape(s))
	}
	return r
}

func (r *redactor) redact(s string) string {
	for _, f := range r.forms {
		s = strings.ReplaceAll(s, f, redacted)
	}
	return secretParamRe.ReplaceAllString(s, "${1}"+redacted)
}

// redactingWriter is the only way output is written: every line passes
// the redactor.
type redactingWriter struct {
	w io.Writer
	r *redactor
}

func (o *redactingWriter) line(s string) {
	io.WriteString(o.w, o.r.redact(s)+"\n")
}
