package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// #118 review round 3.

// What the ingestor logs, registers and publishes keeps "****@" where it
// cut user-info, so an operator can see that the URL carries credentials,
// and a host that is only what follows an '@' reads as cut short.
func TestBrokerForLogMarksRemovedUserinfo_118(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"mqtt://u:p@broker:1883", "mqtt://****@broker:1883"},
		{"wss://host/mqtt?u=me@x.org", "wss://****@x.org"},
		{"wss://host/mqtt?token=abc", "wss://host/mqtt"},
		{"broker:1883", "tcp://broker:1883"},
	} {
		if got := brokerForLog(c.in); got != c.want {
			t.Errorf("brokerForLog(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	resetSourceStatusRegistry()
	t.Cleanup(resetSourceStatusRegistry)
	if got := RegisterSourceStatus("t", "mqtt://u:p@broker:1883").snapshot(time.Now()).Broker; got != "mqtt://****@broker:1883" {
		t.Errorf("status broker = %q, want mqtt://****@broker:1883", got)
	}
	if got := mqttSourceTag(MQTTSource{Broker: "wss://host/mqtt?u=me@x.org"}); got != "wss://****@x.org" {
		t.Errorf("tag = %q, want wss://****@x.org", got)
	}
}

// The secrets the ingestor knows are replaced literally before MaskText,
// which only spots URL-shaped text: a query token quoted without its
// scheme, or the configured user name and password, would pass it.
func TestErrForLogMasksKnownSecrets_118(t *testing.T) {
	src := MQTTSource{
		Broker:   "wss://" + credUser + ":" + credPass + "@host/mqtt?token=abc",
		Username: "cfg-user",
		Password: "cfg-pass",
	}
	secrets := mqttSourceSecrets(src)
	for _, c := range []struct{ in, want string }{
		{"dial host/mqtt?token=abc: refused", "dial host/mqtt?****: refused"},
		{"not authorized: cfg-user/cfg-pass", "not authorized: ****/****"},
		{"auth " + credUser + ":" + credPass + " rejected", "auth **** rejected"},
		{"connect tcp://x:y@host failed", "connect tcp://****@host failed"}, // MaskText still runs
		{"EOF", "EOF"},
	} {
		got := errForLog(errors.New(c.in), secrets...)
		assertNoSecretParts118(t, "errForLog("+c.in+")", got)
		if got != c.want {
			t.Errorf("errForLog(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// longest first: a user name that is also the start of the URL's
	// user-info must not leave the password behind
	both := mqttSourceSecrets(MQTTSource{Broker: "tcp://" + credUser + ":" + credPass + "@host", Username: credUser})
	if got := errForLog(errors.New("auth "+credUser+":"+credPass), both...); got != "auth ****" {
		t.Errorf("user name before user-info: %q, want %q", got, "auth ****")
	}
	// only non-empty values: an empty one would mask between every rune
	if got := errForLog(errors.New("EOF"), mqttSourceSecrets(MQTTSource{Broker: "tcp://host?"})...); got != "EOF" {
		t.Errorf("no secrets: %q", got)
	}
}

// The disconnect handler main() installs logs and stores the error with
// the source's secrets masked.
func TestDisconnectErrorMasksKnownSecrets_118(t *testing.T) {
	resetSourceStatusRegistry()
	t.Cleanup(resetSourceStatusRegistry)
	buf := captureLog118(t)
	src := MQTTSource{Name: "feed", Broker: "wss://host/mqtt?token=abc", Password: "cfg-pass"}
	opts, status, _ := prepareMQTTSource(src, "feed")
	opts.OnConnectionLost(nil, errors.New("dial host/mqtt?token=abc: cfg-pass rejected"))
	const want = "dial host/mqtt?****: **** rejected"
	if got := status.snapshot(time.Now()).LastError; got != want {
		t.Errorf("lastError = %q, want %q", got, want)
	}
	if out := buf.String(); !strings.Contains(out, want) || strings.Contains(out, "abc") || strings.Contains(out, "cfg-pass") {
		t.Errorf("log:\n%s", out)
	}
}

// All four random bytes reach the suffix, in order.
func TestMQTTClientIDSuffixUsesAllRandomBytes_118(t *testing.T) {
	old := clientIDRandom
	t.Cleanup(func() { clientIDRandom = old })
	clientIDRandom = bytes.NewReader([]byte{0xde, 0xad, 0xbe, 0xef})
	if got := mqttClientID(MQTTSource{Name: "Feed"}); got != "corescope-feed-deadbeef" {
		t.Fatalf("client id %q, want corescope-feed-deadbeef", got)
	}
}

// A stale tmp file owned by another user is refused before anything is
// truncated, chmod-ed or written. Root's chmod succeeds on any file, and
// the owner may still hold it open, so failing chmod is no guard there.
func TestWriteStatsAtomicRefusesForeignTmp_118(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix file owners")
	}
	old := statsFileEUID
	t.Cleanup(func() { statsFileEUID = old })
	statsFileEUID = func() int { return os.Geteuid() + 1 } // the tmp is someone else's
	assertForeignTmpRefused118(t, func(string) {})
}

// A stale tmp file of our own loses its old content: without O_TRUNC the
// file is truncated after the owner check, and a longer stale file must
// not leave a tail behind the new JSON.
func TestWriteStatsAtomicTruncatesOwnStaleTmp_118(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.json")
	if err := os.WriteFile(path+".tmp", []byte(strings.Repeat("stale ", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeStatsAtomic(path, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != `{}` {
		t.Fatalf("stats file %q, %v; want {}", b, err)
	}
}

// The same with a real foreign owner, which needs root to set up.
func TestWriteStatsAtomicRefusesForeignTmpAsRoot_118(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() != 0 {
		t.Skip("needs root to plant a file owned by another user")
	}
	assertForeignTmpRefused118(t, func(tmp string) {
		if err := os.Chown(tmp, 65534, 65534); err != nil {
			t.Fatal(err)
		}
	})
}

func assertForeignTmpRefused118(t *testing.T, plant func(tmp string)) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stats.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte("planted"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		t.Fatal(err)
	}
	plant(tmp)
	if err := writeStatsAtomic(path, []byte(`{"source_statuses":[]}`)); err == nil {
		t.Error("writeStatsAtomic accepted a tmp file owned by another user")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stats file published: %v", err)
	}
	fi, err := os.Stat(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(tmp); string(b) != "planted" || fi.Mode().Perm() != 0o644 {
		t.Errorf("foreign tmp changed: %q mode %o", b, fi.Mode().Perm())
	}
}
