package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func rpm2FileHash(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// rpm2LongHex matches anything shaped like a node key or a key prefix.
var rpm2LongHex = regexp.MustCompile(`(?i)[0-9a-f]{12,}`)

// TestRegionPlacement488_DryRunReport: the operator report counts nodes per
// basis and region and sweeps maxHops, on the synthetic scenario whose
// ground truth is known. It must name no node and change no byte of the
// database it reads.
func TestRegionPlacement488_DryRunReport(t *testing.T) {
	path, truth := rpm2FileDB(t, 1, time.Now().Unix())
	before := rpm2FileHash(t, path)

	settings := regionPlacementSettings{Mode: regionModeHeard, Params: defaultRegionPlacementParams}
	var out bytes.Buffer
	if err := runRegionPlacementDryRun(path, settings, &out); err != nil {
		t.Fatal(err)
	}
	report := out.String()
	t.Logf("\n%s", report)

	if rpm2FileHash(t, path) != before {
		t.Error("the dry-run changed the database file")
	}
	for pk := range truth {
		if strings.Contains(strings.ToLower(report), pk) {
			t.Fatalf("report names node %s", pk)
		}
	}
	if m := rpm2LongHex.FindString(report); m != "" {
		t.Fatalf("report contains a key-shaped hex string %q", m)
	}
	for _, want := range []string{
		"# Region placement dry-run (#488)",
		"maxHops 3", "lifetimeDays 30", "persistSpanHours 0",
		"| AAA |", "| DDD |",
		"| 0 |", "| 7 |",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q", want)
		}
	}

	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r, err := buildRegionPlacementDryRun(context.Background(), db, settings, regionDryRunSweep)
	if err != nil {
		t.Fatal(err)
	}
	// Scenario under the default rule (maxHops 3, transient rule off):
	// direct = residents, boundary, one-off far, re-shared, quiet, moved
	// and scoped (27); fewest_hops = the four 1-hop companions; unknown =
	// the four 5-hop nodes and the four legacy NULL-path nodes.
	if r.Nodes != 39 || r.Bases.Direct != 27 || r.Bases.Fewest != 4 || r.Bases.Unknown != 8 {
		t.Errorf("bases: nodes %d direct %d fewest %d unknown %d; want 39/27/4/8", r.Nodes, r.Bases.Direct, r.Bases.Fewest, r.Bases.Unknown)
	}
	if r.Bases.FewestByHops[1] != 4 {
		t.Errorf("fewest by hops %v, want 4 at 1 hop", r.Bases.FewestByHops)
	}
	wantUnknown := map[int]int{0: 12, 1: 8, 2: 8, 3: 8, 4: 8, 5: 4, 7: 4}
	if len(r.Sweep) != len(regionDryRunSweep) {
		t.Fatalf("sweep has %d rows, want %d", len(r.Sweep), len(regionDryRunSweep))
	}
	for _, row := range r.Sweep {
		if row.Direct+row.Fewest+row.Unknown != r.Nodes {
			t.Errorf("maxHops %d: bases do not partition the nodes: %+v", row.MaxHops, row)
		}
		if w, ok := wantUnknown[row.MaxHops]; ok && row.Unknown != w {
			t.Errorf("maxHops %d: unknown %d, want %d", row.MaxHops, row.Unknown, w)
		}
	}
	// Heard per region is exactly what today's filter serves.
	cur := map[string]int{}
	for _, reg := range rpm2Regions {
		cur[reg] = len(rpm2Members(t, db, reg))
	}
	got := map[string]int{}
	for _, row := range r.Regions {
		got[row.Region] = row.Heard
		if row.Placed > row.Heard || row.Removed != row.Heard-row.Placed || row.Direct+row.Fewest != row.Placed {
			t.Errorf("region %s row inconsistent: %+v", row.Region, row)
		}
	}
	if !reflect.DeepEqual(got, cur) || !reflect.DeepEqual(cur, map[string]int{"AAA": 22, "BBB": 23, "CCC": 23, "DDD": 22}) {
		t.Errorf("heard per region %v, today's filter %v, pinned AAA 22 BBB 23 CCC 23 DDD 22", got, cur)
	}
	if r.Hearings[hearingShared] != 12 || r.Hearings[hearingUnknown] != 20 {
		t.Errorf("hearings %v: want 12 shared (3 re-shares × 4) and 20 unknown (5 NULL paths × 4)", r.Hearings)
	}
	// With the transient rule on, the four one-off far hearings are counted.
	span := settings
	span.Params.PersistSpan = 24 * time.Hour
	rs, err := buildRegionPlacementDryRun(context.Background(), db, span, regionDryRunSweep)
	if err != nil {
		t.Fatal(err)
	}
	if rs.Bases.Transient != 4 {
		t.Errorf("transient nodes %d, want 4", rs.Bases.Transient)
	}
}

