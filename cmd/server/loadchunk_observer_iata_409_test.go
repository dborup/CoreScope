package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// createObserverIATAFixture409 builds a DB with three transmissions heard by
// observers whose IATA is set (SJC), NULL, and empty. For the legacy schema a
// fourth observation references an observer that has no observers row at all.
// v3 selects observations.observer_idx; otherwise the legacy observer_id join.
func createObserverIATAFixture409(t *testing.T, v3 bool, ts string) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	conn, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exec := func(s string, args ...interface{}) {
		if _, err := conn.Exec(s, args...); err != nil {
			t.Fatalf("setup exec failed: %v\nSQL: %s", err, s)
		}
	}
	// PREFLIGHT: async=true reason="test fixture: one-shot CREATE in a t.TempDir SQLite, not a migration over existing data."
	exec(`CREATE TABLE transmissions (id INTEGER PRIMARY KEY, raw_hex TEXT, hash TEXT, first_seen TEXT,
		route_type INTEGER, payload_type INTEGER, payload_version INTEGER, decoded_json TEXT)`)
	obsCol := "observer_id TEXT, observer_name TEXT"
	if v3 {
		obsCol = "observer_idx INTEGER"
	}
	// PREFLIGHT: async=true reason="test fixture, tmpdir DB"
	exec(`CREATE TABLE observations (id INTEGER PRIMARY KEY, transmission_id INTEGER, ` + obsCol + `,
		direction TEXT, snr REAL, rssi REAL, score INTEGER, path_json TEXT, timestamp TEXT)`)
	// PREFLIGHT: async=true reason="test fixture, tmpdir DB"
	exec(`CREATE TABLE observers (rowid INTEGER PRIMARY KEY, id TEXT, name TEXT, iata TEXT)`)
	// PREFLIGHT: async=true reason="test fixture, tmpdir DB"
	exec(`CREATE TABLE nodes (public_key TEXT PRIMARY KEY, name TEXT, role TEXT, lat REAL, lon REAL,
		last_seen TEXT, first_seen TEXT, advert_count INTEGER DEFAULT 0)`)
	// PREFLIGHT: async=true reason="test fixture, tmpdir DB"
	exec(`CREATE TABLE schema_version (version INTEGER)`)
	exec(`INSERT INTO schema_version (version) VALUES (1)`)

	exec(`INSERT INTO observers (rowid, id, name, iata) VALUES (1,'obsA','A','SJC'),(2,'obsB','B',NULL),(3,'obsC','C','')`)
	type row struct {
		obs string
		idx int
	}
	rows := []row{{"obsA", 1}, {"obsB", 2}, {"obsC", 3}}
	if !v3 {
		rows = append(rows, row{"obsGhost", 0})
	}
	for i, r := range rows {
		id := i + 1
		exec("INSERT INTO transmissions VALUES (?,?,?,?,0,4,1,'{}')", id, "aa", fmt.Sprintf("h409_%d", id), ts)
		if v3 {
			exec(`INSERT INTO observations (id, transmission_id, observer_idx, direction, snr, rssi, score, path_json, timestamp)
				VALUES (?,?,?,?,?,?,?,?,?)`, id, id, r.idx, "RX", -1.0, -80.0, 5, "[]", time.Now().UTC().Add(-2*time.Hour).Unix())
		} else {
			exec(`INSERT INTO observations (id, transmission_id, observer_id, observer_name, direction, snr, rssi, score, path_json, timestamp)
				VALUES (?,?,?,?,?,?,?,?,?,?)`, id, id, r.obs, r.obs, "RX", -1.0, -80.0, 5, "[]", ts)
		}
	}
	return dbPath
}

type iataSnapshot409 struct {
	ObsIATA map[string]string // tx hash -> observation IATA
	TxIATA  map[string]string
	Distinc map[string][]string
	Region  []string // hashes returned for Region=SJC
}

func snapshotIATA409(t *testing.T, s *PacketStore) iataSnapshot409 {
	t.Helper()
	snap := iataSnapshot409{ObsIATA: map[string]string{}, TxIATA: map[string]string{}, Distinc: map[string][]string{}}
	s.mu.RLock()
	for _, tx := range s.packets {
		snap.TxIATA[tx.Hash] = tx.ObserverIATA
		snap.Distinc[tx.Hash] = storeTxDistinctIatas(tx)
		for _, o := range tx.Observations {
			snap.ObsIATA[tx.Hash] = o.ObserverIATA
		}
	}
	s.mu.RUnlock()
	res := s.QueryPackets(PacketQuery{Region: "SJC", Limit: 50})
	if res != nil {
		for _, m := range res.Packets {
			if h, ok := m["hash"].(string); ok {
				snap.Region = append(snap.Region, h)
			}
		}
	}
	sort.Strings(snap.Region)
	return snap
}

// TestLoadChunk_PreservesObserverIATA_409 loads identical observations through
// the hot path (Load) and the background path (loadChunk) and requires the same
// IATA metadata and region filtering. NULL/empty/missing observers stay unknown.
func TestLoadChunk_PreservesObserverIATA_409(t *testing.T) {
	for _, v3 := range []bool{true, false} {
		name := "legacy"
		if v3 {
			name = "v3"
		}
		t.Run(name, func(t *testing.T) {
			ts := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
			dbPath := createObserverIATAFixture409(t, v3, ts)
			db, err := OpenDB(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.conn.Close()
			if db.isV3() != v3 {
				t.Fatalf("setup: isV3=%v want %v", db.isV3(), v3)
			}

			hot := NewPacketStore(db, &PacketStoreConfig{RetentionHours: 72})
			if err := hot.Load(); err != nil {
				t.Fatalf("Load: %v", err)
			}
			bg := NewPacketStore(db, &PacketStoreConfig{RetentionHours: 72})
			if err := bg.loadChunk(time.Now().UTC().Add(-72*time.Hour), time.Now().UTC()); err != nil {
				t.Fatalf("loadChunk: %v", err)
			}

			h, b := snapshotIATA409(t, hot), snapshotIATA409(t, bg)
			if h.ObsIATA["h409_1"] != "SJC" {
				t.Fatalf("setup: hot path lost IATA: %#v", h.ObsIATA)
			}
			if !reflect.DeepEqual(h, b) {
				t.Errorf("background-loaded IATA/region differs from hot-loaded:\n hot=%+v\n bg =%+v", h, b)
			}
			if len(b.Region) != 1 || b.Region[0] != "h409_1" {
				t.Errorf("Region=SJC on background-loaded store = %v, want [h409_1]", b.Region)
			}
			// Unknown must stay unknown.
			for _, hash := range []string{"h409_2", "h409_3", "h409_4"} {
				if iata, ok := b.ObsIATA[hash]; ok && iata != "" {
					t.Errorf("%s: unknown IATA became %q", hash, iata)
				}
			}
		})
	}
}
