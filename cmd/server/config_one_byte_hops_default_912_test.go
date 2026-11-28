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

// #912 M2 — customizer.oneByteHopsDefault: the operator's default for the
// 1-byte hop mode (show / mark / hide). It is read from the exact-case key
// only (#884); an unknown value logs one warning and falls back to mark, and
// an absent key behaves as before (mark). /api/config/client always serves
// the effective value in its customizer block.

func loadOneByteHopsDefault(t *testing.T, cfgJSON string) (*Config, string) {
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
	return cfg, buf.String()
}

func TestOneByteHopsDefaultValidation912(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
		warn bool
	}{
		{"absent block", `{}`, "mark", false},
		{"absent key", `{"customizer": {"disabledTabs": ["export"]}}`, "mark", false},
		{"show", `{"customizer": {"oneByteHopsDefault": "show"}}`, "show", false},
		{"mark", `{"customizer": {"oneByteHopsDefault": "mark"}}`, "mark", false},
		{"hide", `{"customizer": {"oneByteHopsDefault": "hide"}}`, "hide", false},
		{"unknown", `{"customizer": {"oneByteHopsDefault": "bogus"}}`, "mark", true},
		{"wrong case", `{"customizer": {"oneByteHopsDefault": "Hide"}}`, "mark", true},
		{"empty", `{"customizer": {"oneByteHopsDefault": ""}}`, "mark", true},
		{"proto-ish", `{"customizer": {"oneByteHopsDefault": "constructor"}}`, "mark", true},
		{"not a string", `{"customizer": {"oneByteHopsDefault": 2}}`, "mark", true},
		{"null", `{"customizer": {"oneByteHopsDefault": null}}`, "mark", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, logs := loadOneByteHopsDefault(t, c.json)
			if got := cfg.OneByteHopsDefault(); got != c.want {
				t.Fatalf("OneByteHopsDefault() = %q, want %q", got, c.want)
			}
			n := strings.Count(logs, "customizer.oneByteHopsDefault")
			if c.warn && n != 1 {
				t.Fatalf("want exactly one oneByteHopsDefault warning, got %d:\n%s", n, logs)
			}
			if !c.warn && n != 0 {
				t.Fatalf("unexpected oneByteHopsDefault warning:\n%s", logs)
			}
		})
	}
}

// A non-string value must not reject the whole config file: the field is
// read through configkey only, never by the struct decode.
func TestOneByteHopsDefaultBadTypeKeepsTheFile912(t *testing.T) {
	cfg, _ := loadOneByteHopsDefault(t, `{"port": 4242, "customizer": {"oneByteHopsDefault": 2, "disabledTabs": ["export"]}}`)
	if cfg.Port != 4242 {
		t.Fatalf("config file was dropped: port = %d", cfg.Port)
	}
	if cfg.Customizer == nil || !reflect.DeepEqual(cfg.Customizer.DisabledTabs, []string{"export"}) {
		t.Fatalf("disabledTabs lost: %+v", cfg.Customizer)
	}
}

// #884: a case variant is not the canonical key, so it is ignored (with the
// configkey warning) and the default stays mark.
func TestOneByteHopsDefaultCanonicalKeyOnly912(t *testing.T) {
	for _, j := range []string{
		`{"customizer": {"OneByteHopsDefault": "hide"}}`,
		`{"customizer": {"onebytehopsdefault": "hide"}}`,
		`{"Customizer": {"oneByteHopsDefault": "hide"}}`,
	} {
		cfg, _ := loadOneByteHopsDefault(t, j)
		if got := cfg.OneByteHopsDefault(); got != "mark" {
			t.Fatalf("%s: OneByteHopsDefault() = %q, want mark", j, got)
		}
	}
	// The exact key still wins next to a variant.
	cfg, _ := loadOneByteHopsDefault(t, `{"customizer": {"oneByteHopsDefault": "show", "OneByteHopsDefault": "hide"}}`)
	if got := cfg.OneByteHopsDefault(); got != "show" {
		t.Fatalf("exact key next to a variant: got %q, want show", got)
	}
}

func TestOneByteHopsDefaultIsANamedField912(t *testing.T) {
	f, ok := reflect.TypeOf(CustomizerConfig{}).FieldByName("OneByteHopsDefault")
	if !ok || f.Type.Kind() != reflect.String {
		t.Fatalf("CustomizerConfig.OneByteHopsDefault must be a string field, got %v (found=%v)", f.Type, ok)
	}
	f, ok = reflect.TypeOf(CustomizerClientConfig{}).FieldByName("OneByteHopsDefault")
	if !ok || f.Type.Kind() != reflect.String || f.Tag.Get("json") != "oneByteHopsDefault" {
		t.Fatalf("CustomizerClientConfig.OneByteHopsDefault must be a string tagged oneByteHopsDefault, got %v %q", f.Type, f.Tag)
	}
}

func TestClientConfigOneByteHopsDefaultShape912(t *testing.T) {
	for _, c := range []struct {
		cfg  string
		want string
	}{
		{`{}`, "mark"},
		{`{"customizer": {"oneByteHopsDefault": "hide"}}`, "hide"},
		{`{"customizer": {"oneByteHopsDefault": "show", "disabledTabs": ["export"]}}`, "show"},
		{`{"customizer": {"oneByteHopsDefault": "nope"}}`, "mark"},
	} {
		body, _ := clientConfigFor(t, c.cfg)
		var got struct {
			Customizer map[string]json.RawMessage `json:"customizer"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		raw, ok := got.Customizer["oneByteHopsDefault"]
		if !ok {
			t.Fatalf("cfg %s: customizer.oneByteHopsDefault missing from %s", c.cfg, body)
		}
		var v string
		if err := json.Unmarshal(raw, &v); err != nil || v != c.want {
			t.Fatalf("cfg %s: customizer.oneByteHopsDefault = %s, want %q", c.cfg, raw, c.want)
		}
		if _, ok := got.Customizer["disabledTabs"]; !ok {
			t.Fatalf("cfg %s: customizer.disabledTabs must stay in the block", c.cfg)
		}
	}
}
