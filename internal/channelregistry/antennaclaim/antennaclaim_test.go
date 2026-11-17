package antennaclaim

import (
	"math"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNewCode789_AlphabetAndLength(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		c, err := NewCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(c) != CodeLen || !ValidCode(c) {
			t.Fatalf("bad code %q", c)
		}
		for _, bad := range "ILOU" {
			if strings.ContainsRune(c, bad) {
				t.Fatalf("code %q has ambiguous letter %q", c, bad)
			}
		}
		seen[c] = true
	}
	// 30 bits: 200 draws must not repeat (a constant or low-entropy source would).
	if len(seen) < 199 {
		t.Fatalf("codes repeat: %d distinct of 200", len(seen))
	}
}

func TestValidCode789(t *testing.T) {
	for _, ok := range []string{"0123AB", "ZZZZZZ", "7KX9QM"} {
		if !ValidCode(ok) {
			t.Errorf("ValidCode(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "ABCDE", "ABCDEFG", "abcdef", "ABCDEI", "ABCDEL", "ABCDEO", "ABCDEU", "ABC DE", "ABC-DE"} {
		if ValidCode(bad) {
			t.Errorf("ValidCode(%q) = true", bad)
		}
	}
}

func TestValidHeight789_Bounds(t *testing.T) {
	for _, h := range []float64{0, 0.1, 12.5, 99.9, 100} {
		if !ValidHeight(h) {
			t.Errorf("ValidHeight(%v) = false", h)
		}
	}
	for _, h := range []float64{-0.1, 100.1, 150, 12.55, 0.05, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if ValidHeight(h) {
			t.Errorf("ValidHeight(%v) = true", h)
		}
	}
}

func TestValidPubKey789(t *testing.T) {
	good := strings.Repeat("ab", 32)
	if !ValidPubKey(good) {
		t.Fatal("64 lowercase hex rejected")
	}
	for _, bad := range []string{"", good[:62], good + "ab", strings.ToUpper(good), strings.Repeat("zz", 32)} {
		if ValidPubKey(bad) {
			t.Errorf("ValidPubKey(%q) = true", bad)
		}
	}
}

func TestNameBudget789_Firmware(t *testing.T) {
	// firmware AdvertDataHelpers.cpp:21: MAX_ADVERT_DATA_SIZE(32) - offset.
	cases := []struct {
		loc, f1, f2 bool
		want        int
	}{
		{false, false, false, 31},
		{true, false, false, 23},
		{true, true, false, 21},
		{true, true, true, 19},
		{false, false, true, 29},
	}
	for _, c := range cases {
		if got := NameBudget(c.loc, c.f1, c.f2); got != c.want {
			t.Errorf("NameBudget(%v,%v,%v) = %d, want %d", c.loc, c.f1, c.f2, got, c.want)
		}
	}
}

func TestContainsCode789_WholeTokenInBudget(t *testing.T) {
	const code = "7KX9QM"
	cases := []struct {
		name   string
		budget int
		want   bool
	}{
		{"Roof Rptr 7KX9QM", 23, true},
		{"7KX9QM", 23, true},
		{"7KX9QM Roof", 23, true},
		{"Roof  7KX9QM ", 23, true},
		{"Roof Rptr x7KX9QM", 23, false},         // substring, not a token
		{"Roof Rptr 7KX9QMx", 23, false},         // substring, not a token
		{"Roof-7KX9QM", 23, false},               // only spaces separate tokens
		{"Roof Rptr 7kx9qm", 23, false},          // exact code only
		{"Roof Repeater Abcd 7KX9QM", 23, false}, // code ends past byte 23: cut by firmware
		{"Roof Repeater A 7KX9QM", 23, true},     // ends exactly at byte 22
		{"Roof Repeater AB 7KX9QM", 23, true},    // ends exactly at byte 23
		{"Roof Repeater ABC 7KX9QM", 23, false},
		{"Roof Repeater ABC 7KX9QM", 31, true},
		{"", 23, false},
	}
	for _, c := range cases {
		if got := ContainsCode(c.name, c.budget, code); got != c.want {
			t.Errorf("ContainsCode(%q, %d) = %v, want %v", c.name, c.budget, got, c.want)
		}
	}
	if ContainsCode("Roof 7KX9QM", 23, "") {
		t.Error("empty code must never match")
	}
}

func TestSuggestedName789_FitsBudget(t *testing.T) {
	const code = "7KX9QM"
	cases := []struct{ cur, want string }{
		{"Roof", "Roof 7KX9QM"},
		{"", "7KX9QM"},
		{"A very long repeater name", "A very long repe 7KX9QM"},
		{"Æblegård Øster Ålborg", "Æblegård Øste 7KX9QM"},
		{"Trailing   ", "Trailing 7KX9QM"},
	}
	for _, c := range cases {
		got := SuggestedName(c.cur, code, NameBudgetWithLocation)
		if got != c.want {
			t.Errorf("SuggestedName(%q) = %q, want %q", c.cur, got, c.want)
		}
		if len(got) > NameBudgetWithLocation || !utf8.ValidString(got) {
			t.Errorf("SuggestedName(%q) = %q: %d bytes or invalid UTF-8", c.cur, got, len(got))
		}
		if !ContainsCode(got, NameBudgetWithLocation, code) {
			t.Errorf("SuggestedName(%q) = %q does not prove", c.cur, got)
		}
	}
}

func TestNeedsReview789(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		h    float64
		s    *float64
		want bool
	}{
		{30, nil, false},    // nothing known: not flagged
		{20, f(18), false},  // on the roof
		{80, f(18), false},  // 60 m above surface+2 exactly
		{80.1, f(18), true}, // more than 60 m above
		{10, f(30), true},   // well below the roof
		{28, f(30), false},  // within 2 m below
		{27.9, f(30), true},
	}
	for _, c := range cases {
		if got := NeedsReview(c.h, c.s); got != c.want {
			t.Errorf("NeedsReview(%v, %v) = %v, want %v", c.h, c.s, got, c.want)
		}
	}
}

func TestCommandValidate789(t *testing.T) {
	h := 12.5
	bad := 150.0
	id := "0123456789abcdef"
	pk := strings.Repeat("ab", 32)
	good := []Command{
		{RequestID: id, Op: OpClaim, PubKey: pk, HeightM: &h, Code: "7KX9QM", CreatedAt: 1},
		{RequestID: id, Op: OpClaim, PubKey: pk, Clear: true, Code: "7KX9QM", CreatedAt: 1},
		{RequestID: id, Op: OpApprove, ClaimID: id, CreatedAt: 1},
		{RequestID: id, Op: OpReject, ClaimID: id, CreatedAt: 1},
		{RequestID: id, Op: OpRevoke, ClaimID: id, CreatedAt: 1},
	}
	for i, c := range good {
		if err := c.Validate(); err != nil {
			t.Errorf("good[%d]: %v", i, err)
		}
	}
	badCmds := []Command{
		{RequestID: "x", Op: OpClaim, PubKey: pk, HeightM: &h, Code: "7KX9QM"},
		{RequestID: id, Op: "nope"},
		{RequestID: id, Op: OpClaim, PubKey: "zz", HeightM: &h, Code: "7KX9QM"},
		{RequestID: id, Op: OpClaim, PubKey: pk, Code: "7KX9QM"},                           // neither
		{RequestID: id, Op: OpClaim, PubKey: pk, HeightM: &h, Clear: true, Code: "7KX9QM"}, // both
		{RequestID: id, Op: OpClaim, PubKey: pk, HeightM: &bad, Code: "7KX9QM"},
		{RequestID: id, Op: OpClaim, PubKey: pk, HeightM: &h, Code: "short"},
		{RequestID: id, Op: OpApprove, ClaimID: "x"},
	}
	for i, c := range badCmds {
		if err := c.Validate(); err == nil {
			t.Errorf("bad[%d] validated", i)
		}
	}
}

func TestLimits789_Defaults(t *testing.T) {
	var c *Config
	l := c.Limits()
	if l.MaxPending != 256 || l.MaxPendingPerNode != 3 || l.MaxQueuedRequests != 256 || l.SubmissionsPerHour != 20 || l.RetentionDays != 30 || l.RequestTTL.Hours() != 24 || l.MaxResults != 1024 {
		t.Fatalf("defaults %+v", l)
	}
	if c.SubmissionsRequested() || c.AutoApprove() {
		t.Fatal("nil config must be off")
	}
	on := true
	c = &Config{Enabled: &on, AutoApproveProven: true, MaxPendingPerNode: 5}
	if !c.SubmissionsRequested() || !c.AutoApprove() || c.Limits().MaxPendingPerNode != 5 {
		t.Fatal("explicit config ignored")
	}
	// Auto-approval is off by default, also with submissions on.
	if (&Config{Enabled: &on}).AutoApprove() {
		t.Fatal("auto-approval must default off")
	}
}
