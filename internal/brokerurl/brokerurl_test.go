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
		{"tcp://dev-user:secret@host:1883", []string{"dev-user:secret"}},
		{"wss://host/mqtt?token=abc#frag", []string{"token=abc", "frag"}},
		{"dev-user:1234?abc@host?x=1", []string{"dev-user:1234?abc", "x=1"}},
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
