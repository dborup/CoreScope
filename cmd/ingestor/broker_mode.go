package main

// Broker-mode diagnostics (#372).
//
// CoreScope ships a bundled mosquitto that an operator can turn off with
// DISABLE_MOSQUITTO=true to point the ingestor at their own broker. Two
// things could go wrong silently in that arrangement:
//
//  1. Nothing validates the external source. LoadConfig falls back to
//     mqtt://localhost:1883 when no mqttSources are configured, which is
//     the bundled broker's address — with the bundled broker off, the
//     ingestor then connects to nothing and ingest stops with no explanation
//     beyond a connection error in the log.
//  2. The value of DISABLE_MOSQUITTO is read differently by different
//     launchers. docker/entrypoint-go.sh — the only reader inside this
//     container — disables mosquitto on exactly "true". manage.sh's
//     is_true() also accepts 1/TRUE/yes/y/on, but it resolves the value
//     itself: dc_prod/dc_staging swap in docker-compose.no-mosquitto.yml,
//     which hard-codes DISABLE_MOSQUITTO=true in the container. So a
//     container that can see "yes" here was started WITHOUT manage.sh, by
//     a bare `docker compose up` against docker-compose.yml, and the
//     bundled broker is running even though the operator asked for an
//     external one.
//
// This file resolves the mode the way the entrypoint does and reports both
// hazards as diagnostics. They are diagnostics, not refusals: an install
// that merely updates must behave exactly as before. MQTT_SOURCE_STRICT
// turns the errors into a refusal to start for operators who want that.
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
	// it — untrimmed, because the entrypoint compares it untrimmed.
	Raw string
	// Ambiguous is true when Raw is neither the exact "true" the entrypoint
	// requires nor a recognised false value. Such a value leaves the
	// bundled broker running in this container even though the operator
	// who typed it probably meant to disable it.
	Ambiguous bool
}

// ResolveBrokerMode reads DISABLE_MOSQUITTO through getenv (os.Getenv in
// production, a map in tests).
//
// The value is compared RAW, not trimmed: docker/entrypoint-go.sh does
// `[ "${DISABLE_MOSQUITTO:-false}" = "true" ]`, so " true" is not "true" to
// it and the bundled broker keeps running. Trimming here would report the
// opposite mode to the one this container is actually in. Such a value can
// only reach the container through the process environment — manage.sh's
// own .env reader and docker compose's dotenv parser both trim — which is
// exactly the case Ambiguous exists to flag.
func ResolveBrokerMode(getenv func(string) string) BrokerMode {
	raw := getenv("DISABLE_MOSQUITTO")
	mode := BrokerMode{Raw: raw, Bundled: raw != "true"}
	mode.Ambiguous = mode.Bundled && !isClearlyFalse(raw)
	return mode
}

// StrictBrokerSources reports whether MQTT_SOURCE_STRICT asks for broker
// errors to stop the process instead of being logged. It is a new setting
// with no legacy spelling to preserve, so it accepts the same truthy values
// manage.sh does.
func StrictBrokerSources(getenv func(string) string) bool {
	return isShellTruthy(strings.TrimSpace(getenv("MQTT_SOURCE_STRICT")))
}

// isShellTruthy mirrors manage.sh's is_true().
func isShellTruthy(v string) bool {
	switch v {
	case "1", "true", "TRUE", "yes", "YES", "y", "Y", "on", "ON":
		return true
	}
	return false
}

// isClearlyFalse reports whether a value unambiguously means "keep the
// bundled broker". Anything outside both this set and the exact "true" is
// ambiguous: the entrypoint keeps the bundled broker, but the operator who
// typed it may well have meant the opposite.
func isClearlyFalse(v string) bool {
	switch v {
	case "", "false", "FALSE", "False", "0", "no", "NO", "n", "N", "off", "OFF":
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

	if mode.Ambiguous {
		diags = append(diags, BrokerDiagnostic{
			Severity: SeverityWarn,
			Message: fmt.Sprintf("DISABLE_MOSQUITTO=%q is neither the exact %q the container entrypoint requires nor a recognised false value, "+
				"so the bundled broker WILL start in this container. A container that sees this value was started by a bare "+
				"`docker compose up` against docker-compose.yml: ./manage.sh resolves the value itself and swaps in "+
				"docker-compose.no-mosquitto.yml, which passes the exact %q instead. Set it to %q or %q so every launcher reads it the same way.",
				mode.Raw, "true", "true", "true", "false"),
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
			Message: "DISABLE_MOSQUITTO=true selects an external MQTT broker, but no mqttSources are configured: " +
				"the ingestor fell back to its built-in mqtt://localhost:1883, which is the bundled broker's address and " +
				"nothing is listening there. Configure mqttSources in config.json (or set MQTT_BROKER) to the external broker.",
		})
	case usable == 0:
		// Every source was malformed; the errors above already say so.
	case loopback == usable:
		diags = append(diags, BrokerDiagnostic{
			Severity: SeverityError,
			Message: fmt.Sprintf("DISABLE_MOSQUITTO=true disables the bundled broker, but all %d configured mqtt source(s) point at loopback (%s): "+
				"nothing is listening there. Point them at the external broker's address.", usable, maskedBrokerList(sources)),
		})
	case loopback > 0:
		diags = append(diags, BrokerDiagnostic{
			Severity: SeverityWarn,
			Message: fmt.Sprintf("DISABLE_MOSQUITTO=true disables the bundled broker, so %d of %d mqtt source(s) pointing at loopback cannot connect (%s); "+
				"the remaining source(s) still ingest.", loopback, usable, maskedBrokerList(sources)),
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
