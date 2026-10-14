# CoreScope Deployment Guide

Comprehensive guide to deploying and operating CoreScope. For a quick start, see [DEPLOY.md](../DEPLOY.md).

## Table of Contents

- [System Requirements](#system-requirements)
- [Docker Deployment](#docker-deployment)
- [Migration — pulling the new Compose file](#migration--pulling-the-new-compose-file)
- [Configuration Reference](#configuration-reference)
- [MQTT Setup](#mqtt-setup)
- [TLS / HTTPS](#tls--https)
- [Behind a CDN (Cloudflare, Fastly)](#behind-a-cdn-cloudflare-fastly)
- [Monitoring & Health Checks](#monitoring--health-checks)
- [Backup & Restore](#backup--restore)
- [Troubleshooting](#troubleshooting)

---

## System Requirements

| Resource | Minimum | Recommended |
|----------|---------|-------------|
| RAM | 256 MB | 512 MB+ |
| Disk | 500 MB (image + DB) | 2 GB+ for long-term data |
| CPU | 1 core | 2+ cores |
| Architecture | `linux/amd64`, `linux/arm64` | — |
| Docker | 20.10+ | Latest stable |

CoreScope runs well on Raspberry Pi 4/5 (ARM64). The Go server uses ~300 MB RAM for 56K+ packets.

---

## Docker Deployment

### Quick Start (one command)

```bash
docker run -d --name corescope \
  -p 80:80 \
  -v corescope-data:/app/data \
  ghcr.io/kpa-clawbot/corescope:latest
```

Open `http://localhost` — you'll see an empty dashboard.

No `config.json` is required. The server starts with sensible defaults:
- HTTP on port 3000 (Caddy proxies port 80 → 3000 internally)
- **No MQTT broker.** The bundled Mosquitto is opt-in: add
  `-e DISABLE_MOSQUITTO=false -p 1883:1883` to run it, or point `mqttSources`
  in `config.json` (or `-e MQTT_BROKER=…`) at your own broker
- The ingestor connects to `mqtt://localhost:1883` unless configured otherwise
- SQLite database at `/app/data/meshcore.db`

### Full `docker run` Reference (recommended)

The bare `docker run` command is the primary deployment method. One image, documented parameters — run it however you want.

```bash
docker run -d --name corescope \
  --restart=unless-stopped \
  -p 80:80 -p 443:443 -p 1883:1883 \
  -e DISABLE_MOSQUITTO=false \
  -e DISABLE_CADDY=false \
  -v /your/data:/app/data \
  -v /your/Caddyfile:/etc/caddy/Caddyfile:ro \
  -v /your/caddy-data:/data/caddy \
  ghcr.io/kpa-clawbot/corescope:latest
```

#### Parameters

| Parameter | Required | Description |
|-----------|----------|-------------|
| `-p 80:80` | Yes | HTTP web UI |
| `-p 443:443` | No | HTTPS (only if using built-in Caddy with a domain) |
| `-p 1883:1883` | No | MQTT broker (expose if external gateways connect directly) |
| `-v /your/data:/app/data` | Yes | Persistent data: SQLite DB, config.json, theme.json |
| `-v /your/Caddyfile:/etc/caddy/Caddyfile:ro` | No | Custom Caddyfile for HTTPS |
| `-v /your/caddy-data:/data/caddy` | No | Caddy TLS certificate storage |
| `-e DISABLE_MOSQUITTO=false` | No | Run the bundled Mosquitto broker. It is **opt-in**: unset, the broker does not start and the ingestor needs your own broker in `mqttSources` |
| `-e DISABLE_CADDY=true` | No | Skip the built-in Caddy reverse proxy |
| `-e MQTT_BROKER=mqtt://host:1883` | No | Override MQTT broker URL |
| `-e CORESCOPE_RUN_AS_NONROOT=1` | No | Opt-in: run `corescope-server` and `corescope-ingestor` without root — see [Running the Go services without root](#running-the-go-services-without-root-opt-in) |
| `-e CORESCOPE_UID=10001 -e CORESCOPE_GID=10001` | With the above | Dedicated numeric user and group the two Go services run as |

#### `/app/data/.env` convenience file

Instead of passing `-e` flags, you can drop a `.env` file in your data volume:

```bash
# /your/data/.env
DISABLE_MOSQUITTO=true
DISABLE_CADDY=true
MQTT_BROKER=mqtt://my-broker:1883
```

The entrypoint sources this file before starting services. This works with any launch method (`docker run`, compose, or manage.sh).

### Running the Go services without root (opt-in)

By default every program in the container runs as root, exactly as in earlier
releases. Setting `CORESCOPE_RUN_AS_NONROOT` runs **only** `corescope-server`
and `corescope-ingestor` as an unprivileged user, which limits what a
compromised web server or ingestor can do. Nothing changes until you turn it on.

| Variable | Values |
|----------|--------|
| `CORESCOPE_RUN_AS_NONROOT` | `1` or `true` turns it on. Unset, empty, `0` or `false` keep the default. Any other value stops the container with an error. |
| `CORESCOPE_UID` | Required when on. A dedicated numeric user ID, e.g. `10001`: not 0, not an account that already exists in the image (`mosquitto` 101, `caddy` 100, `nobody` …), and not the owner of the data directory. |
| `CORESCOPE_GID` | Required when on. Numeric group ID, 100 or higher and not the `mosquitto`/`caddy` group, e.g. `10001`. Use a dedicated group that no host user belongs to: its members can create files in the data directory. (An existing image group such as `users` (100) is reused, but on most hosts GID 100 is a shared group.) |

Set them with `-e`, in compose `environment:`, or in `/app/data/.env` as plain
`KEY=VALUE` lines. Use a **dedicated** user ID that owns nothing on the host,
not your own host user: root reads `.env`, `config.json` and `theme.json` from
the data directory at every start, so the services must never own the data
directory or those files. A start with `CORESCOPE_UID` equal to the owner of
the data directory is refused.

**What stays the same.** supervisord, the entrypoint and Caddy still run as
root; Mosquitto still runs as `mosquitto` and still ignores `PUID`/`PGID`.
With the setting off, on a volume that never used it, the entrypoint takes
the old path unchanged and starts the same supervisor config files.

**What happens at start.** As root, before any service starts:

1. **Check.** Before `.env` is sourced and `config.json`/`theme.json` are
   linked, the start stops with a `[nonroot] ERROR` line, and nothing is
   sourced, linked or changed, if any of those three files is a symlink, a
   hard link or not a regular file, is owned by `CORESCOPE_UID`, is owned by
   anyone but root or the owner of the data directory, or is writable for
   `CORESCOPE_GID` or for everyone, or if `CORESCOPE_UID` owns the data
   directory. A read-only mount (`config.json:ro`) is accepted whoever owns
   it, since nothing in the container can change or replace it. A file the
   services created, or could have changed, is therefore never used by root.
2. The entrypoint creates a `corescope` account with those IDs and writes
   `/run/corescope/supervisord.conf`: the selected config with
   `user=corescope` added to the two Go programs only.
3. **The data directory keeps its owner.** It gets the group `CORESCOPE_GID`,
   group write and the sticky bit (for example `755` becomes `1775`). The
   services can create their database files there (SQLite needs that for
   `-wal`, `-shm` and `-journal`), but the sticky bit stops them from removing,
   renaming or replacing anything they do not own.
4. These paths are given to `CORESCOPE_UID:CORESCOPE_GID`, each one only if
   its owner differs:
   - `meshcore.db`, `ping_scores_history.db` and their `-wal`, `-shm` and
     `-journal` files,
   - `client-rx-delete.salt` and `client-rx-delete-audit.jsonl` (the only log
     files the services write; their other logs go to the container output),
   - the queue directories `prune-requests/`, `mbcap-snapshot/`,
     `channel-proposal-requests/` and `client-rx-deletes/` (not their
     contents).
5. A queued request (`request-*.json`, `cmd-*.json`) in those directories that
   the services did not write — for example from an admin command run as
   root, or left by a root run — is **not** handed to them. It is renamed to
   `….held`, keeps its owner, and the log says
   `[nonroot] WARNING: … renamed to ….held and NOT run`. Repeat the command as
   the services' user, or rename the file back if you still want it run.
6. It writes `/app/data/.corescope-nonroot` (root-owned, holds `UID:GID`).
   While that file exists, the check in step 1 runs at every start, even
   after the setting is removed, until the rollback below.

There is no recursive `chown`. Symlinks and hard-linked files on that list are
left unchanged with a warning in the log. The cost is a few `stat` calls per
start, plus a `chown` for each wrong owner: a few milliseconds, whatever the
database size. The log shows what happened, e.g.
`[nonroot] ownership: 7 of 9 path(s) in /app/data changed to 10001:10001`;
later starts report `0 of …`. Files that a root process creates later (a
`docker cp` restore, a run with the setting off) are fixed at the next start
in this mode.

**Host-side effect.** With a bind mount, the host sees the data directory with
group `CORESCOPE_GID` and mode `1775` (or similar), the listed files and any
new file the services create owned by those numeric IDs, the
`.corescope-nonroot` file, and possibly `….held` requests. Your own files —
`config.json`, `theme.json`, `.env`, other files and subdirectories — keep
their owner and mode. They must be readable for the services (mode `0644`, or
`0640` with group `CORESCOPE_GID`) and must not be writable for that group;
an unreadable `config.json` stops the start with
`config.json is not readable for UID:GID`. An external `hashRegionsPath` file,
a `dbPath` outside `/app/data` and a `CORESCOPE_INGESTOR_STATS` path are not
migrated and must be accessible to that user.

**config.json is read-only for the services.** In this mode the server never
writes `config.json`, whether it is mounted read-only
(`-v /srv/corescope/config.json:/app/data/config.json:ro`) or sits in the data
directory. Saving the geo filter from the customizer fails with HTTP 409 and
`geo filter not saved: config.json is read-only for the server …`, and the
filter is not applied; edit `geo_filter` in `config.json` on the host and
restart instead.

**Admin commands** must run as the services' user, so the running ingestor can
read the request files they write:

```bash
docker exec -u 10001:10001 corescope /app/corescope-ingestor -config /app/config.json \
  admin delete-client-rx --pubkey <64 hex> --dry-run
```

Run as root, `admin delete-client-rx` refuses (`refusing to run as root`) and
writes nothing. Run other tools that open the databases (e.g. `sqlite3`) with
`-u` too: in the sticky, group-writable data directory the kernel's
`fs.protected_regular` protection, enabled by default on many distributions,
can stop even root from opening files that it does not own.

#### Turning it on

This is also the migration from the default mode.

1. Back up both databases with the offline helper (see [Backup](#backup)):
   `sh scripts/backup-sqlite.sh corescope /backups`. Also keep a copy or
   snapshot of the whole data directory (salt, audit log, queues,
   `config.json`, `.env`).
2. Record the current owner and mode of every path this mode changes, with
   `nonroot.sh owners`, so you can restore them exactly:

   ```sh
   IMAGE=$(docker inspect --format '{{.Image}}' corescope)
   docker run --rm --pull=never --network none --volumes-from corescope \
     --entrypoint /usr/local/lib/corescope/nonroot.sh "$IMAGE" owners > /backups/corescope-owners.txt
   ```

3. On the host, make sure `config.json`, `theme.json` and `.env` are readable
   but not writable for the chosen group, e.g. `chmod 644` (or `chgrp 10001`
   and `chmod 640`).
4. Add `CORESCOPE_RUN_AS_NONROOT=1`, `CORESCOPE_UID` and `CORESCOPE_GID` to the
   container environment or `/app/data/.env`, and recreate or restart the
   container.
5. Check the log line `[nonroot] corescope-server and corescope-ingestor run as
   …`, that there is no `[nonroot] ERROR`, and the processes:
   `docker exec corescope ps -o pid,user,group,args`.

#### If the start stops with `[nonroot] ERROR`

The line says what is wrong; nothing was sourced, linked or changed. For a file
that the services own or could write, **check its content before you use it
again**: the services could have changed it. Then give it to root or the host
user, remove group write for `CORESCOPE_GID`, and start again. If
`CORESCOPE_UID` owns the data directory, choose a dedicated ID, or give the
directory back to its previous owner.

Development builds of this mode from before this check existed gave the data
directory itself to `CORESCOPE_UID`. If you ran one: stop the container, check
`.env`, `config.json` and `theme.json` for changes you did not make, then run
`set-owner` as in Rollback step 2 (or `chown` the directory back to its
previous owner on the host) before you start the new image.

#### Rollback

1. Remove the three variables (or set `CORESCOPE_RUN_AS_NONROOT=0`) and
   recreate or restart the container. Everything runs as root again, and this
   alone restores service: the first start with the setting off removes group
   write and the sticky bit from the data directory (logged as
   `[nonroot] the mode is off: …`), because `fs.protected_regular` would
   otherwise stop root from opening the services' files there. The check from
   step 1 above keeps running at every start until step 2.
2. Restore the recorded owners and modes. With the container **stopped**, run
   the helper from the same image as root:

   ```sh
   IMAGE=$(docker inspect --format '{{.Image}}' corescope)
   docker stop corescope
   docker run --rm -i --pull=never --network none --volumes-from corescope \
     --entrypoint /usr/local/lib/corescope/nonroot.sh "$IMAGE" restore-owners < /backups/corescope-owners.txt
   docker start corescope
   ```

   `restore-owners` gives every recorded path its recorded owner and mode
   back, the data directory included, so mixed owners come back exactly.
   Listed paths created while the mode was on go to root (`0:0`, as a root
   run would create them; pass another `UID:GID` as an argument if needed).
   It accepts only the data directory and the listed paths, and removes
   `.corescope-nonroot`. Without a record, `set-owner 0:0` (or another
   `UID:GID`) gives the data directory and every listed path, queue entries
   included, to one owner and removes the group write and sticky bit from the
   data directory. Both refuse while `.env`, `config.json` or `theme.json`
   fails the check, and neither touches your other files. `….held` requests
   stay; delete them or rename them back.
3. If data looks wrong, restore the backup from step 1 as described in
   [Restore](#restore).

### Docker Compose (legacy alternative)

Docker Compose files are maintained for backward compatibility but are no longer the recommended approach.

```bash
curl -sL https://raw.githubusercontent.com/Kpa-clawbot/CoreScope/master/docker-compose.example.yml \
  -o docker-compose.yml
docker compose up -d
```

#### Compose environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `HTTP_PORT` | `80` | Host port for the web UI |
| `DATA_DIR` | `./data` | Host path for persistent data |
| `DISABLE_MOSQUITTO` | `true` | Set `false` to run the bundled MQTT broker (opt-in) |
| `DISABLE_CADDY` | `false` | Set `true` to skip the built-in Caddy proxy |
| `PROD_MQTT_BIND` | *(empty — IPv4 and IPv6)* | Host interface the bundled broker's `1883` is published on; `127.0.0.1` for host-only |
| `PPROF_BIND` | `127.0.0.1` | Host interface the pprof ports are published on (staging compose files); `0.0.0.0` for remote access |

### manage.sh (legacy alternative)

The `manage.sh` wrapper script provides a setup wizard and convenience commands. It uses Docker Compose internally. See [DEPLOY.md](../DEPLOY.md) for usage. New deployments should prefer bare `docker run`.

### Image tags

| Tag | Use case |
|-----|----------|
| `v3.4.1` | Pinned release — recommended for production |
| `v3.4` | Latest patch in the v3.4.x series |
| `v3` | Latest minor+patch in v3.x |
| `latest` | Latest release tag |
| `edge` | Built from master on every push — unstable |

### Updating

```bash
docker compose pull
docker compose up -d
```

For `docker run` users:

```bash
docker pull ghcr.io/kpa-clawbot/corescope:latest
docker stop corescope && docker rm corescope
docker run -d --name corescope ... # same flags as before
```

Data is preserved in the volume — updates are non-destructive.

---

## Migration — pulling the new Compose file

### Upgrading past #441 — bundled broker opt-in, pprof on loopback

Two defaults change. Both are deliberate and both are one line to undo.

**1. The bundled Mosquitto no longer starts unless you ask for it.**
`DISABLE_MOSQUITTO` now defaults to `true` everywhere: `docker-compose.yml`,
`docker-compose.example.yml`, the image's entrypoint (so a bare `docker run`
too), `manage.sh` and its setup wizard (which now defaults to "no"), and
`.env.example`. An install that relies on the bundled broker and never set
the variable — or set it to something that is not a recognised value — will
start **without a broker** and ingest nothing, with a `[broker] error:` line
in the ingestor log. To keep the bundled broker, set:

```bash
DISABLE_MOSQUITTO=false          # in .env, or -e DISABLE_MOSQUITTO=false
```

An install that already sets `DISABLE_MOSQUITTO=true`, or `false`, behaves
exactly as before. So does one whose `.env` was copied from an older
`.env.example`, which carried an explicit `DISABLE_MOSQUITTO=false`.

**2. `DISABLE_MOSQUITTO` is read the same way by every launcher.** The
entrypoint, `manage.sh` and the ingestor accept, trimmed and in any case,
`true`/`1`/`yes`/`on` (broker off) and `false`/`0`/`no`/`off` (broker on).
Unset or empty means the default, `true`. Anything else (`y`, `n`, a typo)
also means `true`, and is reported by the entrypoint, the ingestor and
`./manage.sh broker-check`. Before, a bare `docker compose up` disabled the
broker only on the exact `true` while `manage.sh` also took `yes`/`1`/`on`;
`broker-check` no longer has a mismatch to report and prints the effective
mode instead, e.g. `Broker mode: external (DISABLE_MOSQUITTO=yes, read as
true)`.

**3. pprof listens on loopback when enabled.** With `ENABLE_PPROF=true` the
server (`6060`) and ingestor (`6061`) listeners bind `127.0.0.1`, and the
staging compose files publish those ports on `127.0.0.1` only. Ports are
unchanged and stay distinct. `/debug/pprof/` is unauthenticated, so the
intended way in is an SSH tunnel:

```bash
ssh -L 6060:127.0.0.1:6060 -L 6061:127.0.0.1:6061 you@your-host
# then open http://localhost:6060/debug/pprof/ and http://localhost:6061/debug/pprof/
```

To keep remote access instead:

| Where pprof runs | Setting that restores remote access |
|------------------|-------------------------------------|
| `docker-compose.staging*.yml` (publishes `6060`/`6061`) | `PPROF_BIND=0.0.0.0` in `.env` — IPv4 on every interface. The in-container listener is pinned to every interface by those files, so `PPROF_BIND` alone decides |
| A bare binary, or a `docker run`/compose file that maps the pprof ports itself | `PPROF_HOST=0.0.0.0` (or `*`) — a loopback listener inside a container is unreachable through a port mapping |

`MQTT_SOURCE_STRICT` is unchanged and stays off by default.

**A non-container ingestor.** An ingestor started outside the image with
`DISABLE_MOSQUITTO` unset now assumes an external broker, so a loopback-only
`mqttSources` (a broker on the same host) logs a `[broker] error:` line. It
still starts and still connects; set `DISABLE_MOSQUITTO=false` to silence it.

### The #372 settings

#372 made the bundled MQTT broker's exposure and the pprof listeners'
exposure **explicit settings** instead of hard-coded values.

**#372 itself changed nothing for an existing install.** Every setting it
added shipped with the default that reproduced the previous behaviour; the
two defaults #441 moved are marked in the table below. The table is what you
can change, not what changed under you.

| Setting | Shipped default | What the default does | Change it to |
|---------|-----------------|-----------------------|--------------|
| `PROD_MQTT_BIND` | empty | Publishes the bundled broker's `1883` with no host interface, i.e. on both IPv4 (`0.0.0.0`) and IPv6 (`[::]`) — byte for byte the mapping this file always had | `127.0.0.1` to reach the plaintext broker from the host only |
| `PPROF_BIND` | `127.0.0.1` (since #441) | Publishes the staging pprof ports `6060`/`6061` on the host's loopback only | `0.0.0.0` for remote access over IPv4 |
| `PPROF_HOST` | `127.0.0.1` (since #441; pinned to `0.0.0.0` in the staging compose files) | The in-process pprof listener binds loopback | `0.0.0.0` (or `*`) for a process whose pprof port is reached from outside it (see the warning below) |
| `PPROF_PORT_SERVER` / `PPROF_PORT_INGESTOR` | `6060` / `6061` | Same ports as before; they must stay distinct | Any free pair, when `6060`/`6061` are taken |
| `MQTT_SOURCE_STRICT` | `false` | A broker setup that cannot ingest is logged, and the ingestor still starts | `true` to refuse to start instead |
| `DISABLE_MOSQUITTO` | `true` (since #441) | The bundled broker does not run | `false` to run the bundled broker |
| `MQTT_BROKER` / `MQTT_TOPIC` | empty | Unchanged — `config.json` decides the sources | the external broker's URL, as an alternative to editing `config.json` |

**Leave `PROD_MQTT_BIND` empty, do not write `0.0.0.0` into it.** Docker
publishes a mapping with no host interface (`"1883:1883"`) on *both* `0.0.0.0`
and `[::]`, but a mapping with an explicit IPv4 host (`"0.0.0.0:1883:1883"`)
publishes IPv4 **only**. Its shipped default is therefore empty, so the
rendered mapping is identical to the one this file always had. The same is
why `PPROF_BIND=0.0.0.0` restores remote profiling over IPv4 only.

**`PPROF_BIND` and `PPROF_HOST` are not interchangeable.** `PPROF_BIND` is the
*host* interface a published container port listens on; `PPROF_HOST` is the
interface the listener *inside* the process binds. A loopback listener inside
a container is not reachable through a docker port mapping at all, because
docker's proxy connects to the container's network address — which is why
the staging compose files pin `PPROF_HOST=0.0.0.0` and narrow with
`PPROF_BIND`. Use `PPROF_BIND` for a compose file that publishes pprof and
`PPROF_HOST` for anything else.

**What you may now see in the log.** Two situations that used to pass silently
are reported:

- `ENABLE_PPROF` set to something other than the exact string `true` (for
  example `1`, `yes` or `TRUE`) leaves pprof **off**, as it always did, and
  the process now logs that it did so.
- With the bundled broker off and no `mqttSources` configured, or with only
  loopback ones, the ingestor logs a `[broker] error:` line — it is pointed at
  a broker that is not running and will ingest nothing. It still starts; set
  `MQTT_SOURCE_STRICT=true` if you would rather the startup fail. A
  `DISABLE_MOSQUITTO` value that is not recognised logs a `[broker] warn:`
  line (see "Upgrading past #441" above).

**Settings in `.env` that a process reads now reach the container.**
`MQTT_BROKER`, `MQTT_TOPIC`, `MQTT_SOURCE_STRICT`, `ENABLE_PPROF` and the
`PPROF_HOST*` / `PPROF_PORT*` settings are read by the server and the
ingestor, not by docker compose, so every app compose file forwards them
through its `environment:` block with an empty default. An unset setting
arrives as an empty string, which every reader ignores, so an install that
never set one is unaffected.

> **The one thing that can change under you: `MQTT_BROKER`.** Before this
> release the compose files did not forward it, so a value in `.env` was dead
> and `config.json` decided the sources. It now reaches the ingestor, where it
> **overrides** `mqttSources` in `config.json` entirely (`LoadConfig`
> precedence: `MQTT_BROKER` > `mqttSources` > legacy `mqtt.broker`). If you
> have a stale or experimental `MQTT_BROKER=` line in `.env` *and* real
> `mqttSources` in `config.json`, the ingestor will switch to that single env
> source on the next restart. Check which source wins before you restart:
>
> ```bash
> ./manage.sh broker-check
> ```
>
> It prints the sources in the ingestor's own precedence. Delete the `.env`
> line if `config.json` is the one you meant.

Broker URLs in every one of those diagnostics are masked: user name,
password, query and fragment never reach the log.

To check an external-broker setup before starting or updating:

```bash
./manage.sh broker-check
```

---

## Configuration Reference

CoreScope uses a layered configuration system (highest priority wins):

1. **Environment variables** — `MQTT_BROKER`, `DB_PATH`, etc.
2. **`/app/data/config.json`** — full config file (volume-mounted)
3. **Built-in defaults** — work out of the box with no config

### Environment variable overrides

| Variable | Default | Description |
|----------|---------|-------------|
| `MQTT_BROKER` | `mqtt://localhost:1883` | MQTT broker URL (overrides config file) |
| `MQTT_TOPIC` | `meshcore/#` | MQTT topic subscription pattern |
| `DB_PATH` | `data/meshcore.db` | SQLite database path |
| `DISABLE_MOSQUITTO` | `true` | Skip the bundled Mosquitto broker; `false` runs it (opt-in). `true`/`1`/`yes`/`on` and `false`/`0`/`no`/`off`, trimmed, any case; anything else means `true` |
| `DISABLE_CADDY` | `false` | Skip the built-in Caddy reverse proxy |
| `MQTT_SOURCE_STRICT` | `false` | Refuse to start when the broker setup cannot ingest, instead of only logging it |
| `ENABLE_PPROF` | `false` | Expose `/debug/pprof/`. Only the exact string `true` enables it |
| `PPROF_HOST` | `127.0.0.1` | Host the in-process pprof listener binds; `0.0.0.0` or `*` for every interface |
| `PPROF_PORT_SERVER` | `6060` | Server pprof port (overrides the shared `PPROF_PORT`) |
| `PPROF_PORT_INGESTOR` | `6061` | Ingestor pprof port (overrides the shared `PPROF_PORT`) |

### config.json

For advanced configuration, create a `config.json` and mount it at `/app/data/config.json`:

```bash
docker run -d --name corescope \
  -p 80:80 \
  -v corescope-data:/app/data \
  -v ./config.json:/app/data/config.json:ro \
  ghcr.io/kpa-clawbot/corescope:latest
```

See `config.example.json` in the repository for all available options including:
- MQTT sources (multiple brokers)
- Channel encryption keys
- Branding and theming
- Health thresholds
- Region filters
- Retention policies
- Geo-filtering
- Map tile providers (OSM, Stamen, Carto, etc.)

### Disable estimated node positions

Neighbor-derived position estimates are enabled by default. To disable them
for the entire instance, add this to `config.json`:

```json
{
  "estimatedPositions": {
    "enabled": false
  }
}
```

Omitting the section or its `enabled` field preserves existing behavior
(`true`). Use a JSON boolean, not the string `"false"`; invalid policy types
are rejected at startup.

Restart the **Go server** after changing this setting, then refresh open
browser tabs. The policy is fixed for the lifetime of each server instance;
SIGHUP does not reload it. No ingestor change or database migration is needed.

When disabled, the server skips neighbor-position estimation and omits its
results from node details and packet paths, including saved Ping Scores
paths. Estimate-dependent tools and map views display an operator-disabled
notice instead of suggesting that no neighbor evidence exists. Areas retains
reported-position density, bridge nodes, and the unpositioned-node count.
Reported GPS, ordinary neighbor graphs, and independent IATA/name-based
fallbacks are unchanged. No stored data is deleted or rewritten: Ping Scores
path archives captured while estimates were enabled keep their recorded
approximate geometry and capture time, and the background history refresh
does not rewrite them while the policy is off, so the original evidence
reappears as soon as the setting is turned back on.

The effective setting is published through `/api/config/client`. Browser
preferences and deep links cannot enable it against the server policy. A
Customizer display preference is deferred to a later milestone and must
remain subordinate to this operator setting.

### Large region lists in an external file (`hashRegionsPath`)

`hashRegions` lives inline in `config.json`. On a real deployment that is a
list of ~1,100 names in the middle of hand-edited operational settings and
credentials, which makes the file hard to review and diff. The optional
`hashRegionsPath` key moves just that list out:

```json
{
  "hashRegions": ["#belgium", "#eu"],
  "hashRegionsPath": "hash-regions.json"
}
```

`hash-regions.json` is a plain JSON array of names:

```json
["dk", "dk-aarhus", "#dk-fyn"]
```

Rules:

- **Opt-in.** Omitted or empty means inline `hashRegions` only, exactly as
  before. There is no auto-discovered default file, so a file that merely
  happens to sit next to `config.json` is never read.
- **Union, not replacement.** The effective set is the inline list plus the
  file, normalized the same way (whitespace trimmed, a leading `#` added,
  blank entries dropped) and deduplicated after normalization. `dk` and
  `#dk` are the same scope; `#dk` and `#DK` are **not** — the key is
  `SHA256` of the exact name, so case matters.
- **Both processes, one loader.** The ingestor (which derives one HMAC key
  per region) and the server (unknown scopes in Tools → Observer Neighbors,
  `unusedRegions` in `/api/scope-stats`) read the file through the same
  loader, so they cannot disagree about which scopes are configured.
- **Relative paths resolve against the directory holding `config.json`**,
  never the process working directory — the two processes are started from
  different places, and anchoring at the config file is what makes them
  agree. Absolute paths are used as-is.
- **Env override.** `HASH_REGIONS_PATH` wins over the config key.
- **Failures are loud but survivable.** If the path is set and the file is
  missing or is not a JSON array of strings, the error is logged and
  startup falls back to the inline `hashRegions` list. On a SIGHUP reload
  the previous keys are kept instead (see below).

`areas` — the largest block in `config.json` — has no equivalent yet.

### Reloading config changes without a restart (SIGHUP)

Most `config.json` changes require a container restart to take effect. **`hashChannels`** and **`hashRegions`** are the exception — the ingestor can reload just these two settings live:

```bash
docker exec corescope kill -HUP $(docker exec corescope pgrep corescope-ingestor)
```

This re-reads `config.json` — and the external `hashRegionsPath` file it names, if any — and derives fresh channel-decryption and region-scope keys in place — no restart, no dropped MQTT connections. The ingestor logs the result:

```
[hot-reload] SIGHUP received, reloading hashChannels/hashRegions from /app/config.json
[hot-reload] reloaded 1415 channel key(s), 1098 region key(s) from /app/config.json
```

If the edited `config.json` is malformed, the reload is aborted and logged, and the ingestor keeps its previous, working keys rather than going dark. The same holds for the `hashRegionsPath` file: a malformed or missing region-name file aborts the reload and keeps every previous key, so a half-saved file cannot blank out a working ingestor.

Why this matters more than it sounds: restarting the whole container to add a single hashtag channel or region also resets the in-memory relay/scope analytics (Analytics → Scopes tab — Repeaters by Region, Bridge Repeaters, etc.), which take real time to rebuild from live traffic after a cold start. SIGHUP lets you add a channel or region without paying that cost.

Other `config.json` changes (MQTT sources, retention, branding, health thresholds, etc.) still require a full restart.

### Map Tile Providers

Map tile providers are enabled and configured via the `config.json` file. You can provide your custom API credentials (e.g. `osm_url`, `stamen_api_key`, `mapbox_api_key`) to activate external tile services. Once configured on the server, users can select their preferred tile provider from the Customizer UI on the client, and their choice will be persisted automatically.

---

## MQTT Setup

CoreScope receives MeshCore packets via MQTT. The container ships with an internal Mosquitto broker, which is **opt-in**: it runs only with `DISABLE_MOSQUITTO=false`. Otherwise configure your own broker (below).

### Internal broker (opt-in)

With `DISABLE_MOSQUITTO=false` the built-in Mosquitto broker listens on port 1883 inside the container. Point your MeshCore gateways at it:

```bash
# Run the bundled broker and expose its port for external gateways
docker run -d --name corescope \
  -p 80:80 -p 1883:1883 \
  -e DISABLE_MOSQUITTO=false \
  -v corescope-data:/app/data \
  ghcr.io/kpa-clawbot/corescope:latest
```

### External broker

To use your own MQTT broker (Mosquitto, EMQX, HiveMQ, etc.):

1. Leave the internal broker off — the default (`DISABLE_MOSQUITTO` unset or
   `true`).

2. Point the ingestor at your broker:
   ```bash
   -e MQTT_BROKER=mqtt://your-broker:1883
   ```

   Or via `config.json`:
   ```json
   {
     "mqttSources": [
       {
         "name": "my-broker",
         "broker": "mqtt://your-broker:1883",
         "username": "user",
         "password": "pass",
         "topics": ["meshcore/#"]
       }
     ]
   }
   ```

3. Check the result before you start or update:
   ```bash
   ./manage.sh broker-check
   ```

   With the bundled broker off, nothing configures a replacement for you. With no `mqttSources`, the ingestor falls back to
   its built-in `mqtt://localhost:1883` — the bundled broker's own address,
   where nothing is now listening — and ingests nothing while otherwise
   looking healthy. `broker-check` and the ingestor's own startup log both
   report that, with broker credentials masked. Set `MQTT_SOURCE_STRICT=true`
   to make the ingestor refuse to start in that state.

### Multiple brokers

CoreScope can connect to multiple MQTT brokers simultaneously:

```json
{
  "mqttSources": [
    {
      "name": "local",
      "broker": "mqtt://localhost:1883",
      "topics": ["meshcore/#"]
    },
    {
      "name": "remote",
      "broker": "mqtts://remote-broker:8883",
      "username": "reader",
      "password": "secret",
      "topics": ["meshcore/+/+/packets"]
    }
  ]
}
```

### MQTT topic format

MeshCore gateways typically publish to `meshcore/<gateway>/<region>/packets`. The default subscription `meshcore/#` catches all of them.

---

## TLS / HTTPS

### Option 1: External reverse proxy (recommended)

Run CoreScope behind nginx, Traefik, or Cloudflare Tunnel for TLS termination:

```nginx
# nginx example
server {
    listen 443 ssl;
    server_name corescope.example.com;

    ssl_certificate /etc/ssl/certs/corescope.pem;
    ssl_certificate_key /etc/ssl/private/corescope.key;

    location / {
        proxy_pass http://localhost:80;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
    }
}
```

The `Upgrade` and `Connection` headers are required for WebSocket support.

### Option 2: Built-in Caddy (auto-TLS)

The container includes Caddy for automatic Let's Encrypt certificates:

1. Create a Caddyfile:
   ```
   corescope.example.com {
     reverse_proxy localhost:3000
   }
   ```

2. Mount it and expose TLS ports:
   ```bash
   docker run -d --name corescope \
     -p 80:80 -p 443:443 \
     -v corescope-data:/app/data \
     -v caddy-certs:/data/caddy \
     -v ./Caddyfile:/etc/caddy/Caddyfile:ro \
     ghcr.io/kpa-clawbot/corescope:latest
   ```

Caddy handles certificate issuance and renewal automatically.

---

## API Documentation

CoreScope auto-generates an OpenAPI 3.0 specification from its route definitions. The spec is always in sync with the running server — no manual maintenance required.

### Endpoints

| URL | Description |
|-----|-------------|
| `/api/spec` | OpenAPI 3.0 JSON schema — machine-readable API definition |
| `/api/docs` | Interactive Swagger UI — browse and test all 40+ endpoints |

### Usage

**Browse the API interactively:**
```
http://your-instance/api/docs
```

**Fetch the spec programmatically:**
```bash
curl http://your-instance/api/spec | jq .
```

**For bot/integration developers:** The spec includes all request parameters, response schemas, and example values. Import it into Postman, Insomnia, or any OpenAPI-compatible tool.

### Public instance
The live instance at [analyzer.00id.net](https://analyzer.00id.net) has all API endpoints publicly accessible:
- Spec: [analyzer.00id.net/api/spec](https://analyzer.00id.net/api/spec)
- Docs: [analyzer.00id.net/api/docs](https://analyzer.00id.net/api/docs)

---

## Behind a CDN (Cloudflare, Fastly)

If you front CoreScope with a CDN — Cloudflare, Fastly, Akamai, or
similar — you **must** configure the CDN to bypass cache for `/api/*`.
The server emits `Cache-Control: no-store` on every API response
(see #1551), but Cloudflare's zone-level Cache Rules and legacy Page
Rules can override origin headers. When that happens, observers, packets,
stats and other API responses get cached at the edge for minutes to hours,
producing observer-flap, stale dashboards and inconsistent state across
viewers.

### 1. Verify whether your CDN is caching `/api/*`

From **outside** the CDN (a different network than your origin), run:

```sh
curl -sI 'https://<your-domain>/api/observers' | grep -iE 'cf-cache|age|cache-control'
```

Healthy output (cache is bypassed):

```
cache-control: no-store
cf-cache-status: BYPASS
age: 0
```

Unhealthy output (CDN is caching despite `no-store`):

```
cache-control: no-store
cf-cache-status: HIT
age: 4732
```

`HIT` or `age > 0` means an intermediary is serving cached JSON. Fix it
before relying on the dashboard.

You can also run the bundled helper, which exits non-zero with a precise
diagnostic when caching is detected:

```sh
scripts/check-cdn-bypass.sh https://<your-domain>
```

### 2. Cloudflare: add a Cache Rule (recommended)

Cloudflare Dashboard → your zone → **Caching → Cache Rules → Create rule**:

- **When incoming requests match:** Field = `URI Path`, Operator = `starts with`, Value = `/api/`
- **Then:** Cache eligibility → **Bypass cache**

Save and deploy. Re-run the curl above; you should now see
`cf-cache-status: BYPASS`.

Legacy Page Rules equivalent (if your zone has no Cache Rules):

- URL pattern: `*your-domain*/api/*`
- Setting: **Cache Level → Bypass**

### 3. Fastly / other CDNs

Apply the equivalent bypass-cache rule for the `/api/` path prefix.
The key invariant is: any response from `/api/*` must reach the
browser uncached (no shared-cache HIT, no positive `Age` header).

### 4. Re-verify

After applying the rule, run step 1's curl from outside the CDN again
and confirm `cf-cache-status: BYPASS` (or absence of HIT) and `age: 0`.

### 5. Watch the server log

The server logs a one-shot warning at the first request bearing a
CDN-specific header (`CF-Connecting-IP`, `CF-Ray`, `Fastly-Client-IP`,
or `True-Client-IP`):

Generic reverse-proxy headers (`X-Forwarded-For`, `X-Real-IP`) are
deliberately NOT used as the signal — every nginx/Caddy/Traefik/k8s
deploy sets them, so they'd produce false positives on every
reverse-proxied install.

```
[security] WARNING: detected request via CDN (CF-Ray header present).
Ensure /api/* is bypassed in your CDN config — see docs/deployment-behind-cdn.md.
Cached API responses cause observer-flap and incorrect dashboards.
```

This is informational — the request is not blocked. Treat it as a
prompt to run the verification curl above. The warning logs at most
once per process boot, regardless of how many CDN-fronted requests
arrive.

### Why this can't be fixed server-side

CDN cache policy is operator-controlled. The application emits the
most conservative cache header it can (`Cache-Control: no-store`),
but Cloudflare Cache Rules / Page Rules have higher precedence than
origin headers in many zone configurations. The only durable fix is
the operator-side bypass rule.

---

## Monitoring & Health Checks

### Docker health check

The container includes a built-in health check that hits `/api/stats`:

```bash
docker inspect --format='{{.State.Health.Status}}' corescope
```

Docker reports `healthy` or `unhealthy` automatically. The check runs every 30 seconds.

### Manual health check

```bash
curl -f http://localhost/api/stats
```

Returns JSON with packet counts, node counts, and version info:

```json
{
  "totalPackets": 56234,
  "totalNodes": 142,
  "totalObservers": 12,
  "packetsLastHour": 830,
  "packetsLast24h": 19644,
  "engine": "go",
  "version": "v3.4.1"
}
```

### Log monitoring

```bash
# All logs
docker compose logs -f

# Server only
docker compose logs -f | grep '\[server\]'

# Ingestor only
docker compose logs -f | grep '\[ingestor\]'
```

### Resource monitoring

```bash
docker stats corescope
```

---

## Backup & Restore

### Backup

All persistent data lives in `/app/data`. Back up both `meshcore.db` and
`ping_scores_history.db` together: the latter holds computed ping scores that
cannot be rebuilt after the underlying packets have passed retention.

**Both databases run in WAL mode**, so the newest commits can still live in a
`meshcore.db-wal` / `ping_scores_history.db-wal` sidecar instead of the `.db`
file. The image ships no `sqlite3` binary, so there is no online `.backup` to
run inside the container — stop it first. A clean shutdown checkpoints the WAL
into the main file and removes the sidecars, but a hard stop (the stop timeout
expiring into a `SIGKILL`) can leave them behind, so copy any `-wal` file that
is still there. Two rules make the difference between a usable backup and a
silently wrong one:

- **Never copy a `.db` without the `-wal` sitting next to it.** Everything the
  WAL holds is simply missing from the copy, and the result opens without an
  error.
- **Never let a `-wal` meet a different `.db`.** SQLite applies WAL frames by
  page number without checking which database file they came from, so a
  leftover sidecar overwrites the restored content and `PRAGMA
  integrity_check` still reports `ok`.

`-shm` is a scratch file rebuilt on open. Do not carry it into a backup, and
delete it alongside a stale `-wal` on restore.

Use the checked-in operator helper from a CoreScope checkout (host Docker
access is required). It works with either a named volume or a bind mount:

```sh
sh scripts/backup-sqlite.sh corescope /backups
```

The helper prints a new private directory such as
`/backups/corescope-backup-20261008T030000Z.A1b2c3`, containing exactly
`meshcore.db`, `ping_scores_history.db`, and their `-wal` files if present,
each with mode 0600.
There are no `-shm` files or config/secrets in the completed backup. Every run
gets a fresh directory, so a previous backup's WAL cannot be reused by mistake.

It copies the stopped container's **whole `/app/data` directory into temporary
0700 staging**, then keeps only those database files. Allow free temporary disk
space for the entire directory, including any unrelated files. The container
must be its only writer: stop any other process or container sharing that data
first. The helper never pulls an image or starts an additional container.

On a copy/validation failure, or on `SIGHUP`/`SIGINT`/`SIGTERM` (also when
the signal reaches the whole process group and kills `docker stop` mid-way),
it waits for any stop in progress and confirms that the container has exited,
restarts a previously running container, checks that it is running, and returns
the original error (129/130/143 for a signal, with an `interrupted` error). The
Docker CLI does not stay immune to a repeated `SIGINT`/`SIGTERM`, so where
`setsid` exists (util-linux, BusyBox) recovery runs its Docker commands in their
own session: a repeated Ctrl-C or process-group signal does not reach them.
A killed or failed recovery stop gets up to three attempts (this is what
covers a repeated signal without `setsid`, e.g. on macOS). If the stop still
cannot be confirmed, the helper attempts a start anyway but reports `restart not
verified`; a restart failure is reported too. `SIGKILL` of the helper itself is
not covered. No completed backup is published.
A container already stopped is left stopped. For success, both the copy and
any required restart must succeed. Temporary
staging is removed on normal exit or a caught signal; `SIGKILL` or a host crash
can leave a private `.corescope-backup-*` directory. That directory is **not** a
completed backup. Check the container's state and remove only that abandoned
staging directory after investigating. The helper cannot guarantee recovery
from a dead Docker daemon or host.

**Concurrent runs.** Invocations for the same container are serialised, so
a scheduled run that overlaps a manual one cannot restart the writer while the
other is still copying. Before it reads the container's running state, the
helper takes an exclusive lock keyed by the container's full ID: a name, the ID
and a short ID share one lock, whatever backup directory each run uses. The lock
is held through the stop, the copy, the restart and its check, and any
recovery, and is released on every exit path, including a caught signal. A
second run prints `Waiting up to …s for another backup of …` and waits up to
`CORESCOPE_BACKUP_LOCK_WAIT` (default 600 seconds); `0` gives up at once. It
then exits with status 75 without having read, stopped or started the
container.
The lock lives in a private `corescope-backup-locks` directory inside
`CORESCOPE_BACKUP_LOCK_DIR` (default `/tmp`); not in `$TMPDIR`, which differs
per user on macOS and under `sudo`. It is named
`corescope-backup-<container ID>.lock`. It is host-local: run every backup of a
container on its Docker host, as the same user, with the same lock directory
(a systemd unit with `PrivateTmp=yes` needs `CORESCOPE_BACKUP_LOCK_DIR` set to
a shared absolute path).
Because `/tmp` is writable by every user, a lock path directly in it could be
swapped for a symlink between the helper's check and its open. The helper
therefore creates `corescope-backup-locks` with mode 0700 and uses it only once
verified to be a directory of the current user with that mode, not a symlink;
no other user can create anything inside it. `CORESCOPE_BACKUP_LOCK_DIR` itself
must belong to root or that user and must not be writable by other users
unless it is sticky like `/tmp`, so that nobody else can rename the private
directory away. A `corescope-backup-locks` of another user, a symlink there, or
an unsafe lock directory makes the helper exit with status 1 before it touches
the container: one user's backups cannot then share, or be redirected by,
another user's locks. (A POSIX shell cannot open a file with `O_NOFOLLOW`,
and a directory named per user would let backups run as different users hold
different locks and overlap.) With `flock` (util-linux, BusyBox) the lock is a file
that the kernel releases when the helper exits; the file itself stays. Without
`flock` (macOS) it is a directory holding the owner's PID: a lock whose owner
no longer exists is removed automatically, one held by a live process (or
without a readable owner) never is. Remove a lock by hand (with a leftover
`….lock.reclaim` directory, which only a `SIGKILL` mid-reclaim leaves), or a
lock path of the wrong type that the helper reports, only once no backup of
that container is running.

Back up `config.json` (which may contain secrets) and `theme.json` separately
if wanted; the SQLite helper intentionally does not publish them.

### Restore

Restore both databases from **one complete, trusted backup directory**, with
their own WALs if present. Take a fresh backup of the current state first.
Set `RESTORE_DIR` to the completed directory (not private temporary staging):

```sh
export RESTORE_DIR=/backups/corescope-backup-20261008T030000Z.A1b2c3
export CONTAINER=corescope
```

Run the following block in a new shell. It validates the local input before
stopping, then clears exactly the two databases' stale WAL/SHM files. The
cleanup container uses the already-installed image and mounted data, works
with both bind mounts and named volumes, has no network and cannot pull an
image. No other writer may use the data, and the backup must not change during
the restore.

<!-- tested-sqlite-restore -->
```sh
set -eu
: "${RESTORE_DIR:?Set RESTORE_DIR to one complete backup directory}"
CONTAINER=${CONTAINER:-corescope}
case "$CONTAINER" in
  ''|-*|*[!a-zA-Z0-9_.-]*) echo 'invalid container name or ID' >&2; exit 2 ;;
esac
for db in meshcore ping_scores_history; do
  file="$RESTORE_DIR/$db.db"
  [ -f "$file" ] && [ ! -L "$file" ] && [ -r "$file" ] || {
    echo "missing or unreadable regular database $db.db" >&2; exit 1;
  }
  file="$RESTORE_DIR/$db.db-wal"
  if [ -e "$file" ] || [ -L "$file" ]; then
    [ -f "$file" ] && [ ! -L "$file" ] && [ -r "$file" ] || {
      echo "unreadable regular WAL $db.db-wal" >&2; exit 1;
    }
  fi
done
IMAGE=$(docker inspect --format '{{.Image}}' "$CONTAINER")
docker stop "$CONTAINER"
docker run --rm --pull=never --network none --volumes-from "$CONTAINER" \
  --entrypoint /bin/sh "$IMAGE" -c \
  'rm -f /app/data/meshcore.db-wal /app/data/meshcore.db-shm /app/data/ping_scores_history.db-wal /app/data/ping_scores_history.db-shm'
for db in meshcore ping_scores_history; do
  docker cp "$RESTORE_DIR/$db.db" "$CONTAINER:/app/data/$db.db"
  # Absence is optional; a failure copying an existing WAL is never ignored.
  if [ -e "$RESTORE_DIR/$db.db-wal" ]; then
    docker cp "$RESTORE_DIR/$db.db-wal" "$CONTAINER:/app/data/$db.db-wal"
  fi
done
docker start "$CONTAINER"
```

**On any restore error after stopping, leave the service stopped.** A failed
copy can have installed only part of the DB/WAL pair; blindly restarting can
lose data. Repair the copy problem, then rerun the full block with the same
complete backup (or your fresh pre-restore backup). Start only after every
cleanup and copy has succeeded. This is an offline, non-transactional operator
procedure, not an automatic rollback. A failure of the final `docker start`
also remains an error requiring an operator check.

### Automated backups

Schedule the same helper using an absolute checkout path and a private backup
directory. The account needs Docker access and enough disk space; alert on a
nonzero exit. Each successful run produces one matching snapshot directory,
not two independent flat-file backup sets. For example, daily at 03:00:

```cron
0 3 * * * sh /opt/corescope/scripts/backup-sqlite.sh corescope /backups
```

There is a downtime window for each run. Apply retention only to completed
`corescope-backup-*` directories created by this helper, keeping the whole
directory together. Do not use a broad `find /backups -name '*.db' -delete`,
which also removes unrelated backup files. No automatic pruning is performed
by the helper.

---

## Troubleshooting

### Container starts but dashboard is empty

This is normal on first start with no MQTT sources configured. The dashboard shows data once packets arrive via MQTT. Either:
- Run the bundled broker (`DISABLE_MOSQUITTO=false`, opt-in) and point a MeshCore gateway at it (port 1883)
- Configure an external MQTT source in `config.json`

### "no MQTT connections established" in logs

The ingestor couldn't connect to any MQTT broker. Check:
1. If you rely on the internal Mosquitto, is it running? It is opt-in: `DISABLE_MOSQUITTO` must be `false` (or `0`/`no`/`off`)
2. Is the external broker reachable? Test with `mosquitto_sub -h broker -t meshcore/#`
3. Are credentials correct in `config.json`?
4. Run `./manage.sh broker-check`, and look for `[broker]` lines in the ingestor log — with the bundled broker off and no external `mqttSources`, the ingestor is pointed at a broker that is not running.

### WebSocket disconnects / real-time updates stop

If behind a reverse proxy, ensure WebSocket upgrade headers are forwarded:
```nginx
proxy_http_version 1.1;
proxy_set_header Upgrade $http_upgrade;
proxy_set_header Connection "upgrade";
```

Also check proxy timeouts — set them to at least 300s for long-lived WebSocket connections.

### High memory usage

The in-memory packet store grows with retained packets. Configure retention limits in `config.json`:

```json
{
  "packetStore": {
    "retentionHours": 24,
    "maxMemoryMB": 512
  },
  "retention": {
    "nodeDays": 7,
    "packetDays": 30
  }
}
```

`packetStore.maxMemoryMB` bounds the store **and the caches that belong to it** — the decoded-packet cache, the path indexes, the resolved relay entries and the per-packet index entries, not just the stored rows. It is enforced in two places: the startup load stops at the budget, and the store evicts oldest-first when it exceeds it, down to 85% of it. Leaving it unset means no limit. Actual usage is on `/api/perf` as `packetStore.trackedMB`, next to `maxMB`.

`retention.packetDays` deletes old transmissions and their observations;
channel messages (including ping-triggering GRP_TXT packets) remain until
`retention.channelDays` when that is longer. Both prune paths deliberately
keep `ping_triggers` rows in `meshcore.db` beyond either cutoff. The trigger
is the detection index for an old ping; `ping_scores_history.db` holds its
computed score so it can still appear in all-time results. An old trigger
with no matching transmission is therefore expected after pruning, not an
integrity fault.

Do not "clean up" such a trigger. The server reconciles its history file
against a fresh `ping_triggers` read every cycle and deletes any entry whose
trigger has gone, so removing an old trigger destroys that ping's all-time
result permanently — the score does not survive in the history file.

A trigger whose transmission is missing *inside* the retention window is the
case worth investigating, so count the two separately. Pings are GRP_TXT
channel messages, so their effective cutoff is `N = max(packetDays,
channelDays)` days; substitute the number, as SQLite cannot parameterise a
date modifier. Run this read-only, on a stopped container or a consistent
snapshot:

```sql
SELECT
  COALESCE(SUM(p.first_seen <  strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-N days')), 0)
    AS expected_retained,
  COALESCE(SUM(p.first_seen >= strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-N days')), 0)
    AS investigate
FROM ping_triggers AS p
LEFT JOIN transmissions AS t ON t.id = p.tx_id
WHERE t.id IS NULL;
```

`ping_triggers.first_seen` is the same RFC3339 UTC string the prune compares
against, so the two buckets line up with the prune's own cutoff.
`expected_retained` is the designed state: it grows over time and is not
bounded by `packetDays`. `investigate` should be 0. List those rows with:

```sql
SELECT p.tx_id, p.first_seen, p.channel_hash, p.sender
FROM ping_triggers AS p
LEFT JOIN transmissions AS t ON t.id = p.tx_id
WHERE t.id IS NULL
  AND p.first_seen >= strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-N days')
ORDER BY p.first_seen DESC
LIMIT 20;
```

A row or two right at the cutoff can still be benign: a trigger's
`first_seen` is written once, at the first observation, while the
transmission's own `first_seen` can later be lowered by a relayed copy that
was received earlier, which can carry the transmission past the cutoff while
the trigger's copy still reads as newer. Rows well inside the window, a
missing trigger for a stored score, or a damaged history DB are the real
signals.

Any future size limit on `ping_triggers` needs a separate policy for
preserving all-time ping results.

### Database locked errors

SQLite doesn't support concurrent writers well. Ensure only one CoreScope instance accesses the database file. If running multiple containers, each needs its own database.

### Container unhealthy

Check logs: `docker compose logs --tail 50`. Common causes:
- Port 3000 already in use inside the container
- Database file permissions (must be writable by the container user)
- Corrupted database — restore from backup

### ARM / Raspberry Pi issues

- Use `linux/arm64` images (Pi 4 and 5). Pi 3 (armv7) is not supported.
- First pull may be slow — the multi-arch manifest selects the right image automatically.
- If memory is tight, set `packetStore.maxMemoryMB` to limit RAM usage.
