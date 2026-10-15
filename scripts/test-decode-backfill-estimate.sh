#!/usr/bin/env bash
# test-decode-backfill-estimate.sh — unit tests for the #380 read-only
# measurement script. The point under test is the read-only claim: the
# coordinator runs this against a staging copy next to a live ingestor, so it
# must not create a journal, must not touch the file, and must print counts
# rather than row contents.
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
SCRIPT="$HERE/decode-backfill-estimate.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
fails=0

fail() { echo "FAIL: $*" >&2; fails=$((fails + 1)); }
ok() { echo "ok: $*"; }

DB="$TMP/t.db"
sqlite3 "$DB" <<'SQL'
CREATE TABLE transmissions (
	id INTEGER PRIMARY KEY AUTOINCREMENT, raw_hex TEXT NOT NULL,
	hash TEXT NOT NULL UNIQUE, first_seen TEXT NOT NULL,
	payload_type INTEGER, channel_hash TEXT, scope_name TEXT, decoded_json TEXT);
-- 3 channel candidates, 1 already resolved, 2 unknown-scoped, 1 named scope.
-- The remaining 5 rows have scope_name NULL: not transport-scoped at all.
INSERT INTO transmissions (raw_hex, hash, first_seen, payload_type, channel_hash, scope_name) VALUES
	('15', 'h1', 't', 5, 'enc_3A', NULL),
	('15', 'h2', 't', 5, 'enc_3A', NULL),
	('15', 'h3', 't', 5, 'enc_7C', NULL),
	('15', 'h4', 't', 5, '#named', NULL),
	('14', 'h5', 't', 8, NULL, ''),
	('14', 'h6', 't', 8, NULL, ''),
	('14', 'h7', 't', 8, NULL, 'a-region'),
	('14', 'h8', 't', 8, NULL, NULL);
SQL
# Drop the journal the seeding created, so the read-only assertion below is
# about this script and not about sqlite3's own write.
sqlite3 "$DB" 'PRAGMA journal_mode=delete;' >/dev/null
rm -f "$DB-wal" "$DB-shm"
BEFORE=$(cksum < "$DB")

OUT=$("$SCRIPT" "$DB")

expect() {
	if printf '%s' "$OUT" | grep -qE "$1"; then ok "$2"; else fail "$2 (pattern: $1)"; fi
}
expect 'channel candidates \(payload_type 5' "channel candidate section present"
expect '^  rows:              3$' "3 channel candidates counted"
expect '^  distinct hashes:   2$' "2 distinct enc_ hashes counted"
expect '^  already resolved:  1$' "1 resolved channel row counted"
expect '^  rows:              2$' "2 unknown-scoped rows counted"
expect '^  named scopes:      1$' "1 named scope counted"
expect '^  not scoped \(NULL\): 5$' "5 non-transport-scoped rows counted"
expect 'recorded run:        none' "no recorded run on a fresh database"
expect '  candidate rows:    5$' "candidate total is channel + scope"
expect 'total wall clock' "an estimate is printed"

# Read-only: no journal files, and the database byte-identical.
for sfx in -wal -shm -journal; do
	if [ -e "$DB$sfx" ]; then fail "the script created $DB$sfx"; fi
done
ok "no journal files created"
if [ "$(cksum < "$DB")" != "$BEFORE" ]; then fail "the script modified the database"; else ok "database unchanged"; fi

# A pre-#899 database (no scope_name) must degrade to a note, not an error.
OLD="$TMP/old.db"
sqlite3 "$OLD" "CREATE TABLE transmissions (id INTEGER PRIMARY KEY, raw_hex TEXT, hash TEXT UNIQUE, first_seen TEXT, payload_type INTEGER, channel_hash TEXT);
INSERT INTO transmissions VALUES (1, '15', 'o1', 't', 5, 'enc_3A');"
sqlite3 "$OLD" 'PRAGMA journal_mode=delete;' >/dev/null
rm -f "$OLD-wal" "$OLD-shm"
if OLDOUT=$("$SCRIPT" "$OLD" 2>&1); then
	if printf '%s' "$OLDOUT" | grep -q 'column absent'; then
		ok "pre-#899 database reports the absent column"
	else
		fail "pre-#899 database did not report the absent column: $OLDOUT"
	fi
	if printf '%s' "$OLDOUT" | grep -qi 'error'; then fail "pre-#899 run printed an error: $OLDOUT"; else ok "pre-#899 run is error-free"; fi
else
	fail "the script exited non-zero on a pre-#899 database: $OLDOUT"
fi

# Argument handling.
if "$SCRIPT" >/dev/null 2>&1; then fail "a missing argument must fail"; else ok "missing argument rejected"; fi
if "$SCRIPT" "$TMP/does-not-exist.db" >/dev/null 2>&1; then fail "an unreadable path must fail"; else ok "unreadable path rejected"; fi

if [ "$fails" -ne 0 ]; then
	echo "$fails check(s) failed" >&2
	exit 1
fi
echo "all checks passed"
