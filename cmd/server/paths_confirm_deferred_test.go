package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// /api/nodes/{pk}/paths and /hop_analytics used to confirm every hash-index
// candidate with confirmResolvedPathContains: one SQL query per candidate
// transmission, each scanning all observation rows of that tx. For a busy
// node that was thousands of sequential queries and the dominant CPU cost of
// the endpoint. Membership is decided from the canonical resolved_path
// anyway, so the query is now only issued for candidates that have no
// canonical path. These tests pin both halves: the queries are gone in the
// normal case, and results are unchanged when the index is stale/colliding.

const confirmTestTarget = "aabbccdd11223344" // seeded TestRepeater

// seedConfirmTx inserts a transmission with one observation and returns its
// id. An empty rpJSON stores a NULL resolved_path.
func seedConfirmTx(t *testing.T, srv *Server, hash, pathJSON, rpJSON string) int {
	t.Helper()
	now := time.Now().UTC().Add(-20 * time.Minute)
	if _, err := srv.db.conn.Exec(`INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, decoded_json)
		VALUES ('FF01', ?, ?, 1, 4, '{}')`, hash, now.Format(time.RFC3339)); err != nil {
		t.Fatalf("insert tx %s: %v", hash, err)
	}
	var txID int
	if err := srv.db.conn.QueryRow(`SELECT id FROM transmissions WHERE hash = ?`, hash).Scan(&txID); err != nil {
		t.Fatalf("lookup tx %s: %v", hash, err)
	}
	var rp interface{}
	if rpJSON != "" {
		rp = rpJSON
	}
	if _, err := srv.db.conn.Exec(`INSERT INTO observations (transmission_id, observer_idx, snr, rssi, path_json, timestamp, resolved_path)
		VALUES (?, 1, 10.0, -90, ?, ?, ?)`, txID, pathJSON, now.Unix(), rp); err != nil {
		t.Fatalf("insert obs %s: %v", hash, err)
	}
	return txID
}

// reloadConfirmStore swaps in a freshly loaded store so the seeded rows and
// their resolved-path index entries are visible to the handlers.
func reloadConfirmStore(t *testing.T, srv *Server) *PacketStore {
	t.Helper()
	store := NewPacketStore(srv.db, nil)
	if err := store.Load(); err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if !store.WaitIndexesReady(5 * time.Second) {
		t.Fatal("indexes never became ready")
	}
	srv.store = store
	return store
}

// lruHasTx reports whether any observation of txID has a resolved-path LRU
// entry, i.e. whether a handler read the tx's canonical path.
func lruHasTx(store *PacketStore, txID int) bool {
	store.mu.RLock()
	tx := store.byTxID[txID]
	store.mu.RUnlock()
	if tx == nil {
		return false
	}
	store.lruMu.RLock()
	defer store.lruMu.RUnlock()
	for _, obs := range tx.Observations {
		if _, ok := store.apiResolvedPathLRU[obs.ID]; ok {
			return true
		}
	}
	return false
}

func resetLRU(store *PacketStore) {
	store.lruMu.Lock()
	defer store.lruMu.Unlock()
	store.apiResolvedPathLRU = make(map[int]resolvedPathLRUEntry, lruMaxSize)
	store.lruOrder = store.lruOrder[:0]
}

type confirmPathsResp struct {
	Paths              []json.RawMessage `json:"paths"`
	TotalTransmissions int               `json:"totalTransmissions"`
}

func TestNodePaths_NoPerCandidateSQLConfirm(t *testing.T) {
	srv, router := setupTestServer(t)
	const n = 25
	for i := 0; i < n; i++ {
		seedConfirmTx(t, srv, fmt.Sprintf("confirm_def_%02d", i),
			`["aa","bb"]`, `["`+confirmTestTarget+`","eeff00112233aabb"]`)
	}
	store := reloadConfirmStore(t, srv)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/nodes/"+confirmTestTarget+"/paths", nil))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp confirmPathsResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if resp.TotalTransmissions < n {
		t.Errorf("totalTransmissions = %d, want >= %d (all seeded txs resolve through the target)", resp.TotalTransmissions, n)
	}
	if q := store.confirmResolvedPathQueries.Load(); q != 0 {
		t.Errorf("confirmResolvedPathContains ran %d times for %d candidates, want 0 (every candidate has a canonical resolved_path)", q, n)
	}
}

