package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/channelregistry"
	"github.com/meshcore-analyzer/channelregistry/antennaclaim"
)

// #789 M2a — owner antenna-height claims, the writer side.
//
// Every advert here is firmware-shaped (AdvertDataBuilder::encodeTo: flags,
// int32 lat/lon ×1e6, name) and signed with a TEST ed25519 key derived from a
// fixed seed. No real node key is used anywhere.

const claimCode = "7KX9QM"

type claimRig struct {
	t      *testing.T
	store  *Store
	runner *antennaClaimRunner
	queue  *channelregistry.Queue
	clock  time.Time
	pub    ed25519.PublicKey
	priv   ed25519.PrivateKey
	pk     string
	ts     uint32
}

func testKey(seedByte byte) (ed25519.PublicKey, ed25519.PrivateKey) {
	seed := sha256.Sum256([]byte{'c', 'o', 'r', 'e', 's', 'c', 'o', 'p', 'e', '-', 't', 'e', 's', 't', seedByte})
	priv := ed25519.NewKeyFromSeed(seed[:])
	return priv.Public().(ed25519.PublicKey), priv
}

func newClaimRig(t *testing.T, cfg *antennaclaim.Config) *claimRig {
	t.Helper()
	store, err := OpenStoreWithInterval(filepath.Join(t.TempDir(), "test.db"), 300)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	r := &claimRig{t: t, store: store, clock: time.UnixMilli(1_800_000_000_000), ts: 1_800_000_000}
	r.runner = newAntennaClaimRunner(store, cfg)
	r.runner.now = func() time.Time { return r.clock }
	store.antennaClaims = r.runner
	r.queue = channelregistry.NewQueue(antennaclaim.QueueDir(store.path))
	r.pub, r.priv = testKey(1)
	r.pk = hex.EncodeToString(r.pub)
	return r
}

// firmwareAppdata builds appdata as the firmware does: flags, then the
// position, then the name — the name not cut, so a test can put the code
// past the on-air budget.
func firmwareAppdata(role byte, lat, lon *float64, name string) []byte {
	flags := role | 0x80
	if lat != nil {
		flags |= 0x10
	}
	out := []byte{flags}
	if lat != nil {
		var b [8]byte
		binary.LittleEndian.PutUint32(b[0:4], uint32(int32(*lat*1e6)))
		binary.LittleEndian.PutUint32(b[4:8], uint32(int32(*lon*1e6)))
		out = append(out, b[:]...)
	}
	return append(out, name...)
}

var claimLat, claimLon = 55.676100, 12.568300

// advert sends one advert through the real ingest path. A new timestamp
// per call makes a new transmission, as a re-sent advert does.
func (r *claimRig) advert(pub ed25519.PublicKey, priv ed25519.PrivateKey, name string, withPos bool) string {
	r.t.Helper()
	r.ts++
	var lat, lon *float64
	if withPos {
		lat, lon = &claimLat, &claimLon
	}
	raw := buildAdvertHex(pub, priv, r.ts, firmwareAppdata(0x02, lat, lon, name))
	r.ingest(raw)
	return raw
}

func (r *claimRig) ingest(raw string) {
	r.t.Helper()
	msg := newMockMsg("meshcore/DK/obs1/packet", `{"raw":"`+raw+`","origin":"TestObs"}`)
	handleMessage(r.store, "test", MQTTSource{Name: "test"}, msg, nil, nil, &Config{})
}

func (r *claimRig) enqueue(c antennaclaim.Command) {
	r.t.Helper()
	if c.CreatedAt == 0 {
		c.CreatedAt = r.clock.UnixMilli()
	}
	if err := r.queue.EnqueueValue(c.RequestID, c, 100); err != nil {
		r.t.Fatal(err)
	}
}

func (r *claimRig) claim(id string, h float64, code string) {
	r.t.Helper()
	r.enqueue(antennaclaim.Command{RequestID: id, Op: antennaclaim.OpClaim, PubKey: r.pk, HeightM: &h, Code: code})
	r.runner.RunOnce(context.Background())
}

func (r *claimRig) result(id string) antennaclaim.Result {
	r.t.Helper()
	var res antennaclaim.Result
	if queued, err := r.queue.LookupValue(id, &res); err != nil || queued {
		r.t.Fatalf("result %s: queued=%v err=%v", id, queued, err)
	}
	return res
}

type claimRow struct {
	status, approvedBy, proofHash string
	provenAt                      sql.NullInt64
	proofLat, proofLon            sql.NullFloat64
}

