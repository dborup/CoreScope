package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The diagnostics dial loopback sources (#870). Tests that are not about the
// dial must not depend on what happens to listen on the machine running them,
// so the probe defaults to "nothing answers"; the #870 tests below use the
// real probe or their own stub.
func init() { brokerListenerProbe = func(string) bool { return false } }

func brokerEnv(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// diagText joins diagnostics into one blob for substring assertions.
func diagText(diags []BrokerDiagnostic) string {
	var b strings.Builder
	for _, d := range diags {
		b.WriteString(string(d.Severity))
		b.WriteString(": ")
		b.WriteString(d.Message)
		b.WriteString("\n")
	}
	return b.String()
}

// disableMosquittoCase is one row of test-fixtures/disable-mosquitto-cases.json,
// the table docker/entrypoint-go.sh and manage.sh are held to as well
// (test-issue-441-disable-mosquitto.js). A nil Value means unset.
type disableMosquittoCase struct {
	Value      *string `json:"value"`
	Disabled   bool    `json:"disabled"`
	Recognised bool    `json:"recognised"`
}

func loadDisableMosquittoCases(t *testing.T) []disableMosquittoCase {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "test-fixtures", "disable-mosquitto-cases.json"))
	if err != nil {
		t.Fatalf("read the shared DISABLE_MOSQUITTO table: %v", err)
	}
	var table struct {
		Cases []disableMosquittoCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &table); err != nil {
		t.Fatalf("parse the shared DISABLE_MOSQUITTO table: %v", err)
	}
	if len(table.Cases) == 0 {
		t.Fatal("the shared DISABLE_MOSQUITTO table is empty")
	}
	return table.Cases
}

// envFor returns the environment one table row describes. os.Getenv reads
// an unset variable as "", which is what a missing key gives here.
func (c disableMosquittoCase) envFor() map[string]string {
	if c.Value == nil {
		return map[string]string{}
	}
	return map[string]string{"DISABLE_MOSQUITTO": *c.Value}
}

func (c disableMosquittoCase) String() string {
	if c.Value == nil {
		return "<unset>"
	}
	return fmt.Sprintf("%q", *c.Value)
}

// Issue #441: the bundled broker is opt-in, and the ingestor reads
// DISABLE_MOSQUITTO exactly as the entrypoint and manage.sh do — trimmed,
// case-insensitive, true/1/yes/on and false/0/no/off, anything else the
// default (true).
func TestResolveBrokerMode_FollowsTheSharedTable(t *testing.T) {
	for _, c := range loadDisableMosquittoCases(t) {
		got := ResolveBrokerMode(brokerEnv(c.envFor()))
		if got.Bundled == c.Disabled {
			t.Errorf("DISABLE_MOSQUITTO=%s: Bundled = %v, want %v", c, got.Bundled, !c.Disabled)
		}
		if got.Unrecognised == c.Recognised {
			t.Errorf("DISABLE_MOSQUITTO=%s: Unrecognised = %v, want %v", c, got.Unrecognised, !c.Recognised)
		}
	}
}

func TestBrokerMode_JunkValueIsReportedAndFallsBackToTheDefault(t *testing.T) {
	good := []MQTTSource{{Name: "ext", Broker: "mqtt://broker.example:1883"}}
	for _, c := range loadDisableMosquittoCases(t) {
		if c.Recognised {
			continue
		}
		mode := ResolveBrokerMode(brokerEnv(c.envFor()))
		diags := CheckBrokerSources(mode, good, false)
		joined := diagText(diags)
		if WorstBrokerSeverity(diags) != SeverityWarn {
			t.Errorf("DISABLE_MOSQUITTO=%s is junk: want exactly a warning (it must not block a start); got:\n%s", c, joined)
		}
		for _, want := range []string{"DISABLE_MOSQUITTO", *c.Value, "not a recognised", "default"} {
			if !strings.Contains(joined, want) {
				t.Errorf("DISABLE_MOSQUITTO=%s: the warning must mention %q; got:\n%s", c, want, joined)
			}
		}
	}
}

