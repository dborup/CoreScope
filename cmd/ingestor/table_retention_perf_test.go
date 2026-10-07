package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestTableRetentionTiming measures the #329 table retention on a multi-GB
// synthetic DB next to today's daily prunes, and counts the [db-slow-writer]
// lines each emits (default 500 ms threshold) while a probe writer stands in
// for MQTT ingest. Fixture: 365 days of history; 3M transmissions (30k a day
// for the last 100 days) with four observations each; 20k inactive_nodes
// (2k of them nodes that came back), 500k node_changes, 50k ping_triggers,
// 600 observers (300 soft-deleted, half of those still with metrics) with
// metrics and neighbour rows. The windows are 30 days, the operator's
// setting. Three passes, as in production order (transmissions and metrics
// first, then the table retention): the first run after enabling (335 days
// of backlog), the next day (one more day ages out) and a run with nothing to
// delete. Opt-in, as seeding takes several minutes and several GB of disk:
//
//	CORESCOPE_RETENTION_PERF=1 go test -run TestTableRetentionTiming -v -timeout 60m .
//
// CORESCOPE_RETENTION_PERF_DIR puts the DB somewhere other than t.TempDir().
func TestTableRetentionTiming(t *testing.T) {
	if os.Getenv("CORESCOPE_RETENTION_PERF") == "" {
		t.Skip("set CORESCOPE_RETENTION_PERF=1 to run the table retention timing on a multi-GB fixture")
	}
	dir := os.Getenv("CORESCOPE_RETENTION_PERF_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	path := filepath.Join(dir, "retention-perf.db")
	os.Remove(path)
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close(); os.Remove(path) })
	store.WaitForAsyncMigrations()
	seedTableRetentionPerf(t, store)
	if fi, err := os.Stat(path); err == nil {
		t.Logf("DB size %.2f GB", float64(fi.Size())/1e9)
	}

	var logBuf syncBuffer
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	type step struct {
		label string
		run   func() error
	}
	today := func(days int) step {
		return step{"today's prunes", func() error {
			if _, err := store.PruneTransmissions(days, 0); err != nil {
				return err
			}
			if _, err := store.PruneOldMetrics(days); err != nil {
				return err
			}
			if _, err := store.PruneOldNeighborMetrics(days); err != nil {
				return err
			}
			if _, err := store.PruneDroppedPackets(days); err != nil {
				return err
			}
			_, err := store.RemoveStaleObservers(14)
			return err
		}}
	}
	tables := func(days int) step {
		return step{"#329 table retention", func() error {
			runTableRetention(store, TableRetention{InactiveNodeDays: days, NodeChangeDays: days, PingTriggerDays: days, ObserverPurgeDays: days}, "perf")
			if strings.Contains(logBuf.String(), "table retention error") {
				return fmt.Errorf("%s", logBuf.String())
			}
			return nil
		}}
	}
	components := []string{"prune_packets", "prune_metrics", "prune_observers",
		"prune_inactive_nodes", "prune_node_changes", "prune_ping_triggers", "purge_observers"}

	for _, pass := range []struct {
		name string
		days int
	}{{"first run (30)", 30}, {"next day (29)", 29}, {"nothing to delete (29)", 29}} {
		for _, st := range []step{today(pass.days), tables(pass.days)} {
			counts := map[string]int{}
			for _, tbl := range []string{"transmissions", "inactive_nodes", "node_changes", "ping_triggers", "observers"} {
				counts[tbl] = countRows(t, store, tbl)
			}
			ResetWriterStatsForTest()
			logBuf.Reset()
			stop, probeWaitMax := startIngestProbe(store)
			start := time.Now()
			if err := st.run(); err != nil {
				t.Fatalf("%s %s: %v", pass.name, st.label, err)
			}
			elapsed := time.Since(start)
			stop()
			stats := store.WriterStatsSnapshot()
			var parts []string
			for _, c := range components {
				if s, ok := stats[c]; ok {
					parts = append(parts, fmt.Sprintf("%-21s %5d tx  hold max %7.1fms  p99 %7.1fms", c, s.Count, s.HoldMsMax, s.HoldMsP99))
				}
			}
			var deleted []string
			for _, tbl := range []string{"transmissions", "inactive_nodes", "node_changes", "ping_triggers", "observers"} {
				if d := counts[tbl] - countRows(t, store, tbl); d > 0 {
					deleted = append(deleted, fmt.Sprintf("%s=%d", tbl, d))
				}
			}
			slow := strings.Count(logBuf.String(), "[db-slow-writer]")
			t.Logf("%-22s %-20s %9v  [db-slow-writer] lines=%d  ingest probe wait max %.1fms  deleted[%s]\n    %s",
				pass.name, st.label, elapsed.Round(time.Millisecond), slow, *probeWaitMax,
				strings.Join(deleted, " "), strings.Join(parts, "\n    "))
			if slow > 0 {
				for _, l := range strings.Split(logBuf.String(), "\n") {
					if strings.Contains(l, "[db-slow-writer]") {
						t.Logf("      %s", l)
					}
				}
			}
		}
	}
}

// startIngestProbe writes one small row through WriterTx every 5 ms, as MQTT
// ingest would, and records the longest wait for the writer lock.
func startIngestProbe(store *Store) (stop func(), waitMax *float64) {
	var max float64
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			case <-time.After(5 * time.Millisecond):
			}
			start := time.Now()
			_ = store.WriterTx("probe_ingest", func(tx *sql.Tx) error {
				if w := float64(time.Since(start).Microseconds()) / 1000; w > max {
					max = w
				}
				_, err := tx.Exec(`INSERT OR REPLACE INTO _migrations (name) VALUES ('perf-probe')`)
				return err
			})
		}
	}()
	return func() { close(done); wg.Wait() }, &max
}