func (r *claimRig) row(id string) claimRow {
	r.t.Helper()
	var c claimRow
	var by, hash sql.NullString
	err := r.store.db.QueryRow(`SELECT status, approved_by, proof_hash, proven_at, proof_lat, proof_lon
		FROM node_antenna_claims WHERE request_id = ?`, id).Scan(&c.status, &by, &hash, &c.provenAt, &c.proofLat, &c.proofLon)
	if err != nil {
		r.t.Fatalf("row %s: %v", id, err)
	}
	c.approvedBy, c.proofHash = by.String, hash.String
	return c
}

const (
	claimA = "a000000000000001"
	claimB = "b000000000000002"
	claimC = "c000000000000003"
	claimD = "d000000000000004"
)

func TestAntennaClaim789_ValidAdvertProves(t *testing.T) {
	r := newClaimRig(t, nil)
	r.claim(claimA, 18.5, claimCode)
	if res := r.result(claimA); res.Status != antennaclaim.StatusPending || res.ClaimID != claimA {
		t.Fatalf("claim result %+v", res)
	}
	raw := r.advert(r.pub, r.priv, "Roof Rptr "+claimCode, true)
	row := r.row(claimA)
	if row.status != antennaclaim.StatusProven || row.approvedBy != "" {
		t.Fatalf("status %+v, want proven and not approved (auto-approval off by default)", row)
	}
	if row.proofHash != ComputeContentHash(raw) || !row.provenAt.Valid || row.provenAt.Int64 != r.clock.UnixMilli() {
		t.Fatalf("proof %+v (hash want %s)", row, ComputeContentHash(raw))
	}
	if !row.proofLat.Valid || row.proofLat.Float64 != claimLat || row.proofLon.Float64 != claimLon {
		t.Fatalf("proof position %+v", row)
	}
	if n := r.runner.pendingCount(); n != 0 {
		t.Fatalf("code not consumed: %d pending in memory", n)
	}
}

func TestAntennaClaim789_NegativeCases(t *testing.T) {
	otherPub, otherPriv := testKey(2)
	cases := []struct {
		name string
		send func(r *claimRig)
	}{
		{"wrong pubkey", func(r *claimRig) { r.advert(otherPub, otherPriv, "Roof "+claimCode, true) }},
		{"forged signature", func(r *claimRig) {
			// Signed by another key but carrying this node's pubkey: the
			// ingestor drops it before any claim code runs.
			r.ts++
			raw := buildAdvertHex(r.pub, otherPriv, r.ts, firmwareAppdata(0x02, &claimLat, &claimLon, "Roof "+claimCode))
			r.ingest(raw)
		}},
		{"code past the 23-byte budget", func(r *claimRig) { r.advert(r.pub, r.priv, "Roof Repeater Abcd "+claimCode, true) }},
		{"substring, not a token", func(r *claimRig) { r.advert(r.pub, r.priv, "Roof x"+claimCode, true) }},
		{"lower case", func(r *claimRig) { r.advert(r.pub, r.priv, "Roof 7kx9qm", true) }},
		{"height proof without a position", func(r *claimRig) { r.advert(r.pub, r.priv, "Roof "+claimCode, false) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newClaimRig(t, nil)
			r.claim(claimA, 18.5, claimCode)
			c.send(r)
			if row := r.row(claimA); row.status != antennaclaim.StatusPending || row.provenAt.Valid {
				t.Fatalf("%s proved the claim: %+v", c.name, row)
			}
			// The genuine advert still proves it afterwards.
			r.advert(r.pub, r.priv, "Roof "+claimCode, true)
			if row := r.row(claimA); row.status != antennaclaim.StatusProven {
				t.Fatalf("genuine advert after %s: %+v", c.name, row)
			}
		})
	}
}

func TestAntennaClaim789_CodeBeyondBudgetStillVerifies(t *testing.T) {
	// The over-long appdata is still a valid signature for CoreScope, so the
	// budget rule, not the signature check, is what refuses it.
	r := newClaimRig(t, nil)
	r.claim(claimA, 18.5, claimCode)
	r.advert(r.pub, r.priv, "Roof Repeater Abcd "+claimCode, true)
	var n int
	r.store.db.QueryRow(`SELECT COUNT(*) FROM transmissions`).Scan(&n)
	if n != 1 {
		t.Fatalf("the long advert was not stored (%d): the test would not exercise the budget", n)
	}
}

func TestAntennaClaim789_ExpiredCode(t *testing.T) {
	r := newClaimRig(t, nil)
	r.claim(claimA, 18.5, claimCode)
	r.clock = r.clock.Add(24*time.Hour + time.Second)
	r.advert(r.pub, r.priv, "Roof "+claimCode, true)
	if row := r.row(claimA); row.status != antennaclaim.StatusPending {
		t.Fatalf("expired code proved: %+v", row)
	}
	r.runner.RunOnce(context.Background())
	if row := r.row(claimA); row.status != antennaclaim.StatusExpired {
		t.Fatalf("not expired by the runner: %+v", row)
	}
	if r.runner.pendingCount() != 0 {
		t.Fatal("expired claim kept in memory")
	}
}

