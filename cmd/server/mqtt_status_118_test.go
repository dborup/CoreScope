package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #118 re-review: /api/mqtt/status is public (no API key). The broker,
// name and lastError it serves come from the ingestor stats file, and an
// ingestor of any version may have written them raw, so the server masks
// defensively: all user-info (a lone user name or token too), the query
// and the fragment, also without a scheme and for URLs url.Parse rejects.

// secrets118 are the credential parts used in the inputs below; none may
// reach a response.
var secrets118 = []string{"secret", "tok3n", "abc", "dev-user", "p%zz", "2024", "1234", "frag", "sec ret"}

func assertNoSecrets118(t *testing.T, what, s string) {
	t.Helper()
	for _, bad := range secrets118 {
		if strings.Contains(s, bad) {
			t.Errorf("%s leaks %q: %s", what, bad, s)
		}
	}
}

func TestMaskBrokerURLStripsAllCredentials_118(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"dev-user:secret@host:1883", "****@host:1883"},               // no scheme
		{"tcp://tok3n@host", "tcp://****@host"},                       // user name / token only
		{"wss://host/mqtt?token=abc", "wss://host/mqtt"},              // query
		{"wss://host/mqtt#frag", "wss://host/mqtt"},                   // fragment
		{"tcp://dev-user:p%zz@host", "tcp://****@host"},               // url.Parse fails
		{"mqtt://dev-user:secret@host:1883", "mqtt://****@host:1883"}, // user name too
		{"tcp://dev-user:2024/secret@host:1883", "tcp://****@host:1883"},
		{"tcp://dev-user:1234?abc@host", "tcp://****@host"},
		{"dev-user:sec://ret@host", "****@host"}, // "://" inside the password
		{"MQTTS://dev-user:secret@host:8883/x?abc", "MQTTS://****@host:8883/x"},
		{"mqtt://u:p@broker:1883", "mqtt://****@broker:1883"},
		{"wss://host/mqtt?u=me@x.org", "wss://****@x.org"}, // the '@' in the query: cut, and marked as cut
		// unchanged
		{"mqtt://broker.example.com:1883", "mqtt://broker.example.com:1883"},
		{"wss://broker.example.com/mqtt", "wss://broker.example.com/mqtt"},
		{"host:1883", "host:1883"},
		{"", ""},
	} {
		got := maskBrokerURL(c.in)
		assertNoSecrets118(t, "maskBrokerURL("+c.in+")", got)
		if got != c.want {
			t.Errorf("maskBrokerURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Free-form text (lastError, and name when an older ingestor used the raw
// broker as tag): every broker-shaped token is masked, the rest is kept.
func TestMaskBrokerTextStripsCredentials_118(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`dial "wss://host/mqtt?token=abc": bad handshake`, `dial "wss://host/mqtt bad handshake`},
		{"network error tcp://dev-user:secret@host:1883: refused", "network error tcp://****@host:1883: refused"},
		{"auth dev-user:secret@host failed", "auth ****@host failed"},
		{"EOF", "EOF"},
		{"read tcp 10.0.0.1:5000->10.0.0.2:1883: connection reset by peer", "read tcp 10.0.0.1:5000->10.0.0.2:1883: connection reset by peer"},
		{"Local Feed #1", "Local Feed #1"},
	} {
		got := maskBrokerText(c.in)
		assertNoSecrets118(t, "maskBrokerText("+c.in+")", got)
		if got != c.want {
			t.Errorf("maskBrokerText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func writeStats118(t *testing.T, v any) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ingestor-stats.json")
	t.Setenv("CORESCOPE_INGESTOR_STATS", p)
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// End to end through the handler, with a stats file as an older ingestor
// writes it (raw broker, raw broker as the tag of an unnamed source).
func TestMqttStatusServesNoCredentials_118(t *testing.T) {
	var sources []map[string]any
	for _, raw := range []string{
		"dev-user:secret@host:1883",
		"tcp://tok3n@host",
		"wss://host/mqtt?token=abc",
		"tcp://dev-user:p%zz@host",
		"tcp://dev-user:2024/secret@host:1883",
		"tcp://dev-user:sec ret@host:1883", // a space splits MaskText tokens
	} {
		sources = append(sources, map[string]any{
			"name":      raw,
			"broker":    raw,
			"lastError": "connect " + raw + " failed",
		})
	}
	writeStats118(t, map[string]any{"sampledAt": "2026-09-30T12:00:00Z", "source_statuses": sources})

	rec := httptest.NewRecorder()
	(&Server{}).handleMqttStatus(rec, httptest.NewRequest(http.MethodGet, "/api/mqtt/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	assertNoSecrets118(t, "/api/mqtt/status", rec.Body.String())
	var resp MqttStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Sources) != len(sources) {
		t.Fatalf("got %d sources, want %d", len(resp.Sources), len(sources))
	}
	for _, s := range resp.Sources {
		if !strings.Contains(s.Broker, "host") || !strings.Contains(s.Name, "host") {
			t.Errorf("masked row lost its host: name %q broker %q", s.Name, s.Broker)
		}
		// only the broker in the error text is masked, not the message
		if !strings.HasPrefix(s.LastError, "connect ") || !strings.HasSuffix(s.LastError, " failed") || !strings.Contains(s.LastError, "host") {
			t.Errorf("lastError masked beyond its broker: %q", s.LastError)
		}
	}
}

// /api/healthz (public) keys ingest_liveness by source tag, which an older
// ingestor set to the raw broker of an unnamed source. Masked keys that
// coincide stay separate entries.
func TestIngestLivenessKeysHaveNoCredentials_118(t *testing.T) {
	resetSourceLivenessCache()
	t.Cleanup(resetSourceLivenessCache)
	writeStats118(t, map[string]any{"source_liveness": map[string]any{
		"tcp://dev-user:secret@host:1883":  map[string]int64{"lastReceiptUnix": 1},
		"tcp://tok3n@host:1883":            map[string]int64{"lastReceiptUnix": 2},
		"tcp://dev-user:sec ret@host:1883": map[string]int64{"lastReceiptUnix": 4},
		"feed":                             map[string]int64{"lastReceiptUnix": 3},
	}})
	got := readIngestorSourceLiveness()
	if len(got) != 4 {
		t.Fatalf("got %d entries, want 4: %v", len(got), got)
	}
	if _, ok := got["feed"]; !ok {
		t.Errorf("plain tag renamed: %v", got)
	}
	receipts := map[int64]bool{}
	for k, v := range got {
		assertNoSecrets118(t, "ingest_liveness key", k)
		receipts[v.LastReceiptUnix] = true
	}
	if len(receipts) != 4 {
		t.Errorf("entries merged or lost: %v", got)
	}
}

// A name is masked only when it is a raw broker URL: one of the stats
// file's raw brokers (an older ingestor tagged an unnamed source with it),
// or one holding "://". Whitespace in a password is covered then, also
// without a scheme; names the operator chose are left alone, '@' or not.
func TestMaskSourceName_118(t *testing.T) {
	raw := map[string]bool{"dev-user:sec ret@host:1883": true}
	for _, c := range []struct{ in, want string }{
		{"dev-user:sec ret@host:1883", "****@host:1883"},
		{"tcp://dev-user:sec ret@host:1883", "tcp://****@host:1883"},
		{"tcp://host:1883 (2)", "tcp://host:1883 (2)"},
		{"tcp://****@host:1883 (2)", "tcp://****@host:1883 (2)"},
		{"obs@north", "obs@north"},
		{"Feed @ CPH", "Feed @ CPH"},
		{"Local Feed #1", "Local Feed #1"},
		{"feed", "feed"},
	} {
		got := maskSourceName(c.in, raw)
		assertNoSecrets118(t, "maskSourceName("+c.in+")", got)
		if got != c.want {
			t.Errorf("maskSourceName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// #118 round 3: names the operator chose are served as they are, and no
// longer collide into " (2)"; a raw broker as name is still masked.
func TestMqttStatusKeepsChosenNames_118(t *testing.T) {
	writeStats118(t, map[string]any{"source_statuses": []map[string]string{
		{"name": "obs@north", "broker": "tcp://north:1883"},
		{"name": "obs@south", "broker": "tcp://south:1883"},
		{"name": "Feed @ CPH", "broker": "tcp://cph:1883"},
		{"name": "dev-user:secret@host:1883", "broker": "dev-user:secret@host:1883"},
		{"name": "tcp://tok3n@host", "broker": "tcp://tok3n@host"},
	}})
	rec := httptest.NewRecorder()
	(&Server{}).handleMqttStatus(rec, httptest.NewRequest(http.MethodGet, "/api/mqtt/status", nil))
	assertNoSecrets118(t, "/api/mqtt/status", rec.Body.String())
	var resp MqttStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range resp.Sources {
		names = append(names, s.Name)
	}
	want := []string{"obs@north", "obs@south", "Feed @ CPH", "****@host:1883", "tcp://****@host"}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Errorf("names\n got %q\nwant %q", names, want)
	}
}

func TestIngestLivenessKeepsChosenNames_118(t *testing.T) {
	resetSourceLivenessCache()
	t.Cleanup(resetSourceLivenessCache)
	writeStats118(t, map[string]any{
		"source_statuses": []map[string]string{
			{"name": "obs@north", "broker": "tcp://north:1883"},
			{"name": "dev-user:secret@host:1883", "broker": "dev-user:secret@host:1883"},
		},
		"source_liveness": map[string]any{
			"obs@north":                 map[string]int64{"lastReceiptUnix": 1},
			"obs@south":                 map[string]int64{"lastReceiptUnix": 2},
			"Feed @ CPH":                map[string]int64{"lastReceiptUnix": 3},
			"dev-user:secret@host:1883": map[string]int64{"lastReceiptUnix": 4},
			"tcp://tok3n@host:1883":     map[string]int64{"lastReceiptUnix": 5},
		},
	})
	got := readIngestorSourceLiveness()
	want := map[string]int64{"obs@north": 1, "obs@south": 2, "Feed @ CPH": 3, "****@host:1883": 4, "tcp://****@host:1883": 5}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k].LastReceiptUnix != v {
			t.Errorf("key %q: got %v, want lastReceiptUnix %d (all: %v)", k, got[k], v, got)
		}
	}
}
