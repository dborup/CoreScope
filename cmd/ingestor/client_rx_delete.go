// Package main: ingestor-side processor for admin delete-client-rx requests.
//
// An operator who must erase one contributor's mobile RX coverage on request
// runs `corescope-ingestor admin delete-client-rx --pubkey <64 hex>`. That CLI
// enqueues a marker file (client_rx_delete_queue.go); the running ingestor —
// the single writer (#1283) — picks it up on its 15s tick and performs the
// DELETEs here, inside WriterTx so they are serialised with ingest and never
// contend a second read-write process (no SQLITE_BUSY).
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// clientRxPubkeyRe is the STRICT validator for the delete target: trim +
// lowercase first, then require exactly 64 hex chars, matched with `=`. It is
// deliberately NOT the 2–64 topic regex (clientPubkeyRe in client_reception.go)
// — a short prefix would match and delete many contributors' rows.
var clientRxPubkeyRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// deleteClientRxComponent is the writer-timing label for the batched delete.
// It surfaces on /api/perf/write-sources and in the [db-slow-writer] log.
const deleteClientRxComponent = "delete_client_rx"

// deleteClientRxBatchSize bounds one delete transaction so a contributor with
// a very large coverage history does not hold the single writer for the whole
// delete. A package var (not const) so tests can shrink it and exercise the
// multi-batch loop.
var deleteClientRxBatchSize = 5000

// clientRxDeleteReceptionsSQL is the batched delete. The inner seek uses the
// unique index sqlite_autoindex_client_receptions_1 (rx_pubkey leftmost,
// matched with `=`); the outer delete uses the rowid PK; LIMIT bounds each
// transaction. Defined as a const so the EXPLAIN test asserts the exact query
// that runs — a prefix/LIKE edit would change both at once.
const clientRxDeleteReceptionsSQL = `DELETE FROM client_receptions
	   WHERE id IN (SELECT id FROM client_receptions WHERE rx_pubkey = ? LIMIT ?)`

const (
	clientRxSaltFileName  = "client-rx-delete.salt"
	clientRxAuditFileName = "client-rx-delete-audit.jsonl"
	clientRxSaltLen       = 32
)

