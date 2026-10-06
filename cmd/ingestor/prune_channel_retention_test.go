package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// payloadTypeGrpTxt is the channel-message payload type (PAYLOAD_TYPE_GRP_TXT
// in firmware/src/Packet.h).
const payloadTypeGrpTxt = 5

// seedRetentionTx inserts one transmission of the given payload type (nil for
// a NULL payload_type) stamped ageDays in the past, with obs observations, and
// returns its id.
func seedRetentionTx(t *testing.T, store *Store, hash string, payloadType *int, ageDays, obs int) int64 {
	t.Helper()
	ts := time.Now().UTC().AddDate(0, 0, -ageDays).Format(time.RFC3339)
	res, err := store.db.Exec(
		`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, payload_version, decoded_json)
		 VALUES ('AA', ?, ?, 0, ?, 1, '{}')`,
		hash, ts, payloadType,
	)
	if err != nil {
		t.Fatalf("seed tx %s: %v", hash, err)
	}
	txID, _ := res.LastInsertId()
	for j := 0; j < obs; j++ {
		if _, err := store.db.Exec(
			`INSERT INTO observations (transmission_id, observer_idx, direction, snr, rssi, score, path_json, timestamp)
			 VALUES (?, ?, 'rx', 1.0, -100, 0, '[]', ?)`,
			txID, j, time.Now().Unix(),
		); err != nil {
			t.Fatalf("seed obs %s/%d: %v", hash, j, err)
		}
	}
	return txID
}

// seedRetentionMix seeds the #296 acceptance set and returns nothing; the
// hashes name each row's role.
func seedRetentionMix(t *testing.T, store *Store) {
	t.Helper()
	seedRetentionTx(t, store, "chan-30d", intPtr(payloadTypeGrpTxt), 30, 3)
	seedRetentionTx(t, store, "chan-100d", intPtr(payloadTypeGrpTxt), 100, 2)
	seedRetentionTx(t, store, "chan-5d", intPtr(payloadTypeGrpTxt), 5, 1)
	seedRetentionTx(t, store, "advert-30d", intPtr(4), 30, 2)
	seedRetentionTx(t, store, "txtmsg-30d", intPtr(2), 30, 1)
	seedRetentionTx(t, store, "nulltype-30d", nil, 30, 1)
	seedRetentionTx(t, store, "advert-100d", intPtr(4), 100, 1)
	seedRetentionTx(t, store, "advert-5d", intPtr(4), 5, 1)
}

func remainingHashes(t *testing.T, store *Store) []string {
	t.Helper()
	rows, err := store.db.Query(`SELECT hash FROM transmissions ORDER BY hash`)
	if err != nil {
		t.Fatalf("list hashes: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			t.Fatalf("scan hash: %v", err)
		}
		out = append(out, h)
	}
	return out
}

func observationsOf(t *testing.T, store *Store, hash string) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM observations o
		JOIN transmissions t ON t.id = o.transmission_id WHERE t.hash = ?`, hash).Scan(&n); err != nil {
		t.Fatalf("count observations of %s: %v", hash, err)
	}
	return n
}

func orphanObservations(t *testing.T, store *Store) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM observations o
		LEFT JOIN transmissions t ON t.id = o.transmission_id WHERE t.id IS NULL`).Scan(&n); err != nil {
		t.Fatalf("orphan check: %v", err)
	}
	return n
}

