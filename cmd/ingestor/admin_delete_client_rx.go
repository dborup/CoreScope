// Package main: the `admin` CLI dispatch for the ingestor.
//
// main() routes to runAdmin before the normal MQTT boot (and before
// flag.Parse) whenever an `admin` token is present in argv, so normal
// `-config` boot is unaffected. Each admin subcommand owns its own FlagSet.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// clientRxPollInterval is how often the online CLI polls for the ingestor's
// result file. A package var so tests can shrink it.
var clientRxPollInterval = 200 * time.Millisecond

// argsHaveAdmin reports whether argv (os.Args[1:]) selects the admin dispatch.
func argsHaveAdmin(args []string) bool {
	for _, a := range args {
		if a == "admin" {
			return true
		}
	}
	return false
}

// normalizeAndValidateClientRxKey trims + lowercases raw, then requires exactly
// 64 hex chars (clientRxPubkeyRe). Returns the normalized key and whether it is
// valid. Lowercasing first is why an uppercase-hex input is accepted.
func normalizeAndValidateClientRxKey(raw string) (string, bool) {
	key := strings.ToLower(strings.TrimSpace(raw))
	return key, clientRxPubkeyRe.MatchString(key)
}

// runAdmin handles `admin <subcommand> …` and returns a process exit code. It
// strips the `admin` keyword and the subcommand token, leaving the flags
// (global `-config` plus subcommand flags, order-independent) for the
// subcommand FlagSet.
func runAdmin(args []string) int {
	rest := make([]string, 0, len(args))
	sub := ""
	seenAdmin := false
	for _, a := range args {
		switch {
		case !seenAdmin && a == "admin":
			seenAdmin = true
		case seenAdmin && sub == "" && !strings.HasPrefix(a, "-"):
			sub = a
		default:
			rest = append(rest, a)
		}
	}
	switch sub {
	case "delete-client-rx":
		return runAdminDeleteClientRx(rest)
	case "":
		fmt.Fprintln(os.Stderr, "admin: missing subcommand (want: delete-client-rx)")
		return 2
	default:
		fmt.Fprintf(os.Stderr, "admin: unknown subcommand %q\n", sub)
		return 2
	}
}

// runAdminDeleteClientRx parses the delete-client-rx flags, validates the key,
// and runs either the read-only dry-run or the online (queued) delete.
func runAdminDeleteClientRx(args []string) int {
	fs := flag.NewFlagSet("admin delete-client-rx", flag.ContinueOnError)
	configPath := fs.String("config", "config.json", "path to config file")
	pubkey := fs.String("pubkey", "", "contributor public key to erase (64 lowercase hex)")
	dryRun := fs.Bool("dry-run", false, "report counts without deleting (read-only)")
	wait := fs.Duration("wait", 60*time.Second, "how long to wait for the ingestor to finish")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Trim + lowercase, then require exactly 64 hex. Never the 2–64 topic regex.
	key, ok := normalizeAndValidateClientRxKey(*pubkey)
	if !ok {
		fmt.Fprintln(os.Stderr, "delete-client-rx: --pubkey must be exactly 64 hex chars")
		return 2
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "delete-client-rx: config: %v\n", err)
		return 1
	}

	if *dryRun {
		return runAdminDeleteClientRxDryRun(cfg.DBPath, key)
	}
	return runAdminDeleteClientRxOnline(cfg.DBPath, key, *wait)
}

// clientRxReadOnlyDSN builds a physically read-only SQLite DSN (VFS-level
// mode=ro — no writes possible even by a buggy statement), matching the
// server's handle (cmd/server/db.go).
func clientRxReadOnlyDSN(dbPath string) string {
	return fmt.Sprintf("file:%s?mode=ro&_journal_mode=WAL&_busy_timeout=5000", dbPath)
}

// clientRxCoverageCounts returns the per-contributor counts and rx_at range
// over a connection the caller owns (read-only for dry-run).
func clientRxCoverageCounts(db *sql.DB, key string) (receptions, observers int64, rng string, err error) {
	var minAt, maxAt sql.NullString
	if err = db.QueryRow(
		`SELECT COUNT(*), MIN(rx_at), MAX(rx_at) FROM client_receptions WHERE rx_pubkey = ?`, key,
	).Scan(&receptions, &minAt, &maxAt); err != nil {
		return 0, 0, "", err
	}
	if err = db.QueryRow(
		`SELECT COUNT(*) FROM client_observers WHERE pubkey = ?`, key,
	).Scan(&observers); err != nil {
		return 0, 0, "", err
	}
	rng = "none"
	if minAt.Valid && maxAt.Valid {
		rng = minAt.String + " … " + maxAt.String
	}
	return receptions, observers, rng, nil
}

func runAdminDeleteClientRxDryRun(dbPath, key string) int {
	db, err := sql.Open("sqlite", clientRxReadOnlyDSN(dbPath))
	if err != nil {
		fmt.Fprintf(os.Stderr, "delete-client-rx: open read-only: %v\n", err)
		return 1
	}
	defer db.Close()

	receptions, observers, rng, err := clientRxCoverageCounts(db, key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "delete-client-rx: dry-run query: %v\n", err)
		return 1
	}
	fmt.Printf("dry-run: would delete client_receptions=%d client_observers=%d rx_at=[%s]\n",
		receptions, observers, rng)
	fmt.Println("dry-run: no changes written")
	return 0
}

func runAdminDeleteClientRxOnline(dbPath, key string, wait time.Duration) int {
	id := clientRxNewID()
	if err := clientRxWriteRequest(dbPath, clientRxDeleteRequest{
		ID:          id,
		RequestedAt: time.Now().UTC(),
		Pubkey:      key,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "delete-client-rx: enqueue: %v\n", err)
		return 1
	}

	deadline := time.Now().Add(wait)
	for {
		res, err := clientRxReadResult(dbPath, id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "delete-client-rx: read result: %v\n", err)
			return 1
		}
		if res != nil {
			if res.Error != "" {
				fmt.Fprintf(os.Stderr, "delete-client-rx: ingestor reported error: %s\n", res.Error)
				return 1
			}
			fmt.Printf("deleted client_receptions=%d client_observers=%d\n",
				res.ClientReceptions, res.ClientObservers)
			printObserverBlacklistHint()
			return 0
		}
		if !time.Now().Before(deadline) {
			fmt.Fprintln(os.Stderr, "delete-client-rx: queued — ingestor not running?")
			return 2
		}
		time.Sleep(clientRxPollInterval)
	}
}

// printObserverBlacklistHint reminds the operator that a delete alone is not
// durable: a backdated upload can re-insert the contributor's coverage.
func printObserverBlacklistHint() {
	fmt.Println("hint: add this key to observerBlacklist in the ingestor config, " +
		"or later uploads (rx_at can be backdated up to 30 days) will re-insert its coverage.")
}
