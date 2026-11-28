package main

import (
	"reflect"
	"testing"
	"time"
	"unsafe"
)

func TestObservedPathHashSizeMask_ConservativePathEvidence(t *testing.T) {
	tests := []struct {
		name string
		path string
		want uint8
	}{
		{"one byte", `["FD","01"]`, 0b001},
		{"two bytes", `["01fa","BEEF"]`, 0b010},
		{"two bytes with whitespace", " [ \"01fa\" ,\n\t\"BEEF\" ] ", 0b010},
		{"three bytes", `["010203","A0B0C0"]`, 0b100},
		{"empty direct path", `[]`, 0},
		{"null path", `null`, 0},
		{"empty string", ``, 0},
		{"invalid json", `["AA"`, 0},
		{"empty hop", `[""]`, 0},
		{"odd length", `["ABC"]`, 0},
		{"unsupported four bytes", `["01020304"]`, 0},
		{"non hex", `["GG"]`, 0},
		{"mixed widths", `["AA","BEEF"]`, 0},
		{"non string hop", `["AA",2]`, 0},
		{"trailing data", `["AA"] true`, 0},
		{"escaped hex is not direct evidence", `["\u0041A"]`, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := observedPathHashSizeMask(tt.path); got != tt.want {
				t.Fatalf("observedPathHashSizeMask(%q) = %03b, want %03b", tt.path, got, tt.want)
			}
		})
	}
}

func TestObservedPathHashSizeMask_NoAllocations(t *testing.T) {
	if got := testing.AllocsPerRun(1000, func() {
		if observedPathHashSizeMask(`["01FA","BEEF"]`) != 0b010 {
			panic("unexpected evidence")
		}
	}); got != 0 {
		t.Fatalf("observedPathHashSizeMask allocations = %g, want 0 on the load/ingest hot path", got)
	}
}

func TestObservedPathHashSizes_SortedAndOrderIndependent(t *testing.T) {
	pt := PayloadGRP_TXT
	forward := StoreTx{PayloadType: &pt}
	for _, path := range []string{`["AA"]`, `["BEEF"]`, `["010203"]`, `[]`, `["AA","BEEF"]`} {
		forward.mergeObservedPathHashSize(path)
	}
	reverse := StoreTx{PayloadType: &pt}
	for _, path := range []string{`["010203"]`, `["BEEF"]`, `["AA"]`} {
		reverse.mergeObservedPathHashSize(path)
	}
	want := []int{1, 2, 3}
	if got := forward.observedPathHashSizes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("forward sizes = %v, want %v", got, want)
	}
	if got := reverse.observedPathHashSizes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("reverse sizes = %v, want %v", got, want)
	}
	if got := observedPathHashSizes(0); len(got) != 0 {
		t.Fatalf("unknown sizes = %v, want empty", got)
	}
}

func TestStoreTxLayoutFitsObservedPathHashSizeMaskInPadding(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("layout budget is defined for 64-bit platforms")
	}
	if got := unsafe.Sizeof(StoreTx{}); got != 320 {
		t.Fatalf("unsafe.Sizeof(StoreTx{}) = %d, want 320: observed path hash size mask must use existing padding", got)
	}
}

