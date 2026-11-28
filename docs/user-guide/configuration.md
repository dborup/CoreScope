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

The three opt-in settings (`inactiveNodeDays`, `nodeChangeDays`,
`observerPurgeDays`) cover the tables nothing else prunes, so an instance can
honour a fixed retention period for node and observer data. The ingestor
applies them at startup and then daily, in small batches, and logs each count,
for example `[prune] deleted 12 node_changes older than 30 days`. Leaving them
unset keeps today's behaviour.

- A node deleted by `inactiveNodeDays` that adverts again later counts as a new
  node: it shows in New Nodes, and Node Changes records no return. Set it above
  `nodeDays`.
- `observerPurgeDays` deletes an observer only after its packets, metrics and
  dropped packets have aged out, so set it above `observerDays`, `packetDays`
  (and `channelDays`) and `metricsDays`; below those, an observer waits until
  that data is gone. The observer's current neighbour list is deleted with it.
- Ping Scores triggers (`ping_triggers`, which hold sender names) have no
  setting and are never deleted. The all-time Ping Scores records need them:
  a ping stays in those records after `packetDays` has removed its packet, but
  only while its trigger exists.

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

## Owner antenna heights

`antennaClaims` lets a node's owner set its antenna height for path profiles. The owner gets a 6-character code on the node page, puts it into the node's advert name and sends an advert; the advert's signature proves control of the node. Owners follow [Setting your node's antenna height](antenna-height-claims.md). The server and the ingestor read the same block; changing it requires a restart of both.

