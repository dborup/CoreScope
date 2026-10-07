package main

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gorilla/mux"
	_ "modernc.org/sqlite"
)

// Synthetic, reproducible datasets for the Reach leaderboard benchmarks.
// Each node gets edgesPerNode random neighbours (fixed seed, canonical a<b,
// duplicates dropped), 2% of nodes also have an observer row, and the DB is a
// file on disk (b.TempDir) like production rather than :memory:.
var reachRankBenchSets = []struct {
	name         string
	nodes        int
	edgesPerNode int
}{
	{"small_1k", 1000, 4},
	{"medium_5k", 5000, 4},
	{"large_20k", 20000, 5},
}

func benchRankDB(b *testing.B, nodes, edgesPerNode int) (*DB, []string) {
	b.Helper()
	conn, err := sql.Open("sqlite", filepath.Join(b.TempDir(), "rank.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { conn.Close() })
	for _, s := range []string{
		`CREATE TABLE nodes (public_key TEXT PRIMARY KEY, name TEXT, role TEXT, lat REAL, lon REAL, last_seen TEXT, first_seen TEXT, advert_count INTEGER DEFAULT 0)`,
		`CREATE TABLE observers (id TEXT PRIMARY KEY, name TEXT)`,
		`CREATE TABLE transmissions (id INTEGER PRIMARY KEY, from_pubkey TEXT, payload_type INTEGER)`,
		`CREATE TABLE observations (id INTEGER PRIMARY KEY, transmission_id INTEGER, observer_idx INTEGER, snr REAL, path_json TEXT, timestamp INTEGER)`,
		`CREATE TABLE neighbor_edges (node_a TEXT NOT NULL, node_b TEXT NOT NULL, count INTEGER DEFAULT 1, last_seen TEXT, PRIMARY KEY (node_a, node_b))`,
	} {
		if _, err := conn.Exec(s); err != nil {
			b.Fatal(err)
		}
	}
	rng := rand.New(rand.NewSource(1))
	pks := make([]string, nodes)
	for i := range pks {
		pks[i] = fmt.Sprintf("%016x%048x", rng.Uint64(), i)
	}
	tx, err := conn.Begin()
	if err != nil {
		b.Fatal(err)
	}
	for i, pk := range pks {
		if _, err := tx.Exec(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen) VALUES (?, ?, 'repeater', 56.1, 10.2, '2026-09-01T00:00:00Z', '2026-06-01T00:00:00Z')`,
			pk, fmt.Sprintf("Node-%05d", i)); err != nil {
			b.Fatal(err)
		}
		if i%50 == 0 {
			if _, err := tx.Exec(`INSERT INTO observers (id, name) VALUES (UPPER(?), ?)`, pk, fmt.Sprintf("Obs-%05d", i)); err != nil {
				b.Fatal(err)
			}
		}
		for k := 0; k < edgesPerNode; k++ {
			a, c := pk, pks[rng.Intn(i+1)]
			if a == c {
				continue
			}
			if a > c {
				a, c = c, a
			}
			if _, err := tx.Exec(`INSERT OR IGNORE INTO neighbor_edges (node_a, node_b, last_seen) VALUES (?, ?, '2026-09-01T00:00:00Z')`, a, c); err != nil {
				b.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return &DB{conn: conn}, pks
}

func benchRankServer(db *DB) *Server {
	cfg := &Config{HiddenNamePrefixes: []string{"🚫"}}
	return &Server{store: &PacketStore{db: db}, db: db, cfg: cfg, perfStats: NewPerfStats()}
}

func (s *Server) benchDropDegreeSnapshot() {
	s.reach.degreeMu.Lock()
	s.reach.degreeSnap, s.reach.rankView = nil, nil
	s.reach.degreeMu.Unlock()
}

// BenchmarkReachRankColdBuild: a snapshot rebuild (three bulk queries) plus
// the ranked view — what one request pays once per 60s TTL.
func BenchmarkReachRankColdBuild(b *testing.B) {
	for _, ds := range reachRankBenchSets {
		b.Run(ds.name, func(b *testing.B) {
			db, _ := benchRankDB(b, ds.nodes, ds.edgesPerNode)
			srv := benchRankServer(db)
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				srv.benchDropDegreeSnapshot()
				v, err := srv.reachRankView(ctx)
				if err != nil || len(v.rows) == 0 {
					b.Fatalf("view: %v", err)
				}
			}
		})
	}
}

// BenchmarkReachRankViewRebuild: the CPU-only re-rank after a blacklist /
// hidden-prefix change (no DB).
func BenchmarkReachRankViewRebuild(b *testing.B) {
	for _, ds := range reachRankBenchSets {
		b.Run(ds.name, func(b *testing.B) {
			db, _ := benchRankDB(b, ds.nodes, ds.edgesPerNode)
			srv := benchRankServer(db)
			snap, err := srv.loadDegreeSnapshot(context.Background())
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if v := buildReachRankView(snap, srv.cfg, uint64(i+1), 0, 0); len(v.rows) == 0 {
					b.Fatal("empty view")
				}
			}
		})
	}
}

// BenchmarkReachRankWarm: full HTTP handler on a warm snapshot — a plain page,
// a deep page, a broad name search, and a pubkey search.
func BenchmarkReachRankWarm(b *testing.B) {
	for _, ds := range reachRankBenchSets {
		db, pks := benchRankDB(b, ds.nodes, ds.edgesPerNode)
		srv := benchRankServer(db)
		cases := []struct{ name, path string }{
			{"page1", "/api/reach-rank"},
			{"deep_page", fmt.Sprintf("/api/reach-rank?offset=%d", ds.nodes/2)},
			{"search_name", "/api/reach-rank?q=node-01"},
			{"search_pubkey", "/api/reach-rank?q=" + pks[ds.nodes/3][:12]},
		}
		router := mux.NewRouter()
		router.HandleFunc("/api/reach-rank", srv.handleReachRank).Methods("GET")
		get := func(path string) int {
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
			return rr.Code
		}
		for _, c := range cases {
			b.Run(ds.name+"/"+c.name, func(b *testing.B) {
				if code := get(c.path); code != http.StatusOK {
					b.Fatalf("warm-up %s: %d", c.path, code)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if code := get(c.path); code != http.StatusOK {
						b.Fatalf("%s: %d", c.path, code)
					}
				}
			})
		}
	}
}

// BenchmarkNodeReachCacheHit: /api/nodes/{pk}/reach served from the response
// cache. Uses only APIs that also exist on master so the same benchmark can
// run before/after: the branch adds the shared rank-view lookup to this path.
func BenchmarkNodeReachCacheHit(b *testing.B) {
	db, pks := benchRankDB(b, 1000, 4)
	srv := benchRankServer(db)
	path := "/api/nodes/" + pks[0] + "/reach?days=7"
	if rr := serveReach(srv, path); rr.Code != http.StatusOK {
		b.Fatalf("warm-up: %d %s", rr.Code, rr.Body.String())
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if rr := serveReach(srv, path); rr.Code != http.StatusOK {
			b.Fatalf("status %d", rr.Code)
		}
	}
}

// BenchmarkReachAndRankParallel: concurrent GOMAXPROCS goroutines hitting a
// warm cache/view — 3 parts /api/reach-rank (page1) to 1 part
// /api/nodes/{pk}/reach, resembling the leaderboard driving traffic to
// individual Reach pages. Confirms the shared snapshot/view mutex is not a
// bottleneck under concurrent readers.
func BenchmarkReachAndRankParallel(b *testing.B) {
	for _, ds := range reachRankBenchSets {
		b.Run(ds.name, func(b *testing.B) {
			db, pks := benchRankDB(b, ds.nodes, ds.edgesPerNode)
			srv := benchRankServer(db)
			router := mux.NewRouter()
			router.HandleFunc("/api/nodes/{pubkey}/reach", srv.handleNodeReach).Methods("GET")
			router.HandleFunc("/api/reach-rank", srv.handleReachRank).Methods("GET")
			get := func(path string) int {
				rr := httptest.NewRecorder()
				router.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
				return rr.Code
			}
			reachPath := "/api/nodes/" + pks[0] + "/reach?days=7"
			if code := get("/api/reach-rank"); code != http.StatusOK {
				b.Fatalf("warm-up rank: %d", code)
			}
			if code := get(reachPath); code != http.StatusOK {
				b.Fatalf("warm-up reach: %d", code)
			}
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					i++
					path := "/api/reach-rank"
					if i%4 == 0 {
						path = reachPath
					}
					if code := get(path); code != http.StatusOK {
						b.Fatalf("%s: %d", path, code)
					}
				}
			})
		})
	}
}
