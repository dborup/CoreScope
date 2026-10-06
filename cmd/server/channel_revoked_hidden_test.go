package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/channelregistry"
)

// #251: an administrator who revokes a shared channel expects it to leave the
// channel list. The decoded messages stay in the database, so the list query
// still returns the channel; GET /api/channels leaves out names whose proposal
// is revoked, unless the ingestor decrypts that name through its config-derived
// keys (built-in, rainbow table, hashChannels, channelKeys).

// addChannelTraffic stores one decoded message on a channel, the way the
// ingestor does for a decrypted GRP_TXT.
func (ps *proposalServer) addChannelTraffic(name string) {
	ps.t.Helper()
	hash := fmt.Sprintf("%x", name) // unique per name
	if _, err := ps.srv.db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		VALUES ('EEFF', ?, '2024-01-01T00:00:00Z', 1, 5, ?, ?)`,
		hash, `{"type":"CHAN","channel":"`+name+`","text":"alice: hello","sender":"alice"}`, name); err != nil {
		ps.t.Fatal(err)
	}
	ps.srv.db.channelsCache.reset()
}

// expireApproved moves the clock past the approved-snapshot TTL, as the
// passing of time does between two refreshes of the channel list.
func (ps *proposalServer) expireApproved() { ps.clock = ps.clock.Add(approvedCacheTTL + time.Second) }

func (ps *proposalServer) setStatus(id, status string) {
	ps.t.Helper()
	if _, err := ps.srv.db.conn.Exec(`UPDATE channel_proposals SET status = ?, reviewed_at = ? WHERE id = ?`,
		status, ps.clock.UnixMilli(), id); err != nil {
		ps.t.Fatal(err)
	}
}

func (ps *proposalServer) listedNames(query string) []string {
	ps.t.Helper()
	w := ps.do("GET", "/api/channels"+query, "", "")
	if w.Code != 200 {
		ps.t.Fatalf("GET /api/channels%s = %d: %s", query, w.Code, w.Body)
	}
	var names []string
	for _, ch := range decode[ChannelListResponse](ps.t, w).Channels {
		names = append(names, fmt.Sprint(ch["name"]))
	}
	sort.Strings(names)
	return names
}

func containsName(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func newRevokeFixture(t *testing.T) *proposalServer {
	t.Helper()
	ps := newProposalServer(t, strongTestKey, enabledConfig(nil), true)
	// The ingestor has published its config-derived names (it does so before
	// it applies any command).
	if err := ps.queue.WriteBuiltinNames([]string{"#chat"}); err != nil {
		t.Fatal(err)
	}
	return ps
}

// Acceptance 1: after a revoke the channel is gone from the list although
// historical messages exist; the messages themselves stay readable.
func TestRevokedChannelIsHiddenFromList(t *testing.T) {
	ps := newRevokeFixture(t)
	ps.addChannelTraffic("#helloworld")
	ps.insert("aaaaaaaaaaaaaaaa", "#helloworld", channelregistry.StatusApproved)
	ps.expireApproved()
	if got := ps.listedNames(""); !containsName(got, "#helloworld") {
		t.Fatalf("precondition: an approved channel with traffic is listed: %v", got)
	}

	ps.setStatus("aaaaaaaaaaaaaaaa", channelregistry.StatusRevoked)
	ps.expireApproved()
	got := ps.listedNames("")
	if containsName(got, "#helloworld") {
		t.Fatalf("revoked channel still listed: %v", got)
	}
	if !containsName(got, "#test") {
		t.Fatalf("unrelated channels must stay listed: %v", got)
	}
	// History is kept: the conversation can still be read directly.
	w := ps.do("GET", "/api/channels/%23helloworld/messages", "", "")
	if w.Code != 200 || decode[ChannelMessagesResponse](t, w).Total != 1 {
		t.Fatalf("history of a revoked channel must stay readable: %d %s", w.Code, w.Body)
	}
	// A revoked channel is not in approvedChannels either.
	if resp := decode[ChannelListResponse](t, ps.do("GET", "/api/channels", "", "")); len(resp.ApprovedChannels) != 0 {
		t.Fatalf("approvedChannels = %+v", resp.ApprovedChannels)
	}
}

// The revoke result the admin's poller reads invalidates the snapshot early,
// so the list changes within one refresh without waiting for the TTL.
func TestRevokeHidesChannelWithoutWaitingForTTL(t *testing.T) {
	ps := newRevokeFixture(t)
	ps.addChannelTraffic("#helloworld")
	ps.insert("aaaaaaaaaaaaaaaa", "#helloworld", channelregistry.StatusApproved)
	ps.expireApproved()
	if got := ps.listedNames(""); !containsName(got, "#helloworld") {
		t.Fatalf("precondition: %v", got)
	}

	ps.setStatus("aaaaaaaaaaaaaaaa", channelregistry.StatusRevoked)
	reqID := channelregistry.NewID()
	p := &channelregistry.Proposal{ID: "aaaaaaaaaaaaaaaa", Name: "#helloworld", Status: channelregistry.StatusRevoked}
	if err := ps.queue.Complete(channelregistry.Result{RequestID: reqID, Status: channelregistry.RequestRevoked, Proposal: p, CompletedAt: ps.clock.Add(time.Millisecond).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if w := ps.do("GET", "/api/channel-proposals/requests/"+reqID, "", ""); w.Code != 200 {
		t.Fatalf("request status = %d", w.Code)
	}
	if got := ps.listedNames(""); containsName(got, "#helloworld") {
		t.Fatalf("revoked channel still listed inside the TTL: %v", got)
	}
}

// Acceptance 2a: re-approval shows the channel again, with its history.
func TestReapprovedChannelIsListedAgain(t *testing.T) {
	ps := newRevokeFixture(t)
	ps.addChannelTraffic("#helloworld")
	ps.insert("aaaaaaaaaaaaaaaa", "#helloworld", channelregistry.StatusRevoked)
	ps.expireApproved()
	if got := ps.listedNames(""); containsName(got, "#helloworld") {
		t.Fatalf("precondition: revoked channel hidden: %v", got)
	}

	// The ingestor revives the same row: revoked -> pending (suggested
	// again) -> approved.
	ps.setStatus("aaaaaaaaaaaaaaaa", channelregistry.StatusPending)
	ps.expireApproved()
	ps.setStatus("aaaaaaaaaaaaaaaa", channelregistry.StatusApproved)
	ps.expireApproved()
	var found map[string]interface{}
	for _, ch := range decode[ChannelListResponse](t, ps.do("GET", "/api/channels", "", "")).Channels {
		if ch["name"] == "#helloworld" {
			found = ch
		}
	}
	if found == nil {
		t.Fatal("re-approved channel is not listed")
	}
	if found["messageCount"] != float64(1) {
		t.Fatalf("history must come back with the channel: %v", found)
	}
}

// Acceptance 2b: a channel the ingestor decrypts through its config-derived
// keys is never hidden by a revoked proposal for the same name.
func TestBuiltinChannelIsNeverHiddenByRevokedProposal(t *testing.T) {
	ps := newRevokeFixture(t)
	if err := ps.queue.WriteBuiltinNames([]string{"#test", "#chat"}); err != nil {
		t.Fatal(err)
	}
	// "#test" has seeded traffic and is decrypted by the built-in layer.
	ps.insert("aaaaaaaaaaaaaaaa", "#test", channelregistry.StatusRevoked)
	ps.expireApproved()
	if got := ps.listedNames(""); !containsName(got, "#test") {
		t.Fatalf("built-in channel hidden by a revoked proposal: %v", got)
	}
}

// Hiding needs a positive "not config-decrypted": without the ingestor's
// names file (missing or unreadable) nothing is hidden.
func TestRevokedChannelIsNotHiddenWithoutBuiltinNames(t *testing.T) {
	ps := newProposalServer(t, strongTestKey, enabledConfig(nil), true) // no names file written
	ps.insert("aaaaaaaaaaaaaaaa", "#test", channelregistry.StatusRevoked)
	ps.expireApproved()
	if got := ps.listedNames(""); !containsName(got, "#test") {
		t.Fatalf("fail-open expected without the built-in names file: %v", got)
	}

	if err := os.MkdirAll(filepath.Dir(ps.queue.BuiltinNamesPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ps.queue.BuiltinNamesPath(), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	ps.expireApproved()
	if got := ps.listedNames(""); !containsName(got, "#test") {
		t.Fatalf("fail-open expected with an unreadable built-in names file: %v", got)
	}
}

// Only the exact name is hidden: a different-case channel is a different
// channel (different key) and stays listed.
func TestRevokedChannelHidesOnlyTheExactName(t *testing.T) {
	ps := newRevokeFixture(t)
	ps.addChannelTraffic("#HelloWorld")
	ps.addChannelTraffic("#helloworld")
	ps.insert("aaaaaaaaaaaaaaaa", "#HelloWorld", channelregistry.StatusRevoked)
	ps.insert("bbbbbbbbbbbbbbbb", "#helloworld", channelregistry.StatusApproved)
	ps.expireApproved()
	got := ps.listedNames("")
	if containsName(got, "#HelloWorld") || !containsName(got, "#helloworld") {
		t.Fatalf("list = %v: want #helloworld listed and #HelloWorld hidden", got)
	}
}

// The filter works on a copy: the DB's cached slice keeps the hidden channel
// (it is only hidden from this response), and includeEncrypted still appends.
func TestRevokedFilterDoesNotMutateCacheAndKeepsEncrypted(t *testing.T) {
	ps := newRevokeFixture(t)
	ps.addChannelTraffic("#helloworld")
	if _, err := ps.srv.db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		VALUES ('EEFF', 'enc0000000000001', '2024-01-01T00:00:00Z', 1, 5, '{}', 'enc_AB')`); err != nil {
		t.Fatal(err)
	}
	ps.insert("aaaaaaaaaaaaaaaa", "#helloworld", channelregistry.StatusRevoked)
	ps.expireApproved()
	for i := 0; i < 3; i++ {
		got := ps.listedNames("?includeEncrypted=true")
		if containsName(got, "#helloworld") || !containsName(got, "Encrypted (0xAB)") {
			t.Fatalf("list = %v", got)
		}
	}
	cached, err := ps.srv.db.GetChannels("")
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, ch := range cached {
		if ch["name"] == "#helloworld" {
			seen = true
		}
	}
	if !seen {
		t.Fatal("the cached channel list was altered by the filter")
	}
}

