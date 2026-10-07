package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// BenchmarkGroupedPackets165 measures the default packets-page request
// (/api/packets?groupByHash=true, desktop limit 50000) on 30K transmissions
// with two observations each and a 3-hop resolved_path, the shape #165
// asked to be checked before the grouped rows carry resolved_path. It
// reports the JSON size of the page as json-bytes, and gzipped (the server
// compresses responses, compress.go) as gzip-bytes.
//
//	go test -run '^$' -bench GroupedPackets165 -benchtime 20x .
func BenchmarkGroupedPackets165(b *testing.B) {
	const nTx = 30000
	db := setupTestDB(b)
	defer db.Close()
	now := time.Now().UTC()
	ts := now.Format(time.RFC3339)
	epoch := now.Add(-time.Minute).Unix()
	tx, err := db.conn.Begin()
	if err != nil {
		b.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		tx.Exec(`INSERT INTO observers (id, name, iata, last_seen, first_seen, packet_count) VALUES (?, ?, 'SJC', ?, ?, 1)`,
			fmt.Sprintf("OBS%02d", i), fmt.Sprintf("O%d", i), ts, ts)
	}
	// Pubkeys look random, as real ones do, so gzip-bytes is not flattered.
	pk := func(i, h int) string {
		sum := sha256.Sum256([]byte(fmt.Sprint(i, ":", h)))
		return fmt.Sprintf("%02x", (i+h)%256) + hex.EncodeToString(sum[:])[2:]
	}
	for i := 1; i <= nTx; i++ {
		tx.Exec(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json) VALUES (?, ?, ?, ?, 1, 4, '{}')`,
			i, "11223344556677889900aabbccddeeff", fmt.Sprintf("%016x", i), ts)
		path := fmt.Sprintf(`["%02x","%02x","%02x"]`, (i+0)%256, (i+1)%256, (i+2)%256)
		rp := fmt.Sprintf(`["%s","%s",null]`, pk(i, 0), pk(i, 1))
		tx.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp, resolved_path) VALUES (?, 1, 5, -90, ?, ?, ?)`, i, path, epoch-int64(i), rp)
		tx.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp, resolved_path) VALUES (?, 2, 5, -90, ?, ?, ?)`,
			i, fmt.Sprintf(`["%02x"]`, (i+2)%256), epoch-int64(i), fmt.Sprintf(`["%s"]`, pk(i, 2)))
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	store := NewPacketStore(db, nil)
	if err := store.Load(); err != nil {
		b.Fatal(err)
	}
	q := PacketQuery{Limit: 50000}
	r := store.QueryGroupedPackets(q)
	if r.Total != nTx {
		b.Fatalf("total = %d, want %d", r.Total, nTx)
	}
	body, _ := json.Marshal(r)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.QueryGroupedPackets(q)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(body)
	zw.Close()
	b.ReportMetric(float64(len(body)), "json-bytes")
	b.ReportMetric(float64(gz.Len()), "gzip-bytes")
}
