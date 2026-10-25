#!/usr/bin/env bash
# test-resolved-path-route-repair-estimate.sh — unit tests for the #289 M2
# read-only measurement script. The point under test is the read-only claim:
# the coordinator runs this against a staging copy next to a live ingestor, so
# it must not create a journal, must not touch the file, and must print counts
# rather than row contents. The counted candidate split is the second point: it
# is what decides whether the repair is worth running on a given database.
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
SCRIPT="$HERE/resolved-path-route-repair-estimate.sh"
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
	route_type INTEGER, route_mask INTEGER, payload_type INTEGER, decoded_json TEXT);
CREATE TABLE observations (
	id INTEGER PRIMARY KEY AUTOINCREMENT, transmission_id INTEGER NOT NULL,
	observer_idx INTEGER NOT NULL, path_json TEXT, timestamp INTEGER,
	raw_hex TEXT, resolved_path TEXT);
-- tx 1: FLOOD first reception, both families seen. tx 2: DIRECT, both seen.
-- tx 3: FLOOD, flood only. tx 4: DIRECT, direct only.
INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, route_mask, payload_type, decoded_json) VALUES
	('00', 'h1', 't', 1, 6, 5, '{}'),
	('00', 'h2', 't', 2, 6, 5, '{}'),
	('00', 'h3', 't', 1, 2, 5, '{}'),
	('00', 'h4', 't', 2, 4, 5, '{}');
INSERT INTO observations (transmission_id, observer_idx, path_json, timestamp, raw_hex, resolved_path) VALUES
	-- header candidates: DIRECT frame (16) under the FLOOD tx, stored relay.
	(1, 1, '["c3"]', 1, '1601c3', '["c355"]'),
	(1, 2, '["c3"]', 2, '1601c3', '["c355"]'),
	-- header candidate: FLOOD frame (15) under the DIRECT tx, still NULL.
	(2, 1, '["c3"]', 3, '1501c3', NULL),
	-- not candidates: the frame and t.route_type agree.
	(1, 3, '["c3"]', 4, '1501c3', '["c355"]'),
	(2, 2, '["c3"]', 5, '1601c3', NULL),
	-- not a candidate: no hops at all.
	(1, 4, '[]', 6, '1601c3', NULL),
	-- route_mask candidate: no frame, both families, FLOOD tx.
	(1, 5, '["c3"]', 7, NULL, '["c355"]'),
	-- not candidates: no frame, mixed mask but a DIRECT tx; single-family mask.
	(2, 3, '["c3"]', 8, NULL, NULL),
	(3, 1, '["c3"]', 9, NULL, '["c355"]'),
	(4, 1, '["c3"]', 10, NULL, NULL),
	-- The split is by header family, not by NULL/stored (PR #590 review F6):
	-- a DIRECT-frame candidate that is NULL, and a FLOOD-frame candidate that
	-- carries a stored path.
	(1, 6, '["c3"]', 11, '1601c3', NULL),
	(2, 4, '["c3"]', 12, '1501c3', '["c355"]'),
	-- The transport route types, 3 (17) and 0 (14): they differ from the other
	-- family's non-transport type in bit 1 only (PR #590 review F4).
	(1, 7, '["c3"]', 13, '1701c3', '["c355"]'),
	(2, 5, '["c3"]', 14, '1401c3', NULL),
	-- An unparseable header: reported on its own, because the pass re-checks
	-- it in Go rather than trusting the prefilter's family bit.
	(1, 8, '["c3"]', 15, 'zz01c3', NULL);
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
expect '^  rows:              15$' "15 observations counted"
expect '^  max id \(scan end\): 15$' "the scan end is MAX(observations.id)"
expect '^  with a frame:      11$' "11 rows carry a frame"
expect '^  rows:              8$' "8 header candidates counted (both directions)"
# The split is by header family first. Each bucket mixes NULL and stored rows,
# so a NULL/non-NULL split could not produce these four numbers.
expect '^  DIRECT frame under a FLOOD transmission' "the first direction is labelled by header family"
expect '^    rows:            4 \(stored 3, NULL 1\)$' "4 DIRECT-frame candidates: 3 stored, 1 NULL"
expect '^  FLOOD frame under a DIRECT transmission' "the second direction is labelled by header family"
expect '^    rows:            3 \(stored 1, NULL 2\)$' "3 FLOOD-frame candidates: 1 stored, 2 NULL"
expect '^  unparseable header: 1 ' "the unparseable header is reported on its own"
expect '^  rows:              1$' "1 route_mask candidate counted"
expect 'recorded run:        none' "no recorded run on a fresh database"
expect '  candidate rows:    9$' "candidate total is header + route_mask"
expect 'total wall clock' "an estimate is printed"

# Read-only: no journal files, and the database byte-identical.
for sfx in -wal -shm -journal; do
	if [ -e "$DB$sfx" ]; then fail "the script created $DB$sfx"; fi
done
ok "no journal files created"
if [ "$(cksum < "$DB")" != "$BEFORE" ]; then fail "the script modified the database"; else ok "database unchanged"; fi

# A recorded run is reported back.
sqlite3 "$DB" "CREATE TABLE resolved_path_route_repair_state (id INTEGER PRIMARY KEY, watermark INTEGER,
	ceiling INTEGER, scanned INTEGER, candidates INTEGER, repaired INTEGER, cleared INTEGER,
	started_at TEXT, updated_at TEXT);
