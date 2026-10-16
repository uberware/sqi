# `deploy/`

Deployment artifacts for `sqi`:

- `deploy/docker/` — Dockerfiles for the server and worker images.
- `deploy/docker-compose.yml` — user-facing all-in-one stack (server + worker),
  state in embedded SQLite.
- `deploy/docker-compose.postgres.yml` — the same stack with its state store on
  a PostgreSQL 18 container (see below).
- `deploy/docker-compose.smoke.yml` — minimal stack used by CI smoke tests.
- `deploy/systemd/` — sample unit files for bare-metal Linux worker installs.
- `deploy/config/` — pointer to the canonical example configs under `config/`.

## PostgreSQL

`deploy/docker-compose.postgres.yml` runs PostgreSQL 18 beside the server and
worker and sets `store.driver: postgres`, so the server creates its schema
there on first start:

```sh
export SQI_POSTGRES_PASSWORD='choose-a-strong-password'
docker compose -f deploy/docker-compose.postgres.yml up -d --build
```

The password is required; it sets the database role's password and reaches the
server as `PGPASSWORD`, so the DSN in the file carries none. `--build` builds
the images from this checkout, because PostgreSQL support is not yet in a
published release (the latest is `v0.3.0`); once a release includes it, pin
its tag in the file and drop `--build`.

Backups run `pg_dump` inside the database container. Restores run
`sqi-server restore` into an empty database (created with `createdb`), from a
one-off container of the `postgres` service, because the `sqi-server` image
has no `pg_restore`. The commands for both are in the file's header;
[Restore](../docs/operations.md#restore) explains what a restore does and what
runs again afterwards. To use an existing PostgreSQL server instead of the
container, see
[`docs/operations.md`](../docs/operations.md#running-on-postgresql).

## TLS

Every artifact here deploys sqi in its **plaintext default**: the REST API, the
WebSocket gateway and the embedded broker all serve unencrypted. That is
deliberate — TLS is opt-in — but it means none of these files is production-ready
as shipped on an untrusted network.

To add TLS, generate material with `sqi-server tls init`, mount it into the
container or place it on the host, and set `http.tls` and `nats.tls` (plus the
matching worker CA settings). The full guide, including the fact that enabling
TLS on a running farm is a coordinated restart rather than a rolling one, is in
[`docs/tls.md`](../docs/tls.md).

Terminating TLS at a reverse proxy in front of the API is equally supported —
but note it does nothing for the broker, which workers connect to directly.

Kubernetes manifests / a Helm chart for production-mode deployments are planned
for a later phase and are not shipped yet.

Goreleaser configuration lives at the repo root as `.goreleaser.yaml` to match
tool convention.
