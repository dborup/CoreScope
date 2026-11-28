// Package account holds the rules shared by the server and the ingestor for
// owner accounts (#1018 M1): sign-in with a companion, the session token and
// the synced "My nodes" list.
//
// Sign-in is option A of the #1018 research: the server issues a random
// single-use code bound to one companion public key; the owner puts it in the
// companion's name and sends a flood advert (firmware
// examples/companion_radio/MyMesh.cpp:1259-1265 sets the name,
// :1297-1313 sends the advert, flood when byte 1 is 1). The advert is signed
// over pubkey ‖ timestamp ‖ appdata, so the name is covered
// (src/Mesh.cpp:404-437, the signature at :434). A companion's advert is
// ADV_TYPE_CHAT (src/helpers/BaseChatMesh.cpp:21-41,
// src/helpers/AdvertDataHelpers.h:8): only that type can sign in (owner
// decision 2).
//
// The server issues codes and writes command files into a channelregistry
// queue (QueueDirName); the ingestor owns the account tables, matches codes at
// advert ingest, creates the session key file and applies every account
// write. The server only reads (#1283): it mints and checks a stateless
// HMAC-signed session token and reads one primary-key row per authenticated
// request.
package account

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/meshcore-analyzer/channelregistry/antennaclaim"
)

// QueueDirName is the command queue directory next to the database.
const QueueDirName = "account-requests"

// QueueDir returns the queue directory for a database path.
func QueueDir(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), QueueDirName)
}

// Command operations (M1).
const (
	OpLogin        = "login"          // a sign-in challenge: pending until an advert proves it
	OpMyNodesSet   = "my-nodes-set"   // replace the list (the localStorage migration)
	OpMyNodeAdd    = "my-node-add"    // add one node
	OpMyNodeRemove = "my-node-remove" // remove one node
	OpProfileSet   = "profile-set"    // set or clear the display name
	OpSignOutAll   = "sign-out-all"   // end every session: session_gen + 1
	OpDelete       = "delete"         // delete the account and its list
	OpRedeem       = "redeem"         // mark a proven sign-in as redeemed (ProofID)
)

// IsRevocation reports whether op ends the account's sessions. The server
// never makes these wait on a write budget: an owner can always sign out
// everywhere or delete the account, even when a stolen session spent it.
func IsRevocation(op string) bool { return op == OpSignOutAll || op == OpDelete }

// Login statuses, as stored in account_proofs.status.
const (
	StatusPending = "pending" // issued, waiting for an advert with the code
	StatusProven  = "proven"  // a companion advert carried the code
	StatusExpired = "expired" // not proven within the code lifetime
)

// ProofKindLogin is account_proofs.kind of a sign-in (M2 adds 'link').
const ProofKindLogin = "login"

// Request statuses of a queued command: queued while the ingestor has not
// read it, done once applied, error when refused (Result.Error says why).
const (
	RequestQueued = "queued"
	RequestDone   = "done"
	RequestError  = "error"
)

// Bounds.
const (
	// IDLen is the hex length of an account id: 128 random bits, never
	// derived from the companion key.
	IDLen = 32
	// RedeemSecretLen is the hex length of the redeem secret (128 bits).
	RedeemSecretLen = 32
	// MaxDisplayNameRunes caps the display name.
	MaxDisplayNameRunes = 32
	// MaxNameHintBytes caps a My-nodes entry's name hint.
	MaxNameHintBytes = 64
	// HardMaxMyNodes bounds maxMyNodes and any one command's list.
	HardMaxMyNodes = 1000
	// UnknownBudget is the name budget when the companion's last advert is
	// unknown: the worst case, with a position (#1018 research §2.1).
	UnknownBudget = antennaclaim.NameBudgetWithLocation
)

