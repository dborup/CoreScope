package main

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/meshcore-analyzer/brokerurl"
)

// maskBrokerURL returns a broker URL fit for the public /api/mqtt/status:
// all user-info (a lone user name or token too) becomes "****", and the
// query and fragment are dropped, also without a scheme and for URLs that
// url.Parse rejects (brokerurl.Mask). The ingestor already masks its
// brokers (#118); this is the second layer, for any ingestor version.
// `mqtt://user:secret@host:1883` -> `mqtt://****@host:1883`.
func maskBrokerURL(s string) string { return brokerurl.Mask(s) }

// maskBrokerText masks every broker URL or user-info in free text, such as
// an error message (brokerurl.MaskText).
func maskBrokerText(s string) string { return brokerurl.MaskText(s) }

// maskSourceName masks a source name or tag that is a raw broker URL: an
// older ingestor tagged an unnamed source with its raw broker. That is a
// name among rawBrokers (the brokers in the stats file's source_statuses)
// or one holding "://"; it is masked as one broker URL (Mask, which also
// covers whitespace in a password). Names the operator chose, such as
// "obs@north" or "Feed @ CPH", are returned as they are (#118).
func maskSourceName(s string, rawBrokers map[string]bool) string {
	if rawBrokers[s] || strings.Contains(s, "://") {
		return brokerurl.Mask(s)
	}
	return s
}

// rawBrokerSet returns the non-empty brokers of statuses as the stats file
// holds them, for maskSourceName.
func rawBrokerSet(statuses []MqttSourceStatus) map[string]bool {
	set := make(map[string]bool, len(statuses))
	for _, s := range statuses {
		if s.Broker != "" {
			set[s.Broker] = true
		}
	}
	return set
}

// maskLivenessKeys masks the keys (source tags) of the ingestor's liveness
// map (maskSourceName) for the public /api/healthz: an older ingestor
// tagged an unnamed source with its raw broker (#118). Keys that masking
// leaves unchanged keep their name; a masked key that coincides with
// another gets " (2)", " (3)", … so no entry is lost. Called on a cache
// refresh only.
//
// haveStatuses reports whether the stats file had source_statuses. An
// ingestor built 2026-06-07..06-12 wrote source_liveness without them, so
// rawBrokers is empty and a raw broker without a scheme
// ("user:pass@host:1883") would pass maskSourceName. Without statuses,
// every key holding '@' is therefore masked as a broker URL; a name the
// operator chose with an '@' loses what precedes it then.
func maskLivenessKeys(m map[string]SourceLivenessSnapshot, rawBrokers map[string]bool, haveStatuses bool) map[string]SourceLivenessSnapshot {
	if len(m) == 0 {
		return m
	}
	mask := func(k string) string {
		if !haveStatuses && strings.Contains(k, "@") {
			return brokerurl.Mask(k)
		}
		return maskSourceName(k, rawBrokers)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]SourceLivenessSnapshot, len(m))
	var masked []string
	for _, k := range keys {
		if mask(k) == k {
			out[k] = m[k]
		} else {
			masked = append(masked, k)
		}
	}
	for _, k := range masked {
		base := mask(k)
		key := base
		for n := 2; ; n++ {
			if _, taken := out[key]; !taken {
				break
			}
			key = base + " (" + strconv.Itoa(n) + ")"
		}
		out[key] = m[k]
	}
	return out
}

// MqttSourceStatus is the per-MQTT-source status row surfaced via
// /api/mqtt/status. Mirrors the on-disk shape the ingestor publishes
// (cmd/ingestor SourceStatusSnapshot) but with the broker URL credentials
// redacted before serving — the endpoint needs no API key, so nobody may
// see a broker password or token in the response (#1043, #118).
type MqttSourceStatus struct {
	Name               string `json:"name"`
	Broker             string `json:"broker"`
	Connected          bool   `json:"connected"`
	LastConnectUnix    int64  `json:"lastConnectUnix"`
	LastDisconnectUnix int64  `json:"lastDisconnectUnix"`
	LastPacketUnix     int64  `json:"lastPacketUnix"`
	ConnectCount       int64  `json:"connectCount"`
	DisconnectCount    int64  `json:"disconnectCount"`
	PacketsTotal       int64  `json:"packetsTotal"`
	PacketsLast5m      int64  `json:"packetsLast5m"`
	LastError          string `json:"lastError,omitempty"`
}

