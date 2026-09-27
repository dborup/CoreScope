package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// Issue #100 (port of `Kpa-clawbot/CoreScope#2072`/`#2074`): the ingestor
// writes sqlite_stat1; the server only reads it, through its mode=ro handle.
// These pin that the statistics reach that handle and change the plan of the
// region-filtered channel query, and that the handle cannot write them itself.

// plannerStatsFixture builds a v3-shaped database with the ingestor's two
// competing indexes and a staging-like payload mix: ~10% GRP_TXT
// (payload_type 5), the rest spread over the other types, ~8 observations per
// transmission. Built through a separate read-write connection, the way the
// ingestor owns the file.
func plannerStatsFixture(t *testing.T) (path string, rw *sql.DB) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "stats.db")
	rw, err := sql.Open("sqlite", path+"?_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	rw.SetMaxOpenConns(1)
	t.Cleanup(func() { rw.Close() })
	for _, q := range []string{
		`CREATE TABLE transmissions (id INTEGER PRIMARY KEY, raw_hex TEXT, hash TEXT UNIQUE, first_seen TEXT,
			route_type INTEGER, payload_type INTEGER, payload_version INTEGER, decoded_json TEXT, channel_hash TEXT)`,
		`CREATE TABLE observers (rowid INTEGER PRIMARY KEY, id TEXT UNIQUE, name TEXT, iata TEXT)`,
		`CREATE TABLE observations (id INTEGER PRIMARY KEY, transmission_id INTEGER, observer_idx INTEGER,
			direction TEXT, snr REAL, rssi REAL, score INTEGER, path_json TEXT, timestamp INTEGER)`,
		`CREATE INDEX idx_transmissions_payload_type ON transmissions(payload_type)`,
		`CREATE INDEX idx_tx_channel_hash ON transmissions(channel_hash) WHERE payload_type = 5`,
		`CREATE INDEX idx_observations_transmission_id ON observations(transmission_id)`,
		`BEGIN`,
	} {
		if _, err := rw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for i := 1; i <= 20; i++ {
		iata := "AAA"
		if i%2 == 0 {
			iata = "BBB"
		}
		rw.Exec(`INSERT INTO observers (rowid, id, name, iata) VALUES (?, ?, ?, ?)`, i, fmt.Sprintf("obs%02d", i), fmt.Sprintf("Obs %d", i), iata)
	}
	types := []int{4, 4, 4, 2, 1, 1, 0, 3, 9} // plus 5 on every 10th row
	for i := 1; i <= 4000; i++ {
		pt, ch := types[i%len(types)], interface{}(nil)
		if i%10 == 0 {
			pt, ch = 5, fmt.Sprintf("#ch%d", i%40)
		}
		if _, err := rw.Exec(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
			VALUES (?, '00', ?, ?, 1, ?, '{"text":"a: b"}', ?)`, i, fmt.Sprintf("h%05d", i), fmt.Sprintf("2026-09-01T00:%02d:00Z", i%60), pt, ch); err != nil {
			t.Fatal(err)
		}
		for o := 0; o < 8; o++ {
			rw.Exec(`INSERT INTO observations (transmission_id, observer_idx, timestamp) VALUES (?, ?, ?)`, i, 1+(i+o)%20, 1788000000+i)
		}
	}
	if _, err := rw.Exec(`COMMIT`); err != nil {
		t.Fatal(err)
	}
	return path, rw
}

// The same shape as the v3 region branch of DB.GetChannels.
const plannerStatsChannelsSQL = `SELECT t.channel_hash, COUNT(*) AS msg_count, MAX(t.first_seen) AS last_activity
	FROM transmissions t
	JOIN observations o ON o.transmission_id = t.id
	LEFT JOIN observers obs ON obs.rowid = o.observer_idx
	WHERE t.payload_type = 5
	AND t.channel_hash IS NOT NULL
	AND t.channel_hash NOT LIKE 'enc_%'
	AND obs.rowid IS NOT NULL AND UPPER(TRIM(obs.iata)) IN (?)
	GROUP BY t.channel_hash
	ORDER BY last_activity DESC`

func channelsPlan(t *testing.T, conn *sql.DB) string {
	t.Helper()
	rows, err := conn.Query(`EXPLAIN QUERY PLAN `+plannerStatsChannelsSQL, "AAA")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	return strings.Join(plan, " | ")
}

func TestServerReadsPlannerStatsThroughReadOnlyHandle_Issue100(t *testing.T) {
	path, rw := plannerStatsFixture(t)

	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if !db.isV3() {
		t.Fatal("fixture not detected as v3; the plan below would be for the wrong branch")
	}

	before := channelsPlan(t, db.conn)
	if !strings.Contains(before, "idx_transmissions_payload_type") {
		t.Fatalf("without statistics the plan should drive from idx_transmissions_payload_type (the #2058 plan); got %s", before)
	}

	// The server cannot build the statistics itself: its handle is mode=ro.
	if _, err := db.conn.Exec(`ANALYZE`); err == nil {
		t.Fatal("ANALYZE succeeded on the server's handle; it is not read-only")
	}

	// The ingestor's side: bounded ANALYZE on its own read-write connection.
	if _, err := rw.Exec(`PRAGMA analysis_limit=10000`); err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Exec(`ANALYZE`); err != nil {
		t.Fatal(err)
	}

	// The already-open read-only handle picks the statistics up without a
	// restart: ANALYZE changes the schema cookie, so the next statement
	// reloads sqlite_stat1.
	var rowsStat int
	if err := db.conn.QueryRow(`SELECT count(*) FROM sqlite_stat1`).Scan(&rowsStat); err != nil || rowsStat == 0 {
		t.Fatalf("server handle sees %d sqlite_stat1 rows (err %v)", rowsStat, err)
	}
	after := channelsPlan(t, db.conn)
	t.Logf("plan without stats: %s", before)
	t.Logf("plan with stats:    %s", after)
	if !strings.Contains(after, "idx_tx_channel_hash") {
		t.Fatalf("with statistics the plan should drive from the partial idx_tx_channel_hash; got %s", after)
	}

	// And a freshly opened read-only handle (a server restart) reads the same.
	db2, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if p := channelsPlan(t, db2.conn); !strings.Contains(p, "idx_tx_channel_hash") {
		t.Fatalf("a reopened read-only handle lost the statistics; plan %s", p)
	}

	// The real query still answers through the read-only handle.
	chans, err := db2.GetChannels("AAA")
	if err != nil {
		t.Fatal(err)
	}
	if len(chans) == 0 {
		t.Fatal("GetChannels returned nothing for region AAA")
	}
}
