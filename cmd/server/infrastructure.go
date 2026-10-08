package main

import (
	"errors"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/channelregistry"
	"github.com/meshcore-analyzer/infrastructure"
)

// InfrastructureResponse deliberately contains only curation, not a second
// copy of /api/nodes. The browser joins this bounded set to its bulk node
// snapshot and calculates candidate suggestions from existing metrics.
type InfrastructureResponse struct {
	Selected []infrastructure.Entry `json:"selected"`
}

type InfrastructureAuthResponse struct {
	OK bool `json:"ok"`
}

func (s *Server) handleInfrastructureAuth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, InfrastructureAuthResponse{OK: true})
}

func (s *Server) infrastructureQueue() *channelregistry.Queue {
	// Reuse the existing atomic, bounded file transport in a separate
	// directory: OpSubmit maps to select and OpRevoke to remove. No channel
	// proposal consumer ever sees these command files.
	return channelregistry.NewQueue(infrastructure.QueuePath(s.db.path))
}

func (s *Server) handleInfrastructure(w http.ResponseWriter, r *http.Request) {
	if s.db == nil || s.db.path == "" {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	state, err := infrastructure.LoadDB(s.db.conn)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "infrastructure snapshot unavailable")
		return
	}
	if state.Selected == nil {
		state.Selected = []infrastructure.Entry{}
	}
	writeJSON(w, InfrastructureResponse{Selected: state.Selected})
}

func (s *Server) handleInfrastructureDecision(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.db == nil || s.db.path == "" {
			writeError(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}
		key := mux.Vars(r)["pubkey"]
		if !infrastructure.ValidPublicKey(key) {
			writeError(w, http.StatusBadRequest, "invalid public key")
			return
		}
		if op == infrastructure.OpSelect {
			known, err := infrastructure.KnownRepeater(s.db.conn, key)
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, "node lookup unavailable")
				return
			}
			if !known {
				writeError(w, http.StatusConflict, "node is not a known repeater")
				return
			}
		}
		id := channelregistry.NewID()
		cmd := channelregistry.Command{RequestID: id, CreatedAt: time.Now().UnixMilli()}
		if op == infrastructure.OpSelect {
			cmd.Op, cmd.Name = channelregistry.OpSubmit, key
		} else {
			cmd.Op, cmd.ProposalID = channelregistry.OpRevoke, key
		}
		if err := s.infrastructureQueue().Enqueue(cmd, 256); err != nil {
			if errors.Is(err, channelregistry.ErrQueueFull) {
				writeError(w, http.StatusTooManyRequests, "infrastructure queue full")
				return
			}
			writeError(w, http.StatusServiceUnavailable, "infrastructure queue unavailable")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, ChannelProposalAcceptedResponse{RequestID: id})
	}
}

func (s *Server) handleInfrastructureRequest(w http.ResponseWriter, r *http.Request) {
	if s.db == nil || s.db.path == "" {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	status, err := s.infrastructureQueue().Lookup(mux.Vars(r)["id"])
	if err != nil {
		writeError(w, http.StatusNotFound, "unknown or expired request")
		return
	}
	writeJSON(w, status)
}
