package configkey

import (
	"fmt"
	"strings"
	"testing"
)

// logged collects the warnings a Doc reports.
type logged []string

func (l *logged) warn(format string, args ...interface{}) {
	*l = append(*l, fmt.Sprintf(format, args...))
}

func TestOnReadsOnlyTheExactKeyAndTheJSONTrue(t *testing.T) {
	cases := []struct {
		name, doc string
		want      bool
		warnings  int
	}{
		{"exact true", `{"a":{"enabled":true}}`, true, 0},
		{"exact false", `{"a":{"enabled":false}}`, false, 0},
		{"missing", `{"a":{}}`, false, 0},
		{"string true", `{"a":{"enabled":"true"}}`, false, 0},
		{"number 1", `{"a":{"enabled":1}}`, false, 0},
		{"null", `{"a":{"enabled":null}}`, false, 0},
		{"leaf case variant alone", `{"a":{"Enabled":true}}`, false, 1},
		{"leaf case variant after exact false", `{"a":{"enabled":false,"Enabled":true}}`, false, 1},
		{"leaf case variant before exact false", `{"a":{"ENABLED":true,"enabled":false}}`, false, 1},
		{"two leaf case variants", `{"a":{"Enabled":true,"ENABLED":true}}`, false, 2},
		{"exact true next to a variant", `{"a":{"enabled":true,"Enabled":false}}`, true, 1},
		{"parent case variant", `{"A":{"enabled":true}}`, false, 1},
		{"parent case variant next to exact off", `{"a":{"enabled":false},"A":{"enabled":true}}`, false, 1},
		{"parent not an object", `{"a":true}`, false, 0},
		{"document not an object", `[1]`, false, 0},
		{"malformed document", `{`, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var l logged
			d := New([]byte(tc.doc), l.warn)
			if got := d.On("a", "enabled"); got != tc.want {
				t.Fatalf("On = %v, want %v", got, tc.want)
			}
			if len(l) != tc.warnings {
				t.Fatalf("warnings = %d (%q), want %d", len(l), l, tc.warnings)
			}
		})
	}
}

func TestEachIgnoredKeyIsReportedOnce(t *testing.T) {
	var l logged
	d := New([]byte(`{"Share":{"nodes":{"enabled":true},"packets":{"enabled":true}}}`), l.warn)
	d.On("share", "nodes", "enabled")
	d.On("share", "packets", "enabled")
	d.Get("share")
	if len(l) != 1 {
		t.Fatalf("warnings = %q, want exactly one", l)
	}
	if !strings.Contains(l[0], "Share") || !strings.Contains(l[0], "share") {
		t.Fatalf("warning %q must name the ignored key and the canonical one", l[0])
	}
}

func TestWarningNamesTheFullPathNeverTheValue(t *testing.T) {
	var l logged
	d := New([]byte(`{"map":{"tiles":{"providers":{"dataforsyningen":{"Token":"dummy-value-884"}}}}}`), l.warn)
	d.Get("map", "tiles", "providers", "dataforsyningen", "token")
	if len(l) != 1 {
		t.Fatalf("warnings = %q, want one", l)
	}
	if !strings.Contains(l[0], "map.tiles.providers.dataforsyningen.Token") {
		t.Fatalf("warning %q must name the full path of the ignored key", l[0])
	}
	if strings.Contains(l[0], "dummy-value-884") {
		t.Fatalf("warning %q must not contain the value", l[0])
	}
}

func TestGetReturnsTheExactMember(t *testing.T) {
	var l logged
	d := New([]byte(`{"map":{"x":1},"Map":{"x":2}}`), l.warn)
	if got := strings.TrimSpace(string(d.Get("map"))); got != `{"x":1}` {
		t.Fatalf("Get(map) = %s", got)
	}
	if d.Get("missing") != nil {
		t.Fatal("Get of a missing key must be nil")
	}
	if len(l) != 1 {
		t.Fatalf("warnings = %q, want one for Map", l)
	}
}

func TestNilWarnIsAllowed(t *testing.T) {
	if New([]byte(`{"A":true}`), nil).On("a") {
		t.Fatal("a case variant must not switch a gate on")
	}
}
