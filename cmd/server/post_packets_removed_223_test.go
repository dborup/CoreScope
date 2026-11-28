package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// POST /api/packets (#223) inserted into transmissions, observers and
// observations on the server's mode=ro handle (#1283), so every call failed
// with a 500 carrying the raw SQLite error. It was removed: ingest goes
// through MQTT and cmd/ingestor. These tests pin what is left; the source
// guard against packet-table writes is TestServerHasNoPacketTableWrites
// (readonly_invariant_test.go).

const removedPostAPIKey = "test-secret-key-strong-enough"

// removedPostPacketBody is a valid FLOOD/ADVERT body that the old handler
// accepted (it decoded, then failed on the INSERT).
const removedPostPacketBody = `{"hex":"110011223344556677889900AABBCCDD","observer":"obs1","snr":5.5,"rssi":-72}`

// readOnlyPacketServer seeds a file DB, opens it the way main.go does
// (OpenDB, mode=ro) and registers the API routes with a valid API key.
func readOnlyPacketServer(t *testing.T) (dbPath string, router *mux.Router) {
	t.Helper()
	dbPath = filepath.Join(t.TempDir(), "ro.db")
	now := time.Now().UTC()
	seedTestDBRows(t, dbPath, 3, 2, func(i int) (string, int64) {
		ts := now.Add(-time.Duration(i) * time.Minute)
		return ts.Format(time.RFC3339), ts.Unix()
	})
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.conn.Close() })
	srv := NewServer(db, &Config{Port: 3000, APIKey: removedPostAPIKey}, NewHub())
	router = mux.NewRouter()
	srv.RegisterRoutes(router)
	return dbPath, router
}

// packetTableCounts reads the row counts of the tables the old handler wrote,
// through a separate connection.
func packetTableCounts(t *testing.T, dbPath string) map[string]int {
	t.Helper()
	conn, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	out := map[string]int{}
	for _, table := range []string{"transmissions", "observations", "observers"} {
		var n int
		if err := conn.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		out[table] = n
	}
	return out
}

func postRemovedPacket(router http.Handler) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/packets", strings.NewReader(removedPostPacketBody))
	req.Header.Set("X-API-Key", removedPostAPIKey)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// On the API router, POST /api/packets now hits the GET route's path with the
// wrong method: gorilla/mux answers 405 before any handler or DB access. A
// valid key and a decodable body make no difference, and the read-only DB is
// left untouched.
func TestPostPacketsRemovedReturns405OnReadOnlyDB(t *testing.T) {
	dbPath, router := readOnlyPacketServer(t)
	before := packetTableCounts(t, dbPath)

	w := postRemovedPacket(router)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/packets: want 405, got %d (body: %q)", w.Code, w.Body.String())
	}
	assertAllowSet(t, w.Header().Get("Allow"), "GET", "HEAD")
	if body := strings.ToLower(w.Body.String()); strings.Contains(body, "sqlite") || strings.Contains(body, "readonly") || strings.Contains(body, "insert") {
		t.Errorf("response leaks database error text: %q", w.Body.String())
	}
	if after := packetTableCounts(t, dbPath); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("packet tables changed: before %v, after %v", before, after)
	}
}

// The other /api/packets routes keep their registration. Matching does not
// run handlers, so this pins only the routing, not handler output.
func TestPacketsRoutesSurviveRemoval(t *testing.T) {
	_, router := readOnlyPacketServer(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/packets"},
		{"GET", "/api/packets/timestamps"},
		{"POST", "/api/packets/observations"},
		{"GET", "/api/packets/abc123"},
		{"GET", "/api/packets/abc123/path"},
		{"POST", "/api/decode"},
	} {
		var m mux.RouteMatch
		if !router.Match(httptest.NewRequest(c.method, c.path, nil), &m) || m.MatchErr != nil {
			t.Errorf("%s %s: no longer routed (err %v)", c.method, c.path, m.MatchErr)
		}
	}
}

// The served OpenAPI spec keeps GET /api/packets and drops the POST.
func TestOpenAPISpecHasNoPostPackets(t *testing.T) {
	_, router := readOnlyPacketServer(t)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/spec", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/spec: want 200, got %d", w.Code)
	}
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &spec); err != nil {
		t.Fatal(err)
	}
	ops := spec.Paths["/api/packets"]
	if _, ok := ops["get"]; !ok {
		t.Errorf("/api/packets lost its get operation: %v", ops)
	}
	if _, ok := ops["post"]; ok {
		t.Errorf("/api/packets still documents a post operation")
	}
}

// main.go mounts a catch-all SPA handler after the API routes. Before #233,
// gorilla/mux let that catch-all win over the method mismatch, so a POST to
// the removed endpoint was served index.html like any other unmatched path.
// #233 adds a JSON /api/ fallback (registerAPIFallback, api_fallback.go)
// ahead of the SPA catch-all, so this now pins 405 with an Allow header,
// not the SPA page. See api_fallback_test.go for the general-purpose
// coverage; this test keeps the #223/#231 read-only-DB angle (nothing
// written) on this specific endpoint.
func TestPostPacketsRemovedReturns405NotSPAInProductionRouter(t *testing.T) {
	dbPath, router := readOnlyPacketServer(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>SPA</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	router.PathPrefix("/").Handler(wsOrStatic(NewHub(), spaHandler(dir, http.FileServer(http.Dir(dir)))))
	before := packetTableCounts(t, dbPath)

	w := postRemovedPacket(router)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/packets: want 405, got %d %q %q",
			w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	assertAllowSet(t, w.Header().Get("Allow"), "GET", "HEAD")
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Errorf("want application/json content-type, got %q", w.Header().Get("Content-Type"))
	}
	if after := packetTableCounts(t, dbPath); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("packet tables changed: before %v, after %v", before, after)
	}
}
