package main

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
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
		if u, err := url.Parse(source.Broker); err == nil {
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
// without user-info, query or fragment (brokerForLog), so credentials or
// device tokens embedded in it never reach the log.
func mqttConnectedLogLine(tag, broker, clientID string) string {
	return "MQTT [" + tag + "] connected to " + brokerForLog(broker) + " as client " + clientID
}

// mqttSourceTag is a source's tag in logs and the liveness/status
// registries: its name or, for an unnamed source, the broker without
// credentials (brokerForLog).
func mqttSourceTag(source MQTTSource) string {
	if source.Name != "" {
		return source.Name
	}
	return brokerForLog(source.Broker)
}

// brokerForLog returns broker without user-info, query or fragment, so
// credentials or tokens embedded in it never reach a log. A broker without
// a scheme is read as tcp://, as paho's AddBroker does.
func brokerForLog(broker string) string {
	if !strings.Contains(broker, "://") {
		broker = "tcp://" + broker
	}
	u, err := url.Parse(broker)
	if err != nil {
		// Unparseable: keep only what follows the last '@' (the host part)
		// and cut any query or fragment.
		if i := strings.LastIndex(broker, "@"); i >= 0 {
			broker = broker[i+1:]
		}
		if i := strings.IndexAny(broker, "?#"); i >= 0 {
			broker = broker[:i]
		}
		return broker
	}
	u.User = nil
	u.RawQuery, u.ForceQuery = "", false
	u.Fragment, u.RawFragment = "", ""
	return u.String()
}
