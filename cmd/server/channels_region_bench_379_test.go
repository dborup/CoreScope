package main

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// Portable benchmark: the same file runs unchanged on the parent revision.
// Cold means application-list-cache miss; SQLite/OS cold-disk performance is
// NOT measured by this in-memory synthetic fixture. Every row remains in SQL.
func channel379BenchmarkDB(b *testing.B, v3 bool) *DB {
	b.Helper()
	db := pinTestDB(b, v3, true)
	tx, err := db.conn.Begin()
	if err != nil {
		b.Fatal(err)
	}
	for _, o := range []struct{ id, region string }{{"bench379-broad", "AAR"}, {"bench379-selective", "ZZZ"}, {"bench379-other", "CPH"}} {
		if _, err := tx.Exec(`INSERT INTO observers(id,name,iata) VALUES (?, ?, ?)`, o.id, o.id, o.region); err != nil {
			b.Fatal(err)
		}
	}
	txs, err := tx.Prepare(`INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type,channel_hash,decoded_json) VALUES (?,'0500',?,?,5,?,?)`)
	if err != nil {
		b.Fatal(err)
	}
	observerColumn := "observer_id"
	if v3 {
		observerColumn = "observer_idx"
	}
	obs, err := tx.Prepare(`INSERT INTO observations(transmission_id,` + observerColumn + `,path_json,timestamp) VALUES (?,?,'[]',?)`)
	if err != nil {
		b.Fatal(err)
	}
	rng := rand.New(rand.NewSource(379))
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= 30000; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		channel := fmt.Sprintf("enc_%02X", i%64)
		if i%4 == 0 {
			channel = fmt.Sprintf("#bench379-%d", i%20)
		}
		if _, err := txs.Exec(i, fmt.Sprintf("bench379-%d", i), at.Format(time.RFC3339), channel, `{"text":"Synthetic: benchmark","sender":"Synthetic"}`); err != nil {
			b.Fatal(err)
		}
		observer := interface{}("bench379-broad")
		which := 1
		switch p := rng.Intn(100); {
		case p < 1:
			observer = "bench379-selective"
			which = 2
		case p >= 91:
			observer = "bench379-other"
			which = 3
		}
		if v3 {
			observer = which
		}
		for j := 0; j < 10; j++ {
			if _, err := obs.Exec(i, observer, at.Unix()+int64(j)); err != nil {
				b.Fatal(err)
			}
		}
	}
	txs.Close()
	obs.Close()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return db
}

func BenchmarkChannelRegion379Lists(b *testing.B) {
	for _, v3 := range []bool{true, false} {
		b.Run(fmt.Sprintf("v3=%v", v3), func(b *testing.B) {
			db := channel379BenchmarkDB(b, v3)
			for _, region := range []string{"AAR", "ZZZ"} {
				for _, pinned := range []bool{true, false} {
					b.Run(fmt.Sprintf("region=%s/raw-query/pinned=%v", region, pinned), func(b *testing.B) {
						query := channelsSQL(v3, "?", pinned)
						b.ReportAllocs()
						b.ResetTimer()
						for i := 0; i < b.N; i++ {
							rows, err := db.conn.Query(query, region)
							if err != nil {
								b.Fatal(err)
							}
							for rows.Next() {
								var hash, at, decoded string
								var count int
								if err := rows.Scan(&hash, &count, &at, &decoded); err != nil {
									b.Fatal(err)
								}
							}
							if err := rows.Err(); err != nil {
								b.Fatal(err)
							}
							rows.Close()
						}
					})
				}
				for _, encrypted := range []bool{false, true} {
					b.Run(fmt.Sprintf("region=%s/encrypted=%v", region, encrypted), func(b *testing.B) {
						call := func() error {
							var err error
							if encrypted {
								_, err = db.GetEncryptedChannels(region)
							} else {
								_, err = db.GetChannels(region)
							}
							return err
						}
						b.Run("cold-list-cache", func(b *testing.B) {
							b.ReportAllocs()
							for i := 0; i < b.N; i++ {
								db.channelsCache.reset()
								db.encChannelsCache.reset()
								if err := call(); err != nil {
									b.Fatal(err)
								}
							}
						})
						b.Run("warm-list-cache", func(b *testing.B) {
							if err := call(); err != nil {
								b.Fatal(err)
							}
							b.ReportAllocs()
							b.ResetTimer()
							for i := 0; i < b.N; i++ {
								if err := call(); err != nil {
									b.Fatal(err)
								}
							}
						})
					})
				}
			}
		})
	}
}
