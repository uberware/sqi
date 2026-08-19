# sqi-worker Docker Guide

This document covers running `sqi-worker` as a Docker container, including
the image layout, required environment variables, volume mounts, network
requirements, and a `docker run` quickstart.

---

## Image

`sqi-worker` Docker images are published to the GitHub Container Registry:

```
ghcr.io/uberware/sqi/sqi-worker:<tag>
```

| Tag | Description |
|---|---|
| `latest` | Rolling build of the `main` branch — republished on every push to `main` (may be ahead of the last tagged release) |
| `v0.3.0` | A specific tagged release |
| `v0` | Rolling major-version tag — the newest `v0.x` release |

For production, pin a specific release tag (`v0.3.0`) for reproducibility.
There is no `main` tag; the `latest` tag tracks the `main` branch.

Pull the image:

```sh
docker pull ghcr.io/uberware/sqi/sqi-worker:latest
```

### Image layout

The image is based on **Alpine 3.21** with the following additions:

- `ca-certificates` — required for TLS connections to NATS over the network.
- `tzdata` — required for timezone-aware job scheduling.
- A dedicated non-root user `sqiworker` runs the worker process.
- The worker binary is installed at `/usr/local/bin/sqi-worker`.
- `/var/lib/sqi-worker` is created and owned by `sqiworker` — mount a volume
  here for worker ID persistence. The image already pins
  `SQI_WORKER_DATA_DIR=/var/lib/sqi-worker` (the binary's built-in default is
  `~/.sqi/worker`), so you only need to set it yourself when mounting the volume
  at a different path.

The entrypoint is:

```
ENTRYPOINT ["/usr/local/bin/sqi-worker"]
CMD ["start"]
```

Running the container without arguments starts the worker agent. Pass
`version` or `config print` as arguments to use other subcommands.

### Exposed port

| Port | Protocol | Purpose |
|---|---|---|
| `9091` | TCP | Local HTTP server: `/healthz`, `/readyz`, `/metrics` |

---

## Required environment variables

The only strictly required environment variable is the NATS server address
(when mDNS auto-discovery is not available, which is the common case in
container environments):

| Variable | Description |
|---|---|
| `SQI_WORKER_NATS_URL` | URL of the NATS server embedded in `sqi-server`, e.g. `nats://sqi-server:4222` |
| `SQI_WORKER_DISCOVERY_ENABLE_MDNS` | Set to `false` to disable mDNS (required in container networks that prohibit multicast) |

Strongly recommended when mounting a data volume:

| Variable | Description |
|---|---|
| `SQI_WORKER_DATA_DIR` | Set to the volume mount path (`/var/lib/sqi-worker`) so the worker ID is written to the persistent volume rather than the default `~/.sqi/worker` |

Useful optional variables for container deployments:

| Variable | Description |
|---|---|
| `SQI_WORKER_NAME` | Human-readable worker name shown in the web UI (defaults to the container hostname) |
| `SQI_WORKER_CAPABILITY_TAGS` | Comma-separated capability tags merged with auto-detected ones, e.g. `maya-2025,gpu` |
| `SQI_WORKER_COMPUTE_LOCATION` | Compute-location name for multi-site farms (see [Compute locations](compute-locations.md)) |
| `SQI_WORKER_FARM_ID` | Restrict the worker to a single farm (empty = accept tasks from any farm) |
| `SQI_WORKER_QUEUE_IDS` | Comma-separated queue IDs the worker serves (empty = all queues) |
| `SQI_WORKER_LOG_FORMAT` | `json` (default) or `text` |
| `SQI_DIAGNOSTICS_ENABLED` | `true` (default) mirrors the worker's own logs to the server's diagnostics view; set `false` to disable |

All other configuration options have sensible defaults for container
deployments. See
[`docs/worker-configuration.md`](worker-configuration.md) for the full list.
Note that the operator-owned `staging.scratch_dir` / `staging.sync_command`
settings (used for `stage_locally` path delivery) have **no** environment-variable
form — they must be supplied via a mounted config file.

---

## Volume mounts

### Worker data directory (required for ID persistence)

Mount a named volume or host path at `/var/lib/sqi-worker` and point
`SQI_WORKER_DATA_DIR` at it to persist the worker ID across container restarts.
Without this, every container restart creates a new worker ID and the old
record in `sqi-server` is orphaned (it will be swept up by the heartbeat
timeout after ~30 s, but the orphaned record accumulates in the database).

