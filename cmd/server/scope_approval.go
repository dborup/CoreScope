package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/channelregistry"
	"github.com/meshcore-analyzer/regions"
)

type RegionScopeDecision struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	CreatedAt  int64  `json:"createdAt"`
	ReviewedAt int64  `json:"reviewedAt"`
}

type RegionScopeDecisionsResponse struct {
	Decisions []RegionScopeDecision `json:"decisions"`
}
type RegionScopeAudit struct {
	Name           string `json:"name"`
	Operation      string `json:"operation"`
	PreviousStatus string `json:"previousStatus,omitempty"`
	NewStatus      string `json:"newStatus"`
	DecidedAt      int64  `json:"decidedAt"`
}
type RegionScopeAuditResponse struct {
	Events []RegionScopeAudit `json:"events"`
}
type RegionScopeRequest struct {
	Name string `json:"name"`
}
type RegionScopeAccepted struct {
	RequestID string `json:"requestId"`
}
type ObserverNeighborsResponse struct {
	Neighbors     []AllObserverNeighborsEntry `json:"neighbors"`
	UnknownScopes []UnknownScopeEntry         `json:"unknownScopes"`
}

func (s *Server) regionScopeDecisions() ([]RegionScopeDecision, error) {
	if s.db == nil || s.db.conn == nil {
		return []RegionScopeDecision{}, nil
	}
	rows, err := s.db.conn.Query(`SELECT name,status,created_at,reviewed_at FROM approved_region_scopes ORDER BY name LIMIT 4096`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return []RegionScopeDecision{}, nil
		}
		return nil, err
	}
	defer rows.Close()
	out := []RegionScopeDecision{}
	for rows.Next() {
		var d RegionScopeDecision
		if err := rows.Scan(&d.Name, &d.Status, &d.CreatedAt, &d.ReviewedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Server) regionScopeQueue() *channelregistry.Queue {
	if s.db == nil || s.db.path == "" {
		return nil
	}
	return channelregistry.NewQueue(filepath.Join(filepath.Dir(s.db.path), "region-scope-requests"))
}

func (s *Server) handleRegionScopeDecisions(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	d, err := s.regionScopeDecisions()
	if err != nil {
		log.Printf("[scope-approval] decisions read failed: %v", err)
		writeError(w, 500, "could not read scope decisions")
		return
	}
	writeJSON(w, RegionScopeDecisionsResponse{Decisions: d})
}

func (s *Server) handleRegionScopeAudit(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if s.db == nil {
		writeJSON(w, RegionScopeAuditResponse{Events: []RegionScopeAudit{}})
		return
	}
	rows, err := s.db.conn.QueryContext(r.Context(), `SELECT name,operation,COALESCE(previous_status,''),new_status,decided_at FROM region_scope_audit ORDER BY decided_at DESC, request_id DESC LIMIT 100`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			writeJSON(w, RegionScopeAuditResponse{Events: []RegionScopeAudit{}})
			return
		}
		writeError(w, 500, "could not read scope audit")
		return
	}
	defer rows.Close()
	events := []RegionScopeAudit{}
	for rows.Next() {
		var e RegionScopeAudit
		if err := rows.Scan(&e.Name, &e.Operation, &e.PreviousStatus, &e.NewStatus, &e.DecidedAt); err != nil {
			writeError(w, 500, "could not read scope audit")
			return
		}
		events = append(events, e)
	}
	if rows.Err() != nil {
		writeError(w, 500, "could not read scope audit")
		return
	}
	writeJSON(w, RegionScopeAuditResponse{Events: events})
}

func (s *Server) handleRegionScopeDecision(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		q := s.regionScopeQueue()
		if q == nil {
			writeError(w, 503, "scope approval unavailable")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1024)
		var req RegionScopeRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, 400, "invalid request body")
			return
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			writeError(w, 400, "invalid request body")
			return
		}
		if err := regions.ValidateCandidate(req.Name); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		decisions, err := s.regionScopeDecisions()
		if err != nil {
			writeError(w, 500, "could not read scope decisions")
			return
		}
		current := ""
		for _, d := range decisions {
			if d.Name == req.Name {
				current = d.Status
				break
			}
		}
		if op == channelregistry.OpScopeRevoke {
			if current != "approved" {
				writeError(w, 409, "scope is not approved")
				return
			}
		} else {
			entries, err := s.db.GetAllObserverNeighbors()
			if err != nil {
				writeError(w, 500, "could not verify observed scope")
				return
			}
			if s.cfg != nil && len(s.cfg.ObserverBlacklist) > 0 {
				visible := entries[:0]
				for _, e := range entries {
					if !s.cfg.IsObserverBlacklisted(e.ObserverID) {
						visible = append(visible, e)
					}
				}
				entries = visible
			}
			candidates := computeUnknownScopes(entries, append(append([]string{}, s.cfg.EffectiveHashRegions()...), approvedRegionNames(decisions)...))
			found := false
			for _, c := range candidates {
				if c.Scope == req.Name {
					found = true
					break
				}
			}
			if !found && current != "rejected" {
				writeError(w, 409, "scope is not an observed unknown scope")
				return
			}
		}
		cmd := channelregistry.Command{RequestID: channelregistry.NewID(), Op: op, Name: req.Name, CreatedAt: time.Now().UnixMilli()}
		if err := q.Enqueue(cmd, 100); err != nil {
			log.Printf("[scope-approval] enqueue failed: %v", err)
			writeError(w, 503, "could not queue scope decision")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, RegionScopeAccepted{RequestID: cmd.RequestID})
	}
}

func approvedRegionNames(decisions []RegionScopeDecision) []string {
	names := make([]string, 0, len(decisions))
	for _, d := range decisions {
		if d.Status == "approved" {
			names = append(names, d.Name)
		}
	}
	return names
}

func (s *Server) handleRegionScopeRequestStatus(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	q := s.regionScopeQueue()
	if q == nil {
		writeError(w, 503, "scope approval unavailable")
		return
	}
	id := mux.Vars(r)["id"]
	st, err := q.Lookup(id)
	if err != nil {
		if errors.Is(err, channelregistry.ErrInvalidID) {
			writeError(w, 400, "invalid request id")
			return
		}
		if errors.Is(err, channelregistry.ErrUnknownRequest) {
			writeError(w, 404, "request not found")
			return
		}
		writeError(w, 500, "could not read request status")
		return
	}
	writeJSON(w, st)
}
