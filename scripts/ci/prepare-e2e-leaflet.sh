#!/usr/bin/env bash
set -euo pipefail
: "${RUNNER_TEMP:?RUNNER_TEMP is required}"
leaflet_dir="$RUNNER_TEMP/corescope-315-leaflet"
mkdir -p "$leaflet_dir/images"
curl --fail --silent --show-error --location --retry 3 https://unpkg.com/leaflet@1.9.4/dist/leaflet.js -o "$leaflet_dir/leaflet.js"
curl --fail --silent --show-error --location --retry 3 https://unpkg.com/leaflet@1.9.4/dist/leaflet.css -o "$leaflet_dir/leaflet.css"
curl --fail --silent --show-error --location --retry 3 https://unpkg.com/leaflet@1.9.4/dist/images/marker-icon.png -o "$leaflet_dir/images/marker-icon.png"
curl --fail --silent --show-error --location --retry 3 https://unpkg.com/leaflet@1.9.4/dist/images/marker-icon-2x.png -o "$leaflet_dir/images/marker-icon-2x.png"
curl --fail --silent --show-error --location --retry 3 https://unpkg.com/leaflet@1.9.4/dist/images/marker-shadow.png -o "$leaflet_dir/images/marker-shadow.png"
echo "db49d009c841f5ca34a888c96511ae936fd9f5533e90d8b2c4d57596f4e5641a  $leaflet_dir/leaflet.js" | sha256sum --check
echo "a7837102824184820dfa198d1ebcd109ff6d0ff9a2672a074b9a1b4d147d04c6  $leaflet_dir/leaflet.css" | sha256sum --check
echo "574c3a5cca85f4114085b6841596d62f00d7c892c7b03f28cbfa301deb1dc437  $leaflet_dir/images/marker-icon.png" | sha256sum --check
echo "00179c4c1ee830d3a108412ae0d294f55776cfeb085c60129a39aa6fc4ae2528  $leaflet_dir/images/marker-icon-2x.png" | sha256sum --check
echo "264f5c640339f042dd729062cfc04c17f8ea0f29882b538e3848ed8f10edb4da  $leaflet_dir/images/marker-shadow.png" | sha256sum --check