```sh
docker run \
  -e SQI_WORKER_DATA_DIR=/var/lib/sqi-worker \
  -v sqi-worker-data:/var/lib/sqi-worker \
  ghcr.io/uberware/sqi/sqi-worker:latest
```

### DCC software and shared storage

If the tasks executed by this worker require access to DCC tools (Maya,
Houdini, etc.) or shared NFS/SMB storage, bind-mount those paths into the
container:

```sh
docker run \
  -v /opt/maya:/opt/maya:ro \
  -v /mnt/nas/studio:/mnt/nas/studio \
  ghcr.io/uberware/sqi/sqi-worker:latest
```

> **Note:** The Alpine-based worker image intentionally does not include any
> DCC software. You are responsible for providing the appropriate base image
> or bind mounts for the software your tasks require.

### Named storage locations

If your jobs reference [storage locations](storage-locations.md) by name
(`loc://…`), each worker resolves those names to concrete paths using its
`SQI_WORKER_COMPUTE_LOCATION`. Bind-mount the real path for this worker's
location and set the matching compute-location name:

```sh
docker run \
  -e SQI_WORKER_COMPUTE_LOCATION=cloud_linux \
  -v /mnt/nas/studio:/mnt/studio \
  ghcr.io/uberware/sqi/sqi-worker:latest
```

For **staged** access to object storage (the `stage_locally` path-translation
mode — see [S3-compatible storage](storage-s3.md)), the worker invokes an
operator-provided `staging.sync_command` before and after each task. sqi moves
no bytes itself, so that sync tool (`aws`, `rclone`, `mc`, `rsync`, …) must be
present in the image — add it to a custom image built `FROM` this one — and the
`staging.scratch_dir` / `staging.sync_command` settings must come from a mounted
config file (they have no environment-variable form).

---

## Network requirements

The worker container must be able to reach the **NATS port** of `sqi-server`
(default `4222`). No inbound connections from the server to the worker are
required — all communication is worker-initiated over NATS.

| Direction | Protocol | Port | Purpose |
|---|---|---|---|
| Worker → Server | TCP | `4222` | NATS — core-NATS work leases (task assignments) plus JetStream (task status, logs, heartbeat, registration) |
| (optional) Prometheus → Worker | TCP | `9091` | Metrics scraping — only if metrics are exposed |

### Docker networking

In a Docker Compose or standalone Docker setup where `sqi-server` and
`sqi-worker` are on the same Docker network, use the service name as the
hostname:

```yaml
services:
  sqi-server:
    image: ghcr.io/uberware/sqi/sqi-server:latest
    ports:
      - "8080:8080"
      - "4222:4222"

  sqi-worker:
    image: ghcr.io/uberware/sqi/sqi-worker:latest
    environment:
      SQI_WORKER_NATS_URL: "nats://sqi-server:4222"
      SQI_WORKER_DISCOVERY_ENABLE_MDNS: "false"
      SQI_WORKER_DATA_DIR: "/var/lib/sqi-worker"
    volumes:
      - sqi-worker-data:/var/lib/sqi-worker
    depends_on:
      - sqi-server

volumes:
  sqi-worker-data:
```

