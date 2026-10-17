# sqi-submitter

DCC submitter framework for [`sqi`](https://github.com/uberware/sqi) — a distributed
task and render farm manager.

`sqi-submitter` is a Python framework for building DCC (Digital Content Creation)
integrations on top of [`sqi-sdk`](../python). It provides one Qt submit dialog,
host adapters for Maya, Houdini, Nuke, and 3ds Max, and a handoff for Blender,
Cinema 4D, and After Effects, for submitting OpenJD job templates to an `sqi`
farm directly from within your application.

Every host opens the same dialog. Maya, Houdini, Nuke, and 3ds Max open it in
their own process. Blender, Cinema 4D, and After Effects write a small context
file and start `sqi-submit --context FILE`, which opens it; their host files
are installed with `sqi-submit install-host`.

## Installation

```sh
pip install sqi-submitter              # core (requires sqi-sdk)
pip install "sqi-submitter[qt]"        # + PySide6 for standalone UI
```

For development (run from `clients/submitter/`):

```sh
pip install -e ../python -e '.[dev]'   # local sqi-sdk + submitter with test & lint tools
```

The same checks CI runs (split there across three jobs), as one local chain —
every stage must pass before a PR:

```sh
ruff format --check . && ruff check . && mypy && mypy --python-version=3.13 tests && pytest -q
```

Run `mypy` bare, not `mypy src`: `pyproject.toml` lists `src` and the Cinema 4D
plugin (`sqi_submit.pyp`, which `mypy src` skips because it is not a `.py`
file). `mypy --python-version=3.13 tests` is not optional either: a bare `mypy`
does not read the tests, so it alone misses type errors in test files that will
still fail the PR.

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
For Blender, Cinema 4D, and After Effects, install the host file with:

```sh
sqi-submit install-host blender        # or cinema4d, aftereffects
```

The handoff context file that those hosts write is documented in
[The handoff contract](../../docs/dcc-submitters.md#the-handoff-contract); its
JSON Schema is `src/sqi_submitter/core/context.schema.json`.

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