| Field | Default | Description |
|-------|---------|-------------|
| `enabled` | `false` | Opens public claims. Only takes effect with a strong `apiKey` (16+ characters, not a placeholder). |
| `detailsEnabled` | `false` | Also lets owners set station details: antenna type, gain, power, installation and two optional contacts (see [Station details](#station-details) below). Needs `enabled` and a strong `apiKey` too. Off, a request that sets details is refused with 403, but one that only removes details is still accepted by the API (the page offers no *Remove* while the switch is off). Details already stored are kept but not served, and an administrator can still revoke them. |
| `autoApproveProven` | `false` | Approves a proven height without an administrator, unless it is flagged (see `reviewDeltaM`). Only repeater and room claims with a cached surface estimate can be flagged. Covers claims that set no station details; see `autoApproveDetails` for those. |
| `autoApproveDetails` | `false` | Approves a proven claim that sets station details without an administrator: all of it, the height, the details and both contacts. A flagged claim still waits. Needs `detailsEnabled` (otherwise it has no effect and the ingestor logs a warning). **The public contact is free text and goes public without review; revoke is the only remedy.** See [Station details](#station-details). |
| `submissionsPerHour` | `20` | Global limit on new claims per hour. |
| `maxPending` | `256` | Claims waiting for an advert, over all nodes. A claim over the limit is refused; it never evicts another. |
| `maxPendingPerNode` | `3` | Claims waiting for an advert, per node. An administrator can reject unproven claims under "Waiting for advert". |
| `maxQueuedRequests` | `256` | Requests waiting for the ingestor in the queue directory next to the database. |
| `retentionDays` | `30` | Finished claims are deleted after this many days. The approved one is kept. |
| `staleMoveM` | `50` | Metres a node may move from where its height was proven. Further away, the height is not used (nothing is deleted) and the admin queue marks it stale. Range above 0 to 5000. |
| `reviewDeltaM` | `60` | Metres above the cached surface estimate + 2 m at which a height is flagged for review. A height more than 2 m below the surface is always flagged. Range above 0 to 1000. |
| `pollInitialMs` | `2000` | The owner page's first wait before it checks a claim's status. Range 500–60000. |
| `pollBackoff` | `1.5` | Each later wait is this many times the one before. Range 1–4. |
| `pollMaxMs` | `30000` | The longest single wait. Never below `pollInitialMs`; at most 600000. |
| `pollMaxAttempts` | `60` | Status checks before the page stops and offers **Check again**. At most 1000. |
| `adminDefaultFilter` | `proven` | The admin queue's first view (`#/tools/antenna-claims`): `proven`, `pending`, `approved`, `rejected`, `revoked`, `expired` or `superseded`. |

A value outside its range falls back to its default. The page reads the last five from `GET /api/antenna-claims/config`, so they need only a server restart.

The switches `enabled`, `detailsEnabled`, `autoApproveProven` and `autoApproveDetails` are read by their exact spelling, like the other opt-in gates: `DetailsEnabled`, `AutoApproveDetails`, or a top-level `AntennaClaims`, is ignored and logged, even after a canonical `false`. The page learns from the same route whether to show the station form (`detailsEnabled`); the route and the admin list also report `autoApproveDetails`.

### Station details

With `detailsEnabled`, a claim can carry station details next to the height, under one code. Owners follow [Station details](antenna-height-claims.md#station-details). What an operator needs to know:

- **Review.** An administrator approves or rejects the whole claim in the admin queue. A claim that sets details is never approved by `autoApproveProven`, because the public contact is free text. A claim that only removes the height or the details takes effect at once after its advert.
- **Auto-approval of details (opt-in).** With `autoApproveDetails` (and `detailsEnabled`), a proven claim that sets details is approved at its advert, without an administrator, all fields included. A flagged claim (a height far from the surface estimate, or a gain outside 0–15 dBi) still waits, whole. The admin queue lists these claims as approved by `auto`, and an administrator can revoke them as usual. Claims proven before the switch was turned on stay in the queue. Changing the switch needs an ingestor restart.

  > **Warning:** with `autoApproveDetails` on, the public contact (`contactPublic`) is free text that anyone who controls a node can publish on that node's page without review. Revoking the claim is the only remedy.

  The two switches are independent: `autoApproveProven` covers claims without details, `autoApproveDetails` claims with them. For every unflagged claim to go through without an administrator, set both to `true`. `autoApproveDetails` alone still leaves height-only claims in the queue.
- **Public.** Antenna type, gain, power, installation and the optional public contact are served to everyone by `GET /api/nodes/{pubkey}/station` and shown on the node page. They are not in node cards, link previews, share images, `/api/nodes` or exports, and path profiles do not use them yet.
- **Admin only.** The administrator contact is stored in its own table and read only by the admin queue (`GET /api/admin/antenna-claims`), for proven and approved claims. It is in no public response and is never logged.
- **Deletion.** Both contacts are deleted when the details are removed, replaced, rejected, revoked or expired, and when retention deletes the claim. See the [owner guide](antenna-height-claims.md#when-the-details-are-deleted).
- **Stale.** Details are not served once the node has moved more than `staleMoveM` from where they were proven. Portable stations too.
- **Fixed limits.** The gain range (−5 to 20 dBi in 0.1 steps, with review outside 0–15 dBi) and the contact length (32 characters) are fixed in code and enforced by the database. They are not configurable.
- **Backups and logical deletion.** `GET /api/backup` and `scripts/backup-sqlite.sh` copy the whole database, contacts included, while they exist. See [Deployment → Owner contacts in backups](../deployment.md#owner-contacts-in-backups).

An approved height ranks below a per-node height in `radio.antennaHeightsM` and above the surface estimate and the role heights in `radio.antennaHeightsM`. In full, a profile takes the first of: the request (a typed height or a link), the operator's height for the node, the owner's approved height, the surface estimate (only where it is above the role height), the operator's height for the role, the built-in role default. A height a viewer types or opens from a link still wins for that viewer, and each viewer can turn **Prefer owner heights** off in the customizer (see [Customization](customization.md#terrain-profile-display-tab)).

## Owner accounts

`accounts` lets an owner sign in with their companion radio and keep **My Mesh** on every device (#1018 M1). There is no password and no e-mail: the owner gets a 6-character code on `#/account`, puts it in the companion's name and sends a **flood** advert; a new advert with a valid signature from that companion proves the sign-in. Only a companion (an `ADV_TYPE_CHAT` advert) can sign in; a repeater, room or sensor cannot. A blacklisted key is refused like an unknown node; a node hidden only by a `hiddenNamePrefixes` prefix may sign in. Owners follow [Your owner account](owner-account.md). The server and the ingestor read the same block; changing it requires a restart of both.

| Field | Default | Description |
|-------|---------|-------------|
| `enabled` | `false` | Opens sign-in. Only takes effect with a strong `apiKey` (16+ characters, not a placeholder). Read by its exact spelling. With it on, the ingestor creates the session key file at its start. |
| `sessionDays` | `30` | How long a session lasts. It is renewed on use once it is a day old. At most 90, and never more than `sessionMaxDays`. |
| `sessionMaxDays` | `90` | The hard cap: no session lasts longer than this after the sign-in, renewed or not. At most 365. |
| `loginCodeMinutes` | `15` | A sign-in code's lifetime. At most 60. |
| `maxMyNodes` | `200` | Nodes on one account's list. At most 1000. |
| `loginsPerHour` | `120` | Global sliding window on new sign-ins. This is the limit that holds in the shipped Docker setup, where per-client limits cannot tell clients apart. |
| `loginsPerMinutePerIP` | `2` | Per-client limit on new sign-ins. Only enforced when `webSocket.trustedProxies` names your reverse proxy. |
| `writesPerHour` | `600` | Global sliding window on account changes (list, display name, sign-out-everywhere, delete). |
| `maxPendingLogins` | `512` | Unproven sign-ins over all companions. A sign-in over the limit is refused; it never evicts another. |
| `maxPendingPerKey` | `8` | Unproven sign-ins per companion. |
| `maxQueuedRequests` | `256` | Requests waiting for the ingestor in `account-requests/` next to the database. |
| `inactiveMonths` | `24` | Accounts without a sign-in for this long are deleted with their list (owner decision 9). At most 120. |

A value outside its range falls back to its default.

**The session.** The cookie is `__Host-mv_session`: `HttpOnly`, `Secure`, `SameSite=Strict`, `Path=/`. Browsers drop a `Secure` cookie on plain `http://` (except `localhost`), so serve the site over HTTPS. Every change (`POST`, `DELETE`) must come from the site itself: a missing or foreign `Origin` is refused with 403. Behind a proxy that rewrites `Host`, pass `X-Forwarded-Host`.

**The key file.** The ingestor creates `account-session.key` (32 random bytes, mode 0600) next to the database. The server only reads it and refuses one that other users can read. It is never logged or served, and `GET /api/backup` does not include it (the backup is the database alone). Replacing it and restarting both processes signs every account out. Until it exists, `GET /api/account/config` reports `available: false` and sign-in answers 503.

**What is stored.** `accounts` (a random account id, the companion's public key, an optional display name, the session generation, the first and last sign-in times), `account_proofs` (one row per sign-in code, deleted a day after it ran out) and `account_my_nodes` (the list). No IP address or user agent. The companion key is never served publicly; the account itself sees its first 8 characters.

**Customizer.** None of these are visitor preferences; they stay operator settings in `config.json` (rule 8: a later milestone may expose display-only values).

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

> **Warning:** Leaving `retentionHours` at `0` on a large database will cause the server to OOM-kill itself on every cold start. The full packet history is loaded into memory at startup, with its per-packet indexes.

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

## Moments

Off by default. The detection thresholds, the rate limit and the history size
are the operator's; a viewer only chooses how the cards look (Customize →
Display). Full reference, including every range, in [Live Moments](../moments.md).

| Field | Default | Description |
|-------|---------|-------------|
| `moments.enabled` | `false` | Turn the detector, the `moment` WebSocket frames and `/api/moments` on |
| `moments.backOnlineHours` | `24` | Silence before a repeater counts as back online |
| `moments.recordWindowHours` | `24` | Rolling window for a record link |
| `moments.recordMinKm` | `5` | Shortest link that can be a record |
| `moments.busyWindowMinutes` | `10` | Window for the busiest repeater |
| `moments.busyMinRelays` | `20` | Least relays the busiest repeater needs |
| `moments.maxPerMinute` | `6` | Global rate limit (burst 3) |
| `moments.historySize` | `50` | Moments kept for `/api/moments` (in memory) |

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
