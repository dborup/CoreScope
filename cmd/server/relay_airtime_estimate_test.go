package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/lora"
)

// #697 M2: estimated relay airtime per repeater — LoRa time-on-air of every
// packet a repeater relayed, summed into 24 rolling hours.

func TestRelayAirtimeHours_Buckets(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	var h relayAirtimeHours
	add := func(age time.Duration, d time.Duration) { h.add(now, now.Add(-age), d) }
	add(0, 1*time.Second)
	add(59*time.Minute+59*time.Second, 2*time.Second)
	add(time.Hour, 4*time.Second)
	add(23*time.Hour+59*time.Minute+59*time.Second, 8*time.Second)
	add(24*time.Hour, 100*time.Second) // too old
	add(-time.Second, 100*time.Second) // future-dated
	if h[0] != 3*time.Second {
		t.Errorf("last hour: want 3s (ages 0 and 59m59s), got %v", h[0])
	}
	if h[1] != 4*time.Second {
		t.Errorf("hour 1: want 4s (age exactly 1h), got %v", h[1])
	}
	if h[23] != 8*time.Second {
		t.Errorf("hour 23: want 8s (age 23h59m59s), got %v", h[23])
	}
	var total time.Duration
	for _, d := range h {
		total += d
	}
	if total != 15*time.Second {
		t.Errorf("total: want 15s (24h-old and future-dated packets left out), got %v", total)
	}
}

func TestRelayAirtimeHours_Percents(t *testing.T) {
	var h relayAirtimeHours
	h[0] = 36 * time.Second // 1% of an hour
	h[5] = 18 * time.Second
	avg, peak := h.percents()
	if peak != 1 {
		t.Errorf("busiest hour: want 1%% (36s of 3600s), got %v", peak)
	}
	if want := airtimePct(54*time.Second, 24*time.Hour); avg != want {
		t.Errorf("24h average: want %v (54s of 86400s), got %v", want, avg)
	}
	var empty relayAirtimeHours
	if a, p := empty.percents(); a != 0 || p != 0 {
		t.Errorf("no airtime: want 0/0, got %v/%v", a, p)
	}
}

func estEntry(now time.Time, age time.Duration, pt int, airtime time.Duration) relayEntry {
	ts := now.Add(-age)
	return relayEntry{ts: ts.Format(time.RFC3339), pt: pt, rt: routeTypeFlood, parsed: true, t: ts, valid: true, airtime: airtime}
}

func TestComputeRelayInfo_EstAirtime(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	selfAdvert := estEntry(now, 2*time.Hour, payloadTypeAdvert, 100*time.Second)
	selfAdvert.self = true
	entries := []relayEntry{
		estEntry(now, 30*time.Minute, 1, 10*time.Second),                          // relayed message
		estEntry(now, 2*time.Hour+time.Minute, payloadTypeAdvert, 20*time.Second), // relayed advert of another node
		selfAdvert, // the repeater's own advert: not relaying
		estEntry(now, 25*time.Hour, 1, 50*time.Second), // older than 24 h
		{ts: "not a time", pt: 1, airtime: 70 * time.Second},
	}
	info := computeRelayInfoFromEntriesAt(entries, 24, now)
	if want := airtimePct(20*time.Second, time.Hour); info.EstAirtimePeakPct != want {
		t.Errorf("busiest hour: want %v (the 20s relayed advert), got %v", want, info.EstAirtimePeakPct)
	}
	if want := airtimePct(30*time.Second, 24*time.Hour); info.EstAirtimePct24h != want {
		t.Errorf("24h average: want %v (10s + 20s), got %v", want, info.EstAirtimePct24h)
	}
	if info.RelayCount24h != 1 {
		t.Errorf("relay counts still leave adverts out: want RelayCount24h=1, got %d", info.RelayCount24h)
	}
}

