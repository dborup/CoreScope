package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// #199: a 404 from /api/nodes/{pubkey} carries what the instance still knows
// about the key -- its inactive_nodes row and/or its observer row -- so the
// node page can explain the state instead of dead-ending on "Node not found".

const (
	issue199InactiveObs = "b199000000000000000000000000000000000000000000000000000000000001"
	issue199ObsOnly     = "b199000000000000000000000000000000000000000000000000000000000002"
	issue199Unknown     = "b199000000000000000000000000000000000000000000000000000000000003"
)

// Decoded locally (not via the server's types) so the contract is pinned by
// its JSON field names.
type issue199Inactive struct {
	PublicKey string `json:"public_key"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	LastSeen  string `json:"last_seen"`
}

type issue199Observer struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	LastSeen string `json:"last_seen"`
}

func issue199Get(t *testing.T, router *mux.Router, pubkey string) (int, map[string]json.RawMessage) {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/nodes/"+pubkey, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return w.Code, body
}

func issue199Seed(t *testing.T, srv *Server) {
	t.Helper()
	ensureInactiveNodesTable(t, srv)
	if _, err := srv.db.conn.Exec(`INSERT INTO inactive_nodes (public_key, name, role, last_seen, first_seen) VALUES (?, 'Quiet Repeater', 'repeater', '2026-09-24T15:35:00Z', '2026-09-11T00:00:00Z')`, issue199InactiveObs); err != nil {
		t.Fatal(err)
	}
	for _, o := range []struct{ id, name string }{
		{strings.ToUpper(issue199InactiveObs), "Quiet Observer"},
		{strings.ToUpper(issue199ObsOnly), "Listener Only"},
	} {
		if _, err := srv.db.conn.Exec(`INSERT INTO observers (id, name, last_seen, first_seen) VALUES (?, ?, '2026-10-04T04:23:00Z', '2026-09-11T00:00:00Z')`, o.id, o.name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNodeDetail404CarriesInactiveNodeAndObserver(t *testing.T) {
	srv, router := setupTestServer(t)
	issue199Seed(t, srv)

	code, body := issue199Get(t, router, issue199InactiveObs)
	if code != 404 {
		t.Fatalf("status=%d, want 404 (the node is not in nodes)", code)
	}
	var inactive issue199Inactive
	if err := json.Unmarshal(body["inactive_node"], &inactive); err != nil {
		t.Fatalf("inactive_node missing or malformed: %v (body %v)", err, body)
	}
	if inactive.PublicKey != issue199InactiveObs || inactive.Name != "Quiet Repeater" || inactive.Role != "repeater" || inactive.LastSeen != "2026-09-24T15:35:00Z" {
		t.Errorf("inactive_node=%+v", inactive)
	}
	var obs issue199Observer
	if err := json.Unmarshal(body["observer"], &obs); err != nil {
		t.Fatalf("observer missing or malformed: %v (body %v)", err, body)
	}
	if obs.ID != strings.ToUpper(issue199InactiveObs) || obs.Name != "Quiet Observer" || obs.LastSeen != "2026-10-04T04:23:00Z" {
		t.Errorf("observer=%+v", obs)
	}

	// Upper-case path (as an observer id) finds the same rows.
	if _, body := issue199Get(t, router, strings.ToUpper(issue199InactiveObs)); body["inactive_node"] == nil {
		t.Error("upper-case pubkey did not find the inactive_nodes row")
	}
}

func TestNodeDetail404ObserverWithoutNodeRecord(t *testing.T) {
	srv, router := setupTestServer(t)
	issue199Seed(t, srv)

	code, body := issue199Get(t, router, issue199ObsOnly)
	if code != 404 {
		t.Fatalf("status=%d, want 404", code)
	}
	if body["inactive_node"] != nil {
		t.Errorf("unexpected inactive_node: %s", body["inactive_node"])
	}
	var obs issue199Observer
	if err := json.Unmarshal(body["observer"], &obs); err != nil || obs.Name != "Listener Only" {
		t.Errorf("observer=%+v err=%v", obs, err)
	}
}

func TestNodeDetail404UnknownStaysBare(t *testing.T) {
	srv, router := setupTestServer(t)
	issue199Seed(t, srv)

	code, body := issue199Get(t, router, issue199Unknown)
	if code != 404 || len(body) != 1 || body["error"] == nil {
		t.Errorf("status=%d body=%v, want 404 with only an error field", code, body)
	}
}

// Without an inactive_nodes table (minimal schemas) the observer still counts.
func TestNodeDetail404WithoutInactiveTable(t *testing.T) {
	srv, router := setupTestServer(t)
	if _, err := srv.db.conn.Exec(`INSERT INTO observers (id, name, last_seen) VALUES (?, 'Listener Only', '2026-10-04T04:23:00Z')`, strings.ToUpper(issue199ObsOnly)); err != nil {
		t.Fatal(err)
	}
	code, body := issue199Get(t, router, issue199ObsOnly)
	if code != 404 || body["observer"] == nil || body["inactive_node"] != nil {
		t.Errorf("status=%d body=%v", code, body)
	}
}

// Blacklisted or hidden identities get the bare 404: nothing about the rows leaks.
func TestNodeDetail404HiddenIdentityStaysBare(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*Server)
	}{
		{"node blacklist", func(s *Server) { s.cfg.SetNodeBlacklist([]string{issue199InactiveObs}) }},
		{"observer blacklist", func(s *Server) { s.cfg.ObserverBlacklist = []string{strings.ToUpper(issue199InactiveObs)} }},
		{"hidden inactive name", func(s *Server) { s.cfg.SetHiddenNamePrefixes([]string{"Quiet Rep"}) }},
		{"hidden observer name", func(s *Server) { s.cfg.SetHiddenNamePrefixes([]string{"Quiet Obs"}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, router := setupTestServer(t)
			issue199Seed(t, srv)
			tc.setup(srv)
			code, body := issue199Get(t, router, issue199InactiveObs)
			if code != 404 || len(body) != 1 || body["error"] == nil {
				t.Errorf("status=%d body=%v, want bare 404", code, body)
			}
		})
	}
}

// #208 item 1: a failed lookup still answers the bare 404, but is logged --
// once at first, then at most once per missingNodeLogEvery -- so a broken
// lookup (schema drift on inactive_nodes) does not pass for a plain miss.
func TestNodeDetail404LookupErrorIsLoggedOnce(t *testing.T) {
	srv, router := setupTestServer(t)
	// Schema drift: an inactive_nodes without the columns the lookup reads.
	if _, err := srv.db.conn.Exec(`CREATE TABLE inactive_nodes (public_key TEXT PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	// The capture is locked (#310): the server's background goroutines keep
	// logging into whatever writer is installed.
	out := captureLog(func() {
		for i := 0; i < 3; i++ {
			code, body := issue199Get(t, router, issue199Unknown)
			if code != 404 || len(body) != 1 || body["error"] == nil {
				t.Fatalf("request %d: status=%d body=%v, want the bare 404", i, code, body)
			}
		}
	})
	if n := strings.Count(out, "missing-node lookup failed"); n != 1 {
		t.Fatalf("logged %d lookup failures for 3 requests, want 1; log:\n%s", n, out)
	}
	if !strings.Contains(out, "no such column") {
		t.Errorf("log line does not carry the error: %s", out)
	}
	if strings.Contains(out, issue199Unknown) {
		t.Errorf("log line carries the requested key: %s", out)
	}
}

// A request cancelled by its client is not a broken lookup: nothing logged.
func TestNodeDetail404CancelledLookupIsNotLogged(t *testing.T) {
	srv, _ := setupTestServer(t)
	// The capture is locked (#310): the server's background goroutines keep
	// logging into whatever writer is installed.
	out := captureLog(func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest("GET", "/api/nodes/"+issue199Unknown, nil).WithContext(ctx)
		w := httptest.NewRecorder()
		srv.writeNodeNotFound(w, req, issue199Unknown)
		if w.Code != 404 {
			t.Fatalf("status=%d, want 404", w.Code)
		}
	})
	if strings.Contains(out, "missing-node lookup failed") {
		t.Errorf("cancelled request logged as a lookup failure: %s", out)
	}
}

// The throttle logs the first failure, suppresses the rest inside the
// interval and reports how many it suppressed with the next logged one.
func TestMissingNodeLookupLogThrottle(t *testing.T) {
	var l missingNodeLookupLog
	t0 := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	steps := []struct {
		at             time.Duration
		log            bool
		wantSuppressed int
	}{
		{0, true, 0},
		{time.Second, false, 0},
		{missingNodeLogEvery - time.Second, false, 0},
		{missingNodeLogEvery, true, 2},
		{missingNodeLogEvery + time.Minute, false, 0},
		{3 * missingNodeLogEvery, true, 1},
		{5 * missingNodeLogEvery, true, 0},
	}
	for i, s := range steps {
		logIt, suppressed := l.note(t0.Add(s.at))
		if logIt != s.log || suppressed != s.wantSuppressed {
			t.Errorf("step %d (+%v): note()=(%v, %d), want (%v, %d)", i, s.at, logIt, suppressed, s.log, s.wantSuppressed)
		}
	}
}
