#!/bin/sh
# fetch-browser-deps.sh — download the browser dependencies pinned in
# tools/browser-deps-manifest.json, verify each file's sha384 against the
# manifest, and only then write it into public/vendor/<name>/<version>/<dest>.
# A digest mismatch aborts before anything is written or overwritten, so a
# tampered or wrong-version response can never land in the tree.
#
# Usage:
#   sh tools/fetch-browser-deps.sh          # download, verify, write
#   sh tools/fetch-browser-deps.sh --check  # verify committed files only, no network
#
# Relates to #442. See docs/browser-dependencies.md for the update procedure.
set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
MANIFEST="$ROOT/tools/browser-deps-manifest.json"
VENDOR="$ROOT/public/vendor"
CHECK_ONLY=0

for arg in "$@"; do
  case "$arg" in
    --check) CHECK_ONLY=1 ;;
    *) echo "fetch-browser-deps.sh: unknown argument: $arg" >&2; exit 2 ;;
  esac
done

command -v jq >/dev/null 2>&1 || { echo "fetch-browser-deps.sh: jq is required" >&2; exit 2; }
command -v openssl >/dev/null 2>&1 || { echo "fetch-browser-deps.sh: openssl is required" >&2; exit 2; }

sha384_of() {
  openssl dgst -sha384 -binary "$1" | openssl base64 -A
}

fail=0
count=$(jq '.dependencies | length' "$MANIFEST")
i=0
while [ "$i" -lt "$count" ]; do
  name=$(jq -r ".dependencies[$i].name" "$MANIFEST")
  version=$(jq -r ".dependencies[$i].version" "$MANIFEST")
  destdir="$VENDOR/$name/$version"
  fcount=$(jq ".dependencies[$i].files | length" "$MANIFEST")
  j=0
  while [ "$j" -lt "$fcount" ]; do
    url=$(jq -r ".dependencies[$i].files[$j].url" "$MANIFEST")
    dest=$(jq -r ".dependencies[$i].files[$j].dest" "$MANIFEST")
    want=$(jq -r ".dependencies[$i].files[$j].sha384" "$MANIFEST")
    out="$destdir/$dest"

    if [ "$CHECK_ONLY" = 1 ]; then
      if [ ! -f "$out" ]; then
        echo "MISSING  $name/$version/$dest (expected at ${out#"$ROOT"/})" >&2
        fail=1
        j=$((j + 1))
        continue
      fi
      got="sha384-$(sha384_of "$out")"
      if [ "$got" != "$want" ]; then
        echo "MISMATCH $name/$version/$dest" >&2
        echo "  want: $want" >&2
        echo "  got:  $got" >&2
        fail=1
      else
        echo "OK       $name/$version/$dest"
      fi
      j=$((j + 1))
      continue
    fi

    tmp=$(mktemp)
    # No explicit Accept-Encoding: SRI is computed over the decoded body,
    # which is what plain curl gives us (see docs/browser-dependencies.md).
    if ! curl -fsSL --max-time 30 "$url" -o "$tmp"; then
      echo "DOWNLOAD FAILED $name/$version/$dest <- $url" >&2
      rm -f "$tmp"
      fail=1
      j=$((j + 1))
      continue
    fi
    got="sha384-$(sha384_of "$tmp")"
    if [ "$got" != "$want" ]; then
      echo "MISMATCH $name/$version/$dest <- $url" >&2
      echo "  want: $want" >&2
      echo "  got:  $got" >&2
      echo "  refusing to write unverified bytes" >&2
      rm -f "$tmp"
      fail=1
      j=$((j + 1))
      continue
    fi

    mkdir -p "$destdir"
    mv "$tmp" "$out"
    chmod 644 "$out"
    echo "OK       $name/$version/$dest <- $url"
    j=$((j + 1))
  done
  i=$((i + 1))
done

if [ "$fail" != 0 ]; then
  echo "fetch-browser-deps.sh: one or more files failed verification" >&2
  exit 1
fi
echo "fetch-browser-deps.sh: all files verified"
