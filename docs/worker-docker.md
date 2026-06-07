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
| `latest` | Most recent stable release |
| `v1.2.3` | Specific release version |
| `main` | Build from the `main` branch (may be unstable) |

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
  here for worker ID persistence.

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

All other configuration options have sensible defaults for container
deployments. See
[`docs/worker-configuration.md`](worker-configuration.md) for the full list.

---

## Volume mounts

### Worker data directory (required for ID persistence)

Mount a named volume or host path at `/var/lib/sqi-worker` to persist the
worker ID across container restarts. Without this volume, every container
restart creates a new worker ID and the old record in `sqi-server` is orphaned
(it will be swept up by the heartbeat timeout after ~30 s, but the orphaned
record accumulates in the database).

```sh
docker run \
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

---

## Network requirements

The worker container must be able to reach the **NATS port** of `sqi-server`
(default `4222`). No inbound connections from the server to the worker are
required — all communication is worker-initiated over NATS.

| Direction | Protocol | Port | Purpose |
|---|---|---|---|
| Worker → Server | TCP | `4222` | NATS JetStream (task assignments, status, logs, heartbeat) |
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
    volumes:
      - sqi-worker-data:/var/lib/sqi-worker
    depends_on:
      - sqi-server

volumes:
  sqi-worker-data:
```

---

## `docker run` quickstart

### Minimal — mDNS disabled, explicit NATS URL

```sh
docker run -d \
  --name sqi-worker \
  -e SQI_WORKER_NATS_URL=nats://sqi-server:4222 \
  -e SQI_WORKER_DISCOVERY_ENABLE_MDNS=false \
  -v sqi-worker-data:/var/lib/sqi-worker \
  ghcr.io/uberware/sqi/sqi-worker:latest
```

### With capability tags and worker name

```sh
docker run -d \
  --name render-worker-01 \
  -e SQI_WORKER_NATS_URL=nats://sqi-server:4222 \
  -e SQI_WORKER_DISCOVERY_ENABLE_MDNS=false \
  -e SQI_WORKER_NAME=render-worker-01 \
  -e SQI_WORKER_MAX_CONCURRENT_TASKS=2 \
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
  -t sqi-worker:dev \
  .
```

---

## See also

- [`docs/worker-deployment.md`](worker-deployment.md) — Bare-metal Linux, macOS, and Windows deployment.
- [`docs/worker-configuration.md`](worker-configuration.md) — Full configuration reference.
- [`deploy/docker/worker/Dockerfile`](../deploy/docker/worker/Dockerfile) — Image source.
- [`deploy/docker-compose.smoke.yml`](../deploy/docker-compose.smoke.yml) — Smoke-test Compose file for server + worker.
