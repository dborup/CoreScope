package channelregistry

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeNameAcceptsAndPreservesCase(t *testing.T) {
	cases := map[string]string{
		"#MeshCore":       "#MeshCore",
		"MeshCore":        "#MeshCore", // missing '#' is added, like hashChannels
		"  #wardriving  ": "#wardriving",
		"#Test":           "#Test", // case preserved: #Test and #test are different channels
		"#test":           "#test",
		"#København":      "#København", // non-ASCII is allowed; the firmware has no charset
		"#my channel":     "#my channel",
		"##double":        "##double",
		"#\U0001f468\u200d\U0001f469\u200d\U0001f467": "#\U0001f468\u200d\U0001f469\u200d\U0001f467", // zero-width joiners in emoji stay allowed
		"#" + strings.Repeat("a", 30):                 "#" + strings.Repeat("a", 30),                 // exactly 31 bytes
	}
	for in, want := range cases {
		got, err := NormalizeName(in)
		if err != nil {
			t.Errorf("NormalizeName(%q) error %v, want %q", in, err, want)
			continue
		}
		if got != want {
			t.Errorf("NormalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeNameRejects(t *testing.T) {
	cases := []struct {
		in   string
		want error
	}{
		{"", ErrNameEmpty},
		{"   ", ErrNameEmpty},
		{"#", ErrNameEmpty},
		{"#" + strings.Repeat("a", 31), ErrNameTooLong},       // 32 bytes: the firmware would truncate it
		{"#" + strings.Repeat("æ", 16), ErrNameTooLong},       // 16 runes but 33 bytes: the limit is bytes
		{"#0123456789abcdef0123456789abcdef", ErrNameTooLong}, // a pasted PSK is not a hashtag name
		{"#bad\x00name", ErrNameControl},
		{"#tab\tname", ErrNameControl},
		{"#rtl\u202eevil", ErrNameControl},
		{"#iso\u2066late", ErrNameControl},
		{"# leading", ErrNameSpace},
		{"#\u00a0leading-nbsp", ErrNameSpace}, // trailing whitespace is trimmed; a leading one after # is not
		{"#\xff\xfe", ErrNameInvalidUTF8},
	}
	for _, c := range cases {
		got, err := NormalizeName(c.in)
		if !errors.Is(err, c.want) {
			t.Errorf("NormalizeName(%q) = %q, %v; want error %v", c.in, got, err, c.want)
		}
	}
}

func TestConfigLimitsDefaultsAndOverrides(t *testing.T) {
	var nilCfg *Config
	l := nilCfg.Limits()
	if l.MaxPending != 100 || l.MaxApproved != 128 || l.MaxQueuedRequests != 256 ||
		l.RetentionDays != 30 || l.SubmissionsPerHour != 20 || l.RequestTTL.Hours() != 24 {
		t.Fatalf("defaults = %+v", l)
	}
	if l.MaxResults != 256*maxResultFactor {
		t.Fatalf("MaxResults = %d", l.MaxResults)
	}
	if nilCfg.SubmissionsRequested() {
		t.Fatal("nil config must not request submissions")
	}
	on := true
	c := &Config{Enabled: &on, MaxPending: 5, MaxApproved: -1, SubmissionsPerHour: 3}
	l = c.Limits()
	if l.MaxPending != 5 || l.MaxApproved != 128 || l.SubmissionsPerHour != 3 {
		t.Fatalf("overrides = %+v", l)
	}
	if !c.SubmissionsRequested() {
		t.Fatal("enabled config must request submissions")
	}
}
