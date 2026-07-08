# SPDX-License-Identifier: AGPL-3.0-or-later
"""Nuke adapter extraction tests against a fake nuke module."""

from __future__ import annotations

from typing import Any


class K:
    def __init__(self, v: Any) -> None:
        self._v = v

    def value(self) -> Any:
        return self._v


class FakeWrite:
    def __init__(
        self,
        name: str,
        file: str,
        disabled: bool = False,
        use_limit: bool = False,
        first: int = 1,
        last: int = 1,
    ) -> None:
        self._name = name
        self._k = {
            "file": K(file),
            "disable": K(disabled),
            "use_limit": K(use_limit),
            "first": K(first),
            "last": K(last),
        }

    def name(self) -> str:
        return self._name

    def __getitem__(self, key: str) -> K:
        return self._k[key]


def _install_fake_nuke(
    fake_host_module: Any,
    script: str = "/shows/c/comp_010.nk",
    writes: list[FakeWrite] | None = None,
) -> tuple[Any, list[bool]]:
    class FakeRoot:
        def __init__(self) -> None:
            self._k = {"first_frame": K(1001.0), "last_frame": K(1050.0)}

        def name(self) -> str:
            return script

        def modified(self) -> bool:
            return True

        def __getitem__(self, key: str) -> K:
            return self._k[key]

    saved: list[bool] = []

    def script_save() -> bool:
        saved.append(True)
        return True

    return fake_host_module(
        "nuke",
        root=lambda: FakeRoot(),
        allNodes=lambda t: list(writes or []),
        scriptSave=script_save,
    ), saved


def test_scene_context_and_unsaved_script(fake_host_module: Any) -> None:
    _install_fake_nuke(fake_host_module)
    from sqi_submitter.hosts.nuke.adapter import NukeAdapter

    ctx = NukeAdapter().scene_context()
    assert ctx.scene_path == "/shows/c/comp_010.nk"
    assert ctx.frame_range == "1001-1050"


def test_root_name_means_unsaved(fake_host_module: Any) -> None:
    _install_fake_nuke(fake_host_module, script="Root")
    from sqi_submitter.hosts.nuke.adapter import NukeAdapter

    adapter = NukeAdapter()
    assert adapter.scene_context().scene_path is None
    assert adapter.save_scene() is False


def test_targets_skip_disabled_and_carry_limits(fake_host_module: Any) -> None:
    writes = [
        FakeWrite("Write1", "/r/a.####.exr"),
        FakeWrite("WriteOff", "/r/b.####.exr", disabled=True),
        FakeWrite("Write2", "/r/c.####.exr", use_limit=True, first=5, last=9),
    ]
    _install_fake_nuke(fake_host_module, writes=writes)
    from sqi_submitter.hosts.nuke.adapter import NukeAdapter

    targets = NukeAdapter().render_targets()
    assert [t.extra["WriteNode"] for t in targets] == ["Write1", "Write2"]
    assert targets[0].frame_range is None
    assert targets[1].frame_range == "5-9"
    assert targets[1].output_path == "/r/c.####.exr"
