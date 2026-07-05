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

For development:

```sh
pip install -e '.[dev]'                # with test & lint tools
pip install -e ../python               # resolve sqi-sdk from local checkout
```

## Quick start

Use the `fake_host_module` fixture in tests to inject mock DCC modules:

```python
def test_with_maya(fake_host_module):
    maya_cmds = fake_host_module("maya.cmds")
    # Your adapter code now sees maya.cmds in sys.modules
```

## Documentation

Full reference and integration guides: [`docs/dcc-submitters.md`](../../docs/dcc-submitters.md).

## License

AGPL-3.0-or-later. See the repository root for the full license text.
