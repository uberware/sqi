# Quickstart

Get a working sqi farm running and your first job submitted in a few minutes.
Two paths: native binaries (recommended for a real farm) or Docker Compose
(fastest to try). Both end at the same place: the web UI at
http://localhost:8080.

## Option A — Binaries (all-in-one, simple mode)

1. **Download and extract** the release archive for your platform from
   https://github.com/uberware/sqi/releases/latest (see
   [worker deployment](worker-deployment.md#pre-built-binaries) for the exact
   commands). Each archive contains both `sqi-server` and `sqi-worker`.

2. **Start the server** (scheduler, REST API, web UI, embedded NATS, SQLite):
   ```sh
   ./sqi-server serve
   ```
   It listens on http://localhost:8080 (UI) and NATS on :4222 with zero config.

3. **Start a worker** on the same or another machine. On a LAN it finds the
   server automatically via mDNS:
   ```sh
   ./sqi-worker start
   ```
   Off-LAN or in doubt, point it explicitly:
   ```sh
   ./sqi-worker start --nats-url nats://<server-host>:4222
   ```

4. Continue to **"Submit your first job"** below.

## Option B — Docker Compose

```sh
curl -LO https://raw.githubusercontent.com/uberware/sqi/v0.1.0/deploy/docker-compose.yml
docker compose -f docker-compose.yml up -d
```

This starts a server and one worker, wired together. Open
http://localhost:8080. The worker container has no DCC software — for real
render work, build a custom worker image or bind-mount your tools (see
[worker Docker guide](worker-docker.md)).

## Submit your first job

You need a farm and a queue before submitting. Create them in the web UI
(**Farms** → create, then **Queues** → create), or via the API:

```sh
FARM=$(curl -s -X POST localhost:8080/api/v1/farms \
  -H 'content-type: application/json' -d '{"name":"demo"}' | jq -r .id)

QUEUE=$(curl -s -X POST localhost:8080/api/v1/queues \
  -H 'content-type: application/json' \
  -d "{\"farm_id\":\"$FARM\",\"name\":\"default\"}" | jq -r .id)
```

Grab the sample job template:

```sh
curl -LO https://raw.githubusercontent.com/uberware/sqi/v0.1.0/docs/examples/hello.json
```

Submit it (the OpenJD template is the raw request body; farm, queue, and owner
go as query parameters):

```sh
JOB=$(curl -s -X POST \
  "localhost:8080/api/v1/jobs?farm_id=$FARM&queue_id=$QUEUE&owner=quickstart" \
  -H 'content-type: application/json' \
  --data-binary @hello.json | jq -r .id)

curl -s localhost:8080/api/v1/jobs/$JOB/tasks
```

Watch it run in the web UI (**Jobs**), including live logs. That's a complete
farm.

## Next steps

- [Worker deployment](worker-deployment.md) — run workers as services.
- [Configuration](configuration.md) — every setting.
- [Python client](python-client.md) — scripted submission.