func TestAntennaClaim789_ReusedCode(t *testing.T) {
	r := newClaimRig(t, nil)
	r.claim(claimA, 18.5, claimCode)
	r.advert(r.pub, r.priv, "Roof "+claimCode, true)
	first := r.row(claimA)
	r.claim(claimB, 30, "QQQQQQ")
	r.clock = r.clock.Add(time.Minute)
	r.advert(r.pub, r.priv, "Roof "+claimCode, true) // a new advert, old code
	if row := r.row(claimA); row.provenAt != first.provenAt || row.proofHash != first.proofHash {
		t.Fatalf("a used code proved again: %+v vs %+v", row, first)
	}
	if row := r.row(claimB); row.status != antennaclaim.StatusPending {
		t.Fatalf("another claim's code proved claim B: %+v", row)
	}
}

func TestAntennaClaim789_ReplayedAdvertIsNotNew(t *testing.T) {
	r := newClaimRig(t, nil)
	// The advert is first seen before the claim exists (it cannot carry a
	// fresh code; here it simulates an old advert that does).
	raw := buildAdvertHex(r.pub, r.priv, r.ts+1, firmwareAppdata(0x02, &claimLat, &claimLon, "Roof "+claimCode))
	r.ingest(raw)
	r.claim(claimA, 18.5, claimCode)
	r.ingest(raw) // the same bytes again: same content hash, not new
	if row := r.row(claimA); row.status != antennaclaim.StatusPending {
		t.Fatalf("a replayed advert proved the claim: %+v", row)
	}
}

func TestAntennaClaim789_SecondRequestDoesNotEvict(t *testing.T) {
	r := newClaimRig(t, nil)
	r.claim(claimA, 18.5, claimCode)
	r.claim(claimB, 40, "BBBBBB")
	r.claim(claimC, 50, "CCCCCC")
	r.claim(claimD, 60, "DDDDDD") // a fourth: refused, nothing evicted
	if res := r.result(claimD); res.Status != antennaclaim.RequestError || res.Error != errTooManyNodeClaims.Error() {
		t.Fatalf("fourth claim %+v", res)
	}
	r.advert(r.pub, r.priv, "Roof "+claimCode, true)
	if row := r.row(claimA); row.status != antennaclaim.StatusProven {
		t.Fatalf("first claim evicted: %+v", row)
	}
	for _, id := range []string{claimB, claimC} {
		if row := r.row(id); row.status != antennaclaim.StatusPending {
			t.Fatalf("claim %s: %+v", id, row)
		}
	}
}

func TestAntennaClaim789_TotalPendingCap(t *testing.T) {
	r := newClaimRig(t, &antennaclaim.Config{MaxPending: 2})
	r.claim(claimA, 1, "AAAAAA")
	r.claim(claimB, 2, "BBBBBB")
	r.claim(claimC, 3, "CCCCCC")
	if res := r.result(claimC); res.Status != antennaclaim.RequestError || res.Error != errTooManyClaims.Error() {
		t.Fatalf("over the total cap: %+v", res)
	}
	if n := r.runner.pendingCount(); n != 2 {
		t.Fatalf("pending in memory %d, want 2", n)
	}
}

func TestAntennaClaim789_ClaimReplayIsIdempotent(t *testing.T) {
	r := newClaimRig(t, nil)
	r.claim(claimA, 18.5, claimCode)
	h := 99.0
	r.enqueue(antennaclaim.Command{RequestID: claimA, Op: antennaclaim.OpClaim, PubKey: r.pk, HeightM: &h, Code: "ZZZZZZ"})
	r.runner.RunOnce(context.Background())
	var n int
	var got float64
	var code string
	r.store.db.QueryRow(`SELECT COUNT(*), MAX(height_m), MAX(code) FROM node_antenna_claims`).Scan(&n, &got, &code)
	if n != 1 || got != 18.5 || code != claimCode {
		t.Fatalf("replay changed the claim: n=%d h=%v code=%s", n, got, code)
	}
}

func TestAntennaClaim789_InvalidCommandRefused(t *testing.T) {
	r := newClaimRig(t, nil)
	h := 150.0
	r.enqueue(antennaclaim.Command{RequestID: claimA, Op: antennaclaim.OpClaim, PubKey: r.pk, HeightM: &h, Code: claimCode})
	r.runner.RunOnce(context.Background())
	if res := r.result(claimA); res.Status != antennaclaim.RequestError {
		t.Fatalf("over-cap height accepted from the queue: %+v", res)
	}
	var n int
	r.store.db.QueryRow(`SELECT COUNT(*) FROM node_antenna_claims`).Scan(&n)
	if n != 0 {
		t.Fatal("row inserted")
	}
}

