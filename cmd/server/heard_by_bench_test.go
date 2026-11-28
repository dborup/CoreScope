package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// heardByBenchStore builds the #76 perf fixture: nodeCount relay-capable
// nodes (unique 2-byte, colliding 1-byte prefixes) and txCount transmissions
// with obsPerTx observations each, one in three an ADVERT, one in seven on a
// direct route, paths of 0..3 two-byte hops. Every transmission is indexed
// under its originator and its relays, as resolved_path indexing does.
func heardByBenchStore(tb testing.TB, nodeCount, txCount, obsPerTx int) (*PacketStore, []string) {
	tb.Helper()
	db := setupTestDB(tb)
	keys := make([]string, nodeCount)
	seen := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.conn.Exec("BEGIN"); err != nil {
		tb.Fatal(err)
	}
	for i := range keys {
		keys[i] = fmt.Sprintf("%04x", i*32+1) + strings.Repeat("ab", 30)
		if _, err := db.conn.Exec("INSERT INTO nodes (public_key, name, role, last_seen) VALUES (?, ?, 'repeater', ?)", keys[i], fmt.Sprintf("Node %d", i), seen); err != nil {
			tb.Fatal(err)
		}
	}
	if _, err := db.conn.Exec("COMMIT"); err != nil {
		tb.Fatal(err)
	}
	store := NewPacketStore(db, nil)
	base := time.Now().UTC().Add(-12 * time.Hour)
	obsID := 0
	for t := 0; t < txCount; t++ {
		origin := keys[t%nodeCount]
		pt, rt := []int{PayloadADVERT, PayloadGRP_TXT, PayloadTXT_MSG}[t%3], RouteFlood
		if t%7 == 0 {
			rt = RouteDirect
		}
		field := "srcPubKey"
		if pt == PayloadADVERT {
			field = "pubKey"
		}
		ts := base.Add(time.Duration(t) * time.Second).Format(time.RFC3339)
		tx := &StoreTx{ID: t + 1, Hash: fmt.Sprintf("bench%06d", t), FirstSeen: ts, LatestSeen: ts,
			PayloadType: &pt, RouteType: &rt, DecodedJSON: `{"` + field + `":"` + origin + `"}`}
		store.addToByNode(tx, origin)
		for k := 0; k < obsPerTx; k++ {
			hops := make([]string, k%4)
			for h := range hops {
				relay := keys[(t+k*7+h*13+1)%nodeCount]
				hops[h] = `"` + strings.ToUpper(relay[:4]) + `"`
				store.addToByNode(tx, relay)
			}
			snr, rssi := float64(k)-5, float64(-100+k)
			obsID++
			tx.Observations = append(tx.Observations, &StoreObs{ID: obsID, TransmissionID: tx.ID,
				ObserverID: fmt.Sprintf("obs%02d", (t+k)%50), ObserverName: fmt.Sprintf("Observer %d", (t+k)%50),
				SNR: &snr, RSSI: &rssi, PathJSON: "[" + strings.Join(hops, ",") + "]", Timestamp: ts})
		}
		best := tx.Observations[len(tx.Observations)-1]
		tx.ObservationCount, tx.ObserverID, tx.ObserverName = len(tx.Observations), best.ObserverID, best.ObserverName
		tx.SNR, tx.RSSI, tx.PathJSON = best.SNR, best.RSSI, best.PathJSON
		store.packets = append(store.packets, tx)
		store.byHash[tx.Hash], store.byTxID[tx.ID] = tx, tx
	}
	return store, keys
}

// BenchmarkHeardByBulkHealth30K is the #76 perf proof for /api/nodes/bulk-health
// at its maximum limit (200 nodes) over 30K transmissions / 150K observations.
func BenchmarkHeardByBulkHealth30K(b *testing.B) {
	store, _ := heardByBenchStore(b, 2000, 30000, 5)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if rows := store.GetBulkHealth(200, "", ""); len(rows) != 200 {
			b.Fatalf("bulk rows = %d", len(rows))
		}
	}
}

// BenchmarkHeardByNodeHealth30K measures one node page health request on the
// same fixture.
func BenchmarkHeardByNodeHealth30K(b *testing.B) {
	store, keys := heardByBenchStore(b, 2000, 30000, 5)
	now := time.Now().UTC()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if h, err := store.GetNodeHealth(keys[0], now); err != nil || h == nil {
			b.Fatalf("health: %v %v", h, err)
		}
	}
}

// BenchmarkHeardByNodeHealthHub30K is the worst case for one request: a hub
// indexed under every transmission of the fixture (30K tx, 150K observations).
func BenchmarkHeardByNodeHealthHub30K(b *testing.B) {
	store, keys := heardByBenchStore(b, 2000, 30000, 5)
	for _, tx := range store.packets {
		store.addToByNode(tx, keys[0])
	}
	now := time.Now().UTC()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if h, err := store.GetNodeHealth(keys[0], now); err != nil || h == nil {
			b.Fatalf("health: %v %v", h, err)
		}
	}
}
