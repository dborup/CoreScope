# Configuration

CoreScope is configured via `config.json` in the server's working directory. Copy `config.example.json` to get started.

## Core settings

| Field | Default | Description |
|-------|---------|-------------|
| `port` | `3000` | HTTP server port |
| `apiKey` | — | Secret key for admin API endpoints (POST/PUT routes) |
| `dbPath` | — | Path to SQLite database file (optional, defaults to `meshcore.db`) |

## MQTT

```json
"mqtt": {
  "broker": "mqtt://localhost:1883",
  "topic": "meshcore/+/+/packets"
}
```

The ingestor connects to this MQTT broker and subscribes to the topic pattern.

### Multiple MQTT sources

Use `mqttSources` for multiple brokers:

```json
"mqttSources": [
  {
    "name": "local",
    "broker": "mqtt://localhost:1883",
    "topics": ["meshcore/#"]
  },
  {
    "name": "remote",
    "broker": "mqtts://mqtt.example.com:8883",
    "username": "user",
    "password": "pass",
    "topics": ["meshcore/SJC/#"]
  }
]
```

## Branding

| Field | Description |
|-------|-------------|
| `branding.siteName` | Site title shown in the nav bar |
| `branding.tagline` | Subtitle on the home page |
| `branding.logoUrl` | URL to a custom logo image |
| `branding.faviconUrl` | URL to a custom favicon |

## Theme

Colors used throughout the UI. All values are hex color codes.

| Field | Description |
|-------|-------------|
| `theme.accent` | Primary accent color (links, buttons) |
| `theme.navBg` | Navigation bar background |
| `theme.navBg2` | Secondary nav background |
| `theme.statusGreen` | Healthy status color |
| `theme.statusYellow` | Degraded status color |
| `theme.statusRed` | Silent/error status color |

See [Customization](customization.md) for the full list — the theme customizer exposes every color.

## Node colors

Default marker colors by role:

```json
"nodeColors": {
  "repeater": "#dc2626",
  "companion": "#2563eb",
  "room": "#16a34a",
  "sensor": "#d97706",
  "observer": "#8b5cf6"
}
```

## Health thresholds

How long (in hours) before a node is marked degraded or silent:

| Field | Default | Description |
|-------|---------|-------------|
| `healthThresholds.infraDegradedHours` | `24` | Repeaters/rooms → degraded after this many hours |
| `healthThresholds.infraSilentHours` | `72` | Repeaters/rooms → silent after this many hours |
| `healthThresholds.nodeDegradedHours` | `1` | Companions/others → degraded |
| `healthThresholds.nodeSilentHours` | `24` | Companions/others → silent |

## Retention

| Field | Default | Description |
|-------|---------|-------------|
| `retention.nodeDays` | `7` | Nodes not seen in N days move to inactive |
| `retention.packetDays` | `30` | Packets older than N days are deleted daily |
| `retention.channelDays` | `0` | Channel messages (GRP_TXT) and their observations are kept until they are N days old instead of `packetDays`. Takes effect only when `packetDays` is set and `channelDays` is larger; `0` = channel messages follow `packetDays` |
| `retention.inactiveNodeDays` | `0` | Rows in `inactive_nodes` whose last advert is older than N days are deleted, unless the node has come back or is an observer still uploading. `0` = keep forever |
| `retention.nodeChangeDays` | `0` | Node Changes history (`node_changes`) older than N days is deleted. `0` = keep forever |
| `retention.pingTriggerDays` | `0` | Ping Scores triggers (`ping_triggers`, which hold sender names) first seen more than N days ago are deleted. `0` = keep forever |
| `retention.observerPurgeDays` | `0` | Observers already marked inactive by `observerDays` and not seen in N days are deleted, once no packet, metric or dropped-packet row references them. `0` = keep forever |

`retention.channelDays` lets an instance keep a short `packetDays` to bound the
database while keeping chat history longer. Channel messages are a small share
of all traffic, so the extra rows are cheap. The ingestor prunes at startup and
then daily, and logs both prunes separately, for example
`[prune] startup pruned 120 channel messages older than 90 days`. A value that
is not larger than `packetDays` has no effect, and the ingestor logs that at
startup.

The server's in-memory packet store window (`packetStore.retentionHours`) is
independent of both settings. The Channels page reads the full history from the
database (`/api/channels`, `/api/channels/{hash}/messages`), so messages kept by
`channelDays` stay visible there even when they are older than the in-memory
window.

