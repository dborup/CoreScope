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
corescope-anomaly export -db /path/meshcore.db \
  -start 2026-10-08T00:00:00Z -end 2026-10-15T00:00:00Z \
  -settle 30s -max-rows 20000000 -out events.jsonl
```

- **Read-only.** The database is opened with SQLite `mode=ro` and only
  `SELECT` runs. A missing file is an error, never a new database.
- **Key-free.** The query reads the frame (`raw_hex`), the observation path
  (`path_json`), the observation time and the content hash. It never reads
  `decoded_json`, channel names, public keys, observer columns, SNR/RSSI or
  resolved paths. A line carries payload type, the 1-byte channel hash (group
  payloads), route class, first hop and full route (hop prefixes as on the
  wire), frame and payload size, and the content hash as ID.
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