// DeleteClientRxByPubkey erases one contributor's mobile RX coverage: every
// client_receptions row for rx_pubkey=key (in bounded batches), then the single
// client_observers name row for pubkey=key. key must already be lowercased and
// validated. Matched with `=` only — never a prefix or LIKE. All writes go
// through WriterTx so they serialise with the ingest path and are recorded
// under deleteClientRxComponent in writer timing. Returns the two row counts.
func (s *Store) DeleteClientRxByPubkey(key string) (receptions, observers int64, err error) {
	for {
		var n int64
		e := s.WriterTx(deleteClientRxComponent, func(tx *sql.Tx) error {
			res, err := tx.Exec(clientRxDeleteReceptionsSQL, key, deleteClientRxBatchSize)
			if err != nil {
				return fmt.Errorf("delete client_receptions: %w", err)
			}
			n, _ = res.RowsAffected()
			return nil
		})
		if e != nil {
			return receptions, observers, e
		}
		receptions += n
		// A short batch proves nothing is left: fewer rows matched than the
		// LIMIT allowed. Only a full batch needs another pass.
		if n < int64(deleteClientRxBatchSize) {
			break
		}
	}
	e := s.WriterTx(deleteClientRxComponent, func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM client_observers WHERE pubkey = ?`, key)
		if err != nil {
			return fmt.Errorf("delete client_observers: %w", err)
		}
		observers, _ = res.RowsAffected()
		return nil
	})
	if e != nil {
		return receptions, observers, e
	}
	return receptions, observers, nil
}

// RunPendingClientRxDeletes scans the client-rx-deletes/ queue next to the DB
// and processes any request-<id>.json markers. Safe to call from a ticker —
// a no-op when the queue is empty.
func (s *Store) RunPendingClientRxDeletes() {
	paths, err := clientRxListPending(s.path)
	if err != nil {
		log.Printf("[client-rx-delete] list pending failed: %v", err)
		return
	}
	if len(paths) == 0 {
		return
	}
	for _, p := range paths {
		req, err := clientRxReadRequest(p)
		if err != nil {
			log.Printf("[client-rx-delete] read %s failed: %v — removing", filepath.Base(p), err)
			_ = os.Remove(p)
			continue
		}
		s.processClientRxDelete(req)
	}
}

// processClientRxDelete runs one delete request and writes its result. The
// pubkey is re-validated here (never trust a queue file); the audit log and
// result file carry only counts and a salted HMAC — never the key or a prefix.
func (s *Store) processClientRxDelete(req *clientRxDeleteRequest) {
	key := strings.ToLower(strings.TrimSpace(req.Pubkey))
	res := clientRxDeleteResult{ID: req.ID, RequestedAt: req.RequestedAt}

	if !clientRxPubkeyRe.MatchString(key) {
		res.CompletedAt = time.Now().UTC()
		res.Error = "invalid pubkey"
		log.Printf("[client-rx-delete] id=%s rejected: invalid pubkey", req.ID)
		s.writeClientRxResult(res)
		return
	}

	start := time.Now()
	receptions, observers, derr := s.DeleteClientRxByPubkey(key)
	took := time.Since(start)
	res.CompletedAt = time.Now().UTC()
	res.ClientReceptions = receptions
	res.ClientObservers = observers

	keyHMAC := clientRxKeyHMAC(s.path, key)
	if derr != nil {
		res.Error = derr.Error()
		log.Printf("[client-rx-delete] id=%s key=hmac:%s FAILED after %s: %v",
			req.ID, keyHMAC, took, derr)
	} else {
		log.Printf("[client-rx-delete] id=%s key=hmac:%s client_receptions=%d client_observers=%d took=%s",
			req.ID, keyHMAC, receptions, observers, took)
		if aerr := appendClientRxAudit(s.path, req.ID, keyHMAC, receptions, observers, res.CompletedAt); aerr != nil {
			log.Printf("[client-rx-delete] id=%s audit append failed: %v", req.ID, aerr)
		}
	}
	s.writeClientRxResult(res)
}

func (s *Store) writeClientRxResult(res clientRxDeleteResult) {
	if werr := clientRxWriteResult(s.path, res); werr != nil {
		log.Printf("[client-rx-delete] write result for %s failed: %v", res.ID, werr)
	}
}

// clientRxSaltPath returns the per-install salt file path (next to the DB).
func clientRxSaltPath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), clientRxSaltFileName)
}

// clientRxLoadOrCreateSalt returns the per-install 32-byte salt, creating it
// (mode 0600) next to the DB on first use. Pubkeys are public, so an unsalted
// hash — or a prefix of one — could be reversed by anyone with the mesh's key
// list; the salt makes the audit fingerprint a keyed HMAC instead.
func clientRxLoadOrCreateSalt(dbPath string) ([]byte, error) {
	p := clientRxSaltPath(dbPath)
	b, err := os.ReadFile(p)
	if err == nil {
		if len(b) < clientRxSaltLen {
			return nil, fmt.Errorf("salt file %s too short (%d bytes)", clientRxSaltFileName, len(b))
		}
		return b[:clientRxSaltLen], nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	salt := make([]byte, clientRxSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if err := writeFileAtomic0600(p, salt); err != nil {
		return nil, err
	}
	return salt, nil
}

// clientRxKeyHMAC returns the first 16 hex chars of HMAC-SHA256(salt, key). It
// never returns the key or any prefix of it; on salt failure it returns a fixed
// placeholder so no logging path can fall back to the raw key.
func clientRxKeyHMAC(dbPath, key string) string {
	salt, err := clientRxLoadOrCreateSalt(dbPath)
	if err != nil {
		log.Printf("[client-rx-delete] salt unavailable: %v", err)
		return "unavailable"
	}
	mac := hmac.New(sha256.New, salt)
	mac.Write([]byte(key))
	return hex.EncodeToString(mac.Sum(nil))[:16]
}

// clientRxAuditLine is one JSONL audit record. Counts + salted HMAC only.
type clientRxAuditLine struct {
	ID               string    `json:"id"`
	At               time.Time `json:"at"`
	KeyHMAC          string    `json:"keyHmac"`
	ClientReceptions int64     `json:"clientReceptions"`
	ClientObservers  int64     `json:"clientObservers"`
}

// appendClientRxAudit appends one JSONL line to the 0600 audit file next to the
// DB. It carries no personal data — only the id, counts, and the salted HMAC.
func appendClientRxAudit(dbPath, id, keyHMAC string, receptions, observers int64, at time.Time) error {
	p := filepath.Join(filepath.Dir(dbPath), clientRxAuditFileName)
	line, err := json.Marshal(clientRxAuditLine{
		ID: id, At: at, KeyHMAC: keyHMAC,
		ClientReceptions: receptions, ClientObservers: observers,
	})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	// Force 0600 even if the file pre-existed with a looser mode.
	if err := os.Chmod(p, 0o600); err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}
