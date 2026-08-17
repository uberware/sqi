# sqi-submitter

DCC submitter framework for [`sqi`](https://github.com/uberware/sqi) — a distributed
task and render farm manager.

`sqi-submitter` is a Python framework for building DCC (Digital Content Creation)
integrations on top of [`sqi-sdk`](../python). It provides Qt-based UI components,
host adapters for Maya, Houdini, Nuke, and Blender, and a foundation for submitting
OpenJD job templates to an `sqi` farm directly from within your application.

## Installation

```sh
pip install sqi-submitter              # core (requires sqi-sdk)
pip install 'sqi-submitter[qt]'        # + PySide6 for standalone UI
```

For development (run from `clients/submitter/`):

```sh
pip install -e ../python -e '.[dev]'   # local sqi-sdk + submitter with test & lint tools
```

The same checks CI runs (split there across three jobs), as one local chain —
every stage must pass before a PR:

```sh
ruff format --check . && ruff check . && mypy src && mypy --python-version=3.13 tests && pytest -q
```

`mypy --python-version=3.13 tests` is not optional: `pyproject.toml` scopes
mypy to `src`, so running `mypy src` alone misses type errors in test files
that will still fail the PR.

## Authentication

When the target `sqi-server` has `auth.enabled=true` (see
[`docs/auth.md`](../../docs/auth.md)), a `SubmitterSession` needs an API key
to authenticate headlessly. It resolves one in this order: the `api_key`
argument passed to `SubmitterSession`, then the `$SQI_API_KEY` environment
variable, then the `api_key` value in `~/.sqi/submitter.json`
(`sqi_submitter.core.session.resolve_api_key`). Whatever is found is
forwarded to `SqiClient` as the Bearer token. Issue a key for yourself via
`POST /api/v1/api-keys` or the web Admin → API Keys page — see
[`docs/auth.md`](../../docs/auth.md#api-keys) for how keys work.

## Quick start

Open the standalone dialog (needs the `qt` extra, or a DCC-bundled PySide):

```sh
sqi-submit --server http://localhost:8080
```

Inside a DCC, wire the host's launch glue per
[`docs/dcc-submitters.md`](../../docs/dcc-submitters.md#installation-per-host).

## Testing

Use the `fake_host_module` fixture to inject mock DCC modules:

```python
def test_with_maya(fake_host_module):
    maya_cmds = fake_host_module("maya.cmds")
    # Your adapter code now sees maya.cmds in sys.modules
```

## Documentation

Full reference and integration guides: [`docs/dcc-submitters.md`](../../docs/dcc-submitters.md).

## License

AGPL-3.0-or-later. See the repository root for the full license text.
