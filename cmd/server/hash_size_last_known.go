package main

// Last known advert facts per node (#807 hash size, #848 last advert).
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
//
// The same snapshot holds each node's newest own ADVERT timestamp (#848),
// which the health endpoints use for stats.lastAdvert when the window has
// none (applyLastKnownAdvert), so a node quiet longer than the window does
// not read as "Last activity unknown".

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/meshcore-analyzer/packetpath"
)

// lastKnownAdvertRefreshInterval is how often the snapshot is re-read. The
// window covers every advert newer than the store retention, so the snapshot
// only matters once an advert has aged out; a few minutes of lag is harmless.
const lastKnownAdvertRefreshInterval = 10 * time.Minute

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

// LastKnownAdvert is what the database remembers about a node's adverts,
// for when the packet-store window has nothing (#807, #848).
type LastKnownAdvert struct {
	// HashSize is the size declared by the node's newest size-declaring
	// ADVERT; 0 means unknown.
	HashSize int
	// LastAdvert is the first_seen of the node's newest own ADVERT under the
	// window's identity rule (ownAdvertMatches); "" means none.
	LastAdvert string
}

// lastKnownAdvertJSON is decoded_json when it is valid JSON, else NULL, so
// json_extract never raises on a malformed row.
const lastKnownAdvertJSON = `CASE WHEN json_valid(t.decoded_json) THEN t.decoded_json END`

// lastKnownAdvertSQL returns, for every row of nodes, the raw_hex of that
// node's newest ADVERT that can declare a size (#807), and the first_seen and
// decoded_json of its newest own ADVERT (#848). It is one statement with two
// correlated subqueries per node; each seeks idx_transmissions_from_pubkey
// and walks it newest id first, stopping at the first match (LIMIT 1, no
// sort). The second returns a rowid, joined back by primary key.
//
//   - The unary + on payload_type keeps the planner off
//     idx_transmissions_payload_type, which would visit every ADVERT in the
//     database (same reasoning as nodeAdvertScanSQL).
//   - Size: the two NOT clauses pre-filter the sizeless zero-hop direct
//     adverts so the walk does not stop on one; advertDeclaredHashSize
//     re-checks every returned row in Go.
//   - Last advert: the same identity rule as the window (txIsOwnAdvert):
//     exact origin (from_pubkey and the decoded pubKey both equal the node's
//     key, never a path or prefix match) and a signature that is not invalid.
//     The walk skips rows that fail it, so a forged advert can neither count
//     nor hide an older valid one. lastKnownAdvertTimestamp re-checks the
//     returned row in Go.
var lastKnownAdvertSQL = fmt.Sprintf(`SELECT k.public_key, k.size_raw, a.first_seen, a.decoded_json
	FROM (SELECT n.public_key AS public_key, (
		SELECT t.raw_hex FROM transmissions t
		WHERE t.from_pubkey = n.public_key AND +t.payload_type = %[1]d
		  AND NOT (COALESCE(t.route_type, -1) = %[2]d AND substr(t.raw_hex, 3, 2) = '00')
		  AND NOT (COALESCE(t.route_type, -1) = %[3]d AND substr(t.raw_hex, 11, 2) = '00')
		ORDER BY t.id DESC LIMIT 1) AS size_raw, (
		SELECT t.id FROM transmissions t
		WHERE t.from_pubkey = n.public_key AND +t.payload_type = %[1]d
		  AND json_extract(%[4]s, '$.signatureValid') IS NOT 0
		  AND lower(json_extract(%[4]s, '$.pubKey')) = lower(n.public_key)
		ORDER BY t.id DESC LIMIT 1) AS advert_id
	FROM nodes n) k
	LEFT JOIN transmissions a ON a.id = k.advert_id`, PayloadADVERT, RouteDirect, RouteTransportDirect, lastKnownAdvertJSON)

// lastKnownAdvertTimestamp re-checks one snapshot row against the window's
// identity rule and returns its timestamp, or "" when it does not count.
func lastKnownAdvertTimestamp(pubkey, firstSeen, decodedJSON string) string {
	var d struct {
		PubKey         string `json:"pubKey"`
		SignatureValid *bool  `json:"signatureValid"`
	}
	if json.Unmarshal([]byte(decodedJSON), &d) != nil {
		return ""
	}
	if !ownAdvertMatches(d.PubKey, d.SignatureValid != nil && !*d.SignatureValid, pubkey) {
		return ""
	}
	if _, ok := parseRelayTS(firstSeen); !ok {
		return ""
	}
	return firstSeen
}

// LoadLastKnownAdverts reads each node's last known advert facts. Nodes
// without any are absent. Read-only. Fails on databases that predate
// transmissions.from_pubkey.
func (db *DB) LoadLastKnownAdverts() (map[string]LastKnownAdvert, error) {
	rows, err := db.conn.Query(lastKnownAdvertSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]LastKnownAdvert)
	for rows.Next() {
		var pk string
		var raw, firstSeen, decoded *string
		if err := rows.Scan(&pk, &raw, &firstSeen, &decoded); err != nil {
			return nil, err
		}
		var known LastKnownAdvert
		if raw != nil {
			if hs, ok := advertDeclaredHashSize(*raw); ok {
				known.HashSize = hs
			}
		}
		if firstSeen != nil && decoded != nil {
			known.LastAdvert = lastKnownAdvertTimestamp(pk, *firstSeen, *decoded)
		}
		if known != (LastKnownAdvert{}) {
			out[pk] = known
		}
	}
	return out, rows.Err()
}

// refreshLastKnownAdverts re-reads the snapshot and installs it. On error
// the previous snapshot stays in place.
func (s *PacketStore) refreshLastKnownAdverts() {
	if s.db == nil || s.db.conn == nil {
		return
	}
	start := time.Now()
	m, err := s.db.LoadLastKnownAdverts()
	if err != nil {
		log.Printf("[last-known-advert] load failed: %v", err)
		return
	}
	s.lastKnownAdverts.Store(&m)
	if d := time.Since(start); d > time.Second {
		log.Printf("[last-known-advert] %d nodes in %s", len(m), d)
	}
}

// LastKnownAdverts returns the current snapshot (read-only; nil before the
// first successful load, which reads as "nothing remembered").
func (s *PacketStore) LastKnownAdverts() map[string]LastKnownAdvert {
	if p := s.lastKnownAdverts.Load(); p != nil {
		return *p
	}
	return nil
}

// StartLastKnownAdvertRecomputer loads the snapshot synchronously (so the
// first request after a restart already has it) and then refreshes it every
// interval. The returned func stops the goroutine.
func (s *PacketStore) StartLastKnownAdvertRecomputer(interval time.Duration) func() {
	if interval <= 0 {
		interval = lastKnownAdvertRefreshInterval
	}
	s.refreshLastKnownAdverts()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				s.refreshLastKnownAdverts()
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
