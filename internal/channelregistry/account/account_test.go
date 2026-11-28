package account

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #1018 M1 — the shared account rules and the session token.

var (
	testKeyA = []byte("0123456789abcdef0123456789abcdef")
	testKeyB = []byte("fedcba9876543210fedcba9876543210")
	testAID  = strings.Repeat("ab", 16)
	testPK   = strings.Repeat("cd", 32)
)

const day = int64(24 * time.Hour / time.Millisecond)

func TestSessionRoundTrip(t *testing.T) {
	l := (*Config)(nil).Limits()
	now := int64(1_800_000_000_000)
	c, err := NewClaims(testAID, 3, now-1000, now, l)
	if err != nil {
		t.Fatal(err)
	}
	if c.ExpiresAt != now+30*day {
		t.Fatalf("a session lasts 30 days, got %d ms", c.ExpiresAt-now)
	}
	tok, err := SignSession(testKeyA, c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := VerifySession(testKeyA, tok, now+1, l.SessionMax.Milliseconds())
	if err != nil || got != c {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	if strings.Contains(tok, testPK) {
		t.Fatal("the token must not carry the companion key")
	}
}

func TestSessionRefusesTamperWrongKeyForgedAndExpired(t *testing.T) {
	l := (*Config)(nil).Limits()
	now := int64(1_800_000_000_000)
	c, _ := NewClaims(testAID, 1, now, now, l)
	tok, _ := SignSession(testKeyA, c)
	maxMs := l.SessionMax.Milliseconds()

	if _, err := VerifySession(testKeyB, tok, now, maxMs); err != ErrTokenInvalid {
		t.Errorf("wrong key: %v", err)
	}
	p, sig, _ := strings.Cut(tok, ".")
	// A payload with another account id, re-encoded, keeps the old MAC.
	forged := c
	forged.AccountID = strings.Repeat("ef", 16)
	ftok, _ := SignSession(testKeyB, forged)
	fp, _, _ := strings.Cut(ftok, ".")
	if _, err := VerifySession(testKeyA, fp+"."+sig, now, maxMs); err != ErrTokenInvalid {
		t.Errorf("forged payload: %v", err)
	}
	flipped := []byte(sig)
	if flipped[0] == 'A' {
		flipped[0] = 'B'
	} else {
		flipped[0] = 'A'
	}
	for name, bad := range map[string]string{
		"flipped mac": p + "." + string(flipped),
		"no dot":      p + sig,
		"empty":       "",
		"huge":        strings.Repeat("a", 600) + "." + sig,
	} {
		if _, err := VerifySession(testKeyA, bad, now, maxMs); err != ErrTokenInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := VerifySession(testKeyA, tok, c.ExpiresAt, maxMs); err != ErrTokenExpired {
		t.Errorf("expired: %v", err)
	}
}

func TestSessionHardCapAndRenewal(t *testing.T) {
	l := (*Config)(nil).Limits()
	auth := int64(1_800_000_000_000)
	maxMs := l.SessionMax.Milliseconds()
	// Day 80: a renewal may not run past day 90.
	now := auth + 80*day
	c, err := NewClaims(testAID, 1, auth, now, l)
	if err != nil {
		t.Fatal(err)
	}
	if c.ExpiresAt != auth+90*day {
		t.Fatalf("the hard cap is 90 days after the proof, got day %d", (c.ExpiresAt-auth)/day)
	}
	if _, err := NewClaims(testAID, 1, auth, auth+90*day, l); err == nil {
		t.Fatal("no session may be issued at the hard cap")
	}
	// A token claiming to outlive the cap is refused even though it is
	// signed (a mutant that drops the cap check would accept it).
	bad := SessionClaims{V: 1, AccountID: testAID, Gen: 1, AuthAt: auth, IssuedAt: auth, ExpiresAt: auth + 200*day}
	tok, _ := SignSession(testKeyA, bad)
	if _, err := VerifySession(testKeyA, tok, auth+100*day, maxMs); err != ErrTokenExpired {
		t.Fatalf("past the cap: %v", err)
	}
	fresh, _ := NewClaims(testAID, 1, auth, auth, l)
	if fresh.NeedsRenewal(auth+day/2, l) {
		t.Error("a token younger than a day is not renewed")
	}
	if !fresh.NeedsRenewal(auth+2*day, l) {
		t.Error("a two-day-old token is renewed (sliding 30 days)")
	}
	capped, _ := NewClaims(testAID, 1, auth, auth+80*day, l)
	if capped.NeedsRenewal(auth+85*day, l) {
		t.Error("a token already at the hard cap gains nothing from renewal")
	}
}

func TestReadKeyRefusesOpenOrShortFiles(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "x.db")
	if _, err := ReadKey(db); err != ErrNoKey {
		t.Fatalf("missing: %v", err)
	}
	p := KeyPath(db)
	if err := os.WriteFile(p, testKeyA, 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, 0o644)
	if _, err := ReadKey(db); err != ErrKeyTooOpen {
		t.Fatalf("0644: %v", err)
	}
	os.Chmod(p, 0o600)
	if k, err := ReadKey(db); err != nil || string(k) != string(testKeyA) {
		t.Fatalf("0600: %v", err)
	}
	os.WriteFile(p, []byte("short"), 0o600)
	if _, err := ReadKey(db); err != ErrKeyTooShort {
		t.Fatalf("short: %v", err)
	}
}

func TestCommandValidate(t *testing.T) {
	hash := HashRedeemSecret(strings.Repeat("1", 32))
	name := "Anna"
	empty := ""
	long := strings.Repeat("x", 33)
	good := []Command{
		{RequestID: "0123456789abcdef", Op: OpLogin, PubKey: testPK, Code: "7KX9QM", RedeemHash: hash},
		{RequestID: "0123456789abcdef", Op: OpMyNodesSet, AccountID: testAID, Gen: 1},
		{RequestID: "0123456789abcdef", Op: OpMyNodesSet, AccountID: testAID, Gen: 1, Nodes: []MyNode{{PubKey: testPK, Name: "R1"}}},
		{RequestID: "0123456789abcdef", Op: OpMyNodeAdd, AccountID: testAID, Gen: 2, Nodes: []MyNode{{PubKey: testPK}}},
		{RequestID: "0123456789abcdef", Op: OpMyNodeRemove, AccountID: testAID, Gen: 2, PubKey: testPK},
		{RequestID: "0123456789abcdef", Op: OpProfileSet, AccountID: testAID, Gen: 1, DisplayName: &name},
		{RequestID: "0123456789abcdef", Op: OpProfileSet, AccountID: testAID, Gen: 1, DisplayName: &empty},
		{RequestID: "0123456789abcdef", Op: OpSignOutAll, AccountID: testAID, Gen: 1},
		{RequestID: "0123456789abcdef", Op: OpDelete, AccountID: testAID, Gen: 1},
	}
	for i, c := range good {
		if err := c.Validate(); err != nil {
			t.Errorf("good %d (%s): %v", i, c.Op, err)
		}
	}
	dup := []MyNode{{PubKey: testPK}, {PubKey: testPK}}
	bad := map[string]Command{
		"login with account": {RequestID: "0123456789abcdef", Op: OpLogin, AccountID: testAID, PubKey: testPK, Code: "7KX9QM", RedeemHash: hash},
		"login bad code":     {RequestID: "0123456789abcdef", Op: OpLogin, PubKey: testPK, Code: "7KX9QI", RedeemHash: hash},
		"login no hash":      {RequestID: "0123456789abcdef", Op: OpLogin, PubKey: testPK, Code: "7KX9QM"},
		"login upper key":    {RequestID: "0123456789abcdef", Op: OpLogin, PubKey: strings.ToUpper(testPK), Code: "7KX9QM", RedeemHash: hash},
		"write without gen":  {RequestID: "0123456789abcdef", Op: OpSignOutAll, AccountID: testAID},
		"write bad account":  {RequestID: "0123456789abcdef", Op: OpDelete, AccountID: "abc", Gen: 1},
		"set duplicate":      {RequestID: "0123456789abcdef", Op: OpMyNodesSet, AccountID: testAID, Gen: 1, Nodes: dup},
		"remove no key":      {RequestID: "0123456789abcdef", Op: OpMyNodeRemove, AccountID: testAID, Gen: 1},
		"profile too long":   {RequestID: "0123456789abcdef", Op: OpProfileSet, AccountID: testAID, Gen: 1, DisplayName: &long},
		"profile missing":    {RequestID: "0123456789abcdef", Op: OpProfileSet, AccountID: testAID, Gen: 1},
		"unknown op":         {RequestID: "0123456789abcdef", Op: "link", AccountID: testAID, Gen: 1},
		"bad request id":     {RequestID: "xyz", Op: OpDelete, AccountID: testAID, Gen: 1},
		"name hint control":  {RequestID: "0123456789abcdef", Op: OpMyNodesSet, AccountID: testAID, Gen: 1, Nodes: []MyNode{{PubKey: testPK, Name: "a\nb"}}},
	}
	bad["add two"] = Command{RequestID: "0123456789abcdef", Op: OpMyNodeAdd, AccountID: testAID, Gen: 1, Nodes: []MyNode{{PubKey: testPK}, {PubKey: strings.Repeat("ef", 32)}}}
	for name, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	tooMany := make([]MyNode, HardMaxMyNodes+1)
	if validNodes(tooMany) == nil {
		t.Error("a list past HardMaxMyNodes is refused")
	}
}

func TestDisplayName(t *testing.T) {
	for _, s := range []string{"Anna", "Øresund 🛰", strings.Repeat("å", 32)} {
		if !ValidDisplayName(s) {
			t.Errorf("%q refused", s)
		}
	}
	for _, s := range []string{"", " Anna", "Anna ", "a\u0000b", strings.Repeat("å", 33), "\xff"} {
		if ValidDisplayName(s) {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestLimitsDefaultsAndBounds(t *testing.T) {
	l := (*Config)(nil).Limits()
	if l.SessionTTL != 30*24*time.Hour || l.SessionMax != 90*24*time.Hour || l.LoginTTL != 15*time.Minute ||
		l.MaxMyNodes != 200 || l.LoginsPerHour != 120 || l.InactiveAfter < 730*24*time.Hour {
		t.Fatalf("defaults: %+v", l)
	}
	l = (&Config{SessionDays: 200, SessionMaxDays: 20, MaxMyNodes: 5000, InactiveMonths: -1}).Limits()
	if l.SessionMax != 20*24*time.Hour || l.SessionTTL != l.SessionMax || l.MaxMyNodes != 200 {
		t.Fatalf("bounds: %+v", l)
	}
	var c *Config
	if c.Requested() {
		t.Fatal("nil config is off")
	}
	c = &Config{}
	c.ApplyGates(func(p ...string) bool { return strings.Join(p, ".") == "accounts.enabled" })
	if !c.Requested() {
		t.Fatal("accounts.enabled opens it")
	}
}
