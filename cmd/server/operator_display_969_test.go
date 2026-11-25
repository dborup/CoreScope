package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/gorilla/mux"
)

// #969 — the operator's display defaults for timestamps and Live Moments
// reach the browser through /api/config/client, the one endpoint that
// already carries config.json's `timestamps` and `moments` blocks.
// /api/config/theme stays the colour/branding endpoint and carries
// neither, so every value has one source and one delivery path.

// loadConfig969 writes body as config.json in a temp dir and loads it,
// returning the config and what LoadConfig logged.
func loadConfig969(t *testing.T, body string) (*Config, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var cfg *Config
	var err error
	out := captureLog(func() { cfg, err = LoadConfig(dir) })
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg, out
}

// served969 returns the decoded body of path on a server running cfg.
func served969(t *testing.T, cfg *Config, path string) map[string]json.RawMessage {
	t.Helper()
	db := setupTestDB(t)
	srv := NewServer(db, cfg, NewHub())
	router := mux.NewRouter()
	srv.RegisterRoutes(router)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	if w.Code != 200 {
		t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return body
}

// The operator's timestamps block is served on /api/config/client,
// normalised, as the frontend now reads it.
func TestOperatorDisplay969_TimestampsServedOnClientConfig(t *testing.T) {
	cfg, _ := loadConfig969(t, `{"timestamps":{"defaultMode":"Absolute","timezone":"utc","formatPreset":"iso-seconds","customFormat":"YYYY","allowCustomFormat":true}}`)
	body := served969(t, cfg, "/api/config/client")
	if got := string(body["timestamps"]); got != `{"defaultMode":"absolute","timezone":"utc","formatPreset":"iso-seconds","customFormat":"YYYY","allowCustomFormat":true}` {
		t.Fatalf("timestamps %s", got)
	}
}

// Rule: canonical keys (#884). allowCustomFormat is the operator's gate for
// a free-form format string in every browser; only the exact-case key can
// open it, and a case variant of any timestamps key is ignored with a
// warning instead of overriding the canonical value.
func TestOperatorDisplay969_TimestampKeysAreCanonical(t *testing.T) {
	cfg, out := loadConfig969(t, `{"timestamps":{"defaultMode":"absolute","DefaultMode":"ago","allowCustomFormat":false,"AllowCustomFormat":true,"customFormat":"YYYY"}}`)
	ts := cfg.GetTimestampConfig()
	if ts.AllowCustomFormat {
		t.Fatal("a case variant opened allowCustomFormat")
	}
	if ts.DefaultMode != "absolute" {
		t.Fatalf("defaultMode %q: a case variant won over the canonical key", ts.DefaultMode)
	}
	for _, k := range []string{"timestamps.AllowCustomFormat is ignored", "timestamps.DefaultMode is ignored"} {
		if !strings.Contains(out, k) {
			t.Fatalf("no warning %q in %q", k, out)
		}
	}
	// A variant alone does not open the gate either.
	cfg, _ = loadConfig969(t, `{"timestamps":{"AllowCustomFormat":true}}`)
	if cfg.GetTimestampConfig().AllowCustomFormat {
		t.Fatal("AllowCustomFormat alone opened the gate")
	}
	// The canonical key does.
	cfg, _ = loadConfig969(t, `{"timestamps":{"allowCustomFormat":true}}`)
	if !cfg.GetTimestampConfig().AllowCustomFormat {
		t.Fatal("allowCustomFormat: true did not open the gate")
	}
	// A top-level case variant of the block is not the block.
	cfg, _ = loadConfig969(t, `{"Timestamps":{"defaultMode":"absolute"}}`)
	if got := cfg.GetTimestampConfig().DefaultMode; got != "ago" {
		t.Fatalf("Timestamps block applied: %q", got)
	}
}

// The operator's Moments display defaults: config.json moments.display,
// served as moments.display on /api/config/client, only the keys the
// operator set, only while moments are on.
func TestOperatorDisplay969_MomentsDisplayServed(t *testing.T) {
	cfg, _ := loadConfig969(t, `{"moments":{"enabled":true,"display":{"enabled":false,"dwellSeconds":20,"visibleCards":1,"showBusiest":false}}}`)
	body := served969(t, cfg, "/api/config/client")
	if got := string(body["moments"]); got != `{"enabled":true,"historySize":50,"display":{"enabled":false,"visibleCards":1,"dwellSeconds":20,"showBusiest":false}}` {
		t.Fatalf("moments %s", got)
	}

	// No display block: the moments block is as before.
	cfg, _ = loadConfig969(t, `{"moments":{"enabled":true}}`)
	if got := string(served969(t, cfg, "/api/config/client")["moments"]); got != `{"enabled":true,"historySize":50}` {
		t.Fatalf("moments without display %s", got)
	}

	// Moments off: no moments block at all, display or not.
	cfg, _ = loadConfig969(t, `{"moments":{"enabled":false,"display":{"dwellSeconds":20}}}`)
	if _, ok := served969(t, cfg, "/api/config/client")["moments"]; ok {
		t.Fatal("moments off: a moments block was served")
	}
}

// Rule: the operator's values stay inside the bounds the Live page uses;
// out of range is clamped with a log line, not a number is dropped.
func TestOperatorDisplay969_MomentsDisplayBounds(t *testing.T) {
	cfg, out := loadConfig969(t, `{"moments":{"enabled":true,"display":{"visibleCards":9,"dwellSeconds":1,"showNewNode":"no","enabled":1}}}`)
	d := cfg.Moments.Display
	if d == nil || d.VisibleCards == nil || *d.VisibleCards != 5 || d.DwellSeconds == nil || *d.DwellSeconds != 3 {
		t.Fatalf("not clamped: %+v", d)
	}
	if d.ShowNewNode != nil || d.Enabled != nil {
		t.Fatalf("a non-boolean switch was kept: %+v", d)
	}
	for _, k := range []string{"moments.display.visibleCards=9", "moments.display.dwellSeconds=1", "moments.display.showNewNode", "moments.display.enabled"} {
		if !strings.Contains(out, k) {
			t.Fatalf("no log line for %s in %q", k, out)
		}
	}
	cfg, out = loadConfig969(t, `{"moments":{"enabled":true,"display":{"dwellSeconds":7.5,"visibleCards":"3"}}}`)
	if d := cfg.Moments.Display; d != nil {
		t.Fatalf("a non-integer value was kept: %+v", d)
	}
	if !strings.Contains(out, "moments.display.dwellSeconds") || !strings.Contains(out, "moments.display.visibleCards") {
		t.Fatalf("no log line for the dropped values: %q", out)
	}
}

// Rule: canonical keys (#884) for moments.display: a case variant of the
// block or of a key is ignored with a warning.
func TestOperatorDisplay969_MomentsDisplayKeysAreCanonical(t *testing.T) {
	cfg, out := loadConfig969(t, `{"moments":{"enabled":true,"display":{"enabled":true,"Enabled":false,"DwellSeconds":20}}}`)
	d := cfg.Moments.Display
	if d == nil || d.Enabled == nil || *d.Enabled != true || d.DwellSeconds != nil {
		t.Fatalf("a case variant was read: %+v", d)
	}
	if !strings.Contains(out, "moments.display.Enabled is ignored") || !strings.Contains(out, "moments.display.DwellSeconds is ignored") {
		t.Fatalf("no warning: %q", out)
	}
	cfg, out = loadConfig969(t, `{"moments":{"enabled":true,"Display":{"dwellSeconds":20}}}`)
	if cfg.Moments.Display != nil {
		t.Fatalf("a Display block was read: %+v", cfg.Moments.Display)
	}
	if !strings.Contains(out, "moments.Display is ignored") {
		t.Fatalf("no warning: %q", out)
	}
}

// Rule: one source per value. /api/config/theme carries neither timestamps
// nor moments, even when the operator set both.
func TestOperatorDisplay969_ThemeEndpointCarriesNeither(t *testing.T) {
	cfg, _ := loadConfig969(t, `{"timestamps":{"defaultMode":"absolute"},"moments":{"enabled":true,"display":{"dwellSeconds":20}}}`)
	body := served969(t, cfg, "/api/config/theme")
	for _, k := range []string{"timestamps", "moments"} {
		if _, ok := body[k]; ok {
			t.Fatalf("/api/config/theme carries %s: a second source", k)
		}
	}
}

// The server's keys and bounds are the Live page's rule
// (LiveMoments.SETTINGS in public/live-moments.js), so a value the server
// accepts is one the page uses.
func TestOperatorDisplay969_BoundsMatchTheClient(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "public", "live-moments.js"))
	if err != nil {
		t.Fatal(err)
	}
	keys := regexp.MustCompile(`KEYS: \[([^\]]*)\]`).FindSubmatch(src)
	if keys == nil {
		t.Fatal("no SETTINGS.KEYS in live-moments.js")
	}
	var js []string
	for _, k := range strings.Split(string(keys[1]), ",") {
		js = append(js, strings.Trim(strings.TrimSpace(k), "'"))
	}
	var gos []string
	for _, f := range momentsDisplayFields() {
		gos = append(gos, f.key)
	}
	if strings.Join(js, ",") != strings.Join(gos, ",") {
		t.Fatalf("client keys %v, server %v", js, gos)
	}
	for _, f := range momentsDisplayFields() {
		if f.num == nil {
			continue
		}
		m := regexp.MustCompile(f.key + `: \[(\d+), (\d+)\]`).FindSubmatch(src)
		if m == nil {
			t.Fatalf("no LIMITS.%s in live-moments.js", f.key)
		}
		lo, _ := strconv.Atoi(string(m[1]))
		hi, _ := strconv.Atoi(string(m[2]))
		if lo != f.lo || hi != f.hi {
			t.Fatalf("%s: client %d-%d, server %d-%d", f.key, lo, hi, f.lo, f.hi)
		}
	}
}

