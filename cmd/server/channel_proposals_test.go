package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/channelregistry"
)

const strongTestKey = "proposal-admin-key-strong-enough"

// Test fixture only: the production server never writes this table. The
// ingestor creates it (internal/dbschema) and applies every change.
const proposalTableDDL = `CREATE TABLE channel_proposals (
	id TEXT PRIMARY KEY,
	name TEXT COLLATE BINARY NOT NULL UNIQUE,
	status TEXT NOT NULL CHECK(status IN ('pending','approved','rejected')),
	created_at INTEGER NOT NULL,
	reviewed_at INTEGER NULL
)`

type proposalServer struct {
	t      *testing.T
	srv    *Server
	router *mux.Router
	svc    *channelProposalService
	queue  *channelregistry.Queue
	clock  time.Time
}

func newProposalServer(t *testing.T, apiKey string, pc *channelregistry.Config, withTable bool) *proposalServer {
	t.Helper()
	db := setupTestDB(t)
	seedTestData(t, db)
	if withTable {
		if _, err := db.conn.Exec(proposalTableDDL); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &Config{Port: 3000, APIKey: apiKey, ChannelProposals: pc}
	srv := NewServer(db, cfg, NewHub())
	ps := &proposalServer{t: t, srv: srv, clock: time.UnixMilli(1_700_000_000_000)}
	srv.proposals = newChannelProposalService(cfg, db.conn, filepath.Join(t.TempDir(), channelregistry.QueueDirName))
	srv.proposals.now = func() time.Time { return ps.clock }
	ps.svc = srv.proposals
	ps.queue = srv.proposals.queue
	ps.router = mux.NewRouter()
	srv.RegisterRoutes(ps.router)
	return ps
}

func enabledConfig(mod func(*channelregistry.Config)) *channelregistry.Config {
	on := true
	c := &channelregistry.Config{Enabled: &on}
	if mod != nil {
		mod(c)
	}
	return c
}

func (ps *proposalServer) do(method, path, body, key string) *httptest.ResponseRecorder {
	ps.t.Helper()
	var rd *bytes.Reader
	if body != "" {
		rd = bytes.NewReader([]byte(body))
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	w := httptest.NewRecorder()
	ps.router.ServeHTTP(w, req)
	return w
}

func (ps *proposalServer) insert(id, name, status string) {
	ps.t.Helper()
	var reviewed interface{}
	if status != channelregistry.StatusPending {
		reviewed = ps.clock.UnixMilli()
	}
	if _, err := ps.srv.db.conn.Exec(`INSERT INTO channel_proposals (id, name, status, created_at, reviewed_at) VALUES (?, ?, ?, ?, ?)`,
		id, name, status, ps.clock.UnixMilli(), reviewed); err != nil {
		ps.t.Fatal(err)
	}
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return v
}

func TestChannelProposalConfigNeedsEnabledAndStrongKey(t *testing.T) {
	off := false
	cases := []struct {
		name string
		key  string
		pc   *channelregistry.Config
		want bool
	}{
		{"enabled with strong key", strongTestKey, enabledConfig(nil), true},
		{"enabled with weak key", "test", enabledConfig(nil), false},
		{"enabled with short key", "short-key", enabledConfig(nil), false},
		{"enabled without key", "", enabledConfig(nil), false},
		{"disabled", strongTestKey, &channelregistry.Config{Enabled: &off}, false},
		{"not configured", strongTestKey, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ps := newProposalServer(t, c.key, c.pc, true)
			w := ps.do("GET", "/api/channel-proposals/config", "", "")
			if w.Code != 200 {
				t.Fatalf("status %d", w.Code)
			}
			if got := decode[ChannelProposalConfigResponse](t, w); got.Enabled != c.want {
				t.Fatalf("enabled = %v, want %v", got.Enabled, c.want)
			}
			sub := ps.do("POST", "/api/channel-proposals", `{"name":"#Channel"}`, "")
			if c.want && sub.Code != http.StatusAccepted {
				t.Fatalf("submit = %d %s", sub.Code, sub.Body)
			}
			if !c.want && sub.Code != http.StatusForbidden {
				t.Fatalf("disabled submit = %d, want 403", sub.Code)
			}
		})
	}
}

func TestChannelProposalSubmitValidatesAndQueues(t *testing.T) {
	ps := newProposalServer(t, strongTestKey, enabledConfig(nil), true)
	for _, body := range []string{`not json`, `{"name":""}`, `{"name":"#` + strings.Repeat("x", 40) + `"}`, `{"name":"#a\u0000b"}`} {
		if w := ps.do("POST", "/api/channel-proposals", body, ""); w.Code != http.StatusBadRequest {
			t.Errorf("body %s = %d, want 400", body, w.Code)
		}
	}
	big := `{"name":"` + strings.Repeat("a", 2000) + `"}`
	if w := ps.do("POST", "/api/channel-proposals", big, ""); w.Code != http.StatusBadRequest {
		t.Errorf("oversized body = %d, want 400", w.Code)
	}

	w := ps.do("POST", "/api/channel-proposals", `{"name":" MeshCore "}`, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("submit = %d %s", w.Code, w.Body)
	}
	id := decode[ChannelProposalAcceptedResponse](t, w).RequestID
	if !channelregistry.ValidID(id) {
		t.Fatalf("request id %q", id)
	}
	pending, err := ps.queue.Pending()
	if err != nil || len(pending) != 1 {
		t.Fatalf("queue = %+v, %v", pending, err)
	}
	cmd := pending[0].Command
	if cmd.Op != channelregistry.OpSubmit || cmd.Name != "#MeshCore" || cmd.RequestID != id || cmd.CreatedAt != ps.clock.UnixMilli() {
		t.Fatalf("queued command = %+v (case must be preserved)", cmd)
	}
}

func TestChannelProposalRateLimitAndQueueBound(t *testing.T) {
	ps := newProposalServer(t, strongTestKey, enabledConfig(func(c *channelregistry.Config) {
		c.SubmissionsPerHour = 2
		c.MaxQueuedRequests = 3
	}), true)
	submit := func(name string) *httptest.ResponseRecorder {
		return ps.do("POST", "/api/channel-proposals", `{"name":"`+name+`"}`, "")
	}
	if submit("#a").Code != 202 || submit("#b").Code != 202 {
		t.Fatal("first two submissions must pass")
	}
	w := submit("#c")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "3600" {
		t.Fatalf("third = %d Retry-After=%q", w.Code, w.Header().Get("Retry-After"))
	}
	ps.clock = ps.clock.Add(time.Hour + time.Second)
	if w := submit("#c"); w.Code != 202 {
		t.Fatalf("after the window = %d", w.Code)
	}
	// Queue bound: 3 commands are waiting, the next one is refused with 503
	// and does not consume rate budget.
	ps.clock = ps.clock.Add(2 * time.Hour)
	if w := submit("#d"); w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("queue full = %d", w.Code)
	}
	if n := len(ps.svc.recent); n != 0 {
		t.Fatalf("refused submission kept its rate slot: %d", n)
	}
	if w := ps.do("POST", "/api/admin/channel-proposals/1111111111111111/approve", "", strongTestKey); w.Code != http.StatusNotFound {
		t.Fatalf("approve unknown = %d", w.Code)
	}
}

