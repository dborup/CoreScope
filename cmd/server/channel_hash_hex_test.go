package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// ─── Helper contract ───────────────────────────────────────────────────────────

func TestNormalizeChannelHashHex(t *testing.T) {
	cases := []struct {
		name   string
		in     interface{}
		want   string
		wantOK bool
	}{
		{"zero byte preserved", "00", "00", true},
		{"low byte", "03", "03", true},
		{"mid byte", "A7", "A7", true},
		{"high byte preserved", "FF", "FF", true},
		{"lowercase normalized", "a7", "A7", true},
		{"lowercase ff normalized", "ff", "FF", true},
		{"mixed case normalized", "aF", "AF", true},
		{"missing key", nil, "", false},
		{"empty legacy value", "", "", false},
		{"one character", "3", "", false},
		{"three characters", "0A7", "", false},
		{"non-hex", "GG", "", false},
		{"non-hex second char", "0Z", "", false},
		{"leading space", " 3", "", false},
		{"enc_ routing label rejected", "enc_A7", "", false},
		{"numeric channelHash rejected", float64(3), "", false},
		{"full sha digest rejected", strings.Repeat("AB", 32), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := normalizeChannelHashHex(tc.in)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("normalizeChannelHashHex(%#v) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestSetChannelHashHexLeavesKeyAbsentWhenUnavailable(t *testing.T) {
	for _, bad := range []interface{}{nil, "", "0", "GG", "enc_A7", float64(0)} {
		data := map[string]interface{}{}
		setChannelHashHex(data, bad)
		if _, present := data[channelHashHexKey]; present {
			t.Errorf("source %#v: key must be absent, got %#v", bad, data[channelHashHexKey])
		}
	}
	data := map[string]interface{}{}
	setChannelHashHex(data, "00")
	if data[channelHashHexKey] != "00" {
		t.Errorf("valid 00 must be set, got %#v", data[channelHashHexKey])
	}
}

// ─── Canonical output format ───────────────────────────────────────────────────

// The wire byte is the sole source of truth. The ingestor decoder produces the
// decoded_json this API reads and is covered by its own contract tests
// (cmd/ingestor/channel_hash_hex_decoder_test.go); here we lock that the
// canonical two-character uppercase forms that decoder emits are exactly what
// this package accepts and re-emits unchanged.
//
// Note: cmd/server's own decodeGrpTxt does not populate ChannelHashHex, but it
// never feeds this surface — it has no channel keys, so its GRP_TXT payloads
// never become CHAN, and handlePostPacket inserts them with a NULL
// channel_hash, which both channel-message paths exclude.
func TestChannelHashHexCanonicalFormsRoundTrip(t *testing.T) {
	for _, b := range []byte{0x00, 0x03, 0xA7, 0xFF} {
		want := fmt.Sprintf("%02X", b)
		got, ok := normalizeChannelHashHex(want)
		if !ok || got != want {
			t.Errorf("wire byte 0x%02X: canonical form %q not round-tripped: (%q,%v)", b, want, got, ok)
		}
		if len(got) != 2 {
			t.Errorf("wire byte 0x%02X: length %d, want exactly 2", b, len(got))
		}
	}
}

// ─── Shared fixture plumbing ───────────────────────────────────────────────────

// insertChanTx inserts one decrypted CHAN transmission with the supplied
// decoded_json plus a single observation, and returns its transmission id.
func insertChanTx(t *testing.T, db *DB, rawHex, hash, decodedJSON string) int64 {
	t.Helper()
	now := time.Now().UTC()
	recent := now.Add(-1 * time.Hour).Format(time.RFC3339)
	epoch := now.Add(-1 * time.Hour).Unix()

	res, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		VALUES (?, ?, ?, 1, 5, ?, ?)`, rawHex, hash, recent, decodedJSON, "#chx")
	if err != nil {
		t.Fatalf("insert transmission: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	if _, err := db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp)
		VALUES (?, 1, 10.0, -90, '[]', ?)`, id, epoch); err != nil {
		t.Fatalf("insert observation: %v", err)
	}
	return id
}

func chanJSON(extra string) string {
	return `{"type":"CHAN","channel":"#chx","text":"Alice: hello","sender":"Alice","sender_timestamp":1774643285` + extra + `}`
}

// findMsg returns the message whose packetHash matches, so tests are not
// coupled to page ordering.
func findMsg(t *testing.T, msgs []map[string]interface{}, packetHash string) map[string]interface{} {
	t.Helper()
	for _, m := range msgs {
		if h, _ := m["packetHash"].(string); h == packetHash {
			return m
		}
	}
	t.Fatalf("message with packetHash %q not found in %d messages", packetHash, len(msgs))
	return nil
}

// hashHexMatrix is the shared source/expectation matrix for both query paths.
var hashHexMatrix = []struct {
	name        string
	rawHex      string
	pktHash     string
	extra       string
	want        string
	wantPresent bool
}{
	{"zero byte preserved", "C000", "chx_hash_00", `,"channelHashHex":"00"`, "00", true},
	{"low byte", "C003", "chx_hash_03", `,"channelHashHex":"03"`, "03", true},
	{"mid byte", "C0A7", "chx_hash_a7", `,"channelHashHex":"A7"`, "A7", true},
	{"high byte preserved", "C0FF", "chx_hash_ff", `,"channelHashHex":"FF"`, "FF", true},
	{"lowercase legacy normalized", "C0LC", "chx_hash_lc", `,"channelHashHex":"a7"`, "A7", true},
	{"legacy row without field is absent", "C0NO", "chx_hash_none", ``, "", false},
	{"empty legacy value is absent", "C0MT", "chx_hash_empty", `,"channelHashHex":""`, "", false},
	{"malformed value is not guessed", "C0BAD", "chx_hash_bad", `,"channelHashHex":"ZZ"`, "", false},
	{"truncated value is not padded", "C0TR", "chx_hash_trunc", `,"channelHashHex":"7"`, "", false},
	{"numeric-only channelHash is not used", "C0NUM", "chx_hash_num", `,"channelHash":167`, "", false},
	{"enc_ label is never emitted", "C0ENC", "chx_hash_enc", `,"channelHashHex":"enc_A7"`, "", false},
}

func assertMatrixRow(t *testing.T, m map[string]interface{}, want string, wantPresent bool) {
	t.Helper()
	got, present := m[channelHashHexKey]
	if present != wantPresent {
		t.Errorf("channelHashHex present = %v (%#v), want present = %v", present, got, wantPresent)
		return
	}
	if wantPresent && got != want {
		t.Errorf("channelHashHex = %#v, want %q", got, want)
	}
	// Existing contract fields must survive untouched.
	if s, _ := m["sender"].(string); s != "Alice" {
		t.Errorf("sender = %q, want Alice", s)
	}
	if s, _ := m["text"].(string); s != "hello" {
		t.Errorf("text = %q, want hello", s)
	}
	if m["sender_timestamp"] == nil {
		t.Error("sender_timestamp must remain present")
	}
	if _, ok := m["timestamp"]; !ok {
		t.Error("timestamp must remain present")
	}
}

// ─── SQLite path ───────────────────────────────────────────────────────────────

func TestDBGetChannelMessagesChannelHashHexMatrix(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	seedTestData(t, db)

	for _, tc := range hashHexMatrix {
		insertChanTx(t, db, tc.rawHex, tc.pktHash, chanJSON(tc.extra))
	}

	msgs, total, err := db.GetChannelMessages("#chx", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != len(hashHexMatrix) {
		t.Fatalf("total = %d, want %d", total, len(hashHexMatrix))
	}

	for _, tc := range hashHexMatrix {
		t.Run(tc.name, func(t *testing.T) {
			assertMatrixRow(t, findMsg(t, msgs, tc.pktHash), tc.want, tc.wantPresent)
		})
	}
}

// ─── In-memory path ────────────────────────────────────────────────────────────

func TestStoreGetChannelMessagesChannelHashHexMatrix(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	seedTestData(t, db)

	for _, tc := range hashHexMatrix {
		insertChanTx(t, db, tc.rawHex, tc.pktHash, chanJSON(tc.extra))
	}

	store := NewPacketStore(db, nil)
	store.Load()

	msgs, total := store.GetChannelMessages("#chx", 100, 0)
	if total != len(hashHexMatrix) {
		t.Fatalf("total = %d, want %d", total, len(hashHexMatrix))
	}

	for _, tc := range hashHexMatrix {
		t.Run(tc.name, func(t *testing.T) {
			assertMatrixRow(t, findMsg(t, msgs, tc.pktHash), tc.want, tc.wantPresent)
		})
	}
}

// ─── Path parity ───────────────────────────────────────────────────────────────

// The same logical message built once must come back identically from both
// backends for every contract field this PR touches or promises not to touch.
func TestChannelHashHexPathParity(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	seedTestData(t, db)

	for _, tc := range hashHexMatrix {
		insertChanTx(t, db, tc.rawHex, tc.pktHash, chanJSON(tc.extra))
	}

	dbMsgs, dbTotal, err := db.GetChannelMessages("#chx", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	store := NewPacketStore(db, nil)
	store.Load()
	memMsgs, memTotal := store.GetChannelMessages("#chx", 100, 0)

	if dbTotal != memTotal {
		t.Errorf("total mismatch: sqlite %d, in-memory %d", dbTotal, memTotal)
	}

	for _, tc := range hashHexMatrix {
		t.Run(tc.name, func(t *testing.T) {
			a := findMsg(t, dbMsgs, tc.pktHash)
			b := findMsg(t, memMsgs, tc.pktHash)

			av, aok := a[channelHashHexKey]
			bv, bok := b[channelHashHexKey]
			if aok != bok || av != bv {
				t.Errorf("channelHashHex parity: sqlite (%#v,%v) vs in-memory (%#v,%v)", av, aok, bv, bok)
			}
			for _, field := range []string{"sender", "text", "sender_timestamp", "packetHash", "repeats"} {
				if fmt.Sprintf("%v", a[field]) != fmt.Sprintf("%v", b[field]) {
					t.Errorf("%s parity: sqlite %#v vs in-memory %#v", field, a[field], b[field])
				}
			}
		})
	}
}

// ─── Non-string JSON values (P3-1) ─────────────────────────────────────────────

// decoded_json is untrusted stored data: CoreScope's own decoders always write
// channelHashHex as a string, but a hand-edited row or a future non-Go
// producer could store any JSON type. A non-string value must only drop the
// optional field, never the containing message. Before this fix, the
// in-memory path typed the field as `string` inside its json.Unmarshal
// target, so a non-string value failed the whole Unmarshal and silently
// discarded the message; the SQLite path (map[string]interface{}) already
// kept the message and merely omitted the field. This matrix locks both
// paths to the SQLite behavior.
var nonStringHashHexMatrix = []struct {
	name    string
	rawHex  string
	pktHash string
	extra   string
}{
	{"numeric value", "N001", "chx_hash_numeric", `,"channelHashHex":167`},
	{"boolean value", "N002", "chx_hash_boolean", `,"channelHashHex":true`},
	{"object value", "N003", "chx_hash_object", `,"channelHashHex":{"a":1}`},
	{"array value", "N004", "chx_hash_array", `,"channelHashHex":["A","B"]`},
	{"json null value", "N005", "chx_hash_null", `,"channelHashHex":null`},
}

func TestDBGetChannelMessagesNonStringChannelHashHexMatrix(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	seedTestData(t, db)

	for _, tc := range nonStringHashHexMatrix {
		insertChanTx(t, db, tc.rawHex, tc.pktHash, chanJSON(tc.extra))
	}

	msgs, total, err := db.GetChannelMessages("#chx", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != len(nonStringHashHexMatrix) {
		t.Fatalf("total = %d, want %d — a non-string channelHashHex must not drop the message", total, len(nonStringHashHexMatrix))
	}
	for _, tc := range nonStringHashHexMatrix {
		t.Run(tc.name, func(t *testing.T) {
			assertMatrixRow(t, findMsg(t, msgs, tc.pktHash), "", false)
		})
	}
}

func TestStoreGetChannelMessagesNonStringChannelHashHexMatrix(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	seedTestData(t, db)

	for _, tc := range nonStringHashHexMatrix {
		insertChanTx(t, db, tc.rawHex, tc.pktHash, chanJSON(tc.extra))
	}

	store := NewPacketStore(db, nil)
	store.Load()

	msgs, total := store.GetChannelMessages("#chx", 100, 0)
	if total != len(nonStringHashHexMatrix) {
		t.Fatalf("total = %d, want %d — this is the exact P3-1 regression: a non-string channelHashHex must not drop the message from the in-memory path", total, len(nonStringHashHexMatrix))
	}
	for _, tc := range nonStringHashHexMatrix {
		t.Run(tc.name, func(t *testing.T) {
			assertMatrixRow(t, findMsg(t, msgs, tc.pktHash), "", false)
		})
	}
}

func TestChannelHashHexNonStringParity(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	seedTestData(t, db)

	for _, tc := range nonStringHashHexMatrix {
		insertChanTx(t, db, tc.rawHex, tc.pktHash, chanJSON(tc.extra))
	}

	dbMsgs, dbTotal, err := db.GetChannelMessages("#chx", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	store := NewPacketStore(db, nil)
	store.Load()
	memMsgs, memTotal := store.GetChannelMessages("#chx", 100, 0)

	if dbTotal != memTotal {
		t.Errorf("total mismatch: sqlite %d, in-memory %d", dbTotal, memTotal)
	}
	if dbTotal != len(nonStringHashHexMatrix) {
		t.Errorf("total = %d, want %d", dbTotal, len(nonStringHashHexMatrix))
	}

	for _, tc := range nonStringHashHexMatrix {
		t.Run(tc.name, func(t *testing.T) {
			a := findMsg(t, dbMsgs, tc.pktHash)
			b := findMsg(t, memMsgs, tc.pktHash)

			if _, present := a[channelHashHexKey]; present {
				t.Errorf("sqlite: channelHashHex must be absent for %s, got %#v", tc.name, a[channelHashHexKey])
			}
			if _, present := b[channelHashHexKey]; present {
				t.Errorf("in-memory: channelHashHex must be absent for %s, got %#v", tc.name, b[channelHashHexKey])
			}
			for _, field := range []string{"sender", "text", "sender_timestamp", "packetHash", "repeats"} {
				if fmt.Sprintf("%v", a[field]) != fmt.Sprintf("%v", b[field]) {
					t.Errorf("%s parity: sqlite %#v vs in-memory %#v", field, a[field], b[field])
				}
			}
		})
	}
}

// ─── Security ──────────────────────────────────────────────────────────────────

// The field must be derived from the wire byte alone: no key material may
// reach the response, and the operator-assigned channel name must not be able
// to influence the value.
func TestChannelHashHexSecurityAndNameIndependence(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	seedTestData(t, db)

	// Synthetic, non-functional stand-in for channel key material. Used only
	// as leak-detection bait — no real PSK is embedded in this repository.
	const psk = "00000000deadbeef00000000deadbeef"

	// Same channel name, two different wire hashes -> two different outputs.
	insertChanTx(t, db, "S001", "sec_same_name_a",
		`{"type":"CHAN","channel":"#chx","text":"Alice: hello","sender":"Alice","sender_timestamp":1,"channelHashHex":"11"}`)
	insertChanTx(t, db, "S002", "sec_same_name_b",
		`{"type":"CHAN","channel":"#chx","text":"Alice: hello","sender":"Alice","sender_timestamp":2,"channelHashHex":"22"}`)
	// A payload that also carries key-ish material must not leak it.
	insertChanTx(t, db, "S003", "sec_with_key",
		`{"type":"CHAN","channel":"#chx","text":"Alice: hello","sender":"Alice","sender_timestamp":3,"channelHashHex":"A7","key":"`+psk+`","secret":"`+psk+`"}`)

	check := func(t *testing.T, msgs []map[string]interface{}, label string) {
		t.Helper()
		a := findMsg(t, msgs, "sec_same_name_a")
		b := findMsg(t, msgs, "sec_same_name_b")
		if a[channelHashHexKey] != "11" || b[channelHashHexKey] != "22" {
			t.Errorf("%s: identical channel name must not collapse distinct wire hashes: %#v vs %#v",
				label, a[channelHashHexKey], b[channelHashHexKey])
		}
		for _, m := range msgs {
			for k, v := range m {
				s, ok := v.(string)
				if !ok {
					continue
				}
				if strings.Contains(strings.ToLower(s), psk) {
					t.Errorf("%s: PSK material leaked in field %q", label, k)
				}
			}
			if _, present := m["key"]; present {
				t.Errorf("%s: channel key field must not be emitted", label)
			}
			if _, present := m["secret"]; present {
				t.Errorf("%s: channel secret field must not be emitted", label)
			}
			if hexVal, present := m[channelHashHexKey]; present {
				s, _ := hexVal.(string)
				if len(s) != 2 {
					t.Errorf("%s: channelHashHex must be exactly 2 characters, got %q", label, s)
				}
				if strings.HasPrefix(s, "enc_") {
					t.Errorf("%s: channelHashHex must never carry the enc_ prefix, got %q", label, s)
				}
			}
		}
	}

	dbMsgs, _, err := db.GetChannelMessages("#chx", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	check(t, dbMsgs, "sqlite")

	store := NewPacketStore(db, nil)
	store.Load()
	memMsgs, _ := store.GetChannelMessages("#chx", 100, 0)
	check(t, memMsgs, "in-memory")
}

// Two different channel names sharing one wire hash is a legitimate one-byte
// collision. It must be reported honestly on both rows rather than suppressed
// or disambiguated — the field is evidence, not a unique channel identity.
func TestChannelHashHexCollisionAcrossNamesIsPreserved(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	seedTestData(t, db)

	res, err := db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		VALUES ('K001', 'collide_other_name', ?, 1, 5, ?, ?)`,
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
		`{"type":"CHAN","channel":"#other","text":"Bob: hi","sender":"Bob","sender_timestamp":9,"channelHashHex":"A7"}`,
		"#other")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if _, err := db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp)
		VALUES (?, 1, 10.0, -90, '[]', ?)`, id, time.Now().Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	insertChanTx(t, db, "K002", "collide_chx_name",
		`{"type":"CHAN","channel":"#chx","text":"Alice: hello","sender":"Alice","sender_timestamp":8,"channelHashHex":"A7"}`)

	chx, _, err := db.GetChannelMessages("#chx", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := db.GetChannelMessages("#other", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := findMsg(t, chx, "collide_chx_name")[channelHashHexKey]; got != "A7" {
		t.Errorf("#chx row: got %#v, want A7", got)
	}
	if got := findMsg(t, other, "collide_other_name")[channelHashHexKey]; got != "A7" {
		t.Errorf("#other row: got %#v, want A7", got)
	}
}

// ─── Regression ────────────────────────────────────────────────────────────────

// Routing stays keyed on the existing channel-name/hash label; the new field
// must not have become a route key, and pagination must be unaffected.
func TestChannelHashHexDoesNotAffectRoutingOrPagination(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	seedTestData(t, db)

	for _, tc := range hashHexMatrix {
		insertChanTx(t, db, tc.rawHex, tc.pktHash, chanJSON(tc.extra))
	}

	// Routing by the wire hash value must NOT resolve a channel.
	for _, notARoute := range []string{"A7", "00", "FF", "enc_A7"} {
		msgs, total, err := db.GetChannelMessages(notARoute, 100, 0)
		if err != nil {
			t.Fatal(err)
		}
		if total != 0 || len(msgs) != 0 {
			t.Errorf("route %q must not resolve via the wire hash, got %d/%d", notARoute, total, len(msgs))
		}
	}

	full, total, err := db.GetChannelMessages("#chx", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != len(hashHexMatrix) || len(full) != len(hashHexMatrix) {
		t.Fatalf("unpaginated: total %d / len %d, want %d", total, len(full), len(hashHexMatrix))
	}

	page, pagedTotal, err := db.GetChannelMessages("#chx", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if pagedTotal != total {
		t.Errorf("paginated total = %d, want %d (total is before pagination)", pagedTotal, total)
	}
	if len(page) != 3 {
		t.Errorf("page length = %d, want 3", len(page))
	}
}

// ─── OpenAPI contract ──────────────────────────────────────────────────────────

func TestOpenAPIDocumentsChannelHashHex(t *testing.T) {
	schemas := componentSchemas()

	cm, ok := schemas["ChannelMessage"].(map[string]interface{})
	if !ok {
		t.Fatal("ChannelMessage schema missing from components/schemas")
	}
	props, ok := cm["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("ChannelMessage schema has no properties")
	}

	field, ok := props[channelHashHexKey].(map[string]interface{})
	if !ok {
		t.Fatalf("%s not documented on ChannelMessage", channelHashHexKey)
	}
	if field["type"] != "string" {
		t.Errorf("%s type = %#v, want string", channelHashHexKey, field["type"])
	}
	if field["pattern"] != "^[0-9A-F]{2}$" {
		t.Errorf("%s pattern = %#v, want ^[0-9A-F]{2}$", channelHashHexKey, field["pattern"])
	}

	desc, _ := field["description"].(string)
	// The field must never be documented as a sufficient channel identity,
	// and absence must be documented as unavailable/provenance-based (not
	// just "legacy"), and REST validation must be documented as not
	// extending to the WebSocket surface (P3-2, P3-3).
	for _, required := range []string{"COLLISION-PRONE", "Non-secret", "legacy", "Companion", "permanently", "WebSocket"} {
		if !strings.Contains(desc, required) {
			t.Errorf("%s description must mention %q", channelHashHexKey, required)
		}
	}
	// "00" must be documented as a real present value, not an absence.
	if !strings.Contains(desc, "\"00\"") {
		t.Errorf("%s description must state that \"00\" is a valid present value", channelHashHexKey)
	}

	// Required-list membership would break legacy records, which legitimately
	// omit the field.
	if req, present := cm["required"]; present {
		t.Errorf("ChannelMessage must not declare required fields (legacy rows omit %s): %#v", channelHashHexKey, req)
	}

	// The clock fields must stay documented and distinct.
	for _, clock := range []string{"timestamp", "sender_timestamp"} {
		if _, ok := props[clock]; !ok {
			t.Errorf("%s must remain documented on ChannelMessage", clock)
		}
	}

	resp, ok := schemas["ChannelMessagesResponse"].(map[string]interface{})
	if !ok {
		t.Fatal("ChannelMessagesResponse schema missing")
	}
	rprops, _ := resp["properties"].(map[string]interface{})
	msgs, _ := rprops["messages"].(map[string]interface{})
	items, _ := msgs["items"].(map[string]interface{})
	if items["$ref"] != "#/components/schemas/ChannelMessage" {
		t.Errorf("messages items ref = %#v, want ChannelMessage ref", items["$ref"])
	}

	route, ok := routeDescriptions()["GET /api/channels/{hash}/messages"]
	if !ok {
		t.Fatal("channel messages route missing from routeDescriptions")
	}
	if route.Response["$ref"] != "#/components/schemas/ChannelMessagesResponse" {
		t.Errorf("route response = %#v, want ChannelMessagesResponse ref", route.Response)
	}
}
