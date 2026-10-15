package main

import (
	"context"
	"crypto/aes"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/regions"
)

// Issue #380: historical enc_<HEX> GRP_TXT rows and unknown-scoped transport
// rows are re-decoded when the effective key set grows.

const decodeBFChannel = "#bf-test"

// decodeBFSettings is a fast, fully-deterministic settings value.
func decodeBFSettings(batch int, window int64) DecodeBackfillSettings {
	return DecodeBackfillSettings{Enabled: true, BatchSize: batch, IDWindow: window, Pause: 0, Poll: time.Hour}
}

func decodeBFStore(t *testing.T, name string) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	s.backfillWg.Wait()
	t.Cleanup(func() { s.Close() })
	if err := s.UpsertObserver("obs-bf", "obs-bf", "TST", nil); err != nil {
		t.Fatal(err)
	}
	return s
}

// decodeBFGrpTxt builds a FLOOD-routed GRP_TXT frame whose payload is
// encrypted for channelKey (hex) and carries senderMsg. The frame layout is
// header(1) + path_len(1) + path + payload (no transport codes on FLOOD),
// and payload[0] is the one-byte channel hash ahead of MAC(2)+ciphertext.
func decodeBFGrpTxt(t testing.TB, channelKeyHex, senderMsg string, chanHashByte byte) string {
	t.Helper()
	key, err := hex.DecodeString(channelKeyHex)
	if err != nil || len(key) != 16 {
		t.Fatalf("bad channel key %q: %v", channelKeyHex, err)
	}
	plain := make([]byte, 4+1+len(senderMsg))
	binary.LittleEndian.PutUint32(plain[0:4], 1700000000)
	plain[4] = 0x00
	copy(plain[5:], senderMsg)
	if pad := aes.BlockSize - (len(plain) % aes.BlockSize); pad != aes.BlockSize {
		plain = append(plain, make([]byte, pad)...)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	ct := make([]byte, len(plain))
	for i := 0; i < len(plain); i += aes.BlockSize {
		block.Encrypt(ct[i:i+aes.BlockSize], plain[i:i+aes.BlockSize])
	}
	secret := make([]byte, 32)
	copy(secret, key)
	h := hmac.New(sha256.New, secret)
	h.Write(ct)
	mac := h.Sum(nil)[:2]

	payload := append([]byte{chanHashByte}, mac...)
	payload = append(payload, ct...)
	// header: route_type 1 (FLOOD) | payload_type 5 (GRP_TXT) << 2, then
	// path_len 0x01 (hash_size (0x01>>6)+1 = 1, hash_count 0x01&63 = 1) and
	// its single one-byte path hash, so the payload starts at byte 3.
	frame := []byte{0x01 | (PayloadGRP_TXT << 2), 0x01, 0xAB}
	frame = append(frame, payload...)
	return strings.ToUpper(hex.EncodeToString(frame))
}

// decodeBFTransportFrame builds a TRANSPORT_FLOOD frame (route type 0, so
// transport codes are present) whose code1 is the HMAC of its own payload
// under scope's region key, i.e. a frame that genuinely belongs to scope.
// payloadTail varies the content so two frames get different content hashes.
func decodeBFTransportFrame(t testing.TB, scope string, payloadTail []byte) string {
	t.Helper()
	payload := append([]byte{0x11, 0x22, 0x33, 0x44}, payloadTail...)
	code1 := decodeBFCode1(regions.Key(scope), byte(PayloadRAW_CUSTOM), payload)
	frame := []byte{0x00 | (PayloadRAW_CUSTOM << 2)}
	frame = append(frame, code1[0], code1[1], 0x00, 0x00) // code1 then code2
	frame = append(frame, 0x00)                           // path_len: no hops
	frame = append(frame, payload...)
	return strings.ToUpper(hex.EncodeToString(frame))
}

// decodeBFCode1 mirrors the firmware transport-code derivation matchScope
// verifies against.
func decodeBFCode1(key []byte, payloadType byte, payload []byte) [2]byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte{payloadType})
	mac.Write(payload)
	sum := mac.Sum(nil)
	code := uint16(sum[0]) | uint16(sum[1])<<8
	if code == 0 {
		code = 1
	} else if code == 0xFFFF {
		code = 0xFFFE
	}
	return [2]byte{byte(code & 0xFF), byte(code >> 8)}
}

// decodeBFIngest ingests raw through the normal path with the given keys and
// returns the transmission id.
func decodeBFIngest(t *testing.T, s *Store, raw string, channelKeys map[string]string, regionKeys map[string][]byte) int64 {
	t.Helper()
	decoded, err := DecodePacket(raw, channelKeys, false)
	if err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	pd := BuildPacketData(&MQTTPacketMessage{Raw: raw}, decoded, "obs-bf", "", regionKeys)
	inserted, err := s.InsertTransmission(pd)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatalf("packet %s was a duplicate; each test frame must be unique", raw)
	}
	var id int64
	if err := s.db.QueryRow(`SELECT id FROM transmissions WHERE hash = ?`, pd.Hash).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

type decodeBFRow struct {
	channelHash sql.NullString
	decodedJSON sql.NullString
	scopeName   sql.NullString
}