// Performance: the revoked set rides on the approved-names snapshot, so a
// list request does not query the proposals table again inside the TTL.
func TestRevokedSetIsServedFromTheSnapshot(t *testing.T) {
	ps := newRevokeFixture(t)
	ps.addChannelTraffic("#helloworld")
	ps.insert("aaaaaaaaaaaaaaaa", "#helloworld", channelregistry.StatusRevoked)
	ps.expireApproved()
	if got := ps.listedNames(""); containsName(got, "#helloworld") {
		t.Fatalf("precondition: %v", got)
	}
	// Dropping the table would fail any further query; the snapshot answers.
	if _, err := ps.srv.db.conn.Exec(`DROP TABLE channel_proposals`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if got := ps.listedNames(""); containsName(got, "#helloworld") {
			t.Fatalf("request %d queried the proposals table instead of the snapshot: %v", i, got)
		}
	}
}

// With nothing to hide the filter must be a no-op that returns the cached
// slice untouched (no copy per request), and report no hidden names.
func TestHideRevokedIsNoOpWithoutHiddenNames(t *testing.T) {
	ps := newRevokeFixture(t)
	in := []map[string]interface{}{{"name": "#a"}, {"name": "#b"}}
	resp := ChannelListResponse{Channels: in}
	ps.svc.hideRevoked(context.Background(), &resp)
	if !reflect.DeepEqual(in, resp.Channels) || &in[0] != &resp.Channels[0] {
		t.Fatalf("expected the same slice back, got %v", resp.Channels)
	}
	if resp.HiddenChannels != nil {
		t.Fatalf("hiddenChannels must be omitted when nothing is hidden: %v", resp.HiddenChannels)
	}
}

