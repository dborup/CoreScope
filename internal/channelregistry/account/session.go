package account

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The session token (#1018 research §4.2): base64url(payload) "."
// base64url(HMAC-SHA256(key, payload)). Standard library only. The server
// mints and checks it without a database write; revocation is the account's
// session_gen, which the ingestor increments (sign out everywhere) or
// removes with the row (delete).

// KeyFileName is the session signing key, next to the database. The ingestor
// creates it (32 random bytes, mode 0600) and the server only reads it. It is
// never logged, never served and not in GET /api/backup, which copies the
// database alone. Replacing it signs every account out.
const KeyFileName = "account-session.key"

// KeyLen is the key's length in bytes.
const KeyLen = 32

// CookieName is the session cookie. The __Host- prefix makes the browser
// insist on Secure, Path=/ and no Domain.
const CookieName = "__Host-mv_session"

// TokenVersion is SessionClaims.V of this format.
const TokenVersion = 1

// KeyPath returns the key file path for a database path.
func KeyPath(dbPath string) string { return filepath.Join(filepath.Dir(dbPath), KeyFileName) }

// Key file errors.
var (
	ErrNoKey       = errors.New("the account session key does not exist yet")
	ErrKeyTooOpen  = errors.New("the account session key file is readable by other users")
	ErrKeyTooShort = errors.New("the account session key file is too short")
)

// ReadKey reads the key file of dbPath. A file that group or others may
// access is refused: it could mint any session.
func ReadKey(dbPath string) ([]byte, error) {
	p := KeyPath(dbPath)
	info, err := os.Stat(p)
	if os.IsNotExist(err) {
		return nil, ErrNoKey
	}
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, ErrKeyTooOpen
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if len(b) < KeyLen {
		return nil, ErrKeyTooShort
	}
	return b[:KeyLen], nil
}

// SessionClaims is the token payload. AccountID is the random account id,
// never the companion key.
type SessionClaims struct {
	V         int    `json:"v"`
	AccountID string `json:"aid"`
	Gen       int64  `json:"gen"`  // accounts.session_gen at issue
	AuthAt    int64  `json:"auth"` // Unix ms of the proof that signed in
	IssuedAt  int64  `json:"iat"`  // Unix ms
	ExpiresAt int64  `json:"exp"`  // Unix ms
}

// Token errors. Callers answer every one the same way (signed out).
var (
	ErrTokenInvalid = errors.New("invalid session")
	ErrTokenExpired = errors.New("session expired")
)

// maxTokenLen bounds what VerifySession decodes.
const maxTokenLen = 512

// SignSession returns the token for c.
func SignSession(key []byte, c SessionClaims) (string, error) {
	if len(key) < KeyLen {
		return "", ErrKeyTooShort
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	p := base64.RawURLEncoding.EncodeToString(payload)
	return p + "." + base64.RawURLEncoding.EncodeToString(mac(key, p)), nil
}

func mac(key []byte, payload string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(payload))
	return h.Sum(nil)
}

// VerifySession checks tok's signature and lifetime at nowMs: it must be
// unexpired and within max of the proof that signed in (the hard cap,
// owner decision 3). It does not check the generation: that needs the
// account row (the caller's one primary-key read).
func VerifySession(key []byte, tok string, nowMs, maxMs int64) (SessionClaims, error) {
	var c SessionClaims
	if len(key) < KeyLen || len(tok) == 0 || len(tok) > maxTokenLen {
		return c, ErrTokenInvalid
	}
	p, sig, ok := strings.Cut(tok, ".")
	if !ok {
		return c, ErrTokenInvalid
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, mac(key, p)) {
		return c, ErrTokenInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return c, ErrTokenInvalid
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return SessionClaims{}, ErrTokenInvalid
	}
	if c.V != TokenVersion || !ValidAccountID(c.AccountID) || c.Gen < 1 || c.AuthAt <= 0 ||
		c.IssuedAt < c.AuthAt || c.ExpiresAt <= c.IssuedAt {
		return SessionClaims{}, ErrTokenInvalid
	}
	if nowMs >= c.ExpiresAt || nowMs >= c.AuthAt+maxMs || c.ExpiresAt > c.AuthAt+maxMs {
		return SessionClaims{}, ErrTokenExpired
	}
	return c, nil
}

// NewClaims returns the claims of a session issued at nowMs for a sign-in
// proven at authAt: it lasts l.SessionTTL, never past authAt + l.SessionMax.
func NewClaims(accountID string, gen, authAt, nowMs int64, l Limits) (SessionClaims, error) {
	exp := min(nowMs+l.SessionTTL.Milliseconds(), authAt+l.SessionMax.Milliseconds())
	if exp <= nowMs {
		return SessionClaims{}, fmt.Errorf("%w: past the hard cap", ErrTokenExpired)
	}
	return SessionClaims{V: TokenVersion, AccountID: accountID, Gen: gen, AuthAt: authAt, IssuedAt: nowMs, ExpiresAt: exp}, nil
}

// NeedsRenewal reports whether a valid token should be re-issued on use: it
// is older than RenewAfter and a new one would last longer.
func (c SessionClaims) NeedsRenewal(nowMs int64, l Limits) bool {
	if nowMs-c.IssuedAt < RenewAfter.Milliseconds() {
		return false
	}
	return min(nowMs+l.SessionTTL.Milliseconds(), c.AuthAt+l.SessionMax.Milliseconds()) > c.ExpiresAt
}
