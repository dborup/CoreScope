# Browser dependencies: pinned versions and integrity hashes

CoreScope's web UI loads a handful of third-party libraries from a public CDN.
Every one of them is pinned to an exact version and verified in the browser with
[Subresource Integrity](https://developer.mozilla.org/en-US/docs/Web/Security/Subresource_integrity)
(SRI). Two tests enforce that, and both fail closed on anything new:

- `test-issue-336-external-asset-pinning.js` scans every `*.html` under
  `public/` and `tools/`, plus `cmd/server/openapi.go`, and rejects any
  off-origin `<script src>`/`<link href>` that lacks an exact
  `major.minor.patch` version, a well-formed `integrity`, or
  `crossorigin="anonymous"`.
- `cmd/server/openapi_sri_test.go` pins the `/api/docs` Swagger UI page on its
  own, including the reviewed digest values.

A URL added for a CDN host that has no pinning rule in the scanner fails rather
than passing unchecked; add the rule together with the dependency.

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

3. For the Swagger UI assets, also update `swaggerUIDistVersion` and
   `wantSwaggerUIAssets` in `cmd/server/openapi_sri_test.go`, and this file's
   inventory table (the Node test asserts every live URL is listed here).

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
would then redistribute the bytes and has to carry the licence texts — see the
local-hosting evaluation in the pull request for #336.

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
