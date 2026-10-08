# CoreScope — API Contract Specification

> **Authoritative contract.** Both the Node.js and Go backends MUST conform to this spec.
> The frontend relies on these exact shapes. Breaking changes require a spec update first.

**Version:** 1.1.0
**Last updated:** 2026-04-22

---

## Table of Contents

- [Conventions](#conventions)
- [Estimated-position policy](#estimated-position-policy)
- [GET /api/stats](#get-apistats)
- [GET /api/health](#get-apihealth)
- [GET /api/perf](#get-apiperf)
- [POST /api/perf/reset](#post-apiperfreset)
- [GET /api/nodes](#get-apinodes)
- [GET /api/nodes/search](#get-apinodessearch)
- [GET /api/nodes/bulk-health](#get-apinodesbulk-health)
- [GET /api/nodes/network-status](#get-apinodesnetwork-status)
- [GET /api/nodes/:pubkey](#get-apinodespubkey)
- [GET /api/nodes/:pubkey/health](#get-apinodespubkeyhealth)
- [GET /api/nodes/:pubkey/paths](#get-apinodespubkeypaths)
- [GET /api/nodes/:pubkey/analytics](#get-apinodespubkeyanalytics)
- [GET /api/nodes/:pubkey/reach](#get-apinodespubkeyreach)
- [GET /api/reach-rank](#get-apireach-rank)
- [GET /api/packets](#get-apipackets)
- [GET /api/packets/timestamps](#get-apipacketstimestamps)
- [GET /api/packets/:id](#get-apipacketsid)
- [POST /api/packets/observations](#post-apipacketsobservations)
- [POST /api/decode](#post-apidecode)
- [GET /api/observers](#get-apiobservers)
- [GET /api/observers/:id](#get-apiobserversid)
- [GET /api/observers/:id/analytics](#get-apiobserversidanalytics)
- [GET /api/channels](#get-apichannels)
- [GET /api/channels/:hash/messages](#get-apichannelshashmessages)
- [GET /api/channel-proposals/config](#get-apichannel-proposalsconfig)
- [POST /api/channel-proposals](#post-apichannel-proposals)
- [GET /api/channel-proposals/requests/:requestId](#get-apichannel-proposalsrequestsrequestid)
- [GET /api/admin/channel-proposals](#get-apiadminchannel-proposals)
- [POST /api/admin/channel-proposals/:id/approve and /reject](#post-apiadminchannel-proposalsidapprove-and-reject)
- [POST /api/admin/channel-proposals/:id/revoke](#post-apiadminchannel-proposalsidrevoke)
- [GET /api/analytics/rf](#get-apianalyticsrf)
- [GET /api/analytics/topology](#get-apianalyticstopology)
- [GET /api/analytics/channels](#get-apianalyticschannels)
- [GET /api/analytics/distance](#get-apianalyticsdistance)
- [GET /api/analytics/hash-sizes](#get-apianalyticshash-sizes)
- [GET /api/analytics/subpaths](#get-apianalyticssubpaths)
- [GET /api/analytics/subpath-detail](#get-apianalyticssubpath-detail)
- [GET /api/scope-stats](#get-apiscope-stats)
- [GET /api/resolve-hops](#get-apiresolve-hops)
- [GET /api/traces/:hash](#get-apitraceshash)
- [GET /api/config/theme](#get-apiconfigtheme)
- [GET /api/config/regions](#get-apiconfigregions)
- [GET /api/config/areas](#get-apiconfigareas)
- [GET /api/config/areas/polygons](#get-apiconfigareaspolygons)
- [GET /api/config/client](#get-apiconfigclient)
- [GET /api/config/cache](#get-apiconfigcache)
- [GET /api/config/map](#get-apiconfigmap)
- [GET /api/iata-coords](#get-apiiata-coords)
- [GET /api/nodes/clock-skew](#get-apinodesclock-skew)
- [GET /api/analytics/hash-collisions](#get-apianalyticshash-collisions)
- [GET /api/audio-lab/buckets](#get-apiaudio-labbuckets)
- [WebSocket Messages](#websocket-messages)
- [Area Filter](#area-filter)

---

## Estimated-position policy

`config.json` accepts `estimatedPositions: { "enabled": false }`. Missing
section or field defaults to `true`. This is a server-startup policy, not a
request parameter or a browser preference. Restart the server to change it.
`GET /api/config/client` always publishes the effective value as
`estimatedPositions: { "enabled": <boolean> }`.

When disabled:

- Node detail omits `estimated_lat`, `estimated_lon`,
  `estimated_contributor_count`, and `estimated_distance_km`. Reported
  `lat`/`lon` are unchanged.
- Packet paths and saved Ping Scores path responses retain route identities
  and reported coordinates but omit neighbor-derived coordinates and their
  approximation metadata. Endpoint distances depending on removed estimates
  are omitted; distances between reported endpoints remain valid. Saved
  source archives are not modified -- neither by a request nor by the
  background Ping Scores history refresh -- so re-enabling the policy serves
  the original archived geometry again.
- `/api/analytics/areas` returns `estimatedPositionsEnabled: false` alongside
  `density`, `bridgeNodes`, and `unpositionedTotal`. It omits uncomputed
  `positionGaps`, `estimatedNodes`, and `unpositionedNoNeighborFix` rather than
  claiming zero gaps or no neighbor evidence.
- `/api/analytics/gps-sanity` returns only
  `{ "estimatedPositionsEnabled": false }`, without running the estimator.

Enabled analytics retain their existing response shapes. API clients must
distinguish disabled computation from an enabled, empty result. The policy
does not disable ordinary neighbor graphs or independent IATA/name-based
position fallbacks. No query parameter can override the server setting.

## GET /api/ping-scores/:hash/path

Returns path evidence for a currently displayed Ping Scores record. Requires
`record=allTime.<kind>` or `record=thisWeek.<kind>`, where `<kind>` is one of
`farthestPing`, `mostHopsPing`, `widestSpreadPing`, `fastestSpreadPing`, or
`mostEfficientPing`. The hash must match that slot's current record.

```json
{
  "status": "archived",
  "capturedAt": "2026-01-15T10:05:00Z",
  "path": {
    "hash": "example",
    "branches": [{ "hops": 0, "points": [], "observer": { "name": "Example", "lat": 56.0, "lon": 10.0 } }]
  }
}
```

`status` is `archived` (saved route geometry), `live` (currently available
observations), `initializing` (board not published yet), or `unavailable`.
`capturedAt` is the archive capture time, not a radio transmission timestamp.
Unavailable responses omit `path` and include a `reason`: expired raw data
before capture, no coordinates, privacy filtering, or archive size limits
(`raw_data_expired_before_capture`, `no_coordinates`, `privacy_filtered`,
`archive_too_large`). `record_evidence_unavailable` means current observations
no longer reproduce the saved record and no matching archive exists; a
smaller live map is not substituted. Invalid slots return 400; superseded slot/hash pairs
return 404. A visibility lookup failure returns 500 rather than exposing data.

The existing ping history sidecar stores at most ten record-slot paths, each
limited to 256 KiB, 128 branches and 4,096 points (including `first`). Paths
are saved atomically with record changes and restored after restart. Old
records whose observations were already pruned cannot be reconstructed.
Superseded records are not a permanent path archive. Packet retention and the
main database schema are unchanged.

The sidecar's additive v3 migration also adds a nullable distance-origin
pubkey, without scanning existing rows. This keeps historical distance
evidence separate from current first-hearer leaderboard credit. Missing
GPS, including fallback to an IATA airport, cannot lower an established
distance; genuine corrections with both historical endpoints positioned are
still allowed.

Archived and live paths both apply the current node/observer blacklists and
hidden-name prefixes, including saved names and current/inactive names.
Hidden branches are omitted entirely; hidden first observations and their
relative measurements are removed. Approximate neighbor-centroid positions
are omitted when a visibility policy is active because the path format does
not identify their contributors. Touched areas are recomputed from visible
positions and current area configuration. Existing general packet-path
endpoints are unchanged.

## Conventions

### Types

| Notation        | Meaning                                              |
|-----------------|------------------------------------------------------|
| `string`        | JSON string                                          |
| `number`        | JSON number (integer or float)                       |
| `boolean`       | `true` / `false`                                     |
| `string (ISO)`  | ISO 8601 timestamp, e.g. `"2025-07-17T04:23:01.000Z"` |
| `string (hex)`  | Hex-encoded bytes, uppercase, e.g. `"4F01A3..."`     |
| `number \| null`| May be `null` when data is unavailable               |
| `[T]`           | JSON array of type `T`; always `[]` when empty, never `null` |
| `object`        | Nested JSON object (shape defined inline)            |

### Null Rules

- Fields marked `| null` may be absent or `null`.
- Array fields MUST be `[]` when empty, NEVER `null`.
- String fields that are "unknown" SHOULD be `null`, not `""`.

### Pagination

Paginated endpoints accept `limit` (default 50) and `offset` (default 0) as query params.
They return `total` (the unfiltered/filtered count before pagination).

### Error Responses

```json
{ "error": "string" }
```

- `400` — Bad request (missing/invalid params)
- `404` — Resource not found, or an unrecognized `/api` or `/api/*` path
- `405` — A known `/api/*` path called with an unsupported method; the response carries an `Allow` header listing the methods that path does support
- `413` — Request body over the byte cap on `POST /api/decode` or `POST /api/packets/observations` (other capped endpoints report an over-cap body as `400`; see below)

`HEAD` is accepted on every path that accepts `GET` and returns the same status and headers without a body.

#### Request-body byte caps

Every endpoint that takes a request body bounds it — with one exception, noted
below — and every bound is enforced by the application itself, not by a reverse
proxy in front of it. **Neither the enforcement nor the status code is
uniform**, so the table gives both per endpoint:

| Endpoint | Cap | Enforced on | Over the cap |
|---|---|---|---|
| `POST /api/decode` | 4096 bytes | bytes received, before parsing | `413` `{"error":"request body too large (max 4096 bytes)"}`, `application/json` |
| `POST /api/packets/observations` | 65536 bytes | bytes received, before parsing | `413` `{"error":"request body too large (max 65536 bytes)"}`, `application/json` |
| `POST /api/paths/inspect` | 4096 bytes | the streaming decoder | `400` `{"error":"invalid JSON"}`, served as `text/plain` |
| `POST /api/channel-proposals` | 1024 bytes | the streaming decoder | `400` `{"error":"invalid request body"}`, `application/json` |
| `PUT /api/config/geo-filter` (API key) | 1048576 bytes (1 MiB) | the streaming decoder | `400` `{"error":"invalid JSON"}`, `application/json` |

**Enforced on bytes received, before parsing** (the two rows issue #334
covers) is the strict reading: the verdict depends only on how many bytes
arrived. It does not depend on the body's shape — bulk sitting in an unknown
field, or in a string the endpoint never reads, is rejected the same way — and
it does not depend on `Content-Length`, so a chunked request that declares no
length, or one that understates it, is cut off at the same byte count. A body
of exactly the cap is accepted; one byte more is `413`, and the server stops
reading there instead of buffering the rest.

**Enforced on the streaming decoder** is weaker: the limit wraps the body and
the JSON decoder reads through it, so the limit only bites when the JSON
*value* runs past it. A body that is over the limit only because of data that
follows the value is still accepted — for example a 6019-byte
`POST /api/paths/inspect` body whose JSON object ends at byte 19 answers `200`
despite the 4096-byte limit. When the limit does bite, the decoder reports a
parse failure, which is why these rows answer `400` rather than `413`.

Three gaps are known and deliberately not changed by #334, because each is an
API-visible change with its own clients to check:

- the three streaming rows are not enforced on bytes received;
- they are not aligned on `413` + the shared error shape;
- `POST /api/admin/prune-geo-filter?confirm=true` (API key) decodes a JSON body
  with **no limit at all**.

Caps are deliberately well above anything a legitimate client sends (see each
endpoint below), so a per-endpoint semantic limit — such as the 200-hash limit
on `POST /api/packets/observations` — is what a real client meets first.

---

## GET /api/stats

Server-wide statistics. Lightweight, cached 10s.

### Response `200`

```jsonc
{
  "totalPackets":        number,       // observation count (legacy name)
  "totalTransmissions":  number | null, // unique transmission count
  "totalObservations":   number,       // total observation records
  "totalNodes":          number,       // active nodes (last 7 days)
  "totalNodesAllTime":   number,       // all nodes ever seen
  "totalObservers":      number,       // observer device count
  "packetsLastHour":     number,       // observations in last hour
  "engine":              "node",       // backend engine identifier
  "version":             string,       // package.json version, e.g. "2.6.0"
  "commit":              string,       // git short SHA or "unknown"
  "counts": {
    "repeaters":         number,       // active repeaters (last 7 days)
    "rooms":             number,
    "companions":        number,
    "sensors":           number
  }
}
```

---

## GET /api/health

Server health and telemetry. Used by monitoring.

### Response `200`

```jsonc
{
  "status":    "ok",
  "engine":    "node",
  "version":   string,
  "commit":    string,
  "uptime":    number,          // seconds
  "uptimeHuman": string,       // e.g. "4h 32m"
  "memory": {
    "rss":       number,       // MB
    "heapUsed":  number,       // MB
    "heapTotal": number,       // MB
    "external":  number        // MB
  },
  "eventLoop": {
    "currentLagMs": number,
    "maxLagMs":     number,
    "p50Ms":        number,
    "p95Ms":        number,
    "p99Ms":        number
  },
  "cache": {
    "entries":    number,
    "hits":       number,
    "misses":     number,
    "staleHits":  number,
    "recomputes": number,
    "hitRate":    number        // percentage (0–100)
  },
  "websocket": {
    "clients":   number        // connected WS clients
  },
  "packetStore": {
    "packets":      number,    // loaded transmissions
    "estimatedMB":  number
  },
  "perf": {
    "totalRequests": number,
    "avgMs":         number,
    "slowQueries":   number,
    "recentSlow": [            // last 5
      {
        "path":   string,
        "ms":     number,
        "time":   string,      // ISO timestamp
        "status": number       // HTTP status
      }
    ]
  }
}
```

---

## GET /api/perf

Detailed performance metrics per endpoint.

### Response `200`

```jsonc
{
  "uptime":        number,          // seconds since perf stats reset
  "totalRequests": number,
  "avgMs":         number,
  "endpoints": {
    "/api/packets": {               // keyed by route path
      "count":  number,
      "avgMs":  number,
      "p50Ms":  number,
      "p95Ms":  number,
      "maxMs":  number
    }
    // ... more endpoints
  },
  "slowQueries": [                  // last 20 queries > 100ms
    {
      "path":   string,
      "ms":     number,
      "time":   string,             // ISO timestamp
      "status": number
    }
  ],
  "cache": {
    "size":       number,
    "hits":       number,
    "misses":     number,
    "staleHits":  number,
    "recomputes": number,
    "hitRate":    number             // percentage (0–100)
  },
  "packetStore": {                  // from PacketStore.getStats()
    "totalLoaded":       number,
    "totalObservations": number,
    "evicted":           number,
    "inserts":           number,
    "queries":           number,
    "inMemory":          number,
    "sqliteOnly":        boolean,
    "maxPackets":        number,
    "estimatedMB":       number,
    "maxMB":             number,
    "indexes": {
      "byHash":            number,
      "byObserver":        number,
      "byNode":            number,
      "advertByObserver":  number
    }
  },
  "sqlite": {
    "dbSizeMB":    number,
    "walSizeMB":   number,
    "freelistMB":  number,
    "walPages":    { "total": number, "checkpointed": number, "busy": number } | null,
    "rows": {
      "transmissions": number,
      "observations":  number,
      "nodes":         number,
      "observers":     number
    }
  },
  "goRuntime": {                    // Go server only
    "heapMB":       number,         // heap allocation in MB
    "sysMB":        number,         // total system memory in MB
    "numGoroutine": number,         // active goroutines
    "numGC":        number,         // completed GC cycles
    "gcPauseMs":    number          // last GC pause in ms
  }
}
```

---

## POST /api/perf/reset

Resets performance counters. Requires API key.

### Headers

- `X-API-Key: <key>` (required if `config.apiKey` is set)

### Response `200`

```json
{ "ok": true }
```

---

## GET /api/nodes

Paginated node list with filtering.

### Query Parameters

| Param      | Type   | Default      | Description                                        |
|------------|--------|--------------|----------------------------------------------------|
| `limit`    | number | `50`         | Page size                                          |
| `offset`   | number | `0`          | Pagination offset                                  |
| `role`     | string | —            | Filter by role: `repeater`, `room`, `companion`, `sensor` |
| `region`   | string | —            | Comma-separated IATA codes for regional filtering  |
| `area`     | string | —            | Area key from `config.json` — filters to nodes whose GPS falls inside the area polygon (see [Area Filter](#area-filter)) |
| `lastHeard`| string | —            | Recency filter: `1h`, `6h`, `24h`, `7d`, `30d`    |
| `sortBy`   | string | `lastSeen`   | Sort key: `name`, `lastSeen`, `packetCount`        |
| `search`   | string | —            | Substring match on `name`                          |
| `before`   | string | —            | ISO timestamp; only nodes with `first_seen <= before` |

### Response `200`

```jsonc
{
  "nodes": [
    {
      "public_key":    string,           // 64-char hex public key
      "name":          string | null,
      "role":          string,           // "repeater" | "room" | "companion" | "sensor"
      "lat":           number | null,
      "lon":           number | null,
      "last_seen":     string (ISO),
      "first_seen":    string (ISO),
      "advert_count":  number,
      "hash_size":     number | null,    // latest hash size (1–3 bytes)
      "hash_size_inconsistent": boolean, // true if flip-flopping
      "hash_sizes_seen": [number] | undefined, // present only if >1 unique size seen
      "last_heard":    string (ISO) | undefined, // from in-memory packets or path relay
      "default_scope": string | null | undefined // Most recently observed transport scope for this node. null = never observed transport-scoped, "" = observed scoped but no configured region matched, "#name" = matched region. Only present when ingestor has applied the nodes_default_scope_v1 migration.
    }
  ],
  "total":  number,                      // total matching count (before pagination)
  "counts": {
    "repeaters":  number,                // global counts (not filtered by current query)
    "rooms":      number,
    "companions": number,
    "sensors":    number
  }
}
```

**Notes:**
- `hash_sizes_seen` is only present when more than one hash size has been observed.
- `last_heard` is only present when in-memory data provides a more recent timestamp than `last_seen`.

---

## GET /api/nodes/search

Quick node search for autocomplete/typeahead.

### Query Parameters

| Param | Type   | Required | Description                          |
|-------|--------|----------|--------------------------------------|
| `q`   | string | yes      | Search term (name substring or pubkey prefix) |

### Response `200`

```jsonc
{
  "nodes": [
    {
      "public_key":   string,
      "name":         string | null,
      "role":         string,
      "lat":          number | null,
      "lon":          number | null,
      "last_seen":    string (ISO),
      "first_seen":   string (ISO),
      "advert_count": number
    }
  ]
}
```

Returns `{ "nodes": [] }` when `q` is empty.

---

## GET /api/nodes/bulk-health

Bulk health summary for all nodes. Used by analytics dashboard.

### Query Parameters

| Param    | Type   | Default | Description                                     |
|----------|--------|---------|-------------------------------------------------|
| `limit`  | number | `50`    | Max nodes (capped at 200)                       |
| `region` | string | —       | Comma-separated IATA codes for regional filtering |

### Response `200`

Returns a JSON array (not wrapped in an object):

```jsonc
[
  {
    "public_key": string,
    "name":       string | null,
    "role":       string,
    "lat":        number | null,
    "lon":        number | null,
    "stats": {
      "totalTransmissions": number,
      "totalObservations":  number,
      "totalPackets":       number,   // same as totalTransmissions (backward compat)
      "packetsToday":       number,
      "avgSnr":             number | null,
      "lastHeard":          string (ISO) | null
    },
    "observers": [
      {
        "observer_id":   string,
        "observer_name": string | null,
        "avgSnr":        number | null,
        "avgRssi":       number | null,
        "packetCount":   number
      }
    ]
  }
]
```

**Note:** This is a bare array, not `{ nodes: [...] }`.

---

## GET /api/nodes/network-status

Aggregate network health status counts.

### Query Parameters

| Param    | Type   | Default | Description                         |
|----------|--------|---------|-------------------------------------|
| `region` | string | —       | Comma-separated IATA codes          |

### Response `200`

```jsonc
{
  "total":      number,
  "active":     number,    // within degradedMs threshold
  "degraded":   number,    // between degradedMs and silentMs
  "silent":     number,    // beyond silentMs
  "roleCounts": {
    "repeater":  number,
    "room":      number,
    "companion": number,
    "sensor":    number
    // may include "unknown" if role is missing
  }
}
```

---

## GET /api/nodes/:pubkey

Node detail page data.

### Path Parameters

| Param    | Type   | Description          |
|----------|--------|----------------------|
| `pubkey` | string | Node public key (hex)|

### Query Parameters

| Param     | Type   | Description |
|-----------|--------|-------------|
| `include` | string | Opt-in extras, comma-separated (may also repeat). `advertRoutes` adds `recentAdvertsByRoute`, `advertCounts`, `advertIntervals` and `route_class` on the `recentAdverts` ADVERT rows (see [Advert route classes](#advert-route-classes) and [Estimated advert intervals](#estimated-advert-intervals)). Unknown values are ignored. |

Without `include=advertRoutes` the response is exactly the pre-#2073 one:
no `recentAdvertsByRoute`, no `advertCounts`, no `advertIntervals`, no `route_class`. The breakdown
scans all of the node's ADVERT rows, so only the node page (full view and
side panel) asks for it; the packets, live, channels and route views and the
claimed-nodes lookups do not.

### Response `200`

```jsonc
{
  "node": {
    "public_key":    string,
    "name":          string | null,
    "role":          string,
    "lat":           number | null,
    "lon":           number | null,
    "last_seen":     string (ISO),
    "first_seen":    string (ISO),
    "advert_count":  number,
    "hash_size":     number | null,
    "hash_size_inconsistent": boolean,
    "hash_sizes_seen": [number] | undefined,
    "flood_advert_count_7d": number   // route_type 1 only (see below)
  },
  "recentAdverts": [Packet],  // last 20 packets for this node, newest ingest first;
                              // with include=advertRoutes ADVERT rows also carry route_class
  "recentAdvertsByRoute": {   // include=advertRoutes only; absent when the identity is hidden
    "limit":    20,           // max rows per class
    "flood":    [Packet],     // newest adverts per route class, newest ingest first;
                              // rows without the observations array
    "zero_hop": [Packet],
    "mixed":    [Packet],
    "unknown":  [Packet]      // only present when the node has such adverts
  },
  "advertCounts": {           // include=advertRoutes only; absent when the identity is hidden
    "24h": { "flood": number, "zero_hop": number, "mixed": number, "unknown": number },
    "7d":  { "flood": number, "zero_hop": number, "mixed": number, "unknown": number },
    "truncated": boolean,     // more adverts than the 50,000-row cap in the 7d floor
    "route_mask_backfill": { "status": "pending" | "backfilling" | "complete", "remaining": number | null }
  },
  "advertIntervals": {        // include=advertRoutes only; absent when the identity is hidden
    "window": 20,             // most adverts per class considered
    "flood":    AdvertIntervalEstimate,
    "zero_hop": AdvertIntervalEstimate
  }
}
```

Where `AdvertIntervalEstimate` is:

```jsonc
{
  "interval_s":     number | null,  // estimate in seconds, snapped when "snapped"; null when confidence is none
  "raw_interval_s": number | null,  // the median before snapping
  "snapped":        boolean,
  "samples":        number,         // adverts used (after a raised interval: those since the change)
  "gaps_used":      number,         // gaps between them that fit 1-4x the interval
  "confidence":     "high" | "medium" | "low" | "none",
  "status":         "estimated" | "none_observed" | "too_few" | "irregular",
  "last_advert":    string (ISO) | null  // first_seen of the newest advert in the class
}
```

Where `Packet` is a transmission object (see [Packet Object](#packet-object)).

#### Advert route classes

`recentAdvertsByRoute`, `advertCounts` and `route_class` (port/extension of
upstream `Kpa-clawbot/CoreScope#2073`) classify ADVERTs exactly like the
ADVERT rows of Relay Airtime Share (#89), from `transmissions.route_mask`
(every raw route type observed for the content hash):

| `route_class` | Meaning |
|---------------|---------|
| `flood`       | only route 0/1 (transport flood / flood) seen |
| `zero_hop`    | only route 2/3 — a zero-hop advert is sent DIRECT with an empty path |
| `mixed`       | both flood and zero-hop routes seen for the same advert |
| `unknown`     | no usable route — the same bucket Relay Airtime Share's `route_class` calls `legacy` (its historical plain ADVERT row); one classifier, two names kept for API compatibility |

Rows whose mask is not backfilled yet (and databases without the column)
fall back to the first-inserted `route_type`; `route_mask_backfill` says
whether that fallback is still in use (anything but `complete`: provisional).

- The class is filtered before the per-class limit, so frequent
  zero-hop adverts cannot push rare flood adverts out of `flood`. A mixed
  advert appears only under `mixed`.
- `advertCounts` counts distinct adverts (by hash) whose `first_seen` — when
  the advert was first heard, the axis `flood_advert_count_7d` uses too —
  lies in the window. Rows whose `first_seen` cannot be parsed are skipped,
  as for `flood_advert_count_7d`.
- `flood_advert_count_7d` is unchanged (an external contract): it counts
  `route_type` 1 only. `advertCounts["7d"].flood` differs from it: it also
  counts transport flood (route 0) and never counts a mixed advert, while
  `flood_advert_count_7d` counts a mixed advert whose first-inserted route
  was 1.
- Both new fields follow the node-detail visibility rules (blacklisted and
  hidden-name nodes are 404) and are omitted when the identity is hidden by
  the observer blacklist or an observer name (#68); `route_class` is then
  left off the `recentAdverts` rows as well.
- The per-class rows omit the `observations` array (they repeat rows of
  `recentAdverts`); `observation_count` and the best observation's fields
  stay. Their `route_class` is the class they were listed under.
- The breakdown is cached per node for up to 30 s, and refreshed once the
  node has a newer transmission (at most every 5 s). `flood_advert_count_7d`
  is never cached: it is counted on every request (a request that scanned
  the node for the breakdown itself takes the identical number from that
  scan).

#### Estimated advert intervals

`advertIntervals` (#245) estimates how often the node sends flood and
zero-hop adverts, from the gaps between the adverts listed in
`recentAdvertsByRoute.flood` and `.zero_hop` (so at most 20 per class, and no
extra query). Mixed and unknown adverts are not used.

The firmware settings it maps to (MeshCore `src/helpers/CommonCLI.cpp`):

| Class | Setting | Allowed values | Default |
|-------|---------|----------------|---------|
| `flood` | `flood.advert.interval` (hours) | 0 = off, 3–168 | 47 h on repeaters and room servers, off on sensors |
| `zero_hop` | `advert.interval` (minutes, stored / 2) | 0 = off, 60–240, even minutes | 2 min on an untouched new install, off after the first saved setting |

- **Gaps.** A gap is taken from the adverts' own (sender) timestamps
  when both are plausible: not ahead of `first_seen` by more than 10 min,
  positive, and within max(10 min, 10 %) of the `first_seen` gap. Otherwise
  it is taken from `first_seen`. A sender clock that is wrong by a steady
  offset is still used; a jump or reset is not.
- **The interval.** It must be seen directly, within 10 %, in at least two
  gaps and a quarter of them. It must be an interval the class's timer can
  run at, within 10 %: flood 3 h or more; zero-hop the 2 min new-install
  default or 60 min or more (so manual `advert.zerohop` every 10–30 min is
  irregular). Of those candidates, the one that explains the most gaps as
  1–4× itself wins.
  - A gap of k× the interval counts as k−1 missed adverts.
  - Shorter gaps are dropped and count neither way: manual adverts and
    reboots.
  - Longer gaps that are no multiple are *irregular* and lower the
    confidence. One is the zero-hop gap across a flood advert: the flood
    advert re-arms the zero-hop timer, so that gap is between one and two
    zero-hop intervals.
- **A raised interval.** A new interval of 2–4× the old one fits every new
  gap as missed adverts of the old one. When the newest 3 gaps that fit the
  interval are all the same multiple k > 1, that is read as a raised
  setting: the estimate is redone on the adverts since the newest gap at the
  old interval, and `samples` counts those. Two in a row, or different
  multiples, stay missed adverts. A lowered interval needs no special case:
  the new one explains the old gaps as multiples once it is seen in a
  quarter of the gaps.
- **The value.** It is the median of gap/k over the fitting gaps (`raw_interval_s`).
  It is snapped to the nearest settable value when it lies within 10 % of
  the settable range (`snapped`).
- **Confidence.**
  - `high`: ≥ 6 fitting gaps, and ≥ 75 % of the gaps that are not short fit.
  - `medium`: ≥ 3 fitting gaps and ≥ 50 %.
  - `low`: anything less.
  - `none`: fewer than 3 adverts, or no interval seen twice. `interval_s`
    is then `null`.
- **Status.** `estimated` when `interval_s` is set, `none_observed` with no
  adverts of the class, `too_few` under 3 adverts, `irregular` otherwise.
  The UI words the row from it.
- **Known limitation: sparse coverage.** When the interval itself is never
  heard twice in a row (a distant node heard every second or third time),
  it is no candidate, and a multiple of it is reported: a 47 h flood heard
  94 h and 141 h apart reads as 94 h or 141 h at medium confidence.
- **Known trade-off: false change.** The raised-interval rule can read an
  unchanged interval as raised. When the newest 3 heard gaps are all the
  same multiple k (every 2nd or 3rd advert lost), the result is k× at
  medium confidence on 4 adverts: 60 min reads as 120 min, 47 h as 94 h,
  and 120 min with 3× gaps as 360 min. Gaps that fit no multiple (an
  outage over 4×, a reboot advert between the 2× gaps) do not break the
  run. The next gap heard at the interval itself does, and the estimate
  returns to the interval. The opposite choice kept a raised interval at the
  old value, at high confidence, for weeks (review F1 on #247).
- **No zero-hop adverts.** A zero-hop advert is only recorded when an
  observer hears the node directly. "None observed" can therefore mean that
  the interval is 0 (off), or that no observer is in direct range.
- **Visibility and caching.** The field is cached and hidden together with
  `recentAdvertsByRoute`.

### Response `404`

```json
{ "error": "Not found" }
```

---

## GET /api/nodes/:pubkey/health

Detailed health information for a single node.

### Response `200`

```jsonc
{
  "node": {                          // full node row
    "public_key":   string,
    "name":         string | null,
    "role":         string,
    "lat":          number | null,
    "lon":          number | null,
    "last_seen":    string (ISO),
    "first_seen":   string (ISO),
    "advert_count": number
  },
  "observers": [
    {
      "observer_id":   string,
      "observer_name": string | null,
      "packetCount":   number,
      "avgSnr":        number | null,
      "avgRssi":       number | null,
      "iata":          string | null
    }
  ],
  "stats": {
    "totalTransmissions": number,
    "totalObservations":  number,
    "totalPackets":       number,    // same as totalTransmissions (backward compat)
    "packetsToday":       number,
    "avgSnr":             number | null,
    "avgHops":            number,    // rounded integer
    "lastHeard":          string (ISO) | null
  },
  "recentPackets": [                 // last 20 packets, observations stripped
    {
      // Packet fields (see Packet Object) minus `observations`
      "observation_count": number    // added for display
    }
  ]
}
```

### Response `404`

```json
{ "error": "Not found" }
```

---

## GET /api/nodes/:pubkey/paths

Path analysis for a node — all paths containing this node's prefix.

### Response `200`

```jsonc
{
  "node": {
    "public_key": string,
    "name":       string | null,
    "lat":        number | null,
    "lon":        number | null
  },
  "paths": [
    {
      "hops": [
        {
          "prefix": string,        // raw hex hop prefix
          "name":   string,        // resolved node name
          "pubkey": string | null,
          "lat":    number | null,
          "lon":    number | null
        }
      ],
      "count":      number,        // times this path was seen
      "lastSeen":   string (ISO) | null,
      "sampleHash": string         // hash of a sample packet using this path
    }
  ],
  "totalPaths":         number,    // unique path signatures
  "totalTransmissions": number     // total transmissions with this node in path
}
```

### Response `404`

```json
{ "error": "Not found" }
```

---

## GET /api/nodes/:pubkey/analytics

Per-node analytics over a time range.

### Query Parameters

| Param  | Type   | Default | Description              |
|--------|--------|---------|--------------------------|
| `days` | number | `7`     | Lookback window (1–365)  |

### Response `200`

```jsonc
{
  "node": {                          // full node row (same shape as nodes table)
    "public_key": string, "name": string | null, "role": string,
    "lat": number | null, "lon": number | null,
    "last_seen": string (ISO), "first_seen": string (ISO), "advert_count": number
  },
  "timeRange": {
    "from": string (ISO),
    "to":   string (ISO),
    "days": number
  },
  "activityTimeline": [
    { "bucket": string (ISO),  "count": number }   // hourly buckets
  ],
  "snrTrend": [
    {
      "timestamp":     string (ISO),
      "snr":           number,
      "rssi":          number | null,
      "observer_id":   string | null,
      "observer_name": string | null
    }
  ],
  "packetTypeBreakdown": [
    { "payload_type": number, "count": number }
  ],
  "observerCoverage": [
    {
      "observer_id":   string,
      "observer_name": string | null,
      "packetCount":   number,
      "avgSnr":        number | null,
      "avgRssi":       number | null,
      "firstSeen":     string (ISO),
      "lastSeen":      string (ISO)
    }
  ],
  "hopDistribution": [
    { "hops": string, "count": number }    // "0", "1", "2", "3", "4+"
  ],
  "peerInteractions": [
    {
      "peer_key":    string,
      "peer_name":   string,
      "messageCount": number,
      "lastContact": string (ISO)
    }
  ],
  "uptimeHeatmap": [
    { "dayOfWeek": number, "hour": number, "count": number }  // 0=Sun, 0–23
  ],
  "computedStats": {
    "availabilityPct":    number,     // 0–100
    "longestSilenceMs":   number,
    "longestSilenceStart": string (ISO) | null,
    "signalGrade":        string,     // "A", "A-", "B+", "B", "C", "D"
    "snrMean":            number,
    "snrStdDev":          number,
    "relayPct":           number,     // % of packets with >1 hop
    "totalPackets":       number,
    "uniqueObservers":    number,
    "uniquePeers":        number,
    "avgPacketsPerDay":   number
  }
}
```

### Response `404`

```json
{ "error": "Not found" }
```

---

## GET /api/nodes/:pubkey/reach

Per-node RF reach report (two-way link quality). Computes **directional** link counts from raw
path adjacency (a flood path is recorded origin→observer, so in `[A,B]` B received
A directly). A link is **bidirectional** when both directions have observations;
the **bottleneck** (weaker direction) rates two-way stability. Read-only; bounded
to a recent window. Identifies nodes only by **unique 2–3 byte** path prefixes
(1-byte prefixes collide and are excluded).

### Query Parameters

| Param  | Type   | Default | Description                          |
|--------|--------|---------|--------------------------------------|
| `days` | number | `7`     | Lookback window, clamped 1–30        |

### Response `200`

```jsonc
{
  "node": { "pubkey": string, "name": string, "role": string,
            "lat": number | null, "lon": number | null, "first_seen": string (ISO) },
  "window": { "days": number, "since": string (ISO) },
  "reliable_tokens": [string],          // uppercase hex prefixes unique to this node ([] if unidentifiable)
  "importance": {
    "neighbor_degree":    number,        // all-time distinct neighbours over valid neighbor_edges rows (see /api/reach-rank)
    "degree_rank":        number,        // placement on /api/reach-rank; 0 unless rank_status is "ranked"
    "nodes_with_edges":   number,        // ranked (visible) population = /api/reach-rank total
    "rank_status":        string,        // "ranked" | "unranked" | "unavailable" (snapshot unreadable)
    "rank_snapshot_at":   string (ISO),  // snapshot behind the three fields above; "" when unavailable
    "relay_observations": number,        // windowed obs with this node anywhere in path
    "bidirectional_links":number,
    "direct_observers":   number
  },
  "direct_observers": [
    { "pubkey": string, "name": string, "count": number,
      "avg_snr": number | null, "lat": number | null, "lon": number | null,
      "distance_km": number | null }
  ],
  "links": [
    { "pubkey": string, "name": string, "role": string,
      "lat": number | null, "lon": number | null,
      "we_hear": number, "they_hear": number,
      "bottleneck": number, "bidir": boolean,
      "distance_km": number | null }
  ]
}
```

`reliable_tokens: []` means the node has no unique 1–3 byte prefix and cannot be
reliably identified in paths; `links`/`direct_observers` will be empty.

### Visibility

An identity is **hidden** when its pubkey is in `nodeBlacklist` or
`observerBlacklist`, or when any of its names — node, observer, or its
`inactive_nodes` name while it has no named `nodes` row — starts with a
`hiddenNamePrefixes` entry. A hidden target returns
`404` (same body as an unknown node). Hidden identities are omitted from
`links` and `direct_observers`, and `bidirectional_links` / `direct_observers`
count only what is listed. Names are read live on every request — including
cached reports — so **hiding** (a blacklist or prefix change, or a rename into
a hidden prefix) applies on the next request. Un-hiding by renaming — of a
neighbour or of the target itself — can take up to the 5-minute cache TTL
(plus the server's 30 s node cache for the target), because the name recorded
when the report was computed still counts. This errs on the side of hiding.

`neighbor_degree` counts every valid edge, including ones to a hidden
neighbour — it is a number, not an identity, so it does not leak who the
neighbour is (see `/api/reach-rank`). `degree_rank` and `nodes_with_edges`,
by contrast, are computed over the **visible** (ranked) population: a hidden
or blacklisted node never occupies a placement or is counted in the total,
so no rank gap or total reveals it.

### Caching & limits

- **Response cache:** computed responses are cached for **5 minutes** per
  `pubkey|days`. Polling faster than that returns the same report — clients
  should not expect sub-5-minute freshness. Both **visibility** (above) and
  the **rank fields** (`neighbor_degree`, `degree_rank`, `nodes_with_edges`,
  `rank_status`, `rank_snapshot_at`) are applied on every request, cached or
  not: visibility from a live name lookup, rank from the shared degree
  snapshot — so a cached body always reflects the current blacklist/prefix
  state and always matches `/api/reach-rank` for the same
  `rank_snapshot_at`.
- **Scan cap:** the windowed path scan is hard-capped at **200,000** rows. A node
  with more matching observations in the window is truncated (counts become a
  representative sample rather than exhaustive).

### Response `400`

Returned when `:pubkey` is not a 64-char hex string.

```json
{ "error": "invalid pubkey: expected 64 hex chars" }
```

### Response `404`

Returned when the node is unknown or hidden (see Visibility).

```json
{ "error": "Not found" }
```

### Response `429`

Returned when the report is **cold** (not cached) and too many other cold
reports are already scanning the database. At most two cold reports compute at
once, so a burst of different nodes/windows cannot occupy the read pool and
slow unrelated endpoints; a cold request queues briefly for a slot and is only
shed if the limit is still saturated. Warm (cached) responses are never
affected, and concurrent requests for the *same* node and window still share
one computation. Nothing is cached for a shed request — retry after
`Retry-After` seconds and it recomputes normally.

```
Retry-After: 2
```

```json
{ "error": "reach is busy computing other reports", "retryAfter": 2 }
```

### Response `500`

Returned when the scan fails, or when the live name lookup for visibility
fails — the endpoint fails closed rather than serving unfiltered data.

```json
{ "error": "reach computation failed" }
```

---

## GET /api/reach-rank

Reach leaderboard: nodes ranked by **all-time neighbour count** — the same Rank
shown on each node's Reach page. A historical count, **not** a measure of radio
quality, range or traffic.

- **Valid edge** = a `neighbor_edges` row whose endpoints are both MeshCore
  pubkeys (exactly 64 hex characters, case-insensitive) and differ from each
  other. Legacy rows that fail this (e.g. an empty endpoint) are ignored at
  computation time; nothing is deleted.
- **Neighbours** = distinct neighbours over valid edges (within the ingestor's
  edge retention). The same value is `neighbor_degree` on `/api/nodes/:pubkey/reach`.
- **Ranked population** = nodes with at least one valid edge that have a Reach
  page (a node row, or an observer row with a name) and are not
  node-blacklisted, observer-blacklisted or hidden by a hidden-name prefix on
  any current name: node name, observer name, or its `inactive_nodes` name
  while it has no named `nodes` row (a node that aged out). Hidden nodes never
  occupy a placement.
- **Rank** = 1 + the number of ranked nodes with strictly more neighbours
  (competition ranking: 1, 1, 3); ties are listed in pubkey order.
- A search or page returns the global placements — nothing is renumbered.

### Query Parameters

| Param    | Type   | Default | Description                                                     |
|----------|--------|---------|-----------------------------------------------------------------|
| `q`      | string | —       | Case-insensitive substring of name or pubkey; max 64 characters |
| `offset` | number | `0`     | Rows to skip within the (filtered) list; must be ≥ 0            |
| `limit`  | number | `50`    | Rows per page; above 100 → 100; zero, negative or non-numeric → 50 |

### Response `200`

```jsonc
{
  "snapshot_at": string (ISO),  // when the neighbour graph was read
  "total":   number,            // ranked nodes in the whole leaderboard
  "matched": number,            // rows matching q (== total without q)
  "offset":  number,
  "limit":   number,
  "q":       string,            // trimmed query
  "rows": [
    { "rank": number, "pubkey": string, "name": string, "neighbors": number }
  ]
}
```

`name` may be empty (clients fall back to the pubkey).

### Caching

Served from a shared snapshot with a **60 s** TTL (also behind the Reach page's
Rank). Once it expires, the previous snapshot keeps being served — with its own
`snapshot_at` — while one background rebuild refreshes it. Only a request that
finds no snapshot at all (at start-up, or after a failed start-up read) waits
for the read. A failed rebuild is retried at most every 15 s. Blacklist and hidden-prefix changes re-rank immediately without a
new DB read.

### Response `400`

`q` longer than 64 characters (or not UTF-8), or `offset` not a non-negative integer.

### Response `500`

No snapshot could be read (never an empty leaderboard).

```json
{ "error": "reach rank unavailable" }
```

---

## GET /api/packets

Paginated packet (transmission) list with filtering.

### Query Parameters

| Param        | Type   | Default | Description                                        |
|--------------|--------|---------|----------------------------------------------------|
| `limit`      | number | `50`    | Page size                                          |
| `offset`     | number | `0`     | Pagination offset                                  |
| `type`       | string | —       | Filter by numeric payload type                    |
| `excludeTypes` | string | —     | Comma-separated numeric payload types (0–15), excluded before pagination |
| `route`      | string | —       | Filter by route type                               |
| `region`     | string | —       | Filter by region (IATA code substring)             |
| `observer`   | string | —       | Filter by observer ID                              |
| `hash`       | string | —       | Filter by packet hash                              |
| `since`      | string | —       | ISO timestamp lower bound                          |
| `until`      | string | —       | ISO timestamp upper bound                          |
| `node`       | string | —       | Filter by node pubkey                              |
| `nodes`      | string | —       | Comma-separated pubkeys (multi-node filter)        |
| `order`      | string | `DESC`  | Sort direction: `asc` or `desc`                    |
| `groupByHash`| string | —       | Set to `"true"` for grouped response               |
| `expand`     | string | —       | Set to `"observations"` to include observation arrays |

`excludeTypes=11` excludes CONTROL transmissions before `limit`, `offset`, and
`total` are calculated, for both raw and `groupByHash=true` responses. It uses
the same filtering in memory and in the SQLite fallback. It does not delete
packets or affect WebSocket delivery or packet-detail endpoints.

The list accepts at most 16 entries and 64 characters. Whitespace around entries
is trimmed; duplicates collapse. Empty/omitted means no exclusion. Codes 0–15
include reserved wire types; unknown/NULL stored types remain in the result.
An overlapping `type` inclusion and exclusion returns no packets for that type.
Malformed values, repeated `excludeTypes` parameters, and a nonempty exclusion
combined with `nodes` return HTTP 400. Use `node` for a supported single-node
combination. All other existing filters continue to compose normally.

The Packets page refetches when its type selection or Hide CONTROL checkbox
changes, so excluded traffic cannot fill the fetched page. Live updates are
still filtered locally. Pinned-hash views omit these exclusions so a direct
packet link remains visible.

### Response `200` (default)

```jsonc
{
  "packets": [Packet],    // see Packet Object below (observations stripped unless expand=observations)
  "total":   number,
  "limit":   number,
  "offset":  number
}
```

### Response `200` (groupByHash=true)

```jsonc
{
  "packets": [
    {
      "hash":              string,
      "first_seen":        string (ISO),
      "count":             number,       // observation count
      "observer_count":    number,       // unique observers
      "latest":            string (ISO),
      "observer_id":       string | null,
      "observer_name":     string | null,
      "path_json":         string | null,
      "payload_type":      number,
      "route_type":        number,
      "raw_hex":           string (hex),
      "decoded_json":      string | null,
      "observation_count": number,
      "snr":               number | null,
      "rssi":              number | null
    }
  ],
  "total": number
}
```

### Response `200` (nodes=... multi-node)

```jsonc
{
  "packets": [Packet],
  "total":   number,
  "limit":   number,
  "offset":  number
}
```

---

## GET /api/packets/timestamps

Lightweight endpoint returning only timestamps for timeline sparklines.

### Query Parameters

| Param   | Type   | Required | Description                       |
|---------|--------|----------|-----------------------------------|
| `since` | string | yes      | ISO timestamp lower bound         |

### Response `200`

Returns a JSON array of timestamps (strings or numbers):

```jsonc
["2025-07-17T00:00:01.000Z", "2025-07-17T00:00:02.000Z", ...]
```

### Response `400`

```json
{ "error": "since required" }
```

---

## GET /api/packets/:id

Single packet detail with byte breakdown and observations.

### Path Parameters

| Param | Type   | Description                                              |
|-------|--------|----------------------------------------------------------|
| `id`  | string | Packet ID (numeric) or 16-char hex hash                  |

### Response `200`

```jsonc
{
  "packet": Packet,                  // full packet/transmission object
  "path":   [string],                // parsed path hops (from packet.paths or [])
  "breakdown": {                     // byte-level packet structure
    "ranges": [
      {
        "start":  number,            // byte offset
        "end":    number,
        "label":  string,
        "hex":    string,
        "value":  string | number | null
      }
    ]
  } | null,
  "observation_count": number,
  "observations": [
    {
      "id":              number,
      "transmission_id": number,
      "hash":            string,
      "observer_id":     string | null,
      "observer_name":   string | null,
      "direction":       string | null,
      "snr":             number | null,
      "rssi":            number | null,
      "score":           number | null,
      "path_json":       string | null,
      "timestamp":       string (ISO),
      "raw_hex":         string (hex),
      "payload_type":    number,
      "decoded_json":    string | null,
      "route_type":      number
    }
  ]
}
```

### Response `404`

```json
{ "error": "Not found" }
```

---

## POST /api/decode

Decode a raw packet without storing it.

### Request Body

```jsonc
{
  "hex": string              // required — raw hex-encoded packet
}
```

Capped at **4096 bytes** before parsing (see
[Request-body byte caps](#request-body-byte-caps)). The largest frame the decoder
can accept is 1 header + 4 transport codes + 1 path-length byte +
`MAX_PATH_SIZE` (64) path bytes + `MAX_PACKET_PAYLOAD` (184) payload bytes =
254 bytes, i.e. 508 hex characters, i.e. a ~520-byte body — roughly 7x headroom.

### Response `200`

```jsonc
{
  "decoded": {
    "header":  DecodedHeader,
    "path":    DecodedPath,
    "payload": object
  }
}
```

### Response `400`

```json
{ "error": "hex is required" }
```

### Response `413`

```json
{ "error": "request body too large (max 4096 bytes)" }
```

---

## POST /api/packets/observations

Return the stored observations for several packets in one call. Used by the
packets table when a non-observer sort needs the child observations of the
groups it is about to render.

### Request Body

```jsonc
{
  "hashes": string[]         // required — content hashes, at most 200
}
```

Capped at **65536 bytes** before parsing (see
[Request-body byte caps](#request-body-byte-caps)). A full 200-hash request
serializes to ~3.8 KB, so the byte cap leaves roughly 17x headroom and the
200-hash limit below is what a client actually meets first.

### Response `200`

```jsonc
{
  "results": {
    "<hash>": [ Observation, ... ]   // one entry per requested hash
  }
}
```

An empty `hashes` array returns `{"results": {}}`. A hash with no stored
observations gets an empty array.

### Response `400`

```json
{ "error": "too many hashes (max 200)" }
```

```json
{ "error": "invalid JSON body" }
```

### Response `413`

```json
{ "error": "request body too large (max 65536 bytes)" }
```

---

## GET /api/observers

List all observers with packet counts.

### Response `200`

```jsonc
{
  "observers": [
    {
      "id":              string,
      "name":            string | null,
      "iata":            string | null,      // region code
      "last_seen":       string (ISO),
      "first_seen":      string (ISO),
      "packet_count":    number,
      "model":           string | null,      // hardware model
      "firmware":        string | null,
      "client_version":  string | null,
      "radio":           string | null,
      "battery_mv":      number | null,      // millivolts
      "uptime_secs":     number | null,
      "noise_floor":     number | null,      // dBm
      "packetsLastHour": number,             // computed, not from DB
      "lat":             number | null,      // from matched node
      "lon":             number | null,      // from matched node
      "nodeRole":        string | null       // from matched node
    }
  ],
  "server_time": string (ISO)                // server's current time
}
```

---

## GET /api/observers/:id

Single observer detail.

### Response `200`

```jsonc
{
  "id":              string,
  "name":            string | null,
  "iata":            string | null,
  "last_seen":       string (ISO),
  "first_seen":      string (ISO),
  "packet_count":    number,
  "model":           string | null,
  "firmware":        string | null,
  "client_version":  string | null,
  "radio":           string | null,
  "battery_mv":      number | null,
  "uptime_secs":     number | null,
  "noise_floor":     number | null,
  "packetsLastHour": number
}
```

### Response `404`

```json
{ "error": "Observer not found" }
```

---

## GET /api/observers/:id/analytics

Per-observer analytics.

### Query Parameters

| Param  | Type   | Default | Description              |
|--------|--------|---------|--------------------------|
| `days` | number | `7`     | Lookback window          |

### Response `200`

```jsonc
{
  "timeline": [
    { "label": string, "count": number }    // bucketed by hours/days
  ],
  "packetTypes": {
    "4": number,                             // keyed by payload_type number
    "5": number
  },
  "nodesTimeline": [
    { "label": string, "count": number }    // unique nodes per time bucket
  ],
  "snrDistribution": [
    { "range": string, "count": number }    // e.g. "6 to 8"
  ],
  "recentPackets": [Packet]                 // last 20 enriched observations
}
```

---

## GET /api/channels

List decoded channels with message counts.

### Query Parameters

| Param    | Type   | Default | Description                         |
|----------|--------|---------|-------------------------------------|
| `region` | string | —       | Comma-separated IATA codes          |

### Response `200`

```jsonc
{
  "channels": [
    {
      "hash":         string,        // channel name (used as key)
      "name":         string,        // decoded channel name
      "lastMessage":  string | null, // text of most recent message
      "lastSender":   string | null, // sender of most recent message
      "messageCount": number,
      "lastActivity": string (ISO)
    }
  ],
  // Shared hashtag channels approved by the administrator, listed even
  // before they carry traffic. Omitted when there are none. hash == name.
  "approvedChannels"?: [
    { "name": string, "hash": string }
  ],
  // Channels with stored messages left out of `channels` because their
  // proposal is not approved (see below). Omitted when none.
  "hiddenChannels"?: string[]
}
```

**Revoked channels are left out of `channels`.** A channel with stored messages
whose shared-channel proposal is **not approved** — revoked (see
[revoke](#post-apiadminchannel-proposalsidrevoke)), suggested again and
pending, or that re-suggestion rejected — does not appear in the list. Only
approving the proposal lists it again, with its history. Such a name has stored
messages only because it was decrypted earlier (approved, or through the
config), so a pending or rejected proposal never hides a channel that was never
decrypted. The exception is a name the ingestor also decrypts through its
built-in/config list (built-in keys, rainbow table, `hashChannels`,
`channelKeys`): that traffic keeps being decrypted, so such a channel is never
hidden. When the ingestor's `builtin-channels.json` is missing or unreadable,
nothing is hidden. Nothing is deleted: `GET /api/channels/:hash/messages` still
returns the history of a hidden channel.

`hiddenChannels` (`string[]`, omitted when empty) names the channels that were
left out this way. The Channels page uses it so a live WebSocket packet for a
stored message does not create the list row again. It only names channels that
have stored messages, so an unreviewed suggestion is never published here.

`hiddenChannels` is global and not filtered by `region`: a region request
returns the same set, and another open tab keeps the set it last loaded —
including after a re-approval, where live messages do not re-create the row in
that tab until its list reloads.

The decision is read once per 10 s snapshot (the one behind `approvedChannels`,
dropped early when an approve or revoke result is read), per channel that has
stored messages, so there is no per-request query of the proposals table and no
row cap. Proposals that are not approved are removed by retention
(`channelProposals.retentionDays` after the review); a hidden channel whose
messages are still stored then appears again.

`GET /api/analytics/channels` is not filtered: it still lists revoked channels
with their message and sender counts (no message text). Hiding is a list-level
measure, not a confidentiality control: the history stays readable by name.

**Known limit: a suggestion can hide a name no administrator acted on.** The
rule is "not approved", not "was approved before", so a channel whose stored
messages were decrypted through the config, and whose name the ingestor no
longer decrypts (removed from `hashChannels`/`channelKeys`, or past the
4096-name cap of `builtin-channels.json`), is left out of the list while a
suggestion for that name is pending. Distinguishing the two would need a
history marker that survives a re-suggestion — `reviewed_at` is reset when a
revoked proposal is suggested again, which is what makes a resubmission
idempotent — so the trade-off is kept: it is list-level only, the history stays
readable, a name the ingestor still decrypts is never hidden, the suggestion is
in the administrator's pending queue, and approving it lists the channel again.

---

## GET /api/channels/:hash/messages

Messages for a specific channel.

### Path Parameters

| Param  | Type   | Description                 |
|--------|--------|-----------------------------|
| `hash` | string | Channel name (from /api/channels) |

### Query Parameters

| Param    | Type   | Default | Description     |
|----------|--------|---------|-----------------|
| `limit`  | number | `100`   | Page size       |
| `offset` | number | `0`     | Pagination offset (from end) |

### Response `200`

```jsonc
{
  "messages": [
    {
      "sender":           string,
      "text":             string,
      "timestamp":        string (ISO),
      "sender_timestamp": number | null,    // device timestamp (unreliable)
      "packetId":         number,
      "packetHash":       string,
      "repeats":          number,           // dedup count
      "observers":        [string],         // observer names
      "hops":             number,
      "snr":              number | null,
      "observedPathHashSizes": [number],    // sorted unique relayed path widths (1–3)
      "senderPathHashSize": number          // 0 if unknown, otherwise header width (1–3)
    }
  ],
  "total": number                           // total deduplicated messages
}
```

`observedPathHashSizes` aggregates evidence from the message's observations.
It contains only hash widths encoded by non-empty relayed wire paths. Direct
zero-hop copies provide no hash-size evidence and do not add a value. More than
one value means different widths were observed for the same deduplicated
message; the field describes those observations, not the sender's permanent
configuration.

`senderPathHashSize` is read from the transmission's raw frame header, not
inferred from its observations. A flood can encode this width even when no
relay has forwarded it. A direct zero-hop marker, unsupported payload header,
or malformed frame yields 0 (unknown). This value describes that particular
frame, not the sender's permanent configuration.

---

## Shared channel proposals

Visitors suggest public hashtag channels; the administrator approves or rejects them with the existing `apiKey`. The server is read-only: it validates each request and writes it to a bounded file queue next to the database, and the ingestor applies it. An accepted request answers `202 { "requestId": string }` right away; poll the request status for the outcome. Validation errors answer `4xx` before anything is queued, and revoke also checks the stored status first and answers `409` when the proposal is not approved (see below). Timestamps are Unix epoch **milliseconds**.

A proposal:

```jsonc
{
  "id":         string,        // 16 hex characters
  "name":       string,        // "#Channel", case preserved, <= 31 UTF-8 bytes
  "status":     "pending" | "approved" | "rejected" | "revoked",
  "createdAt":  number,        // ms
  "reviewedAt"?: number        // ms, once reviewed (also set when revoked)
}
```

In `GET /api/admin/channel-proposals` each proposal may also carry `"builtIn": true` (see [Built-in names](#built-in-names)).

State machine: a new name becomes `pending` by default, or `approved` immediately if `channelProposals.autoApprove` is enabled. An administrator can move `pending` → `approved` or `rejected`; `approved` → `revoked` (see below); `revoked` → `pending` by suggesting the same name again (never auto-approved). An existing `pending` or `rejected` name is never auto-approved by another suggestion. `rejected` is terminal: suggesting a rejected name again reports the earlier rejection and does not reopen it, until retention deletes the rejected row `channelProposals.retentionDays` (default 30) days after the review; from then on the name can be suggested afresh. This keeps a rejected name from being pushed back into the review queue over and over while its row is retained.

### Built-in names

The ingestor already decrypts every name in its config-derived key list: the built-in keys, the rainbow table `channel-rainbow.json` (about 320 common names such as `#test`, `#chat` and `#general`), `hashChannels` and `channelKeys`. That list wins over approved suggestions, so approving such a name adds nothing (its traffic was already decrypted and listed once it has traffic) and revoking it does not stop decryption. The ingestor publishes those hashtag names to `builtin-channels.json` in the request queue directory at startup and after each `SIGHUP` reload; the server marks matching proposals with `"builtIn": true` in the admin list and in the request status, and the UI says so. An ingestor that predates this file simply leaves nothing marked.

## GET /api/channel-proposals/config

```json
{ "enabled": true }
```

`enabled` is true only when `channelProposals.enabled` is set and `apiKey` is strong.

## POST /api/channel-proposals

Body `{ "name": "#Channel" }` (the `#` is optional; surrounding spaces are trimmed). Only public hashtag channel names are accepted, never keys.

| Status | Meaning |
|--------|---------|
| `202` | `{ "requestId": string }` |
| `400` | Invalid body or name. The body must be exactly one JSON object with only `name` (unknown fields and trailing data are refused). The name must not be empty or over 31 bytes, and must not contain control characters, the line/paragraph separators U+2028/U+2029, direction overrides, or other invisible formatting characters (Unicode category Cf, such as U+200B, U+FEFF, U+00AD and the tag characters U+E0000–U+E007F; the zero-width joiner U+200D is allowed, and variation selectors are not Cf, so emoji sequences work). The firmware only limits the length; the character rule is CoreScope's own, so two different channels cannot look identical. |
| `403` | Suggestions disabled |
| `429` | `submissionsPerHour` reached; `Retry-After` in seconds |
| `503` | Request queue full; `Retry-After` in seconds |

## GET /api/channel-proposals/requests/:requestId

```jsonc
{
  "status":    "queued" | "pending" | "approved" | "rejected" | "revoked" | "error",
  "proposal"?: Proposal,   // once the ingestor has processed the request
  "error"?:    string,     // with status "error"
  "builtIn"?:  true        // the proposal's name is a built-in name (see above)
}
```

A duplicate suggestion reports the existing proposal and its status (for a rejected name: `rejected`, until retention). `revoked` is the outcome of a revoke request. `404` when the id is unknown or older than 24 hours.

## GET /api/admin/channel-proposals

Requires `X-API-Key`. Optional `?status=pending|approved|rejected|revoked`. Newest first, bounded.

```jsonc
{ "proposals": [Proposal & { "builtIn"?: true, "nearDuplicateOf"?: string[] }], "enabled": boolean }
```

**Letter case in names.** A hashtag channel's key is the first 16 bytes of
`sha256("#name")` of the exact name (MeshCore `docs/companion_protocol.md`, "Hashtag
Channels"; the meshcore-open app's `derivePskFromHashtag` does not change case),
so `#HelloWorld` and `#helloworld` are different channels with different keys.
They are therefore separate proposals and are never merged. `nearDuplicateOf`
lists the other proposal names (any status, also outside the `status` filter)
and built-in names that differ from this one only by letter case, sorted; it is
omitted when there are none. It is a hint for the administrator.

## POST /api/admin/channel-proposals/:id/approve and /reject

Requires `X-API-Key`. `202 { "requestId": string }`, `404` for an unknown proposal. Only pending proposals change; repeating the stored decision is harmless, and a contradicting one (reject after approve) ends with status `error`.

Missing or wrong key: `401`. No key configured, or a weak one: `403`.

## POST /api/admin/channel-proposals/:id/revoke

Requires `X-API-Key`. Undoes a previous approval: the ingestor stops decrypting the channel (unless the name is also in its config-derived list — the rainbow table, `channelKeys` or `hashChannels`, see [Built-in names](#built-in-names) — in which case that key keeps it decrypting) and it drops out of `GET /api/channels`' `approvedChannels`.

Unlike approve/reject, this endpoint checks synchronously, before queuing anything:

| Status | Meaning |
|--------|---------|
| `202` | `{ "requestId": string }` — the proposal was approved; the revoke is queued |
| `400` | Invalid suggestion id |
| `404` | Unknown proposal |
| `409` | The proposal is not currently approved (pending, rejected, or already revoked) — **nothing is queued** |

Missing or wrong key: `401`. No key configured, or a weak one: `403`.

There is a real race between the `409` check and the ingestor actually applying the command: another admin could approve, reject or revoke the same proposal in between. The ingestor re-validates the status from scratch when it applies the command and is the true source of truth; the synchronous `409` here is only a best-effort fast-fail for the common case, not a guarantee. When the race is lost, the request ends with status `error` and an error such as `suggestion is not approved (it is pending)`.

**Historical messages are kept, the channel leaves the list.** Revoking a channel removes its decryption key going forward and takes the channel out of `GET /api/channels` (unless the name is a [built-in name](#built-in-names), which keeps decrypting and is never hidden). Messages the ingestor already decoded and stored while the channel was approved are **not deleted**: `GET /api/channels/:hash/messages` still returns them, and the channel returns to the list with its history if the suggestion is approved again (suggest the name again, then approve). The server stays read-only: it only filters the list. The channel stays hidden while the name is re-suggested and pending, and after the administrator rejects that re-suggestion; to hide it again after an approval, revoke it again. A rejected re-suggestion therefore does not bring the channel back.

**Retention.** A revoked proposal's row is not deleted at revoke time — only its status changes, keeping `reviewedAt` as the audit timestamp of when it was revoked. It is removed later by the same retention sweep that prunes rejected proposals, once `reviewedAt` is older than `channelProposals.retentionDays` (see [Configuration](user-guide/configuration.md#shared-channel-suggestions)). Approved rows are still never pruned.

---

## GET /api/analytics/rf

RF signal analytics.

### Query Parameters

| Param    | Type   | Default | Description                         |
|----------|--------|---------|-------------------------------------|
| `region` | string | —       | Comma-separated IATA codes          |
| `area`   | string | —       | Area key — restricts to packets whose transmitter GPS falls in the area (ADVERT packets only; see [Area Filter](#area-filter)) |

### Response `200`

```jsonc
{
  "totalPackets":       number,      // observations with SNR data
  "totalAllPackets":    number,      // all regional observations
  "totalTransmissions": number,      // unique transmission hashes
  "snr": {
    "min":    number,
    "max":    number,
    "avg":    number,
    "median": number,
    "stddev": number
  },
  "rssi": {
    "min":    number,
    "max":    number,
    "avg":    number,
    "median": number,
    "stddev": number
  },
  "snrValues":  Histogram,           // pre-computed histogram (20 bins)
  "rssiValues": Histogram,           // pre-computed histogram (20 bins)
  "packetSizes": Histogram,          // pre-computed histogram (25 bins)
  "minPacketSize": number,           // bytes
  "maxPacketSize": number,
  "avgPacketSize": number,
  "packetsPerHour": [
    { "hour": string, "count": number }   // "2025-07-17T04"
  ],
  "payloadTypes": [
    { "type": number, "name": string, "count": number }
  ],
  "snrByType": [
    { "name": string, "count": number, "avg": number, "min": number, "max": number }
  ],
  "signalOverTime": [
    { "hour": string, "count": number, "avgSnr": number }
  ],
  "scatterData": [
    { "snr": number, "rssi": number }    // max 500 points
  ],
  "timeSpanHours": number
}
```

### Histogram Shape

```jsonc
{
  "bins": [
    { "x": number, "w": number, "count": number }
  ],
  "min": number,
  "max": number
}
```

---

## GET /api/analytics/topology

Network topology analytics.

### Query Parameters

| Param    | Type   | Default | Description                         |
|----------|--------|---------|-------------------------------------|
| `region` | string | —       | Comma-separated IATA codes          |
| `area`   | string | —       | Area key — only hops that resolve to nodes inside the area are counted in repeater/pair frequency tables |

### Response `200`

```jsonc
{
  "uniqueNodes": number,
  "avgHops":     number,
  "medianHops":  number,
  "maxHops":     number,
  "hopDistribution": [
    { "hops": number, "count": number }      // capped at 25
  ],
  "topRepeaters": [
    {
      "hop":    string,         // raw hex prefix
      "count":  number,
      "name":   string | null,  // resolved name
      "pubkey": string | null
    }
  ],
  "topPairs": [
    {
      "hopA":    string,
      "hopB":    string,
      "count":   number,
      "nameA":   string | null,
      "nameB":   string | null,
      "pubkeyA": string | null,
      "pubkeyB": string | null
    }
  ],
  "hopsVsSnr": [
    { "hops": number, "count": number, "avgSnr": number }
  ],
  "observers": [
    { "id": string, "name": string }
  ],
  "perObserverReach": {
    "<observer_id>": {
      "observer_name": string,
      "rings": [
        {
          "hops": number,
          "nodes": [
            {
              "hop":       string,
              "name":      string | null,
              "pubkey":    string | null,
              "count":     number,
              "distRange": string | null   // e.g. "1-3" or null if constant
            }
          ]
        }
      ]
    }
  },
  "multiObsNodes": [
    {
      "hop":    string,
      "name":   string | null,
      "pubkey": string | null,
      "observers": [
        {
          "observer_id":   string,
          "observer_name": string,
          "minDist":       number,
          "count":         number
        }
      ]
    }
  ],
  "bestPathList": [
    {
      "hop":           string,
      "name":          string | null,
      "pubkey":        string | null,
      "minDist":       number,
      "observer_id":   string,
      "observer_name": string
    }
  ]
}
```

---

## GET /api/analytics/channels

Channel analytics.

### Query Parameters

| Param    | Type   | Default | Description                         |
|----------|--------|---------|-------------------------------------|
| `region` | string | —       | Comma-separated IATA codes          |
| `area`   | string | —       | Area key — area filtering is supported but not exposed in the dashboard (channel stats are observer-based) |

### Response `200`

```jsonc
{
  "activeChannels": number,
  "decryptable":    number,
  "channels": [
    {
      "hash":       string,
      "name":       string,
      "messages":   number,
      "senders":    number,        // unique sender count
      "lastActivity": string (ISO),
      "encrypted":  boolean
    }
  ],
  "topSenders": [
    { "name": string, "count": number }
  ],
  "channelTimeline": [
    { "hour": string, "channel": string, "count": number }
  ],
  "msgLengths": [number]            // raw array of message character lengths
}
```

---

## GET /api/analytics/distance

Hop distance analytics.

### Query Parameters

| Param    | Type   | Default | Description                         |
|----------|--------|---------|-------------------------------------|
| `region` | string | —       | Comma-separated IATA codes          |
| `area`   | string | —       | Area key — restricts distance calculations to paths where the transmitter GPS falls in the area |

### Response `200`

```jsonc
{
  "summary": {
    "totalHops":  number,
    "totalPaths": number,
    "avgDist":    number,      // km, 2 decimal places
    "maxDist":    number       // km
  },
  "topHops": [
    {
      "fromName": string,
      "fromPk":   string,
      "toName":   string,
      "toPk":     string,
      "dist":     number,      // km
      "type":     string,      // "R↔R" | "C↔R" | "C↔C"
      "snr":      number | null,
      "hash":     string,
      "timestamp": string (ISO)
    }
  ],
  "topPaths": [
    {
      "hash":      string,
      "totalDist": number,     // km
      "hopCount":  number,
      "timestamp": string (ISO),
      "hops": [
        {
          "fromName": string,
          "fromPk":   string,
          "toName":   string,
          "toPk":     string,
          "dist":     number
        }
      ]
    }
  ],
  "catStats": {
    "R↔R": { "count": number, "avg": number, "median": number, "min": number, "max": number },
    "C↔R": { "count": number, "avg": number, "median": number, "min": number, "max": number },
    "C↔C": { "count": number, "avg": number, "median": number, "min": number, "max": number }
  },
  "distHistogram": Histogram | [],   // empty array if no data
  "distOverTime": [
    { "hour": string, "avg": number, "count": number }
  ]
}
```

---

## GET /api/analytics/hash-sizes

Hash size analysis across the network.

### Query Parameters

| Param    | Type   | Default | Description                         |
|----------|--------|---------|-------------------------------------|
| `region` | string | —       | Comma-separated IATA codes          |
| `area`   | string | —       | Area key — restricts to packets from nodes in the area |

### Response `200`

```jsonc
{
  "total": number,              // packets analyzed
  "distribution": {
    "1": number,                // 1-byte hash count
    "2": number,                // 2-byte hash count
    "3": number                 // 3-byte hash count
  },
  "hourly": [
    { "hour": string, "1": number, "2": number, "3": number }
  ],
  "topHops": [
    {
      "hex":    string,         // raw hop hex
      "size":   number,         // bytes (ceil(hex.length/2))
      "count":  number,
      "name":   string | null,
      "pubkey": string | null
    }
  ],
  "multiByteNodes": [
    {
      "name":     string,
      "hashSize": number,
      "packets":  number,
      "lastSeen": string (ISO),
      "pubkey":   string | null
    }
  ]
}
```

---

## GET /api/analytics/hash-collisions

Hash collision analysis — packets where the same hash was used by multiple different nodes (ambiguous routing).

### Query Parameters

| Param    | Type   | Default | Description                         |
|----------|--------|---------|-------------------------------------|
| `region` | string | —       | Comma-separated IATA codes          |
| `area`   | string | —       | Area key — restricts to packets from nodes in the area |

### Response `200`

```jsonc
{
  "collisions": [
    {
      "hash":     string,           // hop hex prefix that collides
      "count":    number,           // number of distinct nodes sharing this prefix
      "nodes": [
        {
          "pubkey": string,
          "name":   string | null,
          "count":  number          // observation count for this node
        }
      ]
    }
  ],
  "totalCollisions": number,
  "affectedPackets": number
}
```

---

## GET /api/nodes/clock-skew

Fleet-wide clock skew data. Returns all nodes for which clock skew has been calculated from ADVERT timestamp pairs.

### Query Parameters

| Param  | Type   | Default | Description                                         |
|--------|--------|---------|-----------------------------------------------------|
| `area` | string | —       | Area key — restricts to nodes whose GPS falls in the area |

### Response `200`

Returns a JSON array (not wrapped in an object):

```jsonc
[
  {
    "pubkey":         string,
    "nodeName":       string | null,
    "nodeRole":       string | null,
    "skewMs":         number | null,          // current estimated clock offset (ms)
    "driftPerDaySec": number | null,          // drift rate (seconds/day)
    "severity":       string,                 // "good" | "warning" | "critical"
    "samples":        null                    // always null in fleet response (too large)
  }
]
```

**Note:** This is a bare array, not `{ nodes: [...] }`.

---

## GET /api/analytics/subpaths

Subpath frequency analysis.

### Query Parameters

| Param    | Type   | Default | Description                            |
|----------|--------|---------|----------------------------------------|
| `minLen` | number | `2`     | Minimum subpath length (≥2)            |
| `maxLen` | number | `8`     | Maximum subpath length                 |
| `limit`  | number | `100`   | Max results                            |
| `region` | string | —       | Comma-separated IATA codes             |

### Response `200`

```jsonc
{
  "subpaths": [
    {
      "path":    string,        // "Node A → Node B → Node C"
      "rawHops": [string],      // ["aa", "bb", "cc"]
      "count":   number,
      "hops":    number,        // length of subpath
      "pct":     number         // percentage of totalPaths (0–100)
    }
  ],
  "totalPaths": number
}
```

---

## GET /api/analytics/subpath-detail

Detailed stats for a specific subpath.

### Query Parameters

| Param  | Type   | Required | Description                         |
|--------|--------|----------|-------------------------------------|
| `hops` | string | yes      | Comma-separated raw hex hop prefixes |

### Response `200`

```jsonc
{
  "hops":  [string],                     // input hops echoed back
  "nodes": [
    {
      "hop":    string,
      "name":   string,
      "lat":    number | null,
      "lon":    number | null,
      "pubkey": string | null
    }
  ],
  "totalMatches": number,
  "firstSeen":    string (ISO) | null,
  "lastSeen":     string (ISO) | null,
  "signal": {
    "avgSnr":  number | null,
    "avgRssi": number | null,
    "samples": number
  },
  "hourDistribution": [number],         // 24-element array (index = UTC hour)
  "parentPaths": [
    { "path": string, "count": number }
  ],
  "observers": [
    { "name": string, "count": number }
  ]
}
```

---

## GET /api/scope-stats

Scope-based packet statistics over a time window. Requires ingestor `scope_name_v1` migration to have run.

### Query Parameters

| Param    | Type   | Default | Description                                    |
|----------|--------|---------|------------------------------------------------|
| `window` | string | `24h`   | Time window: `1h`, `24h`, `7d`                |

### Response `200`

```jsonc
{
  "window":    string,               // echoed window ("1h", "24h", or "7d")
  "summary": {
    "transportTotal": number,        // scoped + unscoped transport-route packets
    "scoped":         number,        // Code1 ≠ 0000 (named + unknown regions)
    "unscoped":       number,        // transport-route with Code1 = 0000
    "unknownScope":   number         // scoped but no configured region matched (subset of scoped)
  },
  "byRegion": [
    { "name": string, "count": number }  // region name and packet count
  ],
  "timeSeries": [
    { "t": string (ISO), "scoped": number, "unscoped": number }  // bucket timestamps and counts
  ]
}
```

**Notes:**
- `transportTotal` = `scoped` + `unscoped` (only route_type 0 or 3 packets)
- `scoped` = packets with Code1 ≠ 0000
- `unscoped` = transport-route packets with Code1 = 0000
- `unknownScope` = scoped packets that did not match any configured region name
- Time-series bucket size depends on window:
  - `1h` window → 5-minute buckets
  - `24h` window → 1-hour buckets
  - `7d` window → 6-hour buckets
- Cached 30 seconds

> **Note:** On deployments with pre-existing data, `unscoped` will be inflated until the async startup backfill completes, because transport-route rows inserted before the `scope_name_v1` migration ran have `scope_name = NULL` and are indistinguishable from Code1=0000 rows. The backfill goroutine populates them at startup but may take several minutes on large databases.

### Response `400`

```json
{ "error": "window must be 1h, 24h, or 7d" }
```

### Response `500` Internal Server Error

`scope_name` column does not exist (ingestor has not run migrations yet):

```json
{ "error": "scope_name column not present — run ingestor to apply migrations" }
```

---

## GET /api/resolve-hops

Resolve path hop hex prefixes to node names with regional disambiguation.

### Query Parameters

| Param       | Type   | Required | Description                              |
|-------------|--------|----------|------------------------------------------|
| `hops`      | string | yes      | Comma-separated hex hop prefixes         |
| `observer`  | string | no       | Observer ID for regional context         |
| `originLat` | number | no       | Origin latitude for distance-based disambiguation |
| `originLon` | number | no       | Origin longitude                         |

### Response `200`

```jsonc
{
  "resolved": {
    "<hop>": {
      "name":         string | null,
      "pubkey":       string | null,
      "ambiguous":    boolean | undefined,   // true if multiple candidates
      "unreliable":   boolean | undefined,   // true if failed sanity check
      "candidates":   [Candidate],
      "conflicts":    [Candidate],
      "globalFallback": boolean | undefined,
      "filterMethod": string | undefined,    // "geo" | "observer"
      "hopBytes":     number | undefined,    // for ambiguous entries
      "totalGlobal":  number | undefined,
      "totalRegional": number | undefined,
      "filterMethods": [string] | undefined
    }
  },
  "region": string | null
}
```

**Candidate shape:**

```jsonc
{
  "name":         string,
  "pubkey":       string,
  "lat":          number | null,
  "lon":          number | null,
  "regional":     boolean,
  "filterMethod": string,
  "distKm":       number | null
}
```

---

## GET /api/traces/:hash

All observations of a specific packet hash, sorted chronologically.

### Path Parameters

| Param  | Type   | Description    |
|--------|--------|----------------|
| `hash` | string | Packet hash    |

### Response `200`

```jsonc
{
  "traces": [
    {
      "observer":      string | null,   // observer_id
      "observer_name": string | null,
      "time":          string (ISO),
      "snr":           number | null,
      "rssi":          number | null,
      "path_json":     string | null
    }
  ]
}
```

---

## GET /api/config/theme

Theme and branding configuration (merged from config.json + theme.json).

### Response `200`

```jsonc
{
  "branding": {
    "siteName": string,          // default: "CoreScope"
    "tagline":  string           // default: "Real-time MeshCore LoRa mesh network analyzer"
    // ... additional branding keys from config/theme files
  },
  "theme": {
    "accent":      string,       // hex color, default "#4a9eff"
    "accentHover": string,
    "navBg":       string,
    "navBg2":      string
    // ... additional theme CSS values
  },
  "themeDark": {
    // dark mode overrides (may be empty object)
  },
  "nodeColors": {
    "repeater":  string,         // hex color
    "companion": string,
    "room":      string,
    "sensor":    string,
    "observer":  string
  },
  "typeColors": {
    // payload type → hex color overrides
  },
  "home": object | null          // home page customization
}
```

---

## GET /api/config/regions

Available regions (IATA codes) merged from config + DB.

### Response `200`

```jsonc
{
  "<iata_code>": string          // code → display name
  // e.g. "SFO": "San Francisco", "LAX": "Los Angeles"
}
```

Returns a flat key-value object.

---

## GET /api/config/areas

Available area filters defined in `config.json` under `areas`. Used by the frontend to populate the area pill bar. Entries with an empty `label` (e.g. comment keys) are excluded.

### Response `200`

```jsonc
[
  {
    "key":   string,   // area key as defined in config (e.g. "bayarea")
    "label": string    // display name (e.g. "Bay Area")
  }
]
```

Returns `[]` when no areas are configured.

**Note:** Polygon coordinates are **not** included. Use `/api/config/areas/polygons` for the full geometry.

---

## GET /api/config/areas/polygons

Full area definitions including polygon/bounding-box coordinates. Intended for map rendering tools (e.g. the area-map debug tool).

### Response `200`

```jsonc
[
  {
    "key":   string,
    "label": string,
    "polygon": [[number, number]] | undefined,   // [lat, lon] pairs (if polygon-style)
    "latMin":  number | undefined,               // bounding-box style
    "latMax":  number | undefined,
    "lonMin":  number | undefined,
    "lonMax":  number | undefined
  }
]
```

Returns `[]` when no areas are configured.

---

## GET /api/config/client

Client-side configuration values.

### Response `200`

```jsonc
{
  "roles":              object | null,
  "healthThresholds":   object | null,
  "tiles":              object | null,
  "snrThresholds":      object | null,
  "distThresholds":     object | null,
  "maxHopDist":         number | null,
  "limits":             object | null,
  "perfSlowMs":         number | null,
  "wsReconnectMs":      number | null,
  "cacheInvalidateMs":  number | null,
  "externalUrls":       object | null,
  "propagationBufferMs": number,         // default: 5000
  "estimatedPositions": { "enabled": boolean } // default: true; operator policy
}
```

---

## GET /api/config/cache

Cache TTL configuration (raw values in seconds).

### Response `200`

Returns the raw `cacheTTL` object from `config.json`, or `{}` if not set:

```jsonc
{
  "stats":                number | undefined,    // seconds
  "nodeDetail":           number | undefined,
  "nodeHealth":           number | undefined,
  "nodeList":             number | undefined,
  "bulkHealth":           number | undefined,
  "networkStatus":        number | undefined,
  "observers":            number | undefined,
  "channels":             number | undefined,
  "channelMessages":      number | undefined,
  "analyticsRF":          number | undefined,
  "analyticsTopology":    number | undefined,
  "analyticsChannels":    number | undefined,
  "analyticsHashSizes":   number | undefined,
  "analyticsSubpaths":    number | undefined,
  "analyticsSubpathDetail": number | undefined,
  "nodeAnalytics":        number | undefined,
  "nodeSearch":           number | undefined,
  "invalidationDebounce": number | undefined
}
```

---

## GET /api/config/map

Map default center and zoom.

### Response `200`

```jsonc
{
  "center": [number, number],      // [lat, lon], default [37.45, -122.0]
  "zoom":   number                 // default 9
}
```

---

## GET /api/iata-coords

IATA airport/region coordinates for client-side regional filtering.

### Response `200`

```jsonc
{
  "coords": {
    "<iata_code>": {
      "lat": number,
      "lon": number,
      "radiusKm": number
    }
  }
}
```

---

## GET /api/audio-lab/buckets

Representative packets bucketed by payload type for audio lab.

### Response `200`

```jsonc
{
  "buckets": {
    "<type_name>": [
      {
        "hash":              string,
        "raw_hex":           string (hex),
        "decoded_json":      string | null,
        "observation_count": number,
        "payload_type":      number,
        "path_json":         string | null,
        "observer_id":       string | null,
        "timestamp":         string (ISO)
      }
    ]
  }
}
```

---

## WebSocket Messages

### Connection

Connect to `ws://<host>` (or `wss://<host>` for HTTPS). No authentication.
The server broadcasts messages to all connected clients.

### Message Wrapper

All WebSocket messages use this envelope:

```jsonc
{
  "type": string,     // "packet" or "message"
  "data": object      // payload (shape depends on type)
}
```

### Message Type: `"packet"`

Broadcast on every new packet ingestion.

```jsonc
{
  "type": "packet",
  "data": {
    "id":                number,           // observation or transmission ID
    "raw":               string (hex) | null,
    "decoded": {
      "header": {
        "routeType":       number,
        "payloadType":     number,
        "payloadVersion":  number,
        "payloadTypeName": string          // "ADVERT", "GRP_TXT", "TXT_MSG", etc.
      },
      "path": {
        "hops":            [string]        // hex hop prefixes
      },
      "payload":           object          // decoded payload (varies by type)
    },
    "snr":               number | null,
    "rssi":              number | null,
    "hash":              string | null,
    "observer":          string | null,    // observer_id
    "observer_name":     string | null,
    "path_json":         string | null,    // JSON-stringified hops array
    "packet":            Packet | undefined, // full packet object (when available)
    "observation_count": number | undefined
  }
}
```

**Notes:**
- `data.decoded` is always present with at least `header.payloadTypeName`.
- `data.packet` is included for raw packet ingestion (Format 1 / MQTT), may be absent for companion bridge messages.
- `data.path_json` is the JSON-stringified version of `data.decoded.path.hops`.

#### Fields consumed by frontend pages:

| Field                     | live.js | packets.js | app.js | channels.js |
|---------------------------|---------|------------|--------|-------------|
| `data.id`                 | ✓       | ✓          |        |             |
| `data.hash`               | ✓       | ✓          |        |             |
| `data.raw`                | ✓       |            |        |             |
| `data.decoded.header.payloadTypeName` | ✓ | ✓   |        |             |
| `data.decoded.payload`    | ✓       | ✓          |        |             |
| `data.decoded.path.hops`  | ✓       |            |        |             |
| `data.snr`                | ✓       |            |        |             |
| `data.rssi`               | ✓       |            |        |             |
| `data.observer`           | ✓       |            |        |             |
| `data.observer_name`      | ✓       |            |        |             |
| `data.packet`             |         | ✓          |        |             |
| `data.observation_count`  |         | ✓          |        |             |
| `data.path_json`          | ✓       |            |        |             |
| `data.observed_path_hash_sizes` |       |            |        | ✓           |
| (any)                     |         |            | ✓ (*)  |             |

(*) `app.js` passes all messages to registered `wsListeners` and uses them only for cache invalidation.

### Message Type: `"message"`

Broadcast for GRP_TXT (channel message) packets only. Same `data` shape as `"packet"` type.
`channels.js` listens for this type to update the channel message feed in real time.

```jsonc
{
  "type": "message",
  "data": {
    // identical shape to "packet" data
  }
}
```

---

## Shared Object Shapes

### Packet Object

A transmission/packet as stored in memory and returned by most endpoints:

```jsonc
{
  "id":                number,              // transmission ID
  "raw_hex":           string (hex) | null,
  "hash":              string,              // content hash (dedup key)
  "first_seen":        string (ISO),        // when first observed
  "timestamp":         string (ISO),        // display timestamp (= first_seen)
  "route_type":        number,              // 0=DIRECT, 1=FLOOD, 2=reserved, 3=TRANSPORT
  "payload_type":      number,              // 0=REQ, 1=RESPONSE, 2=TXT_MSG, 3=ACK, 4=ADVERT, 5=GRP_TXT, 7=ANON_REQ, 8=PATH, 9=TRACE, 11=CONTROL
  "payload_version":   number | null,
  "decoded_json":      string | null,       // JSON-stringified decoded payload
  "observation_count": number,
  "observer_id":       string | null,       // from "best" observation
  "observer_name":     string | null,
  "snr":               number | null,
  "rssi":              number | null,
  "path_json":         string | null,       // JSON-stringified hop array
  "observed_path_hash_sizes": [number] | undefined, // sorted unique relayed path widths (1–3)
  "direction":         string | null,
  "score":             number | null,
  "observations":      [Observation] | undefined  // stripped by default on list endpoints
}
```

### Observation Object

A single observation of a transmission by an observer:

```jsonc
{
  "id":              number,
  "transmission_id": number,
  "hash":            string,
  "observer_id":     string | null,
  "observer_name":   string | null,
  "direction":       string | null,
  "snr":             number | null,
  "rssi":            number | null,
  "score":           number | null,
  "path_json":       string | null,
  "timestamp":       string (ISO) | number,  // ISO string or unix epoch
  // Enriched fields (from parent transmission):
  "raw_hex":         string (hex) | null,
  "payload_type":    number,
  "decoded_json":    string | null,
  "route_type":      number
}
```

### DecodedHeader

```jsonc
{
  "routeType":       number,
  "payloadType":     number,
  "payloadVersion":  number,
  "payloadTypeName": string    // human-readable name
}
```

### DecodedPath

```jsonc
{
  "hops":      [string],       // hex hop prefixes, e.g. ["a1b2", "c3d4"]
  "hashSize":  number,         // bytes per hop hash (1–3)
  "hashCount": number          // number of hops in path field
}
```

---

## Payload Type Reference

| Value | Name       | Description                      |
|-------|------------|----------------------------------|
| 0     | `REQ`      | Request                          |
| 1     | `RESPONSE` | Response                         |
| 2     | `TXT_MSG`  | Direct text message              |
| 3     | `ACK`      | Acknowledgement                  |
| 4     | `ADVERT`   | Node advertisement               |
| 5     | `GRP_TXT`  | Group/channel text message       |
| 7     | `ANON_REQ` | Anonymous request                |
| 8     | `PATH`     | Path / traceroute                |
| 9     | `TRACE`    | Trace response                   |
| 11    | `CONTROL`  | Control message                  |

## Route Type Reference

| Value | Name        | Description                          |
|-------|-------------|--------------------------------------|
| 0     | `DIRECT`    | Direct (with transport codes)        |
| 1     | `FLOOD`     | Flood/broadcast                      |
| 2     | (reserved)  |                                      |
| 3     | `TRANSPORT` | Transport (with transport codes)     |

---

## Area Filter

The `?area=<key>` query parameter is a **display-side geographic filter** that attributes data to a region based on the **transmitting node's own GPS coordinates**, as broadcast in its ADVERT packets. It is distinct from the observer-based `?region=` filter.

### Configuration

Areas are defined in `config.json` under the `areas` key:

```jsonc
{
  "areas": {
    "bayarea": {
      "label": "Bay Area",
      "polygon": [[37.9, -122.5], [37.9, -121.9], [37.3, -121.9], [37.3, -122.5]]
    },
    "sanjose": {
      "label": "San Jose",
      "latMin": 37.25, "latMax": 37.45,
      "lonMin": -122.05, "lonMax": -121.75
    }
  }
}
```

Each entry may use either a `polygon` (array of `[lat, lon]` pairs, minimum 3 points) or a bounding box (`latMin`/`latMax`/`lonMin`/`lonMax`). The polygon check uses standard ray-casting point-in-polygon.

### Attribution rules

| Packet type | Area-attributable? | Reason |
|-------------|-------------------|--------|
| ADVERT (4)  | Yes | Carries `public_key` + transmitter GPS in payload |
| GRP_TXT (5), TXT_MSG (2), REQ (0), others | No | Sender is encrypted; origin cannot be determined |

When `?area=` is active, **only ADVERT packets** (and nodes derived from them) are included in filtered results. All other packet types are excluded. This is by design — non-ADVERT packets have encrypted senders and cannot be attributed to a geographic origin.

### GPS staleness

Node GPS coordinates are read from the `nodes` table, which is updated on ADVERT ingest. A node that moves between areas will not be re-attributed until its next ADVERT (typically 12–24 hours for repeaters). The area node set is cached for 30 seconds server-side.

### Endpoints supporting `?area=`

| Endpoint | Area support |
|----------|-------------|
| `GET /api/nodes` | Filters node list by GPS in area |
| `GET /api/analytics/rf` | Restricts RF stats to ADVERT packets from area nodes |
| `GET /api/analytics/topology` | Counts only hops that resolve to nodes in the area |
| `GET /api/analytics/channels` | Supported (not used by dashboard UI) |
| `GET /api/analytics/distance` | Restricts distance paths to area-node transmitters |
| `GET /api/analytics/hash-sizes` | Restricts hash analysis to area-node packets |
| `GET /api/analytics/hash-collisions` | Restricts collision analysis to area-node packets |
| `GET /api/nodes/clock-skew` | Restricts fleet clock skew list to nodes in area |

### Cross-antimeridian polygons

Polygons that span the 180° meridian (antimeridian) are **not supported** — ray-casting point-in-polygon breaks at the date line. Split such areas into two separate entries.