// A hash-index entry that points at a tx whose stored resolved_path does NOT
// contain the queried pubkey (hash collision, or an index entry made stale by
// a later resolved_path overwrite) must still be excluded — now by the
// canonical-path check instead of the SQL pre-filter.
func TestNodePaths_StaleIndexEntryStillExcluded(t *testing.T) {
	srv, router := setupTestServer(t)
	staleID := seedConfirmTx(t, srv, "confirm_stale_hash",
		`["aa","bb"]`, `["aacafe0000000000","eeff00112233aabb"]`)
	store := reloadConfirmStore(t, srv)

	store.mu.Lock()
	h := resolvedPubkeyHash(confirmTestTarget)
	store.resolvedPubkeyIndex[h] = append(store.resolvedPubkeyIndex[h], staleID)
	store.mu.Unlock()

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/nodes/"+confirmTestTarget+"/paths", nil))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "confirm_stale_hash") {
		t.Error("tx whose resolved_path does not contain the target leaked into /paths via a stale index entry")
	}

	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/nodes/"+confirmTestTarget+"/hop_analytics", nil))
	if w.Code != 200 {
		t.Fatalf("hop_analytics: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "confirm_stale_hash") {
		t.Error("tx whose resolved_path does not contain the target leaked into /hop_analytics via a stale index entry")
	}
	if q := store.confirmResolvedPathQueries.Load(); q != 0 {
		t.Errorf("confirmResolvedPathContains ran %d times, want 0", q)
	}
}

// Candidates with no canonical resolved_path are decided by the legacy
// fallback arm, which still relies on the SQL confirmation. That behaviour is
// preserved: the query runs (once) and a NULL resolved_path does not confirm.
func TestNodePaths_NoCanonicalPathStillConfirmedBySQL(t *testing.T) {
	srv, router := setupTestServer(t)
	nullID := seedConfirmTx(t, srv, "confirm_nullrp_hash", `["aa","bb"]`, "")
	store := reloadConfirmStore(t, srv)

	// Force the "in the hash index but no canonical path" state.
	store.mu.Lock()
	h := resolvedPubkeyHash(confirmTestTarget)
	store.resolvedPubkeyIndex[h] = append(store.resolvedPubkeyIndex[h], nullID)
	store.resolvedPubkeyReverse[nullID] = []uint64{h ^ 1} // has *some* indexed pubkey
	store.mu.Unlock()

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/nodes/"+confirmTestTarget+"/paths", nil))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "confirm_nullrp_hash") {
		t.Error("tx with NULL resolved_path was confirmed for the target")
	}
	if q := store.confirmResolvedPathQueries.Load(); q != 1 {
		t.Errorf("confirmResolvedPathContains ran %d times, want exactly 1 (the tx without a canonical path)", q)
	}
}

// #277 point 2. Since #246 a candidate admitted by the hash index costs a
// canonical-path fetch, and an LRU entry, even when its stored path turns out
// not to contain the target. That stays cheap only because prefix collisions,
// the usual reason a /paths candidate does not belong to the node, are dropped
// by the index check before any fetch; only hash collisions and stale index
// entries get as far as the fetch. This pins that order for both endpoints.
func TestNodePaths_PrefixCollisionsExcludedBeforeCanonicalFetch(t *testing.T) {
	srv, router := setupTestServer(t)
	ownID := seedConfirmTx(t, srv, "collide_own", `["aa","bb"]`, `["`+confirmTestTarget+`","eeff00112233aabb"]`)
	var otherIDs []int
	for i := 0; i < 10; i++ {
		// Same first-hop prefix as the target, resolved to another node.
		otherIDs = append(otherIDs, seedConfirmTx(t, srv, fmt.Sprintf("collide_other_%02d", i),
			`["aa","bb"]`, `["aacafe0000000000","eeff00112233aabb"]`))
	}
	store := reloadConfirmStore(t, srv)

	for _, ep := range []string{"paths", "hop_analytics"} {
		resetLRU(store)
		body := nodeEndpointBody(t, router, ep)
		if !strings.Contains(body, "collide_own") {
			t.Errorf("%s: the target's own tx is missing", ep)
		}
		if !lruHasTx(store, ownID) {
			t.Errorf("%s: the target's own tx was decided without its canonical path", ep)
		}
		for i, id := range otherIDs {
			if strings.Contains(body, fmt.Sprintf("collide_other_%02d", i)) {
				t.Errorf("%s: prefix-colliding tx %d attributed to the target", ep, id)
			}
			if lruHasTx(store, id) {
				t.Errorf("%s: prefix-colliding tx %d reached the canonical-path fetch (LRU entry for a tx that is not the node's)", ep, id)
			}
		}
	}
}