func decodeBFReadRow(t *testing.T, s *Store, id int64) decodeBFRow {
	t.Helper()
	var r decodeBFRow
	if err := s.db.QueryRow(`SELECT channel_hash, decoded_json, scope_name FROM transmissions WHERE id = ?`, id).
		Scan(&r.channelHash, &r.decodedJSON, &r.scopeName); err != nil {
		t.Fatal(err)
	}
	return r
}

func decodeBFChannelKeys(names ...string) map[string]string {
	out := map[string]string{}
	for _, n := range names {
		out[n] = deriveHashtagChannelKey(n)
	}
	return out
}

func decodeBFRegionKeys(names ...string) map[string][]byte {
	out := map[string][]byte{}
	for _, n := range names {
		out[n] = regions.Key(n)
	}
	return out
}

// ---------------------------------------------------------------------------
// Point 1: the trigger is effective key-set growth
// ---------------------------------------------------------------------------

func TestDecodeBackfill_ChannelKeyAdditionResolvesHistory(t *testing.T) {
	s := decodeBFStore(t, "chan.db")
	key := deriveHashtagChannelKey(decodeBFChannel)
	raw := decodeBFGrpTxt(t, key, "Alice: historic", 0x3A)
	// Ingested with no key at all: the row keeps the enc_<HEX> placeholder.
	id := decodeBFIngest(t, s, raw, nil, nil)
	if got := decodeBFReadRow(t, s, id); got.channelHash.String != "enc_3A" {
		t.Fatalf("precondition: channel_hash = %q, want enc_3A", got.channelHash.String)
	}

	hk := newHotKeys(decodeBFChannelKeys(decodeBFChannel), nil)
	res, err := s.RunDecodeBackfill(context.Background(), hk, decodeBFSettings(10, 1000))
	if err != nil {
		t.Fatal(err)
	}
	if res.Kinds != "channel" {
		t.Errorf("kinds = %q, want channel (only the channel set grew)", res.Kinds)
	}
	if res.ChannelUpdated != 1 {
		t.Errorf("channelUpdated = %d, want 1", res.ChannelUpdated)
	}
	got := decodeBFReadRow(t, s, id)
	if got.channelHash.String != decodeBFChannel {
		t.Errorf("channel_hash = %q, want %q", got.channelHash.String, decodeBFChannel)
	}
	if !strings.Contains(got.decodedJSON.String, `"decryptionStatus":"decrypted"`) {
		t.Errorf("decoded_json not rewritten: %s", got.decodedJSON.String)
	}
	if !strings.Contains(got.decodedJSON.String, "Alice: historic") {
		t.Errorf("decoded_json lacks the plaintext: %s", got.decodedJSON.String)
	}
	if s.Stats.SnapshotBackfills()["decode_channel"] != 1 {
		t.Errorf("backfill counter = %v, want decode_channel=1", s.Stats.SnapshotBackfills())
	}
}

func TestDecodeBackfill_RegionKeyAdditionResolvesHistory(t *testing.T) {
	s := decodeBFStore(t, "scope.db")
	const scope = "bf-region"
	raw := decodeBFTransportFrame(t, scope, []byte{0x01})
	// Ingested with no region keys: transport-scoped, region unknown ('').
	id := decodeBFIngest(t, s, raw, nil, nil)
	pre := decodeBFReadRow(t, s, id)
	if !pre.scopeName.Valid || pre.scopeName.String != "" {
		t.Fatalf("precondition: scope_name = %#v, want a valid empty string", pre.scopeName)
	}

	hk := newHotKeys(nil, decodeBFRegionKeys(scope))
	res, err := s.RunDecodeBackfill(context.Background(), hk, decodeBFSettings(10, 1000))
	if err != nil {
		t.Fatal(err)
	}
	if res.Kinds != "scope" {
		t.Errorf("kinds = %q, want scope", res.Kinds)
	}
	if res.ScopeUpdated != 1 {
		t.Errorf("scopeUpdated = %d, want 1", res.ScopeUpdated)
	}
	if got := decodeBFReadRow(t, s, id); got.scopeName.String != scope {
		t.Errorf("scope_name = %q, want %q", got.scopeName.String, scope)
	}
}

func TestDecodeBackfill_NoRunWithoutKeySetGrowth(t *testing.T) {
	s := decodeBFStore(t, "nogrowth.db")
	hk := newHotKeys(decodeBFChannelKeys(decodeBFChannel), decodeBFRegionKeys("bf-region"))
	set := decodeBFSettings(10, 1000)
	if _, err := s.RunDecodeBackfill(context.Background(), hk, set); err != nil {
		t.Fatal(err)
	}
	// Second call: the same key set is covered, so nothing is armed.
	res, err := s.RunDecodeBackfill(context.Background(), hk, set)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kinds != "" || res.Batches != 0 {
		t.Errorf("second run did work: %+v", res)
	}
	// A removed key must not arm a run either: resolved rows are never revisited.
	shrunk := newHotKeys(nil, nil)
	res, err = s.RunDecodeBackfill(context.Background(), shrunk, set)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kinds != "" || res.Batches != 0 {
		t.Errorf("a shrinking key set armed a run: %+v", res)
	}
}

// ---------------------------------------------------------------------------
// Point 2: already-resolved rows are never touched
// ---------------------------------------------------------------------------