func TestAntennaClaim789_AutoApprove(t *testing.T) {
	r := newClaimRig(t, &antennaclaim.Config{AutoApproveProven: true})
	r.claim(claimA, 18.5, claimCode)
	r.advert(r.pub, r.priv, "Roof "+claimCode, true)
	if row := r.row(claimA); row.status != antennaclaim.StatusApproved || row.approvedBy != antennaclaim.ApprovedByAuto {
		t.Fatalf("auto-approval: %+v", row)
	}
	// A flagged claim waits for an administrator even with auto-approval.
	h := 90.0
	r.enqueue(antennaclaim.Command{RequestID: claimB, Op: antennaclaim.OpClaim, PubKey: r.pk, HeightM: &h, Code: "BBBBBB", NeedsReview: true})
	r.runner.RunOnce(context.Background())
	r.advert(r.pub, r.priv, "Roof BBBBBB", true)
	if row := r.row(claimB); row.status != antennaclaim.StatusProven {
		t.Fatalf("flagged claim auto-approved: %+v", row)
	}
	if row := r.row(claimA); row.status != antennaclaim.StatusApproved {
		t.Fatalf("a proven claim superseded the approved one: %+v", row)
	}
}

func (r *claimRig) decide(id, op, claimID string) antennaclaim.Result {
	r.t.Helper()
	r.enqueue(antennaclaim.Command{RequestID: id, Op: op, ClaimID: claimID})
	r.runner.RunOnce(context.Background())
	return r.result(id)
}

func TestAntennaClaim789_AdminDecisions(t *testing.T) {
	r := newClaimRig(t, nil)
	r.claim(claimA, 18.5, claimCode)
	// Approving an unproven claim is a conflict.
	if res := r.decide("e000000000000001", antennaclaim.OpApprove, claimA); res.Status != antennaclaim.RequestError {
		t.Fatalf("approved unproven: %+v", res)
	}
	r.advert(r.pub, r.priv, "Roof "+claimCode, true)
	if res := r.decide("e000000000000002", antennaclaim.OpApprove, claimA); res.Status != antennaclaim.StatusApproved {
		t.Fatalf("approve: %+v", res)
	}
	if row := r.row(claimA); row.approvedBy != antennaclaim.ApprovedByAdmin {
		t.Fatalf("approved_by %+v", row)
	}
	// Repeating the decision is idempotent.
	if res := r.decide("e000000000000003", antennaclaim.OpApprove, claimA); res.Status != antennaclaim.StatusApproved {
		t.Fatalf("repeat approve: %+v", res)
	}
	// A newer approved claim supersedes the older one.
	r.claim(claimB, 25, "BBBBBB")
	r.advert(r.pub, r.priv, "Roof BBBBBB", true)
	r.decide("e000000000000004", antennaclaim.OpApprove, claimB)
	if row := r.row(claimA); row.status != antennaclaim.StatusSuperseded {
		t.Fatalf("older claim not superseded: %+v", row)
	}
	// Revoke works only on approved.
	if res := r.decide("e000000000000005", antennaclaim.OpRevoke, claimA); res.Status != antennaclaim.RequestError {
		t.Fatalf("revoked a superseded claim: %+v", res)
	}
	if res := r.decide("e000000000000006", antennaclaim.OpRevoke, claimB); res.Status != antennaclaim.StatusRevoked {
		t.Fatalf("revoke: %+v", res)
	}
	if row := r.row(claimB); row.approvedBy != "" {
		t.Fatalf("revoked row keeps approved_by: %+v", row)
	}
	// Reject: pending or proven only.
	r.claim(claimC, 30, "CCCCCC")
	if res := r.decide("e000000000000007", antennaclaim.OpReject, claimC); res.Status != antennaclaim.StatusRejected {
		t.Fatalf("reject pending: %+v", res)
	}
	if res := r.decide("e000000000000008", antennaclaim.OpReject, claimB); res.Status != antennaclaim.RequestError {
		t.Fatalf("rejected a revoked claim: %+v", res)
	}
	if res := r.decide("e000000000000009", antennaclaim.OpApprove, "f00000000000000f"); res.Status != antennaclaim.RequestError || res.Error != errClaimNotFound.Error() {
		t.Fatalf("unknown claim: %+v", res)
	}
	// A rejected pending claim can no longer be proven.
	r.advert(r.pub, r.priv, "Roof CCCCCC", true)
	if row := r.row(claimC); row.status != antennaclaim.StatusRejected {
		t.Fatalf("rejected claim proven: %+v", row)
	}
}