// config.example.json's moments.display is the Live page's built-in
// DEFAULTS, so the example documents what the code does, and its
// timestamps block is the server's built-in timestamps.
func TestOperatorDisplay969_ExampleConfigMatchesTheBuiltIns(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, out := loadConfig969(t, string(data))
	if strings.Contains(out, "moments.display") || strings.Contains(out, "timestamps") {
		t.Fatalf("the example logs about its own display blocks: %q", out)
	}
	got, _ := json.Marshal(cfg.Moments.Display)
	src, err := os.ReadFile(filepath.Join("..", "..", "public", "live-moments.js"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`DEFAULTS: (\{[^}]*\})`).FindSubmatch(src)
	if m == nil {
		t.Fatal("no SETTINGS.DEFAULTS in live-moments.js")
	}
	js := regexp.MustCompile(`(\w+):`).ReplaceAll(m[1], []byte(`"$1":`))
	var want, have map[string]json.RawMessage
	if err := json.Unmarshal(js, &want); err != nil {
		t.Fatalf("DEFAULTS %s: %v", js, err)
	}
	if err := json.Unmarshal(got, &have); err != nil {
		t.Fatal(err)
	}
	if len(want) != len(have) {
		t.Fatalf("example display %s, client defaults %s", got, js)
	}
	for k, v := range want {
		if string(have[k]) != string(v) {
			t.Fatalf("example display %s, client defaults %s", got, js)
		}
	}
	if ts := cfg.GetTimestampConfig(); ts != defaultTimestampConfig() {
		t.Fatalf("example timestamps %+v, built-in %+v", ts, defaultTimestampConfig())
	}
}
