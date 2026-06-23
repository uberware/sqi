# `deploy/`

Deployment artifacts for `sqi`:

- `deploy/docker/` — Dockerfiles for the server and worker images.
- `deploy/docker-compose.yml` — user-facing all-in-one stack (server + worker).
- `deploy/docker-compose.smoke.yml` — minimal stack used by CI smoke tests.
- `deploy/systemd/` — sample unit files for bare-metal Linux worker installs.
- `deploy/config/` — pointer to the canonical example configs under `config/`.

Kubernetes manifests / a Helm chart for production-mode deployments are planned
for a later phase and are not part of v0.1.0.

Goreleaser configuration lives at the repo root as `.goreleaser.yaml` to match
tool convention.
