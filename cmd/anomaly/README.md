# corescope-anomaly (experimental, offline)

Offline tool for the `internal/anomaly` study (#457). It is not part of the
server or the ingestor, is not built into any image and has no defaults:
every bound is an explicit flag.

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
  size.
- **Opaque IDs.** A line's `id` is the export's own number: `tx1`, `tx2`, … in
  the order each transmission's first row is read. It is not the content hash,
  which would join the line to every CoreScope instance that heard the packet
  (and on a database not rehashed by #215 is `raw_hex[:16]`, for an ADVERT six
  bytes of the advertiser's public key). It is not a row ID or part of a frame
  either. IDs are unique within one file; each export numbers from `tx1` again,
  so IDs from different exports mean nothing to each other.
- **Bounded.** The query is one index range scan on `observations.timestamp`
  over `[start - settle, end + settle)`, with a primary-key lookup and an index
  search per row. Exceeding `-max-rows` fails the export. The range is at most
  31 days.
- `-out` must not exist. The file appears only when the export succeeds. The
  summary (counts only) is printed as JSON on stderr.

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

So do not export from the live file. Take a snapshot with a short read, then
export the snapshot with `-immutable`:

```sh
sqlite3 -readonly /path/meshcore.db ".backup /snap/meshcore.db"   # one self-contained file
corescope-anomaly export -db /snap/meshcore.db -immutable ...
```

`-immutable` opens the file with `immutable=1`: SQLite takes no locks and
creates no side files, so a read-only directory works. SQLite then also
ignores a `-wal` or hot `-journal` file, whose contents would silently be
missing, so the export refuses a snapshot with either file beside it.

## replay

Runs `anomaly.Replay` and prints the deterministic report, in the format of
`internal/anomaly/testdata/replay_golden.json`.

```sh
corescope-anomaly replay -config config.json -input events.jsonl \
  -start 2026-10-08T00:00:00Z -end 2026-10-15T00:00:00Z -max-line-bytes 65536 [-out report.json]
```

The config is the replay JSON config (`anomaly.ParseReplayConfig`). Periodic
rules accept the optional `end_after_missed_pulses` (train tracking, #447).
Leaving it out means 0, which turns tracking off.
