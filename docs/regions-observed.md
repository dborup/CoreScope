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

Relay identity and scope attribution require each observation's own scoped
transport-flood header, with both known transport codes matching the stored
canonical scoped frame and a nonzero scope code. Direct planned paths, missing
own-frame evidence and mismatched transport codes never acquire membership.
The canonical transmission may itself be direct: a valid flood observation of
that same content still supplies evidence. A single resolved flood hop is
sufficient for membership; a pair of consecutive hops is not required.
Observations of a scoped transmission that do not supply membership are
counted in `coverage.evidence`, per stored scope and `FirstSeen` hour, in
three classes:

- **excluded**: identified as not eligible, namely a known DIRECT or unscoped
  header, or own transport codes that differ from the stored frame. Forwarders
  never rewrite transport codes (firmware `Mesh::sendFlood`/`sendZeroHop` set
  them only at origin), so a mismatch is a frame for another scope code.
  Exclusions never make counts incomplete.
- **unprovable**: the own header, own transport codes or canonical scope code
  are unknown or malformed, so eligibility cannot be decided.
- **capped**: observations, or per-transmission relay memberships, omitted by a
  safety limit. Directed-step caps are reported in `stepEvidence` instead.

Unprovable or capped evidence makes only its own hour and scope incomplete:
rates, chart values and comparisons are withheld for intervals that include
an affected hour of the selected scope (or of any scope for "all scopes"),
and remain available elsewhere. If the per-bucket evidence budget (128 scopes
x 505 hours) is exhausted, `coverage.evidence.truncated` withholds every rate
and comparison. Hop tokens without a unique relay identity are counted in
`stepEvidence.unresolvedHops`; they break directed-step adjacency but do not
qualify the memberships of uniquely resolved relays. A fully retained interval
is not necessarily fully attributable; retained observed counts stay
explicitly qualified.
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
relays gives **relay appearances**: one transmission counts once for each
distinct relay identified on **any** observer's own attributable scoped-flood
path, the union across all observers, not only the displayed path. Repeated
observers, identical paths and loops count once. Before #507 history was
attributed through the canonical display path and its candidate index, so
relays seen only on other observers' paths were not counted; appearance totals
therefore rise where several observers hear the same transmission along
different paths. More observers can reveal more relays for the same traffic.
These totals must not be labelled globally unique mesh packets. Traffic
changes and classifications are withheld when
either interval has unknown, loading, partial or truncated history, or
unprovable or capped evidence for the selected scope. Raw
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
selected interval is complete, uncapped and has no unprovable or capped
evidence for the selected scope. Partial counts are never divided
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

## Connections and observed routes

**Connections & routes** separates two evidence classes instead of drawing
inferred traffic links between region names:

- **Shared relay-history memberships** counts distinct visible identities
  with attributable scoped flood observations in both scopes during the
  selected interval. This is an undirected history relationship. It does not prove that a
  packet crossed between those scopes, that the identity is a gateway, or that
  the scopes are neighbouring geographic areas.
- **Observed directed relay steps** counts consecutive, unambiguously resolved
  relay identities within a single observed flood path, labelled with that
  observation's attributable packet scope. Opposite directions remain separate.
  Repeated observers or loops contribute at most once per transmission, directed
  step and scope. Counts across steps are step appearances, not globally unique
  transmissions. Independent steps are never concatenated into a claimed full
  route.

Shared memberships, Timeline, Health and Latest use the same strict
own-observation attribution. Mixed flood/direct observations cannot promote
planned direct paths into scope history. Repeated paths, observers and loops
contribute at most once per transmission, relay identity and scope. This
corrects the older transmission-level history: counts can decrease when
previously accepted paths cannot be proven, or increase where a valid flood
observation was previously hidden by a canonical direct transmission. No stored
packets are changed or deleted. Generic node liveness, relay statistics and
traffic-share calculations are deliberately unchanged by this Regions-only fix.

Flood forwarding appends the forwarding identity to the path. Direct paths
instead name remaining intended hops, so direct observations are not evidence
of already-traversed steps. TRACE and ADVERT packets are excluded from this
traffic view. Unresolved, colliding, invalid or listener-only hop identities
break adjacency: removing an unknown intermediate hop must not manufacture
a connection between the endpoints on either side. No origin-to-first-hop or
last-hop-to-observer edge is inferred from generic packet metadata. Observer
RSSI/SNR is not a measurement of the internal steps.

One content transmission can have observations with different route types or
transport codes. A named-scope step therefore requires the observation's own
explicit transport-flood header and both transport codes matching the stored
canonical scoped frame. Missing legacy headers and mismatched or malformed
transport evidence are omitted rather than falling back to the transmission's
route. This reuses the stored scope attribution; it does not independently
authenticate the region name. The scope is a property of the observed packet,
not a home-region assignment to either relay.

The wire path does not label each hop with a region, and the reserved second
transport code is not a destination-region identifier. Shared relays, current
declared/default scopes and the order in which listeners reported a packet
cannot establish a scope-change point. This milestone intentionally does not
claim directional region-to-region crossings. Such a view needs additional
explicit crossing evidence, not geographic or membership guesses.

