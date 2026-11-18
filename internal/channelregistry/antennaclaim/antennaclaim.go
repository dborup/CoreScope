// Package antennaclaim holds the rules shared by the server and the ingestor
// for owner-submitted antenna heights (#789 M2a).
//
// An owner proves control of a node by putting a server-issued code into the
// node's advert name: the advert's ed25519 signature covers the name
// (firmware Mesh::createAdvert signs pubkey ‖ timestamp ‖ appdata), so only
// the key holder, or someone with the node's admin CLI, can send it. The code
// is random, single-use, bound to one public key and expires, so no advert
// signed before it was issued can carry it: replay protection comes from the
// code, not from timestamps.
//
// The server issues codes and writes command files into a channelregistry
// queue (QueueDirName); the ingestor owns the node_antenna_claims table,
// matches codes at advert ingest and applies admin decisions. The server only
// reads (#1283).
package antennaclaim

import (
	"crypto/rand"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// QueueDirName is the command queue directory next to the database.
const QueueDirName = "antenna-claim-requests"

// QueueDir returns the queue directory for a database path.
func QueueDir(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), QueueDirName)
}

// Command operations.
const (
	OpClaim   = "claim"   // a new claim: pending until an advert proves it
	OpApprove = "approve" // admin: proven → approved
	OpReject  = "reject"  // admin: pending or proven → rejected
	OpRevoke  = "revoke"  // admin: approved → revoked
)

// Claim statuses, as stored in node_antenna_claims.status.
const (
	StatusPending    = "pending"    // issued, waiting for an advert with the code
	StatusProven     = "proven"     // an advert carried the code; waiting for review
	StatusApproved   = "approved"   // in force: served as the node's owner height
	StatusRejected   = "rejected"   // an administrator refused it
	StatusRevoked    = "revoked"    // an administrator withdrew an approval
	StatusExpired    = "expired"    // not proven within RequestTTL
	StatusSuperseded = "superseded" // replaced by a newer approved claim for the key
)

// Statuses lists every stored status.
var Statuses = []string{StatusPending, StatusProven, StatusApproved, StatusRejected, StatusRevoked, StatusExpired, StatusSuperseded}

// ValidStatus reports whether s is a stored status.
func ValidStatus(s string) bool { return oneOf(s, Statuses) }

// Who put a claim in force, as stored in node_antenna_claims.approved_by.
const (
	ApprovedByAuto  = "auto"  // autoApproveProven approved a proven, unflagged height
	ApprovedByAdmin = "admin" // an administrator approved it
	ApprovedByOwner = "owner" // a proven clear-only claim: the owner withdrew their own data
)

// Part operations, as stored in node_antenna_claims.height_op and
// details_op (#898). A claim carries a height part, a details part or both;
// a part it does not carry is NULL. Clear is always explicit: no code path
// reads a NULL value column as a clear.
const (
	PartSet   = "set"
	PartClear = "clear"
)

// RequestQueued is the status of a command the ingestor has not applied yet;
// RequestError of one it refused (Result.Error says why).
const (
	RequestQueued = "queued"
	RequestError  = "error"
)

// Bounds.
const (
	// MaxHeightM is the antenna height cap: the profile's one rule
	// (terrainMaxAntennaM, public/antenna-height.js), owner decision 2.
	MaxHeightM = 100
	// CodeLen is the code length: 6 Crockford base32 characters, 30 bits.
	CodeLen = 6
	// MaxAdvertDataSize is the firmware's appdata cap (MeshCore.h).
	MaxAdvertDataSize = 32
	// NameBudgetWithLocation is the name's bytes on air in an advert with a
	// position, which every claimable node has.
	NameBudgetWithLocation = MaxAdvertDataSize - 1 - 8
	// ReviewBelowM flags a height this far below the surface.
	ReviewBelowM = 2
	// RoofM is the M1 estimate's antenna above the surface.
	RoofM = 2
)

