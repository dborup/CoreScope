// Package infrastructure stores the administrator's curated repeater set.
// The ingestor is the sole writer; the API server only reads this snapshot and
// places bounded commands in its sibling queue directory.
package infrastructure

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
)

const (
	MaxSelected  = 128
	MaxRecent    = 4096
	OpSelect     = "select"
	OpRemove     = "remove"
	QueueDirName = "infrastructure-requests"
)

type Entry struct {
	PublicKey string `json:"publicKey"`
	AddedAt   int64  `json:"addedAt"`
}

type Decision struct {
	RequestID string `json:"requestId"`
	AppliedAt int64  `json:"appliedAt"`
}

type State struct {
	Selected []Entry    `json:"selected"`
	Recent   []Decision `json:"recent,omitempty"`
}

func QueuePath(dbPath string) string { return filepath.Join(filepath.Dir(dbPath), QueueDirName) }

// KnownRepeater is checked at both ends of the command queue. Select never
// creates a phantom infrastructure item from an arbitrary 64-hex string.
func KnownRepeater(db *sql.DB, key string) (bool, error) {
	if !ValidPublicKey(key) { return false, nil }
	var exists int
	err := db.QueryRow(`SELECT 1 FROM nodes WHERE public_key = ? AND role = 'repeater' LIMIT 1`, key).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) { return false, nil }
	return err == nil, err
}

func ValidPublicKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	for i := range key {
		c := key[i]
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}

func validID(id string) bool {
	if len(id) != 16 {
		return false
	}
	for i := range id {
		if (id[i] < '0' || id[i] > '9') && (id[i] < 'a' || id[i] > 'f') {
			return false
		}
	}
	return true
}

func (s *State) Apply(id, op, key string, now int64) error {
	if !ValidPublicKey(key) || !validID(id) {
		return errors.New("invalid infrastructure command")
	}
	for _, d := range s.Recent {
		if d.RequestID == id {
			return nil
		}
	}
	// Reject rather than evict a still-live request ID. Queue expiry is five
	// minutes; the oldest ID is retained for ten minutes before reuse.
	if len(s.Recent) >= MaxRecent && now-s.Recent[0].AppliedAt < 600000 {
		return errors.New("infrastructure decision history full")
	}
	for len(s.Recent) >= MaxRecent {
		s.Recent = s.Recent[1:]
	}
	idx := -1
	for i, e := range s.Selected {
		if e.PublicKey == key {
			idx = i
			break
		}
	}
	switch op {
	case OpSelect:
		if idx < 0 {
			if len(s.Selected) >= MaxSelected {
				return errors.New("infrastructure selection full")
			}
			s.Selected = append(s.Selected, Entry{PublicKey: key, AddedAt: now})
			sort.Slice(s.Selected, func(i, j int) bool { return s.Selected[i].PublicKey < s.Selected[j].PublicKey })
		}
	case OpRemove:
		if idx >= 0 {
			s.Selected = append(s.Selected[:idx], s.Selected[idx+1:]...)
		}
	default:
		return errors.New("invalid infrastructure operation")
	}
	s.Recent = append(s.Recent, Decision{RequestID: id, AppliedAt: now})
	return nil
}

func decodeState(b []byte) (State, error) {
	var s State
	if len(b) > 512*1024 {
		return s, errors.New("infrastructure state oversized")
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, err
	}
	if len(s.Selected) > MaxSelected || len(s.Recent) > MaxRecent {
		return State{}, errors.New("infrastructure state exceeds limit")
	}
	seen := make(map[string]bool, len(s.Selected))
	for _, e := range s.Selected {
		if !ValidPublicKey(e.PublicKey) || seen[e.PublicKey] {
			return State{}, fmt.Errorf("invalid infrastructure selection")
		}
		seen[e.PublicKey] = true
	}
	return s, nil
}

// LoadDB reads the single bounded curation row from the main SQLite DB. This
// keeps selections inside the normal SQLite backup and restore lifecycle.
func LoadDB(db *sql.DB) (State, error) {
	var raw string
	err := db.QueryRow(`SELECT state_json FROM infrastructure_state WHERE id = 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	return decodeState([]byte(raw))
}

// SaveDB is called only by the ingestor's queue processor. The API server
// has a read-only DB connection and never calls it.
func SaveDB(db *sql.DB, s State) error {
	if len(s.Selected) > MaxSelected || len(s.Recent) > MaxRecent {
		return errors.New("infrastructure state exceeds limit")
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO infrastructure_state(id, state_json) VALUES(1, ?)
		ON CONFLICT(id) DO UPDATE SET state_json = excluded.state_json`, string(b))
	return err
}
