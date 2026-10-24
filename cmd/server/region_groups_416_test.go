// #416 — configurable named region quick-pick groups.
//
// The config side of the feature: `regionGroups` in config.json is an
// optional, bounded list of named groups of observer-region codes, and
// GET /api/config/regions/groups serves the groups a client can actually
// select (codes filtered against the same effective region map that
// GET /api/config/regions returns).
//
// The rule the tests below pin: a structural violation (no name, a name over
// the length cap, no codes, more codes than the cap, a repeated name, more
// groups than the cap) DROPS the offending entry, and normalization (trim,
// case-insensitive de-duplication, removal of codes no observer reports) is
// the only thing that ever rewrites an entry. Silently filtering by a subset
// of a too-long code list, or by a truncated name, would mislabel a filter.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRegionGroupsConfig drops a config.json into dir for LoadConfig.
func writeRegionGroupsConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
}

// collectWarnings returns a warn func for normalizeRegionGroups plus the
// messages it produced, so the tests can assert that a dropped entry is not
// dropped silently.
func collectWarnings() (func(string, ...interface{}), *[]string) {
	var msgs []string
	return func(format string, v ...interface{}) {
		msgs = append(msgs, fmt.Sprintf(format, v...))
	}, &msgs
}

func groupNames(gs []RegionGroup) []string {
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.Name)
	}
	return out
}

func assertGroups(t *testing.T, got []RegionGroup, want []RegionGroup) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d groups %v, want %d %v", len(got), groupNames(got), len(want), groupNames(want))
	}
	for i := range want {
		if got[i].Name != want[i].Name {
			t.Errorf("group %d: name %q, want %q", i, got[i].Name, want[i].Name)
		}
		if strings.Join(got[i].Codes, ",") != strings.Join(want[i].Codes, ",") {
			t.Errorf("group %d (%s): codes %v, want %v", i, want[i].Name, got[i].Codes, want[i].Codes)
		}
	}
}

// --- absent config: exactly today's behaviour ---

func TestNormalizeRegionGroupsAbsent(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []RegionGroup
	}{
		{"nil", nil},
		{"empty", []RegionGroup{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warn, msgs := collectWarnings()
			got := normalizeRegionGroups(tc.in, warn)
			if len(got) != 0 {
				t.Errorf("got %v, want no groups", groupNames(got))
			}
			if len(*msgs) != 0 {
				t.Errorf("absent config warned: %v", *msgs)
			}
		})
	}
}

func TestLoadConfigNormalizesRegionGroups(t *testing.T) {
	dir := t.TempDir()
	writeRegionGroupsConfig(t, dir, `{
		"port": 3000,
		"regions": {"SJC": "San Jose", "SFO": "San Francisco"},
		"regionGroups": [
			{"name": "  Bay Area  ", "codes": ["sjc", " SFO ", "SJC"]},
			{"name": "", "codes": ["SJC"]}
		]
	}`)
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	assertGroups(t, cfg.RegionGroups, []RegionGroup{{Name: "Bay Area", Codes: []string{"sjc", "SFO"}}})
}

func TestLoadConfigWithoutRegionGroups(t *testing.T) {
	dir := t.TempDir()
	writeRegionGroupsConfig(t, dir, `{"port": 3000, "regions": {"SJC": "San Jose"}}`)
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.RegionGroups) != 0 {
		t.Errorf("got %v, want no groups", groupNames(cfg.RegionGroups))
	}
}

// --- invalid entries are dropped, loudly ---

func TestNormalizeRegionGroupsDropsInvalidEntries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry RegionGroup
	}{
		{"empty name", RegionGroup{Name: "", Codes: []string{"SJC"}}},
		{"blank name", RegionGroup{Name: "   ", Codes: []string{"SJC"}}},
		{"nil codes", RegionGroup{Name: "Bay", Codes: nil}},
		{"empty codes", RegionGroup{Name: "Bay", Codes: []string{}}},
		{"blank codes only", RegionGroup{Name: "Bay", Codes: []string{" ", ""}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warn, msgs := collectWarnings()
			got := normalizeRegionGroups([]RegionGroup{tc.entry, {Name: "Keep", Codes: []string{"SFO"}}}, warn)
			assertGroups(t, got, []RegionGroup{{Name: "Keep", Codes: []string{"SFO"}}})
			if len(*msgs) != 1 {
				t.Errorf("want exactly one warning, got %v", *msgs)
			}
		})
	}
}

