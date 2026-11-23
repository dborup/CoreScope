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
   visited in tile order, so each tile is fetched once.
5. **Scores.** The margin of a pair is the link budget (`radio` config,
   default 158 dB) minus the loss. A pair counts as **reached** at a margin
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
