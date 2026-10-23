# Observed Regions

The Regions page describes scoped relay traffic observed by CoreScope. Its
outline joins node positions into a convex hull; it is not a measurement of
radio coverage, a home-region assignment, or an administrative boundary.

## Time windows and evidence

The first milestone adds rolling 1-hour, 24-hour and 7-day selections. The
selected window and scope are shareable in the URL, for example
`#/regions?window=7d&scope=%23example`.

All periods use the same relay snapshot. The page shows when that snapshot
was generated and the start of the history known to be retained in memory.
A 7-day selection can therefore show partial history when only 24 hours are
available. Unknown or loading history must never be presented as a complete
period. Even a complete retained window does not prove uninterrupted radio
reception or observer uptime.

Relay identity and scope attribution reuse the existing relay-evidence rules.
The window is based on transmission `first_seen`, as in the existing relay
activity fields. It is not an observation-time replay and does not reconstruct
the historical position of a moving node. Each scope has its own most recent
relay timestamp so traffic in one scope cannot make another scope look active.

Coordinates are self-reported node positions. They are not independently
verified and have no inferred accuracy radius. Nodes without a usable position
remain in the activity totals but are omitted from markers and outlines.
Neighbour-estimated coordinates are not supplied by this milestone.

The endpoint reuses the background relay snapshot instead of scanning packet
history for each period change. Limits on nodes and scope evidence must be
reported as truncation, rather than silently suggesting a complete map.

## Timeline and period comparison

Latest retains the rolling-window view described above. Timeline reconstructs
activity in complete UTC hours and offers hourly steps through the last seven
days, plus local playback. The selected mode, period, scope, timestamp and
comparison state are shareable. Playback never starts automatically from a
link, stops at the newest frame, and stops when the page is hidden or left.

The history endpoint returns one bounded snapshot of sparse hourly counts.
Its 21-day horizon allows a seven-day interval ending seven days ago to be
compared with the preceding seven days, when that much history is retained.
This is a maximum display horizon; it does not extend database or memory
retention. Refresh can advance the snapshot. Scrubbing and playback do not
request data for each frame, and the map keeps its viewport while playing.

Intervals are half-open: `[start, end)`. A transmission exactly at the boundary
between two adjacent periods belongs only to the later period. The unfinished
current UTC hour is excluded from Timeline and remains visible in Latest.

Compare uses the immediately preceding interval of exactly the same duration.
It distinguishes relays observed only in the selected interval, only in the
previous interval, and in both. "Newly observed" describes this pair of periods;
it does not establish a node's creation date. "Not observed" does not establish
that a node is offline. Comparison always identifies both intervals.

Counts are distinct transmissions per relay and scope. Adding counts across
relays gives **relay appearances**: a packet carried by several relays can
contribute once for each relay. These totals must not be labelled globally
unique mesh packets. Traffic changes and classifications are withheld when
either interval has unknown, loading, partial or truncated evidence. Raw
observed counts may still be shown separately. A zero reference count has no
defined percentage increase.

The reconstruction uses transmission `first_seen`, the current identity
catalogue and the latest known node metadata. Archived metadata can be used
when no current node row exists; current metadata takes precedence, including
role changes. Positions are the latest known self-reported coordinates in
both periods, not measurements of historical movement. A refresh can revise
historical attribution as the identity catalogue changes.

History uses the same identity-safe scoped relay attribution as Latest.
Ambiguous prefixes and heuristic-only candidates do not become confirmed
relays merely because the view is historical. Current visibility rules apply
to both periods, including archived identities and observer aliases.

The history snapshot bounds populated bucket cells globally as well as nodes
and scopes. Every truncation is exposed. Its retained-history floor and loading
state are captured with the relay evidence, rather than taken from a newer
store state when a request arrives.

## Region activity health and traffic trends