func TestComputeRelayInfo_EstAirtimeAdvertsOnly(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	info := computeRelayInfoFromEntriesAt([]relayEntry{estEntry(now, 10*time.Minute, payloadTypeAdvert, 36*time.Second)}, 24, now)
	if info.EstAirtimePeakPct != 1 {
		t.Errorf("a repeater that only relayed adverts still has airtime: want 1%%, got %v", info.EstAirtimePeakPct)
	}
	if info.LastRelayed != "" || info.RelayCount24h != 0 {
		t.Errorf("relay activity still ignores adverts: LastRelayed=%q RelayCount24h=%d", info.LastRelayed, info.RelayCount24h)
	}
}

func TestIsSelfAdvert(t *testing.T) {
	cases := []struct {
		origin, key string
		want        bool
	}{
		{"aabbccdd11223344", "aabbccdd11223344", true},
		{"aabbccdd11223344", "aa", true}, // raw hop prefix of the sender
		{"aabbccdd11223344", "bb", false},
		{"", "aa", false}, // not an advert
		{"aabbccdd11223344", "", false},
	}
	for _, c := range cases {
		if got := isSelfAdvert(c.origin, c.key); got != c.want {
			t.Errorf("isSelfAdvert(%q, %q) = %v, want %v", c.origin, c.key, got, c.want)
		}
	}
}

// The per-node lookup and the bulk /api/nodes map must agree, and both must
// count a relayed advert of another node but not the repeater's own advert.
func TestRepeaterRelayInfo_EstAirtime_SingleAndBulk(t *testing.T) {
	db := setupCapabilityTestDB(t)
	defer db.conn.Close()
	pubkey := "aabbccdd11223344"
	db.conn.Exec("INSERT INTO nodes (public_key, name, role, last_seen) VALUES (?, ?, ?, ?)",
		pubkey, "RepAirtime", "repeater", recentTS(1))
	store := NewPacketStore(db, nil)

	msg, adv, flood := 1, payloadTypeAdvert, routeTypeFlood
	long := "01" + strings.Repeat("ab", 60) // header byte, then a 60-byte body
	txs := []*StoreTx{
		{Hash: "est-msg", RawHex: long, PayloadType: &msg, RouteType: &flood, PathJSON: `["aa"]`, FirstSeen: recentTS(0)},
		{Hash: "est-adv-other", RawHex: "1100aa", PayloadType: &adv, RouteType: &flood, PathJSON: `["aa"]`, FirstSeen: recentTS(0),
			DecodedJSON: `{"type":"ADVERT","pubKey":"ffee000000000000"}`},
		{Hash: "est-adv-self", RawHex: "1100aabbccdd", PayloadType: &adv, RouteType: &flood, PathJSON: `["aa"]`, FirstSeen: recentTS(0),
			DecodedJSON: `{"type":"ADVERT","pubKey":"AABBCCDD11223344"}`},
	}
	store.mu.Lock()
	for _, tx := range txs {
		tx.ID = len(store.packets) + 1
		store.packets = append(store.packets, tx)
		store.byHash[tx.Hash] = tx
		store.byTxID[tx.ID] = tx
		store.byPathHop[pubkey] = append(store.byPathHop[pubkey], tx)
	}
	store.mu.Unlock()

	preset := store.resolveLoRaPreset()
	relayed := lora.TimeOnAir(len(long)/2, preset) + lora.TimeOnAir(3, preset)
	wantPeak := airtimePct(relayed, time.Hour)
	wantAvg := airtimePct(relayed, 24*time.Hour)
	if wantPeak == 0 {
		t.Fatalf("fixture airtime rounds to 0%%; make the packets longer")
	}

	single := store.GetRepeaterRelayInfo(pubkey, 24)
	bulk := store.computeRepeaterRelayInfoMap(24)[pubkey]
	for name, info := range map[string]RepeaterRelayInfo{"single": single, "bulk": bulk} {
		if info.EstAirtimePeakPct != wantPeak {
			t.Errorf("%s: busiest hour want %v (message + other node's advert, not own advert), got %v", name, wantPeak, info.EstAirtimePeakPct)
		}
		if info.EstAirtimePct24h != wantAvg {
			t.Errorf("%s: 24h average want %v, got %v", name, wantAvg, info.EstAirtimePct24h)
		}
		if info.RelayCount24h != 1 {
			t.Errorf("%s: RelayCount24h want 1 (adverts excluded), got %d", name, info.RelayCount24h)
		}
	}
}

