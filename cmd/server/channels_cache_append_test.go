package main

// Regression test for a data race in handleChannels (cmd/server/routes.go):
// both the DB path (db.GetChannels) and the in-memory store path
// (store.GetChannels) cache their result slice behind a mutex with a TTL
// and return that same cached slice to every caller — no defensive copy.
//
// When ?includeEncrypted=true, handleChannels does:
//
//	channels = append(channels, encrypted...)
//
// where `channels` IS the cached slice. If it still has spare capacity
// (cap(channels) > len(channels)), append writes into that spare capacity
// IN PLACE, mutating the shared backing array that other concurrent
// callers — and the cache itself — are also holding a reference to. That
// is a real data race, detectable with `go test -race`, and silently
// corrupts memory that a later, unrelated read of the cache's full
// capacity would observe.
//
// These tests seed data that guarantees the cached slice has spare
// capacity, then prove the corruption directly (by comparing the cache's
// full backing array before and after concurrent includeEncrypted=true
// requests) and via the race detector.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"
)

// probeForSpareCapacity repeatedly resets the cache and fetches it,
// padding with extra rows between attempts, until the returned slice has
// spare capacity (cap > len) — the precondition for append-into-shared-
// backing-array corruption. SQLite/Go's growth strategy doesn't guarantee
// spare capacity on every call, so this is a real precondition check, not
// a flake-hider: it fails loudly if spare capacity can't be obtained.
func probeForSpareCapacity(t *testing.T, maxAttempts int, resetCache func(), fetch func() []map[string]interface{}, pad func(round int)) []map[string]interface{} {
	t.Helper()
	var res []map[string]interface{}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resetCache()
		res = fetch()
		if cap(res) > len(res) {
			return res
		}
		if attempt == maxAttempts {
			t.Fatalf("could not obtain a cached channels slice with spare capacity after %d attempts (cap=%d len=%d) — precondition for the append-into-spare-capacity race not met", maxAttempts, cap(res), len(res))
		}
		pad(attempt)
	}
	return res
}

// deepCopyChannelSlice makes an independent copy of a []map[string]interface{}
// (including elements past len, up to cap, which may be nil zero values) so
// later mutation of the original backing array can be detected.
func deepCopyChannelSlice(full []map[string]interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, len(full))
	for i, m := range full {
		if m == nil {
			continue
		}
		cp := make(map[string]interface{}, len(m))
		for k, v := range m {
			cp[k] = v
		}
		out[i] = cp
	}
	return out
}

// insertChannelTx inserts one GRP_TXT (payload_type=5) transmission row
// directly, for db.GetChannels()/db.GetEncryptedChannels() (region="" path,
// which queries the transmissions table with no join, so no observers/
// observations rows are required).
func insertChannelTx(t *testing.T, db *DB, hash, channelHash, decodedJSON string, firstSeen time.Time) {
	t.Helper()
	_, err := db.conn.Exec(
		`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json, channel_hash)
		 VALUES (?, ?, ?, 1, 5, ?, ?)`,
		"AABB", hash, firstSeen.UTC().Format(time.RFC3339), decodedJSON, channelHash,
	)
	if err != nil {
		t.Fatalf("insertChannelTx(%s): %v", hash, err)
	}
}

func resetDBChannelsCache(db *DB) {
	db.channelsCacheMu.Lock()
	db.channelsCacheRes = nil
	db.channelsCacheKey = ""
	db.channelsCacheExp = time.Time{}
	db.channelsCacheMu.Unlock()
}

