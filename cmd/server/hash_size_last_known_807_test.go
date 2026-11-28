package main

// #807: a node keeps its last known advert hash size after its last advert
// has left the in-memory packet-store window, and on a cold start before the
// store has loaded anything. hash_size_inconsistent keeps coming from the
// window only.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

const pk807 = "961d00aa11bb22cc"

// seed807Node inserts the repeater row the tests look up.
func seed807Node(t *testing.T, db *DB) {
	t.Helper()
	recent := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if _, err := db.conn.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count)
		VALUES (?, 'Repeater807', 'repeater', 55.7, 12.5, ?, '2026-01-01T00:00:00Z', 3)`, pk807, recent); err != nil {
		t.Fatal(err)
	}
}

// seed807Advert inserts one ADVERT transmission for pk807.
func seed807Advert(t *testing.T, db *DB, hash, rawHex string, routeType int, age time.Duration) {
	t.Helper()
	ts := time.Now().UTC().Add(-age).Format(time.RFC3339)
	dj := `{"pubKey":"` + pk807 + `","name":"Repeater807","type":"ADVERT","flags":{"isRepeater":true}}`
	if _, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey)
		VALUES (?, ?, ?, ?, 4, ?, ?)`, rawHex, hash, ts, routeType, dj, pk807); err != nil {
		t.Fatal(err)
	}
}

func router807(t *testing.T, db *DB, store *PacketStore) *mux.Router {
	t.Helper()
	cfg := &Config{Port: 3000}
	srv := NewServer(db, cfg, NewHub())
	store.config = cfg
	srv.store = store
	r := mux.NewRouter()
	srv.RegisterRoutes(r)
	return r
}

type node807 struct {
	PublicKey    string `json:"public_key"`
	HashSize     *int   `json:"hash_size"`
	Inconsistent bool   `json:"hash_size_inconsistent"`
	SizesSeen    []int  `json:"hash_sizes_seen"`
}

func get807(t *testing.T, r *mux.Router) (list node807, detail node807) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/nodes?limit=50", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("/api/nodes: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Nodes []node807 `json:"nodes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, n := range body.Nodes {
		if n.PublicKey == pk807 {
			list = n
		}
	}
	if list.PublicKey == "" {
		t.Fatalf("node %s missing from /api/nodes", pk807)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/nodes/"+pk807, nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("/api/nodes/%s: %d %s", pk807, w.Code, w.Body.String())
	}
	var det struct {
		Node node807 `json:"node"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &det); err != nil {
		t.Fatal(err)
	}
	return list, det.Node
}

func wantSize807(t *testing.T, where string, n node807, want int) {
	t.Helper()
	if n.HashSize == nil || *n.HashSize != want {
		var got interface{}
		if n.HashSize != nil {
			got = *n.HashSize
		}
		t.Errorf("%s: hash_size = %v, want %d", where, got, want)
	}
	if n.Inconsistent {
		t.Errorf("%s: hash_size_inconsistent must stay false for a remembered size", where)
	}
	if len(n.SizesSeen) != 0 {
		t.Errorf("%s: hash_sizes_seen must not come from the remembered size, got %v", where, n.SizesSeen)
	}
}

// The node's only advert is older than the store window (retentionHours 24),
// so the window has no hash size for it; the remembered size fills the gap.
func TestLastKnownHashSize807_AdvertOutsideWindow(t *testing.T) {
	db := setupTestDB(t)
	seed807Node(t, db)
	seed807Advert(t, db, "h807old", "1140aabbcc", RouteFlood, 61*time.Hour) // path byte 0x40 → 2-byte
	store := NewPacketStore(db, &PacketStoreConfig{RetentionHours: 24})
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	if !store.WaitIndexesReady(5 * time.Second) {
		t.Fatal("indexes not ready")
	}
	if info := store.GetNodeHashSizeInfo()[pk807]; info != nil {
		t.Fatalf("precondition: advert must be outside the window, got window info %+v", info)
	}
	store.refreshLastKnownAdverts()
	list, detail := get807(t, router807(t, db, store))
	wantSize807(t, "/api/nodes", list, 2)
	wantSize807(t, "/api/nodes/{pk}", detail, 2)
}

