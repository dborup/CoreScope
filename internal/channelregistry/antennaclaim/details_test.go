package antennaclaim

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// #898 M1 — station details: the shared rules (T10) and the combined
// claim's parts (T3, the command side).

func fp(v float64) *float64 { return &v }

func validClaim() Command {
	return Command{RequestID: "0123456789abcdef", Op: OpClaim, PubKey: strings.Repeat("ab", 32), Code: "7KX9QM"}
}

func TestDetails898_ValidateTable(t *testing.T) {
	ok := func(mod func(*DetailsInput)) DetailsInput {
		d := DetailsInput{StationDetails: StationDetails{AntennaType: "omni"}}
		mod(&d)
		return d
	}
	for name, tc := range map[string]struct {
		d    DetailsInput
		want error
	}{
		"each antenna type omni":   {ok(func(d *DetailsInput) { d.AntennaType = "omni" }), nil},
		"each antenna type yagi":   {ok(func(d *DetailsInput) { d.AntennaType = "yagi" }), nil},
		"each antenna type panel":  {ok(func(d *DetailsInput) { d.AntennaType = "panel" }), nil},
		"each antenna type other":  {ok(func(d *DetailsInput) { d.AntennaType = "other" }), nil},
		"antenna type upper case":  {ok(func(d *DetailsInput) { d.AntennaType = "Omni" }), ErrDetailsAntennaType},
		"antenna type unknown":     {ok(func(d *DetailsInput) { d.AntennaType = "dipole" }), ErrDetailsAntennaType},
		"installation roof":        {ok(func(d *DetailsInput) { d.Installation = "roof" }), nil},
		"installation mast":        {ok(func(d *DetailsInput) { d.Installation = "mast" }), nil},
		"installation indoor":      {ok(func(d *DetailsInput) { d.Installation = "indoor" }), nil},
		"installation portable":    {ok(func(d *DetailsInput) { d.Installation = "portable" }), nil},
		"installation unknown":     {ok(func(d *DetailsInput) { d.Installation = "tower" }), ErrDetailsInstallation},
		"gain -5.0":                {ok(func(d *DetailsInput) { d.GainDBi = fp(-5.0) }), nil},
		"gain 20.0":                {ok(func(d *DetailsInput) { d.GainDBi = fp(20.0) }), nil},
		"gain 0":                   {ok(func(d *DetailsInput) { d.GainDBi = fp(0) }), nil},
		"gain 5.8":                 {ok(func(d *DetailsInput) { d.GainDBi = fp(5.8) }), nil},
		"gain -5.1":                {ok(func(d *DetailsInput) { d.GainDBi = fp(-5.1) }), ErrDetailsGain},
		"gain 20.1":                {ok(func(d *DetailsInput) { d.GainDBi = fp(20.1) }), ErrDetailsGain},
		"gain 0.05 off grid":       {ok(func(d *DetailsInput) { d.GainDBi = fp(0.05) }), ErrDetailsGain},
		"power one":                {ok(func(d *DetailsInput) { d.Power = []string{"solar"} }), nil},
		"power all three":          {ok(func(d *DetailsInput) { d.Power = []string{"battery", "mains", "solar"} }), nil},
		"power empty":              {ok(func(d *DetailsInput) { d.Power = []string{} }), ErrDetailsPower},
		"power duplicate":          {ok(func(d *DetailsInput) { d.Power = []string{"solar", "solar"} }), ErrDetailsPower},
		"power unknown":            {ok(func(d *DetailsInput) { d.Power = []string{"wind"} }), ErrDetailsPower},
		"nothing set":              {DetailsInput{}, ErrDetailsEmpty},
		"only contactAdmin (N2)":   {DetailsInput{ContactAdmin: "OZ1ABC"}, nil},
		"only contactPublic (N2)":  {DetailsInput{StationDetails: StationDetails{ContactPublic: "OZ1ABC"}}, nil},
		"only gain":                {DetailsInput{StationDetails: StationDetails{GainDBi: fp(3)}}, nil},
		"only power":               {DetailsInput{StationDetails: StationDetails{Power: []string{"mains"}}}, nil},
		"only installation":        {DetailsInput{StationDetails: StationDetails{Installation: "mast"}}, nil},
		"public contact bad":       {ok(func(d *DetailsInput) { d.ContactPublic = "a:b" }), ErrDetailsContact},
		"admin contact bad":        {ok(func(d *DetailsInput) { d.ContactAdmin = "<b>" }), ErrDetailsContact},
		"public contact good":      {ok(func(d *DetailsInput) { d.ContactPublic = "Søren @oz1abc #3" }), nil},
		"admin contact good":       {ok(func(d *DetailsInput) { d.ContactAdmin = "sms +45 1234/5678" }), nil},
		"both contacts and fields": {ok(func(d *DetailsInput) { d.ContactPublic, d.ContactAdmin = "a.b_c-d", "e" }), nil},
	} {
		if got := tc.d.Validate(); got != tc.want {
			t.Errorf("%s: Validate = %v, want %v", name, got, tc.want)
		}
	}
}

