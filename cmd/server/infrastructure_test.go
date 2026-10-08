package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/meshcore-analyzer/channelregistry"
	"github.com/meshcore-analyzer/infrastructure"
)

func TestInfrastructureRoutesReadOnlyAndAuthenticated(t *testing.T) {
	srv, router := setupTestServerWithAPIKey(t, "strong-test-api-key-for-infrastructure")
	// The normal route fixture predates this optional feature table.
	if _, err := srv.db.conn.Exec(`CREATE TABLE infrastructure_state (id INTEGER PRIMARY KEY CHECK(id=1), state_json TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	srv.db.path = t.TempDir() + "/mesh.db"
	key := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := srv.db.conn.Exec(`INSERT INTO nodes(public_key, role) VALUES(?, 'repeater')`, key); err != nil {
		t.Fatal(err)
	}
	phantomReq := httptest.NewRequest("POST", "/api/admin/infrastructure/cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc/select", nil)
	phantomReq.Header.Set("X-API-Key", "strong-test-api-key-for-infrastructure")
	phantomResp := httptest.NewRecorder()
	router.ServeHTTP(phantomResp, phantomReq)
	if phantomResp.Code != http.StatusConflict {
		t.Fatalf("phantom select: %d", phantomResp.Code)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/infrastructure", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("empty list: %d %s", w.Code, w.Body.String())
	}
	var list InfrastructureResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || list.Selected == nil || len(list.Selected) != 0 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("POST", "/api/admin/infrastructure/"+key+"/select", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized write: %d", w.Code)
	}
	auth := httptest.NewRequest("GET", "/api/admin/infrastructure/auth", nil)
	auth.Header.Set("X-API-Key", "wrong-test-key")
	authResp := httptest.NewRecorder()
	router.ServeHTTP(authResp, auth)
	if authResp.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key auth: %d", authResp.Code)
	}
	auth.Header.Set("X-API-Key", "strong-test-api-key-for-infrastructure")
	authResp = httptest.NewRecorder()
	router.ServeHTTP(authResp, auth)
	if authResp.Code != http.StatusOK {
		t.Fatalf("valid key auth: %d", authResp.Code)
	}
	req := httptest.NewRequest("POST", "/api/admin/infrastructure/"+key+"/select", nil)
	req.Header.Set("X-API-Key", "strong-test-api-key-for-infrastructure")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("queue: %d %s", w.Code, w.Body.String())
	}
	var accepted ChannelProposalAcceptedResponse
	if err := json.Unmarshal(w.Body.Bytes(), &accepted); err != nil || !channelregistry.ValidID(accepted.RequestID) {
		t.Fatalf("accepted = %+v, %v", accepted, err)
	}
	queued, err := channelregistry.NewQueue(infrastructure.QueuePath(srv.db.path)).Pending()
	if err != nil || len(queued) != 1 || queued[0].Command.Name != key {
		t.Fatalf("queue = %+v, %v", queued, err)
	}
	// The API server enqueues only; no curated snapshot exists until the
	// ingestor processes the command.
	state, err := infrastructure.LoadDB(srv.db.conn)
	if err != nil || len(state.Selected) != 0 {
		t.Fatalf("server wrote state: %+v, %v", state, err)
	}
}
