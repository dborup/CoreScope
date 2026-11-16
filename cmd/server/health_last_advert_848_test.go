package main

// #848: /api/nodes/{pk}/health and /api/nodes/bulk-health fall back to the
// node's newest own ADVERT from SQLite when the in-memory packet-store window
// has none, so a repeater quiet longer than the window no longer reads as
// "Last activity unknown". Same identity rule as the window (txIsOwnAdvert):
// exact origin, signature not invalid, never a path/prefix match, never relay
// inference. The fallback is marked lastAdvertSource "db".

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

const pk848 = "848a00bb11cc22dd33ee44ff5566778899aabbccddeeff00112233445566aabb"

// seed848Node inserts the repeater row. lastSeenAge sets nodes.last_seen,
// which the ingestor may refresh from a relay hop (#1855): it must never be
// read as an advert.
func seed848Node(t *testing.T, db *DB, lastSeenAge time.Duration) {
	t.Helper()
	ts := time.Now().UTC().Add(-lastSeenAge).Format(time.RFC3339)
	if _, err := db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count)
		VALUES (?, 'Repeater848', 'repeater', 55.7, 12.5, ?, '2026-01-01T00:00:00Z', 3)`, pk848, ts); err != nil {
		t.Fatal(err)
	}
}

// seed848Advert inserts one ADVERT transmission attributed to pk848
// (from_pubkey) with the given decoded_json and age. Returns its first_seen.
func seed848Advert(t *testing.T, db *DB, hash, decodedJSON string, age time.Duration) string {
	t.Helper()
	ts := time.Now().UTC().Add(-age).Format(time.RFC3339)
	if _, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey)
		VALUES ('1140aabbcc', ?, ?, 1, 4, ?, ?)`, hash, ts, decodedJSON, pk848); err != nil {
		t.Fatal(err)
	}
	return ts
}

func ownAdvertJSON848(extra string) string {
	return `{"type":"ADVERT","pubKey":"` + pk848 + `","name":"Repeater848"` + extra + `}`
}

type stats848 struct {
	LastHeard        *string `json:"lastHeard"`
	LastAdvert       *string `json:"lastAdvert"`
	LastAdvertSource *string `json:"lastAdvertSource"`
}

func getHealth848(t *testing.T, r *mux.Router) stats848 {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/nodes/"+pk848+"/health", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("/health: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Stats stats848 `json:"stats"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Stats
}

func getBulkHealth848(t *testing.T, r *mux.Router) stats848 {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/nodes/bulk-health?limit=50", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("/bulk-health: %d %s", w.Code, w.Body.String())
	}
	var body []struct {
		PublicKey string   `json:"public_key"`
		Stats     stats848 `json:"stats"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, n := range body {
		if n.PublicKey == pk848 {
			return n.Stats
		}
	}
	t.Fatalf("node %s missing from bulk-health", pk848)
	return stats848{}
}

func str848(p *string) string {
	if p == nil {
		return "<null>"
	}
	return *p
}

func wantStats848(t *testing.T, where string, got stats848, advert, heard, source string) {
	t.Helper()
	if str848(got.LastAdvert) != advert || str848(got.LastHeard) != heard || str848(got.LastAdvertSource) != source {
		t.Errorf("%s: lastAdvert=%s lastHeard=%s lastAdvertSource=%s; want %s / %s / %s",
			where, str848(got.LastAdvert), str848(got.LastHeard), str848(got.LastAdvertSource), advert, heard, source)
	}
}

// loadedStore848 builds a store with a 24 h window, loads it and refreshes
// the last-known-advert snapshot (as the background recomputer would).
func loadedStore848(t *testing.T, db *DB) *PacketStore {
	t.Helper()
	store := NewPacketStore(db, &PacketStoreConfig{RetentionHours: 24})
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	if !store.WaitIndexesReady(5 * time.Second) {
		t.Fatal("indexes not ready")
	}
	store.refreshLastKnownAdverts()
	return store
}

// The issue's case: the only own advert is 33 h old, outside a 24 h window.
// Both health endpoints report it, marked as coming from the database.
func TestHealthLastAdvert848_OutsideWindow(t *testing.T) {
	db := setupTestDB(t)
	seed848Node(t, db, 33*time.Hour)
	ts := seed848Advert(t, db, "h848old", ownAdvertJSON848(""), 33*time.Hour)
	store := loadedStore848(t, db)
	if n := len(store.byNode[pk848]); n != 0 {
		t.Fatalf("precondition: advert must be outside the window, store has %d packets", n)
	}
	r := router807(t, db, store)
	wantStats848(t, "/health", getHealth848(t, r), ts, ts, "db")
	wantStats848(t, "/bulk-health", getBulkHealth848(t, r), ts, ts, "db")
}