func TestChannelProposalRequestStatus(t *testing.T) {
	ps := newProposalServer(t, strongTestKey, enabledConfig(nil), true)
	// mux answers path tricks with a clean-path redirect before any handler
	// runs; either way no file outside the queue is ever read.
	if w := ps.do("GET", "/api/channel-proposals/requests/..%2F..%2Fetc", "", ""); w.Code == http.StatusOK {
		t.Fatalf("traversal id served: %s", w.Body)
	}
	if w := ps.do("GET", "/api/channel-proposals/requests/NOT-HEX", "", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("bad id = %d", w.Code)
	}
	if w := ps.do("GET", "/api/channel-proposals/requests/0123456789abcdef", "", ""); w.Code != http.StatusNotFound {
		t.Fatalf("unknown id = %d", w.Code)
	}
	id := decode[ChannelProposalAcceptedResponse](t, ps.do("POST", "/api/channel-proposals", `{"name":"#Status"}`, "")).RequestID
	w := ps.do("GET", "/api/channel-proposals/requests/"+id, "", "")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("queued lookup = %d %s", w.Code, w.Header().Get("Cache-Control"))
	}
	if st := decode[channelregistry.RequestStatus](t, w); st.Status != channelregistry.RequestQueued || st.Proposal != nil {
		t.Fatalf("queued = %+v", st)
	}
	// What the ingestor writes back.
	p := &channelregistry.Proposal{ID: id, Name: "#Status", Status: channelregistry.StatusPending, CreatedAt: ps.clock.UnixMilli()}
	if err := ps.queue.Complete(channelregistry.Result{RequestID: id, Status: channelregistry.RequestPending, Proposal: p}); err != nil {
		t.Fatal(err)
	}
	raw := ps.do("GET", "/api/channel-proposals/requests/"+id, "", "").Body.String()
	for _, want := range []string{`"status":"pending"`, `"name":"#Status"`, `"createdAt":1700000000000`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("pending body %s lacks %s", raw, want)
		}
	}
	if strings.Contains(raw, "reviewedAt") || strings.Contains(raw, `"error"`) {
		t.Fatalf("pending body %s must omit reviewedAt/error", raw)
	}
}

