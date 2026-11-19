package antennaclaim

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// configTags lists the JSON names of Config, so a new key cannot ship
// without its example and its documentation (#898 M3, rule 8).
func configTags(t *testing.T) []string {
	t.Helper()
	var tags []string
	typ := reflect.TypeOf(Config{})
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			t.Fatalf("Config.%s has no json name", typ.Field(i).Name)
		}
		tags = append(tags, name)
	}
	return tags
}

func TestConfig898_ExampleAndGuideListEveryKey(t *testing.T) {
	raw, err := os.ReadFile("../../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		AntennaClaims map[string]json.RawMessage `json:"antennaClaims"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	guide, err := os.ReadFile("../../../docs/user-guide/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range configTags(t) {
		if _, ok := doc.AntennaClaims[k]; !ok {
			t.Errorf("config.example.json antennaClaims has no %q", k)
		}
		if !strings.Contains(string(guide), "| `"+k+"` |") {
			t.Errorf("docs/user-guide/configuration.md has no row for %q", k)
		}
	}
	for k := range doc.AntennaClaims {
		found := false
		for _, g := range configTags(t) {
			found = found || g == k
		}
		if !found {
			t.Errorf("config.example.json antennaClaims.%s is not a Config key", k)
		}
	}
}

// The example is what operators copy: it must not switch anything on, and
// decoding it must leave details off (default behaviour unchanged).
func TestConfig898_ExampleLeavesDetailsOff(t *testing.T) {
	raw, err := os.ReadFile("../../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		AntennaClaims Config `json:"antennaClaims"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.AntennaClaims.DetailsRequested() || doc.AntennaClaims.SubmissionsRequested() || doc.AntennaClaims.AutoApprove() {
		t.Errorf("config.example.json switches a gate on: %+v", doc.AntennaClaims)
	}
}
