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
   Off-LAN or in doubt, point it explicitly with the server's NATS URL:
   ```sh
   SQI_WORKER_NATS_URL=nats://<server-host>:4222 ./sqi-worker start
   ```

4. Continue to **"Submit your first job"** below.

## Option B — Docker Compose

```sh
curl -LO https://raw.githubusercontent.com/uberware/sqi/v0.2.0/deploy/docker-compose.yml
docker compose -f docker-compose.yml up -d
```

This starts a server and one worker, wired together. Open
http://localhost:8080. The worker container has no DCC software — for real
render work, build a custom worker image or bind-mount your tools (see
[worker Docker guide](worker-docker.md)).

## Submit your first job

On first startup the server seeds a **default** farm and queue, so there's
nothing to set up first — just open the web UI and submit a built-in product.

1. Open http://localhost:8080 and click **Submit**.
2. Under **Built In**, pick **Run a Shell Command**.
3. In the **Command** field, enter:

   ```sh
   echo hello world
   ```

4. The **Queue** is already set to `default` and the job is given a name
   automatically — adjust either if you like — then click **Submit job**.
5. You land on the job's page. As soon as a worker picks it up you'll see it run
   with live logs, and the task output shows `hello world`.

> If no worker is connected yet, the job waits in **pending** until one starts
> (Option A, step 3). With a worker running it finishes in seconds.

Need a raw OpenJD template instead of a product? Use **Submit → Advanced:
submit a raw OpenJD template**.

### Prefer the API or Python?

The default farm and queue already exist, so you can submit straight to the
built-in `script` product — no farm/queue creation needed. See
[Submit a job from a product](api.md#submit-a-job-from-a-product) for the REST
call, or the [Python client](python-client.md) for scripted submission.

### Submitting from inside Maya/Houdini/Nuke/Blender

Rather than the web UI, artists can submit directly from the DCC they're
already working in. The [`sqi-submitter`](dcc-submitters.md) Python package
adds an in-application submit dialog (native panel for Blender) to Maya,
Houdini, Nuke, and Blender, driven by the same product catalog — including
six ready-to-install reference render presets — with scene path, frame
range, and render-target pre-fill. See [DCC submitters](dcc-submitters.md) for
installation per host.

## Next steps

- [Worker deployment](worker-deployment.md) — run workers as services.
- [Configuration](configuration.md) — every setting.
- [Python client](python-client.md) — scripted submission.
