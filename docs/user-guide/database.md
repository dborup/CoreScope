# Database

CoreScope uses SQLite in WAL (Write-Ahead Log) mode for both the server
(read-only) and ingestor (read-write).

## WAL mode

WAL mode allows concurrent reads while writes happen. It is set automatically
at connection time via `PRAGMA journal_mode=WAL`. No operator action needed.

The WAL file (`meshcore.db-wal`) grows during writes and is checkpointed
(merged back into the main DB) periodically and at clean shutdown.

## Auto-vacuum

By default, SQLite does not shrink the database file after `DELETE` operations.
Deleted pages are marked free and reused by future writes, but the file size
on disk stays the same. This is surprising when lowering retention settings.

### New databases

Databases created after this feature was added automatically have
`PRAGMA auto_vacuum = INCREMENTAL`. After each retention reaper cycle,
CoreScope runs `PRAGMA incremental_vacuum(N)` to return free pages to the OS.

### Existing databases

The `auto_vacuum` mode is stored in the database header and can only be changed
by rewriting the entire file with `VACUUM`. CoreScope will **not** do this
automatically — on large databases (5+ GB seen in the wild) it takes minutes
and holds an exclusive lock.

**To migrate an existing database:**

1. At startup, CoreScope logs a warning:
   ```
   [db] auto_vacuum=NONE — DB needs one-time VACUUM to enable incremental auto-vacuum.
   ```
2. **Ensure at least 2× the database file size in free disk space.** Full VACUUM
   creates a temporary copy of the entire file — on a near-full disk it will fail.
3. Set `db.vacuumOnStartup: true` in your `config.json`:
   ```json
   {
     "db": {
       "vacuumOnStartup": true
     }
   }
   ```
4. Restart CoreScope. The one-time `VACUUM` will run and block startup.
5. After migration, remove or set `vacuumOnStartup: false` — it's not needed again.

### Configuration

| Field | Default | Description |
|-------|---------|-------------|
| `db.vacuumOnStartup` | `false` | One-time full VACUUM to enable incremental auto-vacuum |
| `db.incrementalVacuumPages` | `1024` | Pages returned to OS per reaper cycle |
| `db.analysisLimit` | `10000` | Index rows per index for the planner-statistics `ANALYZE` (negative disables it) |

## Planner statistics (ANALYZE)

The ingestor keeps SQLite's query-planner statistics (`sqlite_stat1`) up to
date with a bounded `ANALYZE` (`PRAGMA analysis_limit`, default 10000 rows per
index). Without them SQLite picks the plain `payload_type` index for the
channel queries instead of the partial `idx_tx_channel_hash`.

- **First start on a database without statistics:** the ingestor builds them
  right after startup. The `ANALYZE` holds the single write connection until
  it finishes, so ingest pauses and the ingest buffer catches up afterwards.
  The log says so before it starts (`[analyze] this database has no planner
  statistics; building them now …`). This happens once per database: the
  statistics are stored in the file.
- **Every start after that:** one `sqlite_master` lookup, then a refresh
  2 minutes after startup and every 24 h.
- **Order at startup:** the statistics build runs before the `route_mask`
  backfill, and each waits for the ingest buffer to drain first, so the two
  write-lock holds never run back to back.
- **Disable:** set `db.analysisLimit` to a negative value.
- **Remove the statistics:** `DROP TABLE sqlite_stat1;` against the database,
  with the ingestor stopped (the server's connection is read-only). An older
  image simply ignores the table. With `analysisLimit` still enabled the next
  ingestor start rebuilds it.

The server only reads the statistics; it never runs `ANALYZE` itself.

## Manual VACUUM

You can also run a manual vacuum from the SQLite CLI:

```bash
sqlite3 data/meshcore.db "PRAGMA auto_vacuum = INCREMENTAL; VACUUM;"
```

This is equivalent to `vacuumOnStartup: true` but can be done offline.

> ⚠️ Full VACUUM requires **2× the database file size** in free disk space (it
> creates a temporary copy). Check with `ls -lh data/meshcore.db` before running.

## Checking current mode

```bash
sqlite3 data/meshcore.db "PRAGMA auto_vacuum;"
```

- `0` = NONE (default for old databases)
- `1` = FULL (automatic, but slower writes)
- `2` = INCREMENTAL (recommended — CoreScope triggers vacuum after deletes)

See [#919](https://github.com/Kpa-clawbot/CoreScope/issues/919) for background on this feature.
