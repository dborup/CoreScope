#!/usr/bin/env bash
# Run every sites/meshview-info/test-*.cjs and test-*.js. Fails when any test
# fails, or when no test is found (a moved folder must not pass silently).
set -uo pipefail
cd "$(dirname "$0")/../sites/meshview-info" || { echo "sites/meshview-info not found" >&2; exit 1; }
shopt -s nullglob
tests=(test-*.cjs test-*.js)
if [ ${#tests[@]} -eq 0 ]; then echo "no info site tests found" >&2; exit 1; fi
failed=()
for t in "${tests[@]}"; do
  echo "::group::$t"
  if node "$t"; then echo "ok: $t"; else echo "FAIL: $t"; failed+=("$t"); fi
  echo "::endgroup::"
done
echo "${#tests[@]} info site tests, ${#failed[@]} failed"
if [ ${#failed[@]} -gt 0 ]; then printf 'failed: %s\n' "${failed[@]}" >&2; exit 1; fi