// Before #441 a value such as "yes" was read one way by manage.sh and
// another by the entrypoint, and the ingestor said so. Parsing is now the
// same everywhere, so no recognised value may produce a diagnostic about
// it: with a usable source in the mode the value selects, the check is
// silent.
func TestBrokerMode_RecognisedValuesReportNoMismatch(t *testing.T) {
	external := []MQTTSource{{Name: "ext", Broker: "mqtt://broker.example:1883"}}
	bundled := []MQTTSource{{Name: "local", Broker: "mqtt://localhost:1883"}}
	for _, c := range loadDisableMosquittoCases(t) {
		if !c.Recognised {
			continue
		}
		mode := ResolveBrokerMode(brokerEnv(c.envFor()))
		sources := external
		if !c.Disabled {
			sources = bundled
		}
		if diags := CheckBrokerSources(mode, sources, false); len(diags) != 0 {
			t.Errorf("DISABLE_MOSQUITTO=%s with a usable source must be quiet; got:\n%s", c, diagText(diags))
		}
	}
}

func TestExternalMode_MissingSourceIsAnError(t *testing.T) {
	// LoadConfig falls back to mqtt://localhost:1883 when nothing is
	// configured. With the bundled broker off, nothing listens there, so
	// ingest stops dead — the exact silent failure #372 is about.
	mode := ResolveBrokerMode(brokerEnv(map[string]string{"DISABLE_MOSQUITTO": "true"}))
	diags := CheckBrokerSources(mode, []MQTTSource{{Name: "local", Broker: "mqtt://localhost:1883"}}, true)
	if WorstBrokerSeverity(diags) != SeverityError {
		t.Fatalf("a defaulted source in external-broker mode must be an error; got:\n%s", diagText(diags))
	}
	joined := diagText(diags)
	for _, want := range []string{"DISABLE_MOSQUITTO", "mqttSources"} {
		if !strings.Contains(joined, want) {
			t.Errorf("diagnostic must name %q so the operator knows what to fix; got:\n%s", want, joined)
		}
	}
}

func TestExternalMode_LoopbackOnlySourcesAreAnError(t *testing.T) {
	mode := ResolveBrokerMode(brokerEnv(map[string]string{"DISABLE_MOSQUITTO": "true"}))
	for _, broker := range []string{
		"mqtt://localhost:1883",
		"mqtt://127.0.0.1:1883",
		"tcp://127.0.0.5:1883",
		"mqtts://[::1]:8883",
	} {
		diags := CheckBrokerSources(mode, []MQTTSource{{Name: "only", Broker: broker}}, false)
		if WorstBrokerSeverity(diags) != SeverityError {
			t.Errorf("%s: a loopback-only external configuration must be an error; got:\n%s", broker, diagText(diags))
		}
		if !strings.Contains(diagText(diags), "loopback") {
			t.Errorf("%s: diagnostic must say loopback; got:\n%s", broker, diagText(diags))
		}
	}
}

func TestExternalMode_OneLoopbackAmongRealSourcesIsOnlyAWarning(t *testing.T) {
	mode := ResolveBrokerMode(brokerEnv(map[string]string{"DISABLE_MOSQUITTO": "true"}))
	diags := CheckBrokerSources(mode, []MQTTSource{
		{Name: "loop", Broker: "mqtt://localhost:1883"},
		{Name: "ext", Broker: "mqtts://broker.example:8883"},
	}, false)
	if len(diags) == 0 {
		t.Fatalf("a loopback source in external mode must still be reported")
	}
	if sev := WorstBrokerSeverity(diags); sev != SeverityWarn {
		t.Errorf("ingest still works through the other source, so this is a warning; got %q:\n%s", sev, diagText(diags))
	}
}

func TestBundledMode_LoopbackSourcesAreFine(t *testing.T) {
	mode := ResolveBrokerMode(brokerEnv(map[string]string{"DISABLE_MOSQUITTO": "false"}))
	diags := CheckBrokerSources(mode, []MQTTSource{{Name: "local", Broker: "mqtt://localhost:1883"}}, true)
	if len(diags) != 0 {
		t.Fatalf("the bundled broker listens on loopback, so this is the normal shape; got:\n%s", diagText(diags))
	}
}

