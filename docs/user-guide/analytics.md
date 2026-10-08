# Analytics

The Analytics page provides deep-dive charts and tables about your mesh network. Select a tab to explore different aspects.

[Screenshot: analytics page with tab bar]

## Overview

Summary dashboard with key network metrics at a glance. Quick sparklines and counts across all data dimensions.

## RF / Signal

Radio frequency analysis:

- **SNR distribution** — histogram of signal-to-noise ratios across all packets
- **RSSI distribution** — histogram of received signal strength
- **SNR by observer** — which observers are getting the best signals
- **Signal trends** — how signal quality changes over time

Use this to identify weak links or noisy observers.

## Topology

Network structure analysis:

- **Hop count distribution** — how many relay hops packets typically take
- **Top relay nodes** — which repeaters handle the most traffic
- **Node connectivity** — how well-connected each node is

## Channels

Channel message statistics:

- **Messages per channel** — which channels are most active
- **Channel activity over time** — traffic trends by channel
- **Top senders** — most active nodes per channel

## Hash Stats

Mesh hash size analysis:

- **Hash size distribution** — how many bytes nodes use for addressing
- **Hash sizes by role** — do repeaters use different hash sizes than companions?

## Hash Issues

Potential hash collision detection:

- **Collision pairs** — nodes whose short hash prefixes overlap
- **Risk assessment** — how likely collisions are at current hash sizes

Hash collisions can cause packet misrouting. If you see collisions here, consider increasing hash sizes on affected nodes.

## Route Patterns (Subpaths)

Common routing paths through the mesh:

- **Frequent subpaths** — which relay chains appear most often
- **Path reliability** — how consistently each path is used
- **Path detail** — click a subpath to see every packet that used it

## Nodes

Per-node analytics with sortable metrics across the fleet.

## Distance

Estimated distances between nodes based on GPS coordinates, correlated with signal quality.

## Neighbor Graph

Interactive visualization of which nodes can directly hear each other. Shows the mesh topology as a network graph.

## RF Health

Per-observer signal health over time. Identifies observers with degrading reception.

## Scopes: Scope Audit

Open **Scopes → Scope Audit** to compare each node's confirmed observer declaration with flood forwarding seen in the last **1h, 24h or 7d**. The audit covers the whole network; its own window controls the observations, independently of the global analytics filters.

The table shows declared regions, observed regions and counts, unscoped traffic, undeclared regions and unscoped forwarding without a declared `*`. Search by node name, public key or declared region. Search, window and pagination are shareable, for example `#/analytics?tab=scopes&sub=audit&swin=7d&saq=dk`.

**Not observed** means no attributable forwarding was recorded in this window. It does not prove that the node is configured incorrectly: quiet regions, limited observer coverage, hop limits and configuration changes can explain the gap. Declarations older than seven days, unknown declaration timestamps, ambiguous hop prefixes, unknown scoped traffic or legacy observations without their own raw header make the evidence incomplete. A node with no attributable forwarding is shown as **No forwarding evidence**, never as consistent.

Only nodes with a confirmed declaration are included. An unanswered node is different from an answered empty list. Observer firmware exports flood-allowed regions; an empty list therefore means no flood-allowed regions, rather than proving that the node has no region definitions. Observer reports can omit names when their response buffer fills, without reporting truncation. Treat undeclared forwarding as a finding to investigate.

Counts deduplicate a node/transmission pair across observers and repeated path hops. A transmission observed under several scope states contributes to each state's count, so those counts can sum above the unique forwarded total. Direct routes are excluded. Only an exact identity or uniquely matching supported hop prefix is attributed; heuristic path resolution is not evidence.

This first version uses observer declarations already collected by CoreScope. It does not collect configuration itself, ingest CoreDrive declarations or verify unknown region codes against declared names.

### Initial limits and customizer follow-up

The initial defaults are 50 visible rows, a 150ms search debounce, the existing 60s refresh interval, a seven-day declaration-age threshold, a 30s backend cache and a 20s query deadline. Expose operator-adjustable display and freshness limits in a later customizer milestone. The resolver memo is capped at 4096 tokens and the cache at the three supported windows; those implementation bounds stay internal.

## Prefix Tool

Test hash prefix lengths to see how many collisions different sizes would produce. Useful for deciding on hash_size settings.

## Region filter

All analytics tabs respect the **region filter** at the top. Select a region to scope the data to observers in that area.

## Area filter

If [areas are configured](area-filter.md), an area pill bar also appears. Selecting an area scopes all analytics to nodes whose GPS position falls within that area. This is based on the transmitting node's own coordinates — not the observer's location — so it avoids cross-region pollution from distant observers.

## Deep linking

Each tab is deep-linkable. Share a URL like `#/analytics?tab=collisions` to point someone directly at hash issues.
