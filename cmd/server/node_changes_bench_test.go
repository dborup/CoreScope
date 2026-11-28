package main

import (
	"fmt"
	"testing"
	"time"
)

// BenchmarkComputeNodeChanges measures Tools > Node Changes at the fetch cap:
// 500 node_changes rows for 500 distinct nodes, with a hidden-name prefix
// configured. limit=50 is the list endpoint's default, limit=500 the
// Network Digest's fetch.
func BenchmarkComputeNodeChanges(b *testing.B) {
	t := &testing.T{}
	srv, _ := setupTestServer(t)
	ensureInactiveNodesTable(t, srv)
	ensureNodeChangesTable(t, srv)
	srv.cfg.SetHiddenNamePrefixes([]string{"🚫"})
	now := time.Now().UTC()
	for i := 0; i < nodeChangesSQLFetchCap; i++ {
		pk := fmt.Sprintf("benchchg%08d", i)
		name := "Node"
		if i%10 == 0 {
			name = "🚫 Hidden"
		}
		if _, err := srv.db.conn.Exec(`INSERT INTO nodes (public_key, name, role, first_seen, last_seen) VALUES (?,?,?,?,?)`,
			pk, name, "repeater", now.Format(time.RFC3339), now.Format(time.RFC3339)); err != nil {
			b.Fatal(err)
		}
		if _, err := srv.db.conn.Exec(`INSERT INTO node_changes (public_key, change_type, old_value, new_value, detected_at) VALUES (?,?,?,?,?)`,
			pk, "role", "companion", "repeater", now.Add(-time.Duration(i)*time.Minute).Format(time.RFC3339)); err != nil {
			b.Fatal(err)
		}
	}
	for _, limit := range []int{50, nodeChangesSQLFetchCap} {
		b.Run(fmt.Sprintf("limit=%d", limit), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := srv.computeNodeChanges(limit); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
