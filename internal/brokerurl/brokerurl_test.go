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

func TestStrip(t *testing.T) {
	for _, c := range []struct {
		in, want string
		userinfo bool
	}{
		{"tcp://dev-user:secret@host:1883", "tcp://host:1883", true},
		{"dev-user:secret@host:1883", "host:1883", true},
		{"tcp://tok3n@host", "tcp://host", true},
		{"wss://host/mqtt?token=abc", "wss://host/mqtt", false},
		{"wss://host/mqtt#frag", "wss://host/mqtt", false},
		{"wss://host/mqtt?#", "wss://host/mqtt", false},
		{"tcp://dev-user:p%zz@host", "tcp://host", true},
		// unescaped '/', '?' or '#' in the password: url.Parse ends the
		// authority there, so only "everything after the last '@'" is safe
		{"tcp://dev-user:2024/secret@host:1883", "tcp://host:1883", true},
		{"tcp://dev-user:1234?abc@host", "tcp://host", true},
		{"tcp://dev-user:1234#abc@host", "tcp://host", true},
		{"tcp://dev-user:p@ss@secret@host:1883", "tcp://host:1883", true},
		// "://" inside the password is not a scheme
		{"dev-user:sec://ret@host", "host", true},
		{"MQTTS://dev-user:secret@[::1]:8883/x?abc", "MQTTS://[::1]:8883/x", true},
		// nothing to strip
		{"mqtt://broker.example.com:1883", "mqtt://broker.example.com:1883", false},
		{"wss://broker.example.com/mqtt", "wss://broker.example.com/mqtt", false},
		{"host:1883", "host:1883", false},
		{"", "", false},
	} {
		got, userinfo := Strip(c.in)
		assertClean(t, "Strip("+c.in+")", got)
		if got != c.want || userinfo != c.userinfo {
			t.Errorf("Strip(%q) = %q, %v; want %q, %v", c.in, got, userinfo, c.want, c.userinfo)
		}
	}
}

func TestMask(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"mqtt://dev-user:secret@host:1883", "mqtt://****@host:1883"},
		{"dev-user:secret@host:1883", "****@host:1883"},
		{"tcp://tok3n@host", "tcp://****@host"},
		{"tcp://dev-user:1234?abc@host", "tcp://****@host"},
		{"wss://host/mqtt?token=abc", "wss://host/mqtt"},
		{"mqtt://broker.example.com:1883", "mqtt://broker.example.com:1883"},
		{"", ""},
	} {
		got := Mask(c.in)
		assertClean(t, "Mask("+c.in+")", got)
		if got != c.want {
			t.Errorf("Mask(%q) = %q, want %q", c.in, got, c.want)
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

// Idempotent: the server may mask what the ingestor already stripped.
func TestStripAndMaskAreIdempotent(t *testing.T) {
	for _, in := range []string{"tcp://dev-user:secret@host:1883", "tcp://dev-user:1234?abc@host", "wss://host/mqtt?token=abc"} {
		once, _ := Strip(in)
		if twice, _ := Strip(once); twice != once {
			t.Errorf("Strip not idempotent for %q: %q then %q", in, once, twice)
		}
		m := Mask(in)
		if Mask(m) != m || MaskText(m) != m {
			t.Errorf("Mask not idempotent for %q: %q then %q / %q", in, m, Mask(m), MaskText(m))
		}
	}
}
