package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

var bulkScanBase486 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// All optional scan columns are independently present/absent. In each schema,
// a valued observation is followed by NULLs, another valued row, a transmission
// with no observation, and a final distinct row. This catches stale targets,
// retained pointers and argument-order mistakes in all three real loaders.
func bulkScanFixture486(t *testing.T, v3 bool, optional uint) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scan.db")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := conn.Exec(q, args...); err != nil {
			t.Fatalf("fixture: %v: %s", err, q)
		}
	}
	cols := ""
	if optional&4 != 0 {
		cols += ", scope_name TEXT"
	}
	if optional&8 != 0 {
		cols += ", route_mask INTEGER"
	}
	// PREFLIGHT: async=true reason="test fixture: CREATE in a fresh t.TempDir database, not a migration."
	exec(`CREATE TABLE transmissions (id INTEGER PRIMARY KEY, raw_hex TEXT, hash TEXT, first_seen TEXT,
		route_type INTEGER, payload_type INTEGER, payload_version INTEGER, decoded_json TEXT` + cols + `)`)
	obsRef := "observer_id TEXT, observer_name TEXT"
	if v3 {
		obsRef = "observer_idx INTEGER"
	}
	cols = ""
	if optional&1 != 0 {
		cols += ", raw_hex TEXT"
	}
	if optional&2 != 0 {
		cols += ", resolved_path TEXT"
	}
	// PREFLIGHT: async=true reason="test fixture: CREATE in a fresh t.TempDir database, not a migration."
	exec(`CREATE TABLE observations (id INTEGER PRIMARY KEY, transmission_id INTEGER, ` + obsRef + `,
		direction TEXT, snr REAL, rssi REAL, score INTEGER, path_json TEXT, timestamp ` + map[bool]string{true: "INTEGER", false: "TEXT"}[v3] + cols + `)`)
	// PREFLIGHT: async=true reason="test fixture: CREATE in a fresh t.TempDir database, not a migration."
	exec(`CREATE TABLE observers (rowid INTEGER PRIMARY KEY, id TEXT, name TEXT, iata TEXT)`)
	// PREFLIGHT: async=true reason="test fixture: CREATE in a fresh t.TempDir database, not a migration."
	exec(`CREATE TABLE nodes (public_key TEXT PRIMARY KEY, name TEXT, role TEXT, lat REAL, lon REAL,
		last_seen TEXT, first_seen TEXT, advert_count INTEGER DEFAULT 0)`)
	exec(`INSERT INTO observers VALUES (1,'scan-a','Alpha','SJC'),(2,'scan-b',NULL,NULL),(3,'scan-c','Charlie','SEA')`)
	for i := 1; i <= 4; i++ {
		var raw, rt, pt, pv, decoded, scope, mask interface{}
		if i%2 != 0 {
			raw, rt, pt, pv, decoded = fmt.Sprintf("%02x00", i), i, i+3, i-1, fmt.Sprintf(`{"scan":%d}`, i)
			scope, mask = fmt.Sprintf("scope-%d", i), 1<<i
		}
		args := []interface{}{i, raw, fmt.Sprintf("scan-%d", i), bulkScanBase486.Add(time.Duration(i) * time.Minute).Format(time.RFC3339), rt, pt, pv, decoded}
		if optional&4 != 0 {
			args = append(args, scope)
		}
		if optional&8 != 0 {
			args = append(args, mask)
		}
		exec(`INSERT INTO transmissions VALUES (`+strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+`)`, args...)
	}
	for i := 1; i <= 5; i++ {
		txID, observer := 1, i
		if i == 4 {
			txID, observer = 3, 3
		}
		if i == 5 {
			txID, observer = 4, 2
		}
		var direction, snr, rssi, score, pj, stamp, raw, resolved interface{}
		if i != 2 && i != 5 {
			direction, snr, rssi, score = fmt.Sprintf("RX-%d", i), float64(i)+0.25, float64(-80-i), i*2
			pj = fmt.Sprintf(`["%02x"]`, i)
			stamp = bulkScanBase486.Add(time.Duration(10-i) * time.Minute).Format(time.RFC3339)
			if v3 {
				stamp = bulkScanBase486.Add(time.Duration(10-i) * time.Minute).Unix()
			}
			raw = fmt.Sprintf("%02x00", i)
			resolved = fmt.Sprintf(`["%s"]`, strings.Repeat(fmt.Sprintf("%x", i), 64))
		}
		args := []interface{}{i, txID}
		if v3 {
			args = append(args, observer)
		} else {
			var name interface{}
			if observer != 2 {
				name = map[int]string{1: "Alpha", 3: "Charlie"}[observer]
			}
			args = append(args, map[int]string{1: "scan-a", 2: "scan-b", 3: "scan-c"}[observer], name)
		}
		args = append(args, direction, snr, rssi, score, pj, stamp)
		if optional&1 != 0 {
			args = append(args, raw)
		}
		if optional&2 != 0 {
			args = append(args, resolved)
		}
		insert := `INSERT INTO observations VALUES (` + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + `)`
		exec(insert, args...)
		if i == 4 {
			// Same observer/path, heard earlier: it must remain deduplicated
			// even though it overwrites the reused targets before the NULL row.
			duplicate := append([]interface{}(nil), args...)
			duplicate[0] = 6
			if v3 {
				duplicate[8] = bulkScanBase486.Add(5 * time.Minute).Unix()
			} else {
				duplicate[9] = bulkScanBase486.Add(5 * time.Minute).Format(time.RFC3339)
			}
			exec(insert, duplicate...)
		}
	}
	return path
}