// Cold start: the store has not loaded any packet yet. The recomputer's
// synchronous prewarm alone must give the node its size.
func TestLastKnownHashSize807_ColdStart(t *testing.T) {
	db := setupTestDB(t)
	seed807Node(t, db)
	seed807Advert(t, db, "h807cold", "1180aabbcc", RouteFlood, time.Hour) // 0x80 → 3-byte
	store := NewPacketStore(db, nil)                                      // never Load()ed
	stop := store.StartLastKnownAdvertRecomputer(time.Hour)
	defer stop()
	// The prewarm is synchronous: the snapshot is there when Start returns
	// (v0.2.9 review nit, #809 GM5).
	if got := store.LastKnownAdverts()[pk807].HashSize; got != 3 {
		t.Fatalf("snapshot right after Start = %d, want 3", got)
	}
	list, detail := get807(t, router807(t, db, store))
	wantSize807(t, "/api/nodes", list, 3)
	wantSize807(t, "/api/nodes/{pk}", detail, 3)
}

// The recomputer refreshes the snapshot every interval: an advert stored
// after Start is picked up without a restart (v0.2.9 review nit, #809 GM1).
func TestLastKnownHashSize807_PeriodicRefresh(t *testing.T) {
	db := setupTestDB(t)
	seed807Node(t, db)
	store := NewPacketStore(db, nil)
	stop := store.StartLastKnownAdvertRecomputer(20 * time.Millisecond)
	defer stop()
	if got := store.LastKnownAdverts()[pk807].HashSize; got != 0 {
		t.Fatalf("size before any advert = %d, want 0", got)
	}
	seed807Advert(t, db, "h807tick", "1140aabbcc", RouteFlood, time.Hour) // 0x40 → 2-byte
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if store.LastKnownAdverts()[pk807].HashSize == 2 {
			return
		}
	}
	t.Fatalf("snapshot never refreshed: size %d, want 2", store.LastKnownAdverts()[pk807].HashSize)
}

// A newer zero-hop direct advert whose path byte is 0x00 says nothing about
// the size (pre-#3293 firmware); the older flood advert's size is used. The
// same rule as computeNodeHashSizeInfo, for both direct route types.
func TestLastKnownHashSize807_SkipsSizelessZeroHopAdverts(t *testing.T) {
	db := setupTestDB(t)
	seed807Node(t, db)
	seed807Advert(t, db, "h807a", "1140aabbcc", RouteFlood, 50*time.Hour)                 // 2-byte
	seed807Advert(t, db, "h807b", "1200aabbcc", RouteDirect, 40*time.Hour)                // direct, 0x00
	seed807Advert(t, db, "h807c", "13aabbccdd00aabb", RouteTransportDirect, 30*time.Hour) // transport direct, 0x00
	got, err := db.LoadLastKnownAdverts()
	if err != nil {
		t.Fatal(err)
	}
	if got[pk807].HashSize != 2 {
		t.Fatalf("last known size = %d, want 2 (sizeless zero-hop adverts skipped)", got[pk807].HashSize)
	}

	// A zero-hop direct advert that declares its size (0x80) is used.
	seed807Advert(t, db, "h807d", "1280aabbcc", RouteDirect, 20*time.Hour)
	got, err = db.LoadLastKnownAdverts()
	if err != nil {
		t.Fatal(err)
	}
	if got[pk807].HashSize != 3 {
		t.Fatalf("last known size = %d, want 3 (newest size-declaring advert)", got[pk807].HashSize)
	}
}

// The newest advert wins (id order), and transport flood reads the path byte
// at offset 5.
func TestLastKnownHashSize807_NewestAdvertWins(t *testing.T) {
	db := setupTestDB(t)
	seed807Node(t, db)
	seed807Advert(t, db, "h807e", "1180aabbcc", RouteFlood, 50*time.Hour)                // 3-byte
	seed807Advert(t, db, "h807f", "10aabbccdd40aabb", RouteTransportFlood, 40*time.Hour) // 2-byte at offset 5
	got, err := db.LoadLastKnownAdverts()
	if err != nil {
		t.Fatal(err)
	}
	if got[pk807].HashSize != 2 {
		t.Fatalf("last known size = %d, want 2", got[pk807].HashSize)
	}
}