// --- duplicates ---

func TestNormalizeRegionGroupsDuplicateNames(t *testing.T) {
	warn, msgs := collectWarnings()
	got := normalizeRegionGroups([]RegionGroup{
		{Name: "Bay Area", Codes: []string{"SJC"}},
		{Name: " bay area ", Codes: []string{"SFO"}},
		{Name: "BAY AREA", Codes: []string{"OAK"}},
		{Name: "SoCal", Codes: []string{"LAX"}},
	}, warn)
	// First spelling wins; the later repeats are dropped, not merged.
	assertGroups(t, got, []RegionGroup{
		{Name: "Bay Area", Codes: []string{"SJC"}},
		{Name: "SoCal", Codes: []string{"LAX"}},
	})
	if len(*msgs) != 2 {
		t.Errorf("want one warning per dropped repeat, got %v", *msgs)
	}
}

func TestNormalizeRegionGroupsDuplicateCodes(t *testing.T) {
	warn, msgs := collectWarnings()
	got := normalizeRegionGroups([]RegionGroup{
		{Name: "Bay Area", Codes: []string{"SJC", "sjc", " SJC ", "SFO", "Sfo"}},
	}, warn)
	// De-duplication is normalization, not a violation: the group survives
	// with the first spelling of each code, in configured order.
	assertGroups(t, got, []RegionGroup{{Name: "Bay Area", Codes: []string{"SJC", "SFO"}}})
	if len(*msgs) != 0 {
		t.Errorf("de-duplication must not warn, got %v", *msgs)
	}
}

// --- limits ---

func TestNormalizeRegionGroupsLimits(t *testing.T) {
	t.Run("too many groups", func(t *testing.T) {
		in := make([]RegionGroup, 0, regionGroupsMax+3)
		for i := 0; i < regionGroupsMax+3; i++ {
			in = append(in, RegionGroup{Name: fmt.Sprintf("G%02d", i), Codes: []string{"SJC"}})
		}
		warn, msgs := collectWarnings()
		got := normalizeRegionGroups(in, warn)
		if len(got) != regionGroupsMax {
			t.Fatalf("kept %d groups, want %d", len(got), regionGroupsMax)
		}
		if got[0].Name != "G00" || got[regionGroupsMax-1].Name != fmt.Sprintf("G%02d", regionGroupsMax-1) {
			t.Errorf("wrong groups kept: %v", groupNames(got))
		}
		if len(*msgs) != 1 {
			t.Errorf("want one overflow warning, got %v", *msgs)
		}
	})

	t.Run("group at the group cap is kept", func(t *testing.T) {
		in := make([]RegionGroup, 0, regionGroupsMax)
		for i := 0; i < regionGroupsMax; i++ {
			in = append(in, RegionGroup{Name: fmt.Sprintf("G%02d", i), Codes: []string{"SJC"}})
		}
		warn, msgs := collectWarnings()
		got := normalizeRegionGroups(in, warn)
		if len(got) != regionGroupsMax {
			t.Fatalf("kept %d groups, want %d", len(got), regionGroupsMax)
		}
		if len(*msgs) != 0 {
			t.Errorf("exactly the cap must not warn, got %v", *msgs)
		}
	})

	t.Run("too many codes drops the group", func(t *testing.T) {
		codes := make([]string, 0, regionGroupCodesMax+1)
		for i := 0; i <= regionGroupCodesMax; i++ {
			codes = append(codes, fmt.Sprintf("C%02d", i))
		}
		warn, msgs := collectWarnings()
		got := normalizeRegionGroups([]RegionGroup{
			{Name: "Huge", Codes: codes},
			{Name: "Exact", Codes: codes[:regionGroupCodesMax]},
		}, warn)
		assertGroups(t, got, []RegionGroup{{Name: "Exact", Codes: codes[:regionGroupCodesMax]}})
		if len(*msgs) != 1 {
			t.Errorf("want one warning for the dropped group, got %v", *msgs)
		}
	})

	t.Run("repeats do not count against the code cap", func(t *testing.T) {
		codes := make([]string, 0, regionGroupCodesMax+4)
		for i := 0; i < regionGroupCodesMax; i++ {
			codes = append(codes, fmt.Sprintf("C%02d", i))
		}
		codes = append(codes, "c00", "C01", "c02", "C03")
		warn, _ := collectWarnings()
		got := normalizeRegionGroups([]RegionGroup{{Name: "Dedup", Codes: codes}}, warn)
		if len(got) != 1 || len(got[0].Codes) != regionGroupCodesMax {
			t.Fatalf("got %d groups / %v, want one group of %d codes", len(got), got, regionGroupCodesMax)
		}
	})

	t.Run("name over the length cap drops the group", func(t *testing.T) {
		warn, msgs := collectWarnings()
		long := strings.Repeat("æ", regionGroupNameMax+1)
		exact := strings.Repeat("ø", regionGroupNameMax)
		got := normalizeRegionGroups([]RegionGroup{
			{Name: long, Codes: []string{"SJC"}},
			{Name: exact, Codes: []string{"SFO"}},
		}, warn)
		// Counted in runes: a 41-character Danish name is over the cap, a
		// 40-character one is not, no matter how many bytes they take.
		assertGroups(t, got, []RegionGroup{{Name: exact, Codes: []string{"SFO"}}})
		if len(*msgs) != 1 {
			t.Errorf("want one warning for the dropped group, got %v", *msgs)
		}
	})
}

