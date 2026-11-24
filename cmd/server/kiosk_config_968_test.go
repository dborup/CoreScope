package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/meshcore-analyzer/configkey"
)

// #968 M1 — the operator kiosk preset: config.json "kiosk" → the `kiosk`
// block of /api/config/client. Every member is read by its canonical,
// exact-case key (#884); a value outside its bounds logs one warning and
// takes the default; a missing block is not served at all.

// kioskClientFor loads cfgJSON and returns the served `kiosk` block (nil
// when absent) and the load log.
func kioskClientFor(t *testing.T, cfgJSON string) (map[string]interface{}, bool, string) {
	t.Helper()
	dir := t.TempDir()
	writeConfigFile(t, dir, "config.json", cfgJSON)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	cfg, err := LoadConfig(dir)
	log.SetOutput(os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	logs := buf.String()
	srv, router := setupTestServer(t)
	srv.cfg = cfg
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/config/client", nil))
	if w.Code != 200 {
		t.Fatalf("/api/config/client: %d %s", w.Code, w.Body.String())
	}
	var got map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	k, present := got["kiosk"]
	if !present {
		return nil, false, logs
	}
	m, _ := k.(map[string]interface{})
	return m, true, logs
}

// kioskWarnings counts this feature's own bounds warnings.
func kioskWarnings(logs string) int {
	n := 0
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, "[config] kiosk.") && strings.Contains(l, "using the default") {
			n++
		}
	}
	return n
}

func kioskDefaults() map[string]interface{} {
	return map[string]interface{}{
		"tour":         []interface{}{},
		"everySeconds": float64(60),
		"theme":        "dark",
		"tiles":        []interface{}{"rate", "active", "records"},
		"remountHours": float64(6),
	}
}

func kioskWith(over map[string]interface{}) map[string]interface{} {
	m := kioskDefaults()
	for k, v := range over {
		m[k] = v
	}
	return m
}

