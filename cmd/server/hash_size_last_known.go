package main

// Last known advert hash size per node (#807).
//
// computeNodeHashSizeInfo only sees adverts inside the in-memory packet-store
// window. A repeater whose flood-advert interval is longer than the window
// (e.g. ~61 h on a 24 h store), or any node right after a restart, therefore
// had no hash_size in /api/nodes and the map drew it as a 1-byte node.
//
// This file keeps a read-only snapshot of each node's newest size-declaring
// ADVERT from SQLite, refreshed in the background. It only fills hash_size
// when the window has no value: the window always wins, and
// hash_size_inconsistent / hash_sizes_seen still come from the window alone.

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/meshcore-analyzer/packetpath"
)

// lastKnownHashSizeRefreshInterval is how often the snapshot is re-read. The
// window covers every advert newer than the store retention, so the snapshot
// only matters once an advert has aged out; a few minutes of lag is harmless.
const lastKnownHashSizeRefreshInterval = 10 * time.Minute

// advertDeclaredHashSize returns the hash size an ADVERT's path byte declares,
// or ok=false when the packet says nothing about it. Shared by the window scan
// (computeNodeHashSizeInfo) and the last-known snapshot so both apply the
// same rules.
func advertDeclaredHashSize(rawHex string) (int, bool) {
	// Transport routes (0, 3) have 4 transport code bytes before the path
	// byte, so the path byte is at offset 5 instead of 1.
	routeType, pathByte, ok := packetpath.PathByte(rawHex)
	if !ok {
		return 0, false
	}
	// Direct zero-hop adverts carry no path, so the hop count is 0. Whether
	// the SIZE bits are meaningful depends on the sender: firmware that
	// predates meshcore-dev/MeshCore#3293 does `path_len = 0`, wiping the
	// whole byte including the two size bits, so 0x00 says nothing about
	// the node's path.hash.mode. A sender that writes the size through
	// setPathHashSizeAndCount() emits 0x40 / 0x80 with a zero hop count —
	// that is a deliberate declaration and the only way those bits can be
	// non-zero here. Skip on the byte's content, not on the route type.
	if (routeType == RouteDirect || routeType == RouteTransportDirect) && pathByte == 0x00 {
		return 0, false
	}
	// NOTE: hash_size 4 is reserved and rejected by the decoder (#1211),
	// but this function has always reported it (TestHashSizeTransport-
	// RoutePathByteOffset pins HashSize=4 for path byte 0xC1), unlike
	// computeAnalyticsHashSizes which drops it. Skipping on byte content
	// widens that pre-existing gap slightly, from 0xC1 to also 0xC0.
	// Adding a `hs > 3` guard here would change the behaviour that test
	// deliberately pins, so reconciling the two sites is left as separate
	// work rather than smuggled into this fix.
	return int((pathByte>>6)&0x3) + 1, true
}

// lastKnownAdvertHashSizeSQL returns, for every row of nodes, the raw_hex of
// that node's newest ADVERT that can declare a size. It is one statement with
// one correlated subquery per node; each subquery seeks
// idx_transmissions_from_pubkey and walks it newest id first, stopping at the
// first match (LIMIT 1, no sort). The unary + on payload_type keeps the
// planner off idx_transmissions_payload_type, which would visit every ADVERT
// in the database (same reasoning as nodeAdvertScanSQL). The two NOT clauses
// pre-filter the sizeless zero-hop direct adverts so the walk does not stop on
// one; advertDeclaredHashSize re-checks every returned row in Go, which stays
// the single authority (a row the SQL lets through but Go rejects simply
// leaves the node unknown).
var lastKnownAdvertHashSizeSQL = fmt.Sprintf(`SELECT n.public_key, (
		SELECT t.raw_hex FROM transmissions t
		WHERE t.from_pubkey = n.public_key AND +t.payload_type = %d
		  AND NOT (COALESCE(t.route_type, -1) = %d AND substr(t.raw_hex, 3, 2) = '00')
		  AND NOT (COALESCE(t.route_type, -1) = %d AND substr(t.raw_hex, 11, 2) = '00')
		ORDER BY t.id DESC LIMIT 1)
	FROM nodes n`, PayloadADVERT, RouteDirect, RouteTransportDirect)

// LoadLastKnownAdvertHashSizes reads each node's newest size-declaring ADVERT
// and returns pubkey → hash size. Nodes without such an advert are absent.
// Read-only. Fails on databases that predate transmissions.from_pubkey.
func (db *DB) LoadLastKnownAdvertHashSizes() (map[string]int, error) {
	rows, err := db.conn.Query(lastKnownAdvertHashSizeSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var pk string
		var raw *string
		if err := rows.Scan(&pk, &raw); err != nil {
			return nil, err
		}
		if raw == nil {
			continue
		}
		if hs, ok := advertDeclaredHashSize(*raw); ok {
			out[pk] = hs
		}
	}
	return out, rows.Err()
}

// refreshLastKnownHashSizes re-reads the snapshot and installs it. On error
// the previous snapshot stays in place.
func (s *PacketStore) refreshLastKnownHashSizes() {
	if s.db == nil || s.db.conn == nil {
		return
	}
	start := time.Now()
	m, err := s.db.LoadLastKnownAdvertHashSizes()
	if err != nil {
		log.Printf("[hash-size] last-known advert hash size load failed: %v", err)
		return
	}
	s.lastKnownHashSizes.Store(&m)
	if d := time.Since(start); d > time.Second {
		log.Printf("[hash-size] last-known advert hash sizes: %d nodes in %s", len(m), d)
	}
}

// LastKnownHashSizes returns the current snapshot (read-only; nil before the
// first successful load, which reads as "no remembered sizes").
func (s *PacketStore) LastKnownHashSizes() map[string]int {
	if p := s.lastKnownHashSizes.Load(); p != nil {
		return *p
	}
	return nil
}

// StartLastKnownHashSizeRecomputer loads the snapshot synchronously (so the
// first request after a restart already has it) and then refreshes it every
// interval. The returned func stops the goroutine.
func (s *PacketStore) StartLastKnownHashSizeRecomputer(interval time.Duration) func() {
	if interval <= 0 {
		interval = lastKnownHashSizeRefreshInterval
	}
	s.refreshLastKnownHashSizes()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				s.refreshLastKnownHashSizes()
			case <-stop:
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(stop) })
		<-done
	}
}

// EnrichNodeWithLastKnownHashSize fills hash_size from the remembered size
// when the window left it empty. It never touches hash_size_inconsistent or
// hash_sizes_seen. size 0 means unknown.
func EnrichNodeWithLastKnownHashSize(node map[string]interface{}, size int) {
	if size <= 0 || node["hash_size"] != nil {
		return
	}
	node["hash_size"] = size
}