func TestMalformedBrokerIsAnErrorInEitherMode(t *testing.T) {
	for _, broker := range []string{"", "   ", "broker.example:1883", "mqtt://", "http://broker.example:1883", "mqtt://:1883"} {
		for _, disable := range []string{"", "true"} {
			mode := ResolveBrokerMode(brokerEnv(map[string]string{"DISABLE_MOSQUITTO": disable}))
			diags := CheckBrokerSources(mode, []MQTTSource{{Name: "s", Broker: broker}}, false)
			if WorstBrokerSeverity(diags) != SeverityError {
				t.Errorf("broker %q (DISABLE_MOSQUITTO=%q): want an error; got:\n%s", broker, disable, diagText(diags))
			}
		}
	}
}

func TestWellFormedSchemesAreAccepted(t *testing.T) {
	mode := ResolveBrokerMode(brokerEnv(map[string]string{"DISABLE_MOSQUITTO": "true"}))
	for _, broker := range []string{
		"mqtt://broker.example:1883",
		"mqtts://broker.example:8883",
		"tcp://broker.example:1883",
		"ssl://broker.example:8883",
		"ws://broker.example/mqtt",
		"wss://broker.example/mqtt",
	} {
		diags := CheckBrokerSources(mode, []MQTTSource{{Name: "s", Broker: broker}}, false)
		if len(diags) != 0 {
			t.Errorf("broker %q must be accepted; got:\n%s", broker, diagText(diags))
		}
	}
}

func TestDiagnosticsNeverCarryCredentials(t *testing.T) {
	// Diagnostics are logged and can end up in an operator's paste buffer
	// or a bug report. Nothing from the user-info, query or fragment of a
	// broker URL may appear in them.
	const pass = "sup3rs3cret"
	const user = "observer-rw"
	const token = "tok-abcdef"
	mode := ResolveBrokerMode(brokerEnv(map[string]string{"DISABLE_MOSQUITTO": "true"}))
	for _, broker := range []string{
		"mqtt://" + user + ":" + pass + "@localhost:1883",
		"mqtt://" + user + ":" + pass + "@127.0.0.1:1883",
		"mqtt://" + user + ":" + pass + "@broker.example:1883?token=" + token,
		"http://" + user + ":" + pass + "@broker.example:1883",
		user + ":" + pass + "@broker.example:1883",
	} {
		diags := CheckBrokerSources(mode, []MQTTSource{{Name: "s", Broker: broker}}, false)
		joined := diagText(diags)
		for _, secret := range []string{pass, user, token} {
			if strings.Contains(joined, secret) {
				t.Errorf("broker %q: diagnostic leaked %q:\n%s", broker, secret, joined)
			}
		}
	}
}

// Review round 2, finding 4: the credential test above only proves nothing
// leaks. Removing the user-info strip in brokerHost makes every credentialed
// URL fail to parse as a host, so a credentialed loopback-only external
// setup silently produces NO diagnostic at all — the dead deploy #372 is
// about goes unreported. Pin the diagnostic, not just the absence of a leak.
func TestCredentialedLoopbackOnlySourceIsStillFlagged(t *testing.T) {
	mode := ResolveBrokerMode(brokerEnv(map[string]string{"DISABLE_MOSQUITTO": "true"}))
	for _, broker := range []string{
		"mqtt://observer-rw:sup3rs3cret@localhost:1883",
		"mqtt://observer-rw:sup3rs3cret@127.0.0.1:1883",
		"mqtts://observer-rw:sup3rs3cret@[::1]:8883",
		"mqtt://observer-rw@localhost:1883",
	} {
		diags := CheckBrokerSources(mode, []MQTTSource{{Name: "cred", Broker: broker}}, false)
		joined := diagText(diags)
		if WorstBrokerSeverity(diags) != SeverityError {
			t.Errorf("broker %q: a credentialed loopback-only external source must be an error; got:\n%s", broker, joined)
		}
		if !strings.Contains(joined, "loopback") {
			t.Errorf("broker %q: the error must say the source points at loopback; got:\n%s", broker, joined)
		}
		// A URL whose user-info was not understood must not be reported as
		// merely "unusable" — that would hide the loopback problem.
		if strings.Contains(joined, "unusable") {
			t.Errorf("broker %q: a credentialed URL is usable; brokerHost must strip the user-info:\n%s", broker, joined)
		}
		for _, secret := range []string{"sup3rs3cret", "observer-rw"} {
			if strings.Contains(joined, secret) {
				t.Errorf("broker %q: diagnostic leaked %q:\n%s", broker, secret, joined)
			}
		}
	}
}

