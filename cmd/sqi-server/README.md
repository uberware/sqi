# sqi-server

`sqi-server` is the control plane for a `sqi` render farm. A single binary runs
the job scheduler, REST API, WebSocket gateway, embedded NATS JetStream broker,
and embedded SQLite state store. Workers connect to it over NATS; clients
interact with it over HTTP and WebSocket.

---

## What it does

| Component | Responsibility |
|---|---|
| **Scheduler** | Assigns ready tasks to available workers, enforcing priority, capability matching, compute-location affinity, and usage pool limits. |
| **REST API** | `/api/v1/…` endpoints for job submission, status queries, worker management, and system configuration. |
| **WebSocket gateway** | `/api/v1/ws` endpoint for real-time push of job, task, worker, and log events to subscribed clients. |
| **Embedded NATS** | In-process JetStream broker used for work assignment, status reporting, log streaming, and worker heartbeats. |
| **SQLite store** | Single-file database holding all durable state — jobs, tasks, workers, farms, queues, usage pools, and audit log. |
| **Web UI host** | Serves the embedded SPA at the root (`/`, with SPA fallback for extensionless paths; the legacy `/ui/*` prefix still resolves) and the OpenAPI spec at `/api/v1/openapi.yaml`. |
| **mDNS responder** | Advertises `_sqi._tcp` on the local network so workers and the CLI can discover the server without manual address configuration. |

---

## Build

**Prerequisites:** Go 1.26 or newer (the `go` directive in `go.mod` pins 1.26.3).

```sh
# Build just the server binary into ./bin/
make build-server

# Or build both server and worker
make build

# Or build directly with go
go build -o bin/sqi-server ./cmd/sqi-server
```

Cross-compiled binaries for all target platforms (Linux, macOS, Windows ×
amd64/arm64) can be produced with:

```sh
make build-all
```

---

## Run

```sh
# Start with all defaults (listens on 0.0.0.0:8080, SQLite at ./sqi.db)
./bin/sqi-server serve

# Start with a config file
./bin/sqi-server serve --config config/sqi-server.yaml

# Override specific values via flags or environment variables
SQI_LOG_FORMAT=text ./bin/sqi-server serve --http-addr 127.0.0.1:8090

# Print the effective merged configuration and exit (useful for debugging)
./bin/sqi-server config print

# Show build metadata
./bin/sqi-server version
```

The first time the server starts it creates the SQLite database and runs
all pending migrations automatically. No manual schema setup is required.

See [`config/sqi-server.example.yaml`](../../config/sqi-server.example.yaml)
for a fully commented configuration file, and
[`docs/configuration.md`](../../docs/configuration.md) for a complete reference
of every option.

---

## Smoke test

The following sequence verifies the server is healthy, accepts job submissions,
and returns expected responses. It requires `curl` and `jq`.

**1. Start the server**

```sh
./bin/sqi-server serve &
SERVER_PID=$!
sleep 1
```

**2. Liveness and readiness**

```sh
curl -sf http://localhost:8080/healthz   # expects: {"status":"ok"}
curl -sf http://localhost:8080/readyz    # expects: {"status":"ok","checks":{"nats":"ok","sqlite":"ok"}}
```

**3. Submit a minimal OpenJD job**

```sh
FARM=$(curl -sf http://localhost:8080/api/v1/farms | jq -r '.[0].id')
QUEUE=$(curl -sf "http://localhost:8080/api/v1/queues?farm_id=$FARM" | jq -r '.items[0].id')

JOB_ID=$(curl -sf -X POST \
  "http://localhost:8080/api/v1/jobs?farm_id=$FARM&queue_id=$QUEUE&owner=smoke" \
  -H 'Content-Type: application/yaml' \
  --data-binary '
specificationVersion: "jobtemplate-2023-09"
name: smoke-test
steps:
  - name: hello
    script:
      actions:
        onRun:
          command: echo
          args: ["hello from sqi"]
' | jq -r '.id')

echo "Submitted job: $JOB_ID"
```

**4. Query the job**

```sh
curl -sf http://localhost:8080/api/v1/jobs/$JOB_ID | jq '{id, name, status}'
```

**5. List tasks**

```sh
curl -sf http://localhost:8080/api/v1/jobs/$JOB_ID/tasks | jq '[.items[] | {id, status}]'
```

**6. Check Prometheus metrics**

```sh
curl -sf http://localhost:8080/metrics | grep sqi_
```

**7. Verify the OpenAPI spec is served**

```sh
curl -sf http://localhost:8080/api/v1/openapi.yaml | head -5
```

**8. Stop the server**

```sh
kill $SERVER_PID
```

---

## Database migrations

Schema migrations are embedded in the binary and applied automatically at
startup. To manage migrations manually:

```sh
# Show current migration status
./bin/sqi-server migrate status

# Apply all pending migrations
./bin/sqi-server migrate up

# Roll back the most recent migration
./bin/sqi-server migrate down
```

Migration SQL files live in
[`internal/store/migrations/`](../../internal/store/migrations/).

---

## Package layout

```
cmd/sqi-server/
└── main.go            Entry point — wires the cobra command tree, exits.

internal/
├── bus/               Typed NATS JetStream client wrapper and subject definitions.
├── config/            Config struct, layered loader, and startup validator.
├── discovery/         mDNS responder (grandcat/zeroconf).
├── health/            /healthz and /readyz handlers.
├── log/               slog-based structured logger and request-scoped middleware.
├── metrics/           Prometheus metric definitions and the /metrics handler.
├── middleware/        HTTP middleware (recovery, CORS, request ID, gzip, logger).
├── openjd/            OpenJD (jobtemplate-2023-09) parser, validator, parameter-space expansion.
├── scheduler/         Assignment loop, worker registry, heartbeat sweep, retry/failure policy.
├── server/            HTTP server wiring — router, middleware stack, boot sequence.
├── store/             Store interface, SQLite implementation, migration runner, task state machine.
├── ui/                Embedded web/dist assets, SPA fallback handler.
├── version/           Build metadata (version, commit, build date, Go version).
├── worker/            Worker wire protocol handlers and log ingestion.
└── ws/                WebSocket upgrade, subscription hub, NATS fanout.
```

---

## Further reading

- [`docs/architecture.md`](../../docs/architecture.md) — Component layout and job lifecycle data flow.
- [`docs/configuration.md`](../../docs/configuration.md) — Complete configuration reference.
- [`docs/api.md`](../../docs/api.md) — REST API reference with worked examples.
- [`docs/tls.md`](../../docs/tls.md) — In-process TLS for the API and broker, `tls init` / `tls issue`, and worker mTLS.
- [`docs/development.md`](../../docs/development.md) — Local setup and contribution guide.