// An advert inside the window is reported from the window, even when the
// database snapshot is stale.
func TestHealthLastAdvert848_WindowWins(t *testing.T) {
	db := setupTestDB(t)
	seed848Node(t, db, time.Hour)
	seed848Advert(t, db, "h848old", ownAdvertJSON848(""), 33*time.Hour)
	store := loadedStore848(t, db) // snapshot holds the 33 h advert
	recent := seed848Advert(t, db, "h848new", ownAdvertJSON848(""), time.Hour)
	if err := store.Load(); err != nil { // window now has the 1 h advert; snapshot not refreshed
		t.Fatal(err)
	}
	r := router807(t, db, store)
	wantStats848(t, "/health", getHealth848(t, r), recent, recent, "window")
	wantStats848(t, "/bulk-health", getBulkHealth848(t, r), recent, recent, "window")
}

// Cold start: the store has not loaded anything yet. The recomputer's
// synchronous prewarm alone gives the node its last advert.
func TestHealthLastAdvert848_ColdStart(t *testing.T) {
	db := setupTestDB(t)
	seed848Node(t, db, 2*time.Hour)
	ts := seed848Advert(t, db, "h848cold", ownAdvertJSON848(""), 2*time.Hour)
	store := NewPacketStore(db, nil) // never Load()ed
	stop := store.StartLastKnownAdvertRecomputer(time.Hour)
	defer stop()
	r := router807(t, db, store)
	wantStats848(t, "/health", getHealth848(t, r), ts, ts, "db")
	wantStats848(t, "/bulk-health", getBulkHealth848(t, r), ts, ts, "db")
}

