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

## Scope

Regions is limited to the period view described above. Timeline and period
comparison, health and trends, and connections between regions were added in
#502–#510 and removed again in #523: the retained route, membership, history
and evidence snapshot raised server memory and made every relay snapshot
recompute walk all transmissions and observations. Any return of those views
needs a design whose retained memory and recompute cost is measured before it
lands.

The default period should be exposed in the customizer in a later milestone.
The page uses a 24-hour default and existing theme variables, without a new
operator config key.

## Verification

Backend tests cover distinct period selections, exact time boundaries, partial
retention, startup/load failures, memory eviction, safe relay attribution,
visibility rules, missing coordinates, truncation and reuse of the cached
snapshot. Browser tests cover period/scope URLs, browser navigation, pending
requests, failures, provenance labels, incomplete history, theme switching and
page teardown. Visual checks include narrow mobile and desktop layouts.
