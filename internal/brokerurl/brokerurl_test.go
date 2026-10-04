package brokerurl

import (
	"strings"
	"testing"
)

// secrets are the credential parts used in the inputs below.
var secrets = []string{"secret", "tok3n", "abc", "dev-user", "p%zz", "2024", "1234", "frag", "sec", "ret"}

func assertClean(t *testing.T, what, s string) {
	t.Helper()
	for _, bad := range secrets {
		if strings.Contains(s, bad) {
			t.Errorf("%s leaks %q: %s", what, bad, s)
		}
	}
}

func TestMask(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"tcp://dev-user:secret@host:1883", "tcp://****@host:1883"},
		{"mqtt://dev-user:secret@host:1883", "mqtt://****@host:1883"},
		{"mqtt://u:p@broker:1883", "mqtt://****@broker:1883"},
		{"dev-user:secret@host:1883", "****@host:1883"},
		{"tcp://tok3n@host", "tcp://****@host"},
		{"wss://host/mqtt?token=abc", "wss://host/mqtt"},
		{"wss://host/mqtt#frag", "wss://host/mqtt"},
		{"wss://host/mqtt?#", "wss://host/mqtt"},
		{"tcp://dev-user:p%zz@host", "tcp://****@host"},
		// unescaped '/', '?' or '#' in the password: url.Parse ends the
		// authority there, so only "everything after the last '@'" is safe
		{"tcp://dev-user:2024/secret@host:1883", "tcp://****@host:1883"},
		{"tcp://dev-user:1234?abc@host", "tcp://****@host"},
		{"tcp://dev-user:1234#abc@host", "tcp://****@host"},
		{"tcp://dev-user:p@ss@secret@host:1883", "tcp://****@host:1883"},
		// an '@' in the query: what follows it is shown, marked as cut
		{"wss://host/mqtt?u=me@x.org", "wss://****@x.org"},
		// "://" inside the password is not a scheme
		{"dev-user:sec://ret@host", "****@host"},
		{"MQTTS://dev-user:secret@[::1]:8883/x?abc", "MQTTS://****@[::1]:8883/x"},
		// nothing to mask
		{"mqtt://broker.example.com:1883", "mqtt://broker.example.com:1883"},
		{"wss://broker.example.com/mqtt", "wss://broker.example.com/mqtt"},
		{"host:1883", "host:1883"},
		{"", ""},
	} {
		got := Mask(c.in)
		assertClean(t, "Mask("+c.in+")", got)
		if got != c.want {
			t.Errorf("Mask(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Secrets lists exactly what Mask removes, for literal masking elsewhere.
func TestSecrets(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"tcp://dev-user:secret@host:1883", []string{"dev-user:secret", "dev-user", "secret"}},
		{"wss://host/mqtt?token=abc#frag", []string{"token=abc", "abc", "frag"}},
		{"dev-user:1234?abc@host?x=1", []string{"dev-user:1234?abc", "dev-user", "1234?abc", "x=1", "1"}},
		// #159: each part raw and URL-decoded, without duplicates
		{"tcp://dev-user:p%40ss@host", []string{"dev-user:p%40ss", "dev-user:p@ss", "dev-user", "p%40ss", "p@ss"}},
		{"wss://host/?a=x+y%21&b&c=", []string{"a=x+y%21&b&c=", "a=x+y!&b&c=", "a=x y!&b&c=", "x+y%21", "x+y!", "x y!", "b"}},
		{"wss://host/#access_token=xyz&state=1", []string{"access_token=xyz&state=1", "xyz", "1"}},
		{"tcp://:pw@host", []string{":pw", "pw"}},
		{"tcp://u%zz@host", []string{"u%zz"}}, // a bad escape has no decoded form
		{"tcp://@host", nil},
		{"wss://host/mqtt?#", nil},
		{"mqtt://host:1883", nil},
		{"", nil},
	} {
		got := Secrets(c.in)
		if strings.Join(got, "|") != strings.Join(c.want, "|") || len(got) != len(c.want) {
			t.Errorf("Secrets(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMaskText(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`dial "wss://host/mqtt?token=abc": bad handshake`, `dial "wss://host/mqtt bad handshake`},
		{`dial "tcp://dev-user:secret@host": refused`, `dial "tcp://****@host": refused`},
		{"network error tcp://dev-user:secret@host:1883: refused", "network error tcp://****@host:1883: refused"},
		{"auth dev-user:secret@host failed", "auth ****@host failed"},
		{"dev-user:sec://ret@host", "****@host"},
		// whitespace in the password: from the scheme to the last '@'
		{"connect tcp://dev-user:sec ret@host:1883 failed", "connect tcp://****@host:1883 failed"},
		{`dial "wss://dev-user:a b c@host/mqtt?token=abc": refused`, `dial "wss://****@host/mqtt refused`},
		{"x tcp://dev-user:sec://ret@host y", "x tcp://****@host y"},
		{"tcp://h:1883 (2)", "tcp://h:1883 (2)"},
		{"EOF", "EOF"},
		{"read tcp 10.0.0.1:5000->10.0.0.2:1883: connection reset by peer", "read tcp 10.0.0.1:5000->10.0.0.2:1883: connection reset by peer"},
		{"Local Feed #1", "Local Feed #1"},
		{"", ""},
	} {
		got := MaskText(c.in)
		assertClean(t, "MaskText("+c.in+")", got)
		if got != c.want {
			t.Errorf("MaskText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Idempotent: the server masks what the ingestor already masked.
func TestMaskIsIdempotent(t *testing.T) {
	for _, in := range []string{"tcp://dev-user:secret@host:1883", "tcp://dev-user:1234?abc@host", "wss://host/mqtt?token=abc", "wss://host/mqtt?u=me@x.org"} {
		m := Mask(in)
		if Mask(m) != m || MaskText(m) != m {
			t.Errorf("Mask not idempotent for %q: %q then %q / %q", in, m, Mask(m), MaskText(m))
		}
	}
}

// #159: MaskSecrets masks every part of a broker URL's credentials that
// appears in free text, raw or decoded, in one pass over merged match
// intervals, so overlapping secrets leave no residue, and it leaves
// values shorter than MinSecretLen alone.
func TestMaskSecrets_159(t *testing.T) {
	for _, c := range []struct {
		name, in string
		secrets  []string
		want     string
	}{
		{"user and password alone", "bad password hunter2 for dev-user",
			Secrets("tcp://dev-user:hunter2@host"), "bad password **** for ****"},
		{"decoded password", "auth p@ss rejected",
			Secrets("tcp://dev-user:p%40ss@host"), "auth **** rejected"},
		{"raw password", "auth p%40ss rejected",
			Secrets("tcp://dev-user:p%40ss@host"), "auth **** rejected"},
		{"query value alone", "token abc123 expired",
			Secrets("wss://host/mqtt?token=abc123"), "token **** expired"},
		{"query value decoded", "token a b/c! expired",
			Secrets("wss://host/mqtt?token=a+b%2Fc%21"), "token **** expired"},
		{"fragment value", "bad token xyz789",
			Secrets("wss://host/#access_token=xyz789"), "bad token ****"},
		{"overlapping secrets", "abcdef", []string{"abcd", "cdef"}, "****"},
		{"overlap inside text", "x abcdef y", []string{"cdef", "abcd"}, "x **** y"},
		{"one secret contains another", "user:pass", []string{"user", "user:pass", "pass"}, "****"},
		{"a secret overlapping itself", "aaaa", []string{"aaa"}, "****"},
		{"adjacent matches", "abcdef", []string{"abc", "def"}, "****"},
		{"separate matches", "abc-def", []string{"abc", "def"}, "****-****"},
		{"short values untouched", "uptime 1h, pw ok, queue u2",
			Secrets("tcp://u:pw@host?x=1"), "uptime 1h, pw ok, queue u2"},
		{"short values in a longer secret", "auth u:pw failed",
			Secrets("tcp://u:pw@host"), "auth **** failed"},
		{"length counts runes", "pæs æø", []string{"pæs", "æø"}, "**** æø"},
		{"no secrets", "EOF", nil, "EOF"},
		{"empty secret", "EOF", []string{""}, "EOF"},
		{"marker is not re-scanned", "x abc y", []string{"abc", "***"}, "x **** y"},
	} {
		got := MaskSecrets(c.in, c.secrets...)
		if got != c.want {
			t.Errorf("%s: MaskSecrets(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// What a caller logs, MaskText(MaskSecrets(...)), keeps nothing of a
// decoded password with an '@' in it: MaskText alone would read "p@ss" as
// user-info and show "ss".
func TestMaskSecretsThenMaskTextLeavesNoResidue_159(t *testing.T) {
	got := MaskText(MaskSecrets("auth p@ss rejected", Secrets("tcp://dev-user:p%40ss@host")...))
	if got != "auth **** rejected" {
		t.Errorf("got %q, want %q", got, "auth **** rejected")
	}
}

// No change to Mask: what MaskSecrets adds is for free text only, and a
// full URL masked by both is still masked as before.
func TestMaskSecretsKeepsMaskOutput_159(t *testing.T) {
	for _, in := range []string{"tcp://dev-user:secret@host:1883", "wss://host/mqtt?token=abc", "tcp://dev-user:p%40ss@host"} {
		if got := MaskText(MaskSecrets(Mask(in), Secrets(in)...)); got != Mask(in) {
			t.Errorf("%q: %q, want %q", in, got, Mask(in))
		}
	}
}