The four opt-in settings (`inactiveNodeDays`, `nodeChangeDays`,
`pingTriggerDays`, `observerPurgeDays`) cover the tables nothing else prunes, so
an instance can honour a fixed retention period for node and observer data. The
ingestor applies them at startup and then daily, in small batches, and logs
each count, for example `[prune] deleted 12 node_changes older than 30 days`.
Leaving them unset keeps today's behaviour.

- A node deleted by `inactiveNodeDays` that adverts again later counts as a new
  node: it shows in New Nodes, and Node Changes records no return. Set it above
  `nodeDays`.
- `pingTriggerDays` counts from when the ping was first seen, not from when its
  packet was pruned. Ping Scores keeps a ping in its all-time records after
  `packetDays` has removed the packet, and drops it, with its sender name, once
  the trigger is deleted. So `pingTriggerDays` also bounds those records.
- `observerPurgeDays` deletes an observer only after its packets, metrics and
  dropped packets have aged out, so set it above `observerDays`, `packetDays`
  (and `channelDays`) and `metricsDays`; below those, an observer waits until
  that data is gone. The observer's current neighbour list is deleted with it.

> **Note:** Lowering retention does **not** immediately shrink the database file.
> SQLite marks deleted pages as free but does not return them to the filesystem
> unless [incremental auto-vacuum](database.md) is enabled. New databases created
> after v0.x.x have auto-vacuum enabled automatically. Existing databases require
> a one-time migration — see the [Database](database.md) guide.

## Database

| Field | Default | Description |
|-------|---------|-------------|
| `db.vacuumOnStartup` | `false` | Run a one-time full `VACUUM` on startup to enable incremental auto-vacuum (blocks for minutes on large DBs) |
| `db.incrementalVacuumPages` | `1024` | Free pages returned to the OS after each retention reaper cycle |

