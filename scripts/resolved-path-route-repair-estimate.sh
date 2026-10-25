#!/usr/bin/env bash
# resolved-path-route-repair-estimate.sh — read-only sizing for the #289 M2
# resolved_path route-type repair.
#
# Prints the candidate counts and an estimated cost for one full pass. It NEVER
# writes: the database is opened through an immutable URI, so SQLite cannot
# create a -wal/-shm file or take a write lock, and it is safe to run next to a
# live ingestor. No pubkeys, paths or frame bytes are printed — counts only.
#
# RUN IT ON A SNAPSHOT COPY. immutable=1 is what makes this read-only, and it
# also tells SQLite to ignore the -wal file: against a live database the counts
# miss every row that has not been checkpointed yet, so they are a lower bound,
# and a file changing underneath an immutable reader can make that reader
# report wrong counts or a spurious SQLITE_CORRUPT (the database itself is
# never damaged). A backup file or a stopped container's volume gives exact
# numbers. Two further approximations, on any input:
#   - the candidate count is the SQL prefilter the pass uses, which is a
#     superset: the pass re-checks each row in Go with the #289/#308 helper and
#     writes only the rows whose route type actually changes the resolve;
#   - the scan rate is the BenchmarkResolvedPathRouteRepairPass_289 figure
#     below, measured warm-cache on a synthetic database. It is not a
#     measurement of this host or of cold disk I/O.
# It also runs several full-table scans of observations, so on a multi-GB
# database the script's own wall clock is a minute or more.
#
# Usage:
#   scripts/resolved-path-route-repair-estimate.sh /path/to/corescope.db
#
# The scan rate used for the estimate is the measured benchmark figure (see the
# PR body for the hardware); pass a different one as the second argument:
#   scripts/resolved-path-route-repair-estimate.sh /path/to/db 1200000
set -eu

DB=${1:-}
ROWS_PER_SEC=${2:-2400000}
if [ -z "$DB" ]; then
	echo "usage: $0 <path-to-sqlite-db> [observation_rows_per_second]" >&2
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

# has_col <table> <column> — pre-#881 databases have no observations.raw_hex
# and pre-#89 ones no transmissions.route_mask; querying either would be an
# error rather than a count of zero.
has_col() {
	sqlite3 -readonly "$URI" "SELECT COUNT(*) FROM pragma_table_info('$1') WHERE name = '$2';"
}
RAW_COL=$(has_col observations raw_hex)
MASK_COL=$(has_col transmissions route_mask)

echo "database:            $ABS"
echo "file size (bytes):   $(wc -c < "$DB" | tr -d ' ')"
echo

# Scanned extent: the pass walks observations.id from 0 to MAX(id), which is
# why NULL rows below the #188 backfill watermark are covered too.
echo "observations"
echo "  rows:              $(q 'SELECT COUNT(*) FROM observations;')"
echo "  max id (scan end): $(q 'SELECT COALESCE(MAX(id), 0) FROM observations;')"
if [ "$RAW_COL" = "1" ]; then
	echo "  with a frame:      $(q 'SELECT COUNT(*) FROM observations WHERE length(raw_hex) >= 2;')"
else
	echo "  with a frame:      column absent (pre-#881 database)"
fi
echo

# Candidates, split by bug direction. Both use the pass's own prefilter: the
# route family is bit 1 of the low nibble of the frame's header byte, i.e. of
# raw_hex's second character, and (route_type & 2) is the transmission's.
HDR_CAND=0
HDR_NULL=0
MASK_CAND=0
if [ "$RAW_COL" = "1" ]; then
	HDR="length(o.raw_hex) >= 2
		AND ((instr('0123456789abcdef', lower(substr(o.raw_hex, 2, 1))) - 1) & 2) != (COALESCE(t.route_type, -1) & 2)
		AND COALESCE(o.path_json, '') NOT IN ('', '[]')"
	HDR_CAND=$(q "SELECT COUNT(*) FROM observations o JOIN transmissions t ON t.id = o.transmission_id WHERE $HDR;")
	HDR_NULL=$(q "SELECT COUNT(*) FROM observations o JOIN transmissions t ON t.id = o.transmission_id WHERE $HDR AND o.resolved_path IS NULL;")
fi
echo "candidates: frame header disagrees with t.route_type"
echo "  rows:              $HDR_CAND"
echo "  of those NULL:     $HDR_NULL (FLOOD read as DIRECT; never revisited by the #188 backfill)"
echo "  of those stored:   $((HDR_CAND - HDR_NULL)) (DIRECT read as FLOOD; may carry a wrong relay)"
echo

if [ "$RAW_COL" = "1" ] && [ "$MASK_COL" = "1" ]; then
	MASK_CAND=$(q "SELECT COUNT(*) FROM observations o JOIN transmissions t ON t.id = o.transmission_id
		WHERE COALESCE(length(o.raw_hex), 0) < 2
			AND t.route_mask IS NOT NULL
			AND (t.route_mask & 3) != 0 AND (t.route_mask & 12) != 0
			AND COALESCE(t.route_type, -1) IN (0, 1)
			AND COALESCE(o.path_json, '') NOT IN ('', '[]');")
	echo "candidates: no frame, route_mask holds both families, FLOOD transmission"
	echo "  rows:              $MASK_CAND"
else
	echo "candidates: no frame, route_mask holds both families, FLOOD transmission"
	echo "  column absent (pre-#881 or pre-#89 database): not counted"
fi
echo

# Progress of a pass that already ran on this database.
if [ "$(q "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'resolved_path_route_repair_state';")" = "1" ]; then
	echo "recorded run"
	q "SELECT '  watermark/ceiling: ' || watermark || '/' || ceiling || char(10) ||
	          '  scanned:           ' || scanned || char(10) ||
	          '  candidates:        ' || candidates || char(10) ||
	          '  repaired:          ' || repaired || char(10) ||
	          '  cleared (to NULL): ' || cleared || char(10) ||
	          '  started at:        ' || started_at || char(10) ||
	          '  updated at:        ' || updated_at
	   FROM resolved_path_route_repair_state WHERE id = 1;"
else
	echo "recorded run:        none (no #289 M2 pass has run on this database)"
fi
echo

# Estimate. The pass walks the id range once in bounded windows; the deliberate
# pause between windows dominates the wall clock, so both parts are reported.
MAXID=$(q 'SELECT COALESCE(MAX(id), 0) FROM observations;')
CAND=$((HDR_CAND + MASK_CAND))
echo "estimated cost of one full pass (defaults: idWindow 20000, batchSize 500, pause 250 ms)"
awk -v maxid="$MAXID" -v cand="$CAND" -v rps="$ROWS_PER_SEC" 'BEGIN {
	work = (rps > 0) ? maxid / rps : 0;
	windows = int((maxid + 19999) / 20000);
	batches = (cand > 0) ? int((cand + 499) / 500) : 0;
	if (batches < windows) batches = windows;
	pause = batches * 0.25;
	printf "  candidate rows:    %d\n", cand;
	printf "  batches:           %d\n", batches;
	printf "  scan+write:        %.1f s (at %d observation rows/s)\n", work, rps;
	printf "  deliberate pauses: %.1f s\n", pause;
	printf "  total wall clock:  %.1f s (%.1f min)\n", work + pause, (work + pause) / 60;
}'
echo
echo "Counts are exact on a snapshot copy; on a live WAL database they omit"
echo "un-checkpointed rows and the batch count is a lower bound (see the header)."
echo "This script wrote nothing. Verify with: ls -l \"$ABS\"-wal \"$ABS\"-shm 2>/dev/null"
