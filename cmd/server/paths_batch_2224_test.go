package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func batch2224Store(t testing.TB, counting bool) *PacketStore {
	t.Helper()
	driverName := "sqlite"
	if counting {
		registerCountingDriver()
		clearBulkTestFailAfterQueries()
		driverName = "sqlite-counting-ping-score-bulk"
	}
	conn, err := sql.Open(driverName, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { conn.Close(); clearBulkTestFailAfterQueries() })
	if _, err := conn.Exec(`CREATE TABLE observations (id INTEGER PRIMARY KEY, transmission_id INTEGER, path_json TEXT, resolved_path TEXT); CREATE INDEX observations_tx ON observations(transmission_id)`); err != nil {
		t.Fatal(err)
	}
	s := &PacketStore{db: &DB{conn: conn}}
	s.initResolvedPathIndex()
	return s
}

func seed2224(t testing.TB, s *PacketStore, n int, fallback bool) map[int][]*StoreObs {
	t.Helper()
	stmt, err := s.db.conn.Prepare(`INSERT INTO observations VALUES (?,?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	out := make(map[int][]*StoreObs, n)
	for id := 1; id <= n; id++ {
		first := 2*id - 1
		var primary interface{} = `["primary"]`
		if fallback {
			primary = nil
		}
		if _, err := stmt.Exec(first, id, `["aa","bb"]`, primary); err != nil {
			t.Fatal(err)
		}
		if _, err := stmt.Exec(first+1, id, `["cc"]`, `["fallback"]`); err != nil {
			t.Fatal(err)
		}
		out[id] = []*StoreObs{{ID: first, PathJSON: `["aa","bb"]`}, {ID: first + 1, PathJSON: `["cc"]`}}
	}
	return out
}

func TestCanonicalPaths2224BatchCount(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprint(fallback), func(t *testing.T) {
			s := batch2224Store(t, true)
			const n = 1001 // exceeds both 500 and SQLite's older 999-variable budget
			snapshots := seed2224(t, s, n, fallback)
			resetBulkTestQueryLog()
			for tx, obs := range snapshots {
				s.fetchResolvedPathForTxBestAt(tx, obs, s.lruNow())
			}
			wantScalar := n
			if fallback {
				wantScalar *= 2
			}
			if count := len(bulkTestQueryLog()); count != wantScalar {
				t.Fatalf("scalar instrumentation captured %d queries, want %d", count, wantScalar)
			}
			resetLRU(s)
			resetBulkTestQueryLog()
			got := s.loadCanonicalResolvedPathsAt(snapshots, s.lruNow())
			wantQueries := 3
			if fallback {
				wantQueries = 6
			}
			if queries := bulkTestQueryLog(); len(queries) != wantQueries {
				t.Fatalf("%d candidates: %d SQL queries, want %d batches", n, len(queries), wantQueries)
			} else {
				for _, q := range queries {
					if q.argCount > 500 {
						t.Fatalf("unbounded SQL arguments: %d", q.argCount)
					}
				}
			}
			if len(got) != n {
				t.Fatalf("got %d paths, want %d", len(got), n)
			}
			want := "primary"
			if fallback {
				want = "fallback"
			}
			for tx, rp := range got {
				if len(rp) != 1 || rp[0] == nil || *rp[0] != want {
					t.Fatalf("tx %d: %+v, want %s", tx, rp, want)
				}
			}
		})
	}
}

func TestNodePaths2224UsesBatches(t *testing.T) {
	srv, router := setupTestServer(t)
	const n = 1001
	for i := 0; i < n; i++ {
		txID := seedConfirmTx(t, srv, fmt.Sprintf("batch2224_%d", i), `["aa"]`, `["`+confirmTestTarget+`"]`)
		if _, err := srv.db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp) VALUES (?,1,'["aa","bb"]',?)`, txID, time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
	}
	// Use the existing real-driver query recorder against a private copy of
	// this full route fixture. No production connection or live host is used.
	file := filepath.Join(t.TempDir(), "paths.db")
	if _, err := srv.db.conn.Exec(`VACUUM INTO ?`, file); err != nil {
		t.Fatal(err)
	}
	registerCountingDriver()
	conn, err := sql.Open("sqlite-counting-ping-score-bulk", file)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	old := srv.db.conn
	srv.db.conn = conn
	t.Cleanup(func() { conn.Close(); old.Close() })
	reloadConfirmStore(t, srv)
	resetBulkTestQueryLog()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/nodes/"+confirmTestTarget+"/paths", nil))
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var response confirmPathsResp
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.TotalTransmissions < n {
		t.Fatalf("response lost candidates: total %d, err %v", response.TotalTransmissions, err)
	}
	batches := 0
	for _, q := range bulkTestQueryLog() {
		if strings.Contains(q.sql, "SELECT resolved_path FROM observations WHERE id =") || strings.Contains(q.sql, "SELECT id, resolved_path FROM observations WHERE transmission_id =") {
			t.Fatalf("handler still uses per-transmission SQL: %s", q.sql)
		}
		if strings.Contains(q.sql, "SELECT id, resolved_path FROM observations WHERE id IN") || strings.Contains(q.sql, "SELECT id, transmission_id, resolved_path FROM observations") {
			batches++
		}
	}
	if batches < 6 || batches > 8 {
		t.Fatalf("handler resolved paths in %d batches, want six plus at most baseline fixture chunks", batches)
	}
}

