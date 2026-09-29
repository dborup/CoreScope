package main

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/eclipse/paho.mqtt.golang/packets"
)

// Issue #118: every source connects with an explicit client ID: the
// configured mqttSources[].clientId verbatim, else corescope-<name>-<random>
// generated once per client, so paho's auto-reconnect and the watchdog's
// force-reconnect reuse it, while unconfigured sources and processes differ.

func TestMQTTClientIDConfiguredIsVerbatim_118(t *testing.T) {
	for _, id := range []string{"my-ingestor-01", "Edge Box/1", "a"} {
		opts := buildMQTTOpts(MQTTSource{Name: "x", Broker: "tcp://h:1883", ClientID: id})
		if opts.ClientID != id {
			t.Errorf("configured clientId %q became %q", id, opts.ClientID)
		}
	}
}

func TestMQTTClientIDDefaultShape_118(t *testing.T) {
	cases := []struct {
		name, broker string
		want         string
	}{
		{"local", "mqtt://localhost:1883", `^corescope-local-[0-9a-f]{8}$`},
		{"Local Feed #1", "tcp://h:1883", `^corescope-local-feed-1-[0-9a-f]{8}$`},
		{"  ÆØÅ lincomatic__SJC ", "tcp://h:1883", `^corescope-lincomatic-sjc-[0-9a-f]{8}$`},
		// name empty or nothing left after sanitizing: broker host, no port
		{"", "mqtts://user:secret@MQTT.Example.com:8883", `^corescope-mqtt-example-com-[0-9a-f]{8}$`},
		{"###", "wss://ws.example.net/mqtt", `^corescope-ws-example-net-[0-9a-f]{8}$`},
		// neither: plain corescope
		{"", "", `^corescope-[0-9a-f]{8}$`},
		{"%%%", "tcp://:1883", `^corescope-[0-9a-f]{8}$`},
		{"", "not a url", `^corescope-[0-9a-f]{8}$`},
	}
	for _, c := range cases {
		id := buildMQTTOpts(MQTTSource{Name: c.name, Broker: c.broker}).ClientID
		if !regexp.MustCompile(c.want).MatchString(id) {
			t.Errorf("name %q broker %q: client id %q, want %s", c.name, c.broker, id, c.want)
		}
		if strings.Contains(id, "secret") || strings.Contains(id, "user") {
			t.Errorf("client id %q leaks broker credentials", id)
		}
	}
}

// A whitespace-only clientId counts as unset (a blank template value must
// not become a shared, colliding ID).
func TestMQTTClientIDBlankConfiguredIsGenerated_118(t *testing.T) {
	id := buildMQTTOpts(MQTTSource{Name: "local", Broker: "tcp://h:1883", ClientID: "   "}).ClientID
	if !regexp.MustCompile(`^corescope-local-[0-9a-f]{8}$`).MatchString(id) {
		t.Fatalf("blank clientId gave %q", id)
	}
}

func TestMQTTClientIDLongNameIsCapped_118(t *testing.T) {
	id := buildMQTTOpts(MQTTSource{Name: strings.Repeat("abcdefghij", 20), Broker: "tcp://h:1883"}).ClientID
	if len(id) > len("corescope-")+mqttClientIDMaxBase+1+8 {
		t.Fatalf("client id is %d chars: %q", len(id), id)
	}
	base := strings.TrimSuffix(strings.TrimPrefix(id, "corescope-"), id[len(id)-9:])
	if base == "" || strings.HasSuffix(base, "-") || strings.Contains(id, "--") {
		t.Fatalf("capped id %q has an empty base or a dangling separator", id)
	}
}