func TestDecodeBackfill_LeavesResolvedAndUnknownRowsAlone(t *testing.T) {
	s := decodeBFStore(t, "untouched.db")
	keys := decodeBFChannelKeys(decodeBFChannel)
	// Already decrypted at ingest.
	resolvedID := decodeBFIngest(t, s, decodeBFGrpTxt(t, keys[decodeBFChannel], "Bob: live", 0x3A), keys, nil)
	// Encrypted with a key nobody has.
	unknownID := decodeBFIngest(t, s, decodeBFGrpTxt(t, "00112233445566778899aabbccddeeff", "Eve: secret", 0x7C), nil, nil)
	before := decodeBFReadRow(t, s, resolvedID)

	hk := newHotKeys(keys, nil)
	res, err := s.RunDecodeBackfill(context.Background(), hk, decodeBFSettings(10, 1000))
	if err != nil {
		t.Fatal(err)
	}
	if res.ChannelUpdated != 0 {
		t.Errorf("channelUpdated = %d, want 0 (nothing was resolvable)", res.ChannelUpdated)
	}
	after := decodeBFReadRow(t, s, resolvedID)
	if after.channelHash.String != before.channelHash.String || after.decodedJSON.String != before.decodedJSON.String {
		t.Errorf("resolved row changed:\n before %+v\n after  %+v", before, after)
	}
	if got := decodeBFReadRow(t, s, unknownID); got.channelHash.String != "enc_7C" {
		t.Errorf("unknown row channel_hash = %q, want enc_7C", got.channelHash.String)
	}
}

// ---------------------------------------------------------------------------
// Point 3: idempotence
// ---------------------------------------------------------------------------

func TestDecodeBackfill_IdempotentAcrossRepeatedRuns(t *testing.T) {
	s := decodeBFStore(t, "idem.db")
	keys := decodeBFChannelKeys(decodeBFChannel)
	id := decodeBFIngest(t, s, decodeBFGrpTxt(t, keys[decodeBFChannel], "Alice: once", 0x3A), nil, nil)
	hk := newHotKeys(keys, nil)
	set := decodeBFSettings(10, 1000)
	if _, err := s.RunDecodeBackfill(context.Background(), hk, set); err != nil {
		t.Fatal(err)
	}
	first := decodeBFReadRow(t, s, id)
	revAfterFirst := decodeBFRevision(t, s)

	// Force a second pass over the same rows by clearing the covered keys,
	// which is what a key rotation looks like to the watcher.
	if _, err := s.db.Exec(`DELETE FROM decode_backfill_keys`); err != nil {
		t.Fatal(err)
	}
	res, err := s.RunDecodeBackfill(context.Background(), hk, set)
	if err != nil {
		t.Fatal(err)
	}
	if res.ChannelUpdated != 0 {
		t.Errorf("second pass rewrote %d row(s), want 0", res.ChannelUpdated)
	}
	if second := decodeBFReadRow(t, s, id); second != first {
		t.Errorf("row changed on the second pass:\n first  %+v\n second %+v", first, second)
	}
	if rev := decodeBFRevision(t, s); rev != revAfterFirst {
		t.Errorf("revision moved without a rewrite: %d -> %d", revAfterFirst, rev)
	}
}

func decodeBFRevision(t *testing.T, s *Store) int64 {
	t.Helper()
	st, err := s.DecodeBackfillState()
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		return -1
	}
	return st.Revision
}

// ---------------------------------------------------------------------------
// Point 4: crash / cancel resume
// ---------------------------------------------------------------------------

func TestDecodeBackfill_ResumesAfterCancelMidRun(t *testing.T) {
	s := decodeBFStore(t, "resume.db")
	keys := decodeBFChannelKeys(decodeBFChannel)
	var ids []int64
	for i := 0; i < 6; i++ {
		raw := decodeBFGrpTxt(t, keys[decodeBFChannel], fmt.Sprintf("Alice: row %d", i), byte(0x30+i))
		ids = append(ids, decodeBFIngest(t, s, raw, nil, nil))
	}
	hk := newHotKeys(keys, nil)
	set := decodeBFSettings(2, 2)

	ctx, cancel := context.WithCancel(context.Background())
	batches := 0
	decodeBackfillBatchHook = func(decodeBackfillBatchResult) {
		batches++
		if batches == 1 {
			cancel() // the "crash": stop right after the first commit
		}
	}
	t.Cleanup(func() { decodeBackfillBatchHook = nil })
	if _, err := s.RunDecodeBackfill(ctx, hk, set); err != context.Canceled {
		t.Fatalf("cancelled run returned %v, want context.Canceled", err)
	}
	decodeBackfillBatchHook = nil
	if batches != 1 {
		t.Fatalf("%d batches ran after the cancel, want exactly 1", batches)
	}

	st, err := s.DecodeBackfillState()
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "running" {
		t.Fatalf("status = %q, want running so the next start resumes", st.Status)
	}
	if st.Watermark <= 0 || st.Watermark >= st.Ceiling {
		t.Fatalf("watermark = %d, ceiling = %d: want a partial watermark", st.Watermark, st.Ceiling)
	}
	// The covered keys must NOT be credited yet.
	covered, err := s.coveredKeyHashes(decodeBackfillKindChannel)
	if err != nil {
		t.Fatal(err)
	}
	if covered[decodeBackfillKeyHash(decodeBFChannel)] {
		t.Error("key credited as covered by a cancelled run")
	}

	// The resume picks the run up from its watermark and finishes it.
	res, err := s.RunDecodeBackfill(context.Background(), hk, set)
	if err != nil {
		t.Fatal(err)
	}
	if res.From != st.Watermark {
		t.Errorf("resumed from %d, want the persisted watermark %d", res.From, st.Watermark)
	}
	for _, id := range ids {
		if got := decodeBFReadRow(t, s, id); got.channelHash.String != decodeBFChannel {
			t.Errorf("id %d channel_hash = %q, want %q after resume", id, got.channelHash.String, decodeBFChannel)
		}
	}
	covered, err = s.coveredKeyHashes(decodeBackfillKindChannel)
	if err != nil {
		t.Fatal(err)
	}
	if !covered[decodeBackfillKeyHash(decodeBFChannel)] {
		t.Error("key not covered after the run reached its ceiling")
	}
}

