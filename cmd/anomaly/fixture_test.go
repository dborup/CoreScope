package main

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// Synthetic CoreScope databases for the export tests. The tables carry the
// columns of the production schema (cmd/ingestor/db.go base schema plus the
// internal/dbschema additions) that matter here, including the sensitive
// ones the export must never read; those hold sentinel strings that the
// tests look for in the output. All bytes, names and times are made up.

const (
	sentinelText     = "SENTINEL-DECRYPTED-TEXT"
	sentinelChannel  = "#sentinel-channel-name"
	sentinelPubkey   = "SENTINELPUBKEY0123456789"
	sentinelObserver = "SENTINEL-OBSERVER-NAME"
	sentinelObsID    = "SENTINEL-OBSERVER-ID"
	sentinelResolved = "SENTINEL-RESOLVED-PATH"
)

var fixtureSchema = []string{
	`CREATE TABLE transmissions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		raw_hex TEXT NOT NULL,
		hash TEXT NOT NULL UNIQUE,
		first_seen TEXT NOT NULL,
		route_type INTEGER,
		payload_type INTEGER,
		payload_version INTEGER,
		decoded_json TEXT,
		from_pubkey TEXT,
		last_seen INTEGER NOT NULL DEFAULT 0,
		created_at TEXT DEFAULT (datetime('now')),
		channel_hash TEXT DEFAULT NULL)`,
	`CREATE TABLE observers (id TEXT PRIMARY KEY, name TEXT, iata TEXT)`,
	`CREATE TABLE observations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		transmission_id INTEGER NOT NULL REFERENCES transmissions(id),
		observer_idx INTEGER,
		direction TEXT,
		snr REAL,
		rssi REAL,
		score INTEGER,
		path_json TEXT,
		timestamp INTEGER NOT NULL,
		raw_hex TEXT,
		resolved_path TEXT)`,
	`CREATE INDEX idx_transmissions_first_seen ON transmissions(first_seen)`,
	`CREATE INDEX idx_transmissions_hash ON transmissions(hash)`,
	`CREATE INDEX idx_transmissions_payload_type ON transmissions(payload_type)`,
	`CREATE INDEX idx_observations_transmission_id ON observations(transmission_id)`,
	`CREATE INDEX idx_observations_observer_idx ON observations(observer_idx)`,
	`CREATE INDEX idx_observations_timestamp ON observations(timestamp)`,
	`CREATE INDEX idx_observations_tx_ts ON observations(transmission_id, timestamp)`,
	`CREATE UNIQUE INDEX idx_observations_dedup ON observations(transmission_id, observer_idx, COALESCE(path_json, ''))`,
}

// frameHex builds a MeshCore frame (firmware docs/packet_format.md):
// header, transport codes for routes 0 and 3, path length byte (bits 6-7
// hash size - 1, bits 0-5 hop count), path, payload.
func frameHex(route, payloadType, hashSize int, path, payload []byte) string {
	b := []byte{byte(payloadType<<2 | route)}
	if route == 0 || route == 3 {
		b = append(b, 0x12, 0x34, 0x00, 0x00)
	}
	b = append(b, byte((hashSize-1)<<6|len(path)/hashSize))
	b = append(b, path...)
	b = append(b, payload...)
	return strings.ToUpper(hex.EncodeToString(b))
}

// pathJSON renders hops the way the ingestor stores observations.path_json.
func pathJSON(hashSize int, path []byte) string {
	var parts []string
	for i := 0; i+hashSize <= len(path); i += hashSize {
		parts = append(parts, `"`+strings.ToUpper(hex.EncodeToString(path[i:i+hashSize]))+`"`)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// fixtureDB is a writable synthetic database under construction.
type fixtureDB struct {
	t    *testing.T
	path string
	db   *sql.DB
	obs  int
}

func newFixtureDB(t *testing.T, wal bool) *fixtureDB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "meshcore.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if wal {
		if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range fixtureSchema {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO observers (id, name, iata) VALUES (?, ?, 'XXX'), (?, ?, 'XXX')`,
		sentinelObsID+"-1", sentinelObserver+"-1", sentinelObsID+"-2", sentinelObserver+"-2"); err != nil {
		t.Fatal(err)
	}
	f := &fixtureDB{t: t, path: path, db: db}
	t.Cleanup(func() { db.Close() })
	return f
}

// fxObs is one observation of a fixture transmission.
type fxObs struct {
	at time.Time
	// raw is the observation's own frame (observations.raw_hex); "" leaves
	// it NULL as on rows written before #881
	raw string
	// emptyRaw stores raw_hex as '' instead of NULL
	emptyRaw bool
	// path is observations.path_json; "" leaves it NULL
	path string
	// observer is observations.observer_idx; 0 gives each observation an
	// observer of its own
	observer int
	// noObserver leaves observer_idx NULL
	noObserver bool
}

// tx inserts a transmission (with sentinel values in every sensitive
// column) and its observations; the transmission frame is the first one.
func (f *fixtureDB) tx(hash, txRaw string, payloadType int, obs ...fxObs) {
	f.t.Helper()
	res, err := f.db.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, payload_version,
		decoded_json, from_pubkey, channel_hash) VALUES (?, ?, ?, 1, ?, 0, ?, ?, ?)`,
		txRaw, hash, obs[0].at.UTC().Format(time.RFC3339), payloadType,
		`{"text":"`+sentinelText+`","sender":"`+sentinelObserver+`"}`, sentinelPubkey, sentinelChannel)
	if err != nil {
		f.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	for _, o := range obs {
		f.obs++
		var raw, path interface{}
		if o.raw != "" || o.emptyRaw {
			raw = o.raw
		}
		if o.path != "" {
			path = o.path
		}
		var observer interface{} = f.obs
		switch {
		case o.noObserver:
			observer = nil
		case o.observer != 0:
			observer = o.observer
		}
		if _, err := f.db.Exec(`INSERT INTO observations (transmission_id, observer_idx, direction, snr, rssi, score,
			path_json, timestamp, raw_hex, resolved_path) VALUES (?, ?, 'rx', -7.5, -101, 3, ?, ?, ?, ?)`,
			id, observer, path, o.at.Unix(), raw, sentinelResolved); err != nil {
			f.t.Fatal(err)
		}
	}
}

// relay records one observation of a flood frame with its own raw_hex and
// the matching path_json.
func relay(at time.Time, route, payloadType, hashSize int, path, payload []byte) fxObs {
	return fxObs{at: at, raw: frameHex(route, payloadType, hashSize, path, payload), path: pathJSON(hashSize, path)}
}

// grpPayload is a GRP_TXT payload: channel hash, 2-byte MAC, ciphertext.
func grpPayload(ch byte, n int) []byte {
	p := []byte{ch, 0xaa, 0xbb}
	for i := 0; i < n; i++ {
		p = append(p, byte(i))
	}
	return p
}

func fxHash(i int) string { return fmt.Sprintf("%016x", 0x5eed0000+i) }
