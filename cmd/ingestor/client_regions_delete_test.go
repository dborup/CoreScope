package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// admin delete-client-rx also erases a contributor's declared-region reports
// (#126): every client_declared_regions row the key COLLECTED, never the rows
// where the key is only the target.

func seedCollectedReport(t *testing.T, s *Store, collector, target, reportedAt string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO client_declared_regions
		(collector_pubkey, target_pubkey, regions_csv, reported_at, ingested_at, mqtt_source)
		VALUES (?, ?, '#be', ?, '2026-10-14T12:00:00.000Z', 't')`, collector, target, reportedAt); err != nil {
		t.Fatalf("seed report: %v", err)
	}
}

func countCollected(t *testing.T, s *Store, collector string) int64 {
	t.Helper()
	var n int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM client_declared_regions WHERE collector_pubkey=?`, collector).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// seedDeleteFixture: two reports collected by the key, one by its near twin,
// and one where the key is only the target.
func seedDeleteFixture(t *testing.T, s *Store) {
	t.Helper()
	seedCollectedReport(t, s, clientRxTarget, testRegionsTarget, "2026-10-14T10:00:00.000Z")
	seedCollectedReport(t, s, clientRxTarget, testRegionsTarget, "2026-10-14T11:00:00.000Z")
	seedCollectedReport(t, s, clientRxNearTwin, testRegionsTarget, "2026-10-14T10:00:00.000Z")
	seedCollectedReport(t, s, testRegionsOther, clientRxTarget, "2026-10-14T10:00:00.000Z")
}

func TestDeleteClientRxCoversDeclaredRegions(t *testing.T) {
	s, dbPath := openClientRxStore(t)
	seedDeleteFixture(t, s)
	seedRx(t, s, clientRxTarget, "aabb01", "2026-02-01T00:00:00Z")

	id := clientRxNewID()
	if err := clientRxWriteRequest(dbPath, clientRxDeleteRequest{ID: id, RequestedAt: time.Now().UTC(), Pubkey: clientRxTarget}); err != nil {
		t.Fatal(err)
	}
	s.RunPendingClientRxDeletes()

	res, err := clientRxReadResult(dbPath, id)
	if err != nil || res == nil || res.Error != "" {
		t.Fatalf("result: %+v err=%v", res, err)
	}
	if res.ClientDeclaredRegions != 2 || res.ClientReceptions != 1 {
		t.Fatalf("result counts: regions=%d receptions=%d, want 2/1", res.ClientDeclaredRegions, res.ClientReceptions)
	}
	if got := countCollected(t, s, clientRxTarget); got != 0 {
		t.Fatalf("%d reports collected by the erased key remain", got)
	}
	if got := countCollected(t, s, clientRxNearTwin); got != 1 {
		t.Fatalf("near twin's report must survive (exact match only), got %d", got)
	}
	var asTarget int
	s.db.QueryRow(`SELECT COUNT(*) FROM client_declared_regions WHERE target_pubkey=?`, clientRxTarget).Scan(&asTarget)
	if asTarget != 1 {
		t.Fatalf("a report where the key is only the target must survive, got %d", asTarget)
	}
}

func TestDeleteClientDeclaredRegionsBatches(t *testing.T) {
	s, _ := openClientRxStore(t)
	orig := deleteClientRxBatchSize
	deleteClientRxBatchSize = 2
	t.Cleanup(func() { deleteClientRxBatchSize = orig })
	for i := 0; i < 5; i++ {
		seedCollectedReport(t, s, clientRxTarget, testRegionsTarget, time.Date(2026, 10, 1, 0, 0, i, 0, time.UTC).Format(clientRegionsTimeLayout))
	}
	n, err := s.DeleteClientDeclaredRegionsByCollector(clientRxTarget)
	if err != nil || n != 5 || countCollected(t, s, clientRxTarget) != 0 {
		t.Fatalf("deleted %d (err %v), left %d; want all 5 across batches", n, err, countCollected(t, s, clientRxTarget))
	}
}

func TestDeleteClientDeclaredRegionsUsesCollectorIndex(t *testing.T) {
	s, _ := openClientRxStore(t)
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+clientRegionsDeleteSQL, clientRxTarget, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		rows.Scan(&id, &parent, &notused, &detail)
		plan = append(plan, detail)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "idx_cdr_collector") {
		t.Fatalf("erasure must seek idx_cdr_collector, plan:\n%s", strings.Join(plan, "\n"))
	}
}

// The audit record carries the count, never the key.
func TestDeleteClientRxAuditCountsDeclaredRegions(t *testing.T) {
	s, dbPath := openClientRxStore(t)
	seedDeleteFixture(t, s)
	s.processClientRxDelete(&clientRxDeleteRequest{ID: clientRxNewID(), RequestedAt: time.Now().UTC(), Pubkey: clientRxTarget})
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(dbPath), clientRxAuditFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"clientDeclaredRegions":2`) {
		t.Fatalf("audit line lacks the declared-regions count:\n%s", raw)
	}
	if strings.Contains(string(raw), clientRxTarget[:16]) {
		t.Fatalf("audit line leaks the key:\n%s", raw)
	}
}

func TestClientRxDryRunCountsDeclaredRegions(t *testing.T) {
	s, dbPath := openClientRxStore(t)
	seedDeleteFixture(t, s)
	db, err := sql.Open("sqlite", clientRxReadOnlyDSN(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	n, err := clientDeclaredRegionsCount(db, clientRxTarget)
	if err != nil || n != 2 {
		t.Fatalf("dry-run count = %d (err %v), want 2", n, err)
	}
	if code := runAdminDeleteClientRxDryRun(dbPath, clientRxTarget); code != 0 {
		t.Fatalf("dry-run exit code %d", code)
	}
	if got := countCollected(t, s, clientRxTarget); got != 2 {
		t.Fatalf("dry-run must not delete; %d left", got)
	}
}

// A legacy DB the new ingestor has not opened yet has no
// client_declared_regions table; the read-only dry-run must still work.
func TestClientRxDryRunOnLegacyDBWithoutRegionsTable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, ddl := range []string{
		`CREATE TABLE client_receptions (id INTEGER PRIMARY KEY, rx_pubkey TEXT, heard_key TEXT, rx_at TEXT)`,
		`CREATE TABLE client_observers (pubkey TEXT PRIMARY KEY, name TEXT, last_seen TEXT)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	ro, err := sql.Open("sqlite", clientRxReadOnlyDSN(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if n, err := clientDeclaredRegionsCount(ro, clientRxTarget); err != nil || n != 0 {
		t.Fatalf("legacy DB: count=%d err=%v, want 0 and no error", n, err)
	}
	if code := runAdminDeleteClientRxDryRun(dbPath, clientRxTarget); code != 0 {
		t.Fatalf("dry-run on a legacy DB: exit code %d", code)
	}
}