func TestAdminChannelProposalsRequireStrongKey(t *testing.T) {
	cases := []struct {
		name, configured, sent string
		want                   int
	}{
		{"no key configured", "", strongTestKey, http.StatusForbidden},
		{"missing header", strongTestKey, "", http.StatusUnauthorized},
		{"wrong key", strongTestKey, "wrong-key-wrong-key-wrong", http.StatusUnauthorized},
		{"weak configured key", "test", "test", http.StatusForbidden},
		{"correct key", strongTestKey, strongTestKey, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ps := newProposalServer(t, c.configured, enabledConfig(nil), true)
			if w := ps.do("GET", "/api/admin/channel-proposals", "", c.sent); w.Code != c.want {
				t.Fatalf("list = %d, want %d", w.Code, c.want)
			}
			ps.insert("abcdefabcdefabcd", "#x", channelregistry.StatusPending)
			want := c.want
			if want == http.StatusOK {
				want = http.StatusAccepted
			}
			if w := ps.do("POST", "/api/admin/channel-proposals/abcdefabcdefabcd/approve", "", c.sent); w.Code != want {
				t.Fatalf("approve = %d, want %d", w.Code, want)
			}
		})
	}
}

func TestAdminChannelProposalListAndDecisions(t *testing.T) {
	ps := newProposalServer(t, strongTestKey, enabledConfig(nil), true)
	ps.insert("aaaaaaaaaaaaaaaa", "#pending", channelregistry.StatusPending)
	ps.clock = ps.clock.Add(time.Second)
	ps.insert("bbbbbbbbbbbbbbbb", "#approved", channelregistry.StatusApproved)
	ps.insert("cccccccccccccccc", "#rejected", channelregistry.StatusRejected)

	all := decode[ChannelProposalListResponse](t, ps.do("GET", "/api/admin/channel-proposals", "", strongTestKey))
	if len(all.Proposals) != 3 || !all.Enabled {
		t.Fatalf("list = %+v", all)
	}
	only := decode[ChannelProposalListResponse](t, ps.do("GET", "/api/admin/channel-proposals?status=approved", "", strongTestKey))
	if len(only.Proposals) != 1 || only.Proposals[0].Name != "#approved" || only.Proposals[0].ReviewedAt == nil {
		t.Fatalf("approved filter = %+v", only)
	}
	if w := ps.do("GET", "/api/admin/channel-proposals?status=bogus", "", strongTestKey); w.Code != http.StatusBadRequest {
		t.Fatalf("bad status filter = %d", w.Code)
	}

	for _, op := range []string{"approve", "reject"} {
		w := ps.do("POST", "/api/admin/channel-proposals/aaaaaaaaaaaaaaaa/"+op, "", strongTestKey)
		if w.Code != http.StatusAccepted {
			t.Fatalf("%s = %d %s", op, w.Code, w.Body)
		}
		if id := decode[ChannelProposalAcceptedResponse](t, w).RequestID; !channelregistry.ValidID(id) {
			t.Fatalf("%s request id %q", op, id)
		}
	}
	pending, _ := ps.queue.Pending()
	if len(pending) != 2 || pending[0].Command.ProposalID != "aaaaaaaaaaaaaaaa" {
		t.Fatalf("queued decisions = %+v", pending)
	}
	ops := map[string]bool{pending[0].Command.Op: true, pending[1].Command.Op: true}
	if !ops[channelregistry.OpApprove] || !ops[channelregistry.OpReject] {
		t.Fatalf("ops = %v", ops)
	}
	if w := ps.do("POST", "/api/admin/channel-proposals/not-hex/approve", "", strongTestKey); w.Code != http.StatusBadRequest {
		t.Fatalf("bad id = %d", w.Code)
	}
	// Moderation stays available when public submissions are switched off.
	off := false
	closed := newProposalServer(t, strongTestKey, &channelregistry.Config{Enabled: &off}, true)
	closed.insert("dddddddddddddddd", "#late", channelregistry.StatusPending)
	if w := closed.do("POST", "/api/admin/channel-proposals/dddddddddddddddd/approve", "", strongTestKey); w.Code != http.StatusAccepted {
		t.Fatalf("approve with submissions off = %d", w.Code)
	}
}