> A ready-to-run version of this stack ships at
> [`deploy/docker-compose.yml`](https://github.com/uberware/sqi/blob/main/deploy/docker-compose.yml)
> (server + worker, with health checks and persistent volumes).

---

## `docker run` quickstart

### Minimal — mDNS disabled, explicit NATS URL

```sh
docker run -d \
  --name sqi-worker \
  -e SQI_WORKER_NATS_URL=nats://sqi-server:4222 \
  -e SQI_WORKER_DISCOVERY_ENABLE_MDNS=false \
  -e SQI_WORKER_DATA_DIR=/var/lib/sqi-worker \
  -v sqi-worker-data:/var/lib/sqi-worker \
  ghcr.io/uberware/sqi/sqi-worker:latest
```

### With capability tags and worker name

```sh
docker run -d \
  --name render-worker-01 \
  -e SQI_WORKER_NATS_URL=nats://sqi-server:4222 \
  -e SQI_WORKER_DISCOVERY_ENABLE_MDNS=false \
  -e SQI_WORKER_DATA_DIR=/var/lib/sqi-worker \
  -e SQI_WORKER_NAME=render-worker-01 \
  -e SQI_WORKER_CAPABILITY_TAGS=blender-4.2,cpu-render \
  -e SQI_WORKER_LOG_FORMAT=text \
  -v sqi-worker-data:/var/lib/sqi-worker \
  ghcr.io/uberware/sqi/sqi-worker:latest
```

### With TLS

```sh
docker run -d \
  --name sqi-worker \
  -e SQI_WORKER_NATS_URL=nats://sqi-server:4222 \
  -e SQI_WORKER_DISCOVERY_ENABLE_MDNS=false \
  -e SQI_WORKER_NATS_TLS_CERT_FILE=/run/secrets/nats-client.crt \
  -e SQI_WORKER_NATS_TLS_KEY_FILE=/run/secrets/nats-client.key \
  -e SQI_WORKER_NATS_TLS_CA_FILE=/run/secrets/nats-ca.crt \
  -e SQI_WORKER_DATA_DIR=/var/lib/sqi-worker \
  -v sqi-worker-data:/var/lib/sqi-worker \
  -v /path/to/certs:/run/secrets:ro \
  ghcr.io/uberware/sqi/sqi-worker:latest
```

### Exposing metrics for Prometheus scraping

```sh
docker run -d \
  --name sqi-worker \
  -e SQI_WORKER_NATS_URL=nats://sqi-server:4222 \
  -e SQI_WORKER_DISCOVERY_ENABLE_MDNS=false \
  -e SQI_WORKER_METRICS_ADDR=0.0.0.0:9091 \
  -e SQI_WORKER_DATA_DIR=/var/lib/sqi-worker \
  -p 9091:9091 \
  -v sqi-worker-data:/var/lib/sqi-worker \
  ghcr.io/uberware/sqi/sqi-worker:latest
```

---

## Health probes

The worker exposes standard health endpoints useful for container orchestrators:

```sh
# Liveness — returns 200 when the process is running
curl -sf http://localhost:9091/healthz

# Readiness — returns 503 when the NATS connection is not established
curl -sf http://localhost:9091/readyz
```

### Docker HEALTHCHECK

The Alpine-based image does not include `curl`. Use `wget` (provided by Alpine
BusyBox) for in-container health checks.

Add a health check to your `docker run` command or Compose file:

```sh
docker run -d \
  --health-cmd 'wget -qO /dev/null http://localhost:9091/readyz || exit 1' \
  --health-interval 15s \
  --health-timeout 5s \
  --health-retries 3 \
  ...
  ghcr.io/uberware/sqi/sqi-worker:latest
```

Or in `docker-compose.yml`:

```yaml
services:
  sqi-worker:
    image: ghcr.io/uberware/sqi/sqi-worker:latest
    healthcheck:
      test: ["CMD-SHELL", "wget -qO /dev/null http://localhost:9091/readyz || exit 1"]
      interval: 15s
      timeout: 5s
      retries: 3
      start_period: 10s
```

---

## Running as root

The container image runs as the `sqiworker` non-root user by default. If your
DCC software or file mounts require root privileges inside the container, set
`SQI_WORKER_ALLOW_ROOT=true` in addition to overriding the container user:

```sh
docker run \
  --user root \
  -e SQI_WORKER_ALLOW_ROOT=true \
  ...
```

This is acceptable when the container's root does not map to the host root
(e.g., using user namespaces).

---

## Building the image locally

```sh
docker build \
  -f deploy/docker/worker/Dockerfile \
  --build-arg VERSION=dev \
  --build-arg COMMIT=$(git rev-parse --short HEAD) \
  --build-arg BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
  --build-arg GOVERSION=$(go version | awk '{print $3}') \
  -t sqi-worker:dev \
  .
```

---

## See also

- [`docs/worker-deployment.md`](worker-deployment.md) — Bare-metal Linux, macOS, and Windows deployment.
- [`docs/worker-configuration.md`](worker-configuration.md) — Full configuration reference.
- [`deploy/docker/worker/Dockerfile`](https://github.com/uberware/sqi/blob/main/deploy/docker/worker/Dockerfile) — Image source.
- [`deploy/docker-compose.smoke.yml`](https://github.com/uberware/sqi/blob/main/deploy/docker-compose.smoke.yml) — Smoke-test Compose file for server + worker.