func TestStrictModeIsOptIn(t *testing.T) {
	// Turning a diagnostic into a refusal to start is a deployment-visible
	// change, so it must be explicitly requested and must not be the
	// default for an install that merely updates.
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{"false", false},
		{"0", false},
		{"true", true},
		{"TRUE", true},
		{"1", true},
		{"yes", true},
		{"on", true},
	} {
		if got := StrictBrokerSources(brokerEnv(map[string]string{"MQTT_SOURCE_STRICT": tc.value})); got != tc.want {
			t.Errorf("MQTT_SOURCE_STRICT=%q: strict = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestLoadConfigMarksTheDefaultedSource(t *testing.T) {
	// CheckBrokerSources can only tell "operator configured localhost" from
	// "nothing was configured" if LoadConfig records which it was.
	dir := t.TempDir()
	cfg, err := LoadConfig(dir + "/missing.json")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.sourcesDefaulted {
		t.Fatalf("a config with no mqttSources must be marked as defaulted")
	}

	path := dir + "/config.json"
	if err := os.WriteFile(path, []byte(`{"mqttSources":[{"name":"ext","broker":"mqtt://broker.example:1883","topics":["meshcore/#"]}]}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.sourcesDefaulted {
		t.Fatalf("an explicitly configured source must not be marked as defaulted")
	}
}

func TestEnvBrokerPrecedenceCountsAsExplicit(t *testing.T) {
	// MQTT_BROKER overrides whatever config.json declared, so a config
	// whose only source came from the environment is explicit, not
	// defaulted — otherwise external mode would reject a perfectly good
	// env-configured deploy.
	dir := t.TempDir()
	path := dir + "/config.json"
	if err := os.WriteFile(path, []byte(`{"mqttSources":[{"name":"file","broker":"mqtt://file.example:1883","topics":["meshcore/#"]}]}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("MQTT_BROKER", "mqtt://env.example:1883")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.sourcesDefaulted {
		t.Fatalf("a source from MQTT_BROKER must not be marked as defaulted")
	}
	if len(cfg.MQTTSources) != 1 || cfg.MQTTSources[0].Broker != "mqtt://env.example:1883" {
		t.Fatalf("MQTT_BROKER must win over config.json; got %+v", cfg.MQTTSources)
	}
	mode := ResolveBrokerMode(brokerEnv(map[string]string{"DISABLE_MOSQUITTO": "true"}))
	if diags := CheckBrokerSources(mode, cfg.MQTTSources, cfg.sourcesDefaulted); len(diags) != 0 {
		t.Fatalf("an env-configured external broker must be accepted; got:\n%s", diagText(diags))
	}
}

func TestReportBrokerSetup_LogsEveryDiagnosticAndOnlyRefusesUnderStrict(t *testing.T) {
	bad := []MQTTSource{{Name: "local", Broker: "mqtt://localhost:1883"}}

	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }

	// Default: report, then carry on. An install that merely updates must
	// still start, even with a broker setup this check dislikes.
	fatal, _ := ReportBrokerSetup(brokerEnv(map[string]string{"DISABLE_MOSQUITTO": "true"}), bad, true, logf)
	if fatal {
		t.Fatalf("a broker error must not stop the ingestor unless MQTT_SOURCE_STRICT asks for it")
	}
	if len(lines) == 0 || !strings.Contains(strings.Join(lines, "\n"), "DISABLE_MOSQUITTO") {
		t.Fatalf("the diagnostic must be logged; got %v", lines)
	}

	// Strict: refuse to start, with the reason in the message.
	lines = nil
	fatal, msg := ReportBrokerSetup(brokerEnv(map[string]string{
		"DISABLE_MOSQUITTO":  "true",
		"MQTT_SOURCE_STRICT": "true",
	}), bad, true, logf)
	if !fatal {
		t.Fatalf("MQTT_SOURCE_STRICT=true must stop a start with no usable external source")
	}
	if !strings.Contains(msg, "MQTT_SOURCE_STRICT") {
		t.Errorf("the fatal message must name the setting that caused it; got %q", msg)
	}

	// Strict with only a warning (a junk DISABLE_MOSQUITTO): still starts.
	lines = nil
	fatal, _ = ReportBrokerSetup(brokerEnv(map[string]string{
		"DISABLE_MOSQUITTO":  "ture",
		"MQTT_SOURCE_STRICT": "true",
	}), []MQTTSource{{Name: "ext", Broker: "mqtt://broker.example:1883"}}, false, logf)
	if fatal {
		t.Fatalf("a warning must not stop a start even under MQTT_SOURCE_STRICT")
	}
	if len(lines) == 0 {
		t.Fatalf("the warning must still be logged")
	}

	// A sound setup logs nothing.
	lines = nil
	if fatal, _ = ReportBrokerSetup(brokerEnv(map[string]string{"DISABLE_MOSQUITTO": "true"}),
		[]MQTTSource{{Name: "ext", Broker: "mqtts://broker.example:8883"}}, false, logf); fatal {
		t.Fatalf("a sound external setup must start")
	}
	if len(lines) != 0 {
		t.Fatalf("a sound setup must log nothing; got %v", lines)
	}
}

func TestIngestorMainReportsBrokerSetupBeforeConnecting(t *testing.T) {
	src := readIngestorMain(t)
	if !regexp.MustCompile(`ReportBrokerSetup\(\s*os\.Getenv\s*,`).MatchString(src) {
		t.Fatalf("cmd/ingestor/main.go must call ReportBrokerSetup(os.Getenv, ...) after loading the config")
	}
	report := strings.Index(src, "ReportBrokerSetup(")
	connect := strings.Index(src, "ResolvedSources()")
	if report < 0 || connect < 0 {
		t.Fatalf("expected both ReportBrokerSetup and ResolvedSources in main.go (report=%d connect=%d)", report, connect)
	}
	if report > connect {
		t.Fatalf("the broker setup must be reported before the ingestor starts connecting to sources")
	}
}

// TestEmptyBrokerEnvironmentIsTheExternalDefault covers the keys every
// compose file forwards with an empty default (#372): present and empty is
// the same as unset. Since #441 that is the EXTERNAL-broker default, not
// the bundled one, so an install with no source configured is told so —
// but still starts unless MQTT_SOURCE_STRICT asks otherwise.
func TestEmptyBrokerEnvironmentIsTheExternalDefault(t *testing.T) {
	getenv := brokerEnv(map[string]string{
		"DISABLE_MOSQUITTO":  "",
		"MQTT_BROKER":        "",
		"MQTT_TOPIC":         "",
		"MQTT_SOURCE_STRICT": "",
	})
	mode := ResolveBrokerMode(getenv)
	if mode.Bundled || mode.Unrecognised {
		t.Errorf("an empty DISABLE_MOSQUITTO is the recognised external default; got %+v", mode)
	}
	if StrictBrokerSources(getenv) {
		t.Error("an empty MQTT_SOURCE_STRICT must not turn on strict mode")
	}
	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	fatal, msg := ReportBrokerSetup(getenv, []MQTTSource{{Name: "local", Broker: "mqtt://localhost:1883"}}, true, logf)
	if fatal {
		t.Errorf("an empty broker environment must not refuse to start without MQTT_SOURCE_STRICT; got %q", msg)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "no mqttSources are configured") || !strings.Contains(joined, "DISABLE_MOSQUITTO=false") {
		t.Errorf("the missing source must be reported, with the opt-in back to the bundled broker; got:\n%s", joined)
	}
}

// --- #870: do not claim "nothing is listening" when something is ---------

// withRealProbe runs a test against the production probe.
func withRealProbe(t *testing.T) {
	t.Helper()
	prev := brokerListenerProbe
	brokerListenerProbe = probeBrokerListener
	t.Cleanup(func() { brokerListenerProbe = prev })
}

func withProbe(t *testing.T, fn func(string) bool) {
	t.Helper()
	prev := brokerListenerProbe
	brokerListenerProbe = fn
	t.Cleanup(func() { brokerListenerProbe = prev })
}

func listenLoopback(t *testing.T) (addr string, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// A port that was free a moment ago and has no listener now.
func closedLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func externalEnv() func(string) string {
	return brokerEnv(map[string]string{"DISABLE_MOSQUITTO": "true"})
}

// Host networking: the broker runs on the host, the container's "loopback"
// is the host's. A listener answers, so there must be no error.
func TestExternalMode_LoopbackListenerAnswersIsNotAnError(t *testing.T) {
	withRealProbe(t)
	addr, _ := listenLoopback(t)
	src := []MQTTSource{{Name: "host", Broker: "mqtt://" + addr}}
	diags := CheckBrokerSources(ResolveBrokerMode(externalEnv()), src, false)
	if WorstBrokerSeverity(diags) == SeverityError {
		t.Fatalf("a loopback source with a listener must not be an error; got:\n%s", diagText(diags))
	}
	if strings.Contains(diagText(diags), "nothing is listening") {
		t.Fatalf("must not claim nothing is listening; got:\n%s", diagText(diags))
	}
	if !strings.Contains(diagText(diags), "info") {
		t.Errorf("the answered loopback source should be noted at info level; got:\n%s", diagText(diags))
	}
}

func TestExternalMode_LoopbackNoListenerIsStillAnError(t *testing.T) {
	withRealProbe(t)
	src := []MQTTSource{{Name: "dead", Broker: "mqtt://" + closedLoopbackAddr(t)}}
	diags := CheckBrokerSources(ResolveBrokerMode(externalEnv()), src, false)
	if WorstBrokerSeverity(diags) != SeverityError || !strings.Contains(diagText(diags), "nothing is listening") {
		t.Fatalf("no listener must keep today's error; got:\n%s", diagText(diags))
	}
}

func TestExternalMode_DefaultedSourceWithListenerIsNotAnError(t *testing.T) {
	withProbe(t, func(string) bool { return true })
	src := []MQTTSource{{Name: "local", Broker: "mqtt://localhost:1883"}}
	if diags := CheckBrokerSources(ResolveBrokerMode(externalEnv()), src, true); WorstBrokerSeverity(diags) == SeverityError {
		t.Fatalf("a defaulted localhost source that answers must not be an error; got:\n%s", diagText(diags))
	}
	withProbe(t, func(string) bool { return false })
	if diags := CheckBrokerSources(ResolveBrokerMode(externalEnv()), src, true); WorstBrokerSeverity(diags) != SeverityError {
		t.Fatalf("a defaulted localhost source with no listener stays an error; got:\n%s", diagText(diags))
	}
}

func TestExternalMode_AnyAnsweringLoopbackSourceSuppressesTheError(t *testing.T) {
	up, _ := listenLoopback(t)
	withRealProbe(t)
	src := []MQTTSource{
		{Name: "dead", Broker: "mqtt://" + closedLoopbackAddr(t)},
		{Name: "up", Broker: "mqtt://" + up},
	}
	if diags := CheckBrokerSources(ResolveBrokerMode(externalEnv()), src, false); WorstBrokerSeverity(diags) == SeverityError {
		t.Fatalf("one answering loopback source means ingest works; got:\n%s", diagText(diags))
	}
}

// Only loopback addresses are dialled: the check must never connect out.
func TestExternalMode_OnlyLoopbackSourcesAreProbed(t *testing.T) {
	var probed []string
	withProbe(t, func(a string) bool { probed = append(probed, a); return false })
	CheckBrokerSources(ResolveBrokerMode(externalEnv()), []MQTTSource{{Name: "loop", Broker: "mqtt://127.0.0.1:1884"}}, false)
	if len(probed) != 1 || probed[0] != "127.0.0.1:1884" {
		t.Fatalf("the loopback source must be dialled; probed %v", probed)
	}
	// A real external source is never dialled, even next to a loopback one.
	probed = nil
	CheckBrokerSources(ResolveBrokerMode(externalEnv()), []MQTTSource{
		{Name: "loop", Broker: "mqtt://127.0.0.1:1884"},
		{Name: "ext", Broker: "mqtts://broker.example:8883"},
	}, false)
	for _, a := range probed {
		if strings.Contains(a, "broker.example") {
			t.Fatalf("an external source must never be dialled; probed %v", probed)
		}
	}
	// Bundled mode never needed a verdict, so it must not dial either.
	probed = nil
	CheckBrokerSources(ResolveBrokerMode(brokerEnv(map[string]string{"DISABLE_MOSQUITTO": "false"})),
		[]MQTTSource{{Name: "loop", Broker: "mqtt://localhost:1883"}}, false)
	if len(probed) != 0 {
		t.Fatalf("bundled mode must not dial; probed %v", probed)
	}
}

// Default ports: paho applies none for the tcp/ssl family (it hands uri.Host
// straight to the dialer), so a portless URL has no address to probe. ws and
// wss take 80 and 443 from gorilla/websocket (not verified against paho).
func TestBrokerAddr_DefaultPorts(t *testing.T) {
	for broker, want := range map[string]string{
		"ws://localhost/mqtt":           "localhost:80",
		"wss://localhost/mqtt":          "localhost:443",
		"mqtt://user:pw@localhost:1999": "localhost:1999",
		"ssl://[::1]:8883":              "[::1]:8883",
	} {
		got, err := brokerAddr(broker)
		if err != nil || got != want {
			t.Errorf("brokerAddr(%q) = %q, %v; want %q", broker, got, err, want)
		}
	}
	for _, broker := range []string{
		"mqtt://localhost", "tcp://127.0.0.1", "mqtts://localhost", "ssl://[::1]", "tls://localhost",
	} {
		if got, err := brokerAddr(broker); err == nil {
			t.Errorf("brokerAddr(%q) = %q; a portless tcp/ssl-family URL has no address paho can dial", broker, got)
		}
	}
}

// paho cannot connect to a portless tcp/ssl-family URL, so a listener on the
// conventional port must neither be probed nor turn the error into an info.
func TestExternalMode_PortlessTCPFamilyIsNotProbedAndStaysAnError(t *testing.T) {
	var probed []string
	withProbe(t, func(a string) bool { probed = append(probed, a); return true })
	for _, broker := range []string{"mqtt://localhost", "tcp://127.0.0.1", "mqtts://localhost", "ssl://[::1]"} {
		probed = nil
		diags := CheckBrokerSources(ResolveBrokerMode(externalEnv()), []MQTTSource{{Name: "p", Broker: broker}}, false)
		if len(probed) != 0 {
			t.Errorf("%s: a portless URL must not be probed; probed %v", broker, probed)
		}
		if WorstBrokerSeverity(diags) != SeverityError || strings.Contains(diagText(diags), "ingest is expected to work") {
			t.Errorf("%s: must stay an error; got:\n%s", broker, diagText(diags))
		}
	}
}

// A loopback source next to a real one keeps its "cannot connect" warning even
// when something answers on loopback: that case is deliberately not probed.
func TestExternalMode_MixedSourcesKeepTheWarningEvenIfALoopbackListenerAnswers(t *testing.T) {
	var probed []string
	withProbe(t, func(a string) bool { probed = append(probed, a); return true })
	diags := CheckBrokerSources(ResolveBrokerMode(externalEnv()), []MQTTSource{
		{Name: "loop", Broker: "mqtt://127.0.0.1:1884"},
		{Name: "ext", Broker: "mqtts://broker.example:8883"},
	}, false)
	if WorstBrokerSeverity(diags) != SeverityWarn || !strings.Contains(diagText(diags), "cannot connect") {
		t.Fatalf("the mixed-source warning must stay; got:\n%s", diagText(diags))
	}
	if strings.Contains(diagText(diags), "ingest is expected to work") || len(probed) != 0 {
		t.Fatalf("the mixed case must not be probed or turned into info; probed %v, got:\n%s", probed, diagText(diags))
	}
}

// Each distinct loopback address is dialled once, however many sources use it.
func TestExternalMode_DuplicateLoopbackAddressesAreProbedOnce(t *testing.T) {
	var probed []string
	withProbe(t, func(a string) bool { probed = append(probed, a); return false })
	CheckBrokerSources(ResolveBrokerMode(externalEnv()), []MQTTSource{
		{Name: "a", Broker: "mqtt://localhost:1883"},
		{Name: "b", Broker: "tcp://LOCALHOST:1883"},
		{Name: "c", Broker: "mqtt://user:pw@localhost:1883"},
		{Name: "d", Broker: "mqtt://127.0.0.1:1883"},
	}, false)
	if len(probed) != 2 {
		t.Fatalf("want 2 distinct dials (localhost:1883, 127.0.0.1:1883); probed %v", probed)
	}
}

func TestProbeBrokerListener_IsBounded(t *testing.T) {
	if brokerProbeTimeout <= 0 || brokerProbeTimeout > 2*time.Second {
		t.Fatalf("the probe must be short and bounded; got %v", brokerProbeTimeout)
	}
	// The dial itself must carry that timeout: an unbounded net.Dial would
	// bypass the injected dialer and leave this empty.
	var gotNetwork, gotAddr string
	var gotTimeout time.Duration
	calls := 0
	prev := brokerDial
	brokerDial = func(network, addr string, timeout time.Duration) (net.Conn, error) {
		calls++
		gotNetwork, gotAddr, gotTimeout = network, addr, timeout
		return nil, errors.New("stub: no listener")
	}
	t.Cleanup(func() { brokerDial = prev })
	if probeBrokerListener("127.0.0.1:1884") {
		t.Fatalf("a failed dial must report no listener")
	}
	if calls != 1 || gotNetwork != "tcp" || gotAddr != "127.0.0.1:1884" || gotTimeout != brokerProbeTimeout {
		t.Fatalf("probe must dial tcp 127.0.0.1:1884 once with timeout %v; got %d call(s): %s %s %v",
			brokerProbeTimeout, calls, gotNetwork, gotAddr, gotTimeout)
	}
	brokerDial = prev

	addr, closeFn := listenLoopback(t)
	if !probeBrokerListener(addr) {
		t.Fatalf("probe must report a listener")
	}
	closeFn()
	if probeBrokerListener(closedLoopbackAddr(t)) {
		t.Fatalf("probe must report no listener on a closed port")
	}
}

func TestReportBrokerSetup_StrictOnlyRefusesWhenNothingListens(t *testing.T) {
	strict := brokerEnv(map[string]string{"DISABLE_MOSQUITTO": "true", "MQTT_SOURCE_STRICT": "true"})
	var lines []string
	logf := func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }
	withRealProbe(t)

	addr, _ := listenLoopback(t)
	if fatal, msg := ReportBrokerSetup(strict, []MQTTSource{{Name: "host", Broker: "mqtt://" + addr}}, false, logf); fatal {
		t.Fatalf("strict mode must not refuse when a listener answers; got %q", msg)
	}
	for _, l := range lines {
		if strings.Contains(l, "error") || strings.Contains(l, "nothing is listening") {
			t.Fatalf("no error may be logged when a listener answers; got %q", l)
		}
	}

	lines = nil
	fatal, _ := ReportBrokerSetup(strict, []MQTTSource{{Name: "dead", Broker: "mqtt://" + closedLoopbackAddr(t)}}, false, logf)
	if !fatal {
		t.Fatalf("strict mode must still refuse when nothing listens")
	}
}

func TestWorstBrokerSeverity_InfoIsNotAWarning(t *testing.T) {
	info := BrokerDiagnostic{Severity: SeverityInfo, Message: "x"}
	if got := WorstBrokerSeverity([]BrokerDiagnostic{info}); got != "" {
		t.Fatalf("info alone must rank as none; got %q", got)
	}
	if got := WorstBrokerSeverity([]BrokerDiagnostic{info, {Severity: SeverityWarn}}); got != SeverityWarn {
		t.Fatalf("warn must outrank info; got %q", got)
	}
}
