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

## Following milestones

1. **Timeline and comparison:** introduce historical snapshot selection and
   playback, then compare equal, explicitly bounded periods. Show newly observed
   and no-longer-observed relays and changed routes. Distinguish missing history
   from an observed absence. A comparison must identify its two periods and the
   position basis used for each.
2. **Region health:** add active relay counts, last relay and traffic trends
   over comparable periods. Quiet relays should be labelled as not observed in
   the period; lack of reception alone does not establish that a node is offline.
3. **Connections between regions:** add observed paths with direction and
   distinct-transmission counts. Preserve scope transitions, distinguish
   ambiguous hop identities and avoid interpreting shared scope membership as
   proof of a direct radio link.

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
