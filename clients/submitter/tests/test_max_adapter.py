# SPDX-License-Identifier: AGPL-3.0-or-later
"""3ds Max adapter and menu tests against a fake pymxs / qtmax."""

from __future__ import annotations

import xml.etree.ElementTree as ET
from collections.abc import Callable
from importlib import resources
from types import ModuleType, SimpleNamespace
from typing import Any

import pytest

from sqi_submitter.core import HostAdapter

FakeInstaller = Callable[..., ModuleType]

IMPORT_LINE = "from sqi_submitter.hosts.max.menu import open_submitter; open_submitter()"


def _t(frame: int) -> SimpleNamespace:
    """A pymxs time value: ``.frame`` is the frame number."""
    return SimpleNamespace(frame=frame)


def _install_fake_pymxs(fake_host_module: FakeInstaller, **overrides: Any) -> SimpleNamespace:
    saved: list[tuple[str, bool]] = []

    def save_max_file(path: str, quiet: bool = False) -> bool:
        saved.append((path, quiet))
        return True

    rt = SimpleNamespace(
        maxFilePath="D:/shows/a/",
        maxFileName="shot_010.max",
        rendTimeType=3,
        sliderTime=_t(12),
        animationRange=SimpleNamespace(start=_t(0), end=_t(100)),
        rendStart=_t(1001),
        rendEnd=_t(1050),
        rendPickupFrames="1-5,9",
        rendSaveFile=True,
        rendOutputFilename="D:/shows/a/out/shot.exr",
        renderers=SimpleNamespace(current="scanline"),
        classOf=lambda r: f"Class_{r}",
        getSaveRequired=lambda: False,
        saveMaxFile=save_max_file,
        saved=saved,
    )
    for key, value in overrides.items():
        setattr(rt, key, value)
    fake_host_module("pymxs", runtime=rt)
    return rt


def test_max_is_a_host_adapter() -> None:
    from sqi_submitter.hosts.max.adapter import MaxAdapter

    assert issubclass(MaxAdapter, HostAdapter)
    assert MaxAdapter.host_name == "3dsmax"
    assert MaxAdapter.display_name == "3ds Max"


@pytest.mark.parametrize(
    ("rend_time_type", "pickup", "expected"),
    [
        (1, "", "12"),  # current frame (sliderTime)
        (2, "", "0-100"),  # animationRange
        (3, "", "1001-1050"),  # rendStart/rendEnd
        (4, "1-5,9", "1-5,9"),  # pickup frames, valid OpenJD range syntax
        (4, "7", "7"),
        (4, "1-5, 9", None),  # whitespace is not valid
        (4, "1-5;9", None),
        (4, "", None),
        (4, "1-5,", None),
        (5, "", None),  # unknown type
    ],
)
def test_max_frame_range_each_rend_time_type(
    fake_host_module: FakeInstaller, rend_time_type: int, pickup: str, expected: str | None
) -> None:
    _install_fake_pymxs(fake_host_module, rendTimeType=rend_time_type, rendPickupFrames=pickup)
    from sqi_submitter.hosts.max.adapter import MaxAdapter

    assert MaxAdapter().scene_context().frame_range == expected


def test_max_scene_path_and_output(fake_host_module: FakeInstaller) -> None:
    _install_fake_pymxs(fake_host_module)
    from sqi_submitter.hosts.max.adapter import MaxAdapter

    ctx = MaxAdapter().scene_context()
    assert ctx.scene_path == "D:/shows/a/shot_010.max"
    assert ctx.output_path == "D:/shows/a/out/shot.exr"
    assert ctx.renderer == "Class_scanline"


def test_max_output_is_none_when_not_saving(fake_host_module: FakeInstaller) -> None:
    _install_fake_pymxs(fake_host_module, rendSaveFile=False)
    from sqi_submitter.hosts.max.adapter import MaxAdapter

    assert MaxAdapter().scene_context().output_path is None


def test_max_untitled_scene_has_no_path_or_output(fake_host_module: FakeInstaller) -> None:
    _install_fake_pymxs(fake_host_module, maxFilePath="", maxFileName="", rendOutputFilename="")
    from sqi_submitter.hosts.max.adapter import MaxAdapter

    ctx = MaxAdapter().scene_context()
    assert ctx.scene_path is None
    assert ctx.output_path is None


def test_max_save_scene_saves_quietly(fake_host_module: FakeInstaller) -> None:
    rt = _install_fake_pymxs(fake_host_module)
    from sqi_submitter.hosts.max.adapter import MaxAdapter

    assert MaxAdapter().save_scene() is True
    assert rt.saved == [("D:/shows/a/shot_010.max", True)]


def test_max_untitled_scene_cannot_save(fake_host_module: FakeInstaller) -> None:
    rt = _install_fake_pymxs(fake_host_module, maxFilePath="", maxFileName="")
    from sqi_submitter.hosts.max.adapter import MaxAdapter

    assert MaxAdapter().save_scene() is False
    assert rt.saved == []


@pytest.mark.parametrize("modified", [True, False])
def test_max_scene_modified_follows_save_required(
    fake_host_module: FakeInstaller, modified: bool
) -> None:
    _install_fake_pymxs(fake_host_module, getSaveRequired=lambda: modified)
    from sqi_submitter.hosts.max.adapter import MaxAdapter

    assert MaxAdapter().is_scene_modified() is modified


def test_max_has_no_targets(fake_host_module: FakeInstaller) -> None:
    _install_fake_pymxs(fake_host_module)
    from sqi_submitter.hosts.max.adapter import MaxAdapter

    assert MaxAdapter().render_targets() == []


def test_max_menu_parents_to_max_main_window(
    fake_host_module: FakeInstaller, monkeypatch: pytest.MonkeyPatch
) -> None:
    main_window = object()
    fake_host_module("qtmax", GetQMaxMainWindow=lambda: main_window)
    calls: list[tuple[Any, Any]] = []

    def fake_open(adapter: Any, server_url: Any = None, parent: Any = None) -> None:
        calls.append((adapter, parent))

    import sqi_submitter.qt.dialog as dialog

    monkeypatch.setattr(dialog, "open_for_adapter", fake_open)
    from sqi_submitter.hosts.max import menu
    from sqi_submitter.hosts.max.adapter import MaxAdapter

    menu.open_submitter()
    assert len(calls) == 1
    adapter, parent = calls[0]
    assert isinstance(adapter, MaxAdapter)
    assert parent is main_window


def test_max_package_files_are_package_resources() -> None:
    root = resources.files("sqi_submitter").joinpath("hosts", "max", "package")
    assert root.joinpath("PackageContents.xml").is_file()
    assert root.joinpath("Contents", "scripts", "startup", "sqi_submitter.ms").is_file()


def test_max_package_contents_is_well_formed_xml() -> None:
    xml = resources.files("sqi_submitter").joinpath(
        "hosts", "max", "package", "PackageContents.xml"
    )
    root = ET.fromstring(xml.read_bytes())
    assert root.tag == "ApplicationPackage"
    entry = root.find("./Components/ComponentEntry")
    assert entry is not None
    assert entry.get("ModuleName", "").endswith("sqi_submitter.ms")


def test_max_startup_script_defines_macroscript_calling_python() -> None:
    ms = (
        resources.files("sqi_submitter")
        .joinpath("hosts", "max", "package", "Contents", "scripts", "startup", "sqi_submitter.ms")
        .read_text(encoding="utf-8")
    )
    assert "macroScript SubmitToSqi" in ms
    assert 'category:"sqi"' in ms
    assert f'python.Execute "{IMPORT_LINE}"' in ms
    assert "#cuiRegisterMenus" in ms