**Health & trends** is a separate view, leaving Latest's rolling-window map
and Timeline's playback unchanged. It uses the same bounded history snapshot
as Timeline, completed UTC hours, interval selection and scope filter. The
view, selected hour, period, scope and comparison checkbox are shareable in
the URL, for example `#/regions?view=health&window=24h&scope=%23example`.
Switching between Health and Timeline preserves the selected hour. Health
does not play automatically and does not render the map.

The summary shows distinct relay nodes observed in the selected interval,
relay appearances and the most recent observed relay timestamp **within that
scope and interval**. Nodes without usable positions are included. A node
seen in several hours or scopes remains one node in the interval total;
hourly node counts must not be added to obtain that total. A missing last
relay timestamp means not observed in the interval, not an outage.

The traffic chart uses disjoint one-hour bins: one, 24 or 168 bins for the
1h, 24h and 7d periods. These are UTC hours, not local days or extrapolated
daily totals. The exact-values table provides the same timestamps and counts
as the chart. A zero is shown only for a fully retained, uncapped hour.
Partial, unknown, loading or capped evidence produces a gap, not a fabricated
zero; retained observations can still be disclosed explicitly as incomplete
evidence. The mean appearances/hour is available only when the entire
selected interval is complete and uncapped. Partial counts are never divided
by the full requested duration.

The optional comparison reuses Timeline's immediately preceding equal
interval and its completeness checks. A complete selected period may have
an hourly mean even when the reference is incomplete, but changes, percentages
and new/not-observed classifications still require **both** periods to be
complete and uncapped. A zero reference has no defined percentage increase.
The expected set for "not observed" is the preceding interval, not all nodes
ever present in the catalogue.

Health describes observed activity and evidence availability, not service
availability or radio quality. No healthy/degraded/offline score or alarm
threshold is inferred. More traffic can reflect spam, changed routing or
observer reception; it does not establish improved network health. The
snapshot timestamp and retained-history limitations remain visible.

The selected scope's hourly activity is indexed once per payload or scope
change into bounded arrays spanning the existing 504-hour horizon. Selecting
an interval reads at most 168 hourly bins and reuses the existing interval
comparison; it does not rescan raw packets, fetch each node or request a new
payload while scrubbing. No new endpoint, persistence, retention setting,
dependency or operator configuration is introduced. Any future health
thresholds must be evidence-based and exposed through the customizer.

## Following milestones

1. **Connections between regions:** add observed paths with direction and
   distinct-transmission counts. Preserve scope transitions, distinguish
   ambiguous hop identities and avoid interpreting shared scope membership as
   proof of a direct radio link. Route changes belong to this path-evidence
   milestone; timeline relay membership alone does not establish a changed route.

The default period and any future health thresholds or playback settings should
be exposed in the customizer in a later milestone. The first milestone uses a
24-hour default and existing theme variables, without a new operator config key.

## Verification

Backend tests cover distinct period selections, exact time boundaries, partial
retention, startup/load failures, memory eviction, safe relay attribution,
visibility rules, missing coordinates, truncation and reuse of the cached
snapshot. Browser tests cover period/scope URLs, browser navigation, pending
requests, failures, provenance labels, incomplete history, theme switching and
page teardown. Visual checks include narrow mobile and desktop layouts.

Timeline tests additionally cover hour boundaries, distinct-transmission
counts, adjacent equal-duration intervals, archived/current metadata precedence,
scope and global bucket limits, missing reference history, zero denominators,
stable map bounds, keyboard operation, no per-frame requests, and playback
cleanup on manual changes, hidden documents and navigation. Performance checks
cover background snapshot construction, bounded retained history and warm bulk
responses without acquiring the packet-store lock.

Health tests additionally cover cross-scope and cross-hour node deduplication,
scoped interval-specific last relay timestamps, complete-zero versus missing
bins, all coverage/truncation states, hourly means, comparison parity, fixed
UTC boundaries and no sparse-cell rescans after activity indexing. Browser
checks include Health deep links, local filter/time changes, Health/Timeline
transitions, missing-data charts, exact values, themes, teardown and narrow
mobile/desktop layouts.
