package main

import (
	"errors"
	"strings"
	"testing"
)

// #159: errForLog masks the parts of the broker URL's credentials, raw and
// decoded, wherever they appear in an error, and overlapping secrets
// leave no residue.
func TestErrForLogMasksSecretParts_159(t *testing.T) {
	for _, c := range []struct{ broker, in, want string }{
		{"tcp://dev-user:hunter2@host", "bad password hunter2 for dev-user", "bad password **** for ****"},
		{"tcp://dev-user:p%40ss@host", "auth p@ss rejected", "auth **** rejected"},
		{"wss://host/mqtt?token=abc123", "token abc123 expired", "token **** expired"},
		// short values are left alone in free text; the URL itself is
		// still masked by MaskText
		{"tcp://u:pw@host", "uptime 1h, pw ok: tcp://u:pw@host refused", "uptime 1h, pw ok: tcp://****@host refused"},
	} {
		got := errForLog(errors.New(c.in), mqttSourceSecrets(MQTTSource{Broker: c.broker})...)
		if got != c.want {
			t.Errorf("errForLog(%q) for %q = %q, want %q", c.in, c.broker, got, c.want)
		}
	}
	// a configured password overlapping the URL's user name
	src := MQTTSource{Broker: "tcp://abcd@host", Password: "cdef"}
	if got := errForLog(errors.New("auth abcdef"), mqttSourceSecrets(src)...); got != "auth ****" {
		t.Errorf("overlap: %q, want %q", got, "auth ****")
	}
}

// #159 (comment): the watchdog's forced reconnect logs Connect()'s error
// with the source's secrets masked, like every other connect path.
func TestBuildForceReconnectFnMasksConnectError_159(t *testing.T) {
	src := MQTTSource{Broker: "wss://" + credUser + ":" + credPass + "@host/mqtt?token=abc123"}
	c := &fakeClient{connectErr: errors.New(`dial "` + src.Broker + `": refused; token abc123 rejected for ` + credUser)}
	buf := captureLog118(t)
	buildForceReconnectFn(c, "feed", mqttSourceSecrets(src)...)()
	out := buf.String()
	const want = `MQTT [feed] WATCHDOG force-reconnect Connect() failed: dial "wss://****@host/mqtt refused; token **** rejected for ****`
	if !strings.Contains(out, want) {
		t.Errorf("log %q, want it to contain %q", out, want)
	}
	for _, bad := range []string{credUser, credPass, "abc123", "token="} {
		if strings.Contains(out, bad) {
			t.Errorf("log leaks %q: %s", bad, out)
		}
	}
}