// A real SQLite driver wrapper injects failure only into the new IN batches.
// Scalar fallback queries must still succeed; it also exercises rows.Err and
// Scan failures after a batch has already produced one valid row.
type fault2224Driver struct {
	inner  driver.Driver
	mode   string
	faults atomic.Int32
}
type fault2224Conn struct {
	driver.Conn
	owner *fault2224Driver
}
type fault2224Rows struct {
	driver.Rows
	mode string
	read bool
}

var fault2224Names atomic.Int64

func (d *fault2224Driver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &fault2224Conn{Conn: c, owner: d}, nil
}
func (c *fault2224Conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	batch := strings.Contains(query, " IN (")
	if batch && (c.owner.mode == "primary" && strings.Contains(query, "WHERE id IN") || c.owner.mode == "fallback" && strings.Contains(query, "transmission_id IN")) {
		c.owner.faults.Add(1)
		return nil, errors.New("injected batch query failure")
	}
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err == nil && batch && (c.owner.mode == "rows" || c.owner.mode == "scan") {
		c.owner.faults.Add(1)
		return &fault2224Rows{Rows: rows, mode: c.owner.mode}, nil
	}
	return rows, err
}
func (r *fault2224Rows) Next(values []driver.Value) error {
	if r.read {
		if r.mode == "scan" {
			values[0] = "not-an-integer"
			return nil
		}
		return errors.New("injected row iteration failure")
	}
	r.read = true
	return r.Rows.Next(values)
}
func TestCanonicalPaths2224BatchFailureFallsBack(t *testing.T) {
	for _, mode := range []string{"primary", "fallback", "rows", "scan"} {
		t.Run(mode, func(t *testing.T) {
			s := batch2224Store(t, false)
			inner := s.db.conn.Driver()
			// Replace only this test's empty connection, never a shared/global DB.
			s.db.conn.Close()
			d := &fault2224Driver{inner: inner, mode: mode}
			name := fmt.Sprintf("sqlite-paths2224-fault-%d", fault2224Names.Add(1))
			sql.Register(name, d)
			conn, err := sql.Open(name, ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			conn.SetMaxOpenConns(1)
			s.db.conn = conn
			t.Cleanup(func() { conn.Close() })
			if _, err := conn.Exec(`CREATE TABLE observations (id INTEGER PRIMARY KEY, transmission_id INTEGER, path_json TEXT, resolved_path TEXT)`); err != nil {
				t.Fatal(err)
			}
			inputs := seed2224(t, s, 3, true)
			got := s.loadCanonicalResolvedPathsAt(inputs, s.lruNow())
			if d.faults.Load() == 0 {
				t.Fatal("fault was not exercised")
			}
			if len(got) != 3 {
				t.Fatalf("failed batch lost scalar fallback rows: %v", got)
			}
			for tx, rp := range got {
				if len(rp) != 1 || rp[0] == nil || *rp[0] != "fallback" {
					t.Fatalf("tx %d: wrong recovered result", tx)
				}
			}
		})
	}
}

func TestCanonicalPaths2224MatchesBestAt(t *testing.T) {
	cases := []struct {
		name   string
		paths  []string
		stored []interface{}
		want   []*string
	}{
		{"normal", []string{`["a","b"]`, `["a"]`}, []interface{}{`["long"]`, `["short"]`}, []*string{str2224("long")}},
		{"null primary", []string{`["a","b"]`, `["a"]`}, []interface{}{nil, `["short"]`}, []*string{str2224("short")}},
		{"empty string primary", []string{`["a","b"]`, `["a"]`}, []interface{}{``, `["short"]`}, []*string{str2224("short")}},
		{"malformed primary", []string{`["a","b"]`, `["a"]`}, []interface{}{`invalid`, `["short"]`}, []*string{str2224("short")}},
		{"null JSON primary", []string{`["a","b"]`, `["a"]`}, []interface{}{`null`, `["short"]`}, []*string{str2224("short")}},
		{"empty array primary", []string{`["a","b"]`, `["a"]`}, []interface{}{`[]`, `["short"]`}, []*string{}},
		{"nil entries", []string{`["a","b"]`}, []interface{}{`[null,"node"]`}, []*string{nil, str2224("node")}},
		{"snapshot tie order", []string{`["b"]`, `["a"]`}, []interface{}{`["second-id-first"]`, `["first-id-second"]`}, []*string{str2224("second-id-first")}},
		{"fallback tie order", []string{`["a","b"]`, `["b"]`, `["a"]`}, []interface{}{nil, `["second-id-first"]`, `["first-id-second"]`}, []*string{str2224("second-id-first")}},
		{"nothing stored", []string{`["a","b"]`, `["a"]`}, []interface{}{nil, nil}, nil},
		{"malformed raw path", []string{`invalid`, `["a"]`}, []interface{}{`["bad-raw"]`, `["good-raw"]`}, []*string{str2224("good-raw")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := batch2224Store(t, false)
			var obs []*StoreObs
			for i, raw := range c.paths {
				id := 10 - i // snapshot order deliberately differs from SQLite PK order
				if _, err := s.db.conn.Exec(`INSERT INTO observations VALUES (?,?,?,?)`, id, 7, raw, c.stored[i]); err != nil {
					t.Fatal(err)
				}
				obs = append(obs, &StoreObs{ID: id, PathJSON: raw})
			}
			// A longer stored path missing from the immutable snapshot must not win.
			if _, err := s.db.conn.Exec(`INSERT INTO observations VALUES (99,7,'["x","y","z"]','["not-in-snapshot"]')`); err != nil {
				t.Fatal(err)
			}
			now := s.lruNow()
			want := s.fetchResolvedPathForTxBestAt(7, obs, now)
			resetLRU(s)
			got := s.loadCanonicalResolvedPathsAt(map[int][]*StoreObs{7: obs, 8: nil}, now)[7]
			if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(got, c.want) {
				t.Fatalf("batch=%v scalar=%v expected=%v", deref2224(got), deref2224(want), deref2224(c.want))
			}
		})
	}
}

func str2224(s string) *string { return &s }
func deref2224(rp []*string) string {
	var b strings.Builder
	for _, p := range rp {
		if p == nil {
			b.WriteString("<nil>,")
		} else {
			b.WriteString(*p + ",")
		}
	}
	return b.String()
}

func TestCanonicalPaths2224LRUAndErrors(t *testing.T) {
	s := batch2224Store(t, true)
	snapshots := seed2224(t, s, 2, false)
	advance := fakeLRUClock(s)
	s.loadCanonicalResolvedPathsAt(snapshots, s.lruNow())
	if _, err := s.db.conn.Exec(`UPDATE observations SET resolved_path='["changed"]'`); err != nil {
		t.Fatal(err)
	}
	resetBulkTestQueryLog()
	got := s.loadCanonicalResolvedPathsAt(snapshots, s.lruNow())
	if len(bulkTestQueryLog()) != 0 || *got[1][0] != "primary" {
		t.Fatal("warm LRU was not used")
	}
	advance(resolvedPathLRUTTL)
	got = s.loadCanonicalResolvedPathsAt(snapshots, s.lruNow())
	if *got[1][0] != "primary" {
		t.Fatal("TTL boundary expired at equality instead of strictly after")
	}
	advance(time.Nanosecond)
	got = s.loadCanonicalResolvedPathsAt(snapshots, s.lruNow())
	if *got[1][0] != "changed" {
		t.Fatal("expired same-row overwrite remained stale")
	}
	resetLRU(s)
	setBulkTestFailAfterQueries(0)
	if got := s.loadCanonicalResolvedPathsAt(snapshots, s.lruNow()); len(got) != 0 {
		t.Fatal("query errors fabricated canonical paths")
	}
	clearBulkTestFailAfterQueries()
	if got := s.loadCanonicalResolvedPathsAt(snapshots, s.lruNow()); len(got) != 2 {
		t.Fatal("failed batch poisoned subsequent recovery")
	}
	if got := (&PacketStore{}).loadCanonicalResolvedPathsAt(snapshots, 0); len(got) != 0 {
		t.Fatal("nil DB fabricated paths")
	}
}

func TestCanonicalPaths2224HistoricalRowsNotDecoded(t *testing.T) {
	s := batch2224Store(t, false)
	inputs := seed2224(t, s, 20, true)
	largePath := `[` + strings.TrimSuffix(strings.Repeat(`"outside-snapshot",`, 30), ",") + `]`
	for tx := 1; tx <= 20; tx++ {
		for extra := 0; extra < 40; extra++ {
			if _, err := s.db.conn.Exec(`INSERT INTO observations VALUES (?,?,?,?)`, 1000+40*(tx-1)+extra, tx, `["aa","bb","cc"]`, largePath); err != nil {
				t.Fatal(err)
			}
		}
	}
	before := testing.AllocsPerRun(1, func() {
		resetLRU(s)
		out := make(map[int][]*string, len(inputs))
		for id, obs := range inputs {
			out[id] = s.fetchResolvedPathForTxBestAt(id, obs, s.lruNow())
		}
		batch2224Sink = out
	})
	after := testing.AllocsPerRun(1, func() {
		resetLRU(s)
		batch2224Sink = s.loadCanonicalResolvedPathsAt(inputs, s.lruNow())
	})
	defer func() { batch2224Sink = nil }()
	// No wall-clock budget: the regression is retaining/decoding known-
	// irrelevant rows. The real driver still allocates their scanned strings.
	if after >= before/2 {
		t.Fatalf("historical paths still decoded: %.0f allocations, scalar %.0f (want < half)", after, before)
	}
	if len(batch2224Sink) != len(inputs) {
		t.Fatalf("allocation guard lost paths: got %d, want %d", len(batch2224Sink), len(inputs))
	}
	for tx, rp := range batch2224Sink {
		if len(rp) != 1 || rp[0] == nil || *rp[0] != "fallback" {
			t.Fatalf("tx %d: historical row won", tx)
		}
	}
	t.Logf("allocations with 800 out-of-snapshot rows: scalar %.0f, batched %.0f", before, after)
}

func BenchmarkCanonicalPaths2224(b *testing.B) {
	for _, n := range []int{1001, 30000} {
		for _, fallback := range []bool{false, true} {
			for _, batched := range []bool{false, true} {
				b.Run(fmt.Sprintf("tx=%d/fallback=%t/batch=%t", n, fallback, batched), func(b *testing.B) {
					s := batch2224Store(b, false)
					inputs := seed2224(b, s, n, fallback)
					// Five observations per transmission, while the original
					// primary remains longest and the first shorter remains best.
					for tx := 1; tx <= n; tx++ {
						for extra := 0; extra < 3; extra++ {
							id := 2*n + 3*(tx-1) + extra + 1
							if _, err := s.db.conn.Exec(`INSERT INTO observations VALUES (?,?,?,?)`, id, tx, `[]`, `["extra"]`); err != nil {
								b.Fatal(err)
							}
							inputs[tx] = append(inputs[tx], &StoreObs{ID: id, PathJSON: `[]`})
						}
					}
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						resetLRU(s)
						now := s.lruNow()
						if batched {
							batch2224Sink = s.loadCanonicalResolvedPathsAt(inputs, now)
						} else {
							out := make(map[int][]*string, len(inputs))
							for id, obs := range inputs {
								if rp := s.fetchResolvedPathForTxBestAt(id, obs, now); rp != nil {
									out[id] = rp
								}
							}
							batch2224Sink = out
						}
					}
					b.StopTimer()
					batch2224Sink = nil
				})
			}
		}
	}
}