// TestRegionGroupLimitsAreTheDocumentedNumbers pins the caps as literals.
// The behavioural cases above are built from the constants, so they move with
// them: on their own, widening a cap would not fail a single test. These are
// the numbers docs/api-spec.md, the api-spec table and config.example.json
// promise an operator, so changing one is a documentation change too.
func TestRegionGroupLimitsAreTheDocumentedNumbers(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"regionGroupsMax", regionGroupsMax, 12},
		{"regionGroupCodesMax", regionGroupCodesMax, 24},
		{"regionGroupNameMax", regionGroupNameMax, 40},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d (update docs/api-spec.md and config.example.json too)",
				tc.name, tc.got, tc.want)
		}
	}

	t.Run("the 13th group is dropped", func(t *testing.T) {
		in := make([]RegionGroup, 0, 13)
		for i := 0; i < 13; i++ {
			in = append(in, RegionGroup{Name: fmt.Sprintf("G%02d", i), Codes: []string{"SJC"}})
		}
		warn, _ := collectWarnings()
		got := normalizeRegionGroups(in, warn)
		if len(got) != 12 || groupNames(got)[11] != "G11" {
			t.Errorf("kept %v, want the first 12", groupNames(got))
		}
	})

	t.Run("a 25-code group is dropped, a 24-code one is kept", func(t *testing.T) {
		codes := make([]string, 0, 25)
		for i := 0; i < 25; i++ {
			codes = append(codes, fmt.Sprintf("C%02d", i))
		}
		warn, _ := collectWarnings()
		got := normalizeRegionGroups([]RegionGroup{
			{Name: "Twenty five", Codes: codes},
			{Name: "Twenty four", Codes: codes[:24]},
		}, warn)
		if len(got) != 1 || got[0].Name != "Twenty four" {
			t.Errorf("kept %v, want only the 24-code group", groupNames(got))
		}
	})

	t.Run("a 41-character name is dropped, a 40-character one is kept", func(t *testing.T) {
		warn, _ := collectWarnings()
		got := normalizeRegionGroups([]RegionGroup{
			{Name: strings.Repeat("a", 41), Codes: []string{"SJC"}},
			{Name: strings.Repeat("b", 40), Codes: []string{"SFO"}},
		}, warn)
		if len(got) != 1 || got[0].Name != strings.Repeat("b", 40) {
			t.Errorf("kept %d group(s), want only the 40-character one", len(got))
		}
	})
}

// --- unknown codes: filtered per request against the effective region map ---

