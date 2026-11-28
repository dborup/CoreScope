package account

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// Every accounts.* key ships with its example and its row in the owner
// accounts section of the configuration guide (#1018 M1, rule 8), and the
// example switches nothing on.
func TestConfig1018_ExampleAndGuideListEveryKey(t *testing.T) {
	var tags []string
	typ := reflect.TypeOf(Config{})
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		tags = append(tags, name)
	}
	raw, err := os.ReadFile("../../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Accounts map[string]json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	guide, err := os.ReadFile("../../../docs/user-guide/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(guide), "\n## Owner accounts\n")
	if !ok {
		t.Fatal("configuration.md has no Owner accounts section")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	for _, k := range tags {
		if _, ok := doc.Accounts[k]; !ok {
			t.Errorf("config.example.json accounts has no %q", k)
		}
		if !strings.Contains(section, "| `"+k+"` |") {
			t.Errorf("configuration.md Owner accounts has no row for %q", k)
		}
	}
	if len(doc.Accounts) != len(tags) {
		t.Errorf("config.example.json accounts has %d keys, Config %d", len(doc.Accounts), len(tags))
	}
	var parsed struct {
		Accounts Config `json:"accounts"`
	}
	json.Unmarshal(raw, &parsed)
	if parsed.Accounts.Requested() {
		t.Error("config.example.json switches accounts on")
	}
	if l := parsed.Accounts.Limits(); l != (*Config)(nil).Limits() {
		t.Errorf("the example's values are not the defaults: %+v", l)
	}
}
