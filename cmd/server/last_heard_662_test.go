package main

import (
	"fmt"
	"testing"
	"time"
)

// #662 — every Map "Last Heard" key must filter GetNodes at its own window
// boundary; an unknown key applies no filter (today's behaviour).
func TestGetNodesLastHeardKeys662(t *testing.T) {
	h := time.Hour
	cases := []struct {
		key    string
		window time.Duration
	}{
		{"1h", h}, {"3h", 3 * h}, {"6h", 6 * h}, {"12h", 12 * h}, {"24h", 24 * h},
		{"2d", 48 * h}, {"3d", 72 * h}, {"4d", 96 * h}, {"5d", 120 * h}, {"6d", 144 * h},
		{"7d", 168 * h}, {"30d", 720 * h},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			db := setupTestDB(t)
			now := time.Now().UTC()
			ins := func(pk string, age time.Duration) {
				ts := now.Add(-age).Format(time.RFC3339)
				if _, err := db.conn.Exec(`INSERT INTO nodes (public_key, name, role, last_seen, first_seen)
					VALUES (?, ?, 'repeater', ?, '2020-01-01T00:00:00Z')`, pk, pk, ts); err != nil {
					t.Fatal(err)
				}
			}
			ins("inside", c.window-time.Minute)
			ins("outside", c.window+time.Minute)

			nodes, total, _, err := db.GetNodes(50, 0, "", "", "", c.key, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if total != 1 || len(nodes) != 1 || fmt.Sprint(nodes[0]["public_key"]) != "inside" {
				t.Fatalf("lastHeard=%s: want only 'inside', got total=%d nodes=%v", c.key, total, nodes)
			}
		})
	}

	for _, key := range []string{"bogus", "2h", "8d", "constructor"} {
		t.Run("unknown_"+key, func(t *testing.T) {
			db := setupTestDB(t)
			old := time.Now().UTC().Add(-1000 * time.Hour).Format(time.RFC3339)
			if _, err := db.conn.Exec(`INSERT INTO nodes (public_key, name, role, last_seen, first_seen)
				VALUES ('old', 'old', 'repeater', ?, '2020-01-01T00:00:00Z')`, old); err != nil {
				t.Fatal(err)
			}
			_, total, _, err := db.GetNodes(50, 0, "", "", "", key, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if total != 1 {
				t.Fatalf("unknown key %q must apply no filter, total=%d", key, total)
			}
		})
	}
}

// The table has unique keys and positive windows, and lastHeardDuration
// resolves exactly its keys.
func TestLastHeardWindowsTable662(t *testing.T) {
	seen := map[string]bool{}
	for _, w := range lastHeardWindows {
		if seen[w.Key] {
			t.Errorf("duplicate key %q", w.Key)
		}
		seen[w.Key] = true
		d, ok := lastHeardDuration(w.Key)
		if !ok || d != w.Dur || d <= 0 {
			t.Errorf("key %q: got (%v,%v), want (%v,true)", w.Key, d, ok, w.Dur)
		}
	}
	if len(seen) != 12 {
		t.Errorf("want 12 keys, got %d", len(seen))
	}
	if _, ok := lastHeardDuration("bogus"); ok {
		t.Error("unknown key must not resolve")
	}
}
