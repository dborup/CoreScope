package main

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// #583: modernc.org/sqlite applies DSN query parameters only to a "file:"
// name; "path?mode=ro" opened the database read-write and created a missing
// file. corescope-decrypt only reads, so its handle must be read-only.

func TestOpenReadOnlyRefusesWrites_583(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meshcore.db")
	w, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Exec(`CREATE TABLE transmissions (id INTEGER PRIMARY KEY, hash TEXT, raw_hex TEXT, first_seen TEXT, payload_type INTEGER)`); err != nil {
		t.Fatal(err)
	}
	w.Close()

	db, err := openReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM transmissions`).Scan(&n); err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO transmissions (hash) VALUES ('x')`); err == nil || !strings.Contains(err.Error(), "readonly") {
		t.Fatalf("write on the decrypt handle = %v, want a readonly error", err)
	}
}

func TestOpenReadOnlyDoesNotCreateAMissingFile_583(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	if db, err := openReadOnly(path); err == nil {
		err = db.Ping()
		db.Close()
		if err == nil {
			t.Fatal("opened a database that does not exist")
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("opening read-only created %s (stat err %v)", path, err)
	}
}

// The binary itself, end to end: --db naming a missing file fails and
// leaves no file behind.
func TestDecryptMainDoesNotCreateAMissingDB_583(t *testing.T) {
	if os.Getenv("DECRYPT_583_HELPER") == "1" {
		os.Args = []string{"corescope-decrypt", "--channel", "#test", "--db", os.Getenv("DECRYPT_583_DB")}
		main()
		return
	}
	path := filepath.Join(t.TempDir(), "missing.db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestDecryptMainDoesNotCreateAMissingDB_583$", "-test.count=1")
	cmd.Env = append(os.Environ(), "DECRYPT_583_HELPER=1", "DECRYPT_583_DB="+path)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("corescope-decrypt on a missing --db succeeded:\n%s", out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("corescope-decrypt created %s (stat err %v):\n%s", path, err, out)
	}
}
