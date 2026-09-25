package main

// Shared channel proposals — the read-only server side.
//
// Anyone may suggest a public hashtag channel; an administrator (the existing
// apiKey) approves or rejects it. The server never writes SQLite: every
// suggestion and decision becomes one atomic command file in the
// channelregistry queue next to the database, and the ingestor applies it and
// writes a result file the server reads back (cmd/ingestor/channel_proposals.go).
//
// Bounds: the queue holds at most MaxQueuedRequests commands, submissions are
// capped by a global sliding window of SubmissionsPerHour (a per-client limit
// would be unreliable behind a reverse proxy — see ws_limits.go), the ingestor
// enforces MaxPending/MaxApproved, and every list read is capped.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/channelregistry"
)

// approvedCacheTTL bounds how stale GET /api/channels' approvedChannels can
// be. A request-status read that observes an approval invalidates it early.
const approvedCacheTTL = 10 * time.Second

// maxProposalBodyBytes caps the POST body; a name is at most 31 bytes.
const maxProposalBodyBytes = 1024

// ChannelProposalConfigResponse is GET /api/channel-proposals/config.
type ChannelProposalConfigResponse struct {
	Enabled bool `json:"enabled"`
}

// ChannelProposalSubmitRequest is the body of POST /api/channel-proposals.
type ChannelProposalSubmitRequest struct {
	Name string `json:"name"`
}

// ChannelProposalAcceptedResponse is the 202 body of every queued request.
type ChannelProposalAcceptedResponse struct {
	RequestID string `json:"requestId"`
}

// ChannelProposalListResponse is GET /api/admin/channel-proposals.
type ChannelProposalListResponse struct {
	Proposals []channelregistry.Proposal `json:"proposals"`
	// Enabled reports whether public submissions are currently open.
	Enabled bool `json:"enabled"`
}

type approvedSnapshot struct {
	channels []ApprovedChannel
	expires  time.Time
}

type channelProposalService struct {
	enabled bool // public submissions open
	limits  channelregistry.Limits
	queue   *channelregistry.Queue // nil when there is no database path
	db      *sql.DB                // read-only; nil without a database
	now     func() time.Time

	rateMu sync.Mutex
	recent []time.Time // accepted submissions in the last hour, oldest first

	approvedMu sync.Mutex
	approved   *approvedSnapshot
}

func newChannelProposalService(cfg *Config, db *sql.DB, queueDir string) *channelProposalService {
	var pc *channelregistry.Config
	strongKey := false
	if cfg != nil {
		pc = cfg.ChannelProposals
		strongKey = cfg.APIKey != "" && !IsWeakAPIKey(cfg.APIKey)
	}
	svc := &channelProposalService{
		limits: pc.Limits(),
		db:     db,
		now:    time.Now,
	}
	if queueDir != "" {
		svc.queue = channelregistry.NewQueue(queueDir)
	}
	svc.enabled = pc.SubmissionsRequested() && strongKey && svc.queue != nil && db != nil
	if pc.SubmissionsRequested() && !svc.enabled {
		log.Printf("[channel-proposals] WARNING: channelProposals.enabled is set but public suggestions stay closed: " +
			"they need a strong apiKey (16+ characters, not a placeholder) and a database")
	}
	return svc
}

// channelProposals returns the service, building it on first use.
func (s *Server) channelProposals() *channelProposalService {
	s.proposalsOnce.Do(func() {
		if s.proposals != nil {
			return
		}
		var conn *sql.DB
		queueDir := ""
		if s.db != nil {
			conn = s.db.conn
			if s.db.path != "" {
				queueDir = channelregistry.QueueDir(s.db.path)
			}
		}
		s.proposals = newChannelProposalService(s.cfg, conn, queueDir)
	})
	return s.proposals
}

// allowSubmission records a submission if the hourly budget allows it, or
// reports how long until it does.
func (p *channelProposalService) allowSubmission() (bool, time.Duration) {
	p.rateMu.Lock()
	defer p.rateMu.Unlock()
	now := p.now()
	cutoff := now.Add(-time.Hour)
	i := 0
	for i < len(p.recent) && !p.recent[i].After(cutoff) {
		i++
	}
	p.recent = p.recent[i:]
	if len(p.recent) >= p.limits.SubmissionsPerHour {
		return false, p.recent[0].Add(time.Hour).Sub(now)
	}
	p.recent = append(p.recent, now)
	return true, 0
}

// refundSubmission gives back the budget of a submission that was not queued.
func (p *channelProposalService) refundSubmission() {
	p.rateMu.Lock()
	defer p.rateMu.Unlock()
	if n := len(p.recent); n > 0 {
		p.recent = p.recent[:n-1]
	}
}

