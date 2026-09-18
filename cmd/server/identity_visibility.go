package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// identityHidden is the shared visibility rule for a pubkey's identity on
// per-node views such as Reach: an identity is hidden when it is
// node-blacklisted, observer-blacklisted, or when any of its known names —
// node name or observer name — starts with a configured hidden-name prefix
// (#1181). pubkey may be any case.
func identityHidden(cfg *Config, pubkey string, names ...string) bool {
	if cfg == nil {
		return false
	}
	if cfg.IsBlacklisted(pubkey) || cfg.IsObserverBlacklisted(pubkey) {
		return true
	}
	for _, name := range names {
		if cfg.IsNameHidden(name) {
			return true
		}
	}
	return false
}

// identityNamesSQL looks up every name a JSON array of lower-case pubkeys is
// known by, in one statement with fixed text (cheap to prepare, unlike a
// variable-length IN list):
//   - nodes and inactive_nodes: keys are lower-case (the ingestor normalises
//     them), so the primary-key index is used per pubkey. inactive_nodes holds
//     nodes aged out of `nodes`, whose adverts can still sit in a Reach window.
//   - observers: ids arrive raw from the MQTT topic in any case, so they are
//     matched case-insensitively in one pass over the (small) table.
const identityNamesSQL = `
	SELECT n.public_key, COALESCE(n.name, '') FROM json_each(?1) j JOIN nodes n ON n.public_key = j.value
	UNION ALL
	SELECT o.id, COALESCE(o.name, '') FROM observers o WHERE lower(o.id) IN (SELECT value FROM json_each(?1))`

const identityNamesInactiveSQL = identityNamesSQL + `
	UNION ALL
	SELECT i.public_key, COALESCE(i.name, '') FROM json_each(?1) j JOIN inactive_nodes i ON i.public_key = j.value`

// identityNames returns the current node, inactive-node and observer names of
// the given pubkeys, read live from the DB in one bulk query (no per-pubkey
// lookups), keyed by lowercase pubkey. A rename into a hidden prefix
// therefore takes effect on the next request, cached response or not.
// Returns (nil, nil) when no hidden-name prefix is configured: names cannot
// hide anything then.
func (s *Server) identityNames(pubkeys []string) (map[string][]string, error) {
	if s.cfg == nil || !s.cfg.HasHiddenNamePrefixes() || len(pubkeys) == 0 {
		return nil, nil
	}
	if s.db == nil || s.db.conn == nil {
		return nil, fmt.Errorf("identity names: no database")
	}
	seen := make(map[string]bool, len(pubkeys))
	keys := make([]string, 0, len(pubkeys))
	for _, pk := range pubkeys {
		pk = strings.ToLower(pk)
		if pk != "" && !seen[pk] {
			seen[pk] = true
			keys = append(keys, pk)
		}
	}
	arg, err := json.Marshal(keys)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(keys))
	add := func(pk, name string) {
		if name != "" {
			pk = strings.ToLower(pk)
			out[pk] = append(out[pk], name)
		}
	}
	q := identityNamesSQL
	hasInactive, err := s.hasInactiveNodesTable()
	if err != nil {
		return nil, fmt.Errorf("identity names: %w", err)
	}
	if hasInactive {
		q = identityNamesInactiveSQL
	}
	if err := s.scanIdentityNames(q, []interface{}{string(arg)}, add); err != nil {
		return nil, fmt.Errorf("identity names: %w", err)
	}
	return out, nil
}

// hasInactiveNodesTable reports whether inactive_nodes exists. Production
// schemas always have it (dbschema.AssertReady); minimal DBs may not. A
// positive answer is cached — tables are never dropped at runtime — while a
// negative one is re-probed so a table created later is picked up.
func (s *Server) hasInactiveNodesTable() (bool, error) {
	if s.reach.inactiveNodesTable.Load() {
		return true, nil
	}
	var n int
	err := s.db.conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'inactive_nodes'`).Scan(&n)
	if err != nil {
		return false, err
	}
	if n > 0 {
		s.reach.inactiveNodesTable.Store(true)
	}
	return n > 0, nil
}

func (s *Server) scanIdentityNames(q string, args []interface{}, add func(pk, name string)) error {
	rows, err := s.db.conn.Query(q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var pk, name string
		if err := rows.Scan(&pk, &name); err != nil {
			return err
		}
		add(pk, name)
	}
	return rows.Err()
}

// isIdentityHidden applies identityHidden to one pubkey with its live names.
// A failed name lookup is returned as an error so callers fail closed.
func (s *Server) isIdentityHidden(pubkey string) (bool, error) {
	if identityHidden(s.cfg, pubkey) {
		return true, nil
	}
	names, err := s.identityNames([]string{pubkey})
	if err != nil {
		return false, err
	}
	return identityHidden(s.cfg, pubkey, names[strings.ToLower(pubkey)]...), nil
}