// TestChannelsCacheAppend_DBPath drives GET /api/channels through the real
// HTTP handler (db.GetChannels backing) and asserts the cached slice's
// backing array is untouched by concurrent includeEncrypted=true requests.
func TestChannelsCacheAppend_DBPath(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	now := time.Now()
	// One regular (decodable) channel and one encrypted channel (channel_hash
	// LIKE 'enc_%') — without the encrypted row, includeEncrypted=true never
	// reaches the buggy append call at all.
	insertChannelTx(t, db, "txhash-regular-1", "#test", `{"type":"CHAN","text":"Alice: hello","sender":"Alice"}`, now)
	insertChannelTx(t, db, "txhash-enc-1", "enc_AB", `{}`, now)

	padRound := 0
	padDB := func(int) {
		padRound++
		hash := fmt.Sprintf("txhash-pad-%d", padRound)
		chHash := fmt.Sprintf("#pad%d", padRound)
		insertChannelTx(t, db, hash, chHash, `{"type":"CHAN","text":"Pad: msg","sender":"Pad"}`, now)
	}
	fetchDB := func() []map[string]interface{} {
		res, err := db.GetChannels("")
		if err != nil {
			t.Fatalf("db.GetChannels: %v", err)
		}
		return res
	}

	cfg := &Config{Port: 3000}
	srv := NewServer(db, cfg, NewHub())
	router := setupTestRouter(srv)
	_ = router

	t.Run("CorruptsSpareCapacity", func(t *testing.T) {
		cached := probeForSpareCapacity(t, 8, func() { resetDBChannelsCache(db) }, fetchDB, padDB)
		snapshot := deepCopyChannelSlice(cached[:cap(cached)])

		for i := 0; i < 3; i++ {
			req := httptest.NewRequest("GET", "/api/channels", nil)
			w := httptest.NewRecorder()
			srv.router.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("GET /api/channels: status %d body=%s", w.Code, w.Body.String())
			}
		}
		for i := 0; i < 3; i++ {
			req := httptest.NewRequest("GET", "/api/channels?includeEncrypted=true", nil)
			w := httptest.NewRecorder()
			srv.router.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("GET /api/channels?includeEncrypted=true: status %d body=%s", w.Code, w.Body.String())
			}
		}

		db.channelsCacheMu.Lock()
		final := db.channelsCacheRes
		db.channelsCacheMu.Unlock()
		if cap(final) != cap(cached) || len(final) != len(cached) {
			t.Fatalf("cache slice header changed unexpectedly: before cap=%d len=%d, after cap=%d len=%d (cache may have expired/refreshed — this test assumes it didn't)", cap(cached), len(cached), cap(final), len(final))
		}
		fullFinal := final[:cap(final)]
		if !reflect.DeepEqual(snapshot, fullFinal) {
			t.Errorf("cached channels slice's backing array was mutated by includeEncrypted=true append (shared spare capacity written in place):\nbefore: %+v\nafter:  %+v", snapshot, fullFinal)
		}
	})

	t.Run("ConcurrentAppendRace", func(t *testing.T) {
		// Pre-warm the cache with spare capacity so every goroutine below
		// races on the SAME cached backing array.
		probeForSpareCapacity(t, 8, func() { resetDBChannelsCache(db) }, fetchDB, padDB)

		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := httptest.NewRequest("GET", "/api/channels?includeEncrypted=true", nil)
				w := httptest.NewRecorder()
				srv.router.ServeHTTP(w, req)
			}()
		}
		wg.Wait()
	})
}

// makeEncGrpTx builds a GRP_TXT store transmission shaped so
// PacketStore.GetEncryptedChannels (decryptionStatus == "no_key") picks it
// up, independent of the CHAN-shaped helpers in channel_analytics_test.go.
func makeEncGrpTx(id int, channelHashHex string, firstSeen time.Time) *StoreTx {
	decoded := map[string]interface{}{
		"type":             "GRP_TXT",
		"channelHashHex":   channelHashHex,
		"decryptionStatus": "no_key",
	}
	b, _ := json.Marshal(decoded)
	pt := 5
	return &StoreTx{
		ID:          id,
		DecodedJSON: string(b),
		FirstSeen:   firstSeen.UTC().Format(time.RFC3339),
		PayloadType: &pt,
	}
}

