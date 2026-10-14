package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/meshcore-analyzer/channelregistry"
	"github.com/meshcore-analyzer/regions"
)

// Review of #355, gap 1: a SIGHUP reload rebuilds the config layer only. It
// must neither drop an approved scope nor bring back a revoked one, and a
// revoked approval of a configured name must leave the configured key live.
// Uses a real signal and the real runner, so the decisions go through the
// database exactly as in production.
func TestScopeDecisionsSurviveRealSIGHUP(t *testing.T) {
	configPath := writeTestConfig(t, nil, []string{"#cfg"})
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "sighup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.db.Exec(`INSERT INTO observer_neighbors(observer_id,neighbor_pubkey,scopes,status) VALUES('obs','node','#kept,#gone,#cfg','responded')`); err != nil {
		t.Fatal(err)
	}
	hk := newHotKeys(nil, mustRegionKeys(t, cfg, configPath))
	runner := newScopeApprovalRunner(store, hk)
	ctx := context.Background()
	decide := func(op, name, want string) {
		t.Helper()
		cmd := channelregistry.Command{RequestID: channelregistry.NewID(), Op: op, Name: name, CreatedAt: time.Now().UnixMilli()}
		if res := runner.apply(ctx, cmd); res.Status != want {
			t.Fatalf("%s %s = %+v, want %s", op, name, res, want)
		}
	}
	decide(channelregistry.OpScopeApprove, "#kept", "approved")
	decide(channelregistry.OpScopeApprove, "#gone", "approved")
	decide(channelregistry.OpScopeApprove, "#cfg", "approved")
	decide(channelregistry.OpScopeRevoke, "#gone", "revoked")
	decide(channelregistry.OpScopeRevoke, "#cfg", "revoked")

	stop := startSIGHUPReload(hk, configPath)
	defer stop()
	// #later marks that the reload has run.
	if err := os.WriteFile(configPath, mustJSON(t, struct {
		HashRegions []string `json:"hashRegions"`
	}{HashRegions: []string{"#cfg", "#later"}}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := hk.Regions()["#later"]; ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("SIGHUP reload did not run: %v", hk.Regions())
		}
		time.Sleep(10 * time.Millisecond)
	}
	assertRegionSet := func(label string, got map[string][]byte) {
		t.Helper()
		for _, name := range []string{"#kept", "#cfg", "#later"} {
			if _, ok := got[name]; !ok {
				t.Errorf("%s: %s missing", label, name)
			}
		}
		if _, ok := got["#gone"]; ok {
			t.Errorf("%s: revoked #gone is live again", label)
		}
		if len(got) != 3 {
			t.Errorf("%s: regions = %d, want 3", label, len(got))
		}
	}
	assertRegionSet("after SIGHUP", hk.Regions())

	// A restart must load the same overlay from the database.
	cfg, err = LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newHotKeys(nil, mustRegionKeys(t, cfg, configPath))
	if err := newScopeApprovalRunner(store, restarted).LoadApproved(ctx); err != nil {
		t.Fatal(err)
	}
	assertRegionSet("after restart", restarted.Regions())
}

// scopedTxtMsg builds a TRANSPORT_FLOOD TXT_MSG whose Code1 is what a node
// in scope name would send for this payload.
func scopedTxtMsg(name string, payload []byte) string {
	const txtMsg = 2
	h := sha256.Sum256([]byte(name))
	mac := hmacSHA256(h[:16], append([]byte{txtMsg}, payload...))
	code := uint16(mac[0]) | uint16(mac[1])<<8
	if code == 0 {
		code = 1
	} else if code == 0xFFFF {
		code = 0xFFFE
	}
	header := fmt.Sprintf("%02X", txtMsg<<2) // route type 0 = TRANSPORT_FLOOD
	return header + fmt.Sprintf("%02X%02X", byte(code), byte(code>>8)) + "0000" + "00" + hex.EncodeToString(payload)
}

