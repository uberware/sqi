# SPDX-License-Identifier: AGPL-3.0-or-later
"""The built wheel carries every file a host install copies or loads.

The other tests read the source tree, so they cannot see a file the build leaves out:
the host files are not Python (``.pyp``, ``.jsx``, ``.js``, ``.lua``, ``.ms``, ``.xml``,
``.json``) and hatch includes them only because nothing ignores them. This builds a real
wheel into a temporary directory with the build backend (``hatchling``, in the ``dev``
extra) and reads it back.
"""

from __future__ import annotations

import subprocess
import sys
import zipfile
from pathlib import Path

import pytest

from sqi_submitter import install

PROJECT = Path(__file__).resolve().parents[1]

# Files outside install.HOST_FILES that a host or the dialog loads at run time.
REQUIRED = [
    "hosts/_handoff_block.py",
    "core/context.schema.json",
    "hosts/max/package/PackageContents.xml",
    "hosts/max/package/Contents/scripts/startup/sqi_submitter.ms",
    "hosts/maya/startup/sqi.mod",
    "hosts/maya/startup/userSetup.py",
    "hosts/houdini/startup/sqi_submitter.json",
    "hosts/nuke/startup/menu.py",
    "py.typed",
]


@pytest.fixture(scope="module")
def wheel(tmp_path_factory: pytest.TempPathFactory) -> dict[str, bytes]:
    """The wheel's files by name, built once per run outside the source tree, so the build
    writes nothing there."""
    out = tmp_path_factory.mktemp("wheel")
    subprocess.run(
        [sys.executable, "-m", "hatchling", "build", "--target", "wheel", "--directory", str(out)],
        cwd=PROJECT,
        check=True,
        capture_output=True,
        timeout=120,
    )
    (path,) = out.glob("*.whl")
    with zipfile.ZipFile(path) as archive:
        assert archive.testzip() is None
        return {name: archive.read(name) for name in archive.namelist()}


def test_wheel_contains_every_host_file(wheel: dict[str, bytes]) -> None:
    names = set(wheel)
    host_files = [f for files in install.HOST_FILES.values() for f in files]
    assert set(install.HOST_FILES) == {
        "blender",
        "cinema4d",
        "aftereffects",
        "unreal",
        "katana",
        "keyshot",
        "silhouette",
        "modo",
        "fusion",
        "vred",
    }
    # index.cfg is not Python: it ships only because nothing excludes it.
    assert "hosts/modo/sqi/index.cfg" in host_files
    assert "hosts/aftereffects/Submit to sqi.jsx" in host_files
    assert "hosts/aftereffects/json2.js" in host_files
    # A Lua file whose name has spaces: the wheel and the copy must both carry it intact.
    assert "hosts/fusion/Submit to sqi.lua" in host_files
    # VRED's plugin sits in a folder named for the plugin, as VRED loads it.
    assert "hosts/vred/SubmitToSqi/SubmitToSqi.py" in host_files
    missing = [f for f in host_files + REQUIRED if f"sqi_submitter/{f}" not in names]
    assert missing == []


def test_wheel_files_match_the_source(wheel: dict[str, bytes]) -> None:
    host_files = [f for files in install.HOST_FILES.values() for f in files]
    for relative in host_files + REQUIRED:
        source = PROJECT / "src" / "sqi_submitter" / relative
        assert wheel[f"sqi_submitter/{relative}"] == source.read_bytes(), relative


def test_wheel_has_no_tests_or_bytecode(wheel: dict[str, bytes]) -> None:
    names = set(wheel)
    assert [n for n in names if n.startswith("tests/") or "/tests/" in n] == []
    assert [n for n in names if "__pycache__" in n or n.endswith((".pyc", ".pyo"))] == []
    top = {n.split("/", 1)[0] for n in names}
    assert {t for t in top if not t.endswith(".dist-info")} == {"sqi_submitter"}


def test_wheel_declares_the_console_script(wheel: dict[str, bytes]) -> None:
    (entry_points,) = [n for n in wheel if n.endswith(".dist-info/entry_points.txt")]
    text = wheel[entry_points].decode("utf-8")
    assert "[console_scripts]" in text
    assert "sqi-submit = sqi_submitter.cli:main" in text.splitlines()


def test_wheel_declares_the_windowed_script(wheel: dict[str, bytes]) -> None:
    """sqi-submitw is what a handoff host launches on Windows: it opens no console."""
    (entry_points,) = [n for n in wheel if n.endswith(".dist-info/entry_points.txt")]
    lines = wheel[entry_points].decode("utf-8").splitlines()
    section = lines.index("[gui_scripts]")
    assert "sqi-submitw = sqi_submitter.cli:main" in lines[section + 1 :]
