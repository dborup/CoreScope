package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// benchReachVisibilityDB builds a file-backed DB where a target repeater has
// `neighbours` links (half heard both ways) plus 5 direct observers — the Reach
// report a cache hit re-checks for visibility. Uses only APIs that also exist
// on master so the same benchmark runs before/after.
func benchReachVisibilityDB(b *testing.B, neighbours int) (*DB, string) {
	b.Helper()
	conn, err := sql.Open("sqlite", filepath.Join(b.TempDir(), "vis.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { conn.Close() })
	for _, s := range []string{
		`CREATE TABLE nodes (public_key TEXT PRIMARY KEY, name TEXT, role TEXT, lat REAL, lon REAL, last_seen TEXT, first_seen TEXT, advert_count INTEGER DEFAULT 0, battery_mv INTEGER, temperature_c REAL, foreign_advert INTEGER DEFAULT 0)`,
		`CREATE TABLE observers (id TEXT PRIMARY KEY, name TEXT)`,
		`CREATE TABLE transmissions (id INTEGER PRIMARY KEY, from_pubkey TEXT, payload_type INTEGER)`,
		`CREATE TABLE observations (id INTEGER PRIMARY KEY, transmission_id INTEGER, observer_idx INTEGER, snr REAL, path_json TEXT, timestamp INTEGER)`,
		`CREATE TABLE neighbor_edges (node_a TEXT NOT NULL, node_b TEXT NOT NULL, count INTEGER DEFAULT 1, last_seen TEXT, PRIMARY KEY (node_a, node_b))`,
		`CREATE INDEX idx_obs_ts ON observations(timestamp)`,
	} {
		if _, err := conn.Exec(s); err != nil {
			b.Fatal(err)
		}
	}
	target := pk64("01fa")
	tx, err := conn.Begin()
	if err != nil {
		b.Fatal(err)
	}
	ins := func(q string, args ...interface{}) {
		if _, err := tx.Exec(q, args...); err != nil {
			b.Fatal(err)
		}
	}
	ins(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen) VALUES (?, 'Target', 'repeater', 56.1, 10.2, '2026-09-01T00:00:00Z', '2026-06-01T00:00:00Z')`, target)
	for i := 0; i < 5; i++ {
		ins(`INSERT INTO observers (id, name) VALUES (?, ?)`, strings.ToUpper(pk64(fmt.Sprintf("0b%02x", i))), fmt.Sprintf("Observer %d", i))
	}
	now := time.Now().Unix()
	id := 0
	for i := 0; i < neighbours; i++ {
		tok := fmt.Sprintf("%04x", 0x2000+i)
		ins(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen) VALUES (?, ?, 'repeater', 56.2, 10.3, '2026-09-01T00:00:00Z', '2026-06-01T00:00:00Z')`,
			pk64(tok), fmt.Sprintf("Neighbour %d", i))
		paths := []string{`["` + strings.ToUpper(tok) + `","01FA"]`}
		if i%2 == 0 {
			paths = append(paths, `["01FA","`+strings.ToUpper(tok)+`"]`)
		}
		for _, p := range paths {
			id++
			ins(`INSERT INTO transmissions (id, from_pubkey, payload_type) VALUES (?, '', 5)`, id)
			ins(`INSERT INTO observations (id, transmission_id, observer_idx, snr, path_json, timestamp) VALUES (?, ?, ?, -7.0, ?, ?)`, id, id, 1+id%5, p, now)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	db := &DB{conn: conn}
	db.isV3Flag.forceTrue()
	return db, target
}

// BenchmarkNodeReachCacheHitVisibility: /api/nodes/{pk}/reach served from the
// response cache for a node with 50 links, with and without a configured
// hidden-name prefix (only then do names need a live DB lookup).
func BenchmarkNodeReachCacheHitVisibility(b *testing.B) {
	for _, c := range []struct {
		name     string
		prefixes []string
	}{{"prefixes_configured", []string{"🚫"}}, {"no_prefixes", nil}} {
		b.Run(c.name, func(b *testing.B) {
			db, target := benchReachVisibilityDB(b, 50)
			cfg := &Config{HiddenNamePrefixes: c.prefixes}
			srv := &Server{store: &PacketStore{db: db}, db: db, cfg: cfg, perfStats: NewPerfStats()}
			path := "/api/nodes/" + target + "/reach?days=7"
			if rr := serveReach(srv, path); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Neighbour 49") {
				b.Fatalf("warm-up: %d %.200s", rr.Code, rr.Body.String())
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if rr := serveReach(srv, path); rr.Code != http.StatusOK {
					b.Fatalf("status %d", rr.Code)
				}
			}
		})
	}
}
