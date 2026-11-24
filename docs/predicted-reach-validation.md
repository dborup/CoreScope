# Predicted reach: validation against observed links (#941 M2)

The predicted reach (Map → Predicted reach, `GET /api/terrain/coverage`)
computes path loss from the terrain. This harness checks those numbers
against the links the mesh has actually seen, and reports how well each
propagation model separates real links from pairs that were never heard.
Its output is what sets the default model (`map.coverage.model`) and the
calibration offset.

**Outcome (2026-11-23, real Danish links).** The default is `delta`
(delta-Bullington, ITU-R P.526 §4.5.2). Overall ROC-AUC: free space 0.770,
`bullington` 0.817, `delta` 0.830. False-reach rate (pairs never heard that
the model still predicts as reached): free space 100.0 %, `bullington`
91.4 %, `delta` 75.9 %. An operator can still set `map.coverage.model` to
`bullington`, and a request's `model` parameter always wins.

**No calibration offset is set yet.** The median SNR offset depends on the
distance, not on one constant: about −40 dB at 0–5 km, −19 dB at 5–15 km,
−4 dB at 15–30 km and +16 dB beyond 30 km. A single offset would be wrong
in most bands, and only 178 SNR pairs back these numbers, too few to fit a
distance-dependent correction. It waits for more data.

The harness is **test code**: it is not in any binary and never runs in CI
against real data. CI runs it only on the Californian e2e fixture, moved to
Jutland, over synthetic terrain (`TestReachValidation941_FixtureMechanics`),
to check how it works, not what it finds.

CI runs the fixture at a small scale, so the test stays at a few seconds
under `-race`: the nodes within 25 km of the network's centre, a 10 km
radius and a 60 m grid. The code paths are the same as a full run. The
full fixture run (every node, 40 km, the 30 m grid) is opt-in. It takes a
few seconds without `-race` and several minutes with it, and CI never sets
the variable:

```bash
cd cmd/server
CORESCOPE_REACH_VALIDATION_FULL_FIXTURE=1 go test -count=1 \
  -run 'TestReachValidation941_(FixtureMechanics|HiddenAndInactiveNodesAreLeftOut)$' -v .
```

## What it does

1. **Reads a database copy read-only.** It uses the server's own `OpenDB`
   (`mode=ro`) and reads `nodes`, `observers`, `observer_neighbors`,
   `observer_neighbor_metrics` and `neighbor_edges`. It writes nothing. A
   missing optional table counts as empty.