INSERT INTO resolved_path_route_repair_state VALUES (1, 10, 10, 4, 4, 1, 3, 's', 'u');" >/dev/null
sqlite3 "$DB" 'PRAGMA journal_mode=delete;' >/dev/null
rm -f "$DB-wal" "$DB-shm"
RUNOUT=$("$SCRIPT" "$DB")
if printf '%s' "$RUNOUT" | grep -qE '^  cleared \(to NULL\): 3$'; then
	ok "a recorded run is reported"
else
	fail "a recorded run was not reported: $RUNOUT"
fi

# A pre-#881 database (no observations.raw_hex) must degrade to a note.
OLD="$TMP/old.db"
sqlite3 "$OLD" "CREATE TABLE transmissions (id INTEGER PRIMARY KEY, raw_hex TEXT, hash TEXT UNIQUE,
	first_seen TEXT, route_type INTEGER, payload_type INTEGER);
CREATE TABLE observations (id INTEGER PRIMARY KEY, transmission_id INTEGER, observer_idx INTEGER,
	path_json TEXT, timestamp INTEGER, resolved_path TEXT);
INSERT INTO transmissions VALUES (1, '00', 'o1', 't', 1, 5);
INSERT INTO observations VALUES (1, 1, 1, '[\"c3\"]', 1, NULL);" >/dev/null
sqlite3 "$OLD" 'PRAGMA journal_mode=delete;' >/dev/null
rm -f "$OLD-wal" "$OLD-shm"
if OLDOUT=$("$SCRIPT" "$OLD" 2>&1); then
	if printf '%s' "$OLDOUT" | grep -q 'pre-#881 database'; then
		ok "pre-#881 database reports the absent column"
	else
		fail "pre-#881 database did not report the absent column: $OLDOUT"
	fi
	if printf '%s' "$OLDOUT" | grep -qi 'error'; then fail "pre-#881 run printed an error: $OLDOUT"; else ok "pre-#881 run is error-free"; fi
	if printf '%s' "$OLDOUT" | grep -qE '  candidate rows:    0$'; then
		ok "pre-#881 database has no candidates"
	else
		fail "pre-#881 database reported candidates: $OLDOUT"
	fi
else
	fail "the script exited non-zero on a pre-#881 database: $OLDOUT"
fi

# Argument handling.
if "$SCRIPT" >/dev/null 2>&1; then fail "a missing argument must fail"; else ok "missing argument rejected"; fi
if "$SCRIPT" "$TMP/does-not-exist.db" >/dev/null 2>&1; then fail "an unreadable path must fail"; else ok "unreadable path rejected"; fi

if [ "$fails" -ne 0 ]; then
	echo "$fails check(s) failed" >&2
	exit 1
fi
echo "all checks passed"
