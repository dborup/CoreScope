package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The newest page contains only CONTROL; exclusion must precede pagination.
func TestPacketExclusionsBeforePagination(t *testing.T) {
	for _, mode := range []string{"memory", "database", "fallback"} {
		t.Run(mode, func(t *testing.T) {
			db := setupTestDB(t)
			defer db.Close()
			for i, typ := range []interface{}{nil, 5, 11, 11, 11} {
				_, err := db.conn.Exec(`INSERT INTO transmissions(raw_hex,hash,first_seen,payload_type) VALUES('AA',?,?,?)`, fmt.Sprintf("%016x", i+1), fmt.Sprintf("2026-01-01T00:00:%02dZ", i), typ)
				if err != nil {
					t.Fatal(err)
				}
			}
			srv := NewServer(db, &Config{}, NewHub())
			if mode != "database" {
				srv.store = NewPacketStore(db, nil)
				if err := srv.store.Load(); err != nil {
					t.Fatal(err)
				}
				if !srv.store.WaitIndexesReady(5 * time.Second) {
					t.Fatal("indexes")
				}
				if mode == "fallback" {
					srv.store.oldestLoaded = "2026-01-02"
				}
			}
			for _, grouped := range []bool{false, true} {
				suffix := fmt.Sprintf("&groupByHash=%t", grouped)
				if mode == "fallback" {
					suffix += "&until=2026-01-01T12:00:00"
				}
				check := func(query string, total int, wantHash string) {
					t.Helper()
					w := httptest.NewRecorder()
					srv.handlePackets(w, httptest.NewRequest("GET", "/api/packets?limit=1"+suffix+query, nil))
					if w.Code != 200 {
						t.Fatalf("%s: %d %s", query, w.Code, w.Body.String())
					}
					var got PacketResult
					if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
						t.Fatal(err)
					}
					if got.Total != total {
						t.Fatalf("%s grouped=%t total=%d want %d", query, grouped, got.Total, total)
					}
					if wantHash == "" {
						if len(got.Packets) != 0 {
							t.Fatal("expected empty")
						}
						return
					}
					if len(got.Packets) != 1 || got.Packets[0]["hash"] != wantHash {
						t.Fatalf("%s packets=%v want %s", query, got.Packets, wantHash)
					}
				}
				check("", 5, "0000000000000005")
				check("&excludeTypes=11", 2, "0000000000000002")
				check("&excludeTypes=11&offset=1", 2, "0000000000000001")
				check("&excludeTypes=11,5", 1, "0000000000000001")
				check("&excludeTypes=11&type=11", 0, "")
				check("&excludeTypes=11&type=5", 1, "0000000000000002")
				check("&excludeTypes=11&hash=0000000000000005", 0, "")
				check("", 5, "0000000000000005") // grouped cache must not leak exclusions
			}
		})
	}
}

func TestPacketExclusionMaskAndIndexes(t *testing.T) {
	left, err := parsePacketTypeExclusions(url.Values{"excludeTypes": {"11,5,11"}})
	if err != nil {
		t.Fatal(err)
	}
	right, err := parsePacketTypeExclusions(url.Values{"excludeTypes": {"5,11"}})
	if err != nil || left != right {
		t.Fatal("equivalent lists must share cache identity")
	}
	all, err := parsePacketTypeExclusions(url.Values{"excludeTypes": {"0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15"}})
	if err != nil {
		t.Fatal(err)
	}
	for typ := 0; typ < 16; typ++ {
		if !all.excludes(&typ) {
			t.Errorf("wire type %d not excluded", typ)
		}
	}
	unknown := 99
	if all.excludes(nil) || all.excludes(&unknown) {
		t.Fatal("unknown types must remain")
	}
	db := setupTestDB(t)
	defer db.Close()
	store := NewPacketStore(db, nil)
	pk := strings.Repeat("a", 64)
	for i, typ := range []int{5, 11} {
		typ := typ
		tx := &StoreTx{ID: i + 1, Hash: fmt.Sprintf("%016x", i+1), FirstSeen: "2026-01-01", PayloadType: &typ, Observations: []*StoreObs{{ObserverID: "test-observer"}}}
		store.packets = append(store.packets, tx)
		store.byHash[tx.Hash] = tx
		store.byNode[pk] = append(store.byNode[pk], tx)
	}
	for _, query := range []PacketQuery{{Hash: "0000000000000002"}, {Observer: "test-observer"}, {Node: pk}} {
		query.ExcludeTypes = 1 << 11
		got := store.filterPackets(query)
		want := 1
		if query.Hash != "" {
			want = 0
		}
		if len(got) != want {
			t.Errorf("index query %+v yielded %d rows want %d", query, len(got), want)
		}
	}
}

// Both cases include the complete 30K-row filter/sort/page path. Grouped
// cache is deliberately invalidated per iteration to measure cold requests.
func BenchmarkPacketExclusions30K(b *testing.B) {
	store := NewPacketStore(nil, nil)
	for i := 0; i < 30000; i++ {
		typ := 5
		if i%5 != 0 {
			typ = 11
		}
		stamp := fmt.Sprintf("2026-01-01T%08d", i)
		store.packets = append(store.packets, &StoreTx{ID: i + 1, Hash: fmt.Sprintf("%016x", i+1), FirstSeen: stamp, LatestSeen: stamp, PayloadType: &typ, PathJSON: "[]"})
	}
	for _, grouped := range []bool{false, true} {
		for _, mask := range []packetTypeExclusions{0, 1 << 5, 1 << 11} {
			b.Run(fmt.Sprintf("grouped=%t/exclusions=%d", grouped, mask), func(b *testing.B) {
				q := PacketQuery{Limit: 50, ExcludeTypes: mask}
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					var result *PacketResult
					if grouped {
						store.groupedCacheTxs = nil
						result = store.QueryGroupedPackets(q)
					} else {
						result = store.QueryPackets(q)
					}
					if len(result.Packets) != 50 {
						b.Fatal("page size")
					}
				}
			})
		}
	}
}

func TestPacketExclusionsValidation(t *testing.T) {
	srv, _ := setupTestServer(t)
	defer srv.db.Close()
	for _, query := range []string{"-1", "16", "CONTROL", "11,", "1,,2", "1.5", "%2B1", "1&excludeTypes=2", "0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0", "11&nodes=abcd", strings.Repeat("1", 65)} {
		w := httptest.NewRecorder()
		srv.handlePackets(w, httptest.NewRequest("GET", "/api/packets?excludeTypes="+query, nil))
		if w.Code != 400 {
			t.Errorf("%s: got %d want400", query, w.Code)
		}
	}
	for _, query := range []string{"", "0", "15", "11,11", "%2011%20,5", "0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15"} {
		w := httptest.NewRecorder()
		srv.handlePackets(w, httptest.NewRequest("GET", "/api/packets?excludeTypes="+query, nil))
		if w.Code != 200 {
			t.Errorf("%s: got %d want200", query, w.Code)
		}
	}
}
