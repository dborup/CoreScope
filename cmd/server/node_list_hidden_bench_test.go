package main

import (
	"fmt"
	"net/http/httptest"
	"testing"
)

// BenchmarkNodeListPostFiltersHidden measures the /api/nodes post-filter over
// 2,000 rows with a blacklist and a hidden prefix configured (#1005 moved the
// identity check into nodeListHidden). Compiles on master as well, so the two
// can be compared like for like.
func BenchmarkNodeListPostFiltersHidden(b *testing.B) {
	cfg := &Config{NodeBlacklist: []string{"pk00000010", "pk00000020"}, HiddenNamePrefixes: []string{"🚫"}}
	rows := make([]map[string]interface{}, 2000)
	for i := range rows {
		name := fmt.Sprintf("Node %d", i)
		if i%50 == 0 {
			name = "🚫" + name
		}
		rows[i] = map[string]interface{}{"public_key": fmt.Sprintf("pk%08d", i), "name": name}
	}
	f := nodeListPostFilters{cfg: cfg}
	buf := make([]map[string]interface{}, len(rows))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copy(buf, rows)
		if got := len(f.apply(buf)); got != 2000-40-2 {
			b.Fatalf("kept %d rows", got)
		}
	}
}

// BenchmarkAreaAnalyticsColdHidden measures one uncached /api/analytics/areas
// compute over 2,000 nodes (500 without a fix, each with 3 neighbour edges)
// with a blacklist and a hidden prefix configured. Compiles on master too.
func BenchmarkAreaAnalyticsColdHidden(b *testing.B) {
	db := setupTestDB(b)
	f := func(v float64) *float64 { return &v }
	cfg := &Config{NodeBlacklist: []string{"pk00000010"}, HiddenNamePrefixes: []string{"🚫"}}
	cfg.Areas = map[string]AreaEntry{"DK": {Label: "DK", LatMin: f(54.5), LatMax: f(57.8), LonMin: f(8.0), LonMax: f(15.2)}}
	tx, _ := db.conn.Begin()
	for i := 0; i < 2000; i++ {
		name := fmt.Sprintf("Node %d", i)
		if i%50 == 0 {
			name = "🚫" + name
		}
		var lat, lon interface{} = 55.0 + float64(i%100)/100, 10.0 + float64(i/100)/10
		if i%4 == 0 {
			lat, lon = nil, nil
			for k := 1; k <= 3; k++ {
				tx.Exec("INSERT OR IGNORE INTO neighbor_edges (node_a, node_b, count) VALUES (?, ?, ?)", fmt.Sprintf("pk%08d", i), fmt.Sprintf("pk%08d", (i+k)%2000), k)
			}
		}
		tx.Exec("INSERT INTO nodes (public_key, name, role, last_seen, lat, lon) VALUES (?, ?, 'repeater', '2026-11-28T00:00:00Z', ?, ?)", fmt.Sprintf("pk%08d", i), name, lat, lon)
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	s := NewServer(db, cfg, NewHub())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.areaAnalyticsMu.Lock()
		s.areaAnalyticsCache = nil
		s.areaAnalyticsMu.Unlock()
		s.handleAreaAnalytics(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/analytics/areas", nil))
	}
}