func resetStoreChannelsCache(store *PacketStore) {
	store.channelsCacheMu.Lock()
	store.channelsCacheRes = nil
	store.channelsCacheKey = ""
	store.channelsCacheExp = time.Time{}
	store.channelsCacheMu.Unlock()
}

// TestChannelsCacheAppend_StorePath is the in-memory-store analog of
// TestChannelsCacheAppend_DBPath, driving GET /api/channels through the
// real HTTP handler with s.db == nil so handleChannels takes the
// s.store.GetChannels branch.
func TestChannelsCacheAppend_StorePath(t *testing.T) {
	now := time.Now()
	packets := []*StoreTx{
		makeGrpTx(1, "general", "Alice: hello", "Alice"),
		makeEncGrpTx(2, "AB", now),
	}
	store := newChannelTestStore(packets)

	// Padding grows the store's in-memory packet set with new hashtag
	// mentions so GetChannels' "discovered channels" append path grows the
	// result slice past its initial capacity (issue #688 discovery logic;
	// see store.go GetChannels). Each round adds one new unique hashtag
	// mention so the discovered-channel count strictly increases.
	padRound := 0
	padStore := func(int) {
		padRound++
		tag := fmt.Sprintf("#pad%d", padRound)
		tx := makeGrpTx(1, "general", fmt.Sprintf("Bob: mention %s", tag), "Bob")
		store.mu.Lock()
		store.packets = append(store.packets, tx)
		store.byPayloadType[5] = append(store.byPayloadType[5], tx)
		store.mu.Unlock()
	}
	fetchStore := func() []map[string]interface{} {
		return store.GetChannels("")
	}

	cfg := &Config{Port: 3000}
	srv := NewServer(nil, cfg, NewHub()) // s.db == nil forces the store branch
	srv.store = store
	router := setupTestRouter(srv)
	_ = router

	t.Run("CorruptsSpareCapacity", func(t *testing.T) {
		cached := probeForSpareCapacity(t, 8, func() { resetStoreChannelsCache(store) }, fetchStore, padStore)
		snapshot := deepCopyChannelSlice(cached[:cap(cached)])

		for i := 0; i < 3; i++ {
			req := httptest.NewRequest("GET", "/api/channels", nil)
			w := httptest.NewRecorder()
			srv.router.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("GET /api/channels: status %d body=%s", w.Code, w.Body.String())
			}
		}
		for i := 0; i < 3; i++ {
			req := httptest.NewRequest("GET", "/api/channels?includeEncrypted=true", nil)
			w := httptest.NewRecorder()
			srv.router.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("GET /api/channels?includeEncrypted=true: status %d body=%s", w.Code, w.Body.String())
			}
		}

		store.channelsCacheMu.Lock()
		final := store.channelsCacheRes
		store.channelsCacheMu.Unlock()
		if cap(final) != cap(cached) || len(final) != len(cached) {
			t.Fatalf("cache slice header changed unexpectedly: before cap=%d len=%d, after cap=%d len=%d (cache may have expired/refreshed — this test assumes it didn't)", cap(cached), len(cached), cap(final), len(final))
		}
		fullFinal := final[:cap(final)]
		if !reflect.DeepEqual(snapshot, fullFinal) {
			t.Errorf("cached channels slice's backing array was mutated by includeEncrypted=true append (shared spare capacity written in place):\nbefore: %+v\nafter:  %+v", snapshot, fullFinal)
		}
	})

	t.Run("ConcurrentAppendRace", func(t *testing.T) {
		probeForSpareCapacity(t, 8, func() { resetStoreChannelsCache(store) }, fetchStore, padStore)

		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := httptest.NewRequest("GET", "/api/channels?includeEncrypted=true", nil)
				w := httptest.NewRecorder()
				srv.router.ServeHTTP(w, req)
			}()
		}
		wg.Wait()
	})
}
