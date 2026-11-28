package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// target and nearTwin share a 62-char prefix (differ only in the last 2 hex),
// so any prefix/LIKE match would catch both. Exactly 64 lowercase hex.
const (
	clientRxTarget   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa11"
	clientRxNearTwin = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa22"
)

func openClientRxStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store, dbPath
}

func seedRx(t *testing.T, s *Store, rxPubkey, heardKey, rxAt string) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO client_receptions (rx_pubkey, heard_key, heard_keylen, lat, lon, rx_at, ingested_at, src)
		 VALUES (?,?,3,51.0,3.7,?, '2026-01-01T00:00:00Z','rxlog')`,
		rxPubkey, heardKey, rxAt); err != nil {
		t.Fatalf("seed reception: %v", err)
	}
}

func countRx(t *testing.T, s *Store, rxPubkey string) int64 {
	t.Helper()
	var n int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM client_receptions WHERE rx_pubkey=?`, rxPubkey).Scan(&n); err != nil {
		t.Fatalf("count reception: %v", err)
	}
	return n
}

func countObs(t *testing.T, s *Store, pubkey string) int64 {
	t.Helper()
	var n int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM client_observers WHERE pubkey=?`, pubkey).Scan(&n); err != nil {
		t.Fatalf("count observer: %v", err)
	}
	return n
}

// Only the exact target is deleted; a near-twin with the same 62-char prefix
// survives. Kills: WHERE clause dropped, LIKE/prefix match.
func TestDeleteClientRxOnlyTarget(t *testing.T) {
	s, _ := openClientRxStore(t)
	seedRx(t, s, clientRxTarget, "aabb01", "2026-02-01T00:00:00Z")
	seedRx(t, s, clientRxTarget, "aabb02", "2026-02-02T00:00:00Z")
	seedRx(t, s, clientRxNearTwin, "aabb03", "2026-02-03T00:00:00Z")

	rec, _, err := s.DeleteClientRxByPubkey(clientRxTarget)
	if err != nil {
		t.Fatal(err)
	}
	if rec != 2 {
		t.Fatalf("deleted receptions: want 2, got %d", rec)
	}
	if got := countRx(t, s, clientRxTarget); got != 0 {
		t.Fatalf("target rows remain: %d", got)
	}
	if got := countRx(t, s, clientRxNearTwin); got != 1 {
		t.Fatalf("near-twin must survive, got %d", got)
	}
}

// Both tables cleared. Kills: client_observers delete missing.
func TestDeleteClientRxBothTablesCleared(t *testing.T) {
	s, _ := openClientRxStore(t)
	seedRx(t, s, clientRxTarget, "aabb01", "2026-02-01T00:00:00Z")
	if err := s.UpsertClientObserver(clientRxTarget, "Mobile", "2026-02-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if countObs(t, s, clientRxTarget) != 1 {
		t.Fatal("observer not seeded")
	}

	rec, obs, err := s.DeleteClientRxByPubkey(clientRxTarget)
	if err != nil {
		t.Fatal(err)
	}
	if rec != 1 || obs != 1 {
		t.Fatalf("counts: want rec=1 obs=1, got rec=%d obs=%d", rec, obs)
	}
	if countRx(t, s, clientRxTarget) != 0 || countObs(t, s, clientRxTarget) != 0 {
		t.Fatal("both tables must be empty for target")
	}
}

// Running twice is safe: the second run reports 0/0.
func TestDeleteClientRxTwiceSafe(t *testing.T) {
	s, _ := openClientRxStore(t)
	seedRx(t, s, clientRxTarget, "aabb01", "2026-02-01T00:00:00Z")
	if err := s.UpsertClientObserver(clientRxTarget, "Mobile", "2026-02-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DeleteClientRxByPubkey(clientRxTarget); err != nil {
		t.Fatal(err)
	}
	rec, obs, err := s.DeleteClientRxByPubkey(clientRxTarget)
	if err != nil {
		t.Fatal(err)
	}
	if rec != 0 || obs != 0 {
		t.Fatalf("second run: want 0/0, got rec=%d obs=%d", rec, obs)
	}
}

// Batching loop runs more than once. Kills: only a single batch runs.
func TestDeleteClientRxBatchingLoop(t *testing.T) {
	s, _ := openClientRxStore(t)
	orig := deleteClientRxBatchSize
	deleteClientRxBatchSize = 2
	t.Cleanup(func() { deleteClientRxBatchSize = orig })

	for i := 0; i < 5; i++ {
		seedRx(t, s, clientRxTarget, fmt.Sprintf("aabb%02d", i), fmt.Sprintf("2026-02-%02dT00:00:00Z", i+1))
	}
	rec, _, err := s.DeleteClientRxByPubkey(clientRxTarget)
	if err != nil {
		t.Fatal(err)
	}
	if rec != 5 {
		t.Fatalf("want 5 deleted across batches, got %d", rec)
	}
	if countRx(t, s, clientRxTarget) != 0 {
		t.Fatal("rows remain after batched delete")
	}
}

// EXPLAIN proves the delete seeks via the unique index with rx_pubkey=?, not a
// scan. Kills: LIKE/prefix match, and (indirectly) the 2–64 regex reuse.
func TestDeleteClientRxExplainUsesUniqueIndex(t *testing.T) {
	s, _ := openClientRxStore(t)
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+clientRxDeleteReceptionsSQL,
		clientRxTarget, deleteClientRxBatchSize)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var a, b, c int
		var detail string
		if err := rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail)
		plan.WriteString("\n")
	}
	if !strings.Contains(plan.String(), "sqlite_autoindex_client_receptions_1 (rx_pubkey=?)") {
		t.Fatalf("plan must seek the unique index on rx_pubkey; got:\n%s", plan.String())
	}
}

// Writer timing records the delete under its component label. Kills: direct
// Exec used instead of WriterTx.
func TestDeleteClientRxWriterTiming(t *testing.T) {
	s, _ := openClientRxStore(t)
	seedRx(t, s, clientRxTarget, "aabb01", "2026-02-01T00:00:00Z")
	ResetWriterStatsForTest()
	if _, _, err := s.DeleteClientRxByPubkey(clientRxTarget); err != nil {
		t.Fatal(err)
	}
	snap := s.WriterStatsSnapshot()
	st, ok := snap[deleteClientRxComponent]
	if !ok || st.Count < 1 {
		t.Fatalf("writer timing missing %q: %+v", deleteClientRxComponent, snap)
	}
}

// Queue round trip: a result exists, the request is gone, and no key appears
// in the result file. Kills: request file not removed, key in result file.
func TestClientRxQueueRoundTrip(t *testing.T) {
	s, dbPath := openClientRxStore(t)
	seedRx(t, s, clientRxTarget, "aabb01", "2026-02-01T00:00:00Z")
	if err := s.UpsertClientObserver(clientRxTarget, "Mobile", "2026-02-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}

	id := clientRxNewID()
	if err := clientRxWriteRequest(dbPath, clientRxDeleteRequest{
		ID: id, RequestedAt: time.Now().UTC(), Pubkey: clientRxTarget,
	}); err != nil {
		t.Fatal(err)
	}

	s.RunPendingClientRxDeletes()

	if exists, _ := clientRxRequestExists(dbPath, id); exists {
		t.Error("request file must be consumed")
	}
	res, err := clientRxReadResult(dbPath, id)
	if err != nil || res == nil {
		t.Fatalf("ReadResult: res=%v err=%v", res, err)
	}
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if res.ClientReceptions != 1 || res.ClientObservers != 1 {
		t.Fatalf("result counts: rec=%d obs=%d", res.ClientReceptions, res.ClientObservers)
	}

	rp, _ := clientRxResultPath(dbPath, id)
	raw, err := os.ReadFile(rp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), clientRxTarget) || strings.Contains(string(raw), clientRxTarget[:16]) {
		t.Fatalf("result file must not contain the key or a prefix:\n%s", raw)
	}
}

// The audit log line contains neither the key nor its prefix — only hmac:.
// Kills: key or its prefix is logged.
func TestClientRxLogNoKey(t *testing.T) {
	s, _ := openClientRxStore(t)
	seedRx(t, s, clientRxTarget, "aabb01", "2026-02-01T00:00:00Z")

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })

	s.processClientRxDelete(&clientRxDeleteRequest{
		ID: clientRxNewID(), RequestedAt: time.Now().UTC(), Pubkey: clientRxTarget,
	})

	out := buf.String()
	if !strings.Contains(out, "key=hmac:") {
		t.Fatalf("log must carry a hmac fingerprint; got:\n%s", out)
	}
	if strings.Contains(out, clientRxTarget) || strings.Contains(out, clientRxTarget[:16]) {
		t.Fatalf("log leaked the key or a prefix:\n%s", out)
	}
}

// The HMAC changes when the salt changes. Kills: unsalted hash.
func TestClientRxHMACChangesWithSalt(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	h1 := clientRxKeyHMAC(dbPath, clientRxTarget)
	if h1 == "unavailable" || len(h1) != 16 {
		t.Fatalf("bad hmac: %q", h1)
	}
	// Replace the salt and recompute: a salted HMAC must change.
	if err := os.Remove(clientRxSaltPath(dbPath)); err != nil {
		t.Fatal(err)
	}
	h2 := clientRxKeyHMAC(dbPath, clientRxTarget)
	if h1 == h2 {
		t.Fatalf("hmac must change with the salt (unsalted hash?): %q == %q", h1, h2)
	}
}

// Dry-run opens a read-only connection and writes nothing. Kills: dry-run
// writes.
func TestClientRxDryRunWritesNothing(t *testing.T) {
	s, dbPath := openClientRxStore(t)
	seedRx(t, s, clientRxTarget, "aabb01", "2026-02-01T00:00:00Z")
	seedRx(t, s, clientRxTarget, "aabb02", "2026-02-02T00:00:00Z")

	if code := runAdminDeleteClientRxDryRun(dbPath, clientRxTarget); code != 0 {
		t.Fatalf("dry-run exit code: %d", code)
	}
	if got := countRx(t, s, clientRxTarget); got != 2 {
		t.Fatalf("dry-run must not delete; rows now %d", got)
	}
}

func TestClientRxDryRunCounts(t *testing.T) {
	s, dbPath := openClientRxStore(t)
	seedRx(t, s, clientRxTarget, "aabb01", "2026-02-01T00:00:00Z")
	seedRx(t, s, clientRxTarget, "aabb02", "2026-02-02T00:00:00Z")
	if err := s.UpsertClientObserver(clientRxTarget, "Mobile", "2026-02-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", clientRxReadOnlyDSN(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rec, obs, rng, err := clientRxCoverageCounts(db, clientRxTarget)
	if err != nil {
		t.Fatal(err)
	}
	if rec != 2 || obs != 1 {
		t.Fatalf("counts: rec=%d obs=%d", rec, obs)
	}
	if !strings.Contains(rng, "2026-02-01T00:00:00Z") || !strings.Contains(rng, "2026-02-02T00:00:00Z") {
		t.Fatalf("rx_at range wrong: %q", rng)
	}
}

// The dry-run DSN is physically read-only. Kills: a mutant switching to a
// read-write handle.
func TestClientRxReadOnlyDSN(t *testing.T) {
	dsn := clientRxReadOnlyDSN("/data/meshcore.db")
	if !strings.Contains(dsn, "mode=ro") {
		t.Fatalf("dry-run DSN must be read-only: %q", dsn)
	}
}

// Uppercase input works end-to-end through the ingestor processor: the key is
// lowercased before matching and deletion. Kills: no lowercasing.
func TestClientRxUppercaseInput(t *testing.T) {
	s, _ := openClientRxStore(t)
	seedRx(t, s, clientRxTarget, "aabb01", "2026-02-01T00:00:00Z")

	s.processClientRxDelete(&clientRxDeleteRequest{
		ID: clientRxNewID(), RequestedAt: time.Now().UTC(),
		Pubkey: strings.ToUpper(clientRxTarget),
	})
	if got := countRx(t, s, clientRxTarget); got != 0 {
		t.Fatalf("uppercase input must delete the lowercase rows; remaining %d", got)
	}
}

// The request/result/salt/audit files are mode 0600.
func TestClientRxFilesAre0600(t *testing.T) {
	s, dbPath := openClientRxStore(t)
	seedRx(t, s, clientRxTarget, "aabb01", "2026-02-01T00:00:00Z")

	id := clientRxNewID()
	if err := clientRxWriteRequest(dbPath, clientRxDeleteRequest{
		ID: id, RequestedAt: time.Now().UTC(), Pubkey: clientRxTarget,
	}); err != nil {
		t.Fatal(err)
	}
	rqp, _ := clientRxRequestPath(dbPath, id)
	assertMode0600(t, rqp)

	s.RunPendingClientRxDeletes()

	rp, _ := clientRxResultPath(dbPath, id)
	assertMode0600(t, rp)
	assertMode0600(t, clientRxSaltPath(dbPath))
	assertMode0600(t, filepath.Join(filepath.Dir(dbPath), clientRxAuditFileName))
}

func assertMode0600(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("%s mode = %o, want 600", path, perm)
	}
}

func TestRunPendingClientRxDeletesEmptyIsNoop(t *testing.T) {
	s, _ := openClientRxStore(t)
	s.RunPendingClientRxDeletes() // must not panic/error
}