// TestRegionPlacement488_DryRunEmptyDB: a database without advert evidence
// reports zeros instead of failing or dividing by zero.
func TestRegionPlacement488_DryRunEmptyDB(t *testing.T) {
	db := setupTestDB(t)
	path := filepath.Join(t.TempDir(), "empty.db")
	if _, err := db.conn.Exec(`VACUUM INTO ?`, path); err != nil {
		t.Fatal(err)
	}
	db.Close()
	var out bytes.Buffer
	if err := runRegionPlacementDryRun(path, regionPlacementSettings{Mode: regionModeHeard, Params: defaultRegionPlacementParams}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Nodes with advert evidence in a region: 0") || strings.Contains(out.String(), "NaN") {
		t.Fatalf("empty report:\n%s", out.String())
	}
}

const rpm2DryRunHelperEnv = "CORESCOPE_488_DRYRUN_HELPER"

// TestRegionPlacement488_DryRunProcess is the child of the process test
// below: it runs the real main() with the dry-run flag.
func TestRegionPlacement488_DryRunProcess(t *testing.T) {
	if os.Getenv(rpm2DryRunHelperEnv) != "1" {
		t.Skip("helper process for TestRegionPlacement488_DryRunFlag")
	}
	os.Args = []string{"corescope-server",
		"-config-dir", os.Getenv("CORESCOPE_488_DRYRUN_CFG"),
		"-db", os.Getenv("CORESCOPE_488_DRYRUN_DB"),
		"-region-placement-dryrun"}
	main()
}

// TestRegionPlacement488_DryRunFlag: `corescope-server -region-placement-dryrun`
// prints the report for the configured settings and exits 0 without
// serving.
func TestRegionPlacement488_DryRunFlag(t *testing.T) {
	path, _ := rpm2FileDB(t, 1, time.Now().Unix())
	cfgDir := t.TempDir()
	cfg := `{"regionPlacement": {"maxHops": 2, "lifetimeDays": 0}}`
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRegionPlacement488_DryRunProcess$")
	cmd.Env = append(os.Environ(), rpm2DryRunHelperEnv+"=1", "CORESCOPE_488_DRYRUN_CFG="+cfgDir, "CORESCOPE_488_DRYRUN_DB="+path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("dry-run process: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	for _, want := range []string{"# Region placement dry-run (#488)", "maxHops 2", "lifetimeDays 0 (no window)"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout.String())
		}
	}
	if strings.Contains(stderr.String(), "listening") || strings.Contains(stdout.String(), "listening") {
		t.Errorf("dry-run started serving:\n%s", stderr.String())
	}
}

// TestRegionPlacement488_Config: every field defaults to today's behaviour,
// explicit values are honoured, and values the rule cannot use are replaced.
func TestRegionPlacement488_Config(t *testing.T) {
	load := func(t *testing.T, js string) regionPlacementSettings {
		t.Helper()
		dir := t.TempDir()
		if js != "" {
			if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(js), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		cfg, err := LoadConfig(dir)
		if err != nil {
			t.Fatal(err)
		}
		return cfg.RegionPlacementSettings()
	}
	def := regionPlacementSettings{Mode: regionModeHeard, Params: regionPlacementParams{MaxHops: 3, Lifetime: 30 * 24 * time.Hour}}
	for _, tc := range []struct {
		name, js string
		want     regionPlacementSettings
	}{
		{"no config file", "", def},
		{"no block", `{"port": 3000}`, def},
		{"empty block", `{"regionPlacement": {}}`, def},
		{"explicit", `{"regionPlacement": {"mode": " Placed ", "maxHops": 0, "lifetimeDays": 0, "persistSpanHours": 24}}`,
			regionPlacementSettings{Mode: regionModePlaced, Params: regionPlacementParams{MaxHops: 0, Lifetime: 0, PersistSpan: 24 * time.Hour}}},
		{"maxHops above the stored slots", `{"regionPlacement": {"maxHops": 99}}`,
			regionPlacementSettings{Mode: regionModeHeard, Params: regionPlacementParams{MaxHops: regionPlacementHopSlots - 1, Lifetime: def.Params.Lifetime}}},
		{"negative values", `{"regionPlacement": {"maxHops": -1, "lifetimeDays": -5, "persistSpanHours": -2}}`,
			regionPlacementSettings{Mode: regionModeHeard, Params: regionPlacementParams{MaxHops: 0, Lifetime: def.Params.Lifetime}}},
		{"unknown mode keeps heard", `{"regionPlacement": {"mode": "guess"}}`, def},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := load(t, tc.js); got != tc.want {
				t.Errorf("settings %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestRegionPlacement488_ConfigExample: config.example.json documents the
// block, with values that equal the defaults.
func TestRegionPlacement488_ConfigExample(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var ex struct {
		RegionPlacement map[string]any `json:"regionPlacement"`
	}
	if err := json.Unmarshal(raw, &ex); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"_comment", "mode", "maxHops", "lifetimeDays", "persistSpanHours"} {
		if _, ok := ex.RegionPlacement[k]; !ok {
			t.Errorf("config.example.json regionPlacement lacks %q", k)
		}
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.RegionPlacementSettings(); got != (&Config{}).RegionPlacementSettings() {
		t.Errorf("config.example.json settings %+v differ from the defaults", got)
	}
}
