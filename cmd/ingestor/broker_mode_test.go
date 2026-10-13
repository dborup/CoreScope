package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

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

func TestResolveBrokerMode_MatchesTheEntrypointSpelling(t *testing.T) {
	// docker/entrypoint-go.sh disables the bundled mosquitto on exactly
	// `DISABLE_MOSQUITTO=true`. The ingestor must read the same value the
	// same way, or it would report a broker mode the container is not in.
	for _, tc := range []struct {
		value       string
		wantBundled bool
		wantAmbig   bool
	}{
		{"", true, false},
		{"false", true, false},
		{"FALSE", true, false},
		{"0", true, false},
		{"true", false, false},
		// Review round 2, finding 7: docker/entrypoint-go.sh compares the
		// RAW value, so a surrounding space is NOT "true" to it and the
		// bundled broker keeps running. Trimming here would report the
		// opposite mode to the one the container is in. Such a value can
		// only arrive through the process environment — manage.sh's .env
		// reader and docker compose's dotenv parser both trim [T].
		{" true ", true, true},
		{" true", true, true},
		{"true ", true, true},
		{"\ttrue", true, true},
		{"TRUE", true, true},
		{"True", true, true},
		{"1", true, true},
		{"yes", true, true},
		{"on", true, true},
		{"y", true, true},
	} {
		got := ResolveBrokerMode(brokerEnv(map[string]string{"DISABLE_MOSQUITTO": tc.value}))
		if got.Bundled != tc.wantBundled {
			t.Errorf("DISABLE_MOSQUITTO=%q: Bundled = %v, want %v", tc.value, got.Bundled, tc.wantBundled)
		}
		if got.Ambiguous != tc.wantAmbig {
			t.Errorf("DISABLE_MOSQUITTO=%q: Ambiguous = %v, want %v", tc.value, got.Ambiguous, tc.wantAmbig)
		}
	}
}

func TestBrokerMode_AmbiguousValueIsReportedNotSilentlyResolved(t *testing.T) {
	// A value the entrypoint does not accept leaves the BUNDLED broker
	// running in this container, while the operator who typed it most
	// likely meant the opposite. The issue requires that this cannot
	// happen silently.
	mode := ResolveBrokerMode(brokerEnv(map[string]string{"DISABLE_MOSQUITTO": "yes"}))
	diags := CheckBrokerSources(mode, []MQTTSource{{Name: "ext", Broker: "mqtt://broker.example:1883"}}, false)
	if len(diags) == 0 {
		t.Fatalf("an ambiguous DISABLE_MOSQUITTO value must produce a diagnostic")
	}
	joined := diagText(diags)
	for _, want := range []string{"DISABLE_MOSQUITTO", "yes", "bundled"} {
		if !strings.Contains(joined, want) {
			t.Errorf("diagnostic must mention %q; got:\n%s", want, joined)
		}
	}
	if sev := WorstBrokerSeverity(diags); sev != SeverityWarn {
		t.Errorf("an ambiguous value is a warning, not an error (it must not block a start); got %q", sev)
	}
}

// Review round 2, finding 2: manage.sh does NOT leave the bundled broker
// running on an is_true() value. dc_prod/dc_staging swap in
// docker-compose.no-mosquitto.yml, which hard-codes DISABLE_MOSQUITTO=true
// in the container's environment — so a container that can see "yes" at all
// was NOT launched by manage.sh. The diagnostic must therefore not tell the
// operator that manage.sh believes it disabled the broker; it must name the
// launcher that actually delivers this value, a bare docker compose run.
func TestAmbiguousDiagnostic_DoesNotMisattributeTheModeToManageSh(t *testing.T) {
	mode := ResolveBrokerMode(brokerEnv(map[string]string{"DISABLE_MOSQUITTO": "yes"}))
	diags := CheckBrokerSources(mode, []MQTTSource{{Name: "ext", Broker: "mqtt://broker.example:1883"}}, false)
	joined := diagText(diags)
	if !strings.Contains(joined, "docker compose") {
		t.Errorf("the diagnostic must name the launcher that delivers an untranslated value (a bare `docker compose` run); got:\n%s", joined)
	}
	// It may mention manage.sh, but only to say it would have selected the
	// external compose file — never that it "may believe" the broker was
	// disabled while this container still runs it.
	for _, forbidden := range []string{"is_true() may read", "may believe", "manage.sh's is_true(), which accepts"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("the diagnostic still attributes this mode to manage.sh (%q); got:\n%s", forbidden, joined)
		}
	}
	if strings.Contains(joined, "manage.sh") && !strings.Contains(joined, "no-mosquitto") {
		t.Errorf("if the diagnostic mentions manage.sh it must say it swaps in the no-mosquitto compose file; got:\n%s", joined)
	}
}

func TestBrokerMode_UnambiguousValuesAreQuiet(t *testing.T) {
	for _, value := range []string{"", "false", "true"} {
		mode := ResolveBrokerMode(brokerEnv(map[string]string{"DISABLE_MOSQUITTO": value}))
		diags := CheckBrokerSources(mode, []MQTTSource{{Name: "ext", Broker: "mqtt://broker.example:1883"}}, false)
		if len(diags) != 0 {
			t.Errorf("DISABLE_MOSQUITTO=%q with a usable external source must be quiet; got:\n%s", value, diagText(diags))
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
	mode := ResolveBrokerMode(brokerEnv(map[string]string{}))
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

	// Strict with only a warning: still starts.
	lines = nil
	fatal, _ = ReportBrokerSetup(brokerEnv(map[string]string{
		"DISABLE_MOSQUITTO":  "yes",
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

// TestEmptyBrokerEnvironmentIsInert is the ingestor half of review round 2,
// finding 3. The compose files now forward MQTT_BROKER, MQTT_TOPIC and
// MQTT_SOURCE_STRICT with an empty default, so those keys are PRESENT and
// empty in the container of an install that never set them.
func TestEmptyBrokerEnvironmentIsInert(t *testing.T) {
	getenv := brokerEnv(map[string]string{
		"DISABLE_MOSQUITTO":  "",
		"MQTT_BROKER":        "",
		"MQTT_TOPIC":         "",
		"MQTT_SOURCE_STRICT": "",
	})
	mode := ResolveBrokerMode(getenv)
	if !mode.Bundled || mode.Ambiguous {
		t.Errorf("an empty DISABLE_MOSQUITTO must be the quiet bundled default; got %+v", mode)
	}
	if StrictBrokerSources(getenv) {
		t.Error("an empty MQTT_SOURCE_STRICT must not turn on strict mode")
	}
	diags := CheckBrokerSources(mode, []MQTTSource{{Name: "local", Broker: "mqtt://localhost:1883"}}, true)
	if len(diags) != 0 {
		t.Errorf("an all-empty broker environment must produce no diagnostic; got:\n%s", diagText(diags))
	}
	fatal, msg := ReportBrokerSetup(getenv, []MQTTSource{{Name: "local", Broker: "mqtt://localhost:1883"}}, true, func(string, ...any) {})
	if fatal {
		t.Errorf("an all-empty broker environment must never refuse to start; got %q", msg)
	}
}
