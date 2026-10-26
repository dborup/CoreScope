package main

import (
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/channelregistry"
)

// #616: GET /api/admin/channel-proposals exposes who approved a channel as
// approvedBy ("auto" | "admin") once the ingestor has added
// channel_proposals.approved_by. The server only reads it, detected through
// the self-healing schema flag, so an old database without the column keeps
// working (approvedBy omitted) and a column the ingestor adds after the
// server started shows up without a restart.

// Test fixture only: the table as the current ingestor creates it.
const proposalTableApprovedByDDL = `CREATE TABLE channel_proposals (
	id TEXT PRIMARY KEY,
	name TEXT COLLATE BINARY NOT NULL UNIQUE,
	status TEXT NOT NULL CHECK(status IN ('pending','approved','rejected','revoked')),
	created_at INTEGER NOT NULL,
	reviewed_at INTEGER NULL,
	approved_by TEXT NULL CHECK(approved_by IN ('auto','admin'))
)`

func (ps *proposalServer) setApprovedBy(id, by string) {
	ps.t.Helper()
	if _, err := ps.srv.db.conn.Exec(`UPDATE channel_proposals SET approved_by = ? WHERE id = ?`, by, id); err != nil {
		ps.t.Fatal(err)
	}
}

func (ps *proposalServer) adminList(query string) (ChannelProposalListResponse, string) {
	ps.t.Helper()
	w := ps.do("GET", "/api/admin/channel-proposals"+query, "", strongTestKey)
	if w.Code != 200 {
		ps.t.Fatalf("admin list %s = %d %s", query, w.Code, w.Body)
	}
	return decode[ChannelProposalListResponse](ps.t, w), w.Body.String()
}

func approvedByOf(list ChannelProposalListResponse) map[string]string {
	out := map[string]string{}
	for _, p := range list.Proposals {
		out[p.Name] = p.ApprovedBy
	}
	return out
}

func TestAdminChannelProposalListApprovedBy(t *testing.T) {
	ps := newProposalServer(t, strongTestKey, enabledConfig(nil), false)
	if _, err := ps.srv.db.conn.Exec(proposalTableApprovedByDDL); err != nil {
		t.Fatal(err)
	}
	ps.srv.db.detectSchema()
	ps.insert("aaaaaaaaaaaaaaaa", "#auto", channelregistry.StatusApproved)
	ps.insert("bbbbbbbbbbbbbbbb", "#admin", channelregistry.StatusApproved)
	ps.insert("cccccccccccccccc", "#pending", channelregistry.StatusPending)
	ps.insert("dddddddddddddddd", "#revoked", channelregistry.StatusRevoked)
	ps.setApprovedBy("aaaaaaaaaaaaaaaa", channelregistry.ApprovedByAuto)
	ps.setApprovedBy("bbbbbbbbbbbbbbbb", channelregistry.ApprovedByAdmin)
	// A stale value an older ingestor left on a row it moved out of
	// approved must not be reported.
	ps.setApprovedBy("dddddddddddddddd", channelregistry.ApprovedByAuto)

	all, body := ps.adminList("")
	got := approvedByOf(all)
	want := map[string]string{"#auto": "auto", "#admin": "admin", "#pending": "", "#revoked": ""}
	for name, by := range want {
		if got[name] != by {
			t.Errorf("%s approvedBy = %q, want %q (list %s)", name, got[name], by, body)
		}
	}
	if n := strings.Count(body, `"approvedBy"`); n != 2 {
		t.Errorf("approvedBy keys in the list = %d, want 2 (only approved rows): %s", n, body)
	}
	approved, _ := ps.adminList("?status=approved")
	if g := approvedByOf(approved); len(g) != 2 || g["#auto"] != "auto" || g["#admin"] != "admin" {
		t.Fatalf("status=approved approvedBy = %v", g)
	}
}

// An old database (the ingestor has not added the column yet) answers as
// before: 200 and no approvedBy anywhere.
func TestAdminChannelProposalListWithoutApprovedByColumn(t *testing.T) {
	ps := newProposalServer(t, strongTestKey, enabledConfig(nil), true)
	ps.srv.db.detectSchema()
	if ps.srv.db.hasProposalApprovedBy() {
		t.Fatal("precondition: the old table has no approved_by")
	}
	ps.insert("aaaaaaaaaaaaaaaa", "#old", channelregistry.StatusApproved)
	list, body := ps.adminList("?status=approved")
	if len(list.Proposals) != 1 || strings.Contains(body, "approvedBy") {
		t.Fatalf("old DB list = %s", body)
	}
}