func bulkScanLoad486(s *PacketStore, loader string) error {
	switch loader {
	case "Load":
		if err := s.Load(); err != nil {
			return err
		}
		if !s.WaitIndexesReady(10 * time.Second) {
			return fmt.Errorf("Load background indexes did not settle")
		}
		return nil
	case "LoadChunked":
		return s.LoadChunked(5000)
	case "loadChunk":
		return s.loadChunk(bulkScanBase486.Add(-24*time.Hour), bulkScanBase486.Add(1000*time.Hour))
	default:
		return fmt.Errorf("unknown loader %q", loader)
	}
}

func TestBulkScanReuseValues486(t *testing.T) {
	for _, v3 := range []bool{false, true} {
		for optional := uint(0); optional < 16; optional++ {
			t.Run(fmt.Sprintf("v3=%t/options=%04b", v3, optional), func(t *testing.T) {
				path := bulkScanFixture486(t, v3, optional)
				db, err := OpenDB(path)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if db.isV3() != v3 || db.hasObsRawHex() != (optional&1 != 0) || db.hasResolvedPath() != (optional&2 != 0) || db.hasScopeName() != (optional&4 != 0) || db.hasRouteMask() != (optional&8 != 0) {
					t.Fatal("fixture schema capabilities were not detected")
				}
				for _, loader := range []string{"Load", "LoadChunked", "loadChunk"} {
					t.Run(loader, func(t *testing.T) {
						s := NewPacketStore(db, nil)
						load := func() error { return bulkScanLoad486(s, loader) }
						if loader == "LoadChunked" {
							// Recreate the scan storage in the next chunk as well.
							load = func() error { return s.LoadChunked(2) }
						}
						if err := load(); err != nil {
							t.Fatal(err)
						}
						if len(s.packets) != 4 || len(s.byTxID) != 4 || len(s.byHash) != 4 || len(s.byObsID) != 5 || s.totalObs != 5 || s.maxTxID != 4 || s.maxObsID != 5 {
							t.Fatalf("loader indexes/counts changed: tx=%d obs=%d", len(s.packets), s.totalObs)
						}
						for i := 1; i <= 4; i++ {
							tx := s.byTxID[i]
							if tx == nil || tx.Hash != fmt.Sprintf("scan-%d", i) || tx.FirstSeen != bulkScanBase486.Add(time.Duration(i)*time.Minute).Format(time.RFC3339) || s.byHash[tx.Hash] != tx {
								t.Fatalf("transmission %d changed: %+v", i, tx)
							}
							if i%2 == 0 {
								wantObs := 0
								if i == 4 {
									wantObs = 1
								}
								if tx.RawHex != "" || tx.DecodedJSON != "" || tx.RouteType != nil || tx.PayloadType != nil || tx.ScopeName != "" || tx.routeMaskKnown || len(tx.Observations) != wantObs {
									t.Fatalf("NULL/no-observation row inherited a preceding value: %+v", tx)
								}
								continue
							}
							if tx.RawHex != fmt.Sprintf("%02x00", i) || tx.DecodedJSON != fmt.Sprintf(`{"scan":%d}`, i) || tx.RouteType == nil || *tx.RouteType != i || tx.PayloadType == nil || *tx.PayloadType != i+3 {
								t.Fatalf("transmission scan values or retained numeric pointers changed: %+v", tx)
							}
							wantScope := ""
							if optional&4 != 0 {
								wantScope = fmt.Sprintf("scope-%d", i)
							}
							if tx.ScopeName != wantScope || tx.routeMaskKnown != (optional&8 != 0) || (tx.routeMaskKnown && tx.routeMask != uint8(1<<i)) {
								t.Fatalf("optional transmission values changed: %+v", tx)
							}
						}
						for i := 1; i <= 5; i++ {
							o := s.byObsID[i]
							if o == nil || o.ID != i || o.RawHex != "" {
								t.Fatalf("observation %d missing or retained its raw frame: %+v", i, o)
							}
							if i == 2 || i == 5 {
								if o.ObserverID != "scan-b" || o.ObserverName != "" || o.ObserverIATA != "" || o.Direction != "" || o.SNR != nil || o.RSSI != nil || o.Score != nil || o.PathJSON != "" || o.Timestamp != "" || o.wireRoute != 0 {
									t.Fatalf("NULL observation inherited a preceding value: %+v", o)
								}
								continue
							}
							observer := i
							if i == 4 {
								observer = 3
							}
							wantStamp := bulkScanBase486.Add(time.Duration(10-i) * time.Minute).Format(time.RFC3339)
							if v3 {
								wantStamp = bulkScanBase486.Add(time.Duration(10-i) * time.Minute).Format("2006-01-02T15:04:05.000Z")
							}
							if o.ObserverID != map[int]string{1: "scan-a", 3: "scan-c"}[observer] || o.ObserverName != map[int]string{1: "Alpha", 3: "Charlie"}[observer] || o.ObserverIATA != map[int]string{1: "SJC", 3: "SEA"}[observer] || o.Direction != fmt.Sprintf("RX-%d", i) || o.SNR == nil || *o.SNR != float64(i)+0.25 || o.RSSI == nil || *o.RSSI != float64(-80-i) || o.Score == nil || *o.Score != i*2 || o.PathJSON != fmt.Sprintf(`["%02x"]`, i) || o.Timestamp != wantStamp {
								t.Fatalf("observation values or retained numeric pointers changed: %+v", o)
							}
							// scanAndMergeChunk does not currently retain wireRoute;
							// this optimization deliberately preserves that baseline.
							if loader != "LoadChunked" {
								wantWire := uint8(0)
								if optional&1 != 0 {
									wantWire = uint8(i&3) + 1
								}
								if o.wireRoute != wantWire {
									t.Fatalf("observation %d wire route=%d, want %d", i, o.wireRoute, wantWire)
								}
							}
							if optional&2 != 0 {
								pk := strings.Repeat(fmt.Sprintf("%x", i), 64)
								found := false
								for _, tx := range s.byNode[pk] {
									if tx.ID == 4 {
										t.Fatalf("NULL resolved_path inherited relay %s", pk)
									}
									found = found || tx.ID == o.TransmissionID
								}
								if !found {
									t.Fatalf("resolved relay %s lost its transmission", pk)
								}
							}
						}
						if !reflect.DeepEqual(s.byTxID[1].Observations[0].SNR, s.byObsID[1].SNR) || s.byObsID[1].SNR == s.byObsID[3].SNR {
							t.Fatal("observations retain shared mutable scan storage")
						}
						acct113Check(t, s, loader)
					})
				}
			})
		}
	}
}

