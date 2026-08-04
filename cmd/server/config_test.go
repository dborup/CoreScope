package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigValidJSON(t *testing.T) {
	dir := t.TempDir()
	cfgData := map[string]interface{}{
		"port":   8080,
		"dbPath": "/custom/path.db",
		"branding": map[string]interface{}{
			"siteName": "TestSite",
		},
		"mapDefaults": map[string]interface{}{
			"center": []float64{40.0, -74.0},
			"zoom":   12,
		},
		"regions": map[string]string{
			"SJC": "San Jose",
		},
		"healthThresholds": map[string]interface{}{
			"infraDegradedHours": 2,
			"infraSilentHours":   4,
			"nodeDegradedHours":  0.5,
			"nodeSilentHours":    2,
		},
		"liveMap": map[string]interface{}{
			"propagationBufferMs": 3000,
		},
		"timestamps": map[string]interface{}{
			"defaultMode":       "absolute",
			"timezone":          "utc",
			"formatPreset":      "iso-seconds",
			"customFormat":      "2006-01-02 15:04:05",
			"allowCustomFormat": true,
		},
	}
	data, _ := json.Marshal(cfgData)
	os.WriteFile(filepath.Join(dir, "config.json"), data, 0644)

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 8080 {
		t.Errorf("expected port 8080, got %d", cfg.Port)
	}
	if cfg.DBPath != "/custom/path.db" {
		t.Errorf("expected /custom/path.db, got %s", cfg.DBPath)
	}
	if cfg.MapDefaults.Zoom != 12 {
		t.Errorf("expected zoom 12, got %d", cfg.MapDefaults.Zoom)
	}
	if cfg.Timestamps == nil {
		t.Fatal("expected timestamps config")
	}
	if cfg.Timestamps.DefaultMode != "absolute" {
		t.Errorf("expected defaultMode absolute, got %s", cfg.Timestamps.DefaultMode)
	}
	if cfg.Timestamps.Timezone != "utc" {
		t.Errorf("expected timezone utc, got %s", cfg.Timestamps.Timezone)
	}
	if cfg.Timestamps.FormatPreset != "iso-seconds" {
		t.Errorf("expected formatPreset iso-seconds, got %s", cfg.Timestamps.FormatPreset)
	}
}

func TestLoadConfigFromDataSubdir(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	os.Mkdir(dataDir, 0755)
	cfgData := map[string]interface{}{"port": 9090}
	data, _ := json.Marshal(cfgData)
	os.WriteFile(filepath.Join(dataDir, "config.json"), data, 0644)

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 9090 {
		t.Errorf("expected port 9090, got %d", cfg.Port)
	}
}

func TestLoadConfigNoFiles(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 3000 {
		t.Errorf("expected default port 3000, got %d", cfg.Port)
	}
	ts := cfg.GetTimestampConfig()
	if ts.DefaultMode != "ago" || ts.Timezone != "local" || ts.FormatPreset != "iso" {
		t.Errorf("expected default timestamp config ago/local/iso, got %s/%s/%s", ts.DefaultMode, ts.Timezone, ts.FormatPreset)
	}
}

func TestLoadConfigInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.json"), []byte("{invalid"), 0644)

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Should return defaults when JSON is invalid
	if cfg.Port != 3000 {
		t.Errorf("expected default port 3000, got %d", cfg.Port)
	}
}

func TestLoadConfigNoArgs(t *testing.T) {
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
}

func TestLoadConfigTimestampNormalization(t *testing.T) {
	dir := t.TempDir()
	cfgData := map[string]interface{}{
		"timestamps": map[string]interface{}{
			"defaultMode":  "banana",
			"timezone":     "mars",
			"formatPreset": "weird",
		},
	}
	data, _ := json.Marshal(cfgData)
	os.WriteFile(filepath.Join(dir, "config.json"), data, 0644)

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timestamps == nil {
		t.Fatal("expected timestamps to be set")
	}
	if cfg.Timestamps.DefaultMode != "ago" {
		t.Errorf("expected normalized defaultMode ago, got %s", cfg.Timestamps.DefaultMode)
	}
	if cfg.Timestamps.Timezone != "local" {
		t.Errorf("expected normalized timezone local, got %s", cfg.Timestamps.Timezone)
	}
	if cfg.Timestamps.FormatPreset != "iso" {
		t.Errorf("expected normalized formatPreset iso, got %s", cfg.Timestamps.FormatPreset)
	}
}

