package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// #118 follow-up: the log tag of a source without a name used to be the raw
// broker URL, so user-info in it reached every MQTT log line. The tag and
// every logged broker must be free of credentials, also for a broker given
// without a scheme (paho then dials tcp://).

const (
	credUser = "dev-user"
	credPass = "s3cret-pass"
)

func assertNoCredentials(t *testing.T, what, s string) {
	t.Helper()
	for _, bad := range []string{credUser, credPass} {
		if strings.Contains(s, bad) {
			t.Errorf("%s leaks %q: %s", what, bad, s)
		}
	}
}

func TestMQTTSourceTagHasNoCredentials_118(t *testing.T) {
	for _, tc := range []struct{ name, broker, want string }{
		{"", "tcp://" + credUser + ":" + credPass + "@broker.example:1883", "tcp://****@broker.example:1883"},
		{"", credUser + ":" + credPass + "@broker.example:1883", "tcp://****@broker.example:1883"},
		{"", "wss://" + credUser + ":" + credPass + "@broker.example/mqtt?password=" + credPass, "wss://****@broker.example/mqtt"},
		{"", "tcp://broker.example:1883", "tcp://broker.example:1883"},
		{"", "broker.example:1883", "tcp://broker.example:1883"},
		{"feed", "tcp://" + credUser + ":" + credPass + "@broker.example:1883", "feed"},
	} {
		got := mqttSourceTag(MQTTSource{Name: tc.name, Broker: tc.broker})
		assertNoCredentials(t, "tag for "+tc.broker, got)
		if got != tc.want {
			t.Errorf("tag for name %q broker %q = %q, want %q", tc.name, tc.broker, got, tc.want)
		}
	}
	// unparseable input: everything up to the last '@' is dropped
	got := mqttSourceTag(MQTTSource{Broker: "tcp://" + credUser + ":" + credPass + "@bro ker%zz:1883"})
	assertNoCredentials(t, "tag for an unparseable broker", got)
}

// logBuf118 is a log sink safe to read while paho's goroutines log.
type logBuf118 struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuf118) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuf118) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog118 collects the standard logger's output for the test.
func captureLog118(t *testing.T) *logBuf118 {
	t.Helper()
	buf := &logBuf118{}
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	return buf
}

// A real connect through buildMQTTOpts to a loopback broker, for an unnamed
// source whose broker URL carries credentials, with and without a scheme.
// Every line logged (connection attempt, connected, disconnected,
// reconnecting, watchdog) must be free of them.
func TestMQTTLogLinesHaveNoCredentials_118(t *testing.T) {
	b := newIDBroker(t)
	addr := b.ln.Addr().String()
	for _, broker := range []string{
		"tcp://" + credUser + ":" + credPass + "@" + addr,
		credUser + ":" + credPass + "@" + addr,
	} {
		t.Run(broker[:3], func(t *testing.T) {
			buf := captureLog118(t)
			src := MQTTSource{Broker: broker}
			tag, logBroker := mqttSourceTag(src), brokerForLog(src.Broker)
			opts := buildMQTTOpts(src).SetConnectTimeout(time.Second)
			client := mqtt.NewClient(opts)
			seen := len(b.seen())
			client.Connect()
			defer client.Disconnect(0)
			waitForID(t, "connect", func() bool { return len(b.seen()) > seen && client.IsConnectionOpen() })
			log.Print(mqttConnectedLogLine(tag, src.Broker, opts.ClientID))
			s := &SourceLivenessState{Tag: tag, Broker: logBroker, FirstConnectedAt: time.Now().Add(-time.Hour).Unix()}
			if msg, _ := checkSourceLiveness(s, time.Minute, time.Now()); msg != "" {
				log.Print(msg)
			}
			out := buf.String()
			if !strings.Contains(out, "connection attempt #") || !strings.Contains(out, "connected to") {
				t.Fatalf("expected the attempt and connected lines, got:\n%s", out)
			}
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				assertNoCredentials(t, "log line", line)
			}
		})
	}
}

// main() and prepareMQTTSource wire the tag and the logged broker. A quick
// static check of both: no log call or RegisterSourceStatus may take a raw
// X.Broker argument, the tag must not be a raw broker, and the liveness
// state (whose Broker the watchdog logs) must get the log-safe broker. It only sees direct uses;
// TestMQTTSourceWiringLeaksNoCredentials_118 runs the handlers.
func TestMainLogsNoRawBroker_118(t *testing.T) {
	for _, file := range []string{"main.go", "mqtt_source.go"} {
		checkNoRawBrokerLogged118(t, file)
	}
}

func checkNoRawBrokerLogged118(t *testing.T, file string) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	isBroker := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "Broker"
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "RegisterSourceStatus" {
				for _, a := range n.Args {
					if isBroker(a) {
						t.Errorf("%s: status registry given a raw broker URL; the stats file publishes it", fset.Position(a.Pos()))
					}
				}
			}
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "log" {
					for _, a := range n.Args {
						if isBroker(a) {
							t.Errorf("%s: log call with a raw broker URL", fset.Position(a.Pos()))
						}
					}
				}
			}
		case *ast.AssignStmt:
			for i, l := range n.Lhs {
				if id, ok := l.(*ast.Ident); ok && id.Name == "tag" && i < len(n.Rhs) && isBroker(n.Rhs[i]) {
					t.Errorf("%s: tag assigned a raw broker URL", fset.Position(n.Pos()))
				}
			}
		case *ast.CompositeLit:
			if id, ok := n.Type.(*ast.Ident); ok && id.Name == "SourceLivenessState" {
				for _, el := range n.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok && isBroker(kv.Value) {
						t.Errorf("%s: SourceLivenessState.Broker set to a raw broker URL; the watchdog logs it", fset.Position(kv.Pos()))
					}
				}
			}
		}
		return true
	})
}

func TestBrokerForLogHasNoCredentials_118(t *testing.T) {
	u, _ := url.Parse("tcp://" + credUser + ":" + credPass + "@broker.example:1883")
	assertNoCredentials(t, "brokerForLog of paho's URL", brokerForLog(u.String()))
	assertNoCredentials(t, "brokerForLog without a scheme", brokerForLog(credUser+":"+credPass+"@broker.example:1883"))
}