// TestDetails898_ContactAllowList covers both contact fields: the same
// function guards them (Q12), and Validate is checked for each field.
func TestDetails898_ContactAllowList(t *testing.T) {
	good := []string{
		"a", "OZ1ABC", "Søren Ærø", "jens@example.dk", "#meshdk", "+45 12 34 56 78", "a/b.c_d-e",
		strings.Repeat("x", 32), strings.Repeat("æ", 32), // 32 runes, 64 bytes
		"é",  // a combining mark (M)
		"١٢٣", // Arabic-Indic digits (Nd)
	}
	bad := map[string]string{
		"empty":             "",
		"33 runes":          strings.Repeat("x", 33),
		"over 64 bytes":     strings.Repeat("€", 22), // 22 runes, 66 bytes
		"colon":             "https://x",
		"rlo U+202E":        "ab‮cd",
		"lri U+2066":        "ab⁦cd",
		"zwj U+200D":        "ab‍cd",
		"zwsp U+200B":       "ab​cd",
		"line sep U+2028":   "ab cd",
		"para sep U+2029":   "ab cd",
		"lt":                "<img>",
		"amp":               "a&b",
		"quote":             `a"b`,
		"apostrophe":        "a'b",
		"newline":           "a\nb",
		"tab":               "a\tb",
		"nul":               "a\x00b",
		"leading space":     " ab",
		"trailing space":    "ab ",
		"double space":      "a  b",
		"nbsp":              "a b",
		"invalid utf8":      "a\xffb",
		"letter-like digit": "Ⅻ", // Nl, not Nd
		"emoji":             "📡",
	}
	for _, s := range good {
		if !ValidContact(s) {
			t.Errorf("ValidContact(%q) = false, want true", s)
		}
	}
	for name, s := range bad {
		if ValidContact(s) {
			t.Errorf("%s: ValidContact(%q) = true, want false", name, s)
		}
		for field, d := range map[string]DetailsInput{
			"contactPublic": {StationDetails: StationDetails{AntennaType: "omni", ContactPublic: s}},
			"contactAdmin":  {StationDetails: StationDetails{AntennaType: "omni"}, ContactAdmin: s},
		} {
			if s == "" {
				continue // an empty contact is "not given"
			}
			if d.Validate() == nil {
				t.Errorf("%s in %s accepted", name, field)
			}
		}
	}
}

func TestDetails898_GainReviewBand(t *testing.T) {
	for g, want := range map[float64]bool{-5: true, -0.1: true, 0: false, 7.5: false, 15: false, 15.1: true, 20: true} {
		if got := GainNeedsReview(g); got != want {
			t.Errorf("GainNeedsReview(%v) = %v, want %v", g, got, want)
		}
	}
}

func TestDetails898_PowerMaskRoundTrip(t *testing.T) {
	m, ok := PowerMask([]string{"battery", "mains"})
	if !ok || m != 5 {
		t.Fatalf("PowerMask = %d %v, want 5 true", m, ok)
	}
	if got := PowerList(m); !reflect.DeepEqual(got, []string{"mains", "battery"}) {
		t.Errorf("PowerList(5) = %v, want canonical [mains battery]", got)
	}
	if PowerList(0) != nil {
		t.Error("PowerList(0) must be nil")
	}
}