// NewID returns IDLen random hex characters.
func NewID() (string, error) {
	var b [IDLen / 2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// NewRedeemSecret returns a fresh redeem secret and its hash.
func NewRedeemSecret() (secret, hash string, err error) {
	var b [RedeemSecretLen / 2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	secret = hex.EncodeToString(b[:])
	return secret, HashRedeemSecret(secret), nil
}

// HashRedeemSecret is the stored form of a redeem secret: hex SHA-256.
func HashRedeemSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// ValidAccountID accepts IDLen lowercase hex characters.
func ValidAccountID(s string) bool { return len(s) == IDLen && isLowerHex(s) }

// ValidRedeemSecret accepts RedeemSecretLen lowercase hex characters.
func ValidRedeemSecret(s string) bool { return len(s) == RedeemSecretLen && isLowerHex(s) }

func validHash(s string) bool { return len(s) == 64 && isLowerHex(s) }

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ValidDisplayName accepts 1–MaxDisplayNameRunes printable characters with no
// leading or trailing space.
func ValidDisplayName(s string) bool {
	if s == "" || !utf8.ValidString(s) || strings.TrimSpace(s) != s || utf8.RuneCountInString(s) > MaxDisplayNameRunes {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// ValidNameHint accepts an empty hint or up to MaxNameHintBytes of valid
// UTF-8 without control characters.
func ValidNameHint(s string) bool {
	if len(s) > MaxNameHintBytes || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// MyNode is one entry of the synced list, the same shape the browser's
// meshcore-my-nodes entries have. AddedAt is Unix ms.
type MyNode struct {
	PubKey  string `json:"pubkey"`
	Name    string `json:"name,omitempty"`
	AddedAt int64  `json:"addedAt,omitempty"`
}

// Command is one queued request. OpLogin carries PubKey, Code and
// RedeemHash, and no account. OpRedeem carries the proof's request id as
// ProofID. Every other op carries AccountID and Gen, the
// session generation of the token that asked for it: the ingestor refuses a
// command whose Gen is no longer the account's, so nothing queued before a
// sign-out-everywhere or a delete applies after it.
type Command struct {
	RequestID   string   `json:"requestId"`
	Op          string   `json:"op"`
	AccountID   string   `json:"accountId,omitempty"`
	Gen         int64    `json:"gen,omitempty"`
	PubKey      string   `json:"pubkey,omitempty"`
	Code        string   `json:"code,omitempty"`
	RedeemHash  string   `json:"redeemHash,omitempty"`
	ProofID     string   `json:"proofId,omitempty"`
	Nodes       []MyNode `json:"nodes,omitempty"`
	DisplayName *string  `json:"displayName,omitempty"`
	CreatedAt   int64    `json:"createdAt"`
}

// ErrInvalidCommand is a command that breaks the shared rules.
var ErrInvalidCommand = errors.New("invalid account command")

// Validate checks c against the shared rules. The ingestor runs it again on
// every file it reads: it never trusts the queue.
func (c Command) Validate() error {
	if !validRequestID(c.RequestID) {
		return ErrInvalidCommand
	}
	if c.Op == OpLogin {
		if c.AccountID != "" || c.Gen != 0 || !antennaclaim.ValidPubKey(c.PubKey) ||
			!antennaclaim.ValidCode(c.Code) || !validHash(c.RedeemHash) || len(c.Nodes) > 0 || c.DisplayName != nil || c.ProofID != "" {
			return ErrInvalidCommand
		}
		return nil
	}
	if !ValidAccountID(c.AccountID) || c.Gen < 1 || c.Code != "" || c.RedeemHash != "" {
		return ErrInvalidCommand
	}
	if (c.Op == OpRedeem) != (c.ProofID != "") {
		return ErrInvalidCommand
	}
	switch c.Op {
	case OpMyNodesSet:
		if c.PubKey != "" || c.DisplayName != nil || validNodes(c.Nodes) != nil {
			return ErrInvalidCommand
		}
	case OpMyNodeAdd:
		if c.PubKey != "" || c.DisplayName != nil || len(c.Nodes) != 1 || validNodes(c.Nodes) != nil {
			return ErrInvalidCommand
		}
	case OpMyNodeRemove:
		if !antennaclaim.ValidPubKey(c.PubKey) || len(c.Nodes) > 0 || c.DisplayName != nil {
			return ErrInvalidCommand
		}
	case OpProfileSet:
		if c.DisplayName == nil || (*c.DisplayName != "" && !ValidDisplayName(*c.DisplayName)) || c.PubKey != "" || len(c.Nodes) > 0 {
			return ErrInvalidCommand
		}
	case OpRedeem:
		if !validRequestID(c.ProofID) || c.PubKey != "" || len(c.Nodes) > 0 || c.DisplayName != nil {
			return ErrInvalidCommand
		}
	case OpSignOutAll, OpDelete:
		if c.PubKey != "" || len(c.Nodes) > 0 || c.DisplayName != nil {
			return ErrInvalidCommand
		}
	default:
		return ErrInvalidCommand
	}
	return nil
}

// validNodes checks a list: at most HardMaxMyNodes valid, distinct keys
// with valid name hints.
func validNodes(nodes []MyNode) error {
	if len(nodes) > HardMaxMyNodes {
		return ErrInvalidCommand
	}
	seen := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if !antennaclaim.ValidPubKey(n.PubKey) || seen[n.PubKey] || !ValidNameHint(n.Name) || n.AddedAt < 0 {
			return ErrInvalidCommand
		}
		seen[n.PubKey] = true
	}
	return nil
}

// validRequestID mirrors channelregistry.ValidID (16–64 lowercase hex).
func validRequestID(id string) bool { return len(id) >= 16 && len(id) <= 64 && isLowerHex(id) }

// Result is what the ingestor writes back for a command.
type Result struct {
	RequestID   string `json:"requestId"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	CompletedAt int64  `json:"completedAt"`
}

// Defaults. Every limit is bounded.
const (
	DefaultSessionDays = 30
	// HardSessionMaxDays is the hard cap of owner decision 3: no session
	// lasts longer than 90 days after the proof that signed in, whatever
	// the config says. sessionDays and sessionMaxDays are clamped to it.
	HardSessionMaxDays          = 90
	DefaultSessionMaxDays       = HardSessionMaxDays
	DefaultLoginCodeMinutes     = 15
	MaxLoginCodeMinutes         = 60
	DefaultRedeemMinutes        = 10
	DefaultMaxMyNodes           = 200
	DefaultLoginsPerHour        = 120
	DefaultLoginsPerMinutePerIP = 2
	DefaultWritesPerHour        = 600
	DefaultWritesPerHourPerAcct = 120
	DefaultMaxPendingLogins     = 512
	DefaultMaxPendingPerKey     = 8
	DefaultMaxQueuedRequests    = 256
	DefaultInactiveMonths       = 24
	MaxInactiveMonths           = 120
	// RenewAfter is how old a token must be before a request re-issues it
	// (the sliding 30 days, owner decision 3).
	RenewAfter = 24 * time.Hour
	// ProofRetention keeps a finished login row this long after its code
	// ran out, so a late status poll still answers.
	ProofRetention    = 24 * time.Hour
	maxResultFactor   = 4
	resultTTL         = 24 * time.Hour
	daysPerMonthApprx = 31 // never earlier than the calendar months
)

// Config is the "accounts" block in config.json, read by the server and the
// ingestor. Zero, negative or out-of-bound values fall back to the defaults.
type Config struct {
	// Enabled opens sign-in. It only takes effect with a strong admin apiKey
	// and a database; off by default. Read from the exact key
	// accounts.enabled only (ApplyGates).
	Enabled              *bool `json:"enabled,omitempty"`
	SessionDays          int   `json:"sessionDays,omitempty"`
	SessionMaxDays       int   `json:"sessionMaxDays,omitempty"`
	LoginCodeMinutes     int   `json:"loginCodeMinutes,omitempty"`
	MaxMyNodes           int   `json:"maxMyNodes,omitempty"`
	LoginsPerHour        int   `json:"loginsPerHour,omitempty"`
	LoginsPerMinutePerIP int   `json:"loginsPerMinutePerIP,omitempty"`
	WritesPerHour        int   `json:"writesPerHour,omitempty"`
	WritesPerHourPerAcct int   `json:"writesPerHourPerAccount,omitempty"`
	MaxPendingLogins     int   `json:"maxPendingLogins,omitempty"`
	MaxPendingPerKey     int   `json:"maxPendingPerKey,omitempty"`
	MaxQueuedRequests    int   `json:"maxQueuedRequests,omitempty"`
	InactiveMonths       int   `json:"inactiveMonths,omitempty"`
}

// Requested reports whether the operator switched accounts on. Callers must
// additionally require a strong admin API key.
func (c *Config) Requested() bool { return c != nil && c.Enabled != nil && *c.Enabled }

// ApplyGates sets the switch from on (configkey.Doc.On), so `Enabled` or
// `ENABLED` never opens sign-in (#884). Safe on nil.
func (c *Config) ApplyGates(on func(path ...string) bool) {
	if c == nil {
		return
	}
	enabled := on("accounts", "enabled")
	c.Enabled = &enabled
}

// Limits are the resolved limits.
type Limits struct {
	SessionTTL           time.Duration // one token's lifetime
	SessionMax           time.Duration // the hard cap after the sign-in proof
	LoginTTL             time.Duration // one sign-in code's lifetime
	RedeemWindow         time.Duration // how long after the proof the browser may redeem
	MaxMyNodes           int
	LoginsPerHour        int
	LoginsPerMinutePerIP int
	WritesPerHour        int // all accounts together; never charged for a revocation
	WritesPerHourPerAcct int // one account; never charged for a revocation
	MaxPendingLogins     int
	MaxPendingPerKey     int
	MaxQueuedRequests    int
	MaxResults           int
	ResultTTL            time.Duration
	InactiveAfter        time.Duration
}

// Limits resolves the configured limits. Safe on a nil *Config.
func (c *Config) Limits() Limits {
	var in Config
	if c != nil {
		in = *c
	}
	maxDays := clamped(in.SessionMaxDays, HardSessionMaxDays, DefaultSessionMaxDays)
	days := min(clamped(in.SessionDays, HardSessionMaxDays, DefaultSessionDays), maxDays)
	l := Limits{
		SessionTTL:           time.Duration(days) * 24 * time.Hour,
		SessionMax:           time.Duration(maxDays) * 24 * time.Hour,
		LoginTTL:             time.Duration(bounded(in.LoginCodeMinutes, MaxLoginCodeMinutes, DefaultLoginCodeMinutes)) * time.Minute,
		RedeemWindow:         DefaultRedeemMinutes * time.Minute,
		MaxMyNodes:           bounded(in.MaxMyNodes, HardMaxMyNodes, DefaultMaxMyNodes),
		LoginsPerHour:        bounded(in.LoginsPerHour, 100000, DefaultLoginsPerHour),
		LoginsPerMinutePerIP: bounded(in.LoginsPerMinutePerIP, 1000, DefaultLoginsPerMinutePerIP),
		WritesPerHour:        bounded(in.WritesPerHour, 100000, DefaultWritesPerHour),
		WritesPerHourPerAcct: bounded(in.WritesPerHourPerAcct, 100000, DefaultWritesPerHourPerAcct),
		MaxPendingLogins:     bounded(in.MaxPendingLogins, 100000, DefaultMaxPendingLogins),
		MaxPendingPerKey:     bounded(in.MaxPendingPerKey, 100, DefaultMaxPendingPerKey),
		MaxQueuedRequests:    bounded(in.MaxQueuedRequests, 100000, DefaultMaxQueuedRequests),
		ResultTTL:            resultTTL,
		InactiveAfter:        time.Duration(bounded(in.InactiveMonths, MaxInactiveMonths, DefaultInactiveMonths)) * daysPerMonthApprx * 24 * time.Hour,
	}
	l.MaxResults = l.MaxQueuedRequests * maxResultFactor
	return l
}

// clamped is def when v <= 0, hi when v > hi, else v: for a hard cap,
// where asking for more must still get the most that is allowed.
func clamped(v, hi, def int) int {
	if v <= 0 {
		return def
	}
	return min(v, hi)
}

// bounded is v when 0 < v <= hi, else def.
func bounded(v, hi, def int) int {
	if v <= 0 || v > hi {
		return def
	}
	return v
}