// Integrated allocation guard: all targets and SQL Scan arguments must be
// reused, not rebuilt for each joined observation row. Wait for Load's index
// goroutines before each measurement ends so their allocations do not leak
// into later cases. No wall-clock performance assertions are used.
func TestBulkScanReuseAllocations486(t *testing.T) {
	const rows = 2000
	path := filepath.Join(t.TempDir(), "alloc.db")
	createTestDBAt(t, path, rows)
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, loader := range []string{"Load", "LoadChunked", "loadChunk"} {
		t.Run(loader, func(t *testing.T) {
			// Go 1.26.3 darwin/arm64 baseline: 102.38 / 117.47 / 101.43
			// allocations/row. Bounds leave headroom below the old loader.
			limit := map[string]float64{"Load": 91, "LoadChunked": 106, "loadChunk": 90}[loader]
			got := testing.AllocsPerRun(3, func() {
				s := NewPacketStore(db, nil)
				if err := bulkScanLoad486(s, loader); err != nil {
					t.Fatal(err)
				}
				if len(s.packets) != rows || s.totalObs != rows {
					t.Fatalf("loaded %d/%d, want %d", len(s.packets), s.totalObs, rows)
				}
				runtime.KeepAlive(s)
			}) / rows
			t.Logf("%s: %.2f allocations per joined observation row", loader, got)
			if got > limit {
				t.Fatalf("%.2f allocations/row exceeds %.0f: SQL scan targets appear to be rebuilt per row", got, limit)
			}
		})
	}
}
