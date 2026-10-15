#!/usr/bin/env bash
# decode-backfill-estimate.sh — read-only sizing for the #380 historical
# decode backfill.
#
# Prints counts and an estimated cost for one full pass. It NEVER writes:
# the database is opened through an immutable URI, so SQLite cannot create a
# -wal/-shm file or take a write lock, and it is safe to run next to a live
# ingestor. No row contents, channel names, region names or key material are
# printed — counts only.
#
# Usage:
#   scripts/decode-backfill-estimate.sh /path/to/corescope.db
#
# The throughput used for the estimate is the measured
# BenchmarkDecodeBackfillRun figure (see the PR body for the hardware); pass
# a different one as the second argument to re-scale:
#   scripts/decode-backfill-estimate.sh /path/to/db 400000
set -eu

DB=${1:-}
ROWS_PER_SEC=${2:-856000}
if [ -z "$DB" ]; then
	echo "usage: $0 <path-to-sqlite-db> [table_rows_per_second]" >&2
	exit 2
fi
if [ ! -r "$DB" ]; then
	echo "error: $DB is not readable" >&2
	exit 2
fi
command -v sqlite3 >/dev/null 2>&1 || { echo "error: sqlite3 not found" >&2; exit 2; }

# file: URI with immutable=1 — read-only, no locking, no journal files.
ABS=$(cd "$(dirname "$DB")" && pwd)/$(basename "$DB")
URI="file:${ABS}?immutable=1"

q() { sqlite3 -readonly "$URI" "$1"; }

# has_col <table> <column> — pre-#899 databases have no transmissions.scope_name,
# and querying it would be an error rather than a count of zero.
has_col() {
	sqlite3 -readonly "$URI" "SELECT COUNT(*) FROM pragma_table_info('$1') WHERE name = '$2';"
}
SCOPE_COL=$(has_col transmissions scope_name)

echo "database:            $ABS"
echo "file size (bytes):   $(wc -c < "$DB" | tr -d ' ')"
echo

# Scanned extent: the pass walks transmissions.id from 0 to MAX(id).
echo "transmissions"
echo "  rows:              $(q 'SELECT COUNT(*) FROM transmissions;')"
echo "  max id (scan end): $(q 'SELECT COALESCE(MAX(id), 0) FROM transmissions;')"
echo

# Channel candidates: GRP_TXT rows still carrying the enc_<HEX> placeholder.
echo "channel candidates (payload_type 5, channel_hash LIKE 'enc_%')"
echo "  rows:              $(q "SELECT COUNT(*) FROM transmissions WHERE payload_type = 5 AND channel_hash LIKE 'enc_%';")"
echo "  distinct hashes:   $(q "SELECT COUNT(DISTINCT channel_hash) FROM transmissions WHERE payload_type = 5 AND channel_hash LIKE 'enc_%';")"
echo "  already resolved:  $(q "SELECT COUNT(*) FROM transmissions WHERE payload_type = 5 AND channel_hash IS NOT NULL AND channel_hash NOT LIKE 'enc_%';")"
echo

# Scope candidates: transport-scoped rows whose region is unknown. NULL means
# "not transport-scoped" and is not a candidate.
echo "region-scope candidates (scope_name = '')"
SCOPE_CAND=0
if [ "$SCOPE_COL" = "1" ]; then
	SCOPE_CAND=$(q "SELECT COUNT(*) FROM transmissions WHERE scope_name = '';")
	echo "  rows:              $SCOPE_CAND"
	echo "  named scopes:      $(q "SELECT COUNT(*) FROM transmissions WHERE scope_name IS NOT NULL AND scope_name <> '';")"
	echo "  not scoped (NULL): $(q 'SELECT COUNT(*) FROM transmissions WHERE scope_name IS NULL;')"
else
	echo "  column absent (pre-#899 database): no region-scope candidates"
fi
echo

# Progress of any pass that already ran on this database.
if [ "$(q "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'decode_backfill_state';")" = "1" ]; then
	echo "recorded run"
	q "SELECT '  kinds:             ' || kinds || char(10) ||
	          '  status:            ' || status || char(10) ||
	          '  watermark/ceiling: ' || watermark || '/' || ceiling || char(10) ||
	          '  revision:          ' || revision || char(10) ||
	          '  scanned:           ' || scanned || char(10) ||
	          '  channel rewritten: ' || channel_updated || char(10) ||
	          '  scope rewritten:   ' || scope_updated || char(10) ||
	          '  abstained:         ' || ambiguous || char(10) ||
	          '  undecodable:       ' || decode_errors || char(10) ||
	          '  updated at:        ' || updated_at
	   FROM decode_backfill_state WHERE id = 1;"
	echo "  covered keys:      $(q "SELECT COUNT(*) FROM decode_backfill_keys WHERE state = 'covered';") (digests only)"
else
	echo "recorded run:        none (no #380 pass has run on this database)"
fi
echo

# Estimate. The pass walks the id range once; the deliberate pause between
# batches dominates the wall clock, so both parts are reported.
MAXID=$(q 'SELECT COALESCE(MAX(id), 0) FROM transmissions;')
CHAN_CAND=$(q "SELECT COUNT(*) FROM transmissions WHERE payload_type = 5 AND channel_hash LIKE 'enc_%';")
CAND=$((CHAN_CAND + SCOPE_CAND))
echo "estimated cost of one full pass (defaults: batchSize 300, pauseMs 250)"
awk -v maxid="$MAXID" -v cand="$CAND" -v rps="$ROWS_PER_SEC" 'BEGIN {
	work = (rps > 0) ? maxid / rps : 0;
	batches = (cand > 0) ? int((cand + 299) / 300) : 0;
	windows = int((maxid + 19999) / 20000);
	if (batches < windows) batches = windows;
	pause = batches * 0.25;
	printf "  candidate rows:    %d\n", cand;
	printf "  batches:           %d\n", batches;
	printf "  decode+write:      %.1f s (at %d table rows/s)\n", work, rps;
	printf "  deliberate pauses: %.1f s\n", pause;
	printf "  total wall clock:  %.1f s (%.1f min)\n", work + pause, (work + pause) / 60;
}'
echo
echo "This script wrote nothing. Verify with: ls -l \"$ABS\"-wal \"$ABS\"-shm 2>/dev/null"
