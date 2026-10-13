package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #339 review, finding 3: in the container's non-root mode the ingestor runs
// as CORESCOPE_UID and owns the database. A delete requested by a root
// `docker exec` used to leave a root-owned 0600 request that the ingestor
// could not read; the CLI reported "queued — ingestor not running?" and the
// delete then ran unannounced after a later restart. Run as root against a
// database owned by another user, the CLI must refuse and write nothing.

func captureStderr339(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()
	fn()
	w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}

func TestDeleteClientRxAsRootRefusesForeignOwnedDB339(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("needs a database owned by a non-root user")
	}
	dbPath := filepath.Join(t.TempDir(), "meshcore.db")
	if err := os.WriteFile(dbPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	orig := clientRxGeteuid
	t.Cleanup(func() { clientRxGeteuid = orig })
	clientRxGeteuid = func() int { return 0 }

	var code int
	stderr := captureStderr339(t, func() {
		code = runAdminDeleteClientRxOnline(dbPath, strings.Repeat("ab", 32), 0)
	})
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	for _, want := range []string{"refusing to run as root", "-u "} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr %q lacks %q", stderr, want)
		}
	}
	if _, err := os.Stat(clientRxQueueDir(dbPath)); !os.IsNotExist(err) {
		t.Errorf("queue directory exists after the refusal (err=%v)", err)
	}
}

// The same user as the database owner (the ingestor's own user, and root in
// the default container mode where root owns the database) still queues.
func TestDeleteClientRxAsDBOwnerStillQueues339(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "meshcore.db")
	if err := os.WriteFile(dbPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var code int
	stderr := captureStderr339(t, func() {
		code = runAdminDeleteClientRxOnline(dbPath, strings.Repeat("ab", 32), 0)
	})
	if code != 2 || !strings.Contains(stderr, "queued") {
		t.Fatalf("exit = %d (%q), want 2 queued", code, stderr)
	}
	pending, err := clientRxListPending(dbPath)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %v, %v; want one request", pending, err)
	}
}