// A relay-refreshed nodes.last_seen is never an advert (#1855): without an
// own ADVERT in the database the stats stay null, and with an older one the
// advert keeps its own timestamp, not last_seen.
func TestHealthLastAdvert848_RelayOnlyLastSeenDoesNotCount(t *testing.T) {
	db := setupTestDB(t)
	seed848Node(t, db, 10*time.Minute) // last_seen fresh from a relay hop only
	// An ADVERT from another node that merely names pk848 in its path or
	// shares its prefix is not attributed to pk848 (from_pubkey differs).
	if _, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey)
		VALUES ('1140aabbcc', 'h848other', ?, 1, 4, '{"type":"ADVERT","pubKey":"848aff"}', '848aff')`,
		time.Now().UTC().Add(-30*time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	r := router807(t, db, loadedStore848(t, db))
	wantStats848(t, "/health", getHealth848(t, r), "<null>", "<null>", "<null>")
	wantStats848(t, "/bulk-health", getBulkHealth848(t, r), "<null>", "<null>", "<null>")

	ts := seed848Advert(t, db, "h848own", ownAdvertJSON848(""), 40*time.Hour)
	r = router807(t, db, loadedStore848(t, db))
	wantStats848(t, "/health", getHealth848(t, r), ts, ts, "db")
}

// A forged or mis-signed advert never counts: a newer advert whose
// signature failed, or whose decoded origin is another key, is skipped and
// the older valid own advert is used. A node with only such adverts stays
// unknown.
func TestHealthLastAdvert848_ForgedAdvertDoesNotCount(t *testing.T) {
	db := setupTestDB(t)
	seed848Node(t, db, 26*time.Hour)
	seed848Advert(t, db, "h848bad1", ownAdvertJSON848(`,"signatureValid":false`), 26*time.Hour)
	r := router807(t, db, loadedStore848(t, db))
	wantStats848(t, "/health only invalid", getHealth848(t, r), "<null>", "<null>", "<null>")

	valid := seed848Advert(t, db, "h848good", ownAdvertJSON848(`,"signatureValid":true`), 50*time.Hour)
	seed848Advert(t, db, "h848bad2", ownAdvertJSON848(`,"signatureValid":false`), 30*time.Hour)
	// from_pubkey says pk848 but the decoded origin is another node.
	seed848Advert(t, db, "h848spoof", `{"type":"ADVERT","pubKey":"`+pk807+`"}`, 28*time.Hour)
	seed848Advert(t, db, "h848junk", `not json`, 27*time.Hour)
	r = router807(t, db, loadedStore848(t, db))
	wantStats848(t, "/health", getHealth848(t, r), valid, valid, "db")
	wantStats848(t, "/bulk-health", getBulkHealth848(t, r), valid, valid, "db")
}

// The newest own advert wins (id order).
func TestHealthLastAdvert848_NewestAdvertWins(t *testing.T) {
	db := setupTestDB(t)
	seed848Node(t, db, 30*time.Hour)
	seed848Advert(t, db, "h848a", ownAdvertJSON848(""), 60*time.Hour)
	newest := seed848Advert(t, db, "h848b", ownAdvertJSON848(""), 30*time.Hour)
	got, err := db.LoadLastKnownAdverts()
	if err != nil {
		t.Fatal(err)
	}
	if got[pk848].LastAdvert != newest {
		t.Fatalf("last advert = %q, want %q", got[pk848].LastAdvert, newest)
	}
}

// The fallback only fills an empty advert, and lastHeard keeps newer
// identity-safe evidence from the window.
func TestApplyLastKnownAdvert848(t *testing.T) {
	old := "2026-11-13T12:12:00Z"
	newer := "2026-11-15T08:00:00Z"
	cases := []struct {
		name                  string
		heard, advert, known  string
		wantHeard, wantAdvert string
		wantSource            string
	}{
		{"window advert wins", newer, newer, old, newer, newer, "window"},
		{"nothing known", "", "", "", "", "", ""},
		{"unparseable known", "", "", "yesterday", "", "", ""},
		{"fills both", "", "", old, old, old, "db"},
		{"newer window relay evidence kept", newer, "", old, newer, old, "db"},
		{"older heard replaced", old, "", newer, newer, newer, "db"},
	}
	for _, c := range cases {
		heard, advert := c.heard, c.advert
		src := applyLastKnownAdvert(&heard, &advert, c.known)
		if heard != c.wantHeard || advert != c.wantAdvert || src != c.wantSource {
			t.Errorf("%s: heard=%q advert=%q source=%q; want %q %q %q", c.name, heard, advert, src, c.wantHeard, c.wantAdvert, c.wantSource)
		}
	}
}

// The Go re-check of every snapshot row applies the window's identity rule.
func TestLastKnownAdvertRow848(t *testing.T) {
	ts := "2026-11-13T12:12:00Z"
	cases := []struct {
		name, decoded, firstSeen string
		want                     string
	}{
		{"own advert", ownAdvertJSON848(""), ts, ts},
		{"signature valid", ownAdvertJSON848(`,"signatureValid":true`), ts, ts},
		{"signature invalid", ownAdvertJSON848(`,"signatureValid":false`), ts, ""},
		{"other origin", `{"type":"ADVERT","pubKey":"` + pk807 + `"}`, ts, ""},
		{"no origin", `{"type":"ADVERT"}`, ts, ""},
		{"malformed json", `{`, ts, ""},
		{"bad timestamp", ownAdvertJSON848(""), "never", ""},
	}
	for _, c := range cases {
		if got := lastKnownAdvertTimestamp(pk848, c.firstSeen, c.decoded); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// Perf guard (#848, AGENTS.md rule 0): both correlated subqueries seek
// idx_transmissions_from_pubkey and the advert row is joined by rowid, so the
// snapshot never scans transmissions (no per-advert GROUP BY, no temp sort).
func TestLastKnownAdvertSQLPlan848(t *testing.T) {
	db := setupTestDB(t)
	rows, err := db.conn.Query("EXPLAIN QUERY PLAN " + lastKnownAdvertSQL)
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
	seeks := 0
	for _, d := range plan {
		if strings.HasPrefix(d, "SCAN t") || strings.HasPrefix(d, "SCAN a") || strings.Contains(d, "TEMP B-TREE") {
			t.Fatalf("snapshot must not scan transmissions or sort: %q in %q", d, plan)
		}
		if strings.Contains(d, "SEARCH t USING INDEX idx_transmissions_from_pubkey") {
			seeks++
		}
	}
	if seeks != 2 {
		t.Fatalf("want 2 from_pubkey index seeks (size + last advert), got %d in %q", seeks, plan)
	}
}

// Perf proof (#848). Same synthetic shape as the #807 benchmark, but every
// ADVERT carries a real decoded_json (own pubKey, valid signature), and the
// spammer's 20,000 newer adverts have an invalid signature, so both walks
// (size and last advert) hit their worst case for that node. Set
// CORESCOPE_848_BENCH_DB to run against a database file instead.
func BenchmarkLoadLastKnownAdverts848(b *testing.B) {
	var db *DB
	if path := os.Getenv("CORESCOPE_848_BENCH_DB"); path != "" {
		var err error
		if db, err = OpenDB(path); err != nil {
			b.Fatal(err)
		}
	} else {
		db = setupTestDB(b)
		tx, err := db.conn.Begin()
		if err != nil {
			b.Fatal(err)
		}
		pk := func(i int) string { return fmt.Sprintf("%064x", i*2654435761) }
		dj := func(p string, valid bool) string {
			return fmt.Sprintf(`{"type":"ADVERT","pubKey":"%s","name":"n","signatureValid":%t}`, p, valid)
		}
		for i := 1; i <= 2000; i++ {
			tx.Exec(`INSERT INTO nodes (public_key, role) VALUES (?, 'repeater')`, pk(i))
		}
		ins, _ := tx.Prepare(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey) VALUES (?, ?, '2026-11-15T00:00:00Z', ?, ?, ?, ?)`)
		for i := 1; i <= 210000; i++ {
			raw, rt, pt, dec, from := "1502aabbccddeeff", 1, 5, `{"type":"TXT_MSG"}`, interface{}(nil)
			if i%7 == 0 {
				p := pk(i%2000 + 1)
				pt, from, dec = 4, p, dj(p, true)
				if i%2 == 0 {
					raw, rt = "1200aabbccddeeff", 2
				} else {
					raw = "1140aabbccddeeff"
				}
			}
			ins.Exec(raw, fmt.Sprintf("%016x", i), rt, pt, dec, from)
		}
		for i := 0; i < 20000; i++ {
			ins.Exec("1200aabbccddeeff", fmt.Sprintf("s%015x", i), 2, 4, dj(pk(1), false), pk(1))
		}
		if err := tx.Commit(); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m, err := db.LoadLastKnownAdverts()
		if err != nil {
			b.Fatal(err)
		}
		if len(m) == 0 {
			b.Fatal("empty result")
		}
	}
}