// The ingestor's ALTER can land after the server's warm probe; one healer
// tick later the list carries approvedBy, without a restart.
func TestAdminChannelProposalApprovedBySelfHeals(t *testing.T) {
	ps := newProposalServer(t, strongTestKey, enabledConfig(nil), true)
	ps.srv.db.detectSchema()
	ps.insert("aaaaaaaaaaaaaaaa", "#late", channelregistry.StatusApproved)
	if _, err := ps.srv.db.conn.Exec(`ALTER TABLE channel_proposals ADD COLUMN approved_by TEXT NULL CHECK(approved_by IN ('auto','admin'))`); err != nil {
		t.Fatal(err)
	}
	ps.setApprovedBy("aaaaaaaaaaaaaaaa", channelregistry.ApprovedByAdmin)
	if _, body := ps.adminList(""); strings.Contains(body, "approvedBy") {
		t.Fatalf("before the healer tick the flag is still false, got %s", body)
	}
	ps.srv.db.detectSchema() // one healSchemaFlags tick
	if !ps.srv.db.hasProposalApprovedBy() {
		t.Fatal("detectSchema must latch channel_proposals.approved_by once it exists")
	}
	if list, body := ps.adminList(""); approvedByOf(list)["#late"] != "admin" {
		t.Fatalf("after the healer tick: %s", body)
	}
}

// The background healer must keep probing until the column exists: with
// every other flag already true it latches this one and then exits.
func TestHealSchemaFlagsLatchesProposalApprovedBy(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	mustExecDB(t, db, proposalTableApprovedByDDL)
	for _, f := range []*schemaFlag{&db.isV3Flag, &db.hasResolvedPathFlag, &db.hasObsRawHexFlag, &db.hasScopeNameFlag,
		&db.hasDefaultScopeFlag, &db.hasConfiguredScopeFlag, &db.hasDefaultScopeConfirmedAtFlag,
		&db.hasMultibyteSupColsFlag, &db.hasLastSeenFlag, &db.hasRouteMaskFlag, &db.hasClientDeclaredRegionsFlag} {
		f.forceTrue()
	}
	db.schemaHealerStop = make(chan struct{})
	done := make(chan struct{})
	go func() { db.healSchemaFlags(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("healSchemaFlags did not latch channel_proposals.approved_by within two ticks")
	}
	if !db.hasProposalApprovedBy() {
		t.Fatal("the healer exited without latching the approved_by flag")
	}
}

// The production constructor (Server.channelProposals) wires the schema flag
// into the service; newProposalServer above builds the service by hand.
func TestChannelProposalServiceUsesApprovedBySchemaFlag(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	srv := NewServer(db, &Config{Port: 3000, APIKey: strongTestKey}, NewHub())
	svc := srv.channelProposals()
	if svc.hasApprovedBy == nil || svc.hasApprovedBy() {
		t.Fatalf("before the column exists: hasApprovedBy wired=%v", svc.hasApprovedBy != nil)
	}
	mustExecDB(t, db, proposalTableApprovedByDDL)
	db.detectSchema()
	if !svc.hasApprovedBy() {
		t.Fatal("the service must follow the server's approved_by schema flag")
	}
}

// Tests that open a DB and close only its conn leave the schema healer
// ticking on a dead connection for the rest of the test binary, and
// allocation budgets (TestGraphScore561_AllocationBudget) count every
// goroutine's allocations. Detecting approved_by must therefore not add a
// query to the tick: on a closed connection a tick must cost what it cost
// before #616, measured on master with Go 1.27.1 (the version CI pins). A
// separate approved_by query made it 4 and pushed that budget over in CI.
func TestDetectSchemaApprovedByAddsNoQueryToHealerTick(t *testing.T) {
	const closedTickAllocsBefore616 = 2
	db := setupTestDB(t)
	db.conn.Close()
	if tick := testing.AllocsPerRun(20, db.detectSchema); tick > closedTickAllocsBefore616 {
		t.Fatalf("detectSchema on a closed connection: %.0f allocs, want <= %d — the approved_by probe must share the first query", tick, closedTickAllocsBefore616)
	}
}
