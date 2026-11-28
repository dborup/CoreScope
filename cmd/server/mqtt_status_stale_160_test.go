package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mqttStatusFor serves /api/mqtt/status from a stats file holding
// sampledAt (none when sampledAt is nil) and returns the decoded body.
func mqttStatusFor(t *testing.T, sampledAt *string) map[string]any {
	t.Helper()
	statsPath := filepath.Join(t.TempDir(), "ingestor-stats.json")
	t.Setenv("CORESCOPE_INGESTOR_STATS", statsPath)
	if sampledAt != nil {
		data, err := json.Marshal(map[string]any{"sampledAt": *sampledAt, "source_statuses": []any{}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(statsPath, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	(&Server{}).handleMqttStatus(rec, httptest.NewRequest(http.MethodGet, "/api/mqtt/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d; body %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body %s", err, rec.Body)
	}
	return body
}

func stamp(ts time.Time) *string {
	s := ts.UTC().Format(time.RFC3339)
	return &s
}

// When the ingestor stops updating the stats file (#160: a tmp it cannot
// write), /api/mqtt/status marks the frozen data as stale and gives its
// age. The rule is the one /api/perf/io applies to the same file.
func TestMqttStatusMarksStaleStatsFile_160(t *testing.T) {
	old := mqttStatusFor(t, stamp(time.Now().Add(-time.Hour)))
	if old["stale"] != true {
		t.Errorf("hour-old sample: stale = %v, want true; body %v", old["stale"], old)
	}
	if age, _ := old["sampleAgeSec"].(float64); age < 3600 || age > 3700 {
		t.Errorf("hour-old sample: sampleAgeSec = %v, want ~3600", old["sampleAgeSec"])
	}

	justPast := mqttStatusFor(t, stamp(time.Now().Add(-2*IngestorStatsStaleThreshold)))
	if justPast["stale"] != true {
		t.Errorf("sample 2x the /api/perf/io threshold old: stale = %v, want true", justPast["stale"])
	}

	fresh := mqttStatusFor(t, stamp(time.Now()))
	if fresh["stale"] != false {
		t.Errorf("fresh sample: stale = %v, want false; body %v", fresh["stale"], fresh)
	}
	if age, ok := fresh["sampleAgeSec"].(float64); !ok || age > 2 {
		t.Errorf("fresh sample: sampleAgeSec = %v, want ~0", fresh["sampleAgeSec"])
	}

	// A stamp that cannot be read cannot vouch for fresh data.
	bad := "not-a-time"
	garbled := mqttStatusFor(t, &bad)
	if garbled["stale"] != true {
		t.Errorf("unparseable sampledAt: stale = %v, want true", garbled["stale"])
	}
	if _, ok := garbled["sampleAgeSec"]; ok {
		t.Errorf("unparseable sampledAt: sampleAgeSec present: %v", garbled["sampleAgeSec"])
	}

	// No stats file: no data, which the empty sampleAt already says.
	none := mqttStatusFor(t, nil)
	if none["stale"] != false {
		t.Errorf("no stats file: stale = %v, want false", none["stale"])
	}
}