func seedTableRetentionPerf(t *testing.T, store *Store) {
	t.Helper()
	start := time.Now()
	now := time.Now().UTC().Format(time.RFC3339)
	exec := func(label, q string, args ...any) {
		t.Helper()
		s := time.Now()
		if _, err := store.db.Exec(q, args...); err != nil {
			t.Fatalf("seed %s: %v", label, err)
		}
		t.Logf("seeded %-20s in %v", label, time.Since(s).Round(time.Millisecond))
	}
	// 3M transmissions over the last 100 days with realistic payload sizes.
	exec("transmissions", `
		WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i + 1 < 3000000),
		     r(i, ts) AS (SELECT i, strftime('%Y-%m-%dT%H:%M:%SZ', ?1, '-' || (i * 86400 / 30000) || ' seconds') FROM n)
		INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, payload_version, decoded_json, last_seen)
		SELECT hex(randomblob(110)), 'perf-' || i, ts, 1, CASE i % 3 WHEN 0 THEN 5 WHEN 1 THEN 4 ELSE 2 END, 1,
		       '{"type":"ADVERT","pubKey":"' || hex(randomblob(32)) || '","name":"node-' || (i % 3000) || '","lat":55.6,"lon":12.5,"flags":{"repeater":true},"pad":"' || hex(randomblob(90)) || '"}',
		       CAST(strftime('%s', ts) AS INTEGER)
		FROM r`, now)
	exec("observations", `
		WITH RECURSIVE k(j) AS (SELECT 0 UNION ALL SELECT j + 1 FROM k WHERE j + 1 < 4)
		INSERT INTO observations (transmission_id, observer_idx, direction, snr, rssi, score, path_json, timestamp)
		SELECT t.id, 1 + (t.id * 7 + k.j) % 300, 'rx', 5.5, -100, 0, '["a1b2","c3d4","e5f6","0718"]', t.last_seen + k.j FROM transmissions t, k`)
	// Nodes: 3000 active, 20k inactive over 365 days, 2k of them came back.
	exec("nodes", `
		WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i + 1 < 3000)
		INSERT INTO nodes (public_key, name, role, last_seen, first_seen, advert_count)
		SELECT printf('%064x', i), 'node-' || i, 'repeater', ?1, ?1, 10 FROM n`, now)
	exec("inactive_nodes", `
		WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i + 1 < 20000)
		INSERT INTO inactive_nodes (public_key, name, role, last_seen, first_seen, advert_count)
		SELECT printf('%064x', CASE WHEN i < 2000 THEN i ELSE 100000 + i END), 'gone-' || i, 'companion',
		       strftime('%Y-%m-%dT%H:%M:%SZ', ?1, '-' || (8 * 86400 + i * 357 * 86400 / 20000) || ' seconds'), ?1, 3 FROM n`, now)
	exec("node_changes", `
		WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i + 1 < 500000)
		INSERT INTO node_changes (public_key, change_type, old_value, new_value, detected_at)
		SELECT printf('%064x', i % 23000), CASE i % 3 WHEN 0 THEN 'name' WHEN 1 THEN 'role' ELSE 'position' END, 'old-' || i, 'new-' || i,
		       strftime('%Y-%m-%dT%H:%M:%SZ', ?1, '-' || (i * 365 * 86400 / 500000) || ' seconds') FROM n`, now)
	exec("ping_triggers", `
		WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i + 1 < 50000)
		INSERT INTO ping_triggers (tx_id, hash, channel_hash, sender, first_seen)
		SELECT 10000000 - i * 60, 'ping-' || i, '#ping', 'sender-' || (i % 500),
		       strftime('%Y-%m-%dT%H:%M:%SZ', ?1, '-' || (i * 365 * 86400 / 50000) || ' seconds') FROM n`, now)
	// 600 observers, 1..300 referenced by observations; 301..600 soft-deleted
	// 60-200 days ago, every second one with metrics still in the window.
	exec("observers", `
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i + 1 <= 600)
		INSERT INTO observers (rowid, id, name, iata, last_seen, first_seen, packet_count, inactive)
		SELECT i, printf('OBS%061d', i), 'obs-' || i, 'CPH',
		       CASE WHEN i <= 300 THEN ?1 ELSE strftime('%Y-%m-%dT%H:%M:%SZ', ?1, '-' || (60 + i % 140) || ' days') END,
		       ?1, 100, CASE WHEN i <= 300 THEN 0 ELSE 1 END FROM n`, now)
	exec("observer_metrics", `
		WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i + 1 < 200000)
		INSERT OR IGNORE INTO observer_metrics (observer_id, timestamp, noise_floor)
		SELECT printf('OBS%061d', CASE WHEN i % 4 = 0 THEN 301 + 2 * (i % 150) ELSE 1 + i % 300 END),
		       strftime('%Y-%m-%dT%H:%M:%SZ', ?1, '-' || (i * 60 * 86400 / 200000) || ' seconds'), -110 FROM n`, now)
	exec("observer_neighbors", `
		WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i + 1 < 12000)
		INSERT OR IGNORE INTO observer_neighbors (observer_id, neighbor_pubkey, status, reported_at)
		SELECT printf('OBS%061d', 1 + i % 600), printf('%064x', i), 'ok', ?1 FROM n`, now)
	if _, err := store.db.Exec(`ANALYZE`); err != nil {
		t.Fatal(err)
	}
	t.Logf("seeded fixture in %v", time.Since(start).Round(time.Second))
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }
func (s *syncBuffer) Reset()         { s.mu.Lock(); defer s.mu.Unlock(); s.b.Reset() }
