package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// #118 re-review: credentials in a broker URL must not reach the log, the
// status registry, the stats file (which feeds the public /api/mqtt/status
// and /api/healthz) or the client ID, whatever form the URL takes.

// secretParts118 are the credential parts used in the brokers below.
var secretParts118 = []string{credUser, credPass, "tok3n", "2024", "1234", "abc", "p%zz"}

func assertNoSecretParts118(t *testing.T, what, s string) {
	t.Helper()
	for _, bad := range secretParts118 {
		if strings.Contains(s, bad) {
			t.Errorf("%s leaks %q: %s", what, bad, s)
		}
	}
}

// An unescaped '/', '?' or '#' in the password ends the authority for
// url.Parse, so the password (or its first part) used to be logged.
func TestBrokerForLogAmbiguousPassword_118(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"tcp://" + credUser + ":2024/" + credPass + "@host:1883", "tcp://host:1883"},
		{"tcp://" + credUser + ":1234?abc@host", "tcp://host"},
		{"tcp://" + credUser + ":1234#abc@host", "tcp://host"},
		{credUser + ":" + credPass + "@host:1883", "tcp://host:1883"},
		{"tcp://tok3n@host", "tcp://host"},
		{"wss://host/mqtt?token=abc", "wss://host/mqtt"},
		{"tcp://" + credUser + ":p%zz@host", "tcp://host"},
	} {
		got := brokerForLog(c.in)
		assertNoSecretParts118(t, "brokerForLog("+c.in+")", got)
		if got != c.want {
			t.Errorf("brokerForLog(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The generated client ID is logged ("as client …"), so its host part must
// come from the stripped broker, not from url.Parse's idea of the host.
func TestMQTTClientIDBaseHasNoCredentials_118(t *testing.T) {
	id := mqttClientID(MQTTSource{Broker: "tcp://" + credUser + ":2024/" + credPass + "@host:1883"})
	if !regexp.MustCompile(`^corescope-host-[0-9a-f]{8}$`).MatchString(id) {
		t.Fatalf("client id %q", id)
	}
}

// Two unnamed sources on the same host with different credentials used to
// share one tag: the second lost watchdog tracking and the status counters
// were merged. Tags are unique and credential-free; named sources keep
// their name (a duplicate name stays the operator's config error).
func TestMQTTSourceTagsAreUnique_118(t *testing.T) {
	sources := []MQTTSource{
		{Broker: "tcp://" + credUser + ":" + credPass + "@host:1883"},
		{Broker: "tcp://tok3n@host:1883"},
		{Name: "feed", Broker: "tcp://host:1883"},
		{Broker: "host:1883"},
		// a later source named like a suffixed tag keeps its name
		{Name: "tcp://host:1883 (3)", Broker: "tcp://other:1883"},
		{Broker: "tcp://other:1883"},
		{Name: "feed", Broker: "tcp://x:1883"},
	}
	got := mqttSourceTags(sources)
	want := []string{"tcp://host:1883", "tcp://host:1883 (2)", "feed", "tcp://host:1883 (4)", "tcp://host:1883 (3)", "tcp://other:1883", "feed"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("tags\n got %q\nwant %q", got, want)
	}
	for _, tag := range got {
		assertNoSecretParts118(t, "tag", tag)
	}
	// the two unnamed sources on host:1883 now both get tracked and counted
	saved := livenessRegistry
	livenessRegistry = map[string]*SourceLivenessState{}
	resetSourceStatusRegistry()
	t.Cleanup(func() { livenessRegistry = saved; resetSourceStatusRegistry() })
	for i := 0; i < 2; i++ {
		if !registerLivenessOrSkip(&SourceLivenessState{Tag: got[i]}) {
			t.Fatalf("source %d not tracked by the watchdog", i)
		}
		if RegisterSourceStatus(got[i], sources[i].Broker) == lookupSourceStatus(got[1-i]) {
			t.Fatalf("source %d shares its status counters", i)
		}
	}
}

// The status registry is written to the stats file and served by the
// public /api/mqtt/status, so it holds the stripped broker, whoever calls
// RegisterSourceStatus, and a disconnect error that quotes a broker URL
// is stripped too.
func TestSourceStatusHoldsNoCredentials_118(t *testing.T) {
	resetSourceStatusRegistry()
	t.Cleanup(resetSourceStatusRegistry)
	s := RegisterSourceStatus("t", "tcp://"+credUser+":1234?abc@host")
	s.MarkDisconnect(time.Now(), errors.New(`dial "wss://`+credUser+`:`+credPass+`@host/mqtt?token=abc": refused`))
	snap := s.snapshot(time.Now())
	b, _ := json.Marshal(snap)
	assertNoSecretParts118(t, "status snapshot", string(b))
	if snap.Broker != "tcp://host" {
		t.Errorf("Broker = %q, want tcp://host", snap.Broker)
	}
}

// ── runtime wiring: main()'s per-source setup, against a loopback broker ──

// prepareMQTTSource is what main() runs for every source. Driven here
// through connect, a broker-side drop, paho's reconnect and the stats
// file: every log line, the status registry and the file must be free
// of the credentials in the broker URL, whichever path they take.
func TestMQTTSourceWiringLeaksNoCredentials_118(t *testing.T) {
	b := newIDBroker(t)
	addr := b.ln.Addr().String()
	for _, broker := range []string{
		"tcp://" + credUser + ":" + credPass + "@" + addr,
		credUser + ":" + credPass + "@" + addr,
	} {
		t.Run("", func(t *testing.T) {
			resetSourceStatusRegistry()
			saved := livenessRegistry
			livenessRegistry = map[string]*SourceLivenessState{}
			t.Cleanup(func() { livenessRegistry = saved; resetSourceStatusRegistry() })

			buf := captureLog118(t)
			src := MQTTSource{Broker: broker, Topics: []string{"meshcore/#"}}
			tag := mqttSourceTags([]MQTTSource{src})[0]
			opts, _, liveness := prepareMQTTSource(src, tag)
			opts.SetConnectTimeout(time.Second).
				SetMaxReconnectInterval(100 * time.Millisecond).
				SetConnectRetryInterval(50 * time.Millisecond)
			client := mqtt.NewClient(opts)
			liveness.IsConnectedFn = client.IsConnected
			if !registerLivenessOrSkip(liveness) {
				t.Fatal("liveness not registered")
			}
			seen := len(b.seen())
			client.Connect()
			defer client.Disconnect(0)
			waitForID(t, "connect", func() bool { return len(b.seen()) > seen && strings.Contains(buf.String(), "subscribed to") })

			b.dropAll() // ConnectionLost, then paho's Reconnecting and OnConnect
			waitForID(t, "reconnect", func() bool {
				return len(b.seen()) > seen+1 && strings.Count(buf.String(), "subscribed to") >= 2
			})

			out := buf.String()
			for _, want := range []string{"connected to", "disconnected from", "reconnecting to", "subscribed to"} {
				if !strings.Contains(out, want) {
					t.Fatalf("no %q line in:\n%s", want, out)
				}
			}
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				assertNoSecretParts118(t, "log line", line)
			}

			// what the stats file carries: statuses and liveness keys
			stats := struct {
				S []SourceStatusSnapshot            `json:"source_statuses"`
				L map[string]SourceLivenessSnapshot `json:"source_liveness"`
			}{SnapshotSourceStatuses(time.Now()), SnapshotLivenessClocks()}
			j, _ := json.Marshal(stats)
			assertNoSecretParts118(t, "stats data", string(j))
			if len(stats.S) != 1 || stats.S[0].DisconnectCount < 1 || stats.S[0].ConnectCount < 2 {
				t.Fatalf("status not wired: %s", j)
			}
		})
	}
}

// The stats file itself: stripped content, owner-only permissions even
// when a stale tmp file with wider permissions is lying around.
func TestStatsFileHasNoCredentials_118(t *testing.T) {
	dir := t.TempDir()
	statsPath := filepath.Join(dir, "ingestor-stats.json")
	t.Setenv("CORESCOPE_INGESTOR_STATS", statsPath)
	if err := os.WriteFile(statsPath+".tmp", []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(statsPath+".tmp", 0o644); err != nil {
		t.Fatal(err)
	}

	resetSourceStatusRegistry()
	saved := livenessRegistry
	livenessRegistry = map[string]*SourceLivenessState{}
	t.Cleanup(func() { livenessRegistry = saved; resetSourceStatusRegistry() })
	sources := []MQTTSource{
		{Broker: "tcp://" + credUser + ":" + credPass + "@host:1883"},
		{Broker: "tcp://tok3n@host:1883"},
	}
	for i, tag := range mqttSourceTags(sources) {
		_, status, liveness := prepareMQTTSource(sources[i], tag)
		status.MarkDisconnect(time.Now(), errors.New("connect "+sources[i].Broker+" refused"))
		registerLivenessOrSkip(liveness)
	}

	store, err := OpenStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	t.Cleanup(StartStatsFileWriter(store, 20*time.Millisecond))

	var raw []byte
	waitForID(t, "stats file", func() bool {
		raw, err = os.ReadFile(statsPath)
		return err == nil && strings.Contains(string(raw), "source_statuses")
	})
	assertNoSecretParts118(t, "stats file", string(raw))
	for _, want := range []string{`"tcp://host:1883"`, `"tcp://host:1883 (2)"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("stats file misses %s: %s", want, raw)
		}
	}
	fi, err := os.Stat(statsPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("stats file mode %o, want 600", perm)
	}
}