// The window's value wins, and a remembered size never touches the
// inconsistency fields.
func TestEnrichNodeWithLastKnownHashSize807(t *testing.T) {
	windowNode := map[string]interface{}{"hash_size": 1, "hash_size_inconsistent": true}
	EnrichNodeWithLastKnownHashSize(windowNode, 2)
	if windowNode["hash_size"] != 1 || windowNode["hash_size_inconsistent"] != true {
		t.Fatalf("window value must win: %v", windowNode)
	}

	unknown := map[string]interface{}{"hash_size": nil, "hash_size_inconsistent": false}
	EnrichNodeWithLastKnownHashSize(unknown, 0)
	if unknown["hash_size"] != nil {
		t.Fatalf("size 0 means unknown, must stay nil: %v", unknown)
	}
	EnrichNodeWithLastKnownHashSize(unknown, 2)
	if unknown["hash_size"] != 2 || unknown["hash_size_inconsistent"] != false {
		t.Fatalf("remembered size not applied cleanly: %v", unknown)
	}
	if _, ok := unknown["hash_sizes_seen"]; ok {
		t.Fatalf("hash_sizes_seen must not be set from a remembered size: %v", unknown)
	}
}

// Older databases without transmissions.from_pubkey: the load fails cleanly
// and the snapshot stays empty (no panic, no crash at startup).
func TestLastKnownHashSize807_NoFromPubkeyColumn(t *testing.T) {
	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.Exec(`CREATE TABLE nodes (public_key TEXT PRIMARY KEY);
		CREATE TABLE transmissions (id INTEGER PRIMARY KEY, raw_hex TEXT, route_type INTEGER, payload_type INTEGER);
		INSERT INTO nodes VALUES ('` + pk807 + `');`); err != nil {
		t.Fatal(err)
	}
	db := &DB{conn: conn}
	if _, err := db.LoadLastKnownAdverts(); err == nil {
		t.Fatal("expected an error without from_pubkey")
	}
	store := NewPacketStore(db, nil)
	store.refreshLastKnownAdverts()
	if got := store.LastKnownAdverts(); len(got) != 0 {
		t.Fatalf("snapshot must stay empty, got %v", got)
	}
}

// Perf proof (#807, AGENTS.md rule 0). Synthetic realistic shape: 2,000
// nodes, 210,000 transmissions of which 1/7 are ADVERTs (half of them
// sizeless zero-hop direct), plus one advert spammer with 20,000 sizeless
// zero-hop adverts newer than its last flood advert (the walk's worst case).
// Set CORESCOPE_807_BENCH_DB to a database file to run against it instead
// (opened read-only, as the server does).
func BenchmarkLoadLastKnownAdvertHashSizes807(b *testing.B) {
	var db *DB
	if path := os.Getenv("CORESCOPE_807_BENCH_DB"); path != "" {
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
		for i := 1; i <= 2000; i++ {
			tx.Exec(`INSERT INTO nodes (public_key, role) VALUES (?, 'repeater')`, pk(i))
		}
		ins, _ := tx.Prepare(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, from_pubkey) VALUES (?, ?, '2026-11-15T00:00:00Z', ?, ?, ?)`)
		for i := 1; i <= 210000; i++ {
			raw, rt, pt, from := "1502aabbccddeeff", 1, 5, interface{}(nil)
			if i%7 == 0 {
				pt, from = 4, pk(i%2000+1)
				if i%2 == 0 {
					raw, rt = "1200aabbccddeeff", 2
				} else {
					raw = "1140aabbccddeeff"
				}
			}
			ins.Exec(raw, fmt.Sprintf("%016x", i), rt, pt, from)
		}
		for i := 0; i < 20000; i++ {
			ins.Exec("1200aabbccddeeff", fmt.Sprintf("s%015x", i), 2, 4, pk(1))
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