func retentionConfig(t *testing.T, raw string) *Config {
	t.Helper()
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("parse config %s: %v", raw, err)
	}
	return &cfg
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestStartupPruneChannelDaysKeepsChannelMessages is the #296 acceptance
// case: with packetDays = 14 and channelDays = 90, the startup prune keeps a
// 30-day-old channel message with all its observations, and removes a
// 30-day-old advert, other packet types (including a NULL payload_type) and a
// 100-day-old channel message.
func TestStartupPruneChannelDaysKeepsChannelMessages(t *testing.T) {
	store := openPruneStore(t, "prune-channel-days.db")
	seedRetentionMix(t, store)

	cfg := retentionConfig(t, `{"retention":{"packetDays":14,"channelDays":90}}`)
	got := runTransmissionRetention(store, cfg, "startup")

	want := []string{"advert-5d", "chan-30d", "chan-5d"}
	if have := remainingHashes(t, store); !equalStrings(have, want) {
		t.Fatalf("remaining transmissions = %v, want %v", have, want)
	}
	if n := observationsOf(t, store, "chan-30d"); n != 3 {
		t.Fatalf("observations of the kept 30-day channel message = %d, want 3", n)
	}
	if n := orphanObservations(t, store); n != 0 {
		t.Fatalf("expected no orphaned observations, got %d", n)
	}
	if got.Packets != 4 || got.ChannelMessages != 1 {
		t.Fatalf("pruned = %+v, want 4 packets and 1 channel message", got)
	}
}

// TestStartupPruneChannelDaysOffMatchesPacketDays pins the "unchanged
// behaviour" half of #296: channelDays unset, 0, or not longer than
// packetDays prunes exactly what PruneOldPackets(packetDays) prunes, through
// the same prune_packets transactions and no channel prune at all.
func TestStartupPruneChannelDaysOffMatchesPacketDays(t *testing.T) {
	reference := openPruneStore(t, "prune-channel-reference.db")
	seedRetentionMix(t, reference)
	if _, err := reference.PruneOldPackets(14); err != nil {
		t.Fatalf("reference PruneOldPackets: %v", err)
	}
	want := remainingHashes(t, reference)
	wantObs := countRows(t, reference, "observations")

	for name, raw := range map[string]string{
		"unset":          `{"retention":{"packetDays":14}}`,
		"zero":           `{"retention":{"packetDays":14,"channelDays":0}}`,
		"negative":       `{"retention":{"packetDays":14,"channelDays":-1}}`,
		"equal":          `{"retention":{"packetDays":14,"channelDays":14}}`,
		"shorter":        `{"retention":{"packetDays":14,"channelDays":7}}`,
		"packetDays off": `{"retention":{"packetDays":0,"channelDays":90}}`,
	} {
		t.Run(name, func(t *testing.T) {
			store := openPruneStore(t, "prune-channel-off.db")
			seedRetentionMix(t, store)
			cfg := retentionConfig(t, raw)

			ResetWriterStatsForTest()
			got := runTransmissionRetention(store, cfg, "startup")

			if got.ChannelMessages != 0 {
				t.Fatalf("channel prune reported %d, want 0", got.ChannelMessages)
			}
			if c := store.WriterStatsSnapshot()["prune_channel_messages"].Count; c != 0 {
				t.Fatalf("expected no prune_channel_messages transactions, got %d", c)
			}
			if cfg.PacketDaysOrZero() == 0 {
				all := remainingHashes(t, store)
				if len(all) != 8 || got.Packets != 0 {
					t.Fatalf("packetDays off must prune nothing, kept %v, pruned %d", all, got.Packets)
				}
				return
			}
			if have := remainingHashes(t, store); !equalStrings(have, want) {
				t.Fatalf("remaining = %v, want PruneOldPackets(14)'s %v", have, want)
			}
			if n := countRows(t, store, "observations"); n != wantObs {
				t.Fatalf("observations = %d, want %d", n, wantObs)
			}
			if c := store.WriterStatsSnapshot()["prune_packets"].Count; c != 1 {
				t.Fatalf("expected 1 prune_packets transaction, got %d", c)
			}
		})
	}
}