2. **Nodes.** It takes repeaters and rooms (the roles the reach lists)
   that meet all of these:
   - a valid position from their own adverts (estimated positions are never
     stored in `nodes`);
   - seen within the window before the newest `last_seen` in the database
     (default 7 days);
   - not hidden by the operator's blacklists or hidden-name prefixes,
     through any of their names: the node's own, an observer row's and an
     inactive node's. This is the same alias-aware rule nodes in reach use
     when they serve a list.

   Antenna heights follow the same rules as nodes in reach: the operator's
   per-node value (`radio.antennaHeightsM`), then the approved owner height
   (#789), then the role default (repeater 10 m, room 5 m).
3. **Candidate pairs.** Two such nodes at most the radius apart (default:
   `map.coverage.maxRadiusKm`).
   - **Positives** are pairs with an `observer_neighbors` row (the
     firmware's own zero-hop neighbour table) in either direction, or a
     `neighbor_edges` row whose two ends are full keys of candidate nodes,
     seen at least *minEdgeCount* times (default 3, the neighbour graph's
     own threshold).
   - An edge with an unresolved end (a hash prefix, or empty) is
     **ambiguous** and dropped.
   - A resolved edge seen fewer times is **weak** and left out of both
     positives and negatives.
   - **Negatives** are all other candidate pairs. This is weak evidence:
     a link nobody has observed may still exist.
4. **Path loss per model.** It computes free-space loss alone, Bullington
   (P.526 §4.5.1) and delta-Bullington (P.526 §4.5.2). It uses the same
   code as nodes in reach: `dhm.ReachLosses` over a disk of elevation
   tiles around one end of each pair. The tiles come through the reach's
   own tile cache, its 2-slot DHM sub-budget and `map.coverage.upstreamPerMin`.
   When the budget is spent, the harness waits and goes on. Disks are
   visited in tile order, so each tile is fetched once. The report's
   `upstreamCalls` counts the requests the run made: DHM WCS requests, plus
   the calls the GLO-30 fill reports (never fewer than the requests it sent;
   a failed fill counts as one). A run that cannot count prints "upstream
   calls not counted" in the markdown and `-1` in the JSON, never "-1
   upstream calls".
5. **Scores.** The margin of a pair is the link budget (`radio` config,
   default 158 dB) minus the loss. The losses and the budget are raw: the
   environment loss the panel subtracts by default
   (`map.coverage.environmentLossDb`, 25 dB, #962) is not applied, so the
   report measures the terrain model itself. The report names the value in
   use (`params.environmentLossDb`) so a reader can relate the two. A pair counts as **reached** at a margin
   of 3 dB or more (the "marginal" class). The report gives these, per
   model and per distance band (0–5, 5–15, 15–30 and 30+ km):

   | Metric | Meaning |
   |---|---|
   | hit rate | the share of positives that are reached |
   | false-reach rate | the share of negatives that are reached; an upper bound, since negatives are weak |
   | ROC-AUC | how well the margin ranks positives above negatives (0.5 = chance) |
   | Spearman ρ | rank correlation of predicted and observed SNR, over positives with an SNR history (`observer_neighbor_metrics`, median per pair) |
   | median SNR offset | observed minus predicted SNR. The predicted SNR is the received power minus the reporting observer's `noise_floor`, or minus sensitivity + 10 dB (SF8's limit) when it reports none. This offset is the calibration constant to fit. |

   The report also lists up to five **reference repeaters**: the observers
   with the most firmware neighbours (owner decision 6). For each, it gives
   the hit rate and AUC over its own pairs.

## Running it against an instance's database

You need a Go toolchain and a checkout of the repository at the version you
want to validate.

1. **Copy the database.** Don't point the harness at the live file. Take
   a consistent copy, for example with `scripts/backup-sqlite.sh` or
   `sqlite3 meshcore.db ".backup /tmp/reach-validation.db"`.
2. **Use the instance's `config.json`** (or a copy of it) for the terrain
   and the radio defaults:
   - `map.dhm` with its token, for DHM inside Denmark;
   - and/or `map.dhm.profile.foreignFill.enabled`, for Copernicus GLO-30;
   - `radio` (the link budget, `defaultFreqMHz`, `kFactor`,
     `antennaHeightsM`);
   - `map.coverage` (`resolutionM`, `maxRadiusKm`, `upstreamPerMin`).

   `map.coverage.enabled` does not need to be on: the harness turns it on
   for its own run. If `elevationCacheMB` is not set, it uses 1024 MB so a
   whole country's tiles stay cached.
3. **Run:**

   ```sh
   cd cmd/server
   CORESCOPE_REACH_VALIDATION_DB=/tmp/reach-validation.db \
   CORESCOPE_REACH_VALIDATION_CONFIG=/path/to/dir-with-config.json \
   CORESCOPE_REACH_VALIDATION_OUT=/tmp/reach-report \
   go test -run 'TestReachValidation941_RealDB$' -count=1 -timeout 3h -v .
   ```

   Optional settings:

   | Variable | Default | Meaning |
   |---|---|---|
   | `CORESCOPE_REACH_VALIDATION_RADIUS_KM` | `map.coverage.maxRadiusKm` (40) | largest pair distance; capped at maxRadiusKm |
   | `CORESCOPE_REACH_VALIDATION_WINDOW_DAYS` | 7 | how recently both nodes must have been seen |
   | `CORESCOPE_REACH_VALIDATION_MIN_EDGE_COUNT` | 3 | observations a `neighbor_edges` row needs to count as a link |

4. **Read the report.** The output directory gets
   `reach-validation-941.json` and `reach-validation-941.md`. The markdown
   is ready to paste into the issue.

### Cost

The DHM traffic of a run is bounded by the number of distinct 512 px tiles
the pairs touch, at the configured resolution. At 30 m one tile covers
15.36 km × 15.36 km, so all of Denmark is a few hundred tiles. The run uses
the same per-minute budget as the live reach (240 rasters a minute by
default), so a cold run takes a few minutes of DHM traffic on the
operator's token. Compute is about 1 ms to 5 ms per pair and model.

### Privacy

The report names the reference repeaters (name and key prefix) and nothing
else per node. Hidden identities are left out of the scoring and are never
named. The report is meant for the maintainers: check it before you post
it.

## Choosing the default model

Compare `bullington` with `delta`, first on ROC-AUC and the hit rate in the
15–30 km and 30+ km bands (beyond the radio horizon, which is where the two
models differ), then on Spearman ρ. The default is `delta`; it would go
back to `bullington` only if a later report showed `delta` worse there or
losing the short bands. The median SNR
offset of the chosen model is the calibration constant for M2's follow-up.
The fixture run in CI says nothing about this choice: its terrain is
synthetic and its links are Californian.

## Candidate models for buildings, vegetation and ground reflection (#963 M0)

Research only: test code, not in any binary and not served. The candidates
live in `cmd/server/reach_models_963_test.go`, the harness that scores
them in `cmd/server/reach_validation_963_harness_test.go`. It reuses this
harness's nodes, pairs and labels unchanged and scores every candidate:

| Id | Model |
|---|---|
| `delta` | today: delta-Bullington over the DTM |
| `delta+env25` | today as the panel serves it: delta + the 25 dB environment loss (#962) |
| `tworay` | the larger of delta and the plane-earth loss 40·log10 d − 20·log10(h1·h2), with the smooth-earth effective heights |
| `tworay+p2108`, `delta+p2108` | ITU-R P.2108 §3.1 terminal clutter by the book: a terminal below its clutter's height is raised to it, plus the correction |
| `delta+p2108c` | the same at suburban and urban/forest ends only; open land is left to delta, whose smooth-earth term already holds the ground's height gain |
| `tworay+dsm`, `dsm`, `dsm-raw` | delta over DHM's surface model, with and without a 150 m clearance at both ends |
| `hata-open`, `hata-suburban` | Okumura–Hata, terrain-blind |
| `mm-proxy` | map.meshcore.io's approach: 25 m forest obstacles, P.2108 at the antennas, 10 dB fade margin |

The clutter class of an end and the forest of `mm-proxy` come from the
canopy, DHM's surface minus terrain model, so the run fetches both models.
The surface tiles go through the same 2-slot DHM sub-budget and the same
`map.coverage.upstreamPerMin`, so a run makes twice the DHM requests of
the #941 run. Without DHM (GLO-30 only) there is no canopy: every end is
open, and the DSM candidates equal `delta`.

Besides #941's metrics, the report gives:
- a split by the **lower antenna height** of a pair (< 5 m, 5–10 m, ≥ 10 m);
- the **observed SNR** of the positives (median, 95th percentile, max, and how many are at or above a cap);
- a **capped SNR offset**, with both SNRs capped at the SNR a receiver reports at most. Strong links then do not count as optimistic.

**Run it against an instance's database** with the same variables as above:

```sh
cd cmd/server
CORESCOPE_REACH_VALIDATION_DB=/tmp/reach-validation.db \
CORESCOPE_REACH_VALIDATION_CONFIG=/path/to/dir-with-config.json \
CORESCOPE_REACH_VALIDATION_OUT=/tmp/reach-report \
go test -run 'TestReachValidation963_RealDB$' -count=1 -timeout 3h -v .
```

It writes `reach-validation-963.{json,md}`, and the reach radius of one
point to 40 km over 72 bearings, `reach-radius-963.{json,md}`. The point's
transmitter is 10 m above the ground and its receiver 2 m. Optional settings:

| Variable | Default | Meaning |
|---|---|---|
| `CORESCOPE_REACH_VALIDATION_963_POINT` | `56.136,8.975` (open Jutland) | the reach-radius point |
| `CORESCOPE_REACH_VALIDATION_963_SNR_CAP` | 10 | the SNR cap of the capped offset, in dB; set it from the report's observed SNR summary |
| `CORESCOPE_REACH_VALIDATION_963_FETCH_TIMEOUT` | `60s` | one DHM tile attempt, a Go duration (`90s`, `2m`) or seconds; also the DHM client's per-request timeout for this run |
| `CORESCOPE_REACH_VALIDATION_963_FETCH_RETRIES` | 3 | retries of a failed tile, after 5 s, 10 s, 20 s, …; then the tile has no terrain |
| `CORESCOPE_REACH_VALIDATION_963_PROGRESS_EVERY` | 25 | a progress line every this many disks (one disk per transmitting node); 0 for none |

The radius, window and minimum edge count of the #941 run apply too.

**Slow or failing tiles do not abort the run.** The live reach gives
each tile 10 s, including the wait for one of its 2 DHM slots, and fails
the whole computation on one failure. DSM rasters can take longer than
that. The harness therefore fetches each DHM tile itself, two at a time,
with the timeout above, through the same sub-budget and
`upstreamPerMin`. A tile that fails is retried with a doubling backoff.
If it still fails after the last retry, it has no terrain, like a gap
in #941: the pairs over it count as "without terrain" and the rest are
scored. The report's header gives the fetches per source: for example,
`DHM tile fetches (60 s timeout, up to 3 retries; …): DTM 212 fetched,
0 retries, 0 failed; DSM 209 fetched, 6 retries, 3 failed (timeout ×12)`.
Both reports have them in `terrain.fetch` (JSON) as well. A run with
failed tiles is marked partial. If many tiles fail, rerun with a longer
timeout: tiles fetched before are not cached between runs, so the
whole run is repeated. Only an error that no retry can fix stops the
run: a refused token (`status 401`/`403`), a request the client refuses,
or the run's own deadline (`-timeout`). The error names the stage (`DTM`
or `DSM`; the clutter class is computed from these two grids, with no
fetch of its own), the disk, the tile and its EPSG:25832 bounding box,
and the reason, for example:

```
disk 41/312: DSM fetch, disk of 5f0c1a2b: DSM tile 33,404 (EPSG:25832 bbox 506880,6205440,522240,6220800), attempt 1: status 403
```

With `-v`, the progress lines show that a long run is alive:

```
963: disk 50/312, 1210/7034 pairs, 118 + 118 tiles; tile fetches DTM 61 fetched, 0 retries, 0 failed; DSM 60 fetched, 2 retries, 1 failed (timeout ×4), 125 upstream calls; 3m12s elapsed
```

**Without a token**, the fixture moved to Jutland and the reach radius can
run over the public Copernicus GLO-30 tiles. This is opt-in, because it
reads them over the network:

```sh
cd cmd/server
CORESCOPE_REACH_VALIDATION_963_GLO30=1 CORESCOPE_REACH_VALIDATION_963_OUT=/tmp/out \
CORESCOPE_REACH_VALIDATION_FULL_FIXTURE=1 \
go test -run 'TestReachValidation963_GLO30$' -count=1 -timeout 1h -v .
```

CI runs only the mechanics: `TestReachValidation963_FixtureMechanics`
(synthetic terrain with synthetic forest and villages) and the model pins
`TestReachModels963_*`. Set `CORESCOPE_REACH_VALIDATION_963_OUT` to keep
their reports; `TestReachModels963_ReachRadiusFlat` then writes the
flat-ground reach radius too.
