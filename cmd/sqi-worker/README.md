# sqi-worker

`sqi-worker` is the distributed task-execution agent for the sqi render farm. It
connects to a running `sqi-server`, registers its hardware capabilities, and pulls
task assignments from NATS JetStream, executing them as bare-metal OS processes.

---

## What it does

- **Discovers** `sqi-server` automatically via mDNS on the local network, or
  connects to an explicit NATS URL for cross-subnet and cloud deployments.
- **Registers** its capabilities at startup — OS, CPU count, RAM, GPU (where
  detectable), plus any manual tags from configuration such as `maya-2025` or
  `arnold-7`.
- **Pulls** task assignments over NATS JetStream and executes them concurrently;
  the server gates concurrency via CPU-core accounting.
- **Streams** task stdout and stderr back to `sqi-server` in real time so the
  web UI log viewer feels live.
- **Interprets** OpenJD progress directives (`openjd_progress`,
  `openjd_status`, `openjd_fail`) from process output.
- **Reports** task status transitions (`running` → `succeeded` / `failed` /
  `canceled`) and publishes a heartbeat every 15 s so the server can detect
  stale workers.
- **Shuts down gracefully** on SIGINT / SIGTERM: stops accepting new
  assignments, waits for in-flight tasks to finish (up to a configurable grace
  period), then force-kills any stragglers and deregisters from the server.

---

## Minimum configuration

`sqi-worker` requires a way to reach `sqi-server`'s embedded NATS broker. You
have two options:

### Option A — mDNS auto-discovery (local network, zero config)

mDNS is enabled by default. If `sqi-server` is running on the same LAN, no
configuration is needed:

```sh
sqi-worker start
```

The worker browses for `_sqi._tcp` advertisements for up to 5 s. If it finds
one, it uses that server's NATS address automatically.

### Option B — explicit NATS URL (cloud, cross-subnet, or no multicast)

Create a minimal config file at `config/sqi-worker.yaml` next to the binary,
or at `~/.sqi/sqi-worker.yaml`:

```yaml
nats:
  url: "nats://sqi-server.example.com:4222"

discovery:
  enable_mdns: false
```

Then start the worker:

```sh
sqi-worker start
```

Or pass the URL directly without a config file:

```sh
SQI_WORKER_NATS_URL=nats://sqi-server.example.com:4222 \
SQI_WORKER_DISCOVERY_ENABLE_MDNS=false \
  sqi-worker start
```

---

## Running against a local sqi-server

Start `sqi-server` in one terminal (default: NATS on `0.0.0.0:4222`, HTTP on
`0.0.0.0:8080`):

```sh
./bin/sqi-server serve
```

In a second terminal, start the worker pointing at it:

```sh
SQI_WORKER_NATS_URL=nats://127.0.0.1:4222 \
SQI_WORKER_DISCOVERY_ENABLE_MDNS=false \
SQI_WORKER_LOG_FORMAT=text \
SQI_WORKER_LOG_LEVEL=debug \
  ./bin/sqi-worker start
```

The worker logs its worker ID and the connected NATS URL at startup:

```
INFO sqi-worker starting worker_id=<uuid> worker_name=<hostname> ...
```

### Verify configuration before connecting

Use `--dry-run` to resolve configuration, detect capabilities, and print what
would be registered — without touching the server:

```sh
./bin/sqi-worker start --dry-run
```

Print the effective merged configuration at any time:

```sh
./bin/sqi-worker config print
```

---

## Verifying registration via the web UI

Once the worker starts successfully, confirm it is registered:

1. Open `http://localhost:8080` in a browser (or wherever `sqi-server` is
   running).
2. Navigate to **Workers** in the sidebar.
3. The worker should appear with status `online`, its capability tags, and the
   number of active / maximum concurrent tasks.

You can also confirm via the REST API:

```sh
curl -s http://localhost:8080/api/v1/workers | jq '.[].name'
```

---

## Quick reference

| Command | Description |
|---|---|
| `sqi-worker start` | Start the worker agent |
| `sqi-worker start --dry-run` | Validate config and capabilities without connecting |
| `sqi-worker start --nats-insecure-skip-verify` | Skip TLS cert verification (dev only) |
| `sqi-worker config print` | Print the effective merged configuration |
| `sqi-worker version` | Print version, commit, build date, and Go version |

---

## See also

- [`config/sqi-worker.example.yaml`](../../config/sqi-worker.example.yaml) — Fully commented example.
- [`docs/worker-configuration.md`](../../docs/worker-configuration.md) — Every configuration option.
- [`docs/worker-deployment.md`](../../docs/worker-deployment.md) — systemd, launchd, Windows service, Docker.
- [`docs/worker-capabilities.md`](../../docs/worker-capabilities.md) — Capability tag reference.
- [`docs/worker-docker.md`](../../docs/worker-docker.md) — Docker image quickstart.