// A rolling upgrade (table not created yet) must not hide or break anything.
func TestRevokedFilterMissingTable(t *testing.T) {
	ps := newProposalServer(t, strongTestKey, enabledConfig(nil), false)
	if got := ps.listedNames(""); !containsName(got, "#test") {
		t.Fatalf("list = %v", got)
	}
}

// The hidden decision is read per channel that has stored traffic, not from a
// capped list of proposal rows: an old revoked proposal must stay hidden no
// matter how many newer ones exist (review of #257, finding 3).
func TestRevokedChannelStaysHiddenBehindManyNewerRevokedRows(t *testing.T) {
	ps := newRevokeFixture(t)
	ps.addChannelTraffic("#oldrevoked")
	if _, err := ps.srv.db.conn.Exec(`INSERT INTO channel_proposals (id, name, status, created_at, reviewed_at) VALUES ('0000000000000001', '#oldrevoked', 'revoked', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	// More newer revoked rows than any read cap (MaxListed = 1024).
	tx, err := ps.srv.db.conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < channelregistry.MaxListed+1; i++ {
		name := fmt.Sprintf("#newer%d", i)
		if _, err := tx.Exec(`INSERT INTO channel_proposals (id, name, status, created_at, reviewed_at) VALUES (?, ?, 'revoked', ?, ?)`,
			fmt.Sprintf("%016x", i+2), name, 1000+i, 1000+i); err != nil {
			t.Fatal(err)
		}
		// Each newer revoked channel has stored traffic too, as a real
		// revoked channel does.
		if _, err := tx.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
			VALUES ('EEFF', ?, '2024-01-01T00:00:00Z', 1, 5, ?, ?)`,
			fmt.Sprintf("newer%016x", i), `{"type":"CHAN","channel":"`+name+`","text":"a: b","sender":"a"}`, name); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	ps.expireApproved()
	if got := ps.listedNames(""); containsName(got, "#oldrevoked") {
		t.Fatalf("an old revoked channel reappeared behind %d newer revoked rows: %v", channelregistry.MaxListed+1, got)
	}
}

