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

// Invisible formatting characters (Unicode category Cf) and the line/paragraph
// separators make two different channels look identical or break the layout,
// so they are rejected. This is our own presentation rule: the firmware only
// limits the length. ZWJ and variation selectors stay allowed for emoji.
func TestNormalizeNameRejectsInvisibleFormatCharacters(t *testing.T) {
	cases := []struct {
		name string
		r    string
		want error
	}{
		{"soft hyphen U+00AD", "\u00ad", ErrNameInvisible},
		{"Arabic number sign U+0600", "\u0600", ErrNameInvisible},
		{"Mongolian vowel separator U+180E", "\u180e", ErrNameInvisible},
		{"zero width space U+200B", "\u200b", ErrNameInvisible},
		{"zero width non-joiner U+200C", "\u200c", ErrNameInvisible},
		{"word joiner U+2060", "\u2060", ErrNameInvisible},
		{"invisible times U+2062", "\u2062", ErrNameInvisible},
		{"BOM / ZWNBSP U+FEFF", "\ufeff", ErrNameInvisible},
		{"interlinear annotation U+FFF9", "\ufff9", ErrNameInvisible},
		{"language tag U+E0001", "\U000e0001", ErrNameInvisible},
		{"tag space U+E0020", "\U000e0020", ErrNameInvisible},
		{"tag latin small a U+E0061", "\U000e0061", ErrNameInvisible},
		{"cancel tag U+E007F", "\U000e007f", ErrNameInvisible},
		{"line separator U+2028", "\u2028", ErrNameControl},
		{"paragraph separator U+2029", "\u2029", ErrNameControl},
		// Bidi controls are Cf too; they keep their more specific error.
		{"right-to-left override U+202E", "\u202e", ErrNameControl},
		{"left-to-right mark U+200E", "\u200e", ErrNameControl},
	}
	for _, c := range cases {
		inputs := []string{"#a" + c.r + "b", "#" + c.r + "ab", "#ab" + c.r}
		if c.r == "\u2028" || c.r == "\u2029" {
			// Trailing separators are whitespace and are trimmed like any
			// other trailing whitespace (hashChannels does the same).
			inputs = inputs[:2]
		}
		for _, in := range inputs {
			got, err := NormalizeName(in)
			if !errors.Is(err, c.want) {
				t.Errorf("%s: NormalizeName(%q) = %q, %v; want %v", c.name, in, got, err, c.want)
			}
		}
	}
}

func TestNormalizeNameTrimsTrailingLineSeparator(t *testing.T) {
	if got, err := NormalizeName("#ab\u2028"); err != nil || got != "#ab" {
		t.Fatalf("NormalizeName(trailing U+2028) = %q, %v; want \"#ab\"", got, err)
	}
}

func TestNormalizeNameAllowsEmojiJoinersAndVariationSelectors(t *testing.T) {
	for _, in := range []string{
		"#\U0001f3f3\ufe0f\u200d\U0001f308",       // rainbow flag: VS16 + ZWJ
		"#\u2764\ufe0f",                           // red heart: VS16
		"#\u2764\ufe0e",                           // text-style heart: VS15
		"#\U0001f469\u200d\U0001f4bb",             // woman technologist: ZWJ
		"#a\ufe00b",                               // VS1
		"#\u845b\U000e0100",                       // ideographic variation selector VS17
		"#\U0001f441\ufe0f\u200d\U0001f5e8\ufe0f", // eye in speech bubble
		"#mesh\u200dcore",                         // a bare ZWJ between letters is still allowed
	} {
		got, err := NormalizeName(in)
		if err != nil || got != in {
			t.Errorf("NormalizeName(%q) = %q, %v; want it unchanged", in, got, err)
		}
	}
}
