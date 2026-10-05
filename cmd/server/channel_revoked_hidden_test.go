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

// With no revoked proposal the filter must be a no-op that returns the
// cached slice untouched (no copy per request).
func TestVisibleChannelsIsNoOpWithoutRevokedNames(t *testing.T) {
	ps := newRevokeFixture(t)
	in := []map[string]interface{}{{"name": "#a"}, {"name": "#b"}}
	out := ps.svc.visibleChannels(context.Background(), in)
	if !reflect.DeepEqual(in, out) || &in[0] != &out[0] {
		t.Fatalf("expected the same slice back, got %v", out)
	}
}

// A rolling upgrade (table not created yet) must not hide or break anything.
func TestRevokedFilterMissingTable(t *testing.T) {
	ps := newProposalServer(t, strongTestKey, enabledConfig(nil), false)
	if got := ps.listedNames(""); !containsName(got, "#test") {
		t.Fatalf("list = %v", got)
	}
}

func TestListRevokedNames(t *testing.T) {
	ps := newRevokeFixture(t)
	ps.insert("aaaaaaaaaaaaaaaa", "#one", channelregistry.StatusRevoked)
	ps.insert("bbbbbbbbbbbbbbbb", "#two", channelregistry.StatusApproved)
	ps.insert("cccccccccccccccc", "#three", channelregistry.StatusRevoked)
	ps.insert("dddddddddddddddd", "#four", channelregistry.StatusPending)
	names, err := channelregistry.ListRevokedNames(context.Background(), ps.srv.db.conn, 10)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	if want := []string{"#one", "#three"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("revoked = %v, want %v", names, want)
	}
	if _, err := ps.srv.db.conn.Exec(`DROP TABLE channel_proposals`); err != nil {
		t.Fatal(err)
	}
	if names, err := channelregistry.ListRevokedNames(context.Background(), ps.srv.db.conn, 10); err != nil || len(names) != 0 {
		t.Fatalf("missing table = %v, %v; want empty", names, err)
	}
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
func BenchmarkVisibleChannels(b *testing.B) {
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
		if out := p.visibleChannels(ctx, chans); len(out) != 4999 {
			b.Fatalf("len = %d", len(out))
		}
	}
}
