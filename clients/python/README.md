# sqi-client

Pure-Python client for [`sqi`](https://github.com/uberware/sqi) — a distributed
task and render farm manager.

`sqi-client` (import name `sqi_client`) is a pure-Python library for programmatic
job submission, status queries, and management. It covers the same operations as
the web UI via the REST API, and is the foundation for the Phase 2 DCC
submitters and for pipeline automation scripts (see [`../../ROADMAP.md`](../../ROADMAP.md)).

It talks to a running `sqi-server` over its REST API, with an optional WebSocket
extra for live event streaming. The only required dependency is
[`httpx`](https://www.python-httpx.org/); everything else is an opt-in extra, so
the library stays light enough to embed in DCC Python environments (Maya,
Houdini, Nuke).

## Requirements

- **Python 3.9 or newer** (VFX Reference Platform CY2022+; covers Maya 2023+,
  Houdini 19.5+, Nuke 14+).
- A reachable `sqi-server` instance.

## Installation

```sh
pip install sqi-client            # core (httpx only)
pip install 'sqi-client[yaml]'    # + PyYAML (for your own YAML handling; not needed to submit)
pip install 'sqi-client[ws]'      # + websockets for live event streaming
```

Until the package is published to PyPI, install the wheel attached to a
[GitHub release](https://github.com/uberware/sqi/releases):

```sh
pip install https://github.com/uberware/sqi/releases/download/vX.Y.Z/sqi_client-X.Y.Z-py3-none-any.whl
# with an extra:
pip install "sqi_client[ws] @ https://github.com/uberware/sqi/releases/download/vX.Y.Z/sqi_client-X.Y.Z-py3-none-any.whl"
```

The package ships a `py.typed` marker, so type checkers see its annotations.

## Quickstart

```python
from pathlib import Path

from sqi_client import SqiClient

with SqiClient("http://localhost:8080") as sqi:
    # Submit an OpenJD template (a Path is read from disk; a str is sent verbatim;
    # a dict is serialized to JSON) and block until the job finishes.
    job = sqi.submit_and_wait(
        Path("render.yaml"), farm_id="<farm-id>", queue_id="<queue-id>", timeout=600
    )
    print("job", job.id, "->", job.status)

    # Print each task's captured log output.
    for task in sqi.iter_job_tasks(job.id):
        page = sqi.get_task_logs(task.id)
        print("".join(chunk.data for chunk in page.items), end="")
```

## What you can do

- **Submit** raw OpenJD job templates (`submit_job`, `submit_and_wait`).
- **Query** jobs, tasks, workers, and logs with typed models and automatic
  pagination (`list_*` returns a `Page`; `iter_*` walks every page lazily).
- **Manage** jobs (pause, resume, set priority, cancel, retry tasks) and workers
  (enable, disable).
- **CRUD** farms, queues, storage locations, and license pools.
- **Tail logs** by polling (`tail_task_logs`) or live over WebSocket with the
  `ws` extra (`tail_task_logs_live`).

Errors map to a typed hierarchy rooted at `SqiError` (e.g. `NotFoundError`,
`ValidationError`, `ConflictError`, `SqiTimeoutError`). The transport retries
idempotent GETs with backoff and exposes a per-request header hook for future
authentication.

## Documentation

Full reference — construction and configuration, every public method with
examples, error handling, pagination, log tailing, and the conveniences — is in
[`docs/python-client.md`](https://github.com/uberware/sqi/blob/main/docs/python-client.md).
Runnable examples live in [`examples/`](./examples).

## License

AGPL-3.0-or-later. See the repository root for the full license text.