// ---------------------------------------------------------------------------
// Point 5: a key added mid-run is not lost
// ---------------------------------------------------------------------------

func TestDecodeBackfill_KeyAddedMidRunIsNotCredited(t *testing.T) {
	s := decodeBFStore(t, "midrun.db")
	const first, second = "#bf-first", "#bf-second"
	k1, k2 := deriveHashtagChannelKey(first), deriveHashtagChannelKey(second)
	// Two rows: one for each channel, both stored without any key.
	id1 := decodeBFIngest(t, s, decodeBFGrpTxt(t, k1, "A: one", 0x31), nil, nil)
	id2 := decodeBFIngest(t, s, decodeBFGrpTxt(t, k2, "B: two", 0x32), nil, nil)

	hk := newHotKeys(decodeBFChannelKeys(first), nil)
	set := decodeBFSettings(1, 1)
	// Add the second key after the first batch committed, i.e. while the run
	// is still walking ids with its own snapshot.
	added := false
	decodeBackfillBatchHook = func(decodeBackfillBatchResult) {
		if !added {
			added = true
			hk.AddApproved(second)
		}
	}
	t.Cleanup(func() { decodeBackfillBatchHook = nil })
	if _, err := s.RunDecodeBackfill(context.Background(), hk, set); err != nil {
		t.Fatal(err)
	}
	decodeBackfillBatchHook = nil

	covered, err := s.coveredKeyHashes(decodeBackfillKindChannel)
	if err != nil {
		t.Fatal(err)
	}
	if !covered[decodeBackfillKeyHash(first)] {
		t.Error("the key the run started with is not covered")
	}
	if _, present := covered[decodeBackfillKeyHash(second)]; present {
		t.Error("a key added mid-run was recorded by that run; its history would be skipped")
	}

	// The next run arms for it and resolves the row that needed it.
	res, err := s.RunDecodeBackfill(context.Background(), hk, set)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kinds != "channel" {
		t.Fatalf("follow-up run kinds = %q, want channel", res.Kinds)
	}
	if got := decodeBFReadRow(t, s, id1); got.channelHash.String != first {
		t.Errorf("id %d channel_hash = %q, want %q", id1, got.channelHash.String, first)
	}
	if got := decodeBFReadRow(t, s, id2); got.channelHash.String != second {
		t.Errorf("id %d channel_hash = %q, want %q", id2, got.channelHash.String, second)
	}
}

// ---------------------------------------------------------------------------
// Point 6: ambiguous region matches abstain
// ---------------------------------------------------------------------------

