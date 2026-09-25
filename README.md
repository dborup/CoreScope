# CoreScope for MeshView

[![CI](https://github.com/dborup/CoreScope/actions/workflows/deploy.yml/badge.svg?branch=master)](https://github.com/dborup/CoreScope/actions/workflows/deploy.yml)

This repository contains the CoreScope fork used by MeshView, a community service for exploring the Danish MeshCore network. It builds on the [upstream CoreScope project](https://github.com/Kpa-clawbot/CoreScope) with changes maintained for MeshView.

**Explore MeshView:** [meshview.dk](https://meshview.dk) · [API documentation](https://meshview.dk/api/docs) · [OpenAPI specification](https://meshview.dk/api/spec)

CoreScope collects MeshCore packets from MQTT brokers, decodes them, and presents a web interface with live packet feeds, maps, channel messages, packet tracing, and node analytics. MeshView is the hosted service; this repository contains the software you can also run on your own infrastructure.

## Features

The screenshots below are examples inherited from upstream CoreScope. They illustrate the software's features and may differ from the current MeshView interface and data.

### Live Trace Map

Watch packet routes on an animated map, or use the VCR controls to replay recorded activity and move through the timeline.

![Upstream CoreScope example: live VCR playback](docs/screenshots/MeshVCR.gif)

### Packet Feed

Filter the packet stream, inspect individual bytes, resize table columns, and focus on your own nodes.

![Upstream CoreScope example: packets view](docs/screenshots/packets1.png)

### Network Overview

Explore node counts, packet volume, and observer coverage.

![Upstream CoreScope example: network overview](docs/screenshots/mesh-overview.png)

### Node Analytics

Inspect a node's activity, packet types, SNR, hop counts, neighboring nodes, and activity over time.

![Upstream CoreScope example: node analytics](docs/screenshots/node-analytics.png)

### Channel Messages

Read decoded group messages when the corresponding channel keys are available. Hashtag channel keys can be derived automatically.

![Upstream CoreScope example: channels](docs/screenshots/channels1.png)

### Mobile Interface

Use touch controls, mobile navigation, and the compact playback controls on a phone.

<img src="docs/screenshots/Live-view-iOS.png" alt="Upstream CoreScope example: live view on iOS" width="300">

### More Tools

- **Node directory** — search nodes, inspect their roles, view adverts, and open node details.
- **Packet tracing** — follow packets across observers with SNR and RSSI measurements.
- **Observer status** — inspect health, packet counts, and observer analytics.
- **Network analytics** — explore RF activity, topology, hash collisions, distance, routes, and scopes.
- **Multiple MQTT sources** — collect data from several brokers with source-specific region filters.
- **Themes and customization** — choose light or dark mode, adjust the interface, and export theme settings.
- **Shareable views** — link directly to nodes, packets, channels, and filtered views.

## Run Your Own Instance

### Build This Fork

You need Git and Docker. Build the image from this repository to run the MeshView fork:

```bash
git clone --branch master https://github.com/dborup/CoreScope.git
cd CoreScope
docker build -t corescope-meshview:local .
```

The `corescope-meshview:local` tag is created on your machine. The published `ghcr.io/kpa-clawbot/corescope` images belong to upstream CoreScope.

### Configure a Local Instance

The container can start with its built-in defaults. To configure the MQTT source explicitly, create a data directory and save the following as `data/config.json`. This example uses the broker included in the container and contains no MeshView deployment settings.

```bash
mkdir -p data
```

```json
{
  "mqttSources": [
    {
      "name": "local",
      "broker": "mqtt://localhost:1883",
      "topics": ["meshcore/#"]
    }
  ]
}
```

Start the container from the repository root:

```bash
docker run -d --name corescope-meshview \
  --restart=unless-stopped \
  -p 127.0.0.1:8080:80 \
  -p 127.0.0.1:1883:1883 \
  --mount "type=bind,source=$(pwd)/data,target=/app/data" \
  corescope-meshview:local
```

Open [http://localhost:8080](http://localhost:8080). The included Caddy proxy forwards HTTP requests to the Go server. The database and configuration persist in `./data`; packets appear once an observer publishes to the local MQTT broker.

This example exposes HTTP and MQTT on the Docker host's loopback interface. A publisher on that host can connect to `mqtt://localhost:1883`. To collect from an external broker, replace `mqttSources` with that broker's connection settings and restart the container. You can then disable the included broker with `-e DISABLE_MOSQUITTO=true` and omit the MQTT port mapping.

```bash
docker logs -f corescope-meshview
docker restart corescope-meshview
```

For a public deployment, configure your domain and HTTPS using Caddy or your own reverse proxy. The included broker allows anonymous connections; configure broker authentication before exposing it beyond the host. See [deployment documentation](docs/deployment.md) for the available components and options, using the locally built image above for this fork.

### Configuration Reference

The container reads `config.json` from the directory mounted at `/app/data`. [config.example.json](config.example.json) documents additional settings, including branding, map defaults, areas, channel keys, filters, and retention. Adapt individual settings to your own network: that file also contains upstream example brokers and geographic filters.

| Setting | Purpose |
|---------|---------|
| `mqttSources` | MQTT brokers, subscriptions, credentials, and optional `iataFilter` values. |
| `channelKeys` | Keys for channels you want to decode. Hashtag channel keys can be derived automatically. |
| `branding` | Instance name, tagline, logo, and related links. |
| `packetStore.maxMemoryMB` | Estimated in-memory packet-store budget; total process memory also includes other allocations. |
| `packetStore.retentionHours` | Packet history retained in memory. |
| `retention.packetDays` | Packet retention in SQLite, managed by the ingestor. |
| `apiKey` | Key for protected administration endpoints. Omit it to leave those endpoints disabled; use a unique strong key if you enable them. |

The standard container starts the server on internal port `3000` with database `/app/data/meshcore.db`; change the host-facing Docker port mapping to choose a different external port. If you set `DISABLE_CADDY=true`, publish internal port `3000` instead of `80` and let your own reverse proxy handle HTTP/HTTPS.

When running the Go binaries directly, use the server's `-port` and `-db` flags for explicit overrides. The server uses `DB_PATH` only when `dbPath` is absent from its configuration; `PORT` is not a server configuration override. `GOMEMLIMIT` and the `runtime` settings control the Go runtime's soft memory limit separately from the packet-store budget.

## Architecture

```text
MeshCore observers
        │
        ▼
MQTT broker(s) ──► Go ingestor ──► SQLite database
                                         │
                                         ▼
                                  Go HTTP server
                                ┌────────┴────────┐
                                │                 │
                          REST + static UI    WebSocket
                                │                 │
                                └────────┬────────┘
                                         ▼
                                Caddy / reverse proxy
                                         │
                                         ▼
                                      Browser
```

The ingestor owns packet ingestion, decoding, database migrations, and retention. The HTTP server opens SQLite read-only, maintains an indexed packet store with configurable memory and time limits, and polls for new data to broadcast over WebSocket. Responses use a combination of in-memory data, cached analytics, and SQLite queries; older packet windows can fall back to SQLite.

The frontend is plain HTML, CSS, and JavaScript served directly by Go, with no frontend build step. The Docker image runs the Go server and ingestor under supervisord and includes Mosquitto and Caddy as optional services.

Memory use and query latency depend on the dataset, enabled analytics, retention settings, and host. Tune the packet-store and runtime memory settings for your deployment rather than treating historical benchmark figures as guarantees for MeshView or other instances.

## Connect Observers

Configure a MeshCore observer and a compatible MQTT publisher, such as [meshcoretomqtt](https://github.com/Cisien/meshcoretomqtt), following that publisher's setup instructions. Point it at your broker and choose the appropriate IATA region code. Packet topics use the form `meshcore/{IATA}/{PUBKEY}/packets`; the `meshcore/#` subscription in the example above also receives related status topics.

## Project Structure

```text
CoreScope/
├── cmd/
│   ├── server/              # Read-only SQLite access, REST API, WebSocket, static UI
│   ├── ingestor/            # MQTT ingestion, decoding, SQLite writes and migrations
│   ├── decrypt/             # Channel decryption CLI
│   └── migrate/             # Database migration CLI, also used for test fixtures
├── internal/                # Shared Go packages
├── public/                  # HTML, CSS, and vanilla JavaScript
├── proto/                   # Protocol buffer definitions
├── docker/                  # Supervisor, Mosquitto, Caddy, and entrypoint configuration
├── Dockerfile               # Go build and Alpine runtime image
├── config.example.json      # Configuration reference with upstream examples
├── test-fixtures/           # Database fixtures for local and CI browser tests
├── test-*.js                # Frontend unit and browser tests
├── scripts/                 # Test, coverage, and maintenance scripts
└── tools/                   # Development utilities
```

## Development and Tests

Use a Go toolchain compatible with the module files, plus Node.js and npm for the frontend tests. Run these commands from the repository root:

```bash
# Go backend tests; each command leaves the shell in the repository root.
(cd cmd/server && go test ./...)
(cd cmd/ingestor && go test ./...)

# Frontend test dependencies and tests.
npm ci
npm run test:unit
npm test
```

`npm test` runs the JavaScript suites listed in [test-all.sh](test-all.sh). Go tests run separately.

Browser tests need Chromium and a running local Go server with a populated test database. For the fixture preparation, migrations, and server startup used by CI, see [.github/workflows/deploy.yml](.github/workflows/deploy.yml). Run browser tests against a local test instance, not the public MeshView service:

```bash
npx playwright install chromium
BASE_URL=http://localhost:3000 node test-e2e-playwright.js
```

Set `BASE_URL` to your local test server's port, or set `CHROMIUM_PATH` if using an existing Chromium installation. The CI workflow runs additional browser suites beyond this entry point.

The [CI workflow for this fork](https://github.com/dborup/CoreScope/actions/workflows/deploy.yml) runs tests and builds the container image locally in CI. Registry publishing, release uploads, deployment, and badge-file commits are restricted to the upstream repository by the workflow guards.

## Contributing and Upstream

Open issues and pull requests in [dborup/CoreScope](https://github.com/dborup/CoreScope). Read [AGENTS.md](AGENTS.md) for the project's development conventions and validation requirements.

CoreScope was developed in the [upstream Kpa-clawbot/CoreScope project](https://github.com/Kpa-clawbot/CoreScope). This fork retains that history and attribution while maintaining changes for MeshView. MeshCore firmware is developed separately at [meshcore-dev/MeshCore](https://github.com/meshcore-dev/MeshCore).

## License

[GPL-3.0-or-later](LICENSE).
