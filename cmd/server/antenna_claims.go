package main

// Owner antenna-height claims (#789 M2a) — the read-only server side.
//
// An owner asks for a claim (a height, or a clear) on a node; the server
// answers with a random single-use code the owner puts into the node's advert
// name. The ingestor matches the code at advert ingest and owns every write
// (cmd/ingestor/antenna_claims.go). This file only:
//   - issues claims and queues admin decisions as command files in the
//     antenna-claim queue next to the database (#1283: no SQLite writes);
//   - reads the approved heights into a short-lived snapshot for the
//     profile's antenna precedence (owner, below operator-node, above the
//     DSM estimate; owner decision 1);
//   - serves the request status and the admin list from read-only queries.
//
// Hidden identities (#743): issuing, the request status, the admin routes
// and serving a height all refuse a hidden node exactly like an unknown one.
//
// Bounds: public claims open only with antennaClaims.enabled and a strong
// apiKey (owner decision 5); a global sliding window of SubmissionsPerHour;
// MaxQueuedRequests command files; the ingestor enforces MaxPending and
// MaxPendingPerNode; every list is capped.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/channelregistry"
	"github.com/meshcore-analyzer/channelregistry/antennaclaim"
	"github.com/meshcore-analyzer/dhm"
	"golang.org/x/sync/singleflight"
)

const (
	// ownerHeightsTTL bounds how stale the served owner heights can be.
	ownerHeightsTTL = 30 * time.Second
	// maxOwnerHeights caps the snapshot: one approved claim per node.
	maxOwnerHeights = 65536
	// maxAdminClaimsListed caps the admin list.
	maxAdminClaimsListed = 1000
	// maxClaimBodyBytes caps the POST body.
	maxClaimBodyBytes = 1024
)

// AntennaClaimRequest is the body of POST /api/nodes/{pubkey}/antenna-claims:
// exactly one of heightM (0–100 m, 0.1 m steps) or clear:true.
type AntennaClaimRequest struct {
	HeightM *float64 `json:"heightM,omitempty"`
	Clear   bool     `json:"clear,omitempty"`
}

// AntennaClaimIssuedResponse is the 202 body of a queued claim. The code is
// shown only here, to the requester.
type AntennaClaimIssuedResponse struct {
	RequestID string `json:"requestId"`
	Code      string `json:"code"`
	ExpiresAt int64  `json:"expiresAt"`
	// NameBudgetBytes is the advert name's bytes on air with a position:
	// a code past it is cut by the firmware and never proves anything.
	NameBudgetBytes int    `json:"nameBudgetBytes"`
	SuggestedName   string `json:"suggestedName"`
}

// AntennaClaimStatusResponse is GET /api/antenna-claims/requests/{id}.
type AntennaClaimStatusResponse struct {
	Status    string `json:"status"`
	ExpiresAt int64  `json:"expiresAt,omitempty"`
	Error     string `json:"error,omitempty"`
}

// AntennaClaimAcceptedResponse is the 202 body of a queued admin decision.
type AntennaClaimAcceptedResponse struct {
	RequestID string `json:"requestId"`
}

// AdminAntennaClaim is one row of GET /api/admin/antenna-claims. No code.
type AdminAntennaClaim struct {
	ID          string   `json:"id"`
	PubKey      string   `json:"pubkey"`
	Name        string   `json:"name"`
	Role        string   `json:"role"`
	HeightM     *float64 `json:"heightM"` // null: a clear claim
	Status      string   `json:"status"`
	NeedsReview bool     `json:"needsReview,omitempty"`
	CreatedAt   int64    `json:"createdAt"`
	ExpiresAt   int64    `json:"expiresAt"`
	ProvenAt    *int64   `json:"provenAt,omitempty"`
	ProofHash   string   `json:"proofHash,omitempty"`
	ProofLat    *float64 `json:"proofLat,omitempty"`
	ProofLon    *float64 `json:"proofLon,omitempty"`
	ReviewedAt  *int64   `json:"reviewedAt,omitempty"`
	ApprovedBy  string   `json:"approvedBy,omitempty"`
	// MovedM is the distance from the proof position to the node's current
	// position; Stale when it is more than StaleMoveM (the height is then
	// not served).
	MovedM *float64 `json:"movedM,omitempty"`
	Stale  bool     `json:"stale,omitempty"`
}

// AdminAntennaClaimListResponse is GET /api/admin/antenna-claims.
type AdminAntennaClaimListResponse struct {
	Claims      []AdminAntennaClaim `json:"claims"`
	Enabled     bool                `json:"enabled"`
	AutoApprove bool                `json:"autoApprove"`
}