// Review of #355, gap 2: approval changes the live keys for packets that
// arrive afterwards. Transmissions already stored keep their scope_name,
// including when the same packet is heard again after the approval, and a
// revocation leaves rows that matched while it was approved untouched.
func TestScopeApprovalAppliesToNewPacketsOnly(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.db.Exec(`INSERT INTO observer_neighbors(observer_id,neighbor_pubkey,scopes,status) VALUES('obs','node','#late','responded')`); err != nil {
		t.Fatal(err)
	}
	hk := newHotKeys(nil, nil)
	runner := newScopeApprovalRunner(store, hk)
	ctx := context.Background()
	ingest := func(observer, raw string) {
		t.Helper()
		msg := &mockMessage{topic: "meshcore/SJC/" + observer + "/packets", payload: []byte(`{"raw":"` + raw + `"}`)}
		handleMessage(store, "test", MQTTSource{Name: "test"}, msg, hk.Channels(), hk.Regions(), &Config{})
	}
	scopeOf := func(raw string) string {
		t.Helper()
		var got sql.NullString
		if err := store.db.QueryRow(`SELECT scope_name FROM transmissions WHERE raw_hex = ?`, raw).Scan(&got); err != nil {
			t.Fatalf("transmission %s: %v", raw[:16], err)
		}
		if !got.Valid {
			t.Fatalf("transmission %s stored as not transport-scoped", raw[:16])
		}
		return got.String
	}
	decide := func(op string) {
		t.Helper()
		cmd := channelregistry.Command{RequestID: channelregistry.NewID(), Op: op, Name: "#late", CreatedAt: time.Now().UnixMilli()}
		if res := runner.apply(ctx, cmd); res.Error != "" {
			t.Fatalf("%s: %+v", op, res)
		}
	}

	before := scopedTxtMsg("#late", []byte("before approval, 0123456789"))
	during := scopedTxtMsg("#late", []byte("while approved, 0123456789a"))
	after := scopedTxtMsg("#late", []byte("after revocation, 0123456789"))

	ingest("obs1", before)
	if got := scopeOf(before); got != "" {
		t.Fatalf("before approval scope_name = %q, want unknown-scoped", got)
	}
	decide(channelregistry.OpScopeApprove)
	ingest("obs2", before) // the same transmission, heard again after approval
	ingest("obs1", during)
	if got := scopeOf(before); got != "" {
		t.Errorf("stored transmission re-matched to %q; approval must not rewrite history", got)
	}
	if got := scopeOf(during); got != "#late" {
		t.Errorf("packet after approval scope_name = %q, want #late", got)
	}
	decide(channelregistry.OpScopeRevoke)
	ingest("obs1", after)
	if got := scopeOf(during); got != "#late" {
		t.Errorf("revocation rewrote a stored match to %q", got)
	}
	if got := scopeOf(after); got != "" {
		t.Errorf("packet after revocation scope_name = %q, want unknown-scoped", got)
	}
}

// Review of #355, gap 3. With 16-bit code1 the chance that a packet from a
// configured region also matches another region (and is then reported as
// unknown-scoped) is 1-(1-1/65534)^(N-1); a packet from an unconfigured
// scope is misattributed to a configured one with about the same chance.
// The approval cap is what bounds the increase. docs/deployment.md quotes
// these numbers for the ~1,100-region reference deployment; this test keeps
// the cap and that statement in step. Measured on 15,000 random payloads:
// 1.659 % → 1.851 % ambiguous, 1.661 % → 1.863 % misattributed.
func TestApprovedScopeCapCollisionBudget(t *testing.T) {
	const referenceConfigured = 1100
	ambiguous := func(n int) float64 { return 1 - math.Pow(1-1.0/65534, float64(n-1)) }
	added := ambiguous(referenceConfigured+regions.MaxApprovedScopes) - ambiguous(referenceConfigured)
	if added > 0.002 {
		t.Fatalf("cap %d adds %.3f percentage points of ambiguous matches at %d configured regions; the documented budget is 0.2",
			regions.MaxApprovedScopes, 100*added, referenceConfigured)
	}
	if regions.MaxApprovedScopes < 64 {
		t.Fatalf("cap %d is below the ~50 unknown scopes seen on a real deployment plus headroom", regions.MaxApprovedScopes)
	}
}

// BenchmarkMatchScopeApprovalCap measures what the cap costs on the packet
// path: one HMAC per effective region for every scoped packet.
func BenchmarkMatchScopeApprovalCap(b *testing.B) {
	for _, n := range []int{1100, 1100 + regions.MaxApprovedScopes} {
		keys := make(map[string][]byte, n)
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("#r%04d", i)
			h := sha256.Sum256([]byte(name))
			keys[name] = h[:16]
		}
		payload := []byte("benchmark payload of a typical scoped text message.....")
		raw := scopedTxtMsg("#r0007", payload)
		code1 := raw[2:6]
		if matchScope(keys, 2, payload, code1) != "#r0007" {
			b.Fatal("benchmark fixture is ambiguous")
		}
		b.Run(fmt.Sprintf("regions=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				matchScope(keys, 2, payload, code1)
			}
		})
	}
}
