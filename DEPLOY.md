# Deploy CoreScope

Pre-built images are published to GHCR for `linux/amd64` and `linux/arm64` (Raspberry Pi 4/5).

## Quick Start

### Docker run

```bash
docker run -d --name corescope \
  -p 80:80 \
  -v corescope-data:/app/data \
  -e DISABLE_CADDY=true \
  ghcr.io/kpa-clawbot/corescope:latest
```

Open `http://localhost` — done.

### Docker Compose

```bash
curl -sL https://raw.githubusercontent.com/Kpa-clawbot/CoreScope/master/docker-compose.example.yml \
  -o docker-compose.yml
docker compose up -d
```

## Image Tags

| Tag | Description |
|-----|-------------|
| `v3.4.1` | Pinned release (recommended for production) |
| `v3.4` | Latest patch in v3.4.x |
| `v3` | Latest minor+patch in v3.x |
| `latest` | Latest release tag |
| `edge` | Built from master — unstable, for testing |

## Configuration

Settings can be overridden via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `DISABLE_CADDY` | `false` | Skip internal Caddy (set `true` behind a reverse proxy) |
| `DISABLE_MOSQUITTO` | `true` | Skip the bundled MQTT broker. The bundled broker is **opt-in** (#441): set `false` to run it. `./manage.sh`, the container entrypoint and the ingestor read the value the same way — `true`/`1`/`yes`/`on` and `false`/`0`/`no`/`off`, trimmed, any case; unset, empty or anything else means `true` (and an unrecognised value is reported). `./manage.sh broker-check` prints the effective mode. |
| `PROD_MQTT_BIND` | *(empty — every interface, IPv4 and IPv6)* | Host interface the bundled broker's `1883` is published on. Empty renders the mapping with no host interface, which is the unchanged dual-stack publish; `127.0.0.1` makes the plaintext broker reachable from the host only. An explicit `0.0.0.0` is **IPv4 only** — not the same as empty. |
| `PPROF_BIND` | `127.0.0.1` | Host interface the staging pprof ports `6060`/`6061` are published on (#441). Profile through an SSH tunnel (`ssh -L 6060:127.0.0.1:6060 -L 6061:127.0.0.1:6061 you@host`), or set `0.0.0.0` for remote access over IPv4. |
| `PPROF_HOST` | `127.0.0.1` | Host the in-process pprof listener binds (#441); `0.0.0.0` or `*` for every interface. **Not** a substitute for `PPROF_BIND`: a loopback listener inside a container is unreachable through a docker port mapping, which is why the staging compose files pin it to `0.0.0.0`. |
| `PPROF_PORT_SERVER` / `PPROF_PORT_INGESTOR` | `6060` / `6061` | Per-process pprof ports; they must stay distinct. Override the shared `PPROF_PORT`. |
| `MQTT_SOURCE_STRICT` | `false` | Make the ingestor refuse to start when its broker setup cannot ingest, instead of only logging it. |
| `MQTT_BROKER` / `MQTT_TOPIC` | *(empty — `config.json` decides)* | External broker URL and topic for the ingestor; `MQTT_BROKER` replaces the `mqttSources` in `config.json`. |

`MQTT_BROKER`, `MQTT_TOPIC`, `MQTT_SOURCE_STRICT`, `ENABLE_PPROF` and the
`PPROF_HOST*` / `PPROF_PORT*` settings are read by the server and the ingestor,
not by docker compose, so every app compose file forwards them into the
container through its `environment:` block with an empty default. Setting one
in `.env` therefore does reach the process; leaving it unset is inert. The
staging compose files pin `PPROF_HOST=0.0.0.0` instead, because they publish
the pprof ports.

Upgrading past #441 changes two defaults — the bundled broker and the pprof
bind — see [docs/deployment.md, "Upgrading past #441"](docs/deployment.md#upgrading-past-441--bundled-broker-opt-in-pprof-on-loopback).

One caveat: `MQTT_BROKER` **overrides** `mqttSources` in `config.json`. Before
the compose files forwarded it, a value in `.env` was ignored by the container;
now it decides. Run `./manage.sh broker-check` to see which source the ingestor
will use, and remove a stale `.env` line if `config.json` is the one you meant.
| `HTTP_PORT` | `80` | Host port mapping |
| `DATA_DIR` | `./data` | Host path for persistent data |

For advanced configuration, mount a `config.json` into `/app/data/config.json`. See `config.example.json` in the repo.

## Updating

```bash
docker compose pull
docker compose up -d
```

## Data

All persistent data lives in `/app/data`:
- `meshcore.db` — SQLite database (packets, nodes)
- `ping_scores_history.db` — SQLite database (computed ping scores)
- `config.json` — custom config (optional)
- `theme.json` — custom theme (optional)

**Backup:** both databases run in WAL mode, so a plain file copy while the
container runs can be incomplete or inconsistent. From a CoreScope checkout, run
the offline helper (it stops the container briefly and restarts it):
`sh scripts/backup-sqlite.sh corescope ~/backup`. With Compose, use the
container name `docker compose ps` shows. Details and restore:
[docs/deployment.md](docs/deployment.md#backup--restore).

## TLS

Option A — **External reverse proxy** (recommended): Run with `DISABLE_CADDY=true`, put nginx/traefik/Cloudflare in front.

Option B — **Built-in Caddy**: Mount a custom Caddyfile at `/etc/caddy/Caddyfile` and expose ports 80+443.

---

## Standalone MQTT broker (staging)

Starting in v3.7, `docker-compose.staging.yml` assumes a **standalone
`mqtt-broker` container** (image: `eclipse-mosquitto:2`) already runs
on the staging VM, out-of-band from this repo. That container:

- Owns port `8883` externally (TLS-terminated MQTT for real observers).
- Is attached to a shared docker network named `meshcore-net`.
- Is operator-managed state — it is **not** defined in any compose
  file in this repository. Its config, TLS certs, and ACLs live on the
  host, outside git.

`corescope-staging-go` reaches it in-network at `mqtt-broker:1883` via
docker DNS (no host port hop). To make that work, `docker-compose.staging.yml`
joins the external `meshcore-net` network and defaults `DISABLE_MOSQUITTO=true`
so the built-in mosquitto stays off.

### Prereq — one-time provisioning on the staging host

```bash
docker network create meshcore-net
# ...then bring up the operator-managed mqtt-broker container on that
# network (not covered here; that's operator state). THEN:
docker compose -f docker-compose.staging.yml up -d
```

If `meshcore-net` doesn't exist when compose starts, docker will refuse
to bring `staging-go` up (`external: true` — compose won't create it).

### Reverting to the old single-container behaviour

Third-party operators cloning this repo who want the legacy shape
(in-container mosquitto + `1883:1883` on the host, no external broker)
should override both the env default and re-add the port mapping.

In `.env` (or the shell):

```
DISABLE_MOSQUITTO=false
```

And in `docker-compose.staging.yml`, restore the `1883:1883` mapping
under `services.staging-go.ports`:

```yaml
    ports:
      - "${STAGING_GO_HTTP_PORT:-80}:80"
      - "${STAGING_GO_MQTT_PORT:-1883}:1883"   # ← re-added
      - "6060:6060"
      - "6061:6061"
```

That gives you back the pre-v3.7 self-contained staging shape. In that
mode you do **not** need `meshcore-net`, but note the compose file still
declares it as `external: true`, so either remove that declaration in
your fork or ensure the network exists.

---

## Migrating from manage.sh (existing admins)

If you're currently deploying with `manage.sh` (git clone + local build), you have two options going forward:

### Option A: Keep using manage.sh (no changes needed)

`manage.sh update` continues to work exactly as before — it fetches the latest tag, builds locally, and restarts. Nothing breaks.

```bash
./manage.sh update          # latest release
./manage.sh update v3.5.0   # specific version
```

### Option B: Switch to pre-built images (recommended)

Pre-built images skip the build step entirely — faster updates, no Go toolchain needed.

**One-time migration:**

1. Stop the current deployment:
   ```bash
   ./manage.sh stop
   ```

2. Your data is in `~/meshcore-data/` (or whatever `PROD_DATA_DIR` is set to). It's untouched — the database, config, and theme files persist.

3. Copy `docker-compose.example.yml` to where you want to run from:
   ```bash
   cp docker-compose.example.yml ~/docker-compose.yml
   ```

4. Start with the pre-built image:
   ```bash
   cd ~ && docker compose up -d
   ```

5. Verify it picked up your existing data:
   ```bash
   curl http://localhost/api/stats
   ```

**Updates after migration:**
```bash
docker compose pull && docker compose up -d
```

### What about manage.sh features?

| manage.sh command | Pre-built equivalent |
|---|---|
| `./manage.sh update` | `docker compose pull && docker compose up -d` |
| `./manage.sh stop` | `docker compose down` |
| `./manage.sh start` | `docker compose up -d` |
| `./manage.sh logs` | `docker compose logs -f` |
| `./manage.sh status` | `docker compose ps` |
| `./manage.sh setup` | Copy `docker-compose.example.yml`, edit env vars |

`manage.sh` remains available for advanced use cases (building from source, custom patches, development). Pre-built images are recommended for most production deployments.

## Staging VM — disk-usage monitor & cleanup (#1684)

The staging VM ran out of disk during a hot-patch (#1684). To prevent
repeats, two scripts live in `scripts/staging/`:

- `disk-monitor.sh <mount>` — reads `df -P`, classifies usage against
  `<80 ok / >=80 warn / >=90 error / >=95 alert`, emits to stderr +
  journald (via `logger`). Returns non-zero on `error|alert` so
  systemd surfaces the unit as failed.
- `disk-cleanup.sh` — removes `/tmp` snapshot files (`*.db`,
  `staging-snap.*`, `cs-*`, `node-compile-cache`) older than 7 days
  and runs `docker builder prune` + `docker image prune` with
  `--filter "until=72h" --filter "label!=keep"`. Set
  `CORESCOPE_CLEANUP_DRY_RUN=1` to log without deleting.

### Install on the staging host

SSH to `<STAGING_HOST>` as the staging operator user and:

```bash
sudo install -m 0755 scripts/staging/disk-monitor.sh  /usr/local/bin/corescope-disk-monitor
sudo install -m 0755 scripts/staging/disk-cleanup.sh  /usr/local/bin/corescope-disk-cleanup

# 15-minute monitor
sudo tee /etc/systemd/system/corescope-disk-monitor.service >/dev/null <<'UNIT'
[Unit]
Description=CoreScope staging disk-usage monitor (issue #1684)
[Service]
Type=oneshot
ExecStart=/usr/local/bin/corescope-disk-monitor /
UNIT

sudo tee /etc/systemd/system/corescope-disk-monitor.timer >/dev/null <<'UNIT'
[Unit]
Description=Run CoreScope disk-usage monitor every 15 minutes
[Timer]
OnBootSec=5min
OnUnitActiveSec=15min
Unit=corescope-disk-monitor.service
[Install]
WantedBy=timers.target
UNIT

# Daily cleanup at 03:30 local
sudo tee /etc/systemd/system/corescope-disk-cleanup.service >/dev/null <<'UNIT'
[Unit]
Description=CoreScope staging disk cleanup (issue #1684)
[Service]
Type=oneshot
ExecStart=/usr/local/bin/corescope-disk-cleanup
UNIT

sudo tee /etc/systemd/system/corescope-disk-cleanup.timer >/dev/null <<'UNIT'
[Unit]
Description=Run CoreScope disk cleanup daily at off-peak
[Timer]
OnCalendar=*-*-* 03:30:00
Persistent=true
Unit=corescope-disk-cleanup.service
[Install]
WantedBy=timers.target
UNIT

sudo systemctl daemon-reload
sudo systemctl enable --now corescope-disk-monitor.timer corescope-disk-cleanup.timer
```

`<STAGING_HOST>` is the staging VM hostname/IP — operator supplies it,
not committed to the repo.

### Inspecting alerts

```bash
journalctl -t corescope-disk-monitor   --since '-1d'
journalctl -t corescope-disk-cleanup   --since '-7d'
systemctl list-timers | grep corescope-disk
```

`logger` priorities map: `ok→info`, `warn→warning`, `error→err`,
`alert→alert` (syslog severity 1, the highest level). Wire
`journalctl -p alert ...` to whatever ops channel the operator
prefers; use `-p err` to also catch the `error` tier.

### Notes on `staging-snap.db` root cause (#1684 phase 3)

`grep -rn staging-snap.db cmd/ public/ scripts/` returns **zero**
hits in the repo. The 4.4 GB orphan was a manual debugging artifact,
not produced by any committed code. The `disk-cleanup.sh` retention
rule (anything matching `staging-snap.*` in `/tmp` older than 7 days)
prevents recurrence without needing source-side TTL changes.

If a future feature legitimately needs persistent snapshot DBs, put
them under `/var/lib/corescope/snapshots/` with explicit rotation —
not in `/tmp`, which is ephemeral by definition.

## Broker preflight (`./manage.sh broker-check`)

The bundled broker is opt-in, and turning it off (the default,
`DISABLE_MOSQUITTO=true`) does not configure a replacement. With no `mqttSources` the ingestor falls back to its built-in
`mqtt://localhost:1883` — the bundled broker's own address, where nothing is
then listening — and ingests nothing while the container still reports
healthy.

```bash
./manage.sh broker-check
```

It prints the broker mode and every configured source, and exits `1` when
the arrangement cannot ingest: no source configured, or every source
pointing at loopback with the bundled broker off. Broker URLs are masked, so
the output is safe to paste into an issue.

`setup`, `start`, `update`, `restore` and `promote` run the same check and
print the same report, but never refuse the command over it — the broker
topology differs per host and is the operator's call. Set
`MQTT_SOURCE_STRICT=true` if you want the ingestor itself to refuse to start
in that state.

See [docs/deployment.md](docs/deployment.md#migration--pulling-the-new-compose-file)
for what an existing install should expect after pulling the new Compose file
(short answer: no change — every new setting defaults to today's behaviour).