// Review of #257, finding 2: a revoked name that is suggested again (pending)
// or rejected must stay hidden. Only an approval, or the ingestor decrypting
// the name through its built-in/config list, lists the channel.
func TestPendingAndRejectedProposalsKeepAChannelHidden(t *testing.T) {
	ps := newRevokeFixture(t)
	ps.addChannelTraffic("#helloworld")
	ps.insert("aaaaaaaaaaaaaaaa", "#helloworld", channelregistry.StatusRevoked)
	steps := []struct {
		status  string
		listed  bool
		comment string
	}{
		{channelregistry.StatusRevoked, false, "revoked"},
		{channelregistry.StatusPending, false, "suggested again by a visitor"},
		{channelregistry.StatusRejected, false, "re-suggestion rejected by the administrator"},
		{channelregistry.StatusApproved, true, "approved again"},
		{channelregistry.StatusRevoked, false, "revoked again"},
	}
	for _, st := range steps {
		ps.setStatus("aaaaaaaaaaaaaaaa", st.status)
		ps.expireApproved()
		if got := containsName(ps.listedNames(""), "#helloworld"); got != st.listed {
			t.Fatalf("%s: listed = %v, want %v", st.comment, got, st.listed)
		}
	}
	// A pending or rejected name the ingestor decrypts through its config is
	// still listed.
	if err := ps.queue.WriteBuiltinNames([]string{"#test", "#chat"}); err != nil {
		t.Fatal(err)
	}
	ps.insert("bbbbbbbbbbbbbbbb", "#test", channelregistry.StatusRejected)
	ps.expireApproved()
	if got := ps.listedNames(""); !containsName(got, "#test") {
		t.Fatalf("a built-in channel hidden by a rejected proposal: %v", got)
	}
}