func TestKioskPresetClientBlock968(t *testing.T) {
	cases := []struct {
		name  string
		json  string
		want  map[string]interface{} // nil: no block served
		warns int
	}{
		{"absent: not served", `{}`, nil, 0},
		{"empty block: the built-in defaults", `{"kiosk": {}}`, kioskDefaults(), 0},
		{"a full valid preset", `{"kiosk": {"tour": ["AAA", "bbb"], "everySeconds": 45, "theme": "light", "tiles": ["records", "rate"], "remountHours": 12}}`,
			kioskWith(map[string]interface{}{"tour": []interface{}{"AAA", "bbb"}, "everySeconds": float64(45), "theme": "light",
				"tiles": []interface{}{"rate", "records"}, "remountHours": float64(12)}), 0},
		// everySeconds: integer 15-3600, the URL's clamp.
		{"everySeconds 15 (lower edge)", `{"kiosk": {"everySeconds": 15}}`, kioskWith(map[string]interface{}{"everySeconds": float64(15)}), 0},
		{"everySeconds 3600 (upper edge)", `{"kiosk": {"everySeconds": 3600}}`, kioskWith(map[string]interface{}{"everySeconds": float64(3600)}), 0},
		{"everySeconds 14", `{"kiosk": {"everySeconds": 14}}`, kioskDefaults(), 1},
		{"everySeconds 3601", `{"kiosk": {"everySeconds": 3601}}`, kioskDefaults(), 1},
		{"everySeconds 0", `{"kiosk": {"everySeconds": 0}}`, kioskDefaults(), 1},
		{"everySeconds a fraction", `{"kiosk": {"everySeconds": 30.5}}`, kioskDefaults(), 1},
		{"everySeconds a string", `{"kiosk": {"everySeconds": "30"}}`, kioskDefaults(), 1},
		// remountHours: 1-24, the client's remount clamp.
		{"remountHours 1 (lower edge)", `{"kiosk": {"remountHours": 1}}`, kioskWith(map[string]interface{}{"remountHours": float64(1)}), 0},
		{"remountHours 24 (upper edge)", `{"kiosk": {"remountHours": 24}}`, kioskWith(map[string]interface{}{"remountHours": float64(24)}), 0},
		{"remountHours 1.5", `{"kiosk": {"remountHours": 1.5}}`, kioskWith(map[string]interface{}{"remountHours": 1.5}), 0},
		{"remountHours 0.5", `{"kiosk": {"remountHours": 0.5}}`, kioskDefaults(), 1},
		{"remountHours 25", `{"kiosk": {"remountHours": 25}}`, kioskDefaults(), 1},
		// theme: dark, light or auto, exact.
		{"theme auto", `{"kiosk": {"theme": "auto"}}`, kioskWith(map[string]interface{}{"theme": "auto"}), 0},
		{"theme unknown", `{"kiosk": {"theme": "blue"}}`, kioskDefaults(), 1},
		{"theme in another case", `{"kiosk": {"theme": "Light"}}`, kioskDefaults(), 1},
		// tiles: a subset, canonical order, unknown dropped; none left → all.
		{"tiles: unknown and duplicate entries dropped", `{"kiosk": {"tiles": ["active", "bogus", "rate", "active"]}}`,
			kioskWith(map[string]interface{}{"tiles": []interface{}{"rate", "active"}}), 1},
		{"tiles: empty list is all", `{"kiosk": {"tiles": []}}`, kioskDefaults(), 0},
		{"tiles: only unknown is all", `{"kiosk": {"tiles": ["x"]}}`, kioskDefaults(), 1},
		{"tiles: not a list", `{"kiosk": {"tiles": "rate"}}`, kioskDefaults(), 1},
		// tour: area keys, trimmed, at most 12.
		{"tour: trimmed, empty keys dropped", `{"kiosk": {"tour": [" a ", "", "b"]}}`, kioskWith(map[string]interface{}{"tour": []interface{}{"a", "b"}}), 0},
		{"tour: 13 keys cut to 12", `{"kiosk": {"tour": ["1","2","3","4","5","6","7","8","9","10","11","12","13"]}}`,
			kioskWith(map[string]interface{}{"tour": []interface{}{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12"}}), 1},
		{"tour: not a list of strings", `{"kiosk": {"tour": "a,b"}}`, kioskDefaults(), 1},
		// Canonical keys only (#884).
		{"a case-variant block is not the kiosk block", `{"Kiosk": {"everySeconds": 30}}`, nil, 0},
		{"a case-variant member is ignored", `{"kiosk": {"EverySeconds": 30, "THEME": "light"}}`, kioskDefaults(), 0},
		{"the canonical member wins over a variant after it", `{"kiosk": {"everySeconds": 30, "EverySeconds": 90}}`,
			kioskWith(map[string]interface{}{"everySeconds": float64(30)}), 0},
		{"not an object", `{"kiosk": 5}`, nil, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, present, logs := kioskClientFor(t, c.json)
			if c.want == nil {
				if present {
					t.Fatalf("kiosk served: %v", got)
				}
			} else if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("kiosk = %v, want %v", got, c.want)
			}
			if n := kioskWarnings(logs); n != c.warns {
				t.Fatalf("want %d kiosk warning(s), got %d:\n%s", c.warns, n, logs)
			}
		})
	}
}

// A case-variant key logs the #884 ignored-key warning, once.
func TestKioskPresetCanonicalWarning968(t *testing.T) {
	_, _, logs := kioskClientFor(t, `{"kiosk": {"EverySeconds": 30}}`)
	if n := strings.Count(logs, configkey.IgnoredNote); n != 1 {
		t.Fatalf("want one ignored-key warning, got %d:\n%s", n, logs)
	}
	if !strings.Contains(logs, "kiosk.EverySeconds") {
		t.Fatalf("the warning names the key:\n%s", logs)
	}
}

// The response field is a named struct pointer (no map[string]interface{}),
// omitted when unset.
func TestKioskPresetResponseShape968(t *testing.T) {
	f, ok := reflect.TypeOf(ClientConfigResponse{}).FieldByName("Kiosk")
	if !ok {
		t.Fatal("ClientConfigResponse has no Kiosk field")
	}
	if f.Type.Kind() != reflect.Ptr || f.Type.Elem().Kind() != reflect.Struct || f.Type.Elem().Name() == "" {
		t.Fatalf("ClientConfigResponse.Kiosk must be a pointer to a named struct, is %v", f.Type)
	}
	if tag := f.Tag.Get("json"); tag != "kiosk,omitempty" {
		t.Fatalf("json tag = %q", tag)
	}
	for i := 0; i < f.Type.Elem().NumField(); i++ {
		if ft := f.Type.Elem().Field(i).Type; ft.Kind() == reflect.Map || ft.Kind() == reflect.Interface {
			t.Fatalf("%s.%s is %v", f.Type.Elem().Name(), f.Type.Elem().Field(i).Name, ft)
		}
	}
}
