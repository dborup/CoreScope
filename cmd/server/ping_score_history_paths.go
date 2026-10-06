package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// These are storage/response safety ceilings, not protocol assumptions. A
// path exceeding any ceiling is not archived, never silently truncated.
const (
	pingScorePathArchiveMaxRecords  = 10
	pingScorePathArchiveMaxBytes    = 256 * 1024
	pingScorePathArchiveMaxBranches = 128
	pingScorePathArchiveMaxPoints   = 4096
)

// PingScorePathArchive contains actual path evidence at CapturedAt, not a
// reconstruction from today's node positions. RecordKey distinguishes two
// cards for the same hash whose evidence was captured at different times.
type PingScorePathArchive struct {
	RecordKey  string
	Hash       string
	Timestamp  string
	CapturedAt string
	Path       PacketPathResponse
}

var pingScoreRecordKinds = []string{"farthestPing", "mostHopsPing", "widestSpreadPing", "fastestSpreadPing", "mostEfficientPing"}

func pingScoreRecordSlots(snap *PingScoresSnapshot) map[string]*PingScore {
	out := make(map[string]*PingScore, pingScorePathArchiveMaxRecords)
	if snap == nil {
		return out
	}
	add := func(prefix string, a, b, c, d, e *PingScore) {
		for i, p := range []*PingScore{a, b, c, d, e} {
			if p != nil {
				out[prefix+"."+pingScoreRecordKinds[i]] = p
			}
		}
	}
	add("allTime", snap.FarthestPing, snap.MostHopsPing, snap.WidestSpreadPing, snap.FastestSpreadPing, snap.MostEfficientPing)
	if w := snap.ThisWeek; w != nil {
		add("thisWeek", w.FarthestPing, w.MostHopsPing, w.WidestSpreadPing, w.FastestSpreadPing, w.MostEfficientPing)
	}
	return out
}

func validPingScoreRecordKey(key string) bool {
	prefix, kind, ok := strings.Cut(key, ".")
	if !ok || (prefix != "allTime" && prefix != "thisWeek") {
		return false
	}
	for _, k := range pingScoreRecordKinds {
		if kind == k {
			return true
		}
	}
	return false
}

// Preserve distance only when missing evidence explains the downgrade. When
// both of the previously measured endpoints still have authoritative GPS,
// a new shorter distance is a real correction and must remain allowed.
func preservePingDistanceWithoutGPS(existing PingScoreHistoryEntry, score *PingScore, path *PacketPathResponse) *PingScore {
	if score == nil || existing.FarthestKm == nil {
		return score
	}
	if score.FarthestKm != nil && *score.FarthestKm >= *existing.FarthestKm {
		return score
	}
	positioned := func(pk string) bool {
		if pk == "" || path == nil {
			return false
		}
		valid := func(o *PacketPathObserver) bool {
			return o != nil && strings.EqualFold(o.PublicKey, pk) && !o.Approx && !o.iataFallback && o.Lat != nil && o.Lon != nil
		}
		if path.First != nil && valid(path.First.Observer) {
			return true
		}
		for i := range path.Branches {
			if valid(path.Branches[i].Observer) {
				return true
			}
		}
		return false
	}
	distanceOrigin := existing.DistanceFirstPubkey
	if distanceOrigin == "" {
		distanceOrigin = existing.FirstPubkey // legacy v1/v2 fact
	}
	if positioned(distanceOrigin) && positioned(existing.FarthestPubkey) {
		return score
	}
	copy := *score
	copy.FarthestKm, copy.FarthestPubkey = existing.FarthestKm, existing.FarthestPubkey
	// Keep the old distance landmark separate from the actual current
	// first hearer. The latter still feeds the observer leaderboard.
	copy.distanceFirstPubkey = distanceOrigin
	return &copy
}

func pingPathMatchesRecord(key string, score *PingScore, path *PacketPathResponse) bool {
	if score == nil || path == nil || len(path.Branches) == 0 || !strings.EqualFold(path.Hash, score.Hash) {
		return false
	}
	_, kind, _ := strings.Cut(key, ".")
	var distance, spread *float64
	farthestPubkey := ""
	maxHops := 0
	for i := range path.Branches {
		b := &path.Branches[i]
		if b.Hops > maxHops {
			maxHops = b.Hops
		}
		if b.DistanceFromFirstKm != nil && (distance == nil || *b.DistanceFromFirstKm > *distance) {
			distance = b.DistanceFromFirstKm
			farthestPubkey = ""
			if b.Observer != nil {
				farthestPubkey = b.Observer.PublicKey
			}
		}
		if b.SecondsAfterFirst != nil && (spread == nil || *b.SecondsAfterFirst > *spread) {
			spread = b.SecondsAfterFirst
		}
	}
	equal := func(a, b *float64) bool { return a != nil && b != nil && *a == *b }
	distanceMatches := equal(distance, score.FarthestKm) && strings.EqualFold(farthestPubkey, score.FarthestPubkey)
	// A retained old distance must not be paired with a route using a new
	// origin, even if its numeric distance happens to be exactly equal.
	distanceOrigin := score.distanceFirstPubkey
	if distanceOrigin == "" {
		distanceOrigin = score.firstPubkey
	}
	if distanceOrigin != "" {
		distanceMatches = distanceMatches && path.First != nil && path.First.Observer != nil && strings.EqualFold(path.First.Observer.PublicKey, distanceOrigin)
	}
	switch kind {
	case "farthestPing":
		return distanceMatches
	case "mostHopsPing":
		deepestPubkey := ""
		if path.Branches[0].Observer != nil {
			deepestPubkey = path.Branches[0].Observer.PublicKey
		}
		return maxHops == score.DeepestHops && strings.EqualFold(deepestPubkey, score.DeepestPubkey)
	case "widestSpreadPing":
		return len(path.Branches) == score.StationCount
	case "fastestSpreadPing":
		return len(path.Branches) >= 2 && equal(spread, score.SpreadSeconds)
	case "mostEfficientPing":
		return distanceMatches && equal(path.EstimatedAirtimeMs, score.AirtimeMs)
	}
	return false
}

