# corescope-anomaly (experimental, offline)

Offline tool for the `internal/anomaly` study (#457, #473, #474). It is not
part of the server or the ingestor, is not built into any image and has no
defaults: every bound is an explicit flag.

Only `export` reads a database, read-only. Nothing here ever writes to a
CoreScope database: `keys`, `inject`, `label` and `score` read and write
plain files.

```sh
cd cmd/anomaly && go build -o corescope-anomaly .
```

## export

Builds `anomaly.ReplayRecord` JSON Lines from a CoreScope SQLite database.

```sh
corescope-anomaly export -db /snap/meshcore.db -immutable \
  -start 2026-10-08T00:00:00Z -end 2026-10-15T00:00:00Z \
  -settle 30s -max-rows 20000000 -out events.jsonl
```

- **Read-only.** The database is opened with SQLite `mode=ro` (with
  `-immutable` also `immutable=1`) and only `SELECT` runs. A missing file is an
  error, never a new database. See [Run it on a snapshot copy](#run-it-on-a-snapshot-copy)
  for what `mode=ro` still does to a WAL database.
- **Key-free.** The query reads the frame (`raw_hex`), the observation path
  (`path_json`), the observation time and the row IDs that join an observation
  to its transmission. It never reads the content hash, `decoded_json`, channel
  names, public keys, observer columns, SNR/RSSI or resolved paths. A line
  carries payload type, the 1-byte channel hash (group payloads), route class,
  first hop and full route (hop prefixes as on the wire), frame and payload
  size. The only exceptions are the optional
  [ingest-backlog flag](#ingest-backlog-flag-optional-473) and
  [late-delivery flag](#late-delivery-flag-optional-538). They read
  `observations.observer_idx` inside the export and write a boolean only.
- **Opaque IDs.** A line's `id` is the export's own number: `tx1`, `tx2`, … in
  the order each transmission's first row is read. It is not the content hash,
  which would join the line to every CoreScope instance that heard the packet
  (and on a database not rehashed by #215 is `raw_hex[:16]`, for an ADVERT six
  bytes of the advertiser's public key). It is not a row ID or part of a frame
  either. IDs are unique within one file; each export numbers from `tx1` again,
  so IDs from different exports mean nothing to each other.
- **Not anonymous.** The id does not join a line to a database, but the line
  can still be re-identified: its time, payload type, sizes and route can
  single out one transmission in any database that heard it. Treat an
  export, and a replay report built from it, as data from that instance and
  keep them with whoever runs the study; publish aggregates only.
- **Bounded.** The query is one index range scan on `observations.timestamp`
  over `[start - settle, end + settle)`, with a primary-key lookup and an index
  search per row. Exceeding `-max-rows` fails the export. The range is at most
  31 days.
- `-out` must not exist. The file appears only when the export succeeds. The
  summary (counts only) is printed as JSON on stderr.

### Ingest-backlog flag (optional, #473)

```sh
corescope-anomaly export ... -backlog-silence 10m -backlog-span 2h
```

With both flags (whole seconds in (0, 24h]; there are no defaults, and
without them the output is unchanged), a line whose event's first
observation follows a silence of that observer gets `"ingest_backlog": true`:

- An observation is a **reconnect** when its observer has no observation in
  the `-backlog-silence` before it, `[t - silence, t)`. The observer's
  **backlog span** is `[t, t + span)`.
- An event is flagged when its first observation (the one that sets its
  `time`; ties in the same second go by observation row id, as text) lies in a
  backlog span of the observer that made it. A late re-hearing by a
  reconnecting observer does not flag a transmission another observer heard
  first. Rows with a NULL `observer_idx` are never flagged.
- Times are `observations.timestamp` as stored: the observer's receive time
  from the MQTT envelope, or the ingest time where the ingestor falls back to
  it. The #457 artifact label uses the same times.

**Key-free.** The export reads `observations.observer_idx` (never the
`observers` table) and keeps it in memory only. The flag is the only change to
a line, and the output is byte-identical when observers are renamed.

**Cost.** One more index range scan on `observations.timestamp` over
`[start - settle - silence - span, end + settle)` reads the observer and time
of every row, under the same `-max-rows` cap. Memory holds one entry per
observer and one per merged backlog span, both capped. The database must hold
data over that whole lookback, because before its first observation every
observer would look reconnected. A lookback that starts earlier is refused.
The summary adds `ingest_backlog` with the scan's row counts and the number
of flagged events.

### Late-delivery flag (optional, #538)

```sh
corescope-anomaly export ... -late-window 1m -late-min-rows 10 -late-share 0.5 -late-gap 2m
```

The four flags go together (window and gap whole seconds in (0, 1h], min rows
in 1..1048576, share in (0, 1]); there are no defaults, and without them the
output is unchanged. They cannot be combined with the `-backlog-*` flags. They
set the same `"ingest_backlog": true`, so a rate rule's
`exclude_ingest_backlog` reads them unchanged. The flag marks the events of an
observer burst that delivers transmissions the rest of the network reported
minutes earlier: a queue sent on reconnect and stamped at delivery.

- **Already seen.** For a row of observer O at time `t`, `e` is the latest
  observation of the same transmission by another observer before
  `t + gap`. The row is already seen when there is one and its delay
  `d = t - e` is at least `-late-gap`: others heard it at least the gap
  before, and none of them within the gap.
- **Late burst.** Bins are epoch-aligned and `-late-window` wide. O's bin k is
  a late burst when O's rows in bins k-1, k and k+1 number at least
  `-late-min-rows`, at least `-late-share` of them are already seen, and the
  delays of the already-seen rows span at least `-late-gap`. The spread keeps
  an observer whose clock runs behind by a constant offset from looking late.
- **Flag.** An event is flagged when its first observation (the one that sets
  its `time`) is a row of O in a late burst of O. Rows with a NULL
  `observer_idx` are never flagged.

Unlike the #473 flag, it needs no silence: a real storm that an often-quiet
observer hears first is not flagged, because the other observers hear the
same transmissions within seconds or not at all.

**Limits.** These are properties of the definition, pinned in
`late_limits_538_test.go` at window 1m, min rows 10, share 0.5 and gap 2m:

- A **common-cause outage is never flagged.** When two or more observers lose
  the same uplink or broker and drain the same outage together, each queued
  row has a peer, the other draining observer, seconds away. That peer is
  the latest other observation before `t + gap`, so no row is already seen.
  One observer draining a 40-min outage is flagged (25 of 25, 40 of 40); two
  or three draining it together are not (0).
- **Recall falls for large queues after short outages.** The share counts the
  queued rows only the draining observer heard, so a queue is flagged only
  when at least as many shared rows older than the gap come with it. A queue
  of 40 GRP_TXT, delivered in order at 5 rows/s: 40 of 40 flagged after a 20-
  or 40-min outage with 6 shared rows a minute; 0 of 40 after 3-10 min; 0 of
  40 after 3-40 min with 1 shared row a minute.
- **A share does not protect against skewed clocks.** Two observers whose
  clocks run behind by different offsets give another observer's rows two
  different delays. Their spread can exceed the gap: a storm of 40 GRP_TXT
  heard by O and, a third each, by two observers 3 and 7 min behind marks
  the 13 events O heard first, at share 0.5.
- An outage shorter than about 2 gap is never flagged: already-seen delays are
  at least the gap and must spread over at least the gap.
- A queue from an observer that overlaps no other observer, or a queue that
  keeps its receive times, is not flagged.
- The share is compared as `seen >= share * rows` in floating point, so a few
  bounds that are exact in decimal are missed (share 0.07 with 7 of 100 rows
  seen is not a burst). Shares that are multiples of 1/2^k, such as 0.5, are
  exact.

**Key-free.** As for the #473 flag: `observer_idx` stays in memory, the flag
is the only change to a line, and the output is byte-identical when observers
are renamed.

**Cost.** One more index range scan on `observations.timestamp` over
`[start - settle - 2 window, end + settle + 2 window)`, under the same
`-max-rows` cap, with one index search per row over its transmission's
observations. Memory holds one entry per (observer, bin) with rows, capped.
The summary adds `late_delivery` with the scan's row counts, the rows already
seen and the number of flagged events.

Events are built by `anomaly.Normalizer`. An observation is route evidence
when it lies at most `-settle` after the transmission's first observation.
Rows without `observations.raw_hex` (written before #881) take their header and
payload from the transmission frame and their path from `path_json`. Their
count is in the summary.

### Run it on a snapshot copy

`mode=ro` stops the export from writing data, but on a WAL database it is not
free of side effects:

- If the `-wal` and `-shm` files are missing, as on a cleanly closed copy,
  SQLite creates them beside the database (`-wal` empty, `-shm` 32 KiB). In a
  read-only directory the open fails with `attempt to write a readonly
  database`. The database bytes are unchanged.
- On a live database the export's single query holds one read snapshot for the
  whole run. The ingestor cannot checkpoint past it, so its WAL grows until the
  export ends.

So do not export from the live file. Copy it with `VACUUM INTO` on a
read-only connection, then export the copy with `-immutable`:

```sh
sqlite3 -readonly /path/meshcore.db "VACUUM INTO '/snap/meshcore.db'"
corescope-anomaly export -db /snap/meshcore.db -immutable ...
```

`VACUUM INTO` (SQLite 3.27 or later) copies the database inside one read
transaction. Writers carry on meanwhile, so it finishes under any ingest
rate, and the copy is the database as of that transaction's start, in one
self-contained file. It fails if the target exists, and needs free space
for a compacted copy of the database. While it runs the ingestor cannot
checkpoint past it, as with the export, but only for the duration of one
copy.

Do not use the sqlite3 shell's `.backup`. The backup API copies in steps
and restarts whenever another connection writes to the source between
steps, so under steady ingest it may never finish. An interrupted
`.backup` leaves a partial file and a `-journal` beside it (which
`-immutable` refuses).

The offline copy from `scripts/backup-sqlite.sh` (container stopped
briefly) can be the source instead. It may include `meshcore.db-wal`, so
run the same `VACUUM INTO` on that copy, in a writable directory, rather
than exporting it with `-immutable` directly.

`-immutable` opens the file with `mode=ro&immutable=1`: SQLite takes no
locks and creates no side files, so a read-only directory works. SQLite then
also ignores a `-wal` or hot `-journal` file, whose contents would silently
be missing, so the export refuses a snapshot with either file beside it.

## replay

Runs `anomaly.Replay` and prints the deterministic report, in the format of
`internal/anomaly/testdata/replay_golden.json`.

```sh
corescope-anomaly replay -config config.json -input events.jsonl \
  -start 2026-10-08T00:00:00Z -end 2026-10-15T00:00:00Z -max-line-bytes 65536 [-out report.json]
```

The config is the replay JSON config (`anomaly.ParseReplayConfig`). Periodic
rules accept two optional fields:

- `end_after_missed_pulses` — train tracking (#447). Leaving it out means 0,
  which turns tracking off.
- `exclude_channels` — channel hashes the rule ignores, as plain lower-case
  hex (`["81"]`). Leaving it out excludes nothing. See
  [Excluded channels](#excluded-channels).

Rate rules accept the optional `exclude_ingest_backlog` (#473; the #538 flag
sets the same field). With `true`, the rule ignores lines with
`"ingest_backlog": true`: they are not counted in
its window, never trigger it and do not keep its episodes open. Leaving it out
means `false`, and then the flag changes nothing.

## Excluded channels

The periodic rule's target is an abusive channel bot (#474). Automation that
is *expected* on its channel is not a target: a wardriving app posts a
position every 75-123 s while someone drives, and the #457 round-4 labels
show that is what the rule was measuring.

`exclude_channels` names the hashes whose automation is expected. An event on
such a channel never reaches that rule, so it forms no pulse, no chain and no
episode, and the channel claims no key state.

Two properties worth stating plainly:

- **A channel hash is not a channel.** MeshCore's hash is one byte, so
  excluding the hash of `#wardriving` also excludes every other channel name
  that hashes to the same byte (about 1/256 of all names), including any spam
  on them. That is the price of keying on an opaque 1-byte hash.
- **The match is on the whole hash**, length included: the 1-byte hash `81`
  and the 2-byte hash `81ab` are different entries, and a 1-byte entry is
  never read as the prefix of a wider hash.

There are no default exclusions anywhere: an unconfigured rule behaves
exactly as it did before #474, and the replay golden file is unchanged.

## keys

Ranks the stream keys of an exported background by row count, so an
injection panel can be placed on keys real traffic actually uses.

```sh
corescope-anomaly keys -input bg.jsonl -payload-type 5 -top 16 \
  -exclude-channels 81 -max-keys 2000000 -max-line-bytes 65536 -out keys.json
```

**Why it exists.** The stream key is payload type + channel hash + route
class + route kind + first hop. A bot on a channel byte and a first hop no
real transmission carries has that key to itself, so the detector sees a
clean train and recall measured on it says little about finding a bot inside
real traffic: a real spam bot on a busy channel shares its key with everyone
else entering through the same repeater.

**Blind by construction.** `keys` reads the export and counts rows. It never
runs the detector, reads no report, no labels and no metrics, and has no idea
which keys would alarm. The ranking is row counts, ties broken by the
canonical key encoding, so a panel built from its output is chosen
mechanically and before any detection result exists.

**The filter is mechanical** and is restated in the output, with a counter
per reason a line was skipped: the payload type must be `-payload-type`, the
line must carry a channel hash (the periodic rule's target is a channel bot),
its route kind must be `relay` (only then is the first hop part of the stream
key, so only then can a bot share the key), and its channel must not be in
`-exclude-channels`.

Per key the output gives the canonical `key`, the `channel`, `route_class`,
`route_kind` and `first_hop` an injection spec needs to land on it, the most
common whole route (`full_route`, ties broken by the smallest encoding), the
key's whole observed size distribution (`size_mix`), the median wire size,
the median payload size and a `payload_bytes_spread` of (p90 − p10)/2, the
row count and the first and last times. Percentiles are nearest-rank and the
spread is clamped strictly below the median. `first` and `last` are the
earliest and latest line on the key, whatever order the input is in.
Per-key size and route variants are bounded and the overflow is reported per
key; `-max-keys` is a hard bound on distinct keys, as the export's
`-max-rows` is on rows. `-top` reports exactly that many keys, so the full
ranking (a large `-top`) is what an absence check should read.

**`size_mix` is the distribution to inject from.** It is one entry per
distinct `(wire_bytes, payload_bytes)` pair with the number of lines that
carried it, ordered by wire size then payload size; `payload_bytes` is absent
for the lines whose payload length the export did not know. The two sizes are
*not* independent: a frame is header plus path plus payload, so
`wire − payload` is the route's own overhead, while the payload itself comes
in 16-byte cipher blocks. Drawing them separately — a fixed median wire with
a spread payload — both produces frames real traffic never carries and, once
the drawn payload passes `wire − 2`, frames the event validator refuses
outright. So `inject` takes whole observed pairs (see `size_mix` under
Spec).

The output carries hashes, hop prefixes, sizes, counts and times — what the
export itself carries — and no ids, observers, names or text.

## inject

Overlays synthetic channel-bot trains on exported background lines, so recall
can be measured when real spam is rare. It writes two new files and nothing
else.

```sh
corescope-anomaly inject -spec spec.json -input events.jsonl \
  -out events-injected.jsonl -labels labels.json \
  -max-injected 100000 -max-line-bytes 65536
```

- **Clearly synthetic.** Every injected line's `id` is
  `inj-<bot>-<pulse>-<part>`; an export numbers its lines `tx1`, `tx2`, …, so
  no real line can carry that marker. A background line that already starts
  with `inj-` is refused, so the marker stays unambiguous.
- **Never written back.** The tool takes no `-db` flag and opens no database.
  Injected events exist only in the output file; they must never be imported
  into any CoreScope database.
- **File-only and deterministic.** Same spec, same seed, same background →
  byte-identical output and labels. The generator is splitmix64 inside this
  tool, not `math/rand`, and each bot's stream is seeded from the spec seed
  and the bot's name, so adding, removing or reordering bots never changes
  another bot's events.
- **Background untouched.** Background lines are copied through byte for
  byte, in order; injected lines are merged in by data time, so the output is
  a valid replay input.
- `-out` and `-labels` must not exist, and neither appears unless the whole
  run succeeds. `-max-injected` is a hard bound.
- The summary (counts and times only) is printed as JSON on stderr.

### Spec

Every field is required; there are no defaults. Unknown fields are rejected.
The example is the shape of the September bot documented in #130 (5 min,
~3 s jitter, ~30 % multi-part pulses with later parts up to 55 s behind, ~5 %
single missed pulses, one 35-minute hole, one stable first hop).

```json
{
  "seed": 20261015,
  "final_after": "30s",
  "labelling": {"pulse_merge_gap": "60s", "min_period": "1m", "max_period": "1h",
                "min_pulses": 8, "max_harmonic": 3,
                "tolerance_abs": "30s", "tolerance_rel": 0.1},
  "bots": [{
    "name": "sept-shape",
    "channel": "7f",
    "payload_type": 5,
    "route_class": "flood",
    "route_kind": "relay",
    "first_hop": "1:fd",
    "full_route": "1:fd0a1c",
    "size_mix": [{"wire_bytes": 38, "payload_bytes": 35, "count": 70},
                 {"wire_bytes": 54, "payload_bytes": 51, "count": 20},
                 {"wire_bytes": 70, "payload_bytes": 67, "count": 10}],
    "start": "2026-10-16T00:00:00Z",
    "stop": "2026-10-19T13:12:00Z",
    "period": "5m",
    "jitter_sd": "3s",
    "jitter_max": "10s",
    "missed_fraction": 0.05,
    "multipart_fraction": 0.3,
    "multipart_max_extra": 2,
    "multipart_max_delay": "55s",
    "holes": [{"start": "2026-10-17T04:00:00Z", "duration": "35m"}]
  }]
}
```

- `final_after` is added to each event time to give its `final_at`; set it to
  the export's `-settle`, so injected lines are final like the background's.
  Like `-settle`, it must be whole seconds.
- **Times are whole seconds.** Every injected event time is truncated to a
  whole second, as the export's are (`time.Unix(ts, 0)`), so a fractional
  second never gives an injected line away. Truncation moves a time by less
  than a second towards the past, so the effective offset from the grid is in
  `[-(jitter_max + 1s), jitter_max]`.
- **The sizes are described in exactly one of two ways**, and a spec must
  pick one.
  - `size_mix` is the realistic one, and what `keys` emits per key: the
    observed `(wire_bytes, payload_bytes)` pairs with a count each. One whole
    pair is drawn per event, weighted by count, so the injected sizes are
    exactly the sizes the key carries — valid by construction, and nothing an
    injected line can be recognised by. With `size_mix`, `wire_bytes`,
    `payload_bytes` and `payload_bytes_spread` must be left out.
  - `wire_bytes` with `payload_bytes` and `payload_bytes_spread` is the older
    scalar description, for hand-written specs: a fixed wire size and a
    payload drawn uniformly from `payload_bytes ± spread`. 0 keeps the size
    constant. It cannot reproduce a real distribution, so the **whole
    envelope** is required to fit the frame: the spread must be smaller than
    `payload_bytes`, and `payload_bytes + payload_bytes_spread` must be at
    most `wire_bytes − 2`, which is what `Event.Validate` allows.
  Either way, every size a bot can draw is checked against the event
  validator while the spec is read — before a single event, line or file is
  produced — so an impossible size is a spec error and never an abort part
  way through a run.
- `jitter_sd` is the standard deviation of the offset from the nominal grid
  time, clamped to `±jitter_max`. The offset is not cumulative, so the train
  does not drift.
- **Take the jitter from measurement, not from the rule.** Scaling
  `jitter_sd` to the rule's own per-gap tolerance places every bot inside
  that tolerance by construction, which makes recall conditional on the rule
  being evaluated and moves the panel whenever the rule moves. #130 measured
  a real 5-minute bot over 85.2 h: `|gap − 300 s|` median 3 s, p95 9 s. A gap
  offset is the difference of two per-pulse offsets, so its standard
  deviation is √2 times the per-pulse one, and p95 9 s on a normal gap offset
  gives sd(gap) ≈ 9/1.96 ≈ 4.6 s and a per-pulse sd of ≈ 3.2 s. So
  `jitter_sd` 3 s with `jitter_max` 9 s (#130's measured p95) is the
  measured shape, for every period, independent of any rule value. Some
  pulses of a short-period bot then do fall outside a tight tolerance, which
  is the point: it is the rule's job to cope, not the panel's to avoid.
- `missed_fraction` drops pulses other than the first; `multipart_fraction`
  gives a pulse 1..`multipart_max_extra` later parts within
  `multipart_max_delay`. A hole drops every pulse inside it.
- `channel`, `first_hop`, `full_route`, `payload_type` and the sizes are the
  wire fields of a replay line: an injected line carries nothing a real line
  does not. `full_route` is a whole observed route, not just the first hop
  repeated, so an injected line's route looks like an exported one's.

### make_spec.py

`make_spec.py` beside this README is the #474 round-3 panel generator: it
takes the validation window's start and a `keys.json` ranking and prints the
spec for the pre-registered paired panel (nine shapes, each on the key of
rank *i*+1 and on an isolated twin key). It is stdlib-only, reads nothing
else and has no clock, so the spec is a pure function of its two inputs and
can be re-derived from them afterwards.

It lives in the repository rather than only in the pre-registration comment
so that the whole chain — `export` → `keys` → `make_spec.py` → `inject` — is
exercised by the tool's own tests on a background whose payload sizes are
block-quantised like real GRP_TXT traffic. That chain is where round 2 broke:
the spec it produced could not be injected at all on a realistic key.

```sh
python3 make_spec.py 2026-10-17T00:00:00Z keys.json > spec.json
```

### Labels

`-labels` holds the ground truth of the run, computed from the emitted pulses
with the `labelling` recipe, which is the #457 §4 periodic recipe:

1. events closer than `pulse_merge_gap` are one pulse, timed at its first
   event;
2. P is the median pulse gap inside `[min_period, max_period]`;
3. an incident is a maximal run of at least `min_pulses` pulses whose every
   gap is within `max(tolerance_abs, tolerance_rel * P)` of k·P for some
   k in 1..`max_harmonic`.

So a bot with a long hole is two incidents, and a bot with fewer than
`min_pulses` pulses is none — the ground truth is what the recipe says, not
"one bot, one incident". The file holds channel hashes, times, periods and
counts: no ids, routes, hops, observers, names or text.

## score

Measures a replay report against injected labels, or against no labels at
all.

```sh
corescope-anomaly score -report report.json -labels labels.json \
  -baseline report-a.json \
  -rule p474_periodic -start 2026-10-16T00:00:00Z -end 2026-10-23T00:00:00Z \
  [-bots b0-90s-clean,b1-120s-clean] [-out metrics.json]
```

The matching is the #457 §5 periodic rule:

- An **episode** is one `EventID`. Its activation is its first transition to
  `active`; its end is its last transition to `ended`, or the report's end if
  it never ended. Only episodes activated inside `[-start, -end)` are scored.
- An episode is a **true positive** when it is the **same series** as the
  incident, `[activation, end]` overlaps `[incident start, stop + 4P]`, and
  the episode's period is within the labelling tolerance of k·P for some
  k in 1..`max_harmonic`. An episode whose activation carries no periodic
  evidence can never be a true positive.
- **Same series** means the same stream key when the labels carry one (they
  do since #474 round 2), and the same channel hash otherwise. The key is
  what makes a panel placed on real traffic's own keys scorable: on a busy
  channel several route groups share the 1-byte hash, and matching on the
  hash alone would credit any of them to the bot.
- `-baseline` takes the replay report of the **same window without the
  injection** (arm A). An episode it already has, on the same key and
  overlapping in time, is attributed to the background: it is neither a true
  positive nor a false positive, and is counted as `baseline_episodes`. That
  is the differential reading a shared key needs — a detection in the
  injected arm counts only when it is absent from the plain arm.
- Every other episode is a **false positive**. Without `-labels` every
  episode is one, which is the false-alarm arm: run it on background-only
  traffic to get false alarms per day.
- `-bots` narrows which incidents the recall and the medians cover - for a
  non-blind sanity-check bot, say. It never turns a matched episode into a
  false alarm: an episode matching any labelled incident stays a true
  positive, selected or not. A filter that selects no incident is an error.

The metrics are episodes, true and false positives, false alarms per day,
recall, time to detect (also in units of P), stop latency, episodes per
detected incident, the per-incident detail and the false positives per
channel hash. Times to detect and stop latencies are properties of the rule
and the bot's shape, not of the scorer: a noisy train is found much later
than a clean one.

**The median hides the tail.** A clean train alarms at the earliest pulse
`min_pulses` allows, so a panel of mostly clean bots puts the median on that
floor while the realistic shapes take many times longer. Every median is
therefore reported beside its nearest-rank **p75** and its **maximum**, and
`-bots` can narrow a run to the realistic subset so that subset gets its own
numbers. Read the tail, not only the median.

**A tail over the detected incidents cannot fail on a missed bot.** The
medians, p75s and maxima above cover the *detected* incidents only, so a
panel whose four cleanest shapes alarm at the floor and whose four realistic
ones are never found reports recall 0.50 and a p75 of one period — a pass of
every timing bar, on a result that found nothing realistic. So the time to
detect is also reported over every **selected** incident with a miss ranked
above every finite value:
`median_time_to_detect_selected` / `p75_time_to_detect_selected`, with
`_periods` beside each. JSON has no infinity, so a percentile whose rank
falls among the misses is reported as `..._selected_infinite: true` with the
value left out — which fails any bar placed on it. `incidents_missed` counts
the selected incidents that were not detected. A bar belongs on the selected
tail; the detected-only tail describes the shapes that were found.

**Baseline masking is visible per incident.** A `-baseline` episode is not
scored either way, so without a marker an incident whose only episode was
absorbed by the background reads as a plain miss. Each per-incident row
therefore carries `baseline_episodes` (the episodes on the incident's own
series, overlapping its window, that the plain arm already had) and
`baseline_masked` (not detected, but the background already had an episode
here); `incidents_baseline_masked` counts them. That tells a no-go caused by
background contamination apart from a detector miss.

Provenance (`labels`, `baseline`, `real_labels`) is recorded as the file
names only, never the paths they were read from: a path can carry a user or
a host name, and a metrics file may be shared.

### Real-traffic labels in arm A (`-real-labels`, #474 round 4)

`-real-labels` takes arm A's ground truth from [`label`](#label): real
automated senders on the real background. The match is the one above, on
the **channel hash** (the labels carry no stream key, so any route group of
the hash matches), with the labels file's own tolerance. An episode that
matches no labelled sender is a false alarm; `recall` is then the
**real-sender recall** (labelled senders with at least one episode; with no
labelled sender it is 0/0 and the field is left out, not written as 0), and
each sender is a per-incident row named `real-1`, `real-2`, ... in the
file's order. Beside them the metrics carry round 3's **strict** reading of
the same episodes, every one a false alarm: `strict_false_positives` and
`strict_false_alarms_per_day`. Those three fields (with `real_labels`) are
written only with `-real-labels`, so every other run's metrics file is
unchanged.

`-real-labels` is arm A's flag only: it combines with none of `-labels`,
`-baseline` and `-bots`. Each labels file is accepted by its own flag only
(`real: true` against `synthetic: true`), and the real labels' window must
cover `[-start, -end)` — a sender outside the labelled window could not have
been labelled.

## label

Labels real automated senders in an export from aggregate signals alone, so
the false-alarm arm can tell a real sender — the rule's target — from a
false alarm (#474 round 4). It reads the export only, opens no database,
needs no decryption (so it works on channels nobody holds a key for), uses
no detector output and refuses a line with an injected id.

```sh
corescope-anomaly label -input bg.jsonl -payload-type 5 -exclude-channels 81 \
  -start 2026-10-26T00:00:00Z -end 2026-11-02T00:00:00Z \
  -pulse-merge-gap 60s -min-period 1m -max-period 1h -min-pulses 8 -max-harmonic 3 \
  -tolerance-abs 30s -tolerance-rel 0.1 -min-size-share 0.95 \
  -max-lines 50000000 -max-line-bytes 4096 -out labels-real.json
```

The recipe, restated in the output's `recipe`:

1. **Channel-hash level.** Every line of the payload type on one channel
   hash, through any route group, inside `[-start, -end)`, not excluded. A
   sender entering through changing first hops is one series here, where the
   rule's stream key would split it.
2. **Segments.** A channel's lines are split at every gap no run can bridge
   (more than `max_harmonic · max_period` plus the tolerance at
   `max_period`), so P is estimated from the segment's own traffic and not
   from a week of everything on the hash. Splitting there never cuts a run.
3. **The §4 periodic recipe** (see [Labels](#labels)) per segment, with the
   pulse merge capped at half the segment's raw cadence:
   `min(pulse_merge_gap, P0/2)`, P0 the median raw gap of at least
   `min_period/2`. A fixed 60 s merge folds every other pulse of a
   one-minute train into its neighbour, so the recipe could not label the
   bottom of its own period range; from a 2-minute cadence up the cap is
   inactive and the merge is exactly `pulse_merge_gap`.
4. **Constant size.** A run is kept only when one payload size carries at
   least `min_size_share` of its lines (every line of its pulses, multi-part
   parts included). A line whose payload length is unknown counts in the run
   but never as the common size.

**Why the size test.** The timing recipe alone is looser than the rule — its
tolerance over k = 1..3 is nearly continuous at short periods — so on its own
it would label almost anything the rule fires on, and arm A would pass by
construction. The rule never reads a payload size. Human chat mixes sizes
(text of different lengths and sender names, in 16-byte cipher blocks); an
automated sender repeating one message format does not. A periodic train of
varied size — a verse bot, say — is therefore **not** labelled and stays a
false alarm: the label can excuse an episode only when evidence the detector
does not use agrees with it. The counters `timing_runs` and `size_rejected`
show how many runs the timing recipe found and how many the size test
refused.

The output holds `(channel, start, stop, period, size_share)` tuples and
counts: no ids, hops, routes, size values, names or text. It is a pure
function of the lines and the flags; the input's line order does not matter.
