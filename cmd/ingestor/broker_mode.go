package main

// Broker-mode diagnostics (#372).
//
// CoreScope ships a bundled mosquitto. It is opt-in (#441): it runs only
// when DISABLE_MOSQUITTO is a recognised false value, and otherwise the
// ingestor is expected to reach the operator's own broker. Two things could
// go wrong silently in that arrangement:
//
//  1. Nothing validates the external source. LoadConfig falls back to
//     mqtt://localhost:1883 when no mqttSources are configured, which is
//     the bundled broker's address — with the bundled broker off, the
//     ingestor then connects to nothing and ingest stops with no explanation
//     beyond a connection error in the log.
//  2. A DISABLE_MOSQUITTO value nobody recognises. docker/entrypoint-go.sh,
//     manage.sh and this file read the value with one table
//     (test-fixtures/disable-mosquitto-cases.json), so they agree on every
//     value, but a typo silently takes the default.
//
// This file resolves the mode the way the entrypoint does and reports both
// hazards as diagnostics. They are diagnostics, not refusals: an install
// that merely updates must keep starting. MQTT_SOURCE_STRICT turns the
// errors into a refusal to start for operators who want that.
//
// Every broker URL reaches a diagnostic through brokerurl.Mask, and hosts
// are read out of the masked form, so no password, user name or query token
// can leave this file — the ingestor's log is not a secret store.

import (
	"fmt"
	"net"
	"strings"

	"github.com/meshcore-analyzer/brokerurl"
)

// Severity orders a diagnostic. An error means ingest is expected to
// receive nothing; a warning means something is off but data should still
// flow.
type Severity string

