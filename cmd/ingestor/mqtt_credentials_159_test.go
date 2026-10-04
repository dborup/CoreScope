package main

import (
	"errors"
	"os"
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
		// an '@' later in the URL does not hide the password or the query
		// values (review of round 1, F1)
		{"tcp://dev-user:hunter2@broker.example:1883?mail=a@b", "bad password hunter2", "bad password ****"},
		{"tcp://h?token=abc123&mail=a@b", "token abc123 expired", "token **** expired"},
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

// The per-source setup main() uses computes the source's secrets once and
// wires the watchdog to the client with them, so a forced reconnect's
// Connect() error is logged masked (review of round 1, F6 and F7).
func TestAttachClientWiresSecretsToForceReconnect_159(t *testing.T) {
	src := MQTTSource{Name: "feed", Broker: "wss://" + credUser + ":" + credPass + "@host/mqtt?token=abc123", Password: "cfg-pass"}
	setup := prepareMQTTSource(src, "feed")
	if got, want := strings.Join(setup.secrets, "|"), strings.Join(mqttSourceSecrets(src), "|"); got != want {
		t.Errorf("secrets %q, want %q", got, want)
	}
	c := &fakeClient{connectErr: errors.New(`dial "` + src.Broker + `": cfg-pass refused; token abc123 rejected for ` + credUser)}
	setup.attachClient(c)
	if setup.liveness.IsConnectedFn == nil || setup.liveness.ForceReconnectFn == nil {
		t.Fatal("attachClient left the watchdog unwired")
	}
	buf := captureLog118(t)
	setup.liveness.ForceReconnectFn()
	out := buf.String()
	const want = `MQTT [feed] WATCHDOG force-reconnect Connect() failed: dial "wss://****@host/mqtt **** refused; token **** rejected for ****`
	if !strings.Contains(out, want) {
		t.Errorf("log %q, want it to contain %q", out, want)
	}
	for _, bad := range []string{credUser, credPass, "abc123", "cfg-pass"} {
		if strings.Contains(out, bad) {
			t.Errorf("log leaks %q: %s", bad, out)
		}
	}
}

// main() is not unit-testable, so this pins its share of the wiring: the
// watchdog is attached through the source's setup (attachClient, which
// carries the secrets) before the first Connect(), and the initial
// connect error is logged with the same secrets. A rewrite or merge of
// the connect loop must keep both (review of round 1, F7).
func TestMainWiresSourceSecrets_159(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	prepare := strings.Index(s, "setup := prepareMQTTSource(source, tag)")
	attach := strings.Index(s, "setup.attachClient(client)")
	connect := strings.Index(s, "token := client.Connect()")
	if prepare < 0 || attach < 0 || connect < 0 || !(prepare < attach && attach < connect) {
		t.Errorf("main() must prepare the source, attachClient, then Connect(): prepare=%d attach=%d connect=%d", prepare, attach, connect)
	}
	if !strings.Contains(s, "errForLog(token.Error(), setup.secrets...)") {
		t.Error("main() must log the initial connect error with the source's secrets")
	}
	if strings.Contains(s, "ForceReconnectFn = ") {
		t.Error("main() wires ForceReconnectFn itself; use attachClient, which passes the secrets")
	}
}