func boundedPingPathJSON(path *PacketPathResponse) ([]byte, error) {
	if path == nil || len(path.Branches) == 0 || len(path.Branches) > pingScorePathArchiveMaxBranches {
		return nil, fmt.Errorf("path branch bound exceeded or no path")
	}
	points := 0
	stringsBytes := len(path.Hash)
	check := func(b PacketPathBranch) bool {
		points += len(b.Points)
		for _, p := range b.Points {
			stringsBytes += len(p.Name) + len(p.PublicKey) + len(p.Role)
		}
		if b.Observer != nil {
			stringsBytes += len(b.Observer.Name) + len(b.Observer.PublicKey) + len(b.Observer.Role) + len(b.Observer.IATA)
		}
		return points <= pingScorePathArchiveMaxPoints && stringsBytes <= pingScorePathArchiveMaxBytes/6
	}
	for _, b := range path.Branches {
		if !check(b) {
			return nil, fmt.Errorf("path point/string bound exceeded")
		}
	}
	if path.First != nil && !check(*path.First) {
		return nil, fmt.Errorf("first path bound exceeded")
	}
	// Areas are current config projections, not recorded route evidence.
	copy := *path
	copy.TouchedAreas = nil
	b, err := json.Marshal(copy)
	if err != nil {
		return nil, err
	}
	if len(b) > pingScorePathArchiveMaxBytes {
		return nil, fmt.Errorf("path byte bound exceeded")
	}
	return b, nil
}

func newPingScorePathArchive(key string, score *PingScore, path *PacketPathResponse, now time.Time) (PingScorePathArchive, bool) {
	if !validPingScoreRecordKey(key) || !pingPathMatchesRecord(key, score, path) {
		return PingScorePathArchive{}, false
	}
	b, err := boundedPingPathJSON(path)
	if err != nil {
		return PingScorePathArchive{}, false
	}
	var immutable PacketPathResponse
	if err := json.Unmarshal(b, &immutable); err != nil {
		return PingScorePathArchive{}, false
	}
	immutable.Hash = score.Hash // preserve trigger casing, as score identity does
	return PingScorePathArchive{RecordKey: key, Hash: score.Hash, Timestamp: score.Timestamp, CapturedAt: now.UTC().Format(time.RFC3339), Path: immutable}, true
}

func (e *pingScoreHistoryEngine) pathsForRecords(snap *PingScoresSnapshot, paths map[string]*PacketPathResponse, attempted map[string]bool, now time.Time) (map[string]PingScorePathArchive, bool, error) {
	slots := pingScoreRecordSlots(snap)
	missing, seen := []string{}, map[string]bool{}
	for key, score := range slots {
		h := strings.ToLower(score.Hash)
		old, ok := e.pathArchives[key]
		if !attempted[h] && !(ok && old.Hash == score.Hash && old.Timestamp == score.Timestamp && pingPathMatchesRecord(key, score, &old.Path)) && !seen[h] {
			missing, seen[h] = append(missing, h), true
		}
	}
	if len(missing) > 0 {
		// At most ten distinct displayed hashes, in one existing bulk helper.
		extra, err := e.server.db.getPacketPathsBulk(missing, e.config.MaxEdgeKm, e.server.estimatedPositionsEnabled())
		if err != nil {
			return nil, false, fmt.Errorf("record path capture: %w", err)
		}
		if paths == nil {
			paths = make(map[string]*PacketPathResponse)
		}
		for _, h := range missing {
			paths[h] = extra[h]
			if paths[h] != nil {
				e.server.annotatePacketPathAirtime(paths[h])
			}
		}
	}
	out := make(map[string]PingScorePathArchive, len(slots))
	for key, score := range slots {
		old, oldOK := e.pathArchives[key]
		oldOK = oldOK && old.Hash == score.Hash && old.Timestamp == score.Timestamp && pingPathMatchesRecord(key, score, &old.Path)
		if next, ok := newPingScorePathArchive(key, score, paths[strings.ToLower(score.Hash)], now); ok {
			// Identical evidence doesn't create a new capture time or DB write.
			if oldOK {
				a, _ := boundedPingPathJSON(&old.Path)
				b, _ := boundedPingPathJSON(&next.Path)
				if string(a) == string(b) {
					next = old
				}
			}
			out[key] = next
		} else if oldOK {
			out[key] = old
		}
	}
	changed := len(out) != len(e.pathArchives)
	for k, v := range out {
		if old, ok := e.pathArchives[k]; !ok || !reflect.DeepEqual(old, v) {
			changed = true
		}
	}
	return out, changed, nil
}