func TestLoadThemeValidJSON(t *testing.T) {
	dir := t.TempDir()
	themeData := map[string]interface{}{
		"branding": map[string]interface{}{
			"siteName": "CustomTheme",
		},
		"theme": map[string]interface{}{
			"accent": "#ff0000",
		},
		"nodeColors": map[string]interface{}{
			"repeater": "#00ff00",
		},
	}
	data, _ := json.Marshal(themeData)
	os.WriteFile(filepath.Join(dir, "theme.json"), data, 0644)

	theme := LoadTheme(dir)
	if theme.Branding == nil {
		t.Fatal("expected branding")
	}
	if theme.Branding["siteName"] != "CustomTheme" {
		t.Errorf("expected CustomTheme, got %v", theme.Branding["siteName"])
	}
	if theme.Theme["accent"] != "#ff0000" {
		t.Errorf("expected #ff0000, got %v", theme.Theme["accent"])
	}
}

func TestLoadThemeFromDataSubdir(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	os.Mkdir(dataDir, 0755)
	themeData := map[string]interface{}{
		"branding": map[string]interface{}{"siteName": "DataTheme"},
	}
	data, _ := json.Marshal(themeData)
	os.WriteFile(filepath.Join(dataDir, "theme.json"), data, 0644)

	theme := LoadTheme(dir)
	if theme.Branding == nil {
		t.Fatal("expected branding")
	}
	if theme.Branding["siteName"] != "DataTheme" {
		t.Errorf("expected DataTheme, got %v", theme.Branding["siteName"])
	}
}

func TestLoadThemeNoFile(t *testing.T) {
	dir := t.TempDir()
	theme := LoadTheme(dir)
	if theme == nil {
		t.Fatal("expected non-nil theme")
	}
}

func TestLoadThemeNoArgs(t *testing.T) {
	theme := LoadTheme()
	if theme == nil {
		t.Fatal("expected non-nil theme")
	}
}

func TestLoadThemeInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "theme.json"), []byte("{bad json"), 0644)
	theme := LoadTheme(dir)
	// Should return empty theme
	if theme == nil {
		t.Fatal("expected non-nil theme")
	}
}

func TestGetHealthThresholdsDefaults(t *testing.T) {
	cfg := &Config{}
	ht := cfg.GetHealthThresholds()

	if ht.InfraDegradedHours != 24 {
		t.Errorf("expected 24, got %v", ht.InfraDegradedHours)
	}
	if ht.InfraSilentHours != 72 {
		t.Errorf("expected 72, got %v", ht.InfraSilentHours)
	}
	if ht.NodeDegradedHours != 1 {
		t.Errorf("expected 1, got %v", ht.NodeDegradedHours)
	}
	if ht.NodeSilentHours != 24 {
		t.Errorf("expected 24, got %v", ht.NodeSilentHours)
	}
}

func TestGetHealthThresholdsCustom(t *testing.T) {
	cfg := &Config{
		HealthThresholds: &HealthThresholds{
			InfraDegradedHours: 2,
			InfraSilentHours:   4,
			NodeDegradedHours:  0.5,
			NodeSilentHours:    2,
		},
	}
	ht := cfg.GetHealthThresholds()

	if ht.InfraDegradedHours != 2 {
		t.Errorf("expected 2, got %v", ht.InfraDegradedHours)
	}
	if ht.InfraSilentHours != 4 {
		t.Errorf("expected 4, got %v", ht.InfraSilentHours)
	}
	if ht.NodeDegradedHours != 0.5 {
		t.Errorf("expected 0.5, got %v", ht.NodeDegradedHours)
	}
	if ht.NodeSilentHours != 2 {
		t.Errorf("expected 2, got %v", ht.NodeSilentHours)
	}
}