// MqttStatusResponse is the JSON envelope returned by /api/mqtt/status.
type MqttStatusResponse struct {
	Sources  []MqttSourceStatus `json:"sources"`
	SampleAt string             `json:"sampleAt"`
	// WatchdogLastTickUnix (#1749) is the unix-seconds timestamp of the
	// most recent ingestor watchdog tick. Surfaced so external monitoring
	// can detect a wedged watchdog goroutine (value older than ~2× the
	// scan interval — typically 60s — means the watchdog itself died,
	// not just one source). 0 / omitted: ingestor has never ticked yet
	// or is running an older build that did not publish this field.
	WatchdogLastTickUnix int64 `json:"watchdogLastTickUnix,omitempty"`
	// WatchdogPanicCount (#1810 round-1) is the running total of
	// recovered panics inside the ingestor's watchdog per-source work
	// IIFE. Surfaced alongside WatchdogLastTickUnix because the tick
	// clock is stamped BEFORE per-source work — a loop panicking on
	// every source still advances the tick and looks healthy by tick
	// alone. A rapidly-growing value means the watchdog is alive but
	// per-source processing is broken. 0 / omitted: no recovered
	// panics OR older ingestor build.
	WatchdogPanicCount int64 `json:"watchdogPanicCount,omitempty"`
	// WatchdogLogDropCount (#1853) is the running total of ingestor
	// watchdog log calls dropped because the async emit queue was full.
	// 0 / omitted: no drops OR older ingestor build that predates
	// newAsyncEmit.
	WatchdogLogDropCount int64 `json:"watchdogLogDropCount,omitempty"`
	// Stale (#160) is true when sampleAt is older than
	// IngestorStatsStaleThreshold (the rule /api/perf/io applies to the
	// same file) or cannot be parsed: the ingestor is down or cannot
	// write its stats file, and the rows are frozen. False without a
	// stats file; sampleAt is then empty and there are no rows.
	Stale bool `json:"stale"`
	// SampleAgeSec (#160) is the age of sampleAt in whole seconds when
	// the response was built. Omitted when there is no readable sampleAt.
	SampleAgeSec *int64 `json:"sampleAgeSec,omitempty"`
}

// mqttStatusStaleness returns Stale and SampleAgeSec for a sampleAt stamp
// read from the stats file at now.
func mqttStatusStaleness(stamp string, now time.Time) (stale bool, ageSec *int64) {
	ts, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return true, nil
	}
	age := int64(now.Sub(ts) / time.Second)
	return ingestorStatsStale(ts, now), &age
}

// ingestorMqttStatusEnvelope is the partial shape the server decodes from
// the ingestor stats file (additive — older ingestors omit the field).
type ingestorMqttStatusEnvelope struct {
	SampledAt            string             `json:"sampledAt"`
	SourceStatuses       []MqttSourceStatus `json:"source_statuses"`
	WatchdogLastTickUnix int64              `json:"watchdogLastTickUnix"`
	WatchdogPanicCount   int64              `json:"watchdogPanicCount"`
	WatchdogLogDropCount int64              `json:"watchdogLogDropCount"`
}

// handleMqttStatus serves GET /api/mqtt/status. Reads the ingestor stats
// file, masks broker-URL credentials, and returns the per-source status
// list. Returns an empty list (200 OK) when the stats file is missing
// or unparseable — the UI panel renders a "no data yet" state.
func (s *Server) handleMqttStatus(w http.ResponseWriter, r *http.Request) {
	resp := MqttStatusResponse{Sources: []MqttSourceStatus{}, SampleAt: ""}
	data, err := os.ReadFile(IngestorStatsPath())
	if err != nil {
		writeJSON(w, resp)
		return
	}
	var env ingestorMqttStatusEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		writeJSON(w, resp)
		return
	}
	resp.SampleAt = env.SampledAt
	resp.Stale, resp.SampleAgeSec = mqttStatusStaleness(env.SampledAt, time.Now())
	resp.WatchdogLastTickUnix = env.WatchdogLastTickUnix
	resp.WatchdogPanicCount = env.WatchdogPanicCount
	resp.WatchdogLogDropCount = env.WatchdogLogDropCount
	rawBrokers := rawBrokerSet(env.SourceStatuses)
	for _, src := range env.SourceStatuses {
		// An older ingestor tagged an unnamed source with its raw
		// broker, and broker libraries occasionally quote the failing
		// URL in the error string — mask both (#118).
		src.Name = maskSourceName(src.Name, rawBrokers)
		src.Broker = maskBrokerURL(src.Broker)
		src.LastError = maskBrokerText(src.LastError)
		resp.Sources = append(resp.Sources, src)
	}
	writeJSON(w, resp)
}