// TestPruneTransmissionsChannelDaysSpansBatches covers both prunes past the
// batch boundary. Every row of a seed call shares one first_seen, so the
// packet prune's first_seen cursor must re-include the tied rows at the
// boundary (>=, not >), and the channel messages interleaved with the aged
// packets must survive every batch.
func TestPruneTransmissionsChannelDaysSpansBatches(t *testing.T) {
	store := openPruneStore(t, "prune-channel-batches.db")

	const agedPackets = pruneBatchTransmissions*2 + 37
	const keptChannel = 40
	const agedChannel = pruneBatchTransmissions + 3
	ts30 := time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339)
	ts100 := time.Now().UTC().AddDate(0, 0, -100).Format(time.RFC3339)
	seed, err := store.db.Begin()
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	insert := func(hash, ts string, payloadType int) {
		if _, err := seed.Exec(
			`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, payload_version, decoded_json)
			 VALUES ('AA', ?, ?, 0, ?, 1, '{}')`, hash, ts, payloadType); err != nil {
			t.Fatalf("seed %s: %v", hash, err)
		}
	}
	// Interleave by id so kept channel rows sit inside the packet batches.
	for i := 0; i < agedPackets; i++ {
		insert(fmt.Sprintf("pkt-%d", i), ts30, 4)
		if i%(agedPackets/keptChannel) == 0 && i/(agedPackets/keptChannel) < keptChannel {
			insert(fmt.Sprintf("chan-keep-%d", i), ts30, payloadTypeGrpTxt)
		}
	}
	for i := 0; i < agedChannel; i++ {
		insert(fmt.Sprintf("chan-old-%d", i), ts100, payloadTypeGrpTxt)
	}
	if err := seed.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	if n := countRows(t, store, "transmissions"); n != agedPackets+keptChannel+agedChannel {
		t.Fatalf("seeded %d rows, want %d", n, agedPackets+keptChannel+agedChannel)
	}

	ResetWriterStatsForTest()
	got, err := store.PruneTransmissions(14, 90)
	if err != nil {
		t.Fatalf("PruneTransmissions: %v", err)
	}
	if got.Packets != agedPackets || got.ChannelMessages != agedChannel {
		t.Fatalf("pruned = %+v, want %d packets and %d channel messages", got, agedPackets, agedChannel)
	}
	have := remainingHashes(t, store)
	if len(have) != keptChannel {
		t.Fatalf("kept %d transmissions, want the %d 30-day channel messages", len(have), keptChannel)
	}
	sort.Strings(have)
	for _, h := range have {
		if len(h) < 10 || h[:10] != "chan-keep-" {
			t.Fatalf("unexpected survivor %q", h)
		}
	}

	stats := store.WriterStatsSnapshot()
	// 2 full packet batches + 1 partial; 1 full channel batch + 1 partial.
	if c := stats["prune_packets"].Count; c != 3 {
		t.Fatalf("expected 3 prune_packets transactions, got %d", c)
	}
	if c := stats["prune_channel_messages"].Count; c != 2 {
		t.Fatalf("expected 2 prune_channel_messages transactions, got %d", c)
	}
}

// TestRetentionChannelDaysConfig pins the config surface: channelDays parses
// from retention.channelDays and folds unset/0/negative to 0.
func TestRetentionChannelDaysConfig(t *testing.T) {
	for raw, want := range map[string]int{
		`{}`:                               0,
		`{"retention":{"packetDays":14}}`:  0,
		`{"retention":{"channelDays":0}}`:  0,
		`{"retention":{"channelDays":-3}}`: 0,
		`{"retention":{"channelDays":90}}`: 90,
	} {
		if got := retentionConfig(t, raw).ChannelDaysOrZero(); got != want {
			t.Errorf("%s: ChannelDaysOrZero() = %d, want %d", raw, got, want)
		}
	}
}

// TestChannelDaysNoEffectWarning pins the startup warning for a channelDays
// that changes nothing: not longer than packetDays, or packetDays 0, where
// nothing is pruned at all (review F2 of #296). An unset channelDays, or one
// that applies, logs nothing.
func TestChannelDaysNoEffectWarning(t *testing.T) {
	for _, c := range []struct {
		packetDays, channelDays int
		want                    string
	}{
		{14, 7, "[prune] retention.channelDays=7 has no effect: it is not longer than packetDays=14"},
		{14, 14, "[prune] retention.channelDays=14 has no effect: it is not longer than packetDays=14"},
		{0, 90, "[prune] retention.channelDays=90 has no effect: packetDays is 0, so no transmissions are pruned"},
		{14, 90, ""},
		{14, 0, ""},
		{0, 0, ""},
	} {
		if got := channelDaysNoEffect(c.packetDays, c.channelDays); got != c.want {
			t.Errorf("channelDaysNoEffect(%d, %d) = %q, want %q", c.packetDays, c.channelDays, got, c.want)
		}
	}
}