// approvedChannels returns the approved channels for GET /api/channels from a
// short-lived cache. It returns a fresh slice the caller may keep; the cached
// one is never handed out or mutated.
func (p *channelProposalService) approvedChannels(ctx context.Context) []ApprovedChannel {
	if p == nil || p.db == nil {
		return nil
	}
	p.approvedMu.Lock()
	defer p.approvedMu.Unlock()
	now := p.now()
	if p.approved == nil || now.After(p.approved.expires) {
		names, err := channelregistry.ListApprovedNames(ctx, p.db, channelregistry.MaxListed)
		switch {
		case err == nil:
			chans := make([]ApprovedChannel, len(names))
			for i, n := range names {
				chans[i] = ApprovedChannel{Name: n, Hash: n}
			}
			p.approved = &approvedSnapshot{channels: chans, expires: now.Add(approvedCacheTTL)}
		case p.approved == nil:
			log.Printf("[channel-proposals] listing approved channels failed: %v", err)
			return nil
		default:
			// Keep serving the last good snapshot; retry after another TTL.
			log.Printf("[channel-proposals] listing approved channels failed, serving cached list: %v", err)
			p.approved.expires = now.Add(approvedCacheTTL)
		}
	}
	if len(p.approved.channels) == 0 {
		return nil
	}
	out := make([]ApprovedChannel, len(p.approved.channels))
	copy(out, p.approved.channels)
	return out
}

func (p *channelProposalService) invalidateApproved() {
	p.approvedMu.Lock()
	p.approved = nil
	p.approvedMu.Unlock()
}

func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
}

func setRetryAfter(w http.ResponseWriter, d time.Duration) {
	w.Header().Set("Retry-After", fmt.Sprintf("%d", int(math.Ceil(d.Seconds()))))
}

// GET /api/channel-proposals/config
func (s *Server) handleChannelProposalConfig(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	writeJSON(w, ChannelProposalConfigResponse{Enabled: s.channelProposals().enabled})
}

// POST /api/channel-proposals
func (s *Server) handleChannelProposalSubmit(w http.ResponseWriter, r *http.Request) {
	p := s.channelProposals()
	noStore(w)
	if !p.enabled {
		writeError(w, http.StatusForbidden, "channel suggestions are disabled")
		return
	}
	var body ChannelProposalSubmitRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxProposalBodyBytes)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name, err := channelregistry.NormalizeName(body.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ok, wait := p.allowSubmission()
	if !ok {
		setRetryAfter(w, wait)
		writeError(w, http.StatusTooManyRequests, "too many suggestions right now, please try again later")
		return
	}
	cmd := channelregistry.Command{
		RequestID: channelregistry.NewID(),
		Op:        channelregistry.OpSubmit,
		Name:      name,
		CreatedAt: p.now().UnixMilli(),
	}
	if !s.enqueueProposalCommand(w, p, cmd) {
		p.refundSubmission()
	}
}

