#!/bin/sh
# neighbor-edge-decay-replay.sh — measure what neighbor_edges count decay
# (#544, neighborEdgeCountHalfLifeDays) would do to hop resolution, on a copy
# of a real database, before decay is enabled anywhere.
#
# Step 1 (cmd/ingestor TestNeighborEdgeDecayReplay_Prepare) copies the source
# with VACUUM INTO through a read-only connection and writes two databases to
# the output directory: lifetime.db (decay off, the default) and decayed.db
# (the real daily decay). Both get DAYS days of new credits at each edge's
# recent rate, derived from the builder's rebuild over the retained
# observations, so they differ only in their counts.
#
# Step 2 (cmd/server TestNeighborEdgeDecayReplay_DBCopy) opens both read-only,
# loads the last HOURS hours of packets from lifetime.db and resolves every
# ambiguous hop against both snapshots with the server's own resolvers
# (resolvePathForObs for live ingest, resolveWithContext with the analytics
# context). It prints kept / flipped / lost / gained per resolver.
#
# RUN IT ON A COPY, never on a live database. The source is only read, but
# opening a WAL database read-only still needs its -shm file, and a database
# that changes during VACUUM INTO gives an inconsistent copy. Nothing is
# written outside OUT_DIR. The output contains counts only, no node names or
# keys.
#
# Usage:
#   scripts/neighbor-edge-decay-replay.sh /path/to/copy.db [OUT_DIR]
# Environment: DAYS (default 30), HALF_LIFE (default 7), HOURS (default 168,
# 0 = all packets). OUT_DIR defaults to a new temporary directory and must be
# empty.
set -eu

if [ $# -lt 1 ] || [ $# -gt 2 ]; then
  echo "usage: $0 /path/to/copy.db [OUT_DIR]" >&2
  exit 2
fi
src=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
if [ ! -f "$src" ]; then
  echo "no such database: $1" >&2
  exit 2
fi
out=${2:-$(mktemp -d)}
mkdir -p "$out"
out=$(cd "$out" && pwd)
repo=$(cd "$(dirname "$0")/.." && pwd)
log="$out/replay.log"

echo "source:  $src"
echo "output:  $out (days=${DAYS:-30}, half-life=${HALF_LIFE:-7}, packets=${HOURS:-168} h)"

echo "== step 1: prepare lifetime.db and decayed.db"
if ! (cd "$repo/cmd/ingestor" &&
  CORESCOPE_DECAY_REPLAY_SRC="$src" CORESCOPE_DECAY_REPLAY_OUT="$out" \
    CORESCOPE_DECAY_REPLAY_DAYS="${DAYS:-30}" CORESCOPE_DECAY_REPLAY_HALF_LIFE="${HALF_LIFE:-7}" \
    go test -count=1 -timeout 0 -v -run '^TestNeighborEdgeDecayReplay_Prepare$' .) >"$log" 2>&1; then
  tail -n 40 "$log" >&2
  exit 1
fi
grep '_test.go:' "$log" || true

echo "== step 2: replay hop resolution"
if ! (cd "$repo/cmd/server" &&
  CORESCOPE_DECAY_REPLAY_LIFETIME_DB="$out/lifetime.db" CORESCOPE_DECAY_REPLAY_DECAYED_DB="$out/decayed.db" \
    CORESCOPE_DECAY_REPLAY_HOURS="${HOURS:-168}" \
    go test -count=1 -timeout 0 -v -run '^TestNeighborEdgeDecayReplay_DBCopy$' .) >>"$log" 2>&1; then
  tail -n 40 "$log" >&2
  exit 1
fi
sed -n '/TestNeighborEdgeDecayReplay_DBCopy/,$p' "$log" | grep '_test.go:' || true
echo "full log: $log"
