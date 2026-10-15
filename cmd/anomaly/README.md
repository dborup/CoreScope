# corescope-anomaly (experimental, offline)

Offline tool for the `internal/anomaly` study (#457, #473, #474). It is not
part of the server or the ingestor, is not built into any image and has no
defaults: every bound is an explicit flag.

Only `export` reads a database, read-only. Nothing here ever writes to a
CoreScope database: `inject` and `score` read and write plain files.

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
  size. The only exception is the optional
  [ingest-backlog flag](#ingest-backlog-flag-optional-473). It reads
  `observations.observer_idx` inside the export and writes a boolean only.
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

Rate rules accept the optional `exclude_ingest_backlog` (#473). With `true`,
the rule ignores lines with `"ingest_backlog": true`: they are not counted in
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
    "full_route": "1:fd00",
    "wire_bytes": 60,
    "payload_bytes": 35,
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
- `jitter_sd` is the standard deviation of the offset from the nominal grid
  time, clamped to `±jitter_max`. The offset is not cumulative, so the train
  does not drift.
- `missed_fraction` drops pulses other than the first; `multipart_fraction`
  gives a pulse 1..`multipart_max_extra` later parts within
  `multipart_max_delay`. A hole drops every pulse inside it.
- `channel`, `first_hop`, `full_route`, `payload_type` and the sizes are the
  wire fields of a replay line: an injected line carries nothing a real line
  does not.

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
  -rule p474_periodic -start 2026-10-16T00:00:00Z -end 2026-10-23T00:00:00Z \
  [-bots b0-90s-clean,b1-120s-clean] [-out metrics.json]
```

The matching is the #457 §5 periodic rule:

- An **episode** is one `EventID`. Its activation is its first transition to
  `active`; its end is its last transition to `ended`, or the report's end if
  it never ended. Only episodes activated inside `[-start, -end)` are scored.
- An episode is a **true positive** when the channel hashes are equal,
  `[activation, end]` overlaps `[incident start, stop + 4P]`, and the
  episode's period is within the labelling tolerance of k·P for some
  k in 1..`max_harmonic`. An episode whose activation carries no periodic
  evidence can never be a true positive.
- Every other episode is a **false positive**. Without `-labels` every
  episode is one, which is the false-alarm arm: run it on background-only
  traffic to get false alarms per day.
- `-bots` narrows which incidents the recall and the medians cover - for a
  non-blind sanity-check bot, say. It never turns a matched episode into a
  false alarm: an episode matching any labelled incident stays a true
  positive, selected or not. A filter that selects no incident is an error.

The metrics are episodes, true and false positives, false alarms per day,
recall, median time to detect (also in units of P), median stop latency,
median episodes per detected incident, the per-incident detail and the false
positives per channel hash. Times to detect and stop latencies are properties
of the rule and the bot's shape, not of the scorer: a noisy train is found
much later than a clean one.