func applyPingScoreHistoryV3(tx *sql.Tx) error {
	// Nullable metadata only: no main-DB scan, rewrite, or index build.
	// Legacy NULL rows use their first_pubkey until recomputed.
	// Like the older additive migrations, tolerate a pre-existing column
	// when a metadata reset causes the migration ladder to run again.
	var present int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('ping_score_history_entries') WHERE name='distance_first_pubkey'`).Scan(&present); err != nil {
		return err
	}
	if present == 0 {
		if _, err := tx.Exec(`ALTER TABLE ping_score_history_entries ADD COLUMN distance_first_pubkey TEXT`); err != nil {
			return err
		}
	}
	keys := make([]string, 0, pingScorePathArchiveMaxRecords)
	for _, prefix := range []string{"allTime", "thisWeek"} {
		for _, kind := range pingScoreRecordKinds {
			keys = append(keys, "'"+prefix+"."+kind+"'")
		}
	}
	_, err := tx.Exec(fmt.Sprintf(`CREATE TABLE IF NOT EXISTS ping_score_path_archives (
		record_key TEXT PRIMARY KEY CHECK (record_key IN (%s)),
		hash TEXT NOT NULL, timestamp TEXT NOT NULL, captured_at TEXT NOT NULL,
		path_json TEXT NOT NULL CHECK (length(CAST(path_json AS BLOB)) <= %d)
	)`, strings.Join(keys, ","), pingScorePathArchiveMaxBytes))
	return err
}

func (s *PingScoreHistoryStore) LoadPathArchives() (map[string]PingScorePathArchive, error) {
	out := make(map[string]PingScorePathArchive)
	// Future schemas remain read-only compatible even if they lack this
	// optional table; never write/migrate them merely to open the board.
	var present int
	if err := s.conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='ping_score_path_archives'`).Scan(&present); err != nil {
		return nil, err
	}
	if present == 0 && s.readOnly {
		return out, nil
	}
	rows, err := s.conn.Query(`SELECT record_key, hash, timestamp, captured_at, length(CAST(path_json AS BLOB)), CASE WHEN length(CAST(path_json AS BLOB)) <= ? THEN path_json ELSE NULL END FROM ping_score_path_archives ORDER BY record_key LIMIT ?`, pingScorePathArchiveMaxBytes, pingScorePathArchiveMaxRecords+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a PingScorePathArchive
		var size int
		var data sql.NullString
		if err := rows.Scan(&a.RecordKey, &a.Hash, &a.Timestamp, &a.CapturedAt, &size, &data); err != nil {
			return nil, err
		}
		if !validPingScoreRecordKey(a.RecordKey) || !data.Valid || size > pingScorePathArchiveMaxBytes || len(out) >= pingScorePathArchiveMaxRecords {
			return nil, fmt.Errorf("invalid/oversized path archive %q", a.RecordKey)
		}
		if err := json.Unmarshal([]byte(data.String), &a.Path); err != nil {
			return nil, err
		}
		if _, err := boundedPingPathJSON(&a.Path); err != nil {
			return nil, err
		}
		if a.Path.Hash != a.Hash {
			return nil, fmt.Errorf("path archive hash mismatch")
		}
		if _, err := time.Parse(time.RFC3339, a.CapturedAt); err != nil {
			return nil, err
		}
		out[a.RecordKey] = a
	}
	return out, rows.Err()
}

// Validate and replace the old slot set inside the same transaction that
// owns this cycle's scores and integrity metadata. Any error rolls back
// the replacement, so the old evidence is never removed on its own.
func writePingScorePathArchives(tx *sql.Tx, archives map[string]PingScorePathArchive) error {
	if archives == nil {
		return nil
	}
	if len(archives) > pingScorePathArchiveMaxRecords {
		return fmt.Errorf("too many record path archives")
	}
	if _, err := tx.Exec(`DELETE FROM ping_score_path_archives`); err != nil {
		return err
	}
	for key, a := range archives {
		if key != a.RecordKey || !validPingScoreRecordKey(key) || a.Hash != a.Path.Hash {
			return fmt.Errorf("invalid record path identity")
		}
		if _, err := time.Parse(time.RFC3339, a.CapturedAt); err != nil {
			return err
		}
		b, err := boundedPingPathJSON(&a.Path)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO ping_score_path_archives (record_key,hash,timestamp,captured_at,path_json) VALUES (?,?,?,?,?)`, key, a.Hash, a.Timestamp, a.CapturedAt, string(b)); err != nil {
			return err
		}
	}
	return nil
}