func TestAntennaClaim789_OwnerSelfClear(t *testing.T) {
	r := newClaimRig(t, nil)
	r.claim(claimA, 18.5, claimCode)
	r.advert(r.pub, r.priv, "Roof "+claimCode, true)
	r.decide("e000000000000001", antennaclaim.OpApprove, claimA)
	r.enqueue(antennaclaim.Command{RequestID: claimB, Op: antennaclaim.OpClaim, PubKey: r.pk, Clear: true, Code: "BBBBBB"})
	r.runner.RunOnce(context.Background())
	// A clear needs no position and no administrator.
	r.advert(r.pub, r.priv, "Roof BBBBBB", false)
	if row := r.row(claimB); row.status != antennaclaim.StatusApproved || row.approvedBy != antennaclaim.ApprovedByOwner {
		t.Fatalf("clear claim: %+v", row)
	}
	if row := r.row(claimA); row.status != antennaclaim.StatusSuperseded {
		t.Fatalf("height not cleared: %+v", row)
	}
}

func TestAntennaClaim789_PendingSurvivesRestart(t *testing.T) {
	r := newClaimRig(t, nil)
	r.claim(claimA, 18.5, claimCode)
	// A new runner (an ingestor restart) loads the pending claims from the table.
	r.runner = newAntennaClaimRunner(r.store, nil)
	r.runner.now = func() time.Time { return r.clock }
	r.store.antennaClaims = r.runner
	r.runner.RunOnce(context.Background())
	r.advert(r.pub, r.priv, "Roof "+claimCode, true)
	if row := r.row(claimA); row.status != antennaclaim.StatusProven {
		t.Fatalf("claim lost on restart: %+v", row)
	}
}

func TestAntennaClaim789_Prune(t *testing.T) {
	r := newClaimRig(t, &antennaclaim.Config{RetentionDays: 1})
	r.claim(claimA, 18.5, claimCode)
	r.advert(r.pub, r.priv, "Roof "+claimCode, true)
	r.decide("e000000000000001", antennaclaim.OpApprove, claimA)
	r.claim(claimB, 20, "BBBBBB")
	r.decide("e000000000000002", antennaclaim.OpReject, claimB)
	r.claim(claimC, 20, "CCCCCC") // will expire
	r.clock = r.clock.Add(3 * 24 * time.Hour)
	r.runner.RunOnce(context.Background())
	// Result files age by modification time.
	old := r.clock.Add(-48 * time.Hour)
	entries, _ := os.ReadDir(r.queue.Dir())
	for _, e := range entries {
		os.Chtimes(filepath.Join(r.queue.Dir(), e.Name()), old, old)
	}
	r.runner.Prune(context.Background())
	var ids []string
	rows, _ := r.store.db.Query(`SELECT request_id FROM node_antenna_claims ORDER BY request_id`)
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 1 || ids[0] != claimA {
		t.Fatalf("after retention %v, want only the approved claim", ids)
	}
	if left, _ := os.ReadDir(r.queue.Dir()); len(left) != 0 {
		t.Fatalf("queue results not pruned: %d files", len(left))
	}
}

func TestAntennaClaim789_NoRunnerNoMatch(t *testing.T) {
	// Without a runner (no queue configured) ingest is unchanged.
	store, err := OpenStoreWithInterval(filepath.Join(t.TempDir(), "t.db"), 300)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pub, priv := testKey(1)
	raw := buildAdvertHex(pub, priv, 1, firmwareAppdata(0x02, &claimLat, &claimLon, "Roof "+claimCode))
	handleMessage(store, "test", MQTTSource{Name: "test"}, newMockMsg("meshcore/DK/obs1/packet", `{"raw":"`+raw+`"}`), nil, nil, &Config{})
	var n int
	store.db.QueryRow(`SELECT COUNT(*) FROM transmissions`).Scan(&n)
	if n != 1 {
		t.Fatalf("advert not stored: %d", n)
	}
}

// BenchmarkAntennaClaim789_Match is the ingest hook's cost per new advert
// with 256 claims pending: a map lookup and a token scan of at most 32 bytes.
func BenchmarkAntennaClaim789_Match(b *testing.B) {
	r := &antennaClaimRunner{pending: map[string][]pendingClaim{}, now: time.Now, limits: (*antennaclaim.Config)(nil).Limits()}
	for i := 0; i < 256; i++ {
		pk := hex.EncodeToString(sha256.New().Sum([]byte{byte(i), byte(i >> 8)}))[:64]
		r.pending[pk] = []pendingClaim{{id: "a000000000000001", code: "AAAAAA", expiresAt: time.Now().Add(time.Hour).UnixMilli()}}
	}
	pk := hex.EncodeToString(make([]byte, 32))
	r.pending[pk] = []pendingClaim{{id: "a000000000000001", code: claimCode, expiresAt: time.Now().Add(time.Hour).UnixMilli()}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.match(pk, "Roof Repeater Abcd x", 23, true)
	}
}