Connections uses the same completed UTC hours, transmission `first_seen`
assignment and current-knowledge identity reconstruction as Timeline and
Health. Retained-history limits remain visible. Latest remains a rolling view;
switching to Connections does not silently turn it into observation-time
replay. Missing coordinates do not remove a step from the evidence table;
only usable self-reported endpoint positions can be mapped. Lines describe
observed path order, not independently verified direct RF links or coverage.

The view is shareable with `view=connections`, `window`, `scope` and `at`.
Directed-step and shared-pair table pages use `page` and `pairsPage` respectively.
Connections has no automatic playback or route-comparison control. It can use
the paired history payload when switching to Health or Timeline, without
making a request for every selected hour.

Route evidence is published with the existing background relay snapshot, not
recomputed by scanning packets for each request or slider movement. The bulk
response contains bounded hourly step counts as well as relay history. Node,
scope, step and populated-hour limits must be disclosed as incomplete evidence.
No database writes, schema changes, retention extension, dependencies or new
operator configuration are required.

Capture retains at most 16,384 observation-path references **per hourly
bucket** and 128 per transmission, with an 8,192-byte input-path safety bound.
Identical paths are interned, so one relay chain observed by thousands of
transmissions costs a single entry (at most 131,072 distinct paths) and is
parsed and identity-resolved once per snapshot. A per-hour budget replaces the
single global 250,000-variant budget, which 48 h of real traffic exhausted and
which then reported `capped` evidence for every later hour and withheld the
Health and Timeline rates (#508). The per-hour ceiling makes retained capture
scale with retention and stops one busy hour from starving the rest of the
horizon; a reference costs 8 bytes against the roughly 200 the packet store
already holds for the same observation, so the real bound stays the retained
store. The capture includes the current hour for Latest, but completed-hour
history and steps exclude it.

Memberships are aggregated straight into (relay, scope, hour) cells, so the
budget — 500,000 cells and 10,000 identities — grows with populated hours
rather than with observations; nothing per transmission is retained. Step
limits cannot suppress a valid singleton membership. Each identity retains its
32 newest scope names. Source and membership limits are counted as `capped` in
the affected transmission's hour and scope; the published-payload budget and
the per-transmission pair cap are counted separately as
`stepEvidence.cappedStepAppearances` and never affect memberships or history
coverage. Capped capture is a retained subset, not a ranking of all possible
evidence. Published directed steps share one 16 MiB estimated-payload budget
across rows and hourly buckets (at most 93,206 rows or 223,696 buckets, from
180 estimated bytes per row and 75 per bucket) instead of the fixed 5,000-row
and 100,000-cell caps, which real data also reached; `/api/perf?mem=1` reports
what a snapshot actually spent as `regionsSnapshot.estimatedStepPayloadBytes`
against `stepPayloadBudgetBytes`. Publication also retains at most 10,000
endpoint identities; steps whose endpoints are omitted are reported as
`stepEvidence.missingEndpointSteps`. Endpoint metadata is independent of relay-history
membership, so a valid flood observation of a canonically direct transmission
can still be shown. The UI retains at most 1,000 shared scope pairs, paginates
both tables in 25-row pages and maps at most 200 positioned step rows. A capped
subset is not a ranking of all possible relations. Snapshot-wide
`stepEvidence` counts (excluded, unprovable and capped observations, unresolved
hops, capped step appearances and missing endpoints) are not selected-interval
traffic totals. Complete retained history does not imply complete radio
reception or that all paths can be attributed.

The published snapshot's retained size is an operator diagnostic rather than a
guess: `/api/perf?mem=1` reports `regionsSnapshot` with published counts and
per-part retained bytes for memberships, hourly history, directed steps and
per-bucket evidence. Publishing this snapshot raised staging container RSS from
about 1.55 to about 1.72 GiB, and production hosts are smaller, so the figure
is read against the host rather than against the budgets alone.

Future work can add independently evidenced scope transitions and comparisons
of observed routes. A change in timeline relay membership alone does
not establish a changed route.

The default period, display limits and any future health thresholds or playback
settings should be exposed in the customizer in a later milestone. Protocol
validity checks are not user-adjustable tuning parameters. The first milestone
uses a 24-hour default and existing theme variables, without a new operator
config key.

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

Connections tests additionally cover per-observation route/transport evidence,
all observation-loading paths, unchanged observation struct size, separate
observation boundaries, ambiguous intermediate hops, repeated/looped steps,
reverse directions, live visibility, archived metadata and source/publication
limits. Mixed flood/direct tests additionally cover planned-path exclusion,
canonical direct packets with valid non-display flood observations, singleton
memberships, current-hour Latest versus completed history, independent step
and membership budgets, and unchanged generic relay metrics. Missing wire
evidence suppresses comparisons/rates/chart zeros, not the retention floor.
Shared-scope relation keys must remain distinct even when region names
contain delimiter characters. Browser validation covers direction labels,
missing positions, partial history, local hour/scope selection without extra
requests, keyed and bounded rendering, failures, navigation, themes and narrow
screens. Benchmarks include a 30,000-transmission/2,000-node route workload
and maximum frontend fanout, not only empty snapshots.
