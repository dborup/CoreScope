package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dbschema"
)

// BenchmarkNetworkStatus883 is the #883 M2 perf proof for the Live kiosk's
// "active nodes" tile: each visible kiosk screen reads
// GET /api/nodes/network-status once per 60 s, and that handler is not
// cached (owner decision 9), so every call runs db.GetNetworkStatus: one
// scan of the nodes table plus the observer reports. Measured on the e2e
// fixture (migrated on a temp copy, as CI's corescope-migrate step does) and
// on 2,000 nodes (the AGENTS.md scale).
//
//	cd cmd/server && go test -run '^$' -bench NetworkStatus883 -benchmem
func BenchmarkNetworkStatus883(b *testing.B) {
	ht := (&Config{}).GetHealthThresholds()
	b.Run("fixture", func(b *testing.B) {
		path := rpm2CopyFile(b, filepath.Join("..", "..", "test-fixtures", "e2e-fixture.db"))
		rw, err := sql.Open("sqlite", path)
		if err != nil {
			b.Fatal(err)
		}
		err = dbschema.Apply(rw, func(string, ...interface{}) {})
		rw.Close()
		if err != nil {
			b.Fatal(err)
		}
		db, err := OpenDB(path)
		if err != nil {
			b.Fatal(err)
		}
		defer db.Close()
		benchNetworkStatus(b, db, ht)
	})
	b.Run("nodes=2000", func(b *testing.B) {
		db := setupTestDB(b)
		defer db.Close()
		now := time.Now().UTC()
		roles := []string{"repeater", "companion", "room", "sensor"}
		for i := 0; i < 2000; i++ {
			seen := now.Add(-time.Duration(i%96) * time.Hour).Format(time.RFC3339)
			if _, err := db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen)
				VALUES (?, ?, ?, 55.0, 10.0, ?, '2026-01-01T00:00:00Z')`,
				fmt.Sprintf("%064x", i), fmt.Sprintf("node-%d", i), roles[i%len(roles)], seen); err != nil {
				b.Fatal(err)
			}
		}
		benchNetworkStatus(b, db, ht)
	})
}

func benchNetworkStatus(b *testing.B, db *DB, ht HealthThresholds) {
	r, err := db.GetNetworkStatus(ht, nil)
	if err != nil {
		b.Fatal(err)
	}
	if total, _ := r["total"].(int); total == 0 {
		b.Fatal("no nodes")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.GetNetworkStatus(ht, nil); err != nil {
			b.Fatal(err)
		}
	}
}