func TestGetHealthThresholdsPartialCustom(t *testing.T) {
	cfg := &Config{
		HealthThresholds: &HealthThresholds{
			InfraDegradedHours: 2,
			// Others left as zero → should use defaults
		},
	}
	ht := cfg.GetHealthThresholds()

	if ht.InfraDegradedHours != 2 {
		t.Errorf("expected 2, got %v", ht.InfraDegradedHours)
	}
	if ht.InfraSilentHours != 72 {
		t.Errorf("expected default 72, got %v", ht.InfraSilentHours)
	}
}

func TestGetHealthMs(t *testing.T) {
	ht := HealthThresholds{
		InfraDegradedHours: 24,
		InfraSilentHours:   72,
		NodeDegradedHours:  1,
		NodeSilentHours:    24,
	}

	tests := []struct {
		role       string
		wantDeg    int
		wantSilent int
	}{
		{"repeater", 86400000, 259200000},
		{"room", 86400000, 259200000},
		{"companion", 3600000, 86400000},
		{"sensor", 3600000, 86400000},
		{"unknown", 3600000, 86400000},
	}

	for _, tc := range tests {
		t.Run(tc.role, func(t *testing.T) {
			deg, sil := ht.GetHealthMs(tc.role)
			if deg != tc.wantDeg {
				t.Errorf("degraded: expected %d, got %d", tc.wantDeg, deg)
			}
			if sil != tc.wantSilent {
				t.Errorf("silent: expected %d, got %d", tc.wantSilent, sil)
			}
		})
	}
}

func TestResolveDBPath(t *testing.T) {
	t.Run("DBPath set", func(t *testing.T) {
		cfg := &Config{DBPath: "/explicit/path.db"}
		got := cfg.ResolveDBPath("/base")
		if got != "/explicit/path.db" {
			t.Errorf("expected /explicit/path.db, got %s", got)
		}
	})

	t.Run("env var", func(t *testing.T) {
		cfg := &Config{}
		t.Setenv("DB_PATH", "/env/path.db")
		got := cfg.ResolveDBPath("/base")
		if got != "/env/path.db" {
			t.Errorf("expected /env/path.db, got %s", got)
		}
	})

	t.Run("default", func(t *testing.T) {
		cfg := &Config{}
		t.Setenv("DB_PATH", "")
		got := cfg.ResolveDBPath("/base")
		expected := filepath.Join("/base", "data", "meshcore.db")
		if got != expected {
			t.Errorf("expected %s, got %s", expected, got)
		}
	})
}

func TestPropagationBufferMs(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		cfg := &Config{}
		if cfg.PropagationBufferMs() != 5000 {
			t.Errorf("expected 5000, got %d", cfg.PropagationBufferMs())
		}
	})

	t.Run("custom", func(t *testing.T) {
		cfg := &Config{}
		cfg.LiveMap.PropagationBufferMs = 3000
		if cfg.PropagationBufferMs() != 3000 {
			t.Errorf("expected 3000, got %d", cfg.PropagationBufferMs())
		}
	})
}

func TestObserverDaysOrDefault(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
		want int
	}{
		{"nil retention", &Config{}, 14},
		{"zero observer days", &Config{Retention: &RetentionConfig{ObserverDays: 0}}, 14},
		{"positive value", &Config{Retention: &RetentionConfig{ObserverDays: 30}}, 30},
		{"keep forever", &Config{Retention: &RetentionConfig{ObserverDays: -1}}, -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.ObserverDaysOrDefault()
			if got != tt.want {
				t.Errorf("ObserverDaysOrDefault() = %d, want %d", got, tt.want)
			}
		})
	}
}

// Issue #1552 — observer health thresholds configurable.