// Review of #257, finding 2 (frontend half): the response names the hidden
// channels, so the page can keep live updates from re-creating their rows.
func TestChannelListNamesTheHiddenChannels(t *testing.T) {
	ps := newRevokeFixture(t)
	ps.addChannelTraffic("#helloworld")
	ps.insert("aaaaaaaaaaaaaaaa", "#helloworld", channelregistry.StatusRevoked)
	ps.expireApproved()
	resp := decode[ChannelListResponse](t, ps.do("GET", "/api/channels", "", ""))
	if !reflect.DeepEqual(resp.HiddenChannels, []string{"#helloworld"}) {
		t.Fatalf("hiddenChannels = %v", resp.HiddenChannels)
	}
	ps.setStatus("aaaaaaaaaaaaaaaa", channelregistry.StatusApproved)
	ps.expireApproved()
	w := ps.do("GET", "/api/channels", "", "")
	if strings.Contains(w.Body.String(), "hiddenChannels") {
		t.Fatalf("hiddenChannels must be omitted when nothing is hidden: %s", w.Body)
	}
}

// Review of #257, finding 1: the admin list applies the status filter in SQL
// before the row limit. One old approved proposal must stay reachable (and
// removable) behind more newer rejected ones than the limit.
func TestAdminStatusFilterIsAppliedBeforeTheLimit(t *testing.T) {
	ps := newRevokeFixture(t)
	limit := ps.svc.limits.MaxPending + ps.svc.limits.MaxApproved + ps.svc.limits.MaxQueuedRequests
	if _, err := ps.srv.db.conn.Exec(`INSERT INTO channel_proposals (id, name, status, created_at, reviewed_at) VALUES ('0000000000000001', '#oldapproved', 'approved', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	tx, err := ps.srv.db.conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < limit; i++ {
		if _, err := tx.Exec(`INSERT INTO channel_proposals (id, name, status, created_at, reviewed_at) VALUES (?, ?, 'rejected', ?, ?)`,
			fmt.Sprintf("%016x", i+2), fmt.Sprintf("#spam%d", i), 1000+i, 1000+i); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for status, want := range map[string]int{"approved": 1, "rejected": limit} {
		w := ps.do("GET", "/api/admin/channel-proposals?status="+status, "", strongTestKey)
		if got := len(decode[ChannelProposalListResponse](t, w).Proposals); got != want {
			t.Fatalf("?status=%s returned %d proposals, want %d", status, got, want)
		}
	}
}

// The near-duplicate hint is read from a separate name-only list, so it still
// sees a case variant that is older than the row limit.
func TestNearDuplicateHintSeesProposalsBeyondTheRowLimit(t *testing.T) {
	ps := newRevokeFixture(t)
	limit := ps.svc.limits.MaxPending + ps.svc.limits.MaxApproved + ps.svc.limits.MaxQueuedRequests
	if _, err := ps.srv.db.conn.Exec(`INSERT INTO channel_proposals (id, name, status, created_at, reviewed_at) VALUES ('0000000000000001', '#HelloWorld', 'rejected', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	tx, err := ps.srv.db.conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < limit; i++ {
		if _, err := tx.Exec(`INSERT INTO channel_proposals (id, name, status, created_at, reviewed_at) VALUES (?, ?, 'pending', ?, NULL)`,
			fmt.Sprintf("%016x", i+2), fmt.Sprintf("#p%d", i), 1000+i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO channel_proposals (id, name, status, created_at, reviewed_at) VALUES ('ffffffffffffffff', '#helloworld', 'pending', 999999, NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	list := decode[ChannelProposalListResponse](t, ps.do("GET", "/api/admin/channel-proposals?status=pending", "", strongTestKey))
	for _, p := range list.Proposals {
		if p.Name == "#helloworld" {
			if !reflect.DeepEqual(p.NearDuplicateOf, []string{"#HelloWorld"}) {
				t.Fatalf("nearDuplicateOf = %v", p.NearDuplicateOf)
			}
			return
		}
	}
	t.Fatal("#helloworld not in the pending list")
}

// Case handling of proposal names (#251), decided from the key derivation:
// the hashtag key is the first 16 bytes of sha256 of the exact name (firmware
// docs/companion_protocol.md; companion app derivePskFromHashtag), so
// #HelloWorld and #helloworld are different channels. They stay separate
// proposals; the admin list points out the near-duplicate instead.
func TestAdminListFlagsCaseNearDuplicates(t *testing.T) {
	ps := newRevokeFixture(t)
	if err := ps.queue.WriteBuiltinNames([]string{"#test", "#chat"}); err != nil {
		t.Fatal(err)
	}
	ps.insert("aaaaaaaaaaaaaaaa", "#HelloWorld", channelregistry.StatusApproved)
	ps.insert("bbbbbbbbbbbbbbbb", "#helloworld", channelregistry.StatusApproved)
	ps.insert("cccccccccccccccc", "#Test", channelregistry.StatusPending) // near a built-in name
	ps.insert("dddddddddddddddd", "#unique", channelregistry.StatusPending)
	ps.insert("eeeeeeeeeeeeeeee", "#HELLOWORLD", channelregistry.StatusRejected)

	near := func(status string) map[string][]string {
		q := ""
		if status != "" {
			q = "?status=" + status
		}
		w := ps.do("GET", "/api/admin/channel-proposals"+q, "", strongTestKey)
		out := map[string][]string{}
		for _, p := range decode[ChannelProposalListResponse](t, w).Proposals {
			out[p.Name] = p.NearDuplicateOf
		}
		return out
	}
	all := near("")
	want := map[string][]string{
		"#HelloWorld": {"#HELLOWORLD", "#helloworld"},
		"#helloworld": {"#HELLOWORLD", "#HelloWorld"},
		"#HELLOWORLD": {"#HelloWorld", "#helloworld"},
		"#Test":       {"#test"},
		"#unique":     nil,
	}
	if !reflect.DeepEqual(all, want) {
		t.Fatalf("nearDuplicateOf = %v\nwant %v", all, want)
	}
	// A status filter must not hide the other-status duplicate.
	if got := near("pending")["#Test"]; !reflect.DeepEqual(got, []string{"#test"}) {
		t.Fatalf("pending view: %v", got)
	}
	// The field is omitted when there is nothing to warn about.
	w := ps.do("GET", "/api/admin/channel-proposals?status=pending", "", strongTestKey)
	if strings.Count(w.Body.String(), `"nearDuplicateOf"`) != 1 {
		t.Fatalf("nearDuplicateOf must be omitted when empty: %s", w.Body)
	}
}

// Perf bound for the per-request filter: 5000 channels, a few revoked.
func BenchmarkHideRevoked(b *testing.B) {
	p := &channelProposalService{now: time.Now, db: new(sql.DB)} // never queried: the snapshot is fresh
	p.approved = &approvedSnapshot{
		hidden:  map[string]bool{"#r1": true, "#r2": true, "#r3": true},
		expires: time.Now().Add(time.Hour),
	}
	chans := make([]map[string]interface{}, 5000)
	for i := range chans {
		chans[i] = map[string]interface{}{"name": fmt.Sprintf("#chan%d", i)}
	}
	chans[10]["name"] = "#r1"
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp := ChannelListResponse{Channels: chans}
		p.hideRevoked(ctx, &resp)
		if len(resp.Channels) != 4999 {
			b.Fatalf("len = %d", len(resp.Channels))
		}
	}
}

// ---------------------------------------------------------------------------
// #276: follow-ups to the round-2 review of #257.
// ---------------------------------------------------------------------------

// addChannelTrafficAt stores one decoded message on a channel with an
// explicit timestamp, so the order GetChannels returns (last_activity DESC)
// is deterministic and a test can place a channel in the middle of the list.
func (ps *proposalServer) addChannelTrafficAt(name, firstSeen string) {
	ps.t.Helper()
	hash := fmt.Sprintf("%x", name) // unique per name
	if _, err := ps.srv.db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		VALUES ('EEFF', ?, ?, 1, 5, ?, ?)`,
		hash, firstSeen, `{"type":"CHAN","channel":"`+name+`","text":"alice: hello","sender":"alice"}`, name); err != nil {
		ps.t.Fatal(err)
	}
	ps.srv.db.channelsCache.reset()
}

// hiddenNamesFromList returns the hiddenChannels field of GET /api/channels.
func (ps *proposalServer) hiddenNamesFromList(query string) []string {
	ps.t.Helper()
	w := ps.do("GET", "/api/channels"+query, "", "")
	if w.Code != 200 {
		ps.t.Fatalf("GET /api/channels%s = %d: %s", query, w.Code, w.Body)
	}
	return decode[ChannelListResponse](ps.t, w).HiddenChannels
}

// cachedChannelNames returns the names in GetChannels' cached slice, in order.
func (ps *proposalServer) cachedChannelNames() []string {
	ps.t.Helper()
	cached, err := ps.srv.db.GetChannels("")
	if err != nil {
		ps.t.Fatal(err)
	}
	names := make([]string, len(cached))
	for i, ch := range cached {
		names[i] = fmt.Sprint(ch["name"])
	}
	return names
}

// #276 (3), privacy-relevant: hiddenChannels is on the public GET
// /api/channels, while proposals themselves are only visible through the
// authenticated admin route. The EXISTS (… transmissions …) clause is what
// keeps a proposal that has no stored traffic out of the field: without it
// every pending suggestion — a name anyone can submit — would be published
// there. Nothing in the list changes either way, so only this assertion
// catches it (mutant G1: the EXISTS clause dropped).
func TestHiddenChannelsNeverNamesAProposalWithoutTraffic(t *testing.T) {
	ps := newRevokeFixture(t)
	// One real hidden channel, so hiddenChannels is populated at all.
	ps.addChannelTraffic("#helloworld")
	ps.insert("aaaaaaaaaaaaaaaa", "#helloworld", channelregistry.StatusRevoked)
	// Proposals without stored traffic, in every not-approved status. The
	// pending one is the privacy case: an unreviewed suggestion.
	ps.insert("bbbbbbbbbbbbbbbb", "#pendingsecret", channelregistry.StatusPending)
	ps.insert("cccccccccccccccc", "#rejectedsecret", channelregistry.StatusRejected)
	ps.insert("dddddddddddddddd", "#revokednotraffic", channelregistry.StatusRevoked)
	ps.expireApproved()

	got := ps.hiddenNamesFromList("")
	if !reflect.DeepEqual(got, []string{"#helloworld"}) {
		t.Fatalf("hiddenChannels = %v, want only the channel that has stored traffic ([#helloworld])", got)
	}
	for _, name := range []string{"#pendingsecret", "#rejectedsecret", "#revokednotraffic"} {
		if containsName(got, name) {
			t.Fatalf("%s has no stored traffic and must never be published in hiddenChannels: %v", name, got)
		}
	}
	// Same on ?includeEncrypted=true, the other path into hideRevoked.
	if got := ps.hiddenNamesFromList("?includeEncrypted=true"); !reflect.DeepEqual(got, []string{"#helloworld"}) {
		t.Fatalf("hiddenChannels with includeEncrypted = %v", got)
	}
	// The names were never listed either, so hiding them would be pointless
	// as well as public.
	if listed := ps.listedNames(""); containsName(listed, "#pendingsecret") {
		t.Fatalf("precondition: a proposal without traffic is not in the list anyway: %v", listed)
	}
}

// #276 (2): on the default request (no includeEncrypted) hideRevoked receives
// GetChannels' cached slice itself — handleChannels only copies on the
// includeEncrypted path, which is the only path
// TestRevokedFilterDoesNotMutateCacheAndKeepsEncrypted covers. Compacting in
// place there corrupts the shared cache for every later request (mutant G2:
// `out := resp.Channels`).
func TestPlainChannelListRequestDoesNotMutateTheCachedSlice(t *testing.T) {
	ps := newRevokeFixture(t)
	// Distinct timestamps: GetChannels orders by last_activity DESC, so the
	// hidden channel sits in the middle and compacting in place would shift
	// the channels after it over it.
	ps.addChannelTrafficAt("#zulu", "2024-03-03T00:00:00Z")
	ps.addChannelTrafficAt("#helloworld", "2024-02-02T00:00:00Z")
	ps.addChannelTrafficAt("#alpha", "2024-01-01T00:00:00Z")
	ps.insert("aaaaaaaaaaaaaaaa", "#helloworld", channelregistry.StatusRevoked)
	ps.expireApproved()

	before := ps.cachedChannelNames()
	idx := -1
	for i, n := range before {
		if n == "#helloworld" {
			idx = i
		}
	}
	if idx < 0 || idx >= len(before)-1 {
		t.Fatalf("precondition: the hidden channel must sit before the last cached entry so an in-place compaction is detectable, cache = %v", before)
	}

	for i := 0; i < 3; i++ {
		got := ps.listedNames("")
		if containsName(got, "#helloworld") {
			t.Fatalf("request %d: revoked channel listed: %v", i, got)
		}
		if !containsName(got, "#zulu") || !containsName(got, "#alpha") {
			t.Fatalf("request %d: the other channels must stay listed: %v", i, got)
		}
	}

	after := ps.cachedChannelNames()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the plain request altered GetChannels' cached slice:\nbefore %v\nafter  %v", before, after)
	}
}

// #276 (1) / round-2 finding 1, semantics pinned deliberately: a name with
// stored traffic that the ingestor no longer decrypts through its config (an
// operator removed it from hashChannels/channelKeys, or it is past the
// 4096-name cap of builtin-channels.json) is hidden by an anonymous pending
// suggestion, before any administrator acted on it.
//
// This is kept, not fixed, and the trade-off is documented in
// docs/api-spec.md and docs/user-guide/channels.md. "Hide only names that
// were ever approved" needs a marker that survives a re-suggestion, and the
// obvious candidate does not exist: submitChannelProposal resurrects a
// revoked row with `reviewed_at = NULL` (that reset is what makes a
// resubmission idempotent), so a reviewed_at rule cannot tell a fresh
// suggestion from a re-suggestion after a revoke — it would re-open the hole
// #257 round 2 closed, where anyone could unhide a revoked channel by
// suggesting the name again. A new history column is an ingestor-side schema
// change, out of scope here, and cmd/server stays read-only.
//
// What bounds the trade-off: the effect is list-level only (the history stays
// readable), it needs stored traffic for that exact name, a name the ingestor
// still decrypts through its config is never hidden, the administrator sees
// the suggestion in the pending queue, and one Approve lists the channel
// again.
func TestAnonymousPendingSuggestionHidesAHistoricalConfigChannel(t *testing.T) {
	ps := newRevokeFixture(t) // the ingestor publishes #chat as a config name
	ps.addChannelTraffic("#oldcfg")
	ps.addChannelTraffic("#chat")
	ps.expireApproved()
	if got := ps.listedNames(""); !containsName(got, "#oldcfg") || !containsName(got, "#chat") {
		t.Fatalf("precondition: both channels are listed without any proposal: %v", got)
	}

	// An anonymous suggestion the administrator has not reviewed: this is the
	// row submitChannelProposal inserts for an unknown name with
	// autoApprove off.
	ps.insert("aaaaaaaaaaaaaaaa", "#oldcfg", channelregistry.StatusPending)
	// The same for a name the ingestor still decrypts through its config.
	ps.insert("bbbbbbbbbbbbbbbb", "#chat", channelregistry.StatusPending)
	ps.expireApproved()

	got := ps.listedNames("")
	if containsName(got, "#oldcfg") {
		t.Fatalf("documented trade-off: a pending suggestion hides a name the ingestor no longer decrypts: %v", got)
	}
	if !containsName(got, "#chat") {
		t.Fatalf("a config-decrypted name must never be hidden, whatever its proposal says: %v", got)
	}
	if hidden := ps.hiddenNamesFromList(""); !reflect.DeepEqual(hidden, []string{"#oldcfg"}) {
		t.Fatalf("hiddenChannels = %v, want [#oldcfg]", hidden)
	}
	// The history is untouched and still readable by name.
	w := ps.do("GET", "/api/channels/%23oldcfg/messages", "", "")
	if w.Code != 200 || decode[ChannelMessagesResponse](t, w).Total != 1 {
		t.Fatalf("the history of a hidden channel must stay readable: %d %s", w.Code, w.Body)
	}
	// One administrator action undoes it.
	ps.setStatus("aaaaaaaaaaaaaaaa", channelregistry.StatusApproved)
	ps.expireApproved()
	if got := ps.listedNames(""); !containsName(got, "#oldcfg") {
		t.Fatalf("approving the suggestion must list the channel again: %v", got)
	}
}