// Same source, same process or not: every construction is a new client and
// gets its own ID.
func TestMQTTClientIDUniqueAcrossConstructions_118(t *testing.T) {
	seen := map[string]bool{}
	src := MQTTSource{Name: "local", Broker: "mqtt://localhost:1883"}
	for i := 0; i < 5000; i++ {
		id := buildMQTTOpts(src).ClientID
		if seen[id] {
			t.Fatalf("duplicate client id %q after %d constructions", id, i)
		}
		seen[id] = true
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

// Should crypto/rand ever fail, the ID is still non-empty and still differs
// between constructions instead of collapsing to a shared value.
func TestMQTTClientIDRandomFailureStillUnique_118(t *testing.T) {
	old := clientIDRandom
	clientIDRandom = failingReader{}
	defer func() { clientIDRandom = old }()
	a := buildMQTTOpts(MQTTSource{Name: "local", Broker: "tcp://h:1883"}).ClientID
	b := buildMQTTOpts(MQTTSource{Name: "local", Broker: "tcp://h:1883"}).ClientID
	if !regexp.MustCompile(`^corescope-local-[0-9a-f]{8}$`).MatchString(a) || a == b {
		t.Fatalf("with crypto/rand failing: %q and %q", a, b)
	}
}

// The rest of the options are unchanged by the ID.
func TestMQTTClientIDKeepsOtherOptions_118(t *testing.T) {
	f := false
	opts := buildMQTTOpts(MQTTSource{Name: "n", Broker: "mqtts://h:8883", Username: "u", Password: "p", RejectUnauthorized: &f})
	if !opts.CleanSession || opts.Username != "u" || opts.Password != "p" || opts.TLSConfig == nil || !opts.TLSConfig.InsecureSkipVerify {
		t.Fatalf("options changed: clean=%v user=%q tls=%v", opts.CleanSession, opts.Username, opts.TLSConfig)
	}
}

func TestMQTTConnectedLogLine_118(t *testing.T) {
	line := mqttConnectedLogLine("feed", "mqtts://dev-user:tok3n-secret@broker.example:8883", "corescope-feed-0a1b2c3d")
	for _, bad := range []string{"tok3n-secret", "dev-user"} {
		if strings.Contains(line, bad) {
			t.Fatalf("log line leaks %q: %s", bad, line)
		}
	}
	for _, want := range []string{"MQTT [feed]", "broker.example:8883", "as client corescope-feed-0a1b2c3d"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line %q misses %q", line, want)
		}
	}
}

// config.example.json is copied as a live config by some deployments, so it
// must document clientId without setting one.
func TestConfigExampleHasNoLiteralClientID_118(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Sources []map[string]json.RawMessage `json:"mqttSources"`
		Doc     string                       `json:"_comment_mqttSources"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	for i, s := range cfg.Sources {
		if _, ok := s["clientId"]; ok {
			t.Errorf("mqttSources[%d] sets a literal clientId", i)
		}
	}
	if !strings.Contains(cfg.Doc, "clientId") {
		t.Error("_comment_mqttSources does not document clientId")
	}
}

// The legacy single-broker block becomes source "default" and gets a
// generated ID like any other unconfigured source.
func TestLegacyMQTTConfigGetsGeneratedClientID_118(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/config.json"
	if err := os.WriteFile(p, []byte(`{"mqtt":{"broker":"mqtt://old-mosquitto:1883","topic":"meshcore/#"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	id := buildMQTTOpts(cfg.MQTTSources[0]).ClientID
	if !regexp.MustCompile(`^corescope-default-[0-9a-f]{8}$`).MatchString(id) {
		t.Fatalf("legacy source client id %q", id)
	}
}

// ── loopback broker: the ID actually sent, across reconnects ──

type idBroker struct {
	ln    net.Listener
	mu    sync.Mutex
	ids   []string
	conns []net.Conn
	wg    sync.WaitGroup
}

func newIDBroker(t *testing.T) *idBroker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &idBroker{ln: ln}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			b.wg.Add(1)
			go b.serve(c)
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		b.dropAll()
		b.wg.Wait()
	})
	return b
}

func (b *idBroker) serve(c net.Conn) {
	defer b.wg.Done()
	defer c.Close()
	pkt, err := packets.ReadPacket(c)
	if err != nil {
		return
	}
	cp, ok := pkt.(*packets.ConnectPacket)
	if !ok {
		return
	}
	b.mu.Lock()
	b.ids = append(b.ids, cp.ClientIdentifier)
	b.conns = append(b.conns, c)
	b.mu.Unlock()
	ack := packets.NewControlPacket(packets.Connack).(*packets.ConnackPacket)
	ack.ReturnCode = packets.Accepted
	if ack.Write(c) != nil {
		return
	}
	for {
		p, err := packets.ReadPacket(c)
		if err != nil {
			return
		}
		switch p.(type) {
		case *packets.PingreqPacket:
			packets.NewControlPacket(packets.Pingresp).Write(c)
		case *packets.DisconnectPacket:
			return
		}
	}
}

func (b *idBroker) dropAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.conns {
		c.Close()
	}
}

func (b *idBroker) seen() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.ids...)
}

func waitForID(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// First connect, paho auto-reconnect after the broker drops the socket, and
// the watchdog's force-reconnect all present the same non-empty ID; a second
// client for the same source presents a different one.
func TestMQTTClientIDStableAcrossReconnects_118(t *testing.T) {
	b := newIDBroker(t)
	src := MQTTSource{Name: "loop", Broker: "tcp://" + b.ln.Addr().String()}
	opts := buildMQTTOpts(src).
		SetConnectTimeout(time.Second).
		SetMaxReconnectInterval(100 * time.Millisecond).
		SetConnectRetryInterval(50 * time.Millisecond)
	client := mqtt.NewClient(opts)
	client.Connect()
	defer client.Disconnect(0)
	waitForID(t, "first connect", func() bool { return len(b.seen()) == 1 && client.IsConnectionOpen() })

	b.dropAll() // paho auto-reconnect
	waitForID(t, "auto-reconnect", func() bool { return len(b.seen()) == 2 && client.IsConnectionOpen() })

	buildForceReconnectFn(client, "loop")() // watchdog force-reconnect
	waitForID(t, "force-reconnect", func() bool { return len(b.seen()) >= 3 && client.IsConnectionOpen() })

	ids := b.seen()
	if ids[0] == "" {
		t.Fatal("the client connected with an empty client id")
	}
	for i, id := range ids {
		if id != ids[0] {
			t.Fatalf("connect %d used client id %q, first used %q", i+1, id, ids[0])
		}
	}

	other := mqtt.NewClient(buildMQTTOpts(src).SetConnectTimeout(time.Second))
	other.Connect()
	defer other.Disconnect(0)
	waitForID(t, "second client", func() bool { return len(b.seen()) > len(ids) })
	if last := b.seen()[len(b.seen())-1]; last == ids[0] || last == "" {
		t.Fatalf("a second client for the same source reused id %q", last)
	}
}