// enqueueProposalCommand writes cmd and answers 202, or answers the error.
func (s *Server) enqueueProposalCommand(w http.ResponseWriter, p *channelProposalService, cmd channelregistry.Command) bool {
	if p.queue == nil {
		writeError(w, http.StatusServiceUnavailable, "channel suggestions are unavailable")
		return false
	}
	if err := p.queue.Enqueue(cmd, p.limits.MaxQueuedRequests); err != nil {
		if errors.Is(err, channelregistry.ErrQueueFull) {
			setRetryAfter(w, time.Minute)
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return false
		}
		log.Printf("[channel-proposals] enqueue %s failed: %v", cmd.Op, err)
		writeError(w, http.StatusInternalServerError, "could not queue the request")
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(ChannelProposalAcceptedResponse{RequestID: cmd.RequestID})
	return true
}

// GET /api/channel-proposals/requests/{requestId}
func (s *Server) handleChannelProposalRequest(w http.ResponseWriter, r *http.Request) {
	p := s.channelProposals()
	noStore(w)
	id := mux.Vars(r)["requestId"]
	if !channelregistry.ValidID(id) {
		writeError(w, http.StatusBadRequest, "invalid request id")
		return
	}
	if p.queue == nil {
		writeError(w, http.StatusNotFound, channelregistry.ErrUnknownRequest.Error())
		return
	}
	st, err := p.queue.Lookup(id)
	if err != nil {
		if errors.Is(err, channelregistry.ErrUnknownRequest) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		log.Printf("[channel-proposals] lookup %s failed: %v", id, err)
		writeError(w, http.StatusInternalServerError, "could not read the request status")
		return
	}
	// Both an approval and a revocation change the approvedChannels list;
	// invalidate the same single cached snapshot early for either so a
	// caller does not have to wait out approvedCacheTTL to see the result.
	if st.Status == channelregistry.RequestApproved || st.Status == channelregistry.RequestRevoked {
		p.invalidateApproved()
	}
	writeJSON(w, st)
}

// GET /api/admin/channel-proposals?status=pending|approved|rejected|revoked
func (s *Server) handleAdminChannelProposals(w http.ResponseWriter, r *http.Request) {
	p := s.channelProposals()
	noStore(w)
	status := r.URL.Query().Get("status")
	if status != "" && !channelregistry.ValidStatus(status) {
		writeError(w, http.StatusBadRequest, "status must be pending, approved, rejected or revoked")
		return
	}
	resp := ChannelProposalListResponse{Proposals: []channelregistry.Proposal{}, Enabled: p.enabled}
	if p.db != nil {
		limit := p.limits.MaxPending + p.limits.MaxApproved + p.limits.MaxQueuedRequests
		list, err := channelregistry.ListProposals(r.Context(), p.db, status, limit)
		if err != nil {
			log.Printf("[channel-proposals] admin list failed: %v", err)
			writeError(w, http.StatusInternalServerError, "could not list suggestions")
			return
		}
		resp.Proposals = list
	}
	writeJSON(w, resp)
}

// loadAdminProposal validates id and loads the proposal, writing the
// 400/404/500 response itself and returning ok=false when the caller must
// not proceed. Shared by the decision handler below and the revoke handler.
func (s *Server) loadAdminProposal(w http.ResponseWriter, r *http.Request, p *channelProposalService, id string) (proposal channelregistry.Proposal, ok bool) {
	if !channelregistry.ValidID(id) {
		writeError(w, http.StatusBadRequest, "invalid suggestion id")
		return channelregistry.Proposal{}, false
	}
	if p.db == nil {
		writeError(w, http.StatusNotFound, "suggestion not found")
		return channelregistry.Proposal{}, false
	}
	proposal, found, err := channelregistry.GetProposal(r.Context(), p.db, id)
	if err != nil {
		log.Printf("[channel-proposals] get %s failed: %v", id, err)
		writeError(w, http.StatusInternalServerError, "could not read the suggestion")
		return channelregistry.Proposal{}, false
	}
	if !found {
		writeError(w, http.StatusNotFound, "suggestion not found")
		return channelregistry.Proposal{}, false
	}
	return proposal, true
}

// POST /api/admin/channel-proposals/{id}/approve and .../reject
func (s *Server) handleAdminChannelProposalDecision(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := s.channelProposals()
		noStore(w)
		id := mux.Vars(r)["id"]
		if _, ok := s.loadAdminProposal(w, r, p, id); !ok {
			return
		}
		s.enqueueProposalCommand(w, p, channelregistry.Command{
			RequestID:  channelregistry.NewID(),
			Op:         op,
			ProposalID: id,
			CreatedAt:  p.now().UnixMilli(),
		})
	}
}

// POST /api/admin/channel-proposals/{id}/revoke undoes a previous approval.
//
// Unlike approve/reject, this performs a synchronous precondition check
// before enqueueing anything: only a currently-approved proposal may be
// revoked, and a caller who asks to revoke something else gets an
// immediate 409 with no side effect at all (no command file is written).
// This is a deliberate, narrow exception to the rest of this package's
// "let the ingestor validate asynchronously" pattern — approve/reject
// always answer 202 and let a conflict surface later via polling
// GET /api/channel-proposals/requests/{requestId} — because the caller
// here specifically wants a synchronous, side-effect-free rejection for
// the common case of "it's not approved anymore" instead of having to
// poll a request that was never going to do anything.
//
// There is still a real TOCTOU window between this check and the ingestor
// actually applying the command: another admin could approve, reject or
// revoke the same proposal in the meantime. reviewChannelProposal in the
// ingestor (cmd/ingestor/channel_proposals.go) re-validates the status
// from scratch under its own transaction and is the true source of truth;
// the 409 here is only a best-effort fast-fail for the common case, not a
// substitute for that re-validation.
func (s *Server) handleAdminChannelProposalRevoke(w http.ResponseWriter, r *http.Request) {
	p := s.channelProposals()
	noStore(w)
	id := mux.Vars(r)["id"]
	proposal, ok := s.loadAdminProposal(w, r, p, id)
	if !ok {
		return
	}
	if proposal.Status != channelregistry.StatusApproved {
		writeError(w, http.StatusConflict, "suggestion is not currently approved")
		return
	}
	s.enqueueProposalCommand(w, p, channelregistry.Command{
		RequestID:  channelregistry.NewID(),
		Op:         channelregistry.OpRevoke,
		ProposalID: id,
		CreatedAt:  p.now().UnixMilli(),
	})
}