func TestChannelsListIncludesApprovedWithoutTraffic(t *testing.T) {
	ps := newProposalServer(t, strongTestKey, enabledConfig(nil), true)

	before := ps.do("GET", "/api/channels", "", "")
	if before.Code != 200 || strings.Contains(before.Body.String(), "approvedChannels") {
		t.Fatalf("empty list must omit approvedChannels: %s", before.Body)
	}
	baseline := decode[ChannelListResponse](t, before).Channels

	ps.insert("aaaaaaaaaaaaaaaa", "#QuietChannel", channelregistry.StatusApproved)
	ps.insert("bbbbbbbbbbbbbbbb", "#Waiting", channelregistry.StatusPending)
	ps.insert("cccccccccccccccc", "#Nope", channelregistry.StatusRejected)
	ps.clock = ps.clock.Add(approvedCacheTTL + time.Second)

	resp := decode[ChannelListResponse](t, ps.do("GET", "/api/channels", "", ""))
	want := []ApprovedChannel{{Name: "#QuietChannel", Hash: "#QuietChannel"}}
	if !reflect.DeepEqual(resp.ApprovedChannels, want) {
		t.Fatalf("approvedChannels = %+v, want %+v", resp.ApprovedChannels, want)
	}
	if len(resp.Channels) != len(baseline) {
		t.Fatalf("channels changed: %d -> %d", len(baseline), len(resp.Channels))
	}
	// Approved even though it never carried a message.
	for _, ch := range resp.Channels {
		if ch["name"] == "#QuietChannel" {
			t.Fatal("precondition: #QuietChannel must have no traffic")
		}
	}
}

// The approval shows up without waiting for the cache TTL once a request
// status read observes it.
func TestApprovedStatusInvalidatesCache(t *testing.T) {
	ps := newProposalServer(t, strongTestKey, enabledConfig(nil), true)
	ps.do("GET", "/api/channels", "", "") // warm the (empty) cache
	ps.insert("aaaaaaaaaaaaaaaa", "#Fresh", channelregistry.StatusApproved)
	reqID := channelregistry.NewID()
	p := &channelregistry.Proposal{ID: "aaaaaaaaaaaaaaaa", Name: "#Fresh", Status: channelregistry.StatusApproved}
	if err := ps.queue.Complete(channelregistry.Result{RequestID: reqID, Status: channelregistry.RequestApproved, Proposal: p}); err != nil {
		t.Fatal(err)
	}
	if st := decode[channelregistry.RequestStatus](t, ps.do("GET", "/api/channel-proposals/requests/"+reqID, "", "")); st.Status != channelregistry.RequestApproved {
		t.Fatalf("status = %+v", st)
	}
	resp := decode[ChannelListResponse](t, ps.do("GET", "/api/channels", "", ""))
	if len(resp.ApprovedChannels) != 1 || resp.ApprovedChannels[0].Name != "#Fresh" {
		t.Fatalf("approvedChannels after approval = %+v", resp.ApprovedChannels)
	}
}