func TestAntennaClaim789_NoProofWithSignatureValidationOff(t *testing.T) {
	// With validateSignatures off a forged advert is stored, but it must
	// never prove a claim: the hook itself requires a verified signature.
	r := newClaimRig(t, nil)
	r.claim(claimA, 18.5, claimCode)
	_, otherPriv := testKey(2)
	r.ts++
	raw := buildAdvertHex(r.pub, otherPriv, r.ts, firmwareAppdata(0x02, &claimLat, &claimLon, "Roof "+claimCode))
	off := false
	msg := newMockMsg("meshcore/DK/obs1/packet", `{"raw":"`+raw+`","origin":"TestObs"}`)
	handleMessage(r.store, "test", MQTTSource{Name: "test"}, msg, nil, nil, &Config{ValidateSignatures: &off})
	var n int
	r.store.db.QueryRow(`SELECT COUNT(*) FROM transmissions`).Scan(&n)
	if n != 1 {
		t.Fatalf("forged advert not stored with validation off (%d): the test would not reach the hook", n)
	}
	if row := r.row(claimA); row.status != antennaclaim.StatusPending {
		t.Fatalf("an unverified advert proved the claim: %+v", row)
	}
}

func TestAntennaClaim789_ProveGuardsInTheTable(t *testing.T) {
	// The table is the last guard, behind the in-memory map: a claim that
	// is no longer pending, or has expired, is never proven (again).
	r := newClaimRig(t, nil)
	r.claim(claimA, 18.5, claimCode)
	r.claim(claimB, 20, "BBBBBB")
	ctx := context.Background()
	now := r.clock.UnixMilli()
	if st, err := r.store.proveAntennaClaim(ctx, claimA, "h1", &claimLat, &claimLon, autoApproval{}, now); err != nil || st != antennaclaim.StatusProven {
		t.Fatalf("first proof: %q %v", st, err)
	}
	if st, err := r.store.proveAntennaClaim(ctx, claimA, "h2", &claimLat, &claimLon, autoApproval{}, now+1); err != nil || st != "" {
		t.Fatalf("second proof of a used code: %q %v", st, err)
	}
	if row := r.row(claimA); row.proofHash != "h1" || row.provenAt.Int64 != now {
		t.Fatalf("a used code proved again: %+v", row)
	}
	late := r.clock.Add(24*time.Hour + time.Second).UnixMilli()
	if st, err := r.store.proveAntennaClaim(ctx, claimB, "h3", &claimLat, &claimLon, autoApproval{}, late); err != nil || st != "" {
		t.Fatalf("expired proof: %q %v", st, err)
	}
	if row := r.row(claimB); row.status != antennaclaim.StatusPending {
		t.Fatalf("expired claim proven: %+v", row)
	}
}

// ── Review round 1 (F1–F4, F7) ────────────────────────────────────────────

// restart replaces the runner, as an ingestor restart does: the in-memory
// map starts empty and is loaded from the table.
func (r *claimRig) restart() {
	r.runner = newAntennaClaimRunner(r.store, nil)
	r.runner.now = func() time.Time { return r.clock }
	r.store.antennaClaims = r.runner
}