// ownerHeight is one approved height and where it was proven.
type ownerHeight struct {
	heightM  float64
	lat, lon float64
}

type antennaClaimService struct {
	enabled     bool
	autoApprove bool
	limits      antennaclaim.Limits
	queue       *channelregistry.Queue // nil without a database path
	db          *sql.DB                // read-only; nil without a database
	now         func() time.Time
	submissionWindow

	ownersMu      sync.Mutex
	owners        map[string]ownerHeight // immutable once stored
	ownersExpires time.Time
	ownersLoad    singleflight.Group
}

func newAntennaClaimService(cfg *Config, db *sql.DB, queueDir string) *antennaClaimService {
	var cc *antennaclaim.Config
	strongKey := false
	if cfg != nil {
		cc = cfg.AntennaClaims
		strongKey = cfg.APIKey != "" && !IsWeakAPIKey(cfg.APIKey)
	}
	svc := &antennaClaimService{
		limits:      cc.Limits(),
		autoApprove: cc.AutoApprove(),
		db:          db,
		now:         time.Now,
	}
	if queueDir != "" {
		svc.queue = channelregistry.NewQueue(queueDir)
	}
	svc.enabled = cc.SubmissionsRequested() && strongKey && svc.queue != nil && db != nil
	if cc.SubmissionsRequested() && !svc.enabled {
		log.Printf("[antenna-claims] WARNING: antennaClaims.enabled is set but owner claims stay closed: " +
			"they need a strong apiKey (16+ characters, not a placeholder) and a database")
	}
	return svc
}

// antennaClaims returns the service, building it on first use.
func (s *Server) antennaClaims() *antennaClaimService {
	s.claimsOnce.Do(func() {
		if s.claims != nil {
			return
		}
		var conn *sql.DB
		queueDir := ""
		if s.db != nil {
			conn = s.db.conn
			if s.db.path != "" {
				queueDir = antennaclaim.QueueDir(s.db.path)
			}
		}
		s.claims = newAntennaClaimService(s.cfg, conn, queueDir)
	})
	return s.claims
}

// ── Owner heights (read side) ─────────────────────────────────────────────

func isMissingClaimsTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table: node_antenna_claims")
}

// ownerHeights returns the approved heights by public key from a snapshot
// refreshed every ownerHeightsTTL. The read runs outside ownersMu, one at a
// time; while it runs the previous snapshot is served. A failed read keeps
// the last good snapshot. The map must not be modified.
func (c *antennaClaimService) ownerHeights(ctx context.Context) map[string]ownerHeight {
	if c == nil || c.db == nil {
		return nil
	}
	c.ownersMu.Lock()
	cur, fresh := c.owners, c.owners != nil && c.now().Before(c.ownersExpires)
	c.ownersMu.Unlock()
	if fresh {
		return cur
	}
	ctx = context.WithoutCancel(ctx)
	v, _, _ := c.ownersLoad.Do("owners", func() (interface{}, error) {
		next, err := c.readOwnerHeights(ctx)
		c.ownersMu.Lock()
		defer c.ownersMu.Unlock()
		if err != nil {
			log.Printf("[antenna-claims] reading owner heights failed: %v", err)
			if c.owners == nil {
				c.owners = map[string]ownerHeight{}
			}
		} else {
			c.owners = next
		}
		c.ownersExpires = c.now().Add(ownerHeightsTTL)
		return c.owners, nil
	})
	return v.(map[string]ownerHeight)
}

// invalidateOwners drops the snapshot (tests).
func (c *antennaClaimService) invalidateOwners() {
	c.ownersMu.Lock()
	c.owners = nil
	c.ownersMu.Unlock()
}

