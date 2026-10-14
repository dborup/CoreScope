# Client Declared-Region Reports

A mobile companion (for example CoreDrive RX / corescope-rx) asks a repeater in direct RF range which
regions it lets flood (`ANON_REQ_TYPE_REGIONS`) and relays the answer to CoreScope on
`meshcore/client/{PUBLIC_KEY}/regions`. CoreScope keeps every answer as a separate report with its
provenance, so a repeater's declared regions can be compared over time and between collectors.

The feature is **off by default** and separate from `clientRxCoverage`. This page is the protocol
contract: the ingestor (`cmd/ingestor/client_regions.go`) applies the rules below, and the tests in
`cmd/ingestor/client_regions_test.go` pin them.

## What the repeater actually sends (firmware)

From the MeshCore firmware (`examples/simple_repeater/MyMesh.cpp` `handleAnonRegionsReq`,
`src/helpers/RegionMap.cpp` `exportNamesTo`):

- The repeater answers only a request delivered over a **direct** route. It is rate-limited
  (`anon_limiter`). It does not answer a flood request.
- The reply holds the requester's timestamp (4 bytes), the **repeater's own RTC clock** (4 bytes,
  unsigned seconds) and a comma-separated list of the regions **not** marked `REGION_DENY_FLOOD`.
  A leading `#` is stripped from each name. `*` stands for the wildcard region. Private regions keep
  their `$` prefix.
- A name holds at most 30 bytes (`RegionEntry.name[31]`). The list must fit in
  `MAX_PACKET_PAYLOAD - 12` = 172 bytes. A name that does not fit is **skipped**, not cut off, so an
  overflowing answer has gaps and no marker says so.
- Region names are matched case-sensitively (`strcmp`), so CoreScope never changes their case.

## Topic and identity

```
meshcore/client/{COLLECTOR_PUBKEY}/regions
```

- **Identity is the topic segment.** `{COLLECTOR_PUBKEY}` is the public key of the companion that
  relayed the answer, which this page calls the *collector*. Only the broker ACL binds it to the
  publisher, and CoreScope never takes identity from the payload: any `origin_id`, `collector` or
  similar payload field is ignored.
- The collector must be a **full 64-hex key** (trimmed, then lowercased). This is stricter than the
  2–64 hex accepted on `/packets`, on purpose: `admin delete-client-rx` erases by the exact 64-hex
  key, so a shorter key could store rows that the erasure path could never reach. Any other shape
  drops the message.
- The topic must have exactly four segments. `meshcore/client/{key}/regions/extra` is dropped.
- The rules for client packets apply here too, and run before any write: the `observerBlacklist`
  check on the collector key, and the `clientRxCoverage.sources` allowlist, which governs the whole
  `meshcore/client/…` namespace. Nothing on this topic ever reaches the observer path.

## Payload

```json
{
  "timestamp": "2026-10-14T09:12:30.120Z",
  "target": "bb11223344556677889900aabbccddeeff00112233445566778899aabbccddaa",
  "regions": ["*", "be", "be-vlg"],
  "truncated": false,
  "repeater_clock": 1760433150,
  "gps": { "lat": 51.2, "lon": 4.4, "acc_m": 8.0 }
}
```

