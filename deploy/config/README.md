# `deploy/config/`

The canonical annotated example configuration files live at the repository root
under [`config/`](../../config/):

- [`config/sqi-server.example.yaml`](../../config/sqi-server.example.yaml)
- [`config/sqi-worker.example.yaml`](../../config/sqi-worker.example.yaml)

Copy the relevant file to one of the search locations
(`./config/`, `~/.sqi/`, `/etc/sqi/`) and edit. They are kept at the repo root
so the dev workflow and the deployment docs reference one source of truth.

Both files ship with TLS commented out, matching the plaintext default. See
[`docs/tls.md`](../../docs/tls.md) for what to uncomment and in what order.

The server file configures the embedded NATS broker under `nats.embedded`, and
has a commented `nats.external` block for running on a NATS cluster you run
instead (`nats.mode: external`); `sqi-server nats external init` generates the
keys that block names. See
[Running on an external NATS cluster](../../docs/operations.md#running-on-an-external-nats-cluster).
