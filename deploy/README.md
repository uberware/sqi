# `deploy/`

Deployment artifacts for `sqi-server`:

- `deploy/docker/` — Dockerfile(s) for the server image
- `deploy/systemd/` — sample unit files for bare-metal installs
- `deploy/kubernetes/` — manifests and/or a Helm chart for production-mode deployments (post-Phase 1)
- `deploy/config/` — example configuration files referenced by the docs (`sqi-server.example.yaml`)

Goreleaser configuration lives at the repo root as `.goreleaser.yaml` to match tool convention.