| Field | Required | Rule |
|---|---|---|
| `target` | yes | The repeater that answered. Must be a **full 64-hex key** after trim and lowercase. The app can only ask a full key, so anything shorter is malformed and the message is dropped. |
| `regions` | yes | A JSON array of strings. See [Region normalisation](#region-normalisation). A missing field, a non-array, or any non-string entry drops the whole message. |
| `timestamp` | yes | When the collector received the answer (its own clock): the *reported time*. See [Three clocks](#three-clocks). |
| `truncated` | no | Only a JSON `true` sets the flag. It is a hint from the app, stored as sent. |
| `repeater_clock` | no | The repeater's RTC from the reply, in unsigned seconds. See [Three clocks](#three-clocks). |
| `gps` | no | The collector's position. See [Position and accuracy](#position-and-accuracy). |

### Region normalisation

The steps below run on each entry, in order:

1. Trim trailing NUL bytes and surrounding whitespace. The reply is encrypted with a block cipher,
   so the last name can carry NUL padding. Older app builds forward that padding.
2. If the entry is now empty, skip it. It was padding, not a region.
3. Reject the whole message if the name contains a comma, a control character, or invalid UTF-8.
   Reject it too if the name is over 30 bytes, or empty, not counting a leading `#`.
4. Normalise the name. `*` (the wildcard) and `$name` (a private region) stay as they are. Any
   other name gets one leading `#` (`be` and `#be` both become `#be`). This is the convention
   `hashRegions` and the observer `/neighbors` scopes already use (`internal/regions.Normalize`).
   Case is preserved.
5. Drop duplicates, keeping the order in which names first appear.

The message is also rejected if the array has more than 86 entries, or if the names, joined by
commas, take more than 172 bytes (before the `#` is added). No answer the radio can deliver exceeds
these limits.

**An explicitly empty list is an answer.** `"regions": []`, or a list that is only padding, means
*the repeater answered and lets nothing flood*. It is stored as a report with no regions. A repeater
that never answered produces **no report at all**, because the firmware stays silent on a non-direct
request or when the rate limiter blocks it. A missing report is therefore never evidence of
"declares nothing", and a missing or malformed `regions` field is never read as an empty list.

### Position and accuracy

The position is optional, because an answer is meaningful without one. Validation never drops the
report, only the position:

- `gps.lat` and `gps.lon` must be JSON numbers, finite, with lat in [-90, 90] and lon in [-180, 180].
  `(0, 0)` is treated as "no fix", a common GPS placeholder. If either value fails, the report is
  stored with no position.
- `gps.acc_m` is optional. If present, it must be a finite JSON number in [0, 10000] metres.
  Otherwise the **whole position** is dropped: a fix whose accuracy cannot be trusted cannot place a
  direct-range exchange. A missing `acc_m` keeps the position, with the accuracy stored as NULL.
- An accuracy is never stored without the position it qualifies.

### Three clocks

Each report carries three different times. CoreScope never substitutes one for another:

| Column | Source | Used for |
|---|---|---|
| `reported_at` | payload `timestamp` (the collector's clock) | Ordering ("latest answer"), duplicate detection, retention, expiry |
| `ingested_at` | the ingestor's clock when the report was accepted | Audit only |
| `repeater_clock` | payload `repeater_clock` (the repeater's RTC) | Stored as received, for clock-drift diagnosis. Never used for ordering or retention |

- `timestamp` must be RFC3339 with an explicit zone (`Z` or `±hh:mm`). A missing, unparseable or
  zone-less value drops the message. It is **not** replaced with ingest time, which would merge two
  of the clocks. More than 15 minutes in the future (the same tolerance as `/neighbors` reports)
  drops the message.
- **Expired:** a `timestamp` older than the retention window (see [Retention](#retention)) is dropped
  at ingest. Retention would delete the row anyway, and it cannot become the latest answer.
- `repeater_clock` must be a whole JSON number in [0, 4294967295]. Anything else, or a missing value,
  is stored as NULL, never as 0. A repeater with a reset RTC reports a plausible-looking but wrong
  clock, which is why this value is kept apart from `reported_at`.
- `reported_at` and `ingested_at` are stored as UTC with millisecond precision
  (`2006-01-02T15:04:05.000Z`), so a plain text comparison orders them correctly.

### History, provenance, duplicates and order

- Every accepted answer is a **report**. Nothing is overwritten, and the table holds no per-target
  state.
- Reports are unique on `(target, reported_at, collector)`. An exact redelivery (MQTT QoS 1, an app
  retry) is a no-op. Reports from **different collectors**, or from the same collector **at different
  times**, are all kept, so disagreement between collectors stays visible.
- **Out of order:** a buffered report that arrives late is stored in its place in the history. The
  latest answer is always the one with the greatest `reported_at`, never the newest by `ingested_at`
  or row id, so a late report cannot replace a fresher one.

## Storage: `client_declared_regions` (ingestor-owned)

```
client_declared_regions(
  id, collector_pubkey, target_pubkey, regions_csv, truncated,
  reported_at, ingested_at, repeater_clock, lat, lon, pos_acc_m, mqtt_source,
  UNIQUE(target_pubkey, reported_at, collector_pubkey))
INDEX idx_cdr_reported  (reported_at)        -- retention and expiry
INDEX idx_cdr_collector (collector_pubkey)   -- admin delete-client-rx
```

`regions_csv` is the normalised list joined with `,`, and an empty string means an empty answer.
`mqtt_source` is the `mqttSources[].name` that delivered the report, so a report can be traced to a
broker and its ACL. The unique index also serves the per-target read, which is ordered by
`reported_at`.

The name is deliberately not upstream's `node_declared_regions`. This table normalises names with
`#` and orders by a validated reported time, so a later upstream port that reads
`node_declared_regions` must not mistake these rows for its own format.

The schema and every write live in `cmd/ingestor` (#1283). The ingestor creates the table with
`CREATE TABLE IF NOT EXISTS` at startup, so a legacy database gains it on the first start of this
version. The read-only server never creates it.

## Retention

`retention.clientRegionsDays` bounds the table by `reported_at`:

- If it is unset or `0`, the window is `retention.clientRxDays`, the same window as the other
  client-RX data. If that is unset too, the window is **30 days**. The table is never unbounded.
- The ingestor prunes in bounded batches (`id IN (SELECT … WHERE reported_at < ? LIMIT n)`), seeking
  `idx_cdr_reported`. It runs at startup and then once a day, whether or not the gate is on, so data
  stays bounded after the feature is switched off.
- The same window drives the ingest-time expiry check above.

## Erasure: `admin delete-client-rx`

`corescope-ingestor admin delete-client-rx --pubkey <64 hex>` also deletes every
`client_declared_regions` row whose `collector_pubkey` equals the key, in bounded batches through the
same queued, single-writer path. The match is `=`, never a prefix. `--dry-run` reports that count as
well, and reports 0 on a legacy database that does not have the table yet. The result file, the log
line and the audit record carry the count (`client_declared_regions=N`), never the key.

Rows where the key is the **target** are not deleted. They describe a repeater's public
configuration, not the contributor's movements.

## Read API

`GET /api/nodes/{pubkey}/declared-regions` (read-only; 404 while `clientRegions.enabled` is false)

```json
{
  "pubkey": "<64 hex>",
  "reports": [
    { "collector": 1, "regions": ["*", "#be"], "empty": false, "truncated": false,
      "reported_at": "2026-10-14T09:12:30.120Z", "ingested_at": "2026-10-14T09:12:31.004Z",
      "repeater_clock": 1760433150, "has_position": true }
  ],
  "collectors": 1,
  "truncated": false
}
```

- At most 100 reports, newest `reported_at` first. `truncated` means older reports exist.
- `collector` is an **opaque number, valid only within one response**. It numbers the collectors in
  order of first appearance, so you can see that two reports came from different companions. The
  collector's key, its position and the accuracy are **never served**: a collector key plus a
  target plus a time would place that person within direct radio range of a known repeater at a
  known moment.
- `empty: true` marks an explicit empty answer (`regions` is `[]`).
- A malformed key returns 400. A blacklisted or hidden node returns 404.
- **Startup race:** the server opens the database read-only and cannot create the table. If the
  ingestor creates it after the server's startup probe, the server's self-healing schema flag
  (`hasClientDeclaredRegions`, re-probed by the background healer every 2 s) picks it up without a
  restart. Until then the endpoint returns an empty list, never an error. The flag never stores
  "absent" permanently.

This endpoint is the consumer this collection feeds: an operator, or a later scope view, can check
what a repeater claims to flood and compare it with the scopes it is observed forwarding.

## Trust, spoofing and privacy

- **Broker ACL is required.** Bind `meshcore/client/{PUBLIC_KEY}/regions` so a client can publish
  only under its own key. Without that, anyone can publish reports under any collector key. If you
  read several brokers, list only the ACL-enforcing ones in `clientRxCoverage.sources`.
- **`target` and `regions` are not authenticated.** Even under a correct ACL, a collector can claim
  any target and any list. The report is not signed by the repeater. A report is *one collector's
  account* of an answer, not ground truth. The API keeps provenance per collector for that reason,
  and nothing in CoreScope treats a report as authoritative.
- **Location.** A report proves that the collector was within direct radio range of the target at
  `reported_at`, even with no GPS fix. CoreScope stores the collector key and position for provenance
  and erasure, but never serves them (see [Read API](#read-api)). Operators must still tell
  contributors that this data is collected and how long it is kept.
- **Retention and erasure** follow the other client-RX data: the window above, plus
  `admin delete-client-rx`. After an erasure, also blacklist the key, or a later upload re-inserts
  its reports (within the retention window).

## Enabling (operators)

1. Configure the broker ACL for `meshcore/client/{PUBLIC_KEY}/regions`. Sources with an explicit topic
   list must subscribe to `meshcore/client/+/regions`. The default `meshcore/#` covers it.
2. Set `"clientRegions": { "enabled": true }` in `config.json` and restart **both** processes. The
   ingestor gate controls the write path, and the server gate controls the read endpoint.
3. Optionally set `retention.clientRegionsDays`.

`clientRegions` is independent of `clientRxCoverage`. Either can be on without the other, but
`clientRxCoverage.sources` (the source allowlist) applies to both.

## Configurable values (future customizer)

Hardcoded for now and candidates for config later (AGENTS.md rule 8): the 15-minute future
tolerance, the 10 km accuracy ceiling, the 30-day default window and the 100-report API limit.
