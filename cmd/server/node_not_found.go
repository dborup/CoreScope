package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// nodeNotFoundResponse is the 404 body of GET /api/nodes/{pubkey} (#199).
// Beyond the error it carries what this instance still knows about the key,
// so the node page can explain the state instead of dead-ending on
// "Node not found":
//   - InactiveNode: the inactive_nodes row, i.e. the node was retired by the
//     ingestor's retention (no advert in retention.nodeDays). LastSeen is the
//     last advert.
//   - Observer: the observers row, when the pubkey also uploads as an
//     observer (an observer may never have had a node row at all).
//
// Both are omitted for an unknown key, and for a blacklisted or hidden
// identity, whose 404 stays bare.
type nodeNotFoundResponse struct {
	Error        string            `json:"error"`
	InactiveNode *inactiveNodeInfo `json:"inactive_node,omitempty"`
	Observer     *observerRef      `json:"observer,omitempty"`
}

type inactiveNodeInfo struct {
	PublicKey string `json:"public_key"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	LastSeen  string `json:"last_seen"`
	FirstSeen string `json:"first_seen"`
}

type observerRef struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	LastSeen string `json:"last_seen"`
}

// lookupMissingNode builds the 404 body for a pubkey that has no nodes row:
// two read-only lookups, run only on that 404 path. The visibility rule is
// identityHidden, applied to every name found here -- the inactive node's and
// one per observer alias of the key (#286).
func (s *Server) lookupMissingNode(ctx context.Context, pubkey string) (nodeNotFoundResponse, error) {
	resp := nodeNotFoundResponse{Error: "Not found"}
	if s.db == nil || s.db.conn == nil {
		return resp, nil
	}
	pk := strings.ToLower(pubkey)

	hasInactive, err := s.hasInactiveNodesTable(ctx)
	if err != nil {
		return resp, err
	}
	if hasInactive {
		var n inactiveNodeInfo
		err := s.db.conn.QueryRowContext(ctx, `SELECT public_key, COALESCE(name, ''), COALESCE(role, ''), COALESCE(last_seen, ''), COALESCE(first_seen, '')
			FROM inactive_nodes WHERE public_key = ?`, pk).Scan(&n.PublicKey, &n.Name, &n.Role, &n.LastSeen, &n.FirstSeen)
		switch {
		case err == nil:
			resp.InactiveNode = &n
		case !errors.Is(err, sql.ErrNoRows):
			return resp, err
		}
	}

	// Observer ids arrive raw from the MQTT topic in any case, so one pubkey
	// can hold several rows that differ only in letter case. Every row is
	// read, not just the newest: all their names feed the visibility check
	// below, so a hidden name on an older alias still hides the identity
	// (#286). The newest row is the one returned, by the same ordering, with
	// the id as a tie-break so a tie is answered deterministically. The table
	// is small, aliases of one key are a handful, and this runs once per
	// missing-node page view.
	observerNames, observer, err := s.missingNodeObservers(ctx, pk)
	if err != nil {
		return resp, err
	}
	resp.Observer = observer

	names := observerNames
	if resp.InactiveNode != nil {
		names = append(names, resp.InactiveNode.Name)
	}
	if identityHidden(s.cfg, pk, names...) {
		return nodeNotFoundResponse{Error: "Not found"}, nil
	}
	return resp, nil
}

// missingNodeObservers returns the names of every observers row whose id is
// pk ignoring case, plus the newest of those rows, or nil when the pubkey
// never uploaded as an observer (#286).
func (s *Server) missingNodeObservers(ctx context.Context, pk string) ([]string, *observerRef, error) {
	rows, err := s.db.conn.QueryContext(ctx, `SELECT id, COALESCE(name, ''), COALESCE(last_seen, '')
		FROM observers WHERE lower(id) = ? ORDER BY last_seen DESC, id`, pk)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var (
		names  []string
		newest *observerRef
	)
	for rows.Next() {
		var o observerRef
		if err := rows.Scan(&o.ID, &o.Name, &o.LastSeen); err != nil {
			return nil, nil, err
		}
		names = append(names, o.Name)
		if newest == nil {
			newest = &o
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return names, newest, nil
}

// missingNodeLogEvery bounds the log of failed missing-node lookups (#208):
// the first failure is logged at once, later ones at most once per interval.
const missingNodeLogEvery = 10 * time.Minute

// missingNodeLookupLog throttles that log. The zero value is ready.
type missingNodeLookupLog struct {
	mu         sync.Mutex
	last       time.Time
	suppressed int
}

// note reports whether a failure at now is logged and, if so, how many
// failures were suppressed since the previous logged one.
func (l *missingNodeLookupLog) note(now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.last.IsZero() && now.Sub(l.last) < missingNodeLogEvery {
		l.suppressed++
		return false, 0
	}
	n := l.suppressed
	l.last, l.suppressed = now, 0
	return true, n
}

// writeNodeNotFound answers a node-detail miss. A failed lookup falls back to
// the bare 404: it only enriches the error and must not turn it into a 500.
// The failure is logged, throttled, so a broken lookup (e.g. schema drift on
// inactive_nodes) does not pass for a plain miss (#208). A request its client
// cancelled is not a broken lookup and is not logged. The requested key is
// left out of the line: it is client input and adds nothing to the error.
func (s *Server) writeNodeNotFound(w http.ResponseWriter, r *http.Request, pubkey string) {
	resp, err := s.lookupMissingNode(r.Context(), pubkey)
	if err != nil {
		resp = nodeNotFoundResponse{Error: "Not found"}
		if r.Context().Err() == nil {
			if logIt, suppressed := s.missingNodeLog.note(time.Now()); logIt {
				log.Printf("[routes] missing-node lookup failed, answering a bare 404: %v (%d more suppressed since the last report; next report in %v at the earliest)",
					err, suppressed, missingNodeLogEvery)
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("[routes] JSON encode error: %v", err)
	}
}