// /api/nodes and /api/nodes/{pubkey} carry the estimate on repeater rows.
func TestEstAirtime_NodesAPI(t *testing.T) {
	db := setupCapabilityTestDB(t)
	defer db.conn.Close()
	// handleNodes/db.GetNodes selects a foreign_advert column not in the
	// minimal capability-test schema.
	if _, err := db.conn.Exec(`ALTER TABLE nodes ADD COLUMN foreign_advert INTEGER DEFAULT 0`); err != nil {
		t.Fatal(err)
	}
	pubkey := "aa" + strings.Repeat("0", 62)
	recent := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	if _, err := db.conn.Exec(`INSERT INTO nodes (public_key, name, role, last_seen, first_seen, advert_count)
		VALUES (?, 'RepApi', 'repeater', ?, ?, 1)`, pubkey, recent, recent); err != nil {
		t.Fatal(err)
	}
	store := NewPacketStore(db, nil)
	msg, flood := 1, routeTypeFlood
	tx := &StoreTx{Hash: "est-api", RawHex: "01" + strings.Repeat("cd", 100), PayloadType: &msg, RouteType: &flood,
		PathJSON: `["aa"]`, FirstSeen: recentTS(0)}
	store.mu.Lock()
	tx.ID = len(store.packets) + 1
	store.packets = append(store.packets, tx)
	store.byHash[tx.Hash] = tx
	store.byTxID[tx.ID] = tx
	store.byPathHop[pubkey] = append(store.byPathHop[pubkey], tx)
	store.mu.Unlock()

	srv := NewServer(db, &Config{Port: 3000}, NewHub())
	srv.store = store
	router := mux.NewRouter()
	srv.RegisterRoutes(router)
	get := func(path string) map[string]interface{} {
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != 200 {
			t.Fatalf("%s: status %d body=%s", path, rr.Code, rr.Body.String())
		}
		var out map[string]interface{}
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: decode: %v", path, err)
		}
		return out
	}
	check := func(where string, n map[string]interface{}) {
		peak, ok := n["est_airtime_peak_pct"].(float64)
		if !ok || peak <= 0 {
			t.Errorf("%s: est_airtime_peak_pct want > 0, got %v", where, n["est_airtime_peak_pct"])
		}
		avg, ok := n["est_airtime_pct_24h"].(float64)
		if !ok || avg <= 0 || avg >= peak {
			t.Errorf("%s: est_airtime_pct_24h want between 0 and the peak %v, got %v", where, peak, n["est_airtime_pct_24h"])
		}
	}

	var found bool
	for _, raw := range get("/api/nodes?limit=100")["nodes"].([]interface{}) {
		n := raw.(map[string]interface{})
		if n["public_key"] == pubkey {
			found = true
			check("/api/nodes", n)
		}
	}
	if !found {
		t.Fatalf("repeater missing from /api/nodes")
	}
	detail := get("/api/nodes/" + pubkey)
	if n, ok := detail["node"].(map[string]interface{}); ok {
		check("/api/nodes/{pubkey}", n)
	} else {
		check("/api/nodes/{pubkey}", detail)
	}
}

func TestAirtimePct(t *testing.T) {
	if got := airtimePct(36*time.Second, time.Hour); got != 1 {
		t.Errorf("36s of an hour: want 1%%, got %v", got)
	}
	// One second a day still shows: 1/86400 = 0.00116% -> 0.0012.
	if got := airtimePct(time.Second, 24*time.Hour); got != 0.0012 {
		t.Errorf("1s of 24h: want 0.0012%%, got %v", got)
	}
}
