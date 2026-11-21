# `deploy/systemd/`

Sample systemd units for running `sqi-server` and `sqi-worker` on Linux. The
release archives ship them under `deploy/systemd/`.

- `sqi-server.service` — the server, run as the `sqi` user with its state in
  `/var/lib/sqi-server` and its config at `/etc/sqi/sqi-server.yaml`.
- `sqi-worker.service` — single worker instance.
- `sqi-worker@.service` — template unit; one instance per metrics port
  (`systemctl enable --now sqi-worker@9091 sqi-worker@9092`).

See [`docs/worker-deployment.md`](../../docs/worker-deployment.md) for the full
setup (dedicated user, config file, hardening notes).
