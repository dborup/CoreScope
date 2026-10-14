# Browser dependencies: local vendoring, pinned versions and integrity hashes

CoreScope's web UI used to load a handful of third-party libraries from a
public CDN (`unpkg.com`). As of #442 every one of them is vendored into
`public/vendor/<name>/<version>/`, served same-origin, pinned to an exact
version, and still verified in the browser with
[Subresource Integrity](https://developer.mozilla.org/en-US/docs/Web/Security/Subresource_integrity)
(SRI) — the browser makes no third-party request for any of them. Three tests
enforce that, and all fail closed on anything new:

- `test-issue-336-external-asset-pinning.js` scans every `*.html` under
  `public/` and `tools/`, and every non-test `*.go` under `cmd/` and
  `internal/` (today that finds `cmd/server/openapi.go`, but a new handler that
  prints HTML is covered from its first commit). It fails on any off-origin
  `<script src>` or script/stylesheet `<link href>` at all (the CDN must stay
  gone), and separately requires every locally vendored
  `vendor/<name>/<version>/<file>` reference to carry an exact version, a
  well-formed `integrity`, and `crossorigin="anonymous"`, matching the digest
  recorded in `tools/browser-deps-manifest.json`. It also hashes the committed
  bytes under `public/vendor/` and compares them against the same manifest, so
  a tampered or stale vendored file is caught independently of what any tag
  claims. It additionally parses every vendored `*.css` file for `url(...)`
  references (e.g. `leaflet.css`'s layers-control and default-marker icons,
  see the inventory below) and fails if one points at a file that was not
  vendored alongside it — a CSS asset can reference images that no
  `<script>`/`<link>` tag names, so the tag scanner alone cannot see them.
- `cmd/server/openapi_sri_test.go` pins the `/api/docs` Swagger UI page on its
  own, including the reviewed digest values (`wantSwaggerUIAssets`), and hashes
  the committed `public/vendor/swagger-ui-dist/` bytes the same way.
- `tools/fetch-browser-deps.sh --check` (invoked by the Node test, and runnable
  standalone) verifies every committed file against the manifest with no
  network access at all.

A URL added for a CDN host that has no pinning rule in the scanner fails rather
than passing unchecked; add the rule together with the dependency.

### What the scanner checks, and what it cannot see

- **No off-origin subresource is permitted at all.** Before #442 the scanner
  pinned and hashed CDN tags; now it simply rejects any `<script src>` or
  `<link href>` that leaves the origin. A dependency belongs in
  `public/vendor/`, not on a CDN.
- **`<link>` is checked by `rel`.** Only a link that loads a script or a
  stylesheet can carry `integrity`: `rel="stylesheet"`, `rel="modulepreload"`,
  and `rel="preload"` with `as="script"` or `as="style"`. An off-origin
  `rel="preconnect"`, `rel="dns-prefetch"`, `rel="canonical"`, `rel="icon"`
  and similar hints fetch nothing executable and are ignored (there should be
  none left after #442, but the rule still exists as a safety net).
- **Scripts injected from code are not allowed.** The tag scanner reads markup,
  so a script added at runtime with `document.createElement('script')` (or a
  stylesheet with `createElement('link')`, a worker's `importScripts(...)`, or
  an off-origin `import(...)`) would load without any of the checks above. The
  same test therefore fails on any of those patterns in `public/` and `tools/`
  (`*.js` and `*.html`) and in the Go sources. None exists today. Load a new
  dependency with a static, pinned, vendored tag instead; if runtime injection
  is ever genuinely needed, extend the test to verify the injected URL and its
  integrity in the same change.
- **Wrong but well-formed digests are caught by value.** A sha384 that is
  syntactically valid but computed over the wrong bytes would make the browser
  block the file. The tag-format rule cannot see that, so every vendored file
  is additionally hashed on disk and compared against
  `tools/browser-deps-manifest.json` — a second, deliberate place a digest has
  to change, now checked at the byte level rather than only by reading a tag.

## Inventory

Every pinned URL below is the exact upstream version vendored at
`public/vendor/<name>/<version>/`; it is listed for provenance (where the bytes
came from, and the URL to fetch when the pin moves), not because the browser
still requests it.

| Dependency | Vendored at | Pinned from | Loaded by |
| --- | --- | --- | --- |
| Leaflet 1.9.4 (CSS) | `public/vendor/leaflet/1.9.4/leaflet.css` | `https://unpkg.com/leaflet@1.9.4/dist/leaflet.css` | `public/index.html`, `public/area-map.html`, `public/geofilter-builder.html`, `tools/area-map.html`, `tools/geofilter-builder.html` |
| Leaflet 1.9.4 (JS) | `public/vendor/leaflet/1.9.4/leaflet.js` | `https://unpkg.com/leaflet@1.9.4/dist/leaflet.js` | same five pages |
| Leaflet 1.9.4 (layers-control icon) | `public/vendor/leaflet/1.9.4/images/layers.png` | `https://unpkg.com/leaflet@1.9.4/dist/images/layers.png` | `leaflet.css` (`url(images/layers.png)`) |
| Leaflet 1.9.4 (layers-control icon, 2x) | `public/vendor/leaflet/1.9.4/images/layers-2x.png` | `https://unpkg.com/leaflet@1.9.4/dist/images/layers-2x.png` | `leaflet.css` (`url(images/layers-2x.png)`) |
| Leaflet 1.9.4 (default marker icon) | `public/vendor/leaflet/1.9.4/images/marker-icon.png` | `https://unpkg.com/leaflet@1.9.4/dist/images/marker-icon.png` | `leaflet.css` (`.leaflet-default-icon-path`, used by `L.Icon.Default` to auto-detect its image path) |
| Leaflet 1.9.4 (default marker icon, 2x) | `public/vendor/leaflet/1.9.4/images/marker-icon-2x.png` | `https://unpkg.com/leaflet@1.9.4/dist/images/marker-icon-2x.png` | `L.Icon.Default`, relative to the path above |
| Leaflet 1.9.4 (default marker shadow) | `public/vendor/leaflet/1.9.4/images/marker-shadow.png` | `https://unpkg.com/leaflet@1.9.4/dist/images/marker-shadow.png` | `L.Icon.Default`, relative to the path above |
| Leaflet.heat 0.2.0 | `public/vendor/leaflet.heat/0.2.0/leaflet-heat.js` | `https://unpkg.com/leaflet.heat@0.2.0/dist/leaflet-heat.js` | `public/index.html` (live map heatmap) |
| Chart.js 4.5.1 | `public/vendor/chart.js/4.5.1/chart.umd.min.js` | `https://unpkg.com/chart.js@4.5.1/dist/chart.umd.min.js` | `public/index.html` (analytics charts) |
| swagger-ui-dist 5.33.2 (CSS) | `public/vendor/swagger-ui-dist/5.33.2/swagger-ui.css` | `https://unpkg.com/swagger-ui-dist@5.33.2/swagger-ui.css` | `cmd/server/openapi.go` → `/api/docs` |
| swagger-ui-dist 5.33.2 (JS) | `public/vendor/swagger-ui-dist/5.33.2/swagger-ui-bundle.js` | `https://unpkg.com/swagger-ui-dist@5.33.2/swagger-ui-bundle.js` | `cmd/server/openapi.go` → `/api/docs` |

The upstream URLs and digests above live in one place,
`tools/browser-deps-manifest.json`, which also lists each dependency's LICENSE
(and, for swagger-ui-dist, NOTICE) file — see [Licences](#licences).

`public/vendor/MarkerCluster.*`, `aes-ecb.js`, `sha256-hmac.js`, `qrcode.js` and
`jsqr.min.js` are **not** in this table: they were already served from
`public/vendor/`, same-origin, before #442, with a cache-busting query
(`?v=__BUST__`) instead of a version segment and a hash. They are unaffected.

Map tile providers are out of scope. Tiles are configured endpoints that Leaflet
requests from JavaScript, not `<script>`/`<link>` subresources, so they cannot
carry SRI and must not be changed as a side effect of dependency vendoring
(see #332).

## Fetching and verifying a dependency

```sh
sh tools/fetch-browser-deps.sh          # download, verify sha384, write public/vendor/
sh tools/fetch-browser-deps.sh --check  # verify the already-committed files, no network
```

The script reads `tools/browser-deps-manifest.json` — one entry per dependency,
each with its upstream URL, destination filename and reviewed sha384 — and for
every file: downloads it, hashes it, and **only if the hash matches** writes it
to `public/vendor/<name>/<version>/<dest>`. A mismatch aborts before anything is
written; nothing unverified ever lands in the tree, and a `--check` run (no
network) catches a byte that was corrupted or hand-edited after the fact.

## Updating a pinned dependency

`unpkg.com/<pkg>@<exact version>/<file>` is immutable, so a digest only has to
change when the pin moves. To move it:

1. Edit the version and recompute the digest for each changed file in
   `tools/browser-deps-manifest.json`:

   ```sh
   curl -sL https://unpkg.com/chart.js@4.5.1/dist/chart.umd.min.js \
     | openssl dgst -sha384 -binary | openssl base64 -A
   ```

   Do not send `Accept-Encoding`; SRI is computed over the decoded body, which
   is what plain `curl` gives you. `sha384` is the default here — `sha256` and
   `sha512` are also valid SRI algorithms and the scanner accepts them.

2. Run `sh tools/fetch-browser-deps.sh` to download and write the new bytes
   under `public/vendor/<name>/<new version>/`, and `git rm -r` the old
   version's directory once nothing references it.

3. Update every `vendor/<name>/<old version>/...` reference to the new version
   **and** the `integrity` value in the same edit, in every file from the
   inventory above. A version bump with a stale digest means the browser
   refuses to execute the file. For the Swagger UI assets, also update
   `swaggerUIDistVersion` and `wantSwaggerUIAssets` in
   `cmd/server/openapi_sri_test.go`. Finally update this file's inventory
   table (the Node test asserts every manifest URL is listed here).

4. Run `node test-issue-336-external-asset-pinning.js` and
   `go test ./cmd/server/ -run TestSwaggerUI`, then load the affected page in a
   browser and confirm the console shows no SRI or CORS error. A failed
   integrity check is silent in the UI but loud in the console.

### Resolving a floating specifier

To find what a range such as `@4` resolves to right now:

```sh
curl -sI https://unpkg.com/chart.js@4/dist/chart.umd.min.js | grep -i '^location'
```

Pinning to that value is not an upgrade — it freezes the version the browser
would have fetched anyway. Moving to a *newer* version is a separate,
behaviour-affecting change and belongs in its own review.

## Licences

All six artefacts are permissively licensed, and vendoring them means this
repository now redistributes the bytes rather than the CDN: Leaflet and
Leaflet.heat are **BSD-2-Clause**, Chart.js is **MIT**, swagger-ui-dist is
**Apache-2.0**. Each dependency's LICENSE file (recorded in
`tools/browser-deps-manifest.json` and fetched/verified the same way as the
runtime bytes) is committed next to it at
`public/vendor/<name>/<version>/LICENSE`. swagger-ui-dist additionally carries
an Apache **NOTICE** file at `public/vendor/swagger-ui-dist/5.33.2/NOTICE`,
required by the Apache-2.0 NOTICE-preservation clause.

## Failure mode when a vendored file is missing or the server can't be reached

The pages degrade rather than break — this did not change when the files moved
from the CDN to `public/vendor/`:

- `public/index.html` renders and navigates normally. If a vendored script
  failed to load, the live map falls back to a no-map state and analytics
  charts do not draw, because `L` / `Chart` are undefined; the JSON API is
  unaffected.
- `/api/docs` renders an empty, unstyled page if its vendored assets 404.
  `/api/spec` still serves the OpenAPI document, which is the machine-readable
  path an API-only install needs.

A *tampered* vendored file fails the same way: with `integrity` present the
browser discards it instead of executing it, and reports the mismatch in the
console. Unlike the CDN era, nothing above now depends on a third party being
reachable at all — this is what #442 set out to fix.

## Local hosting (#442)

Implemented. Previously (#336/#400) these six artefacts were pinned and
hashed but still loaded from `unpkg.com`; this is the trade-off that was
recorded at the time and has now been acted on.

**Why:** offline and air-gapped installs get maps, charts and API docs instead
of the degraded pages above; the browser makes no third-party request, so the
CDN no longer sees each operator's IP, User-Agent and referrer on every page
load (a privacy and, for some deployments, GDPR-relevant property — SRI alone
only fixed integrity, not that observation); a CDN, DNS or proxy outage no
longer matters; one origin on a cold cache.

**Cost accepted:** about 2 MB of minified third-party bytes
(`swagger-ui-bundle.js` alone is 1.5 MB) now live in the repository and a diff
cannot meaningfully review them — the control is "the manifest's recorded
digest matches what `tools/fetch-browser-deps.sh` downloaded today", not a
line-by-line review; the repository is now a redistributor and carries the
licence texts and the swagger-ui-dist NOTICE (see [Licences](#licences)); the
server carries the bandwidth these files cost instead of a CDN edge.

**Shape:** `tools/browser-deps-manifest.json` is the single source of truth
(URL, destination, sha384, licence id) for `tools/fetch-browser-deps.sh`, for
`test-issue-336-external-asset-pinning.js`, and for
`cmd/server/openapi_sri_test.go`. A version bump touches the manifest, re-runs
the fetch script, and updates the tag references and the Go test's own pins —
see [Updating a pinned dependency](#updating-a-pinned-dependency). Map tile
providers remain untouched and out of scope (#332); Leaflet's map still needs
its configured tile provider regardless of where `leaflet.js` itself is
served from.

### Cache headers for vendored assets

`public/vendor/<name>/<version>/<file>` is immutable by construction: the
version is part of the URL, so a bumped dependency is served from a new path
rather than a mutated file at the old one. The static file server
(`cmd/server/main.go`'s `spaHandler`) recognises that shape and sends
`Cache-Control: public, max-age=31536000, immutable` instead of the
`no-cache, no-store, must-revalidate` it uses for ordinary `.js`/`.css`/`.html`
assets. The older, non-versioned files under `public/vendor/` (`jsqr.min.js`,
`aes-ecb.js`, …) keep the existing no-cache behaviour and their
`?v=__BUST__` cache-busting is unaffected — only the pattern
`/vendor/<name>/<exact version>/...` gets the long-lived header.