func TestObserverThresholdsOverride(t *testing.T) {
	dir := t.TempDir()
	cfgData := map[string]interface{}{
		"healthThresholds": map[string]interface{}{
			"observerOnlineMinutes": 30,
			"observerStaleMinutes":  120,
		},
	}
	data, _ := json.Marshal(cfgData)
	os.WriteFile(filepath.Join(dir, "config.json"), data, 0644)
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.GetHealthThresholds()
	if h.ObserverOnlineMinutes != 30 {
		t.Errorf("ObserverOnlineMinutes = %d, want 30", h.ObserverOnlineMinutes)
	}
	if h.ObserverStaleMinutes != 120 {
		t.Errorf("ObserverStaleMinutes = %d, want 120", h.ObserverStaleMinutes)
	}
	m := h.ToClientMs()
	if m["observerOnlineMs"] != 30*60*1000 {
		t.Errorf("observerOnlineMs = %d, want %d", m["observerOnlineMs"], 30*60*1000)
	}
	if m["observerStaleMs"] != 120*60*1000 {
		t.Errorf("observerStaleMs = %d, want %d", m["observerStaleMs"], 120*60*1000)
	}
}

func TestObserverThresholdsDefaults(t *testing.T) {
	cfg := &Config{}
	h := cfg.GetHealthThresholds()
	if h.ObserverOnlineMinutes != 60 {
		t.Errorf("default ObserverOnlineMinutes = %d, want 60", h.ObserverOnlineMinutes)
	}
	if h.ObserverStaleMinutes != 1440 {
		t.Errorf("default ObserverStaleMinutes = %d, want 1440", h.ObserverStaleMinutes)
	}
	m := h.ToClientMs()
	if m["observerOnlineMs"] != 3600000 {
		t.Errorf("default observerOnlineMs = %d, want 3600000", m["observerOnlineMs"])
	}
	if m["observerStaleMs"] != 86400000 {
		t.Errorf("default observerStaleMs = %d, want 86400000", m["observerStaleMs"])
	}
}

// Loading a config with no healthThresholds block at all must still produce
// the new 60 / 1440 defaults (not zero, not the old 10 / 60).
func TestObserverThresholdsDefaultsFromEmptyConfigFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"port": 3000}`), 0644)
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.GetHealthThresholds()
	if h.ObserverOnlineMinutes != 60 {
		t.Errorf("empty-config ObserverOnlineMinutes = %d, want 60 (new default)", h.ObserverOnlineMinutes)
	}
	if h.ObserverStaleMinutes != 1440 {
		t.Errorf("empty-config ObserverStaleMinutes = %d, want 1440 (new default)", h.ObserverStaleMinutes)
	}
}

func TestApplyListLimitsDefaults(t *testing.T) {
	t.Run("defaults when block is absent", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"port": 3000}`), 0644)
		cfg, err := LoadConfig(dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ListLimits.PacketsMax != 10000 {
			t.Errorf("expected 10000, got %d", cfg.ListLimits.PacketsMax)
		}
		if cfg.ListLimits.NodesMax != 2000 {
			t.Errorf("expected 2000, got %d", cfg.ListLimits.NodesMax)
		}
		if cfg.ListLimits.AnalyticsMax != 200 {
			t.Errorf("expected 200, got %d", cfg.ListLimits.AnalyticsMax)
		}
		if cfg.ListLimits.ChannelMessagesMax != 500 {
			t.Errorf("expected 500, got %d", cfg.ListLimits.ChannelMessagesMax)
		}
		if cfg.ListLimits.BulkHealthMax != 200 {
			t.Errorf("expected 200, got %d", cfg.ListLimits.BulkHealthMax)
		}
	})

	t.Run("operator overrides honored", func(t *testing.T) {
		dir := t.TempDir()
		cfgData := map[string]interface{}{
			"listLimits": map[string]interface{}{
				"packetsMax":         50000,
				"nodesMax":           5000,
				"analyticsMax":       500,
				"channelMessagesMax": 1000,
				"bulkHealthMax":      300,
			},
		}
		data, _ := json.Marshal(cfgData)
		os.WriteFile(filepath.Join(dir, "config.json"), data, 0644)
		cfg, err := LoadConfig(dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ListLimits.PacketsMax != 50000 {
			t.Errorf("expected 50000, got %d", cfg.ListLimits.PacketsMax)
		}
		if cfg.ListLimits.NodesMax != 5000 {
			t.Errorf("expected 5000, got %d", cfg.ListLimits.NodesMax)
		}
		if cfg.ListLimits.AnalyticsMax != 500 {
			t.Errorf("expected 500, got %d", cfg.ListLimits.AnalyticsMax)
		}
		if cfg.ListLimits.ChannelMessagesMax != 1000 {
			t.Errorf("expected 1000, got %d", cfg.ListLimits.ChannelMessagesMax)
		}
		if cfg.ListLimits.BulkHealthMax != 300 {
			t.Errorf("expected 300, got %d", cfg.ListLimits.BulkHealthMax)
		}
	})
}