See [Database](database.md) for details on SQLite auto-vacuum, WAL, and manual maintenance.
See [#919](https://github.com/Kpa-clawbot/CoreScope/issues/919) for background.

### Resolved-path backfill (ingestor)

Once per ingestor start, observations stored with `resolved_path = NULL` are resolved again in small batches. The pass waits until the neighbour-edge build has caught up with the stored observations. The server sees the new values after its next restart.

| Field | Default | Description |
|-------|---------|-------------|
| `resolvedPathBackfill.disabled` | `false` | Skip the pass |
| `resolvedPathBackfill.batchSize` | `500` | Rows per batch; `0` means the default |
| `resolvedPathBackfill.pauseMs` | `250` | Milliseconds between batches; `0` means the default, so the pause cannot be turned off (minimum `1`) |

## Channel decryption

| Field | Description |
|-------|-------------|
| `channelKeys` | Object of `"label": "hex-key"` pairs for decrypting channel messages |
| `hashChannels` | Array of channel names (e.g., `"#LongFast"`) to match by hash |

See [Channels](channels.md) for details.

### Shared channel suggestions

`channelProposals` lets visitors suggest public hashtag channels for everyone. By default an administrator approves them; operators can opt into automatic approval of new names. The server and the ingestor read the same block.

| Field | Default | Description |
|-------|---------|-------------|
| `enabled` | `false` | Opens public suggestions. Only takes effect with a strong `apiKey` (16+ characters, not a placeholder). |
| `autoApprove` | `false` | When `enabled` is true, the ingestor immediately approves *brand-new* valid names. Existing pending, rejected and revoked names are never auto-approved. Changing this policy requires an ingestor restart. |
| `maxPending` | `100` | Suggestions waiting for review. Further suggestions are refused until some are reviewed. |
| `maxApproved` | `128` | Shared channels that can be approved. |
| `maxQueuedRequests` | `256` | Requests waiting for the ingestor in the queue directory next to the database. |
| `retentionDays` | `30` | Rejected and never-reviewed suggestions are deleted after this many days. Approved channels are kept. Until its rejection is deleted, a rejected name cannot be suggested again. |
| `submissionsPerHour` | `20` | Global limit on new suggestions per hour. It is global rather than per visitor, because behind a reverse proxy every visitor can share one address. |

Approved channels stay decrypted and listed when `enabled` is later set to `false`, and survive restarts and `SIGHUP` reloads. A key configured in `channelKeys` for the same name takes priority, and so does the rainbow table (`channel-rainbow.json`): approving or revoking one of those built-in names changes nothing, and the review dialog marks them (see [Channels](channels.md#built-in-names)).

Auto-approval uses the same name validation, global submission rate limit, queue and `maxApproved` cap as manual approval. At capacity, a new suggestion fails rather than becoming an unapproved row. Be careful on a public instance: visitors can fill the approved-channel allowance. Rejected and revoked names remain protected from automatic re-approval while their rows exist. Retention eventually removes these rows, so an old name can be proposed as new again; a permanent blocklist is not provided. Turning off `autoApprove` affects new submissions only and does not revoke already approved channels.

An administrator can also revoke a previously approved channel (see [Channels](channels.md#revoking-an-approved-channel)) — this undoes the decryption going forward and takes the channel out of the channel list, but never deletes messages already decoded while it was approved (they return with the channel if it is approved again; a name the ingestor decrypts through its built-in/config list is never hidden). The channel also stays out of the list while the name is re-suggested (pending) or that re-suggestion is rejected; only an approval lists it again. Once the row is pruned by `retentionDays`, a channel whose messages are still stored shows in the list again. A revoked row is retained and pruned by the same `retentionDays` rule as a rejected one (counted from when it was revoked, not when it was first submitted); an approved row is still never pruned. Revoking introduces no new configuration of its own — it reuses the `maxPending`/`maxApproved`/`retentionDays`/`submissionsPerHour` limits above.

## Map defaults

```json
"mapDefaults": {
  "center": [37.45, -122.0],
  "zoom": 9
}
```

Initial map center and zoom level.

## Regions

```json
"regions": {
  "SJC": "San Jose, US",
  "SFO": "San Francisco, US"
}
```

Named regions for the region filter dropdown. The `defaultRegion` field sets which region is selected by default.

## Cache TTL

All values in seconds. Controls how long the server caches API responses:

```json
"cacheTTL": {
  "stats": 10,
  "nodeList": 90,
  "nodeDetail": 300,
  "analyticsRF": 1800
}
```

Lower values = fresher data but more server load.

## Packet store

| Field | Default | Description |
|-------|---------|-------------|
| `packetStore.maxMemoryMB` | `1024` | Maximum RAM for in-memory packet store |
| `packetStore.estimatedPacketBytes` | `450` | Estimated bytes per packet (for memory budgeting) |
| `packetStore.retentionHours` | `0` | Only load packets younger than N hours on startup and keep them in memory. **Set this on any instance with a large DB.** `0` = unlimited (loads full DB history — causes OOM on cold start when the DB has hundreds of thousands of paths). Recommended: same as `retention.packetDays × 24` (e.g. `168` for 7 days). |

> **Warning:** Leaving `retentionHours` at `0` on a large database will cause the server to OOM-kill itself on every cold start. The full packet history is loaded into the subpath index at startup; a DB with ~280K paths produces ~13M index entries before the process is killed.

## Timestamps

| Field | Default | Description |
|-------|---------|-------------|
| `timestamps.defaultMode` | `"ago"` | Display mode: `"ago"` (relative) or `"absolute"` |
| `timestamps.timezone` | `"local"` | `"local"` or `"utc"` |
| `timestamps.formatPreset` | `"iso"` | Date format preset |

## Live map

| Field | Default | Description |
|-------|---------|-------------|
| `liveMap.propagationBufferMs` | `5000` | How long to buffer observations before animating |

## HTTPS

```json
"https": {
  "cert": "/path/to/cert.pem",
  "key": "/path/to/key.pem"
}
```

Provide cert and key paths to enable HTTPS.

## Geographic filtering

```json
"geo_filter": {
  "polygon": [[51.55, 3.80], [51.55, 5.90], [50.65, 5.90], [50.65, 3.80]],
  "bufferKm": 20
}
```

Restricts ingestion and API responses to nodes within the polygon plus a buffer margin. Remove the block to disable filtering. Nodes with no GPS fix always pass through.

Can also be configured live via the **🗺️ GeoFilter** tab in the Customizer (requires `apiKey`).

See [Geographic Filtering](geofilter.md) for the full guide.

## Areas

```json
"areas": {
  "BAY": {
    "label": "Bay Area",
    "polygon": [[37.90, -122.55], [37.90, -121.75], [37.25, -121.75], [37.25, -122.55]]
  },
  "SJC": {
    "label": "San Jose",
    "latMin": 37.20, "latMax": 37.45, "lonMin": -122.05, "lonMax": -121.75
  }
}
```

GPS-based display filter. When configured, a pill bar appears in the dashboard letting users scope packets, nodes, and analytics to nodes physically located within a named area. Attribution is based on the transmitting node's own GPS coordinates — not the observer's location.

Each entry supports a `polygon` (array of `[lat, lon]` pairs) or a bounding box (`latMin`/`latMax`/`lonMin`/`lonMax`). Remove the block to disable the area filter UI.

See [Area Filter](area-filter.md) for the full guide including the visual builder tool.

## Home page

The `home` section customizes the onboarding experience. See `config.example.json` for the full structure including `steps`, `checklist`, and `footerLinks`.
