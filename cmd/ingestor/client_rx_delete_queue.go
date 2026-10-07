// Package main: on-disk queue for the admin `delete-client-rx` command.
//
// This mirrors the prune-request queue (internal/prunequeue, see
// prune_geofilter.go) but is a dedicated queue — a separate directory next to
// the SQLite database — so the delete-client-rx processor and the geo-prune
// processor never read each other's markers. Unlike the prune queue, both the
// producer (the admin CLI) and the consumer (the running ingestor's tick) live
// in cmd/ingestor, so the protocol stays in-package rather than in a shared
// internal module.
//
// Layout (under <dir(dbPath)>/client-rx-deletes/):
//
//	request-<id>.json  — written by the CLI (mode 0600) with the target pubkey.
//	result-<id>.json   — written by the ingestor after the delete; counts ONLY,
//	                     no pubkey. The ingestor removes the request in the same
//	                     step via os.Rename semantics in clientRxWriteResult.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// clientRxQueueDirName is the subdirectory (next to the SQLite DB) holding the
// request/result marker files for delete-client-rx.
const clientRxQueueDirName = "client-rx-deletes"

// clientRxDeleteRequest is what the admin CLI writes to request-<id>.json
// (mode 0600). It carries the single target pubkey, already lowercased and
// validated. The file is 0600 so a contributor's identity is not
// world-readable on the data volume; the ingestor honours it verbatim after
// re-validating.
type clientRxDeleteRequest struct {
	ID          string    `json:"id"`
	RequestedAt time.Time `json:"requestedAt"`
	Pubkey      string    `json:"pubkey"`
}

// clientRxDeleteResult is what the ingestor writes to result-<id>.json after
// the delete. COUNTS ONLY — the pubkey and any prefix of it must never appear
// here (the file is the channel back to the CLI, which prints it to the
// operator).
type clientRxDeleteResult struct {
	ID               string    `json:"id"`
	RequestedAt      time.Time `json:"requestedAt"`
	CompletedAt      time.Time `json:"completedAt"`
	ClientReceptions int64     `json:"clientReceptions"`
	ClientObservers  int64     `json:"clientObservers"`
	Error            string    `json:"error,omitempty"`
}

// clientRxNewID returns a 16-hex-char random id suitable for filenames.
func clientRxNewID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// clientRxValidID rejects anything that could escape the queue directory.
func clientRxValidID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// clientRxQueueDir returns the absolute path of the queue directory.
func clientRxQueueDir(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), clientRxQueueDirName)
}

// clientRxEnsureDir creates the queue directory if missing.
func clientRxEnsureDir(dbPath string) (string, error) {
	dir := clientRxQueueDir(dbPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func clientRxRequestPath(dbPath, id string) (string, error) {
	if !clientRxValidID(id) {
		return "", errors.New("invalid client-rx-delete id")
	}
	return filepath.Join(clientRxQueueDir(dbPath), "request-"+id+".json"), nil
}

func clientRxResultPath(dbPath, id string) (string, error) {
	if !clientRxValidID(id) {
		return "", errors.New("invalid client-rx-delete id")
	}
	return filepath.Join(clientRxQueueDir(dbPath), "result-"+id+".json"), nil
}

// writeFileAtomic0600 writes b to path via a temp file + rename, forcing mode
// 0600 regardless of umask.
func writeFileAtomic0600(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// clientRxWriteRequest atomically writes a request-<id>.json marker at 0600.
func clientRxWriteRequest(dbPath string, req clientRxDeleteRequest) error {
	if !clientRxValidID(req.ID) {
		return errors.New("invalid client-rx-delete id")
	}
	if _, err := clientRxEnsureDir(dbPath); err != nil {
		return err
	}
	p, _ := clientRxRequestPath(dbPath, req.ID)
	b, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic0600(p, b)
}

// clientRxWriteResult atomically writes result-<id>.json at 0600, then removes
// the matching request marker. The ingestor holds the only writer.
func clientRxWriteResult(dbPath string, res clientRxDeleteResult) error {
	if !clientRxValidID(res.ID) {
		return errors.New("invalid client-rx-delete id")
	}
	if _, err := clientRxEnsureDir(dbPath); err != nil {
		return err
	}
	p, _ := clientRxResultPath(dbPath, res.ID)
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic0600(p, b); err != nil {
		return err
	}
	reqPath, _ := clientRxRequestPath(dbPath, res.ID)
	_ = os.Remove(reqPath)
	return nil
}

// clientRxReadResult reads result-<id>.json. Returns (nil, nil) if the result
// file does not yet exist (request still pending or unknown id).
func clientRxReadResult(dbPath, id string) (*clientRxDeleteResult, error) {
	p, err := clientRxResultPath(dbPath, id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var r clientRxDeleteResult
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// clientRxRequestExists reports whether request-<id>.json is still present.
func clientRxRequestExists(dbPath, id string) (bool, error) {
	p, err := clientRxRequestPath(dbPath, id)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// clientRxListPending returns all request-<id>.json files in the queue dir, in
// lexicographic order. Used by the ingestor's maintenance tick.
func clientRxListPending(dbPath string) ([]string, error) {
	dir := clientRxQueueDir(dbPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "request-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	return out, nil
}

// clientRxReadRequest reads and parses a request file by full path.
func clientRxReadRequest(path string) (*clientRxDeleteRequest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r clientRxDeleteRequest
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
