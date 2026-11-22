# `deploy/systemd/`

Sample systemd units for running `sqi-server` and `sqi-worker` on Linux. The
release archives ship them under `deploy/systemd/`.

- `sqi-server.service` — the server, run as the `sqi` user with its state in
  `/var/lib/sqi-server` and its config at `/etc/sqi/sqi-server.yaml`.
- `sqi-worker.service` — single worker instance.
- `sqi-worker@.service` — template unit; one instance per metrics port
  (`systemctl enable --now sqi-worker@9091 sqi-worker@9092`).

Each unit expects its service user to exist and its configuration file to be
written with `config init` first:

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin sqiworker   # sqi for the server
sudo sqi-worker config init --nats-url nats://sqi-server.example.com:4222    # sqi-server config init for the server
sudo cp deploy/systemd/sqi-worker.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now sqi-worker
```

See
[`docs/worker-deployment.md`](https://github.com/uberware/sqi/blob/main/docs/worker-deployment.md)
for the full worker setup and
[`docs/operations.md`](https://github.com/uberware/sqi/blob/main/docs/operations.md)
for the server.