// GET /api/channels must neither mutate the DB's cached channel slice nor
// hand the approved-channel cache to callers.
func TestChannelsListDoesNotMutateCaches(t *testing.T) {
	ps := newProposalServer(t, strongTestKey, enabledConfig(nil), true)
	ps.insert("aaaaaaaaaaaaaaaa", "#Shared", channelregistry.StatusApproved)
	db := ps.srv.db
	// An encrypted channel makes ?includeEncrypted=true append to the list.
	if _, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		VALUES ('EEFF', 'enc0000000000001', '2024-01-01T00:00:00Z', 1, 5, '{}', 'enc_AB')`); err != nil {
		t.Fatal(err)
	}
	// The bug this guards against only shows when the cached slice has spare
	// capacity, so pad the decrypted channels until it does.
	var cached []map[string]interface{}
	for i := 0; ; i++ {
		db.channelsCacheMu.Lock()
		db.channelsCacheRes = nil
		db.channelsCacheMu.Unlock()
		var err error
		if cached, err = db.GetChannels(""); err != nil {
			t.Fatal(err)
		}
		if cap(cached) > len(cached) {
			break
		}
		if i == 8 {
			t.Fatalf("precondition: cached slice never had spare capacity (len=cap=%d)", len(cached))
		}
		if _, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
			VALUES ('EEFF', ?, '2024-01-01T00:00:00Z', 1, 5, '{"text":"a: b","sender":"a"}', ?)`,
			"pad000000000000"+string(rune('a'+i)), "#pad"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	full := cached[:cap(cached)]
	snapshot := make([]map[string]interface{}, len(full))
	for i, m := range full {
		if m == nil {
			continue
		}
		cp := make(map[string]interface{}, len(m))
		for k, v := range m {
			cp[k] = v
		}
		snapshot[i] = cp
	}
	for i := 0; i < 3; i++ {
		for _, q := range []string{"/api/channels", "/api/channels?includeEncrypted=true"} {
			if w := ps.do("GET", q, "", ""); w.Code != 200 {
				t.Fatalf("%s = %d", q, w.Code)
			}
		}
	}
	again, _ := db.GetChannels("")
	if len(again) != len(cached) || !reflect.DeepEqual(full, cached[:cap(cached)]) {
		t.Fatal("cached channel slice changed")
	}
	for i, m := range cached[:cap(cached)] {
		if !reflect.DeepEqual(m, snapshot[i]) {
			t.Fatalf("cached channel %d mutated: %v -> %v", i, snapshot[i], m)
		}
	}
	a := ps.svc.approvedChannels(context.Background())
	a[0].Name = "#Tampered"
	if b := ps.svc.approvedChannels(context.Background()); b[0].Name != "#Shared" {
		t.Fatal("approvedChannels handed out its cached slice")
	}
}

// Rolling upgrade: a new server against a database whose ingestor has not
// created the table yet. Everything reads as empty, nothing fails, and the
// absence is not cached: once the table appears, approvals show up.
func TestChannelProposalsMissingTable(t *testing.T) {
	ps := newProposalServer(t, strongTestKey, enabledConfig(nil), false)

	w := ps.do("GET", "/api/channels", "", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "approvedChannels") {
		t.Fatalf("channels = %d %s", w.Code, w.Body)
	}
	list := ps.do("GET", "/api/admin/channel-proposals", "", strongTestKey)
	if list.Code != 200 || len(decode[ChannelProposalListResponse](t, list).Proposals) != 0 {
		t.Fatalf("admin list = %d %s", list.Code, list.Body)
	}
	if w := ps.do("POST", "/api/admin/channel-proposals/aaaaaaaaaaaaaaaa/approve", "", strongTestKey); w.Code != http.StatusNotFound {
		t.Fatalf("approve = %d", w.Code)
	}
	if w := ps.do("POST", "/api/channel-proposals", `{"name":"#Early"}`, ""); w.Code != http.StatusAccepted {
		t.Fatalf("submit is queued for the ingestor even before the table exists: %d", w.Code)
	}

	if _, err := ps.srv.db.conn.Exec(proposalTableDDL); err != nil {
		t.Fatal(err)
	}
	ps.insert("aaaaaaaaaaaaaaaa", "#Later", channelregistry.StatusApproved)
	ps.clock = ps.clock.Add(approvedCacheTTL + time.Second)
	resp := decode[ChannelListResponse](t, ps.do("GET", "/api/channels", "", ""))
	if len(resp.ApprovedChannels) != 1 || resp.ApprovedChannels[0].Name != "#Later" {
		t.Fatalf("approvedChannels after the table appeared = %+v", resp.ApprovedChannels)
	}
}

// The server keeps its read-only contract: the queue directory is the only
// thing it writes, and only command files.
func TestChannelProposalServerWritesOnlyCommandFiles(t *testing.T) {
	ps := newProposalServer(t, strongTestKey, enabledConfig(nil), true)
	ps.do("POST", "/api/channel-proposals", `{"name":"#OnlyQueue"}`, "")
	var n int
	if err := ps.srv.db.conn.QueryRow(`SELECT COUNT(*) FROM channel_proposals`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("server wrote %d row(s), %v", n, err)
	}
	entries, err := os.ReadDir(ps.queue.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "cmd-") {
			t.Fatalf("unexpected file %s", e.Name())
		}
	}
}