func TestAreaForPoint(t *testing.T) {
	f := func(v float64) *float64 { return &v }

	areas := map[string]AreaEntry{
		"DK": {
			Label:  "Danmark (alle)",
			LatMin: f(54.5), LatMax: f(57.8),
			LonMin: f(8.0), LonMax: f(15.25),
		},
		"FYN": {
			Label:  "Fyn",
			LatMin: f(54.9), LatMax: f(55.65),
			LonMin: f(9.85), LonMax: f(11.0),
		},
		"ODE": {
			Label:  "Odense by",
			LatMin: f(55.32), LatMax: f(55.45),
			LonMin: f(10.3), LonMax: f(10.5),
		},
	}

	t.Run("picks the most specific nested area", func(t *testing.T) {
		label, ok := AreaForPoint(55.4047, 10.381, areas) // central Odense
		if !ok || label != "Odense by" {
			t.Errorf("expected Odense by, got %q (ok=%v)", label, ok)
		}
	})

	t.Run("falls back to a broader area when no narrower one matches", func(t *testing.T) {
		label, ok := AreaForPoint(55.0, 10.6, areas) // Fyn but not Odense
		if !ok || label != "Fyn" {
			t.Errorf("expected Fyn, got %q (ok=%v)", label, ok)
		}
	})

	t.Run("no match outside every area", func(t *testing.T) {
		_, ok := AreaForPoint(60.0, 20.0, areas)
		if ok {
			t.Error("expected no match far outside Denmark")
		}
	})

	t.Run("zero coordinates never match", func(t *testing.T) {
		_, ok := AreaForPoint(0, 0, areas)
		if ok {
			t.Error("expected (0,0) to never match an area")
		}
	})

	t.Run("empty areas map", func(t *testing.T) {
		_, ok := AreaForPoint(55.4, 10.4, map[string]AreaEntry{})
		if ok {
			t.Error("expected no match with no configured areas")
		}
	})

	t.Run("AreaKeysForPoint returns every containing area, not just the most specific", func(t *testing.T) {
		keys := AreaKeysForPoint(55.4047, 10.381, areas) // central Odense -- also inside Fyn and DK
		want := map[string]bool{"DK": true, "FYN": true, "ODE": true}
		if len(keys) != len(want) {
			t.Fatalf("got %v, want all 3 of %v", keys, want)
		}
		for _, k := range keys {
			if !want[k] {
				t.Errorf("unexpected key %q in %v", k, keys)
			}
		}
	})

	t.Run("AreaKeysForPoint returns nil outside every area", func(t *testing.T) {
		if keys := AreaKeysForPoint(60.0, 20.0, areas); keys != nil {
			t.Errorf("expected nil, got %v", keys)
		}
	})

	t.Run("AreaKeysForPoint returns nil for zero coordinates", func(t *testing.T) {
		if keys := AreaKeysForPoint(0, 0, areas); keys != nil {
			t.Errorf("expected nil, got %v", keys)
		}
	})
}

