package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
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
// two read-only primary-key-sized lookups, run only on that 404 path. The
// visibility rule is identityHidden, applied to both names found here.
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

	// Observer ids arrive raw from the MQTT topic in any case; the table is
	// small, and this runs once per missing-node page view.
	var o observerRef
	err = s.db.conn.QueryRowContext(ctx, `SELECT id, COALESCE(name, ''), COALESCE(last_seen, '')
		FROM observers WHERE lower(id) = ? ORDER BY last_seen DESC LIMIT 1`, pk).Scan(&o.ID, &o.Name, &o.LastSeen)
	switch {
	case err == nil:
		resp.Observer = &o
	case !errors.Is(err, sql.ErrNoRows):
		return resp, err
	}

	var names []string
	if resp.InactiveNode != nil {
		names = append(names, resp.InactiveNode.Name)
	}
	if resp.Observer != nil {
		names = append(names, resp.Observer.Name)
	}
	if identityHidden(s.cfg, pk, names...) {
		return nodeNotFoundResponse{Error: "Not found"}, nil
	}
	return resp, nil
}

// writeNodeNotFound answers a node-detail miss. A failed lookup falls back to
// the bare 404: it only enriches the error and must not turn it into a 500.
func (s *Server) writeNodeNotFound(w http.ResponseWriter, r *http.Request, pubkey string) {
	resp, err := s.lookupMissingNode(r.Context(), pubkey)
	if err != nil {
		resp = nodeNotFoundResponse{Error: "Not found"}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("[routes] JSON encode error: %v", err)
	}
}
