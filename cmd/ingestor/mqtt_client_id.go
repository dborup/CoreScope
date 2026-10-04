package main

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/meshcore-analyzer/brokerurl"
)

// MQTT client IDs (#118).
//
// Without SetClientID paho connects with a zero-length client ID and
// CleanSession=true; whether the broker then assigns one, rejects the client
// or lets two ingestors take over each other's session is broker-dependent.
// Every source therefore gets an explicit ID:
//
//   - mqttSources[].clientId, used verbatim. It must be unique among all
//     clients connected to that broker at the same time.
//   - otherwise corescope-<name>-<8 hex>, generated once per client
//     construction (buildMQTTOpts). <name> is the sanitized source name, else
//     the broker host, else omitted; the suffix is 32 random bits from
//     crypto/rand. paho reuses the options for its own reconnects and the
//     watchdog's force-reconnect reuses the same client, so the ID is stable
//     for the process lifetime but differs between sources and processes.
//
// MQTT 3.1.1 (what paho tries first) only guarantees IDs of 1–23 characters
// from [0-9A-Za-z], although brokers such as Mosquitto and EMQX accept longer
// ones. paho falls back to MQTT 3.1 after a failed first handshake, and a
// strict 3.1 broker rejects IDs over 23 characters. The default is
// 19 characters plus the name part, so for such a broker configure a short
// clientId. (The previous empty ID was invalid under 3.1 as well.)

const mqttClientIDMaxBase = 32

// clientIDRandom is swapped in tests to simulate an entropy failure.
var clientIDRandom io.Reader = rand.Reader

var clientIDFallbackSeq atomic.Uint64

// clientIDNow feeds the fallback suffix; swapped in tests to model a coarse
// clock that returns the same reading twice.
var clientIDNow = time.Now

// mqttClientID returns the client ID for a new client of this source.
func mqttClientID(source MQTTSource) string {
	if strings.TrimSpace(source.ClientID) != "" {
		return source.ClientID
	}
	base := sanitizeClientIDPart(source.Name)
	if base == "" {
		// the masked broker: url.Parse alone may take a user name for
		// the host (see brokerForLog)
		if u, err := url.Parse(brokerForLog(source.Broker)); err == nil {
			base = sanitizeClientIDPart(u.Hostname())
		}
	}
	id := "corescope-"
	if base != "" {
		id += base + "-"
	}
	return id + clientIDSuffix()
}

// sanitizeClientIDPart lowercases s, keeps [a-z0-9], turns every other run
// into one '-', trims '-' and caps the result at mqttClientIDMaxBase.
func sanitizeClientIDPart(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
			continue
		}
		dash = true
	}
	out := b.String()
	if len(out) > mqttClientIDMaxBase {
		out = strings.TrimRight(out[:mqttClientIDMaxBase], "-")
	}
	return out
}

// clientIDSuffix is 8 hex chars from crypto/rand. Should that ever fail it
// mixes the clock and a process-wide counter instead, so two constructions
// still never share a suffix within a process.
func clientIDSuffix() string {
	var b [4]byte
	if _, err := io.ReadFull(clientIDRandom, b[:]); err != nil {
		v := uint64(clientIDNow().UnixNano()) ^ (clientIDFallbackSeq.Add(1) * 0x9E3779B97F4A7C15)
		binary.BigEndian.PutUint32(b[:], uint32(v>>32)^uint32(v))
	}
	return hex.EncodeToString(b[:])
}

// mqttConnectedLogLine is the "connected" log line. The broker URL is logged
// with its user-info masked and without query or fragment (brokerForLog), so
// credentials or device tokens embedded in it never reach the log.
func mqttConnectedLogLine(tag, broker, clientID string) string {
	return "MQTT [" + tag + "] connected to " + brokerForLog(broker) + " as client " + clientID
}

// mqttSourceTag is the base of a source's tag in logs and the
// liveness/status registries: its name or, for an unnamed source, the
// broker with its credentials masked (brokerForLog). mqttSourceTags makes the tags
// of a whole configuration unique.
func mqttSourceTag(source MQTTSource) string {
	if source.Name != "" {
		return source.Name
	}
	return brokerForLog(source.Broker)
}

// mqttSourceTags returns the tag of every source. Unnamed sources on the
// same broker (say, with different credentials) would share one tag: the
// second would lose watchdog tracking and the two would share status
// counters. So an unnamed source whose tag is taken, by any named source or
// an earlier unnamed one, gets " (2)", " (3)", … A duplicate Name is left as
// it is: that is a configuration error, reported by registerLivenessOrSkip.
func mqttSourceTags(sources []MQTTSource) []string {
	used := make(map[string]bool, len(sources))
	for _, s := range sources {
		if s.Name != "" {
			used[s.Name] = true
		}
	}
	tags := make([]string, len(sources))
	for i, s := range sources {
		tag := mqttSourceTag(s)
		if s.Name == "" {
			base := tag
			for n := 2; used[tag]; n++ {
				tag = base + " (" + strconv.Itoa(n) + ")"
			}
			used[tag] = true
		}
		tags[i] = tag
	}
	return tags
}

// brokerForLog returns broker with its user-info replaced by "****" and
// without query or fragment (brokerurl.Mask), so credentials or tokens
// embedded in it never reach a log, the stats file or a client ID, while the
// "****@" shows that the URL carries credentials and that the host may be
// cut short. A broker without a scheme is read as tcp://, as paho's
// AddBroker does.
func brokerForLog(broker string) string {
	if !strings.Contains(broker, "://") {
		broker = "tcp://" + broker
	}
	return brokerurl.Mask(broker)
}

// mqttSourceSecrets returns the non-empty secrets of a source: its
// password and user name, and the user-info, query and fragment of its
// broker URL as configured, with their parts, raw and decoded
// (brokerurl.Secrets).
func mqttSourceSecrets(source MQTTSource) []string {
	var out []string
	for _, v := range append([]string{source.Password, source.Username}, brokerurl.Secrets(source.Broker)...) {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// errForLog is err's text with each of secrets (mqttSourceSecrets) masked
// by "****" in one pass over merged matches, skipping values too short to
// tell from other text (brokerurl.MaskSecrets), and then any broker URL or
// user-info it quotes masked (brokerurl.MaskText): MaskText only spots
// URL-shaped text, so a query token or a password quoted on its own would
// pass it. paho's errors normally quote none, but they reach the log and
// the stats file.
func errForLog(err error, secrets ...string) string {
	if err == nil {
		return "<nil>"
	}
	return brokerurl.MaskText(brokerurl.MaskSecrets(err.Error(), secrets...))
}
