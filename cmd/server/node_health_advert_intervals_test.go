package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNodeHealthAdvertIntervalsOptInRoleAndPrivacy(t *testing.T) {
	srv, router := setupTestServer(t)
	const repeater = "abcdabcdabcdabcd"
	const companion = "bcdabcdabcdabcda"
	for _, n := range []struct{ key, role string }{{repeater, "repeater"}, {companion, "companion"}} {
		_, err := srv.db.conn.Exec(`INSERT INTO nodes (public_key,name,role,lat,lon,last_seen,first_seen,advert_count) VALUES (?, ?, ?, 0,0,'2026-10-07T00:00:00Z','2026-10-07T00:00:00Z',1)`, n.key, n.role, n.role)
		if err != nil {
			t.Fatal(err)
		}
	}
	lookups, scans := narCountRouteWork(srv)
	get := func(key, query string) map[string]json.RawMessage {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/api/nodes/"+key+"/health"+query, nil))
		if w.Code != 200 {
			t.Fatalf("health %s %s: %d %s", key, query, w.Code, w.Body.String())
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	if _, ok := get(repeater, "")["advertIntervals"]; ok {
		t.Fatal("default response added advertIntervals")
	}
	if _, ok := get(companion, "?include=advertIntervals")["advertIntervals"]; ok {
		t.Fatal("non-repeater exposes intervals")
	}
	if lookups.Load() != 0 {
		t.Fatalf("unexpected route lookups: %d", lookups.Load())
	}
	if _, ok := get(repeater, "?include=advertIntervals")["advertIntervals"]; !ok {
		t.Fatal("repeater missing intervals")
	}
	if lookups.Load() != 1 {
		t.Fatalf("want one cache lookup, got %d", lookups.Load())
	}
	if _, ok := get(repeater, "?include=other,advertIntervals")["advertIntervals"]; !ok {
		t.Fatal("comma include missing intervals")
	}
	if _, ok := get(repeater, "?include=other&include=advertIntervals")["advertIntervals"]; !ok {
		t.Fatal("repeated include missing intervals")
	}
	if lookups.Load() != 3 {
		t.Fatalf("want three cache lookups, got %d", lookups.Load())
	}
	if scans.Load() != 1 {
		t.Fatalf("opt-in health requests should reuse one cached scan, got %d", scans.Load())
	}
	srv.cfg.ObserverBlacklist = []string{strings.ToUpper(repeater)}
	if _, ok := get(repeater, "?include=advertIntervals")["advertIntervals"]; ok {
		t.Fatal("hidden identity exposed intervals")
	}
	if lookups.Load() != 3 {
		t.Fatal("privacy must gate before cache")
	}
}

func TestNodeHealthAdvertIntervalsFailsClosedOnIdentityLookupError(t *testing.T) {
	srv, router := setupTestServer(t)
	const key = "abcdabcdabcdabcd"
	if _, err := srv.db.conn.Exec(`INSERT INTO nodes (public_key,name,role,lat,lon,last_seen,first_seen,advert_count) VALUES (?, 'repeater', 'repeater',0,0,'2026-10-07T00:00:00Z','2026-10-07T00:00:00Z',1)`, key); err != nil {
		t.Fatal(err)
	}
	srv.cfg.SetHiddenNamePrefixes([]string{"ZZ-unmatched-prefix"})
	lookups, _ := narCountRouteWork(srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/nodes/"+key+"/health?include=advertIntervals", nil).WithContext(ctx))
	if w.Code != 200 {
		t.Fatalf("health status %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "advertIntervals") || lookups.Load() != 0 {
		t.Fatalf("failed identity lookup must not expose cadence or consult cache: %s", w.Body.String())
	}
}