// readOwnerHeights is one indexed SELECT of the approved heights with a
// proof position; a missing table (an ingestor older than #789 M2a) is none.
func (c *antennaClaimService) readOwnerHeights(ctx context.Context) (map[string]ownerHeight, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT pubkey, height_m, proof_lat, proof_lon FROM node_antenna_claims
		WHERE status = 'approved' AND height_m IS NOT NULL AND proof_lat IS NOT NULL AND proof_lon IS NOT NULL
		ORDER BY reviewed_at LIMIT ?`, maxOwnerHeights)
	if isMissingClaimsTable(err) {
		return map[string]ownerHeight{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ownerHeight{}
	for rows.Next() {
		var pk string
		var h ownerHeight
		if err := rows.Scan(&pk, &h.heightM, &h.lat, &h.lon); err != nil {
			return nil, err
		}
		if antennaclaim.ValidHeight(h.heightM) {
			out[strings.ToLower(pk)] = h // the latest approval wins
		}
	}
	return out, rows.Err()
}

// ownerHeightAt is the approved owner height of node pubkey standing at pt,
// or nil: none, or the node has moved more than StaleMoveM from where the
// height was proven (stale: fall back, no write). The caller has already
// refused hidden nodes.
func (c *antennaClaimService) ownerHeightAt(ctx context.Context, pubkey string, pt dhm.LatLon) *float64 {
	h, ok := c.ownerHeights(ctx)[strings.ToLower(pubkey)]
	if !ok || dhm.GroundDistance(pt, dhm.LatLon{Lat: h.lat, Lon: h.lon}) > antennaclaim.StaleMoveM {
		return nil
	}
	v := h.heightM
	return &v
}

// attachOwnerHeights sets owner on the node endpoints that have one.
func (s *Server) attachOwnerHeights(ctx context.Context, ends *[2]terrainEnd) {
	for i := range ends {
		if ends[i].kind == "node" && ends[i].pubkey != "" {
			ends[i].owner = s.antennaClaims().ownerHeightAt(ctx, ends[i].pubkey, ends[i].pt)
		}
	}
}

// ── Public routes ─────────────────────────────────────────────────────────

// POST /api/nodes/{pubkey}/antenna-claims
func (s *Server) handleAntennaClaimSubmit(w http.ResponseWriter, r *http.Request) {
	c := s.antennaClaims()
	noStore(w)
	if !c.enabled {
		writeError(w, http.StatusForbidden, "antenna height claims are disabled")
		return
	}
	var body AntennaClaimRequest
	if err := decodeSingleJSONObject(http.MaxBytesReader(w, r.Body, maxClaimBodyBytes), &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if (body.HeightM != nil) == body.Clear {
		writeError(w, http.StatusBadRequest, "give either heightM or clear:true")
		return
	}
	if body.HeightM != nil && !antennaclaim.ValidHeight(*body.HeightM) {
		writeError(w, http.StatusBadRequest, "heightM must be 0-100 m in steps of 0.1 m")
		return
	}
	pk := strings.ToLower(mux.Vars(r)["pubkey"])
	if !antennaclaim.ValidPubKey(pk) {
		writeError(w, http.StatusBadRequest, "invalid public key")
		return
	}
	// Hidden, unknown and unpositioned nodes: one identical 404 (#743).
	end, ok := s.resolveTerrainEnd(r.Context(), terrainRef{pubkey: pk})
	if !ok {
		writeError(w, http.StatusNotFound, "Not found")
		return
	}
	if ok, wait := c.allow(c.now(), c.limits.SubmissionsPerHour); !ok {
		setRetryAfter(w, wait)
		writeError(w, http.StatusTooManyRequests, "too many claims right now, please try again later")
		return
	}
	code, err := antennaclaim.NewCode()
	if err != nil {
		c.refund()
		writeError(w, http.StatusInternalServerError, "could not issue a code")
		return
	}
	now := c.now()
	cmd := antennaclaim.Command{
		RequestID: channelregistry.NewID(),
		Op:        antennaclaim.OpClaim,
		PubKey:    pk,
		HeightM:   body.HeightM,
		Clear:     body.Clear,
		Code:      code,
		CreatedAt: now.UnixMilli(),
	}
	if body.HeightM != nil {
		cmd.NeedsReview = antennaclaim.NeedsReview(*body.HeightM, s.cachedSurfaceOnly(end))
	}
	if !c.enqueue(w, cmd) {
		c.refund()
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(AntennaClaimIssuedResponse{
		RequestID:       cmd.RequestID,
		Code:            code,
		ExpiresAt:       now.Add(c.limits.RequestTTL).UnixMilli(),
		NameBudgetBytes: antennaclaim.NameBudgetWithLocation,
		SuggestedName:   antennaclaim.SuggestedName(end.name, code, antennaclaim.NameBudgetWithLocation),
	})
}

// cachedSurfaceOnly is the M1 surface under node e from the estimate's
// cache, never upstream; nil when unknown or the estimate is off.
func (s *Server) cachedSurfaceOnly(e terrainEnd) *float64 {
	p := s.terrain
	if p == nil || p.estimate.src == nil || !terrainEstimateRoles[e.role] {
		return nil
	}
	v, _ := p.cachedSurface(e)
	return v
}

// enqueue writes cmd, answering 503 when the queue is full and 500 on other
// errors; it writes nothing on success.
func (c *antennaClaimService) enqueue(w http.ResponseWriter, cmd antennaclaim.Command) bool {
	if c.queue == nil {
		writeError(w, http.StatusServiceUnavailable, "antenna height claims are unavailable")
		return false
	}
	if err := cmd.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return false
	}
	if err := c.queue.EnqueueValue(cmd.RequestID, cmd, c.limits.MaxQueuedRequests); err != nil {
		if errors.Is(err, channelregistry.ErrQueueFull) {
			setRetryAfter(w, time.Minute)
			writeError(w, http.StatusServiceUnavailable, "the claim queue is full, please try again later")
			return false
		}
		log.Printf("[antenna-claims] enqueue %s failed: %v", cmd.Op, err)
		writeError(w, http.StatusInternalServerError, "could not queue the request")
		return false
	}
	return true
}

// claimRow is one stored claim, as the status and admin routes read it.
type claimRow struct {
	AdminAntennaClaim
	nodeLat, nodeLon sql.NullFloat64
}

const claimSelect = `SELECT c.request_id, c.pubkey, c.height_m, c.status, c.needs_review, c.created_at, c.expires_at,
	c.proven_at, COALESCE(c.proof_hash, ''), c.proof_lat, c.proof_lon, c.reviewed_at, COALESCE(c.approved_by, ''),
	COALESCE(n.name, ''), COALESCE(n.role, ''), n.lat, n.lon
	FROM node_antenna_claims c LEFT JOIN nodes n ON n.public_key = c.pubkey`

func scanClaim(sc interface{ Scan(...interface{}) error }) (claimRow, error) {
	var r claimRow
	var h, plat, plon sql.NullFloat64
	var proven, reviewed sql.NullInt64
	err := sc.Scan(&r.ID, &r.PubKey, &h, &r.Status, &r.NeedsReview, &r.CreatedAt, &r.ExpiresAt,
		&proven, &r.ProofHash, &plat, &plon, &reviewed, &r.ApprovedBy, &r.Name, &r.Role, &r.nodeLat, &r.nodeLon)
	if err != nil {
		return r, err
	}
	r.HeightM = claimFloat(h)
	r.ProofLat, r.ProofLon = claimFloat(plat), claimFloat(plon)
	r.ProvenAt, r.ReviewedAt = claimInt(proven), claimInt(reviewed)
	if r.ProofLat != nil && r.ProofLon != nil && r.nodeLat.Valid && r.nodeLon.Valid {
		d := dhm.GroundDistance(dhm.LatLon{Lat: *r.ProofLat, Lon: *r.ProofLon}, dhm.LatLon{Lat: r.nodeLat.Float64, Lon: r.nodeLon.Float64})
		d = math.Round(d*10) / 10
		r.MovedM, r.Stale = &d, d > antennaclaim.StaleMoveM
	}
	return r, nil
}

func claimFloat(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}

func claimInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	i := v.Int64
	return &i
}

// visibleClaim loads claim id unless it is unknown or its node is hidden; ok
// is false for both, and err for a failed read (callers fail closed).
func (s *Server) visibleClaim(ctx context.Context, id string) (r claimRow, ok bool, err error) {
	c := s.antennaClaims()
	if c.db == nil {
		return r, false, nil
	}
	r, err = scanClaim(c.db.QueryRowContext(ctx, claimSelect+` WHERE c.request_id = ?`, id))
	if err == sql.ErrNoRows || isMissingClaimsTable(err) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	hidden, err := s.isIdentityHidden(ctx, r.PubKey)
	if err != nil {
		return r, false, err
	}
	return r, !hidden, nil
}

// GET /api/antenna-claims/requests/{requestId}: the unguessable request id is
// the capability. The stored row is the truth once it exists; before that,
// the queue says whether the command is waiting or was refused.
func (s *Server) handleAntennaClaimRequest(w http.ResponseWriter, r *http.Request) {
	c := s.antennaClaims()
	noStore(w)
	id := mux.Vars(r)["requestId"]
	if !channelregistry.ValidID(id) {
		writeError(w, http.StatusBadRequest, "invalid request id")
		return
	}
	row, ok, err := s.visibleClaim(r.Context(), id)
	if err != nil {
		log.Printf("[antenna-claims] status %s failed: %v", id, err)
		writeError(w, http.StatusInternalServerError, "could not read the request status")
		return
	}
	if ok {
		writeJSON(w, AntennaClaimStatusResponse{Status: row.Status, ExpiresAt: row.ExpiresAt})
		return
	}
	if row.ID != "" || c.queue == nil {
		writeError(w, http.StatusNotFound, "Not found") // hidden: like unknown
		return
	}
	var res antennaclaim.Result
	queued, err := c.queue.LookupValue(id, &res)
	switch {
	case errors.Is(err, channelregistry.ErrUnknownRequest):
		writeError(w, http.StatusNotFound, "Not found")
	case err != nil:
		log.Printf("[antenna-claims] lookup %s failed: %v", id, err)
		writeError(w, http.StatusInternalServerError, "could not read the request status")
	case queued:
		writeJSON(w, AntennaClaimStatusResponse{Status: antennaclaim.RequestQueued})
	case res.Status == antennaclaim.RequestError:
		writeJSON(w, AntennaClaimStatusResponse{Status: res.Status, Error: res.Error})
	default:
		// A result without a visible row: pruned, or the row's node is
		// hidden — never say which.
		writeError(w, http.StatusNotFound, "Not found")
	}
}

// ── Admin routes ──────────────────────────────────────────────────────────

// GET /api/admin/antenna-claims?status=…
func (s *Server) handleAdminAntennaClaims(w http.ResponseWriter, r *http.Request) {
	c := s.antennaClaims()
	noStore(w)
	status := r.URL.Query().Get("status")
	if status != "" && !antennaclaim.ValidStatus(status) {
		writeError(w, http.StatusBadRequest, "status must be one of "+strings.Join(antennaclaim.Statuses, ", "))
		return
	}
	resp := AdminAntennaClaimListResponse{Claims: []AdminAntennaClaim{}, Enabled: c.enabled, AutoApprove: c.autoApprove}
	if c.db != nil {
		q, args := claimSelect, []interface{}{}
		if status != "" {
			q += ` WHERE c.status = ?`
			args = append(args, status)
		}
		q += ` ORDER BY c.created_at DESC, c.request_id LIMIT ?`
		args = append(args, maxAdminClaimsListed)
		rows, err := c.db.QueryContext(r.Context(), q, args...)
		if err != nil && !isMissingClaimsTable(err) {
			log.Printf("[antenna-claims] admin list failed: %v", err)
			writeError(w, http.StatusInternalServerError, "could not list claims")
			return
		}
		if err == nil {
			defer rows.Close()
			var list []AdminAntennaClaim
			var keys []string
			for rows.Next() {
				row, err := scanClaim(rows)
				if err != nil {
					log.Printf("[antenna-claims] admin list failed: %v", err)
					writeError(w, http.StatusInternalServerError, "could not list claims")
					return
				}
				list = append(list, row.AdminAntennaClaim)
				keys = append(keys, row.PubKey)
			}
			// A hidden node's claims are not listed (#743): one batched
			// name lookup for the page, failing closed.
			names, err := s.hiddenIdentityNames(r.Context(), keys)
			if err != nil {
				log.Printf("[antenna-claims] admin list visibility lookup failed: %v", err)
				writeError(w, http.StatusInternalServerError, "could not list claims")
				return
			}
			for _, a := range list {
				if !identityHidden(s.cfg, a.PubKey, append(names[strings.ToLower(a.PubKey)], a.Name)...) {
					resp.Claims = append(resp.Claims, a)
				}
			}
		}
	}
	writeJSON(w, resp)
}

// claimDecisionSources are the statuses each admin decision applies to. The
// check here answers 409 at once; the ingestor checks again in its own
// transaction and is the source of truth.
var claimDecisionSources = map[string][]string{
	antennaclaim.OpApprove: {antennaclaim.StatusProven},
	antennaclaim.OpReject:  {antennaclaim.StatusPending, antennaclaim.StatusProven},
	antennaclaim.OpRevoke:  {antennaclaim.StatusApproved},
}

// POST /api/admin/antenna-claims/{id}/approve|reject|revoke
func (s *Server) handleAdminAntennaClaimDecision(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := s.antennaClaims()
		noStore(w)
		id := mux.Vars(r)["id"]
		if !channelregistry.ValidID(id) {
			writeError(w, http.StatusBadRequest, "invalid claim id")
			return
		}
		row, ok, err := s.visibleClaim(r.Context(), id)
		if err != nil {
			log.Printf("[antenna-claims] get %s failed: %v", id, err)
			writeError(w, http.StatusInternalServerError, "could not read the claim")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "claim not found")
			return
		}
		allowed := false
		for _, st := range claimDecisionSources[op] {
			allowed = allowed || row.Status == st
		}
		if !allowed {
			writeError(w, http.StatusConflict, "claim is "+row.Status)
			return
		}
		cmd := antennaclaim.Command{RequestID: channelregistry.NewID(), Op: op, ClaimID: id, CreatedAt: c.now().UnixMilli()}
		if !c.enqueue(w, cmd) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(AntennaClaimAcceptedResponse{RequestID: cmd.RequestID})
	}
}