// CodeAlphabet is Crockford base32 without I, L, O and U: one-byte UTF-8 that
// the firmware's CLI name check (AdvertDataParser::isValidName) and
// CoreScope's sanitizeName both keep. 32 symbols, so a random byte's low five
// bits pick one without bias.
const CodeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewCode returns a fresh code from crypto/rand.
func NewCode() (string, error) {
	var b [CodeLen]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	out := make([]byte, CodeLen)
	for i, v := range b {
		out[i] = CodeAlphabet[v&31]
	}
	return string(out), nil
}

// ValidCode reports whether s is a well-formed code.
func ValidCode(s string) bool {
	if len(s) != CodeLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(CodeAlphabet, s[i]) < 0 {
			return false
		}
	}
	return true
}

// ValidHeight accepts 0–MaxHeightM in steps of 0.1 m, like
// public/antenna-height.js.
func ValidHeight(h float64) bool {
	if math.IsNaN(h) || h < 0 || h > MaxHeightM {
		return false
	}
	t := h * 10
	return math.Abs(t-math.Round(t)) < 1e-6
}

// ValidPubKey accepts a 32-byte public key as 64 lowercase hex characters.
func ValidPubKey(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// NameBudget is the bytes left for the name in an advert with these optional
// fields (firmware AdvertDataBuilder::encodeTo: MAX_ADVERT_DATA_SIZE minus the
// flags byte, 8 for a position, 2 per feature word). The firmware cuts a
// longer stored name, so a code past this budget never goes on air.
func NameBudget(hasLocation, hasFeat1, hasFeat2 bool) int {
	n := MaxAdvertDataSize - 1
	if hasLocation {
		n -= 8
	}
	if hasFeat1 {
		n -= 2
	}
	if hasFeat2 {
		n -= 2
	}
	return n
}

// ContainsCode reports whether code appears in name as a whole
// space-separated token that ends within the first budget bytes: the part of
// the name any receiver gets. An empty code never matches.
func ContainsCode(name string, budget int, code string) bool {
	if code == "" {
		return false
	}
	if budget < len(name) {
		name = name[:max(budget, 0)]
	}
	for _, tok := range strings.Split(name, " ") {
		if tok == code {
			return true
		}
	}
	return false
}

// SuggestedName is current, cut to a valid UTF-8 prefix, then a space and the
// code, within budget bytes. The code is appended, never prepended, so a name
// prefix keeps its meaning.
func SuggestedName(current, code string, budget int) string {
	room := budget - len(code) - 1
	if room <= 0 {
		return code
	}
	if len(current) > room {
		current = current[:room]
		for len(current) > 0 && !utf8.ValidString(current) {
			current = current[:len(current)-1]
		}
	}
	current = strings.TrimRight(current, " ")
	if current == "" {
		return code
	}
	return current + " " + code
}

// NeedsReview flags a height an administrator should look at: more than
// ReviewDeltaM above surface + RoofM, or more than ReviewBelowM below the
// surface. surfaceM is the cached M1 surface; nil (unknown) flags nothing.
func (l Limits) NeedsReview(heightM float64, surfaceM *float64) bool {
	if surfaceM == nil {
		return false
	}
	s := *surfaceM
	return heightM > s+RoofM+l.ReviewDeltaM+1e-9 || heightM < s-ReviewBelowM-1e-9
}

// Stale reports whether a node that moved movedM metres from where its
// height was proven has moved too far for the height to be used
// (read-side rule: the height is ignored, nothing is written).
func (l Limits) Stale(movedM float64) bool { return movedM > l.StaleMoveM }

// Command is one queued request. For OpClaim, PubKey and Code are set and
// the claim carries at least one part: the height part (at most one of
// HeightM or Clear) and the details part (at most one of Details or
// ClearDetails, #898); the claim's id is RequestID. For the admin ops,
// ClaimID names the claim. Clear keeps its wire name and meaning (clear the
// height), so command files queued by an older server still apply.
type Command struct {
	RequestID    string        `json:"requestId"`
	Op           string        `json:"op"`
	PubKey       string        `json:"pubkey,omitempty"`
	HeightM      *float64      `json:"heightM,omitempty"`
	Clear        bool          `json:"clear,omitempty"`
	Details      *DetailsInput `json:"details,omitempty"`
	ClearDetails bool          `json:"clearDetails,omitempty"`
	Code         string        `json:"code,omitempty"`
	// NeedsReview is the server's flag from the cached surface estimate;
	// a flagged claim is never auto-approved.
	NeedsReview bool   `json:"needsReview,omitempty"`
	ClaimID     string `json:"claimId,omitempty"`
	CreatedAt   int64  `json:"createdAt"`
}

// ErrInvalidCommand is a command that breaks the shared rules.
var ErrInvalidCommand = errors.New("invalid antenna claim command")

// ErrNewerApproved refuses an approval whose proof is older than the claim
// already in force for the same key: a late review must never undo a newer
// decision, such as the owner's proven clear (owner decision 4). The server
// answers it with 409; the ingestor checks again and is the source of truth.
var ErrNewerApproved = errors.New("a newer claim for this node is already approved")

// NewerApprovedQuery reports whether pubkey has an approved claim, other than
// request id, proven after provenAt, that carries a part this claim carries
// (#898: a newer details claim does not block an older height, and the
// reverse). Args: pubkey, request id, provenAt (ms), carriesHeight,
// carriesDetails. Server and ingestor share it so their 409 and refusal
// agree.
const NewerApprovedQuery = `SELECT EXISTS(SELECT 1 FROM node_antenna_claims
	WHERE pubkey = ? AND status = 'approved' AND request_id != ? AND proven_at > ?
	  AND ((? AND height_op IS NOT NULL) OR (? AND details_op IS NOT NULL)))`

// ClearOnlySQL is true for a claim whose every carried part is a clear: a
// proven clear-only claim takes effect at once (the owner withdraws their
// own data). It reads the explicit ops only, never a value column: a
// details-only claim has a NULL height_m and must not pass for a clear.
const ClearOnlySQL = `((height_op IS NULL OR height_op = 'clear')
	AND (details_op IS NULL OR details_op = 'clear')
	AND (height_op IS NOT NULL OR details_op IS NOT NULL))`

// ContactLiveSQL is true for a claim whose contacts may exist: pending,
// proven, or approved with its details part still in force. Every other
// claim's contact_public is NULL and its admin contact row is gone.
const ContactLiveSQL = `(status IN ('pending','proven') OR (status = 'approved' AND details_in_force = 1))`

// Parts is what a claim carries, from the explicit fields only: "",
// PartSet or PartClear each.
type Parts struct{ Height, Details string }

// Parts returns what c carries.
func (c Command) Parts() Parts {
	var p Parts
	switch {
	case c.HeightM != nil:
		p.Height = PartSet
	case c.Clear:
		p.Height = PartClear
	}
	switch {
	case c.Details != nil:
		p.Details = PartSet
	case c.ClearDetails:
		p.Details = PartClear
	}
	return p
}

// Validate checks c against the shared rules. The ingestor runs it again on
// every file it reads: it never trusts the queue.
func (c Command) Validate() error {
	if !validID(c.RequestID) {
		return ErrInvalidCommand
	}
	switch c.Op {
	case OpClaim:
		if !ValidPubKey(c.PubKey) || !ValidCode(c.Code) || (c.HeightM != nil && c.Clear) ||
			(c.Details != nil && c.ClearDetails) || (c.Parts() == Parts{}) {
			return ErrInvalidCommand
		}
		if c.HeightM != nil && !ValidHeight(*c.HeightM) {
			return ErrInvalidCommand
		}
		if c.Details != nil && c.Details.Validate() != nil {
			return ErrInvalidCommand
		}
	case OpApprove, OpReject, OpRevoke:
		if !validID(c.ClaimID) {
			return ErrInvalidCommand
		}
	default:
		return ErrInvalidCommand
	}
	return nil
}

// validID mirrors channelregistry.ValidID (16–64 lowercase hex), without
// importing the parent package into this one.
func validID(id string) bool {
	if len(id) < 16 || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Result is what the ingestor writes back for a command. Status is the
// claim's status after the command, or RequestError.
type Result struct {
	RequestID   string `json:"requestId"`
	Status      string `json:"status"`
	ClaimID     string `json:"claimId,omitempty"`
	Error       string `json:"error,omitempty"`
	CompletedAt int64  `json:"completedAt"`
}

// Defaults. Every limit is bounded.
const (
	DefaultMaxPending         = 256
	DefaultMaxPendingPerNode  = 3
	DefaultMaxQueuedRequests  = 256
	DefaultRetentionDays      = 30
	DefaultSubmissionsPerHour = 20
	DefaultRequestTTL         = 24 * time.Hour
	maxResultFactor           = 4

	// DefaultStaleMoveM is how far a node may move from where its height
	// was proven before the height is ignored; MaxStaleMoveM bounds it.
	DefaultStaleMoveM = 50.0
	MaxStaleMoveM     = 5000.0
	// DefaultReviewDeltaM flags a height this far above surface + RoofM;
	// MaxReviewDeltaM bounds it.
	DefaultReviewDeltaM = 60.0
	MaxReviewDeltaM     = 1000.0

	// The owner page's status polling: the first delay, the factor each
	// later delay grows by, the cap on one delay and the number of checks
	// before "Check again" (2 s, 3 s, 4.5 s … 30 s, 60 checks ≈ 25 min).
	DefaultPollInitialMs   = 2000
	MinPollInitialMs       = 500
	MaxPollInitialMs       = 60000
	DefaultPollBackoff     = 1.5
	MaxPollBackoff         = 4.0
	DefaultPollMaxMs       = 30000
	MaxPollMaxMs           = 600000
	DefaultPollMaxAttempts = 60
	MaxPollMaxAttempts     = 1000
	// DefaultAdminFilter is the admin queue's first view: the claims to
	// review.
	DefaultAdminFilter = StatusProven
)

// Config is the "antennaClaims" block in config.json, read by the server and
// the ingestor. Zero or negative limits fall back to the defaults.
type Config struct {
	// Enabled opens public claims. It only takes effect with a strong admin
	// apiKey (owner decision 5); off by default.
	Enabled *bool `json:"enabled,omitempty"`
	// DetailsEnabled also lets owners set station details (#898). It only
	// takes effect with Enabled; off by default. Clearing details stays
	// open while claims are enabled.
	DetailsEnabled bool `json:"detailsEnabled,omitempty"`
	// AutoApproveProven approves a proven, unflagged height without an
	// administrator (owner decision 3); off by default.
	AutoApproveProven  bool `json:"autoApproveProven,omitempty"`
	SubmissionsPerHour int  `json:"submissionsPerHour,omitempty"`
	MaxPending         int  `json:"maxPending,omitempty"`
	MaxPendingPerNode  int  `json:"maxPendingPerNode,omitempty"`
	MaxQueuedRequests  int  `json:"maxQueuedRequests,omitempty"`
	RetentionDays      int  `json:"retentionDays,omitempty"`
	// StaleMoveM and ReviewDeltaM are the server's read-side thresholds
	// (#789 M3); zero, negative or above the bound is the default.
	StaleMoveM   float64 `json:"staleMoveM,omitempty"`
	ReviewDeltaM float64 `json:"reviewDeltaM,omitempty"`
	// The owner page's polling and the admin queue's first filter, served
	// to the browser by GET /api/antenna-claims/config (#789 M3); a value
	// out of its bounds is the default.
	PollInitialMs      int     `json:"pollInitialMs,omitempty"`
	PollBackoff        float64 `json:"pollBackoff,omitempty"`
	PollMaxMs          int     `json:"pollMaxMs,omitempty"`
	PollMaxAttempts    int     `json:"pollMaxAttempts,omitempty"`
	AdminDefaultFilter string  `json:"adminDefaultFilter,omitempty"`
}

// SubmissionsRequested reports whether the operator switched claims on.
// Callers must additionally require a strong admin API key.
func (c *Config) SubmissionsRequested() bool {
	return c != nil && c.Enabled != nil && *c.Enabled
}

// DetailsRequested reports whether the operator switched station details
// on. Callers must additionally require SubmissionsRequested and a strong
// admin API key.
func (c *Config) DetailsRequested() bool { return c != nil && c.DetailsEnabled }

// AutoApprove reports whether proven, unflagged heights are approved without
// an administrator.
func (c *Config) AutoApprove() bool { return c != nil && c.AutoApproveProven }

// ApplyGates sets the switches from on, which reports whether the config
// value at a path of canonical, exact-case keys is the JSON boolean true
// (configkey.Doc.On). encoding/json would also take `Enabled` or
// `AutoApproveProven`; the server and the ingestor both call this, so they
// agree on what is switched on (#884).
func (c *Config) ApplyGates(on func(path ...string) bool) {
	if c == nil {
		return
	}
	enabled := on("antennaClaims", "enabled")
	c.Enabled = &enabled
	c.AutoApproveProven = on("antennaClaims", "autoApproveProven")
	c.DetailsEnabled = on("antennaClaims", "detailsEnabled")
}

// Limits are the resolved limits.
type Limits struct {
	MaxPending         int
	MaxPendingPerNode  int
	MaxQueuedRequests  int
	MaxResults         int
	RetentionDays      int
	SubmissionsPerHour int
	RequestTTL         time.Duration
	StaleMoveM         float64
	ReviewDeltaM       float64
}

// Limits resolves the configured limits. Safe on a nil *Config.
func (c *Config) Limits() Limits {
	var in Config
	if c != nil {
		in = *c
	}
	l := Limits{
		MaxPending:         orDefault(in.MaxPending, DefaultMaxPending),
		MaxPendingPerNode:  orDefault(in.MaxPendingPerNode, DefaultMaxPendingPerNode),
		MaxQueuedRequests:  orDefault(in.MaxQueuedRequests, DefaultMaxQueuedRequests),
		RetentionDays:      orDefault(in.RetentionDays, DefaultRetentionDays),
		SubmissionsPerHour: orDefault(in.SubmissionsPerHour, DefaultSubmissionsPerHour),
		RequestTTL:         DefaultRequestTTL,
		StaleMoveM:         inRange(in.StaleMoveM, 0, MaxStaleMoveM, DefaultStaleMoveM),
		ReviewDeltaM:       inRange(in.ReviewDeltaM, 0, MaxReviewDeltaM, DefaultReviewDeltaM),
	}
	l.MaxResults = l.MaxQueuedRequests * maxResultFactor
	return l
}

// ClientSettings are the browser's settings for the owner page and the
// admin queue, as GET /api/antenna-claims/config serves them.
type ClientSettings struct {
	PollInitialMs      int     `json:"pollInitialMs"`
	PollBackoff        float64 `json:"pollBackoff"`
	PollMaxMs          int     `json:"pollMaxMs"`
	PollMaxAttempts    int     `json:"pollMaxAttempts"`
	AdminDefaultFilter string  `json:"adminDefaultFilter"`
}

// Client resolves the client settings. Safe on a nil *Config.
func (c *Config) Client() ClientSettings {
	var in Config
	if c != nil {
		in = *c
	}
	cs := ClientSettings{
		PollInitialMs:      int(inRange(float64(in.PollInitialMs), MinPollInitialMs-1, MaxPollInitialMs, DefaultPollInitialMs)),
		PollBackoff:        inRange(in.PollBackoff, 1-1e-9, MaxPollBackoff, DefaultPollBackoff),
		PollMaxMs:          int(inRange(float64(in.PollMaxMs), 0, MaxPollMaxMs, DefaultPollMaxMs)),
		PollMaxAttempts:    int(inRange(float64(in.PollMaxAttempts), 0, MaxPollMaxAttempts, DefaultPollMaxAttempts)),
		AdminDefaultFilter: DefaultAdminFilter,
	}
	cs.PollMaxMs = max(cs.PollMaxMs, cs.PollInitialMs)
	if ValidStatus(in.AdminDefaultFilter) {
		cs.AdminDefaultFilter = in.AdminDefaultFilter
	}
	return cs
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// inRange is v when lo < v <= hi, else def.
func inRange(v, lo, hi, def float64) float64 {
	if !(v > lo && v <= hi) {
		return def
	}
	return v
}