func seedObservedPathHashSizeMessage(t *testing.T, db *DB, paths []string) {
	t.Helper()
	if _, err := db.conn.Exec(`INSERT INTO observers (rowid, id, name, iata) VALUES (1, 'obs-1', 'Observer One', 'TST')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.Exec(`INSERT INTO transmissions
		(id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		VALUES (1, '1100', 'hash-size-message', ?, 1, 5,
		'{"type":"CHAN","channel":"#hash-size","text":"Alice: hello","sender":"Alice"}', '#hash-size')`,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	for i, path := range paths {
		if _, err := db.conn.Exec(`INSERT INTO observations
			(id, transmission_id, observer_idx, snr, rssi, path_json, timestamp)
			VALUES (?, 1, 1, 7, -90, ?, ?)`, i+1, path, time.Now().Unix()+int64(i)); err != nil {
			t.Fatal(err)
		}
	}
}

func requireObservedPathHashSizes(t *testing.T, got []map[string]interface{}, want []int) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("messages = %d, want 1: %+v", len(got), got)
	}
	sizes, ok := got[0]["observedPathHashSizes"].([]int)
	if !ok {
		t.Fatalf("observedPathHashSizes type/value = %T/%v, want []int", got[0]["observedPathHashSizes"], got[0]["observedPathHashSizes"])
	}
	if !reflect.DeepEqual(sizes, want) {
		t.Fatalf("observedPathHashSizes = %v, want %v", sizes, want)
	}
}

func TestChannelMessagesObservedPathHashSizes_DBAndStoreParity(t *testing.T) {
	orders := []struct {
		name  string
		paths []string
	}{
		{"one then three", []string{`["AA"]`, `[]`, `["010203"]`, `["AA","BEEF"]`}},
		{"three then one", []string{`["010203"]`, `["AA","BEEF"]`, `[]`, `["AA"]`}},
	}
	for _, order := range orders {
		t.Run(order.name, func(t *testing.T) {
			db := setupTestDB(t)
			defer db.Close()
			seedObservedPathHashSizeMessage(t, db, order.paths)

			dbMessages, _, err := db.GetChannelMessages("#hash-size", 10, 0)
			if err != nil {
				t.Fatal(err)
			}
			requireObservedPathHashSizes(t, dbMessages, []int{1, 3})

			store := NewPacketStore(db, nil)
			if err := store.Load(); err != nil {
				t.Fatal(err)
			}
			storeMessages, _ := store.GetChannelMessages("#hash-size", 10, 0)
			requireObservedPathHashSizes(t, storeMessages, []int{1, 3})
		})
	}
}

func TestChannelMessagesObservedPathHashSizes_ChunkedAndLiveObservation(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	seedObservedPathHashSizeMessage(t, db, []string{`["BEEF"]`})

	store := NewPacketStore(db, nil)
	if err := store.LoadChunked(1); err != nil {
		t.Fatal(err)
	}
	messages, _ := store.GetChannelMessages("#hash-size", 10, 0)
	requireObservedPathHashSizes(t, messages, []int{2})

	if _, err := db.conn.Exec(`INSERT INTO observations
		(id, transmission_id, observer_idx, snr, rssi, path_json, timestamp)
		VALUES (2, 1, 1, 8, -89, '["010203"]', ?)`, time.Now().Unix()+10); err != nil {
		t.Fatal(err)
	}
	broadcasts := store.IngestNewObservations(1, 10)
	if len(broadcasts) != 1 {
		t.Fatalf("broadcasts = %d, want 1", len(broadcasts))
	}
	pkt, ok := broadcasts[0]["packet"].(map[string]interface{})
	if !ok {
		t.Fatalf("broadcast packet = %T, want map", broadcasts[0]["packet"])
	}
	if got, ok := pkt["observed_path_hash_sizes"].([]int); !ok || !reflect.DeepEqual(got, []int{2, 3}) {
		t.Fatalf("broadcast observed_path_hash_sizes = %T/%v, want [2 3]", pkt["observed_path_hash_sizes"], pkt["observed_path_hash_sizes"])
	}

	messages, _ = store.GetChannelMessages("#hash-size", 10, 0)
	requireObservedPathHashSizes(t, messages, []int{2, 3})
}

func TestChannelMessagesObservedPathHashSizes_LiveNewTransmission(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	seedObservedPathHashSizeMessage(t, db, []string{`["AA"]`, `["BEEF"]`})

	store := NewPacketStore(db, nil)
	broadcasts, maxID := store.IngestNewFromDB(0, 10)
	if maxID != 1 {
		t.Fatalf("maxID = %d, want 1", maxID)
	}
	if len(broadcasts) != 2 {
		t.Fatalf("broadcasts = %d, want one per observation (2)", len(broadcasts))
	}
	for i, broadcast := range broadcasts {
		pkt, ok := broadcast["packet"].(map[string]interface{})
		if !ok {
			t.Fatalf("broadcast[%d] packet = %T, want map", i, broadcast["packet"])
		}
		if got, ok := pkt["observed_path_hash_sizes"].([]int); !ok || !reflect.DeepEqual(got, []int{1, 2}) {
			t.Fatalf("broadcast[%d] observed_path_hash_sizes = %T/%v, want [1 2]", i,
				pkt["observed_path_hash_sizes"], pkt["observed_path_hash_sizes"])
		}
	}
	messages, _ := store.GetChannelMessages("#hash-size", 10, 0)
	requireObservedPathHashSizes(t, messages, []int{1, 2})

	pt := PayloadGRP_TXT
	packets := store.QueryPackets(PacketQuery{Type: &pt, Limit: 10})
	if len(packets.Packets) != 1 {
		t.Fatalf("packet query returned %d rows, want 1", len(packets.Packets))
	}
	if got, ok := packets.Packets[0]["observed_path_hash_sizes"].([]int); !ok || !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("packet observed_path_hash_sizes = %T/%v, want [1 2]",
			packets.Packets[0]["observed_path_hash_sizes"], packets.Packets[0]["observed_path_hash_sizes"])
	}
}

func TestChannelMessagesObservedPathHashSizes_UnknownIsEmpty(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	seedObservedPathHashSizeMessage(t, db, []string{`[]`, `["AA","BEEF"]`, `["GG"]`})

	messages, _, err := db.GetChannelMessages("#hash-size", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	requireObservedPathHashSizes(t, messages, []int{})
}
