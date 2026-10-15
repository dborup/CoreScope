#!/usr/bin/env bash
# wardriving-position-coverage.sh — issue #381 coverage measurement.
#
# Read-only: a single GET against /api/analytics/wardriving (no API key,
# that route carries none -- see cmd/server/routes.go). Prints ONLY
# aggregate counts of each observer's positionSource ("real"/"iata"/
# "none") plus the total observer count -- never an observer name, id, or
# coordinate. Safe to run against staging and to paste the output
# anywhere; it does not touch stg/prod state (GET only) and does not
# publish any precise or individual observer position.
#
# Usage:
#   scripts/wardriving-position-coverage.sh [BASE_URL] [WINDOW]
#
#   BASE_URL  server to query, default http://localhost:8080
#   WINDOW    1h | 24h | 7d, default 7d (widest observer coverage)
#
# Requires: curl, jq.
set -eu

base_url="${1:-http://localhost:8080}"
window="${2:-7d}"

body=$(curl -sf "${base_url%/}/api/analytics/wardriving?window=${window}")

echo "$body" | jq -r --arg window "$window" '
  {
    window: $window,
    totalObservers: (.observers | length),
    bySource: (.observers | group_by(.positionSource) | map({(.[0].positionSource // "none"): length}) | add // {}),
  }
  | "window=\(.window) totalObservers=\(.totalObservers) real=\((.bySource.real // 0)) iata=\((.bySource.iata // 0)) none=\((.bySource.none // 0))"
'
