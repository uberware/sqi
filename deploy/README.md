# `deploy/`

Deployment artifacts for `sqi`:

- `deploy/docker/` — Dockerfiles for the server and worker images.
- `deploy/docker-compose.yml` — user-facing all-in-one stack (server + worker).
- `deploy/docker-compose.smoke.yml` — minimal stack used by CI smoke tests.
- `deploy/systemd/` — sample unit files for bare-metal Linux worker installs.
- `deploy/config/` — pointer to the canonical example configs under `config/`.

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
