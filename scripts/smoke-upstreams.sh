#!/bin/sh
# scripts/smoke-upstreams.sh: the manual live smoke check of every external
# client's request shape (#803). Thin wrapper around cmd/smoke.
#
# NOT run in CI or by any test: an operator runs it by hand before a release
# candidate. It sends each upstream client's one allow-listed request once,
# read-only, to the real service, and prints status, content type, body size
# and a shape check per upstream. Any FAIL exits 1; a config error exits 2.
#
#   scripts/smoke-upstreams.sh                          # tokenless upstreams only
#   scripts/smoke-upstreams.sh -config /path/config.json  # with map.dhm.token
#
# The token is read from the config's map.dhm.token, sent only in the token
# header of the requests that need it and never printed. Without it those
# probes are SKIP. All arguments go to cmd/smoke (-h lists them).
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

# Built, then run from the caller's directory, so a relative -config path
# means what the caller typed.
(cd "$root/cmd/smoke" && go build -o "$tmp/smoke" .)
"$tmp/smoke" "$@"
