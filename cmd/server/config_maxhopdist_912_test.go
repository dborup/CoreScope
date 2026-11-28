package main

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"reflect"
	"strings"
	"testing"
)

// #912 M3 — maxHopDist (degrees) reaches the client as a typed number, and a
// value the client could not use is replaced by "unset" (the client default,
// 1.8) with one warning, not shipped.

func loadMaxHopDist(t *testing.T, cfgJSON string) (*float64, string) {
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
	return cfg.MaxHopDist, buf.String()
}

func TestMaxHopDistValidation912(t *testing.T) {
	cases := []struct {
		name string
		json string
		want *float64
		warn bool
	}{
		{"absent", `{}`, nil, false},
		{"low", `{"maxHopDist": 0.3}`, mhdPtr(0.3), false},
		{"default value", `{"maxHopDist": 1.8}`, mhdPtr(1.8), false},
		{"upper bound", `{"maxHopDist": 10}`, mhdPtr(10), false},
		{"zero", `{"maxHopDist": 0}`, nil, true},
		{"negative", `{"maxHopDist": -2}`, nil, true},
		{"absurd", `{"maxHopDist": 10.5}`, nil, true},
		{"huge", `{"maxHopDist": 99999}`, nil, true},
		{"case variant is not the canonical key", `{"MaxHopDist": 0.3}`, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, logs := loadMaxHopDist(t, c.json)
			if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
				t.Fatalf("MaxHopDist = %v, want %v", deref(got), deref(c.want))
			}
			if n := strings.Count(logs, "maxHopDist"); c.warn && n != 1 {
				t.Fatalf("want exactly one maxHopDist warning, got %d:\n%s", n, logs)
			} else if !c.warn && strings.Contains(logs, "out of range") {
				t.Fatalf("unexpected out-of-range warning:\n%s", logs)
			}
		})
	}
}

func TestClientConfigMaxHopDistShape912(t *testing.T) {
	f, ok := reflect.TypeOf(ClientConfigResponse{}).FieldByName("MaxHopDist")
	if !ok || f.Type != reflect.TypeOf((*float64)(nil)) {
		t.Fatalf("ClientConfigResponse.MaxHopDist must be *float64, is %v", f.Type)
	}
	for _, c := range []struct {
		cfg  string
		want interface{}
	}{{`{"maxHopDist": 0.3}`, 0.3}, {`{}`, nil}, {`{"maxHopDist": -1}`, nil}} {
		body, _ := clientConfigFor(t, c.cfg)
		var got map[string]interface{}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		v, present := got["maxHopDist"]
		if !present || v != c.want {
			t.Fatalf("cfg %s: maxHopDist = %v (present=%v), want %v", c.cfg, v, present, c.want)
		}
	}
}

func deref(p *float64) interface{} {
	if p == nil {
		return nil
	}
	return *p
}

func mhdPtr(v float64) *float64 { return &v }