// TestCommand898_ClaimParts is the command side of T10: exactly one member
// per part, at least one part, details re-validated (the ingestor runs the
// same Validate on every queue file), and legacy {heightM} / {clear:true}
// command files still valid (N6).
func TestCommand898_ClaimParts(t *testing.T) {
	h := 12.5
	det := &DetailsInput{StationDetails: StationDetails{AntennaType: "yagi"}}
	for name, tc := range map[string]struct {
		mod  func(*Command)
		ok   bool
		want Parts
	}{
		"legacy height":            {func(c *Command) { c.HeightM = &h }, true, Parts{Height: PartSet}},
		"legacy clear":             {func(c *Command) { c.Clear = true }, true, Parts{Height: PartClear}},
		"details only":             {func(c *Command) { c.Details = det }, true, Parts{Details: PartSet}},
		"clear details only":       {func(c *Command) { c.ClearDetails = true }, true, Parts{Details: PartClear}},
		"height and details":       {func(c *Command) { c.HeightM, c.Details = &h, det }, true, Parts{PartSet, PartSet}},
		"clear both":               {func(c *Command) { c.Clear, c.ClearDetails = true, true }, true, Parts{PartClear, PartClear}},
		"height and clear details": {func(c *Command) { c.HeightM, c.ClearDetails = &h, true }, true, Parts{PartSet, PartClear}},
		"no part":                  {func(c *Command) {}, false, Parts{}},
		"heightM and clear":        {func(c *Command) { c.HeightM, c.Clear = &h, true }, false, Parts{}},
		"details and clearDetails": {func(c *Command) { c.Details, c.ClearDetails = det, true }, false, Parts{}},
		"empty details object":     {func(c *Command) { c.Details = &DetailsInput{} }, false, Parts{}},
		"bad gain":                 {func(c *Command) { c.Details = &DetailsInput{StationDetails: StationDetails{GainDBi: fp(20.1)}} }, false, Parts{}},
		"bad admin contact":        {func(c *Command) { c.Details = &DetailsInput{ContactAdmin: "a‮b"} }, false, Parts{}},
	} {
		c := validClaim()
		tc.mod(&c)
		err := c.Validate()
		if (err == nil) != tc.ok {
			t.Errorf("%s: Validate = %v, want ok=%v", name, err, tc.ok)
		}
		if tc.ok && c.Parts() != tc.want {
			t.Errorf("%s: Parts = %+v, want %+v", name, c.Parts(), tc.want)
		}
	}
}

// TestCommand898_ClearOnly is T3 for the command form: only claims whose
// every carried part is a clear are clear-only (mutant M2: any clear).
func TestCommand898_ClearOnly(t *testing.T) {
	for p, want := range map[Parts]bool{
		{}:                     false,
		{Height: PartSet}:      false,
		{Height: PartClear}:    true,
		{Details: PartSet}:     false,
		{Details: PartClear}:   true,
		{PartSet, PartSet}:     false,
		{PartSet, PartClear}:   false,
		{PartClear, PartSet}:   false,
		{PartClear, PartClear}: true,
	} {
		if got := p.ClearOnly(); got != want {
			t.Errorf("%+v.ClearOnly() = %v, want %v", p, got, want)
		}
	}
}

// TestDetails898_StrictJSON: the owner's body decodes the embedded public
// fields and contactAdmin, and the public type has no admin contact field.
func TestDetails898_StrictJSON(t *testing.T) {
	var d DetailsInput
	dec := json.NewDecoder(strings.NewReader(`{"antennaType":"omni","gainDbi":5.8,"power":["solar"],"installation":"mast","contactPublic":"p","contactAdmin":"a"}`))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		t.Fatal(err)
	}
	if d.AntennaType != "omni" || d.ContactPublic != "p" || d.ContactAdmin != "a" || d.GainDBi == nil || *d.GainDBi != 5.8 {
		t.Errorf("decoded %+v", d)
	}
	b, _ := json.Marshal(d.StationDetails)
	if strings.Contains(string(b), "contactAdmin") || strings.Contains(string(b), `"a"`) {
		t.Errorf("StationDetails carries the admin contact: %s", b)
	}
}

func TestConfig898_DetailsGate(t *testing.T) {
	var nilCfg *Config
	if nilCfg.DetailsRequested() {
		t.Error("nil config requests details")
	}
	c := &Config{}
	c.ApplyGates(func(path ...string) bool { return strings.Join(path, ".") == "antennaClaims.detailsEnabled" })
	if !c.DetailsRequested() || c.SubmissionsRequested() {
		t.Errorf("ApplyGates: details=%v enabled=%v", c.DetailsRequested(), c.SubmissionsRequested())
	}
	c.ApplyGates(func(path ...string) bool { return false })
	if c.DetailsRequested() {
		t.Error("detailsEnabled survives a config without it")
	}
}