func TestDecodeBackfill_AbstainsOnAmbiguousScope(t *testing.T) {
	s := decodeBFStore(t, "ambiguous.db")
	const real = "bf-real"
	raw := decodeBFTransportFrame(t, real, []byte{0x02})
	id := decodeBFIngest(t, s, raw, nil, nil)

	// A second region whose key is forged to produce the same code1 for this
	// payload: the 16-bit code cannot distinguish them, which is exactly the
	// birthday collision matchScope abstains on.
	decoded, err := DecodePacket(raw, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	collider := decodeBFColliderKey(t, byte(decoded.Header.PayloadType), decoded.payloadRaw, decoded.TransportCodes.Code1)
	rk := decodeBFRegionKeys(real)
	rk["bf-collider"] = collider

	hk := newHotKeys(nil, rk)
	res, err := s.RunDecodeBackfill(context.Background(), hk, decodeBFSettings(10, 1000))
	if err != nil {
		t.Fatal(err)
	}
	if res.Ambiguous != 1 {
		t.Errorf("ambiguous = %d, want 1", res.Ambiguous)
	}
	if res.ScopeUpdated != 0 {
		t.Errorf("scopeUpdated = %d, want 0: an ambiguous match must abstain", res.ScopeUpdated)
	}
	if got := decodeBFReadRow(t, s, id); got.scopeName.String != "" {
		t.Errorf("scope_name = %q, want the row left unknown-scoped", got.scopeName.String)
	}
	st, err := s.DecodeBackfillState()
	if err != nil {
		t.Fatal(err)
	}
	if st.Ambiguous != 1 {
		t.Errorf("persisted ambiguous = %d, want 1 (abstentions must be observable)", st.Ambiguous)
	}
}

// decodeBFColliderKey brute-forces a 32-byte region key whose HMAC over this
// payload produces want (a 4-hex-char code1). Two bytes of search space, so a
// hit is found in ~65k tries.
func decodeBFColliderKey(t *testing.T, payloadType byte, payload []byte, want string) []byte {
	t.Helper()
	key := make([]byte, 32)
	for i := 0; i < 1<<22; i++ {
		binary.LittleEndian.PutUint32(key[0:4], uint32(i))
		c := decodeBFCode1(key, payloadType, payload)
		if strings.ToUpper(hex.EncodeToString(c[:])) == want {
			out := make([]byte, 32)
			copy(out, key)
			return out
		}
	}
	t.Fatalf("no collider key found for code1 %s", want)
	return nil
}

// ---------------------------------------------------------------------------
// Point 7: current-vs-historical parity
// ---------------------------------------------------------------------------

func TestDecodeBackfill_ParityWithFreshIngest(t *testing.T) {
	// A mixed-case name on purpose: hashtag channel keys are derived from the
	// exact bytes of the name, so #BF-Parity and #bf-parity are different
	// channels and the backfill must store the decoder's spelling verbatim.
	const mixedCase = "#BF-Parity"
	keys := decodeBFChannelKeys(mixedCase)
	const scope = "bf-parity"
	rk := decodeBFRegionKeys(scope)
	chanRaw := decodeBFGrpTxt(t, keys[mixedCase], "Alice: parity", 0x5B)
	scopeRaw := decodeBFTransportFrame(t, scope, []byte{0x03})

	// Fresh: the keys are present at ingest.
	fresh := decodeBFStore(t, "fresh.db")
	freshChan := decodeBFIngest(t, fresh, chanRaw, keys, rk)
	freshScope := decodeBFIngest(t, fresh, scopeRaw, keys, rk)

	// Historical: the very same frames ingested with no keys, then backfilled.
	hist := decodeBFStore(t, "hist.db")
	histChan := decodeBFIngest(t, hist, chanRaw, nil, nil)
	histScope := decodeBFIngest(t, hist, scopeRaw, nil, nil)
	if _, err := hist.RunDecodeBackfill(context.Background(), newHotKeys(keys, rk), decodeBFSettings(10, 1000)); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name            string
		freshID, histID int64
	}{{"channel", freshChan, histChan}, {"scope", freshScope, histScope}} {
		want := decodeBFReadRow(t, fresh, c.freshID)
		got := decodeBFReadRow(t, hist, c.histID)
		if got != want {
			t.Errorf("%s row is not byte-identical to fresh ingest:\n fresh %+v\n back  %+v", c.name, want, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Point 8: bounds
// ---------------------------------------------------------------------------

func TestDecodeBackfill_BatchHonoursIDWindowAndBatchSize(t *testing.T) {
	s := decodeBFStore(t, "bounds.db")
	keys := decodeBFChannelKeys(decodeBFChannel)
	for i := 0; i < 8; i++ {
		decodeBFIngest(t, s, decodeBFGrpTxt(t, keys[decodeBFChannel], fmt.Sprintf("A: %d", i), byte(0x40+i)), nil, nil)
	}
	hk := newHotKeys(keys, nil)

	if err := ensureDecodeBackfillTables(s.db); err != nil {
		t.Fatal(err)
	}
	// A window of 3 ids with a batch limit of 100 reads at most 3 rows and
	// moves the watermark to the window end, not to the ceiling.
	run, err := s.startDecodeBackfillRun([]string{decodeBackfillKindChannel}, hk.Channels(), nil)
	if err != nil || run == nil {
		t.Fatalf("startDecodeBackfillRun: run=%v err=%v", run, err)
	}
	b, err := s.decodeBackfillBatch(context.Background(), run, decodeBFSettings(100, 3))
	if err != nil {
		t.Fatal(err)
	}
	if b.Scanned != 3 {
		t.Errorf("scanned = %d, want 3 (the id window bounds the batch)", b.Scanned)
	}
	if b.Next != 3 {
		t.Errorf("watermark = %d, want 3 (the window end)", b.Next)
	}
	if b.Done {
		t.Error("batch reported done before the ceiling")
	}

	// A batch limit below the window truncates and resumes from the last row
	// read, never from the window end.
	b, err = s.decodeBackfillBatch(context.Background(), run, decodeBFSettings(2, 100))
	if err != nil {
		t.Fatal(err)
	}
	if b.Scanned != 2 {
		t.Errorf("scanned = %d, want 2 (the batch size bounds the batch)", b.Scanned)
	}
	if b.Next != 5 {
		t.Errorf("watermark = %d, want 5 (the last row read, not the window end)", b.Next)
	}
}

func TestDecodeBackfillCandidateSQL_PerKind(t *testing.T) {
	channelOnly := decodeBackfillCandidateSQL(true, false)
	if !strings.Contains(channelOnly, "payload_type = 5") || !strings.Contains(channelOnly, "enc_%") {
		t.Errorf("channel predicate = %q", channelOnly)
	}
	if strings.Contains(channelOnly, "scope_name") {
		t.Errorf("channel-only predicate must not touch scope_name: %q", channelOnly)
	}
	scopeOnly := decodeBackfillCandidateSQL(false, true)
	if scopeOnly != `((scope_name = ''))` {
		t.Errorf("scope predicate = %q", scopeOnly)
	}
	both := decodeBackfillCandidateSQL(true, true)
	if !strings.Contains(both, " OR ") {
		t.Errorf("both-kinds predicate = %q", both)
	}
	if decodeBackfillCandidateSQL(false, false) != "" {
		t.Error("no armed kind must produce no predicate")
	}
}

// ---------------------------------------------------------------------------
// Point 9: settings
// ---------------------------------------------------------------------------

func TestDecodeBackfillSettings_DefaultsAndOverrides(t *testing.T) {
	var c Config
	def := c.DecodeBackfillSettings()
	if !def.Enabled || def.BatchSize != defaultDecodeBackfillBatchSize || def.IDWindow != defaultDecodeBackfillIDWindow ||
		def.Pause != defaultDecodeBackfillPause || def.Poll != defaultDecodeBackfillPoll {
		t.Errorf("defaults = %+v", def)
	}
	c.DecodeBackfill = &DecodeBackfillConfig{Disabled: true, BatchSize: 7, IDWindow: 9, PauseMs: 11, PollSecond: 13}
	got := c.DecodeBackfillSettings()
	if got.Enabled {
		t.Error("disabled:true must disable the pass")
	}
	if got.BatchSize != 7 || got.IDWindow != 9 || got.Pause != 11*time.Millisecond || got.Poll != 13*time.Second {
		t.Errorf("overrides = %+v", got)
	}
	// 0 or negative means the default: the pause cannot be switched off.
	c.DecodeBackfill = &DecodeBackfillConfig{BatchSize: -1, IDWindow: 0, PauseMs: -5, PollSecond: 0}
	got = c.DecodeBackfillSettings()
	if got.BatchSize != defaultDecodeBackfillBatchSize || got.IDWindow != defaultDecodeBackfillIDWindow ||
		got.Pause != defaultDecodeBackfillPause || got.Poll != defaultDecodeBackfillPoll {
		t.Errorf("non-positive overrides did not fall back: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Point 10: matchScopeDetail keeps matchScope's answer
// ---------------------------------------------------------------------------

func TestMatchScopeDetail_MatchesMatchScope(t *testing.T) {
	const scope = "bf-detail"
	raw := decodeBFTransportFrame(t, scope, []byte{0x04})
	decoded, err := DecodePacket(raw, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	pt, payload, code1 := byte(decoded.Header.PayloadType), decoded.payloadRaw, decoded.TransportCodes.Code1
	rk := decodeBFRegionKeys(scope)

	name, n := matchScopeDetail(rk, pt, payload, code1)
	if name != scope || n != 1 {
		t.Errorf("matchScopeDetail = (%q, %d), want (%q, 1)", name, n, scope)
	}
	if got := matchScope(rk, pt, payload, code1); got != name {
		t.Errorf("matchScope = %q, want %q", got, name)
	}

	rk["bf-detail-collider"] = decodeBFColliderKey(t, pt, payload, code1)
	_, n = matchScopeDetail(rk, pt, payload, code1)
	if n != 2 {
		t.Fatalf("matchScopeDetail count = %d, want 2", n)
	}
	if got := matchScope(rk, pt, payload, code1); got != "" {
		t.Errorf("matchScope = %q, want \"\" on an ambiguous match", got)
	}

	// No region, empty payload and the reserved code all answer "no match".
	if _, n := matchScopeDetail(nil, pt, payload, code1); n != 0 {
		t.Error("no region keys must report 0 matches")
	}
	if _, n := matchScopeDetail(rk, pt, nil, code1); n != 0 {
		t.Error("an empty payload must report 0 matches")
	}
	if _, n := matchScopeDetail(rk, pt, payload, "0000"); n != 0 {
		t.Error("code1 0000 must report 0 matches")
	}
}

// ---------------------------------------------------------------------------
// Point 11: undecodable rows do not stop the pass
// ---------------------------------------------------------------------------

func TestDecodeBackfill_UndecodableRowIsCountedAndSkipped(t *testing.T) {
	s := decodeBFStore(t, "baddecode.db")
	keys := decodeBFChannelKeys(decodeBFChannel)
	good := decodeBFIngest(t, s, decodeBFGrpTxt(t, keys[decodeBFChannel], "A: good", 0x3A), nil, nil)
	// A row whose stored frame no longer decodes (truncated beyond repair).
	if _, err := s.db.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, channel_hash)
		VALUES ('15', 'bf-broken', '2026-01-01T00:00:00Z', 1, 5, 'enc_FF')`); err != nil {
		t.Fatal(err)
	}
	res, err := s.RunDecodeBackfill(context.Background(), newHotKeys(keys, nil), decodeBFSettings(10, 1000))
	if err != nil {
		t.Fatal(err)
	}
	if res.DecodeErrors != 1 {
		t.Errorf("decodeErrors = %d, want 1", res.DecodeErrors)
	}
	if res.ChannelUpdated != 1 {
		t.Errorf("channelUpdated = %d, want 1: one bad frame must not stop the pass", res.ChannelUpdated)
	}
	if got := decodeBFReadRow(t, s, good); got.channelHash.String != decodeBFChannel {
		t.Errorf("good row channel_hash = %q", got.channelHash.String)
	}
}

// ---------------------------------------------------------------------------
// Point 12: the per-UPDATE guards close the decode-then-write window
// ---------------------------------------------------------------------------

// A row resolved by someone else (live ingest, or an earlier batch replayed
// after a crash) between the batch's decode and its write must keep the value
// that writer gave it. The candidate predicate cannot cover that window; the
// guards on the UPDATE statements do.
func TestDecodeBackfill_GuardsKeepConcurrentlyResolvedRows(t *testing.T) {
	s := decodeBFStore(t, "guard.db")
	keys := decodeBFChannelKeys(decodeBFChannel)
	const scope = "bf-guard"
	chanID := decodeBFIngest(t, s, decodeBFGrpTxt(t, keys[decodeBFChannel], "A: guard", 0x3A), nil, nil)
	scopeID := decodeBFIngest(t, s, decodeBFTransportFrame(t, scope, []byte{0x05}), nil, nil)

	if err := ensureDecodeBackfillTables(s.db); err != nil {
		t.Fatal(err)
	}
	run, err := s.startDecodeBackfillRun([]string{decodeBackfillKindChannel, decodeBackfillKindScope}, keys, decodeBFRegionKeys(scope))
	if err != nil || run == nil {
		t.Fatalf("startDecodeBackfillRun: run=%v err=%v", run, err)
	}
	decodeBackfillPreWriteHook = func() {
		// Another writer resolves both rows while this batch holds its
		// decoded values.
		if _, err := s.db.Exec(`UPDATE transmissions SET channel_hash = '#someone-else', decoded_json = '{"other":true}' WHERE id = ?`, chanID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE transmissions SET scope_name = 'someone-else' WHERE id = ?`, scopeID); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { decodeBackfillPreWriteHook = nil })
	b, err := s.decodeBackfillBatch(context.Background(), run, decodeBFSettings(10, 1000))
	if err != nil {
		t.Fatal(err)
	}
	decodeBackfillPreWriteHook = nil

	if b.ChannelUpdated != 0 || b.ScopeUpdated != 0 {
		t.Errorf("batch reported %d channel / %d scope rewrites, want 0: both rows were already resolved",
			b.ChannelUpdated, b.ScopeUpdated)
	}
	if got := decodeBFReadRow(t, s, chanID); got.channelHash.String != "#someone-else" || got.decodedJSON.String != `{"other":true}` {
		t.Errorf("channel row overwritten: %+v", got)
	}
	if got := decodeBFReadRow(t, s, scopeID); got.scopeName.String != "someone-else" {
		t.Errorf("scope row overwritten: %q", got.scopeName.String)
	}
}

// A resumed run must keep decoding with the key snapshot it started with, not
// with whatever is live when it resumes. Otherwise a key added after the
// watermark passed row X would be credited by a run that never looked at X.
func TestDecodeBackfill_ResumedRunKeepsItsOwnKeySnapshot(t *testing.T) {
	s := decodeBFStore(t, "snapshot.db")
	const first, second = "#bf-snap-a", "#bf-snap-b"
	early := decodeBFIngest(t, s, decodeBFGrpTxt(t, deriveHashtagChannelKey(first), "A: early", 0x31), nil, nil)
	late := decodeBFIngest(t, s, decodeBFGrpTxt(t, deriveHashtagChannelKey(second), "B: late", 0x32), nil, nil)

	hk := newHotKeys(decodeBFChannelKeys(first), nil)
	set := decodeBFSettings(1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	decodeBackfillBatchHook = func(decodeBackfillBatchResult) { cancel() }
	t.Cleanup(func() { decodeBackfillBatchHook = nil })
	if _, err := s.RunDecodeBackfill(ctx, hk, set); err != context.Canceled {
		t.Fatalf("cancelled run returned %v, want context.Canceled", err)
	}
	decodeBackfillBatchHook = nil

	// The second key appears while the run is parked after its first batch.
	if n := hk.AddApproved(second); n != 1 {
		t.Fatalf("AddApproved added %d keys, want 1", n)
	}
	res, err := s.RunDecodeBackfill(context.Background(), hk, set)
	if err != nil {
		t.Fatal(err)
	}
	if res.From == 0 {
		t.Fatalf("the resume restarted from 0 instead of the watermark: %+v", res)
	}
	if got := decodeBFReadRow(t, s, early); got.channelHash.String != first {
		t.Errorf("id %d channel_hash = %q, want %q", early, got.channelHash.String, first)
	}
	if got := decodeBFReadRow(t, s, late); got.channelHash.String != "enc_32" {
		t.Errorf("id %d channel_hash = %q: the resumed run used a key it never recorded, so the rows below its watermark would never be scanned with it",
			late, got.channelHash.String)
	}

	// The next run is armed for the new key and starts over from id 0.
	res, err = s.RunDecodeBackfill(context.Background(), hk, set)
	if err != nil {
		t.Fatal(err)
	}
	if res.From != 0 {
		t.Errorf("the follow-up run started at %d, want 0 so rows below the old watermark are scanned with the new key", res.From)
	}
	if got := decodeBFReadRow(t, s, late); got.channelHash.String != second {
		t.Errorf("id %d channel_hash = %q, want %q after the follow-up run", late, got.channelHash.String, second)
	}
}

// ---------------------------------------------------------------------------
// Point 13: a key that was covered, lost and added again is scanned again
// ---------------------------------------------------------------------------

// Rows ingested while a key was revoked could not be resolved then, and they
// sit above the watermark of the run that credited the key. Re-approving must
// therefore arm a fresh pass, which only happens if losing the key forgets it.
func TestDecodeBackfill_ReaddedKeyIsScannedAgain(t *testing.T) {
	s := decodeBFStore(t, "readd.db")
	keys := decodeBFChannelKeys(decodeBFChannel)
	early := decodeBFIngest(t, s, decodeBFGrpTxt(t, keys[decodeBFChannel], "A: before", 0x31), nil, nil)
	set := decodeBFSettings(10, 1000)

	if _, err := s.RunDecodeBackfill(context.Background(), newHotKeys(keys, nil), set); err != nil {
		t.Fatal(err)
	}
	if got := decodeBFReadRow(t, s, early); got.channelHash.String != decodeBFChannel {
		t.Fatalf("precondition: id %d channel_hash = %q", early, got.channelHash.String)
	}

	// Revoked: the key leaves the effective set. Nothing is rewritten, but the
	// bookkeeping forgets it.
	if _, err := s.RunDecodeBackfill(context.Background(), newHotKeys(nil, nil), set); err != nil {
		t.Fatal(err)
	}
	covered, err := s.coveredKeyHashes(decodeBackfillKindChannel)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := covered[decodeBackfillKeyHash(decodeBFChannel)]; present {
		t.Error("a key that is no longer effective is still recorded")
	}
	if got := decodeBFReadRow(t, s, early); got.channelHash.String != decodeBFChannel {
		t.Errorf("losing a key rewrote a resolved row: %q", got.channelHash.String)
	}

	// A row arrives while the key is gone, then the key comes back.
	late := decodeBFIngest(t, s, decodeBFGrpTxt(t, keys[decodeBFChannel], "A: during the gap", 0x32), nil, nil)
	res, err := s.RunDecodeBackfill(context.Background(), newHotKeys(keys, nil), set)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kinds != "channel" {
		t.Fatalf("re-adding the key armed %q, want channel", res.Kinds)
	}
	if got := decodeBFReadRow(t, s, late); got.channelHash.String != decodeBFChannel {
		t.Errorf("id %d channel_hash = %q: the row ingested while the key was gone was never re-scanned", late, got.channelHash.String)
	}
}

// A revocation reaches a run that is resumed afterwards: the resumed run
// stops using the revoked key for the rest of the table, and a later
// re-approval starts a fresh pass over the whole table instead.
func TestDecodeBackfill_ResumeHonoursARevocation(t *testing.T) {
	s := decodeBFStore(t, "revokeresume.db")
	keys := decodeBFChannelKeys(decodeBFChannel)
	early := decodeBFIngest(t, s, decodeBFGrpTxt(t, keys[decodeBFChannel], "A: early", 0x31), nil, nil)
	late := decodeBFIngest(t, s, decodeBFGrpTxt(t, keys[decodeBFChannel], "A: late", 0x32), nil, nil)
	set := decodeBFSettings(1, 1)

	ctx, cancel := context.WithCancel(context.Background())
	decodeBackfillBatchHook = func(decodeBackfillBatchResult) { cancel() }
	t.Cleanup(func() { decodeBackfillBatchHook = nil })
	if _, err := s.RunDecodeBackfill(ctx, newHotKeys(keys, nil), set); err != context.Canceled {
		t.Fatalf("cancelled run returned %v, want context.Canceled", err)
	}
	decodeBackfillBatchHook = nil
	if got := decodeBFReadRow(t, s, early); got.channelHash.String != decodeBFChannel {
		t.Fatalf("precondition: id %d channel_hash = %q", early, got.channelHash.String)
	}

	// The key is revoked, then the unfinished run resumes.
	if _, err := s.RunDecodeBackfill(context.Background(), newHotKeys(nil, nil), set); err != nil {
		t.Fatal(err)
	}
	if got := decodeBFReadRow(t, s, late); got.channelHash.String != "enc_32" {
		t.Errorf("id %d channel_hash = %q: the resumed run kept using a revoked key", late, got.channelHash.String)
	}

	// Re-approved: a fresh pass covers the whole table, including the row the
	// resumed run walked past.
	if _, err := s.RunDecodeBackfill(context.Background(), newHotKeys(keys, nil), set); err != nil {
		t.Fatal(err)
	}
	if got := decodeBFReadRow(t, s, late); got.channelHash.String != decodeBFChannel {
		t.Errorf("id %d channel_hash = %q after re-approval, want %q", late, got.channelHash.String, decodeBFChannel)
	}
}

// A run whose every key was revoked while it was parked cannot resolve
// anything, so it is closed without scanning the table.
func TestDecodeBackfill_ResumeWithNoKeysLeftScansNothing(t *testing.T) {
	s := decodeBFStore(t, "nokeys.db")
	keys := decodeBFChannelKeys(decodeBFChannel)
	for i := 0; i < 4; i++ {
		decodeBFIngest(t, s, decodeBFGrpTxt(t, keys[decodeBFChannel], fmt.Sprintf("A: %d", i), byte(0x30+i)), nil, nil)
	}
	set := decodeBFSettings(1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	decodeBackfillBatchHook = func(decodeBackfillBatchResult) { cancel() }
	t.Cleanup(func() { decodeBackfillBatchHook = nil })
	if _, err := s.RunDecodeBackfill(ctx, newHotKeys(keys, nil), set); err != context.Canceled {
		t.Fatalf("cancelled run returned %v, want context.Canceled", err)
	}
	decodeBackfillBatchHook = nil

	res, err := s.RunDecodeBackfill(context.Background(), newHotKeys(nil, nil), set)
	if err != nil {
		t.Fatal(err)
	}
	if res.Batches != 0 || res.Scanned != 0 {
		t.Errorf("a run with no effective key scanned the table: %+v", res)
	}
	st, err := s.DecodeBackfillState()
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "done" {
		t.Errorf("status = %q, want done so the run is not retried on every poll", st.Status)
	}
}
