package main

import (
	"fmt"
	"testing"
	"time"
)

// This benchmark intentionally uses only the pre-existing API so the same
// source can run unchanged on the parent revision for a before/after result.
func BenchmarkNodeAnalyticsCoverage485Endpoints(b *testing.B) {
	for _, n := range []int{1000, 30000} {
		for _, full := range []bool{false, true} {
			b.Run(fmt.Sprintf("packets=%d/full=%t", n, full), func(b *testing.B) {
				db := setupTestDB(b)
				defer db.Close()
				now := time.Now().UTC().Truncate(time.Second)
				pk := "c048500000000000000000000000000000000000000000000000000000000000"
				if _, err := db.conn.Exec(`INSERT INTO nodes (public_key,name,role) VALUES (?, 'Coverage bench','repeater')`, pk); err != nil {
					b.Fatal(err)
				}
				tx, err := db.conn.Begin()
				if err != nil {
					b.Fatal(err)
				}
				stmt, err := tx.Prepare(`INSERT INTO transmissions (id,raw_hex,hash,first_seen,payload_type,decoded_json) VALUES (?,'1000',?,?,4,?)`)
				if err != nil {
					b.Fatal(err)
				}
				obs, err := tx.Prepare(`INSERT INTO observations (transmission_id,path_json,timestamp) VALUES (?,'[]',?)`)
				if err != nil {
					b.Fatal(err)
				}
				for i := 1; i <= n; i++ {
					at := now.Add(-24*time.Hour + time.Duration(i)*time.Second)
					if _, err := stmt.Exec(i, fmt.Sprintf("coverage-bench-%d", i), at.Format(time.RFC3339), `{"pubKey":"`+pk+`"}`); err != nil {
						b.Fatal(err)
					}
					if _, err := obs.Exec(i, at.Unix()); err != nil {
						b.Fatal(err)
					}
				}
				stmt.Close()
				obs.Close()
				if err := tx.Commit(); err != nil {
					b.Fatal(err)
				}
				s := NewPacketStore(db, nil)
				if err := s.Load(); err != nil {
					b.Fatal(err)
				}
				if !s.WaitIndexesReady(5 * time.Second) {
					b.Fatal("indexes not ready")
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if full {
						if _, err := s.getNodeAnalyticsAt(pk, 30, now); err != nil {
							b.Fatal(err)
						}
					} else {
						if _, err := s.getNodeAnalyticsSummaryAt(pk, 30, now); err != nil {
							b.Fatal(err)
						}
					}
				}
			})
		}
	}
}
