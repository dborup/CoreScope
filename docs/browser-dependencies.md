# Browser dependencies: pinned versions and integrity hashes

CoreScope's web UI loads a handful of third-party libraries from a public CDN.
Every one of them is pinned to an exact version and verified in the browser with
[Subresource Integrity](https://developer.mozilla.org/en-US/docs/Web/Security/Subresource_integrity)
(SRI). Two tests enforce that, and both fail closed on anything new:

- `test-issue-336-external-asset-pinning.js` scans every `*.html` under
  `public/` and `tools/`, and every non-test `*.go` under `cmd/` and
  `internal/` (today that finds `cmd/server/openapi.go`, but a new handler that
  prints HTML is covered from its first commit). It rejects any off-origin
  `<script src>` or script/stylesheet `<link href>` that lacks an exact
  `major.minor.patch` version, a well-formed `integrity`, or
  `crossorigin="anonymous"`, and requires every digest to equal the reviewed
  value in its `REVIEWED_DIGESTS` table.
- `cmd/server/openapi_sri_test.go` pins the `/api/docs` Swagger UI page on its
  own, including the reviewed digest values (`wantSwaggerUIAssets`).

A URL added for a CDN host that has no pinning rule in the scanner fails rather
than passing unchecked; add the rule together with the dependency.

### What the scanner checks, and what it cannot see

- **`<link>` is checked by `rel`.** Only a link that loads a script or a
  stylesheet can carry `integrity`: `rel="stylesheet"`, `rel="modulepreload"`,
  and `rel="preload"` with `as="script"` or `as="style"`. An off-origin
  `rel="preconnect"`, `rel="dns-prefetch"`, `rel="canonical"`, `rel="icon"`
  and similar hints fetch nothing executable and are ignored.
- **Scripts injected from code are not allowed.** The tag scanner reads markup,
  so a script added at runtime with `document.createElement('script')` (or a
  stylesheet with `createElement('link')`, a worker's `importScripts(...)`, or
  an off-origin `import(...)`) would load without any of the checks above. The
  same test therefore fails on any of those patterns in `public/` and `tools/`
  (`*.js` and `*.html`) and in the Go sources. None exists today. Load a new
  dependency with a static, pinned tag instead; if runtime injection is ever
  genuinely needed, extend the test to verify the injected URL and its
  integrity in the same change.
- **Wrong but well-formed digests are caught by value.** A sha384 that is
  syntactically valid but computed over the wrong bytes would make the browser
  block the file. The format rule cannot see that, so every URL's digest must
  also match `REVIEWED_DIGESTS` — a second, deliberate place to edit.

## Inventory

| Dependency | URL | Loaded by |
| --- | --- | --- |
| Leaflet 1.9.4 (CSS) | `https://unpkg.com/leaflet@1.9.4/dist/leaflet.css` | `public/index.html`, `public/area-map.html`, `public/geofilter-builder.html`, `tools/area-map.html`, `tools/geofilter-builder.html` |
| Leaflet 1.9.4 (JS) | `https://unpkg.com/leaflet@1.9.4/dist/leaflet.js` | same five pages |
| Leaflet.heat 0.2.0 | `https://unpkg.com/leaflet.heat@0.2.0/dist/leaflet-heat.js` | `public/index.html` (live map heatmap) |
| Chart.js 4.5.1 | `https://unpkg.com/chart.js@4.5.1/dist/chart.umd.min.js` | `public/index.html` (analytics charts) |
| swagger-ui-dist 5.33.2 (CSS) | `https://unpkg.com/swagger-ui-dist@5.33.2/swagger-ui.css` | `cmd/server/openapi.go` → `/api/docs` |
| swagger-ui-dist 5.33.2 (JS) | `https://unpkg.com/swagger-ui-dist@5.33.2/swagger-ui-bundle.js` | `cmd/server/openapi.go` → `/api/docs` |

Leaflet Marker Cluster is **not** in this table: it is already served from
`public/vendor/`, same-origin, with a cache-busting query instead of a hash.

Map tile providers are out of scope. Tiles are configured endpoints that Leaflet
requests from JavaScript, not `<script>`/`<link>` subresources, so they cannot
carry SRI and must not be changed as a side effect of dependency pinning
(see #332).

## Updating a pinned dependency

`unpkg.com/<pkg>@<exact version>/<file>` is immutable, so a digest only has to
change when the pin moves. To move it:

1. Download the exact bytes of the new version and compute the digest:

   ```sh
   curl -sL https://unpkg.com/chart.js@4.5.1/dist/chart.umd.min.js \
     | openssl dgst -sha384 -binary | openssl base64 -A
   ```

   Do not send `Accept-Encoding`; SRI is computed over the decoded body, which
   is what plain `curl` gives you. `sha384` is the default here — `sha256` and
   `sha512` are also valid SRI algorithms and the scanner accepts them.

2. Update the URL **and** the `integrity` value in the same edit, in every file
   from the inventory above. A version bump with a stale digest means the
   browser refuses to execute the file.

3. Update the reviewed digest in `REVIEWED_DIGESTS` in
   `test-issue-336-external-asset-pinning.js` (the test fails on a stale
   entry as well as on a mismatch). For the Swagger UI assets, also update
   `swaggerUIDistVersion` and `wantSwaggerUIAssets` in
   `cmd/server/openapi_sri_test.go`. Finally update this file's inventory
   table (the Node test asserts every live URL is listed here).

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

All six artefacts are permissively licensed and are redistributed by the CDN,
not by this repository: Leaflet and Leaflet.heat are BSD-2-Clause, Chart.js is
MIT, swagger-ui-dist is Apache-2.0. Pinning does not change the licence
position. Vendoring them into `public/vendor/` would, because the repository
would then redistribute the bytes and has to carry the licence texts — see
[Local hosting](#local-hosting-evaluated-not-done) below.

## Failure mode when the CDN is unreachable

The pages degrade rather than break:

- `public/index.html` renders and navigates normally. The live map falls back to
  a no-map state and analytics charts do not draw, because `L` / `Chart` are
  undefined; the JSON API is unaffected.
- `/api/docs` renders an empty, unstyled page. `/api/spec` still serves the
  OpenAPI document, which is the machine-readable path an API-only install
  needs.

A *tampered* response fails the same way as an unreachable one, which is the
point: with `integrity` present the browser discards the file instead of
executing it, and reports the mismatch in the console.

## Local hosting: evaluated, not done

Nothing above is vendored. Serving these files from `public/vendor/` is a real
option but a separate decision; this is the trade-off recorded when the pins
were introduced (#336), kept here so it outlives the pull request.

**For:** offline and air-gapped installs get maps, charts and API docs instead
of the degraded pages described above; the browser makes no third-party
request, so the CDN no longer sees each operator's IP, User-Agent and referrer
(SRI fixes integrity, not that observation); a CDN, DNS or proxy outage stops
mattering; one origin on a cold cache.

**Against:** maintenance moves in-house — about 2 MB of minified third-party
bytes per bump (`swagger-ui-bundle.js` alone is 1.5 MB) that a diff cannot
meaningfully review, so the control stays "we trust the registry", relocated;
the repository becomes a redistributor and must carry the licence texts and,
for swagger-ui-dist (Apache-2.0), the NOTICE; the server carries the bandwidth.

**If it is taken on:** a `tools/` fetch-and-verify script that downloads each
exact version, checks it against the digest already recorded here, writes it
to `public/vendor/`, plus a test that the committed bytes still match — so the
current pins stay the source of truth. Swagger UI is the best first candidate
(largest offline gap, single consumer); Leaflet only partly benefits, because
the map still needs its tile provider.
