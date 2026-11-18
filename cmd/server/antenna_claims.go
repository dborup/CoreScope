package main

// Owner antenna-height claims (#789 M2a) and station details (#898) — the
// read-only server side.
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
// and serving a height or station details all refuse a hidden node exactly
// like an unknown one.
//
// Contacts (#898): contactPublic is served by /station once its claim is
// approved; contactAdmin is read only by the admin list (adminClaimSelect),
// and only for proven or approved claims. No public response type has a
// field for it.
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
	// maxOwnerHeights caps the snapshots: one approved part per node.
	maxOwnerHeights = 65536
	// maxAdminClaimsListed caps the admin list.
	maxAdminClaimsListed = 1000
	// maxClaimBodyBytes caps the POST body.
	maxClaimBodyBytes = 1024
)

// AntennaClaimRequest is the body of POST /api/nodes/{pubkey}/antenna-claims.
// Height part: at most one of heightM (0–100 m, 0.1 m steps) or clear:true
// (clear keeps its meaning, clear the height). Details part (#898): at most
// one of details or clearDetails:true. At least one part.
type AntennaClaimRequest struct {
	HeightM      *float64                   `json:"heightM,omitempty"`
	Clear        bool                       `json:"clear,omitempty"`
	Details      *antennaclaim.DetailsInput `json:"details,omitempty"`
	ClearDetails bool                       `json:"clearDetails,omitempty"`
}

// AntennaClaimConfigResponse is GET /api/antenna-claims/config: whether
// owners can claim a height right now (the submit route's own gate), whether
// they can set station details (#898), and the operator's settings for the
// owner page and the admin queue (#789 M3).
type AntennaClaimConfigResponse struct {
	Enabled        bool `json:"enabled"`
	DetailsEnabled bool `json:"detailsEnabled"`
	antennaclaim.ClientSettings
}

// NodeStationResponse is GET /api/nodes/{pubkey}/station (#898), PUBLIC.
// Details is null when the node has none, they were cleared, the node moved
// more than staleMoveM from where they were proven (Stale), or station
// details are off. It never carries contactAdmin: StationDetails has no
// field for it.
type NodeStationResponse struct {
	Details    *antennaclaim.StationDetails `json:"details"`
	ApprovedAt *int64                       `json:"approvedAt,omitempty"`
	Stale      bool                         `json:"stale,omitempty"`
}

