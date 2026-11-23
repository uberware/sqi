# Quickstart

Get a working sqi farm running and your first job submitted in a few minutes.
Two paths: native binaries (recommended for a real farm) or Docker Compose
(fastest to try). Both end at the same place: the web UI at
http://localhost:8080.

## Option A: binaries (all-in-one, simple mode)

1. **Download and extract** the release archive for your platform from
   https://github.com/uberware/sqi/releases/latest (see
   [worker deployment](worker-deployment.md#pre-built-binaries) for the
   commands). Each archive contains both `sqi-server` and `sqi-worker`.

2. **Start the server** (scheduler, REST API, web UI, embedded NATS, SQLite):
   ```sh
   ./sqi-server serve
   ```
   With no configuration it serves the UI on http://localhost:8080 and NATS on :4222.

3. **Start a worker** on the same or another machine. On a LAN it finds the
   server by mDNS:
   ```sh
   ./sqi-worker start
   ```
   Off the LAN, or if discovery fails, give it the server's NATS URL:
   ```sh
   SQI_WORKER_NATS_URL=nats://<server-host>:4222 ./sqi-worker start
   ```

4. Continue to [Submit your first job](#submit-your-first-job) below.

These commands run in the foreground and stop when you close the terminal. For a
real install, where the server and each worker start at boot, the archive ships
the service files and the binaries write their own starter configuration
(`sqi-server config init`, `sqi-worker config init`). The
[server operations guide](operations.md#running-as-a-system-service) covers the
server as a systemd unit (Linux) or a Windows service, and the
[worker deployment guide](worker-deployment.md) covers the worker on Linux,
macOS (launchd) and Windows.

## Option B: Docker Compose

```sh
curl -LO https://raw.githubusercontent.com/uberware/sqi/v0.4.0/deploy/docker-compose.yml
docker compose -f docker-compose.yml up -d
```

This starts a server and one worker, wired together. Open
http://localhost:8080. The worker container has no DCC software; for
render work, build a custom worker image or bind-mount your tools (see the
[worker Docker guide](worker-docker.md)).

## Submit your first job

On first startup the server creates a `default` farm and queue, so you can
open the web UI and submit a built-in product straight away.

1. Open http://localhost:8080 and click **Submit**.
2. Under **Built In**, pick **Run a Shell Command**.
3. In the **Command** field, enter:

   ```sh
   echo hello world
   ```

4. The **Queue** is set to `default` and the job has a generated name; change
   either if you like, then click **Submit job**.
5. The job's page opens. When a worker picks the job up you see it run
   with live logs, and the task output shows `hello world`.

> If no worker is connected, the job waits in `pending` until one starts
> (Option A, step 3).

Need a raw OpenJD template instead of a product? Use **Submit → Advanced:
submit a raw OpenJD template**.

### Prefer the API or Python?

The default farm and queue exist, so you can submit to the
built-in `script` product without creating either. See
[Submit a job from a product](api.md#submit-a-job-from-a-product) for the REST
call, or the [Python client](python-client.md) for scripted submission.

### Submitting from inside your DCC

Artists can also submit from the DCC they are working in. The
[`sqi-submitter`](dcc-submitters.md) Python package adds a submit dialog to
Maya, Houdini, Nuke, 3ds Max, VRED, Blender, Cinema 4D, After Effects, Unreal
Engine, Katana, KeyShot, Silhouette, Fusion and Modo. Maya, Houdini, Nuke,
3ds Max, VRED and Katana 8 or later open it inside the application; the others
hand the open scene to a separate `sqi-submit` window. The dialog uses the same
product catalog, including the reference render presets, and pre-fills the
scene path, frame range and render targets. See
[DCC submitters](dcc-submitters.md) for installation per host.

## Next steps

- [Worker deployment](worker-deployment.md): run workers as services.
- [Configuration](configuration.md): every setting.
- [Python client](python-client.md): scripted submission.