func (r *claimRig) statusCount(status string) int {
	r.t.Helper()
	var n int
	if err := r.store.db.QueryRow(`SELECT COUNT(*) FROM node_antenna_claims WHERE status = ?`, status).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

// F1: claims that expire while the ingestor is down must not stay pending
// (nor fill the node's cap) after a restart.
func TestAntennaClaim789_ExpiredWhileDown(t *testing.T) {
	t.Run("restart expires them", func(t *testing.T) {
		r := newClaimRig(t, nil)
		r.claim(claimA, 18.5, claimCode)
		r.claim(claimB, 20, "BBBBBB")
		r.claim(claimC, 30, "CCCCCC")
		r.clock = r.clock.Add(25 * time.Hour)
		r.restart()
		r.runner.RunOnce(context.Background())
		r.runner.RunOnce(context.Background())
		for _, id := range []string{claimA, claimB, claimC} {
			if row := r.row(id); row.status != antennaclaim.StatusExpired {
				t.Fatalf("claim %s after a restart past expiry: %+v", id, row)
			}
		}
		r.claim(claimD, 40, "DDDDDD")
		if res := r.result(claimD); res.Status != antennaclaim.StatusPending {
			t.Fatalf("fourth claim after the others expired: %+v", res)
		}
	})
	t.Run("cap ignores expired rows", func(t *testing.T) {
		// The fourth claim is already queued when the new runner first
		// runs: commands are applied before the expiry sweep, so the cap
		// itself must not count expired rows.
		r := newClaimRig(t, &antennaclaim.Config{MaxPending: 3})
		r.claim(claimA, 18.5, claimCode)
		r.claim(claimB, 20, "BBBBBB")
		r.claim(claimC, 30, "CCCCCC")
		r.clock = r.clock.Add(25 * time.Hour)
		r.restart()
		h := 40.0
		r.enqueue(antennaclaim.Command{RequestID: claimD, Op: antennaclaim.OpClaim, PubKey: r.pk, HeightM: &h, Code: "DDDDDD"})
		r.runner.RunOnce(context.Background())
		if res := r.result(claimD); res.Status != antennaclaim.StatusPending {
			t.Fatalf("fourth claim counted expired rows against the cap: %+v", res)
		}
		if n := r.statusCount(antennaclaim.StatusPending); n != 1 {
			t.Fatalf("%d pending, want only the new claim", n)
		}
	})
}

// F2: an administrator approving an older proof must not override a newer
// approved claim, in either order.
func TestAntennaClaim789_ApproveOlderProofRefused(t *testing.T) {
	t.Run("owner clear stays in force", func(t *testing.T) {
		r := newClaimRig(t, nil)
		r.claim(claimA, 18.5, claimCode)
		r.advert(r.pub, r.priv, "Roof "+claimCode, true) // A proven, waits for review
		r.clock = r.clock.Add(time.Minute)
		r.enqueue(antennaclaim.Command{RequestID: claimB, Op: antennaclaim.OpClaim, PubKey: r.pk, Clear: true, Code: "BBBBBB"})
		r.runner.RunOnce(context.Background())
		r.advert(r.pub, r.priv, "Roof BBBBBB", false) // B: the owner's clear, approved at once
		r.clock = r.clock.Add(time.Minute)
		res := r.decide("e000000000000001", antennaclaim.OpApprove, claimA)
		if res.Status != antennaclaim.RequestError || res.Error != antennaclaim.ErrNewerApproved.Error() {
			t.Fatalf("approving the older proof: %+v", res)
		}
		if row := r.row(claimA); row.status != antennaclaim.StatusProven {
			t.Fatalf("older proof: %+v", row)
		}
		if row := r.row(claimB); row.status != antennaclaim.StatusApproved || row.approvedBy != antennaclaim.ApprovedByOwner {
			t.Fatalf("owner clear overridden: %+v", row)
		}
	})
	t.Run("newer height approved first", func(t *testing.T) {
		r := newClaimRig(t, nil)
		r.claim(claimA, 18.5, claimCode)
		r.advert(r.pub, r.priv, "Roof "+claimCode, true)
		r.clock = r.clock.Add(time.Minute)
		r.claim(claimB, 25, "BBBBBB")
		r.advert(r.pub, r.priv, "Roof BBBBBB", true)
		if res := r.decide("e000000000000001", antennaclaim.OpApprove, claimB); res.Status != antennaclaim.StatusApproved {
			t.Fatalf("approve newer: %+v", res)
		}
		if res := r.decide("e000000000000002", antennaclaim.OpApprove, claimA); res.Status != antennaclaim.RequestError || res.Error != antennaclaim.ErrNewerApproved.Error() {
			t.Fatalf("approving the older proof after the newer: %+v", res)
		}
		if row := r.row(claimB); row.status != antennaclaim.StatusApproved {
			t.Fatalf("newer claim overridden: %+v", row)
		}
	})
	t.Run("older height approved first", func(t *testing.T) {
		// The right order still works: the newer approval supersedes.
		r := newClaimRig(t, nil)
		r.claim(claimA, 18.5, claimCode)
		r.advert(r.pub, r.priv, "Roof "+claimCode, true)
		r.clock = r.clock.Add(time.Minute)
		r.claim(claimB, 25, "BBBBBB")
		r.advert(r.pub, r.priv, "Roof BBBBBB", true)
		r.decide("e000000000000001", antennaclaim.OpApprove, claimA)
		if res := r.decide("e000000000000002", antennaclaim.OpApprove, claimB); res.Status != antennaclaim.StatusApproved {
			t.Fatalf("approve newer: %+v", res)
		}
		if row := r.row(claimA); row.status != antennaclaim.StatusSuperseded {
			t.Fatalf("older claim: %+v", row)
		}
	})
}

// F3: the ingestor re-validates every queue file with the shared rules,
// including commands the table itself would accept.
func TestAntennaClaim789_QueueFileRevalidated(t *testing.T) {
	h := 18.5
	cases := []struct {
		name string
		cmd  antennaclaim.Command
	}{
		{"height and clear", antennaclaim.Command{Op: antennaclaim.OpClaim, HeightM: &h, Clear: true, Code: claimCode}},
		{"neither height nor clear", antennaclaim.Command{Op: antennaclaim.OpClaim, Code: claimCode}},
		{"invalid code", antennaclaim.Command{Op: antennaclaim.OpClaim, HeightM: &h, Code: "ILOU12"}},
		{"upper-case pubkey", antennaclaim.Command{Op: antennaclaim.OpClaim, HeightM: &h, Code: claimCode, PubKey: "UPPER"}},
		{"height off the 0.1 m grid", antennaclaim.Command{Op: antennaclaim.OpClaim, HeightM: func() *float64 { v := 18.55; return &v }(), Code: claimCode}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newClaimRig(t, nil)
			cmd := c.cmd
			cmd.RequestID = claimA
			switch cmd.PubKey {
			case "":
				cmd.PubKey = r.pk
			case "UPPER":
				cmd.PubKey = strings.ToUpper(r.pk)
			}
			r.enqueue(cmd)
			r.runner.RunOnce(context.Background())
			if res := r.result(claimA); res.Status != antennaclaim.RequestError || res.Error != antennaclaim.ErrInvalidCommand.Error() {
				t.Fatalf("accepted: %+v", res)
			}
			var n int
			r.store.db.QueryRow(`SELECT COUNT(*) FROM node_antenna_claims`).Scan(&n)
			if n != 0 {
				t.Fatal("row inserted")
			}
		})
	}
}

