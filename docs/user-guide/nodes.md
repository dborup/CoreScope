# Nodes

The Nodes page lists every node your mesh has seen — repeaters, companions, rooms, and sensors.

[Screenshot: nodes list with status indicators]

## What you see

Each row shows:

- **Name** — the node's advertised name (or public key if unnamed)
- **Role** — Repeater, Companion, Room, or Sensor
- **Status** — color-coded health indicator
- **Last seen** — when the node was last heard
- **Advert count** — how many advertisements this node has sent

## Status indicators

| Indicator | Meaning |
|-----------|---------|
| 🟢 Active | Heard recently (within threshold for its role) |
| 🟡 Degraded | Not heard for a while but not yet silent |
| 🔴 Silent | Not heard for an extended period |

Thresholds differ by role. Infrastructure nodes (repeaters, rooms) have longer grace periods than companions. See [Configuration](configuration.md) for `healthThresholds`.

## Filtering

### Role tabs

Click **All**, **Repeaters**, **Rooms**, **Companions**, or **Sensors** to filter by role.

### Search

Type in the search box to filter by name or public key. The filter applies instantly.

### Status filter

Filter to show only active, degraded, or silent nodes.

### Area filter

If [areas are configured](area-filter.md), an area pill bar appears above the list. Selecting an area shows only nodes whose GPS position falls within that area.

### Last heard filter

Filter nodes by how recently they were heard (e.g., last hour, last 24h).

## Sorting

Click any column header to sort. Click again to reverse the order. Your sort preference is saved across sessions.

## Node detail

Click a node row to open the **detail pane** on the right. It shows:

- Full public key
- Role and status explanation
- Location (if known)
- Recent packets involving this node
- Neighbor nodes
- Signal statistics

Click the node name in the detail pane to open the **full node page** with complete history, analytics, and health data.

### Approximate area (neighbor estimate)

The **neighbor evidence area** shows the extent of supported neighboring nodes
with reported coordinates. The target **may be outside it**: this is not a
confidence region, radio-range boundary, triangulation, or GPS measurement.
Accuracy has not been field-validated. A reported GPS position keeps its own pin
and is never replaced by the estimate. Direct RSSI or four receivers are not
required; route/neighbor evidence is the basis.

The estimate considers a bounded pool of up to 20 positioned neighbor candidates.
It scores geographic groups using their combined support rather than choosing
the single busiest neighbor as an anchor. The lifetime observation count has a
capped, logarithmic weight; older link sightings receive less weight. One busy
link therefore cannot have unlimited influence. Candidates whose capped,
age-adjusted weight is below 10% of the strongest candidate's weight are
excluded from both the weighted center and the minimum-two-contributor check.
An almost negligible old link cannot turn a single strong neighbor into an
apparently supported pair. This uses persisted edge counts and `last_seen`,
**not** verified independent receivers, per-edge RF confidence,
RSSI ranging, or signal triangulation. Inferred links and neighbors' reported
positions can themselves be wrong.

The database still selects the top 20 links by lifetime count before filtering
for usable positions and freshness. Busy links without coordinates or with old
sightings can therefore keep better candidates outside the pool. Improving that
candidate selection is a separate follow-up, not an accuracy claim of this change.

- **Neighbor evidence area:** a polygon joining the outer supported contributors.
  Two or collinear positions show an evidence **line**, not an invented area.
  Coincident positions cannot provide geometry. No weighted-center pin is drawn.
- **Insufficient neighbor evidence:** the selected group contains fewer than two
  contributors, so no estimated position marker is shown.
- **Conflicting neighbor groups:** geographically separate groups have similar
  support; the estimator abstains instead of choosing a misleading point.
- **Unavailable:** there is no usable estimate.

The contributor count is relative to the selected candidate pool after this
relative-weight filter, **not all neighbors in the network**. Neighbor spread is
the greatest distance between the selected contributors; it is **not a location
error radius**. The link
sighting dates summarize contributors' stored `last_seen` values, not the dates
of every observation or when their GPS was measured. Missing or implausible
future timestamps are flagged as unknown freshness. The distance from a reported
position is a comparison between two points, not a measured positioning error.
The map uses a dashed hull/line, never an uncertainty circle or a guessed radius.
Hidden/blacklisted contributors and hidden observer aliases are excluded from
node-detail geometry; a failed visibility lookup abstains. Geometry is bounded
to 20 contributors and handles local antimeridian crossings.

This is a **relative** evidence rule, not an absolute freshness guarantee. Two
similarly old links can still support an estimate; read the displayed dates.
Neither an `estimated` status nor a pair of neighbors proves the node's current
position.

Clients connected to an older server show a **Legacy estimate; evidence
quality unavailable** explanation, without plotting its centroid as a precise
point or inventing an area. The conservative display
rules above apply to node detail; other legacy approximate path proxies are not
claimed to have the same evidence sufficiency.

The initial method (`neighbor_cluster_v1`) uses a 7-day freshness half-life, a
count cap of 20, an unknown-freshness multiplier of 0.25, a minimum relative
weight of 10%, and an ambiguity cutoff of 80% of the leading group's support.
These are heuristics, not calibrated probabilities. The distance limit uses the
existing 30 km estimate constant, measured from a group seed, not a bound on
location error or on every pair of contributors. Exposing these policy choices in the customizer
is a later milestone; this change does not add settings or a confidence score.

#### How to validate accuracy separately

1. Reserve a separate set of nodes with independently trustworthy positions and
   time windows before choosing thresholds. Do not tune on this held-out set.
2. Hide each target's position from the **entire** inference pipeline, including
   upstream geographic filtering and neighbor/prefix resolution. Merely omitting
   its coordinates from the final centroid would still leak the answer. If
   stored edges were already resolved with target GPS, rebuild them without that
   input or label the evaluation as contaminated.
3. Run old and new methods on exactly the same inputs and record abstentions.
   Compare median and p90 geodesic error, severe-outlier counts and distances,
   and coverage (fraction of eligible targets receiving an estimate). Report
   both common-target accuracy and overall coverage, so rejecting hard cases
   cannot masquerade as increased accuracy.
4. Stratify by neighbor count, age, competing groups, region, and observation
   density. Publish the sample sizes and limitations. Synthetic regression tests
   establish behavior, not real-world positioning accuracy.

## Favorites

Nodes you've claimed on the Home page appear as favorites. You can also star nodes directly from the Nodes page.

## Tips

- Use the search box for quick lookups — it matches partial names and keys
- Sort by "Last seen" descending to find the most active nodes
- The status explanation tells you exactly why a node is marked degraded or silent