// TestAreaForPoint_TieBreaker covers areaMatchForPoint's deterministic
// tie-break rule: when two or more matching areas have the EXACT same
// bounding-box span, the lowest area config key wins (ordinal string
// comparison), regardless of Go's randomized map iteration order. Without
// this, two same-span overlapping areas could classify a point differently
// from one call to the next within the same running process.
func TestAreaForPoint_TieBreaker(t *testing.T) {
	f := func(v float64) *float64 { return &v }

	// Two areas with the exact same bounding-box span (0.5 x 0.5 degrees),
	// both containing the test point below -- the scenario the tie-break
	// exists for.
	tiedAreas := func() map[string]AreaEntry {
		return map[string]AreaEntry{
			"ZZZ": {
				Label:  "Area ZZZ",
				LatMin: f(55.0), LatMax: f(55.5),
				LonMin: f(10.0), LonMax: f(10.5),
			},
			"AAA": {
				Label:  "Area AAA",
				LatMin: f(55.0), LatMax: f(55.5),
				LonMin: f(10.0), LonMax: f(10.5),
			},
		}
	}
	const testLat, testLon = 55.2, 10.2

	t.Run("equal-span tie is broken by the lowest area key, not iteration order", func(t *testing.T) {
		key, ok := AreaKeyForPoint(testLat, testLon, tiedAreas())
		if !ok {
			t.Fatal("expected a match")
		}
		if key != "AAA" {
			t.Errorf("expected the alphabetically lowest key AAA to win an exact-span tie, got %q", key)
		}
	})

	t.Run("tie-break result is stable across repeated calls on the same map", func(t *testing.T) {
		areas := tiedAreas()
		for i := 0; i < 500; i++ {
			key, ok := AreaKeyForPoint(testLat, testLon, areas)
			if !ok || key != "AAA" {
				t.Fatalf("iteration %d: expected AAA, got %q (ok=%v) -- non-deterministic tie-break", i, key, ok)
			}
		}
	})

	t.Run("tie-break result is independent of map construction/insertion order", func(t *testing.T) {
		// Built with the two keys in the opposite literal order from
		// tiedAreas(). Go map iteration order is randomized regardless of
		// construction order, but this pins that the RESULT doesn't depend
		// on it either way.
		base := tiedAreas()
		reordered := map[string]AreaEntry{
			"AAA": base["AAA"],
			"ZZZ": base["ZZZ"],
		}
		key, ok := AreaKeyForPoint(testLat, testLon, reordered)
		if !ok || key != "AAA" {
			t.Errorf("expected AAA regardless of construction order, got %q (ok=%v)", key, ok)
		}
	})

	t.Run("a smaller area still wins over a larger one despite an alphabetically later key", func(t *testing.T) {
		// AAA is deliberately the LARGER area and alphabetically first --
		// proves span comparison dominates the key tie-break, which only
		// applies on an EXACT span match. Existing semantics for
		// differently-sized areas must not change in this fix.
		areas := map[string]AreaEntry{
			"AAA": {
				Label:  "Big AAA",
				LatMin: f(50.0), LatMax: f(60.0),
				LonMin: f(5.0), LonMax: f(15.0),
			},
			"ZZZ": {
				Label:  "Small ZZZ",
				LatMin: f(55.0), LatMax: f(55.5),
				LonMin: f(10.0), LonMax: f(10.5),
			},
		}
		key, ok := AreaKeyForPoint(testLat, testLon, areas)
		if !ok || key != "ZZZ" {
			t.Errorf("expected the smaller area ZZZ to win despite AAA sorting first, got %q (ok=%v)", key, ok)
		}
	})

	t.Run("three-way exact tie still resolves to the lowest key", func(t *testing.T) {
		areas := map[string]AreaEntry{
			"CCC": {Label: "C", LatMin: f(55.0), LatMax: f(55.5), LonMin: f(10.0), LonMax: f(10.5)},
			"BBB": {Label: "B", LatMin: f(55.0), LatMax: f(55.5), LonMin: f(10.0), LonMax: f(10.5)},
			"DDD": {Label: "D", LatMin: f(55.0), LatMax: f(55.5), LonMin: f(10.0), LonMax: f(10.5)},
		}
		key, ok := AreaKeyForPoint(testLat, testLon, areas)
		if !ok || key != "BBB" {
			t.Errorf("expected BBB (lowest of CCC/BBB/DDD), got %q (ok=%v)", key, ok)
		}
	})
}