var batch2224Sink map[int][]*string

func BenchmarkCanonicalPaths2224HistoricalFanout(b *testing.B) {
	for _, batched := range []bool{false, true} {
		b.Run(fmt.Sprint(batched), func(b *testing.B) {
			s := batch2224Store(b, false)
			inputs := seed2224(b, s, 100, true)
			largePath := `[` + strings.TrimSuffix(strings.Repeat(`"outside-snapshot",`, 30), ",") + `]`
			for tx := 1; tx <= 100; tx++ {
				for extra := 0; extra < 200; extra++ {
					if _, err := s.db.conn.Exec(`INSERT INTO observations VALUES (?,?,?,?)`, 10000+200*(tx-1)+extra, tx, `["aa","bb","cc"]`, largePath); err != nil {
						b.Fatal(err)
					}
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				resetLRU(s)
				now := s.lruNow()
				if batched {
					batch2224Sink = s.loadCanonicalResolvedPathsAt(inputs, now)
				} else {
					out := make(map[int][]*string, len(inputs))
					for id, obs := range inputs {
						if rp := s.fetchResolvedPathForTxBestAt(id, obs, now); rp != nil {
							out[id] = rp
						}
					}
					batch2224Sink = out
				}
			}
			b.StopTimer()
			batch2224Sink = nil
		})
	}
}