// F4: a different key that shares the first byte(s) of the claimed key
// proves nothing; only the full public key binds.
func TestAntennaClaim789_PrefixCollisionProvesNothing(t *testing.T) {
	r := newClaimRig(t, nil)
	var otherPub ed25519.PublicKey
	var otherPriv ed25519.PrivateKey
	for s := 2; s < 1<<16; s++ {
		seed := sha256.Sum256([]byte{'p', 'r', 'e', 'f', 'i', 'x', byte(s), byte(s >> 8)})
		priv := ed25519.NewKeyFromSeed(seed[:])
		pub := priv.Public().(ed25519.PublicKey)
		if pub[0] == r.pub[0] && !pub.Equal(r.pub) {
			otherPub, otherPriv = pub, priv
			break
		}
	}
	if otherPub == nil {
		t.Fatal("no colliding test key found")
	}
	r.claim(claimA, 18.5, claimCode)
	r.advert(otherPub, otherPriv, "Roof "+claimCode, true)
	if row := r.row(claimA); row.status != antennaclaim.StatusPending {
		t.Fatalf("a key sharing the first byte proved the claim: %+v", row)
	}
	r.advert(r.pub, r.priv, "Roof "+claimCode, true)
	if row := r.row(claimA); row.status != antennaclaim.StatusProven {
		t.Fatalf("genuine advert after the collision: %+v", row)
	}
}

// F7: when recording a proof fails, the code stays matchable, so a re-sent
// advert still proves the claim.
func TestAntennaClaim789_ProveFailureKeepsCode(t *testing.T) {
	r := newClaimRig(t, nil)
	r.claim(claimA, 18.5, claimCode)
	// Inject a write failure into the proof transaction only.
	if _, err := r.store.db.Exec(`CREATE TRIGGER fail_prove BEFORE UPDATE OF status ON node_antenna_claims
		WHEN NEW.status != 'pending' BEGIN SELECT RAISE(ABORT, 'injected write failure'); END`); err != nil {
		t.Fatal(err)
	}
	r.advert(r.pub, r.priv, "Roof "+claimCode, true)
	if row := r.row(claimA); row.status != antennaclaim.StatusPending {
		t.Fatalf("the injected failure did not stop the proof: %+v", row)
	}
	if n := r.runner.pendingCount(); n != 1 {
		t.Fatalf("code dropped from memory after a failed proof: %d pending", n)
	}
	if _, err := r.store.db.Exec(`DROP TRIGGER fail_prove`); err != nil {
		t.Fatal(err)
	}
	r.advert(r.pub, r.priv, "Roof "+claimCode, true) // re-sent, no runner tick in between
	if row := r.row(claimA); row.status != antennaclaim.StatusProven {
		t.Fatalf("re-sent advert after a failed proof: %+v", row)
	}
	if n := r.runner.pendingCount(); n != 0 {
		t.Fatalf("code not consumed after the proof: %d pending", n)
	}
}