// TestPruneTransmissionsDailyRunStartsAtPreviousCutoff pins the walk bound of
// the repeated (daily) prune. Its packet prune steps over every kept channel
// message it walks; without a floor every run would walk all of them under
// writerMu, ~250ms a day on a staging-sized fixture
// (TestPruneTransmissionsChannelDaysTiming). A completed run therefore
// records its packet cutoff, and the next run in the process starts there.
// The planted row is written straight to SQLite, past InsertTransmission and
// so past the floor's record of backdated writes
// (TestPruneTransmissionsPrunesBackdatedInsertBelowFloor), which shows that the
// second run really does not look below the floor; a new Store (a restart)
// starts from the bottom again.
func TestPruneTransmissionsDailyRunStartsAtPreviousCutoff(t *testing.T) {
	path := t.TempDir() + "/prune-floor.db"
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	seedRetentionTx(t, store, "advert-30d", intPtr(4), 30, 1)
	seedRetentionTx(t, store, "chan-30d", intPtr(payloadTypeGrpTxt), 30, 1)

	if store.packetPruneFloor != "" {
		t.Fatalf("a new Store must start without a floor, got %q", store.packetPruneFloor)
	}
	before := time.Now().UTC().AddDate(0, 0, -14).Format(time.RFC3339)
	if _, err := store.PruneTransmissions(14, 90); err != nil {
		t.Fatalf("first PruneTransmissions: %v", err)
	}
	after := time.Now().UTC().AddDate(0, 0, -14).Format(time.RFC3339)
	if f := store.packetPruneFloor; f < before || f > after {
		t.Fatalf("floor = %q, want the run's packet cutoff in [%q, %q]", f, before, after)
	}

	// Below the floor and not written by the ingest path, so outside the next
	// run's walk.
	seedRetentionTx(t, store, "planted-20d", intPtr(4), 20, 1)
	got, err := store.PruneTransmissions(14, 90)
	if err != nil {
		t.Fatalf("second PruneTransmissions: %v", err)
	}
	if got.Packets != 0 {
		t.Fatalf("second run pruned %d packets below its floor, want 0", got.Packets)
	}
	store.Close()

	restarted, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer restarted.Close()
	got, err = restarted.PruneTransmissions(14, 90)
	if err != nil {
		t.Fatalf("PruneTransmissions after restart: %v", err)
	}
	if got.Packets != 1 {
		t.Fatalf("after a restart the full walk must prune the planted row, pruned %d", got.Packets)
	}
	if have := remainingHashes(t, restarted); !equalStrings(have, []string{"chan-30d"}) {
		t.Fatalf("remaining = %v, want [chan-30d]", have)
	}
}

// ingestRetentionPacket writes one observation of hash through the real
// ingest path (InsertTransmission), received ageDays ago.
func ingestRetentionPacket(t *testing.T, store *Store, hash string, payloadType, ageDays int) {
	t.Helper()
	if _, err := store.InsertTransmission(&PacketData{
		RawHex:      "AA",
		Timestamp:   time.Now().UTC().AddDate(0, 0, -ageDays).Format(time.RFC3339),
		Hash:        hash,
		PayloadType: payloadType,
		DecodedJSON: "{}",
	}); err != nil {
		t.Fatalf("ingest %s: %v", hash, err)
	}
}

