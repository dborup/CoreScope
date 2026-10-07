package main

import (
	"strings"
	"testing"
)

// Validation table: trim+lowercase, then exactly ^[0-9a-f]{64}$. A 62-char
// prefix must be rejected — this is what kills the "2–64 topic regex reused"
// mutant (that regex would accept the prefix). Uppercase is accepted because
// it is lowercased first — kills the "no lowercasing" mutant.
func TestNormalizeAndValidateClientRxKey(t *testing.T) {
	const valid = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa11"
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"exact 64 lowercase", valid, true},
		{"uppercase lowercased", strings.ToUpper(valid), true},
		{"surrounding whitespace", "  " + valid + "\n", true},
		{"63 chars", valid[:63], false},
		{"65 chars", valid + "a", false},
		{"62-char prefix", valid[:62], false},
		{"non-hex", strings.Repeat("g", 64), false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key, ok := normalizeAndValidateClientRxKey(c.in)
			if ok != c.want {
				t.Fatalf("validate(%q) = %v, want %v", c.in, ok, c.want)
			}
			if ok && key != strings.ToLower(strings.TrimSpace(c.in)) {
				t.Fatalf("normalized key = %q", key)
			}
		})
	}
}

func TestArgsHaveAdmin(t *testing.T) {
	// Normal `-config` boot must NOT trigger the admin dispatch.
	if argsHaveAdmin([]string{"-config", "config.json"}) {
		t.Fatal("normal boot args must not select admin")
	}
	if argsHaveAdmin(nil) {
		t.Fatal("empty args must not select admin")
	}
	if !argsHaveAdmin([]string{"-config", "c.json", "admin", "delete-client-rx"}) {
		t.Fatal("admin token must select admin dispatch")
	}
}

func TestRunAdminUnknownSubcommand(t *testing.T) {
	if code := runAdmin([]string{"admin"}); code != 2 {
		t.Fatalf("missing subcommand: want exit 2, got %d", code)
	}
	if code := runAdmin([]string{"admin", "frobnicate"}); code != 2 {
		t.Fatalf("unknown subcommand: want exit 2, got %d", code)
	}
}

// A too-short --pubkey is rejected with exit 2 and never reaches the DB.
func TestRunAdminDeleteClientRxRejectsShortKey(t *testing.T) {
	code := runAdminDeleteClientRx([]string{"-config", "config.json", "--pubkey", "aabb"})
	if code != 2 {
		t.Fatalf("short pubkey: want exit 2, got %d", code)
	}
}