const (
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

// BrokerDiagnostic is one operator-facing finding about the broker setup.
type BrokerDiagnostic struct {
	Severity Severity
	Message  string
}

// BrokerMode is how the environment selected a broker.
type BrokerMode struct {
	// Bundled is true when the in-container mosquitto is expected to run,
	// decided exactly as docker/entrypoint-go.sh decides it.
	Bundled bool
	// Raw is the DISABLE_MOSQUITTO value exactly as the environment carries
	// it, for diagnostics.
	Raw string
	// Unrecognised is true when Raw is neither a true nor a false spelling,
	// so the default (bundled broker off) applied.
	Unrecognised bool
}

// ResolveBrokerMode reads DISABLE_MOSQUITTO through getenv (os.Getenv in
// production, a map in tests). Trimmed of POSIX whitespace and compared
// case-insensitively, true/1/yes/on disable the bundled broker and
// false/0/no/off enable it; unset, empty and anything else take the
// default, disabled. docker/entrypoint-go.sh and manage.sh's
// parse_disable_mosquitto apply the same table.
func ResolveBrokerMode(getenv func(string) string) BrokerMode {
	raw := getenv("DISABLE_MOSQUITTO")
	mode := BrokerMode{Raw: raw}
	switch strings.ToLower(strings.Trim(raw, " \t\n\v\f\r")) {
	case "false", "0", "no", "off":
		mode.Bundled = true
	case "true", "1", "yes", "on", "":
	default:
		mode.Unrecognised = true
	}
	return mode
}

// setting renders the DISABLE_MOSQUITTO value a diagnostic is about.
func (m BrokerMode) setting() string {
	if m.Raw == "" {
		return "DISABLE_MOSQUITTO unset (default true)"
	}
	return fmt.Sprintf("DISABLE_MOSQUITTO=%q", m.Raw)
}

// StrictBrokerSources reports whether MQTT_SOURCE_STRICT asks for broker
// errors to stop the process instead of being logged. It is a new setting
// with no legacy spelling to preserve, so it accepts the same truthy values
// manage.sh does.
func StrictBrokerSources(getenv func(string) string) bool {
	return isShellTruthy(strings.TrimSpace(getenv("MQTT_SOURCE_STRICT")))
}

// isShellTruthy is the truthy set .env.example documents for
// MQTT_SOURCE_STRICT.
func isShellTruthy(v string) bool {
	switch v {
	case "1", "true", "TRUE", "yes", "YES", "y", "Y", "on", "ON":
		return true
	}
	return false
}

// brokerSchemes are the schemes paho reaches a broker with, after
// Config.ResolvedSources maps mqtt:// and mqtts:// onto tcp:// and ssl://.
var brokerSchemes = map[string]bool{
	"mqtt": true, "mqtts": true,
	"tcp": true, "ssl": true, "tls": true,
	"ws": true, "wss": true,
}

// CheckBrokerSources returns what is wrong with the broker arrangement, or
// nothing when it is sound. sourcesDefaulted says whether the sources came
// from LoadConfig's built-in fallback rather than from config.json or the
// environment; see Config.sourcesDefaulted.
func CheckBrokerSources(mode BrokerMode, sources []MQTTSource, sourcesDefaulted bool) []BrokerDiagnostic {
	var diags []BrokerDiagnostic

	if mode.Unrecognised {
		diags = append(diags, BrokerDiagnostic{
			Severity: SeverityWarn,
			Message: fmt.Sprintf("DISABLE_MOSQUITTO=%q is not a recognised value (true/1/yes/on or false/0/no/off), "+
				"so the default applies and the bundled broker is off. Set it to %q to run the bundled broker, or to %q.",
				mode.Raw, "false", "true"),
		})
	}

	// Malformed sources are wrong in either mode: the ingestor cannot
	// connect to them at all.
	loopback, usable := 0, 0
	for _, src := range sources {
		host, err := brokerHost(src.Broker)
		if err != nil {
			diags = append(diags, BrokerDiagnostic{
				Severity: SeverityError,
				Message:  fmt.Sprintf("mqtt source %q is unusable: %v", src.Name, err),
			})
			continue
		}
		usable++
		if isLoopbackHost(host) {
			loopback++
		}
	}

	if mode.Bundled {
		// The bundled broker listens on loopback inside the container, so
		// loopback sources are the normal shape and say nothing.
		return diags
	}

	switch {
	case sourcesDefaulted:
		diags = append(diags, BrokerDiagnostic{
			Severity: SeverityError,
			Message: mode.setting() + " selects an external MQTT broker, but no mqttSources are configured: " +
				"the ingestor fell back to its built-in mqtt://localhost:1883, which is the bundled broker's address and " +
				"nothing is listening there. Configure mqttSources in config.json (or set MQTT_BROKER) to the external broker, " +
				"or set DISABLE_MOSQUITTO=false to run the bundled broker (it is opt-in).",
		})
	case usable == 0:
		// Every source was malformed; the errors above already say so.
	case loopback == usable:
		diags = append(diags, BrokerDiagnostic{
			Severity: SeverityError,
			Message: fmt.Sprintf("%s disables the bundled broker, but all %d configured mqtt source(s) point at loopback (%s): "+
				"nothing is listening there. Point them at the external broker's address, or set DISABLE_MOSQUITTO=false to run the bundled broker.",
				mode.setting(), usable, maskedBrokerList(sources)),
		})
	case loopback > 0:
		diags = append(diags, BrokerDiagnostic{
			Severity: SeverityWarn,
			Message: fmt.Sprintf("%s disables the bundled broker, so %d of %d mqtt source(s) pointing at loopback cannot connect (%s); "+
				"the remaining source(s) still ingest.", mode.setting(), loopback, usable, maskedBrokerList(sources)),
		})
	}
	return diags
}

// WorstBrokerSeverity returns SeverityError when any diagnostic is an
// error, SeverityWarn when at least one is a warning, and "" for none.
func WorstBrokerSeverity(diags []BrokerDiagnostic) Severity {
	worst := Severity("")
	for _, d := range diags {
		if d.Severity == SeverityError {
			return SeverityError
		}
		worst = SeverityWarn
	}
	return worst
}

// maskedBrokerList renders the loopback sources for a diagnostic, each
// through brokerurl.Mask.
func maskedBrokerList(sources []MQTTSource) string {
	var out []string
	for _, src := range sources {
		host, err := brokerHost(src.Broker)
		if err != nil || !isLoopbackHost(host) {
			continue
		}
		out = append(out, fmt.Sprintf("%s=%s", src.Name, brokerurl.Mask(src.Broker)))
	}
	return strings.Join(out, ", ")
}

// brokerHost returns the lowercased host of a broker URL. It reads the
// MASKED form (brokerurl.Mask), which has no user-info, query or fragment
// left in it, so neither the returned host nor any error built from this
// input can carry a credential. An error here means paho could not use the
// URL either.
func brokerHost(broker string) (string, error) {
	masked := strings.TrimSpace(brokerurl.Mask(strings.TrimSpace(broker)))
	if masked == "" {
		return "", fmt.Errorf("broker URL is empty")
	}
	scheme, rest, ok := strings.Cut(masked, "://")
	if !ok {
		return "", fmt.Errorf("broker URL has no scheme (want one of mqtt, mqtts, tcp, ssl, ws or wss)")
	}
	if !brokerSchemes[strings.ToLower(scheme)] {
		return "", fmt.Errorf("broker scheme %q is not an MQTT scheme (want one of mqtt, mqtts, tcp, ssl, ws or wss)", scheme)
	}
	// Mask drops the query and fragment but keeps a path, and replaces
	// user-info with its marker.
	if _, after, found := strings.Cut(rest, "@"); found {
		rest = after
	}
	if i := strings.IndexAny(rest, "/"); i >= 0 {
		rest = rest[:i]
	}
	host := rest
	if h, _, err := net.SplitHostPort(rest); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return "", fmt.Errorf("broker URL has no host")
	}
	return strings.ToLower(host), nil
}

// isLoopbackHost reports whether a host can only be reached from inside the
// same network namespace.
func isLoopbackHost(host string) bool {
	switch host {
	case "localhost", "ip6-localhost", "ip6-loopback":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.HasSuffix(host, ".localhost")
}

// ReportBrokerSetup logs every broker diagnostic through logf and reports
// whether the process must refuse to start.
//
// It refuses only when MQTT_SOURCE_STRICT asks for it AND a diagnostic is
// an error: an install that merely pulls a new image must keep starting
// exactly as it did, even if this check dislikes its broker setup. The
// returned message is the one the caller should die with.
func ReportBrokerSetup(getenv func(string) string, sources []MQTTSource, sourcesDefaulted bool, logf func(string, ...any)) (bool, string) {
	mode := ResolveBrokerMode(getenv)
	diags := CheckBrokerSources(mode, sources, sourcesDefaulted)
	for _, d := range diags {
		logf("[broker] %s: %s", d.Severity, d.Message)
	}
	if WorstBrokerSeverity(diags) != SeverityError {
		return false, ""
	}
	if !StrictBrokerSources(getenv) {
		logf("[broker] continuing anyway; set MQTT_SOURCE_STRICT=true to make a broker error stop the ingestor instead")
		return false, ""
	}
	return true, "MQTT_SOURCE_STRICT=true and the MQTT broker setup above has an error: refusing to start"
}
