# sqi-client

Pure-Python client for [`sqi`](https://github.com/uberware/sqi) — a distributed
task and render farm manager. `sqi-client` (import name `sqi_client`) wraps the
`sqi-server` REST and WebSocket API for scripted job submission, querying, and
management. Per `sqi.md` §13.2 it is the foundation for the Phase 2 DCC
submitters and for pipeline automation scripts.

> **Status:** Phase 1, under active development. This README is a stub; the full
> documentation lands with task 82 (see `phase1-sqi-client-tasks.md`).

## Requirements

- Python 3.9 or newer (VFX Reference Platform CY2022+).
- A reachable `sqi-server` instance.

## Installation

```sh
pip install sqi-client            # core (httpx only)
pip install 'sqi-client[yaml]'    # + PyYAML for YAML template helpers
pip install 'sqi-client[ws]'      # + websockets for live event streaming
```

## License

AGPL-3.0-or-later. See the repository root for the full license text.