// TestPruneTransmissionsPrunesBackdatedInsertBelowFloor is the review case of
// #296 (F1): first_seen is the observer's receive time, not the ingest time.
// resolveRxTime accepts an envelope timestamp up to 30 days old, so a
// buffered upload or a lagging observer clock lands a non-channel row below a
// completed run's floor. The next run must still prune it, as
// PruneOldPackets would.
func TestPruneTransmissionsPrunesBackdatedInsertBelowFloor(t *testing.T) {
	store := openPruneStore(t, "prune-floor-backdated.db")
	seedRetentionTx(t, store, "chan-30d", intPtr(payloadTypeGrpTxt), 30, 1)
	if _, err := store.PruneTransmissions(14, 90); err != nil {
		t.Fatalf("first PruneTransmissions: %v", err)
	}

	ingestRetentionPacket(t, store, "late-advert-20d", 4, 20)
	ingestRetentionPacket(t, store, "fresh-advert", 4, 0)
	got, err := store.PruneTransmissions(14, 90)
	if err != nil {
		t.Fatalf("second PruneTransmissions: %v", err)
	}
	if got.Packets != 1 {
		t.Fatalf("second run pruned %d packets, want the late 20-day advert", got.Packets)
	}
	if have := remainingHashes(t, store); !equalStrings(have, []string{"chan-30d", "fresh-advert"}) {
		t.Fatalf("remaining = %v, want [chan-30d fresh-advert]", have)
	}
}

// TestPruneTransmissionsPrunesLoweredFirstSeenBelowFloor covers the other
// ingest write of first_seen: a later observation with an earlier receive
// time lowers an existing transmission's first_seen (stmtUpdateTxFirstSeen).
// When that moves a non-channel row below the floor, the next run must still
// prune it.
func TestPruneTransmissionsPrunesLoweredFirstSeenBelowFloor(t *testing.T) {
	store := openPruneStore(t, "prune-floor-lowered.db")
	ingestRetentionPacket(t, store, "advert", 4, 0)
	if _, err := store.PruneTransmissions(14, 90); err != nil {
		t.Fatalf("first PruneTransmissions: %v", err)
	}

	// A second observer uploads the same advert, received 20 days ago.
	ingestRetentionPacket(t, store, "advert", 4, 20)
	got, err := store.PruneTransmissions(14, 90)
	if err != nil {
		t.Fatalf("second PruneTransmissions: %v", err)
	}
	if got.Packets != 1 || countRows(t, store, "transmissions") != 0 {
		t.Fatalf("second run pruned %d packets, kept %v; want the advert whose first_seen moved 20 days back",
			got.Packets, remainingHashes(t, store))
	}
}

// TestPruneTransmissionsFailedRunKeepsBackdatedInsert pins that a failed run
// does not lose a backdated write: the next completed run still starts at or
// below it.
func TestPruneTransmissionsFailedRunKeepsBackdatedInsert(t *testing.T) {
	store := openPruneStore(t, "prune-floor-failed.db")
	if _, err := store.PruneTransmissions(14, 90); err != nil {
		t.Fatalf("first PruneTransmissions: %v", err)
	}
	ingestRetentionPacket(t, store, "late-advert-20d", 4, 20)

	// Make every prune batch fail at its route_mask_changes delete.
	if _, err := store.db.Exec(`ALTER TABLE route_mask_changes RENAME TO route_mask_changes_off`); err != nil {
		t.Fatalf("hide route_mask_changes: %v", err)
	}
	if _, err := store.PruneTransmissions(14, 90); err == nil {
		t.Fatalf("PruneTransmissions without route_mask_changes succeeded, want an error")
	}
	if _, err := store.db.Exec(`ALTER TABLE route_mask_changes_off RENAME TO route_mask_changes`); err != nil {
		t.Fatalf("restore route_mask_changes: %v", err)
	}

	got, err := store.PruneTransmissions(14, 90)
	if err != nil {
		t.Fatalf("PruneTransmissions after the failure: %v", err)
	}
	if got.Packets != 1 || countRows(t, store, "transmissions") != 0 {
		t.Fatalf("run after a failed one pruned %d packets, kept %v; want the late 20-day advert",
			got.Packets, remainingHashes(t, store))
	}
}