func TestRegionGroupsForRegions(t *testing.T) {
	regions := map[string]string{"SJC": "San Jose", "SFO": "San Francisco", "OAK": "Oakland"}

	t.Run("unknown codes are dropped", func(t *testing.T) {
		got := regionGroupsForRegions([]RegionGroup{
			{Name: "Bay Area", Codes: []string{"SJC", "XXX", "OAK"}},
		}, regions)
		assertGroups(t, got, []RegionGroup{{Name: "Bay Area", Codes: []string{"SJC", "OAK"}}})
	})

	t.Run("a group with no known code is omitted", func(t *testing.T) {
		got := regionGroupsForRegions([]RegionGroup{
			{Name: "Nowhere", Codes: []string{"XXX", "YYY"}},
			{Name: "Bay Area", Codes: []string{"SJC"}},
		}, regions)
		assertGroups(t, got, []RegionGroup{{Name: "Bay Area", Codes: []string{"SJC"}}})
	})

	t.Run("codes match case-insensitively and take the served spelling", func(t *testing.T) {
		got := regionGroupsForRegions([]RegionGroup{
			{Name: "Bay Area", Codes: []string{"sjc", "oAk"}},
		}, regions)
		assertGroups(t, got, []RegionGroup{{Name: "Bay Area", Codes: []string{"SJC", "OAK"}}})
	})

	t.Run("two spellings of one code collapse", func(t *testing.T) {
		got := regionGroupsForRegions([]RegionGroup{
			{Name: "Bay Area", Codes: []string{"sjc", "SJC"}},
		}, regions)
		assertGroups(t, got, []RegionGroup{{Name: "Bay Area", Codes: []string{"SJC"}}})
	})

	t.Run("no regions at all leaves no group", func(t *testing.T) {
		got := regionGroupsForRegions([]RegionGroup{{Name: "Bay Area", Codes: []string{"SJC"}}}, map[string]string{})
		if len(got) != 0 {
			t.Errorf("got %v, want no groups", groupNames(got))
		}
		if got == nil {
			t.Error("must return an empty slice, not nil, so the JSON is [] and the client can call .forEach")
		}
	})

	t.Run("absent config returns an empty slice", func(t *testing.T) {
		got := regionGroupsForRegions(nil, regions)
		if got == nil || len(got) != 0 {
			t.Errorf("got %v, want an empty non-nil slice", got)
		}
	})
}

// --- the endpoint ---

func TestHandleConfigRegionGroupsEndpoint(t *testing.T) {
	srv, router := setupTestServer(t)

	get := func(t *testing.T) []RegionGroup {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/config/regions/groups", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d, want 200", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("Content-Type %q, want JSON", ct)
		}
		if body := strings.TrimSpace(w.Body.String()); strings.HasPrefix(body, "null") {
			t.Fatalf("body is null, want a JSON array: %s", body)
		}
		var out []RegionGroup
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v", w.Body.String(), err)
		}
		return out
	}

	t.Run("no groups configured serves []", func(t *testing.T) {
		srv.cfg.RegionGroups = nil
		if got := get(t); len(got) != 0 {
			t.Errorf("got %v, want none", groupNames(got))
		}
	})

	t.Run("configured groups are filtered to served codes", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/config/regions", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		var served map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &served); err != nil {
			t.Fatalf("decode regions: %v", err)
		}
		if len(served) == 0 {
			t.Skip("fixture serves no region codes")
		}
		var one string
		for code := range served {
			if one == "" || code < one {
				one = code
			}
		}
		srv.cfg.RegionGroups = []RegionGroup{
			{Name: "Known", Codes: []string{strings.ToLower(one), "ZZZ"}},
			{Name: "Unknown only", Codes: []string{"ZZZ"}},
		}
		assertGroups(t, get(t), []RegionGroup{{Name: "Known", Codes: []string{one}}})
	})

	t.Run("the regions endpoint stays a flat code to label map", func(t *testing.T) {
		srv.cfg.RegionGroups = []RegionGroup{{Name: "Known", Codes: []string{"SJC"}}}
		req := httptest.NewRequest("GET", "/api/config/regions", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		var served map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &served); err != nil {
			t.Fatalf("/api/config/regions must stay a flat map: %v (%s)", err, w.Body.String())
		}
		for _, key := range []string{"groups", "regionGroups", "regions"} {
			if _, ok := served[key]; ok {
				t.Errorf("/api/config/regions grew a %q key: the documented contract is code → label", key)
			}
		}
	})
}
