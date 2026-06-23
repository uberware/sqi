# `deploy/systemd/`

Sample systemd units for running `sqi-worker` on Linux.

- `sqi-worker.service` — single worker instance.
- `sqi-worker@.service` — template unit; one instance per metrics port
  (`systemctl enable --now sqi-worker@9091 sqi-worker@9092`).

See [`docs/worker-deployment.md`](../../docs/worker-deployment.md) for the full
setup (dedicated user, config file, hardening notes).

> The server (`sqi-server`) is typically run via the all-in-one binary path or
> Docker Compose; a server unit is not yet shipped.