// NodeAntennaResponse is GET /api/nodes/{pubkey}/antenna (#789 M3): the
// antenna height a terrain profile uses for the node and where it comes
// from (profilecard.TerrainEndpoint's antennaM, antennaSource and
// assumed). EstimatePending: no surface estimate is cached yet, so a
// profile may still raise a default to one.
type NodeAntennaResponse struct {
	AntennaM        float64 `json:"antennaM"`
	AntennaSource   string  `json:"antennaSource"`
	Assumed         bool    `json:"assumed"`
	EstimatePending bool    `json:"estimatePending,omitempty"`
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
	HeightM     *float64 `json:"heightM"` // set only when heightOp = set
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
	// position; Stale when it is more than antennaClaims.staleMoveM (the
	// height is then not served).
	MovedM *float64 `json:"movedM,omitempty"`
	Stale  bool     `json:"stale,omitempty"`
	// The parts (#898): heightOp and detailsOp are set or clear, absent
	// when the claim does not carry that part. The in-force flags say which
	// parts of an approved claim are still served; a part a newer approved
	// claim replaced is no longer in force.
	HeightOp       string                       `json:"heightOp,omitempty"`
	DetailsOp      string                       `json:"detailsOp,omitempty"`
	Details        *antennaclaim.StationDetails `json:"details,omitempty"`
	HeightInForce  bool                         `json:"heightInForce,omitempty"`
	DetailsInForce bool                         `json:"detailsInForce,omitempty"`
	// ContactAdmin is admin only: listed for proven and approved claims
	// (an unproven contact is anyone's text, Q5), never in a public route.
	ContactAdmin string `json:"contactAdmin,omitempty"`
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

// ownerStation is one approved details part and where it was proven.
type ownerStation struct {
	details    antennaclaim.StationDetails
	lat, lon   float64
	approvedAt int64
}

type antennaClaimService struct {
	enabled        bool
	detailsEnabled bool
	autoApprove    bool
	limits         antennaclaim.Limits
	client         antennaclaim.ClientSettings
	queue          *channelregistry.Queue // nil without a database path
	db             *sql.DB                // read-only; nil without a database
	now            func() time.Time
	submissionWindow

	owners   claimSnapshot[ownerHeight]
	stations claimSnapshot[ownerStation]
}

// claimSnapshot is a map by public key read from the claims table and
// refreshed every ownerHeightsTTL. The read runs outside mu, one at a time;
// while it runs the previous snapshot is served. A failed read keeps the
// last good snapshot. The map must not be modified.
type claimSnapshot[V any] struct {
	mu      sync.Mutex
	m       map[string]V // immutable once stored
	expires time.Time
	load    singleflight.Group
}

func (s *claimSnapshot[V]) get(ctx context.Context, now func() time.Time, what string, read func(context.Context) (map[string]V, error)) map[string]V {
	s.mu.Lock()
	cur, fresh := s.m, s.m != nil && now().Before(s.expires)
	s.mu.Unlock()
	if fresh {
		return cur
	}
	ctx = context.WithoutCancel(ctx)
	v, _, _ := s.load.Do(what, func() (interface{}, error) {
		next, err := read(ctx)
		s.mu.Lock()
		defer s.mu.Unlock()
		if err != nil {
			log.Printf("[antenna-claims] reading %s failed: %v", what, err)
			if s.m == nil {
				s.m = map[string]V{}
			}
		} else {
			s.m = next
		}
		s.expires = now().Add(ownerHeightsTTL)
		return s.m, nil
	})
	return v.(map[string]V)
}

func (s *claimSnapshot[V]) invalidate() {
	s.mu.Lock()
	s.m = nil
	s.mu.Unlock()
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
		client:      cc.Client(),
		autoApprove: cc.AutoApprove(),
		db:          db,
		now:         time.Now,
	}
	if queueDir != "" {
		svc.queue = channelregistry.NewQueue(queueDir)
	}
	svc.enabled = cc.SubmissionsRequested() && strongKey && svc.queue != nil && db != nil
	svc.detailsEnabled = svc.enabled && cc.DetailsRequested()
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

// ownerHeights returns the approved heights by public key (claimSnapshot).
func (c *antennaClaimService) ownerHeights(ctx context.Context) map[string]ownerHeight {
	if c == nil || c.db == nil {
		return nil
	}
	return c.owners.get(ctx, c.now, "owner heights", c.readOwnerHeights)
}

// ownerStations returns the approved station details by public key
// (claimSnapshot, #898).
func (c *antennaClaimService) ownerStations(ctx context.Context) map[string]ownerStation {
	if c == nil || c.db == nil {
		return nil
	}
	return c.stations.get(ctx, c.now, "station details", c.readOwnerStations)
}

// invalidateOwners drops the snapshots (tests).
func (c *antennaClaimService) invalidateOwners() {
	c.owners.invalidate()
	c.stations.invalidate()
}

// readOwnerHeights is one indexed SELECT of the heights in force with a
// proof position; a missing table (an ingestor older than #789 M2a) is none.
// A height a newer claim replaced is no longer in force (#898), so the
// in-force flag, not the newest approval, decides.
func (c *antennaClaimService) readOwnerHeights(ctx context.Context) (map[string]ownerHeight, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT pubkey, height_m, proof_lat, proof_lon FROM node_antenna_claims
		WHERE status = 'approved' AND height_op = 'set' AND height_in_force = 1
		  AND proof_lat IS NOT NULL AND proof_lon IS NOT NULL
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

// readOwnerStations is one SELECT of the station details in force with a
// proof position. It names its columns and never the admin contact table:
// contactAdmin cannot reach the public snapshot.
func (c *antennaClaimService) readOwnerStations(ctx context.Context) (map[string]ownerStation, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT pubkey, antenna_type, gain_dbi, power_mask, installation, contact_public,
		proof_lat, proof_lon, reviewed_at FROM node_antenna_claims
		WHERE status = 'approved' AND details_op = 'set' AND details_in_force = 1
		  AND proof_lat IS NOT NULL AND proof_lon IS NOT NULL
		ORDER BY reviewed_at LIMIT ?`, maxOwnerHeights)
	if isMissingClaimsTable(err) {
		return map[string]ownerStation{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ownerStation{}
	for rows.Next() {
		var pk string
		var st ownerStation
		var typ, inst, contact sql.NullString
		var gain sql.NullFloat64
		var mask, reviewed sql.NullInt64
		if err := rows.Scan(&pk, &typ, &gain, &mask, &inst, &contact, &st.lat, &st.lon, &reviewed); err != nil {
			return nil, err
		}
		st.details = antennaclaim.StationDetails{
			AntennaType: typ.String, GainDBi: claimFloat(gain), Power: antennaclaim.PowerList(int(mask.Int64)),
			Installation: inst.String, ContactPublic: contact.String,
		}
		st.approvedAt = reviewed.Int64
		out[strings.ToLower(pk)] = st // the latest approval wins
	}
	return out, rows.Err()
}

// ownerHeightAt is the approved owner height of node pubkey standing at pt,
// or nil: none, or the node has moved more than staleMoveM from where the
// height was proven (stale: fall back, no write). The caller has already
// refused hidden nodes.
func (c *antennaClaimService) ownerHeightAt(ctx context.Context, pubkey string, pt dhm.LatLon) *float64 {
	h, ok := c.ownerHeights(ctx)[strings.ToLower(pubkey)]
	if !ok || c.limits.Stale(dhm.GroundDistance(pt, dhm.LatLon{Lat: h.lat, Lon: h.lon})) {
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

// GET /api/antenna-claims/config
func (s *Server) handleAntennaClaimConfig(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	c := s.antennaClaims()
	writeJSON(w, AntennaClaimConfigResponse{Enabled: c.enabled, DetailsEnabled: c.detailsEnabled, ClientSettings: c.client})
}

// GET /api/nodes/{pubkey}/station (#898): the node's approved station
// details, from the snapshot alone. Hidden, unknown and unpositioned nodes
// answer one identical 404 (#743). It does not need the terrain profile.
// Details the node moved away from (more than staleMoveM from where they were
// proven) are withheld and Stale is set; nothing is written.
func (s *Server) handleNodeStation(w http.ResponseWriter, r *http.Request) {
	pk := strings.ToLower(mux.Vars(r)["pubkey"])
	if !antennaclaim.ValidPubKey(pk) {
		writeError(w, http.StatusBadRequest, "invalid public key")
		return
	}
	end, ok := s.resolveTerrainEnd(r.Context(), terrainRef{pubkey: pk})
	if !ok {
		writeError(w, http.StatusNotFound, "Not found")
		return
	}
	c := s.antennaClaims()
	resp := NodeStationResponse{}
	if c.detailsEnabled {
		if st, ok := c.ownerStations(r.Context())[pk]; ok {
			if c.limits.Stale(dhm.GroundDistance(end.pt, dhm.LatLon{Lat: st.lat, Lon: st.lon})) {
				resp.Stale = true
			} else {
				d, at := st.details, st.approvedAt
				resp.Details, resp.ApprovedAt = &d, &at
			}
		}
	}
	writeJSON(w, resp)
}

// GET /api/nodes/{pubkey}/antenna: the height a profile uses for the node,
// by the profile's own precedence (terrainProfiler.antenna), from the
// caches alone: it never measures the surface. Hidden, unknown and
// unpositioned nodes answer one identical 404 (#743).
func (s *Server) handleNodeAntenna(w http.ResponseWriter, r *http.Request) {
	p := s.terrain
	if p == nil {
		writeError(w, http.StatusServiceUnavailable, "antenna heights are unavailable: the terrain profile is off")
		return
	}
	pk := strings.ToLower(mux.Vars(r)["pubkey"])
	if !antennaclaim.ValidPubKey(pk) {
		writeError(w, http.StatusBadRequest, "invalid public key")
		return
	}
	end, ok := s.resolveTerrainEnd(r.Context(), terrainRef{pubkey: pk})
	if !ok {
		writeError(w, http.StatusNotFound, "Not found")
		return
	}
	ends := [2]terrainEnd{end}
	s.attachOwnerHeights(r.Context(), &ends)
	pending := p.cachedEstimates(&ends, [2]*float64{})
	h, src, assumed := p.antenna(ends[0], nil)
	writeJSON(w, NodeAntennaResponse{AntennaM: h, AntennaSource: src, Assumed: assumed, EstimatePending: pending})
}

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
	if body.HeightM != nil && body.Clear {
		writeError(w, http.StatusBadRequest, "give either heightM or clear:true, not both")
		return
	}
	if body.Details != nil && body.ClearDetails {
		writeError(w, http.StatusBadRequest, "give either details or clearDetails:true, not both")
		return
	}
	if body.HeightM == nil && !body.Clear && body.Details == nil && !body.ClearDetails {
		writeError(w, http.StatusBadRequest, "give heightM, clear:true, details or clearDetails:true")
		return
	}
	if body.HeightM != nil && !antennaclaim.ValidHeight(*body.HeightM) {
		writeError(w, http.StatusBadRequest, "heightM must be 0-100 m in steps of 0.1 m")
		return
	}
	// Setting details needs detailsEnabled; clearing them stays open while
	// claims are enabled, so an owner can always remove their data (N4).
	if body.Details != nil && !c.detailsEnabled {
		writeError(w, http.StatusForbidden, "station details are disabled")
		return
	}
	if body.Details != nil {
		if err := body.Details.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
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
		RequestID:    channelregistry.NewID(),
		Op:           antennaclaim.OpClaim,
		PubKey:       pk,
		HeightM:      body.HeightM,
		Clear:        body.Clear,
		Details:      body.Details,
		ClearDetails: body.ClearDetails,
		Code:         code,
		CreatedAt:    now.UnixMilli(),
	}
	if body.HeightM != nil {
		cmd.NeedsReview = c.limits.NeedsReview(*body.HeightM, s.cachedSurfaceOnly(end))
	}
	if body.Details != nil && body.Details.GainDBi != nil {
		cmd.NeedsReview = cmd.NeedsReview || antennaclaim.GainNeedsReview(*body.Details.GainDBi)
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

// claimColumns and claimFrom are the claim columns every claim read shares.
// They hold no admin contact: claimSelect serves the request status and the
// admin decisions; only adminClaimSelect adds the contact.
const (
	claimColumns = `c.request_id, c.pubkey, c.height_m, c.status, c.needs_review, c.created_at, c.expires_at,
	c.proven_at, COALESCE(c.proof_hash, ''), c.proof_lat, c.proof_lon, c.reviewed_at, COALESCE(c.approved_by, ''),
	COALESCE(n.name, ''), COALESCE(n.role, ''), n.lat, n.lon,
	COALESCE(c.height_op, ''), COALESCE(c.details_op, ''), c.height_in_force, c.details_in_force,
	c.antenna_type, c.gain_dbi, c.power_mask, c.installation, c.contact_public`
	claimFrom   = ` FROM node_antenna_claims c LEFT JOIN nodes n ON n.public_key = c.pubkey`
	claimSelect = `SELECT ` + claimColumns + claimFrom
	// adminClaimSelect is the admin list's read, the only one that joins
	// node_claim_admin_contacts. A pending claim's contact is anyone's
	// unproven text (Q5): SQL drops it, so Go never holds it.
	adminClaimSelect = `SELECT ` + claimColumns + `,
	CASE WHEN c.status IN ('proven','approved') THEN ac.contact END` + claimFrom + `
	LEFT JOIN node_claim_admin_contacts ac ON ac.request_id = c.request_id`
)

// scanClaim reads one claim from claimSelect, or from adminClaimSelect with
// withAdminContact; l decides whether a moved node is stale.
func scanClaim(sc interface{ Scan(...interface{}) error }, l antennaclaim.Limits, withAdminContact bool) (claimRow, error) {
	var r claimRow
	var h, plat, plon, gain sql.NullFloat64
	var proven, reviewed, mask sql.NullInt64
	var typ, inst, contactPublic, contactAdmin sql.NullString
	dest := []interface{}{&r.ID, &r.PubKey, &h, &r.Status, &r.NeedsReview, &r.CreatedAt, &r.ExpiresAt,
		&proven, &r.ProofHash, &plat, &plon, &reviewed, &r.ApprovedBy, &r.Name, &r.Role, &r.nodeLat, &r.nodeLon,
		&r.HeightOp, &r.DetailsOp, &r.HeightInForce, &r.DetailsInForce, &typ, &gain, &mask, &inst, &contactPublic}
	if withAdminContact {
		dest = append(dest, &contactAdmin)
	}
	if err := sc.Scan(dest...); err != nil {
		return r, err
	}
	r.ContactAdmin = contactAdmin.String
	if r.DetailsOp == antennaclaim.PartSet {
		r.Details = &antennaclaim.StationDetails{AntennaType: typ.String, GainDBi: claimFloat(gain),
			Power: antennaclaim.PowerList(int(mask.Int64)), Installation: inst.String, ContactPublic: contactPublic.String}
	}
	r.HeightM = claimFloat(h)
	r.ProofLat, r.ProofLon = claimFloat(plat), claimFloat(plon)
	r.ProvenAt, r.ReviewedAt = claimInt(proven), claimInt(reviewed)
	if r.ProofLat != nil && r.ProofLon != nil && r.nodeLat.Valid && r.nodeLon.Valid {
		d := dhm.GroundDistance(dhm.LatLon{Lat: *r.ProofLat, Lon: *r.ProofLon}, dhm.LatLon{Lat: r.nodeLat.Float64, Lon: r.nodeLon.Float64})
		d = math.Round(d*10) / 10
		r.MovedM, r.Stale = &d, l.Stale(d)
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
	r, err = scanClaim(c.db.QueryRowContext(ctx, claimSelect+` WHERE c.request_id = ?`, id), c.limits, false)
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
		q, args := adminClaimSelect, []interface{}{}
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
				row, err := scanClaim(rows, c.limits, true)
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
// check here (and, for an approval, the newer-approved check) answers 409 at
// once; the ingestor checks again in its own transaction and is the source
// of truth.
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
		if op == antennaclaim.OpApprove && row.ProvenAt != nil {
			var newer bool
			if err := c.db.QueryRowContext(r.Context(), antennaclaim.NewerApprovedQuery, row.PubKey, id, *row.ProvenAt,
				row.HeightOp != "", row.DetailsOp != "").Scan(&newer); err != nil {
				log.Printf("[antenna-claims] newer-claim check %s failed: %v", id, err)
				writeError(w, http.StatusInternalServerError, "could not read the claim")
				return
			}
			if newer {
				writeError(w, http.StatusConflict, antennaclaim.ErrNewerApproved.Error())
				return
			}
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
