package main

import (
	"context"
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

// hiddenNamesSQL builds the lookup for a JSON array (?1) of lower-case
// pubkeys that returns only names starting with one of the n hidden-name
// prefixes bound as ?2..?n+1 — normally no rows at all, so a Reach serve does
// not scan a row per listed identity. The prefix test is inlined per prefix
// (usually one) and compares bytes (substr/length on BLOB), i.e. exactly
// strings.HasPrefix as IsNameHidden applies it; NULL names never match.
//   - nodes and inactive_nodes: keys are lower-case (the ingestor normalises
//     them), so the primary-key index is used per pubkey. inactive_nodes holds
//     nodes aged out of `nodes`, whose adverts can still sit in a Reach window.
//     Its row is never removed when a node returns, so an inactive name only
//     counts while the pubkey has no nodes row with a name — a current name
//     supersedes it; a nameless return (stored as an empty name) does not.
//   - observers: ids arrive raw from the MQTT topic in any case, so they are
//     matched case-insensitively; the cheap prefix test runs first.
func hiddenNamesSQL(n int, withInactive bool) string {
	match := func(col string) string {
		terms := make([]string, n)
		for i := range terms {
			terms[i] = fmt.Sprintf("substr(CAST(%s AS BLOB), 1, length(CAST(?%d AS BLOB))) = CAST(?%d AS BLOB)", col, i+2, i+2)
		}
		return "(" + strings.Join(terms, " OR ") + ")"
	}
	q := `SELECT n.public_key, n.name FROM json_each(?1) j JOIN nodes n ON n.public_key = j.value WHERE ` + match("n.name") + `
	UNION ALL
	SELECT o.id, o.name FROM observers o WHERE ` + match("o.name") + ` AND lower(o.id) IN (SELECT value FROM json_each(?1))`
	if withInactive {
		q += `
	UNION ALL
	SELECT i.public_key, i.name FROM json_each(?1) j JOIN inactive_nodes i ON i.public_key = j.value
	WHERE ` + match("i.name") + `
	  AND NOT EXISTS (SELECT 1 FROM nodes n WHERE n.public_key = j.value AND COALESCE(n.name, '') <> '')`
	}
	return q
}

// hiddenIdentityNames returns, keyed by lowercase pubkey, the current names of
// the given pubkeys that start with a hidden-name prefix: node, inactive-node
// (see above) and observer names, read live from the DB in one bulk query (no
// per-pubkey lookups). A rename into a hidden prefix therefore takes effect
// on the next request, cached response or not. Returns (nil, nil) when no
// hidden-name prefix is configured: names cannot hide anything then.
func (s *Server) hiddenIdentityNames(ctx context.Context, pubkeys []string) (map[string][]string, error) {
	prefixes := s.cfg.EnforcedHiddenNamePrefixes()
	if len(prefixes) == 0 || len(pubkeys) == 0 {
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
	keysArg, err := json.Marshal(keys)
	if err != nil {
		return nil, err
	}
	hasInactive, err := s.hasInactiveNodesTable(ctx)
	if err != nil {
		return nil, fmt.Errorf("identity names: %w", err)
	}
	args := make([]interface{}, 0, 1+len(prefixes))
	args = append(args, string(keysArg))
	for _, p := range prefixes {
		args = append(args, p)
	}
	rows, err := s.db.conn.QueryContext(ctx, hiddenNamesSQL(len(prefixes), hasInactive), args...)
	if err != nil {
		return nil, fmt.Errorf("identity names: %w", err)
	}
	defer rows.Close()
	out := make(map[string][]string)
	for rows.Next() {
		var pk, name string
		if err := rows.Scan(&pk, &name); err != nil {
			return nil, fmt.Errorf("identity names: %w", err)
		}
		pk = strings.ToLower(pk)
		out[pk] = append(out[pk], name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("identity names: %w", err)
	}
	return out, nil
}

// hasInactiveNodesTable reports whether inactive_nodes exists. Production
// schemas always have it (dbschema.AssertReady); minimal DBs may not. A
// positive answer is cached — tables are never dropped at runtime — while a
// negative one is re-probed so a table created later is picked up.
func (s *Server) hasInactiveNodesTable(ctx context.Context) (bool, error) {
	if s.reach.inactiveNodesTable.Load() {
		return true, nil
	}
	var n int
	err := s.db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'inactive_nodes'`).Scan(&n)
	if err != nil {
		return false, err
	}
	if n > 0 {
		s.reach.inactiveNodesTable.Store(true)
	}
	return n > 0, nil
}

// isIdentityHidden applies identityHidden to one pubkey with its live names.
// A failed name lookup is returned as an error so callers fail closed.
func (s *Server) isIdentityHidden(ctx context.Context, pubkey string) (bool, error) {
	if identityHidden(s.cfg, pubkey) {
		return true, nil
	}
	names, err := s.hiddenIdentityNames(ctx, []string{pubkey})
	if err != nil {
		return false, err
	}
	return identityHidden(s.cfg, pubkey, names[strings.ToLower(pubkey)]...), nil
}
