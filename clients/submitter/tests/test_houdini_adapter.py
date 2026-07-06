# SPDX-License-Identifier: AGPL-3.0-or-later
"""Houdini adapter extraction tests against a fake hou module."""

from typing import Any, ClassVar


class FakeParm:
    def __init__(self, value: Any) -> None:
        self._v = value

    def eval(self) -> Any:
        return self._v

    def evalAsString(self) -> str:
        return str(self._v)


class FakeRop:
    def __init__(self, path: str, parms: dict[str, FakeParm]) -> None:
        self._path = path
        self._parms = parms

    def path(self) -> str:
        return self._path

    def parm(self, name: str) -> FakeParm | None:
        return self._parms.get(name)


class FakeNet:
    def __init__(self, children: list[FakeRop]) -> None:
        self._children = children

    def children(self) -> list[FakeRop]:
        return self._children


def _install_fake_hou(fake_host_module: Any) -> Any:
    mantra = FakeRop(
        "/out/mantra1",
        {
            "f1": FakeParm(10.0),
            "f2": FakeParm(20.0),
            "vm_picture": FakeParm("/r/m.$F4.exr"),
        },
    )
    usd = FakeRop("/stage/usdrender1", {"picture": FakeParm("/r/u.$F4.exr")})
    nets: dict[str, FakeNet] = {"/out": FakeNet([mantra]), "/stage": FakeNet([usd])}

    class HipFile:
        @staticmethod
        def path() -> str:
            return "/shows/b/shot.hip"

        @staticmethod
        def basename() -> str:
            return "shot.hip"

        @staticmethod
        def hasUnsavedChanges() -> bool:
            return True

        saved: ClassVar[list[bool]] = []

        @classmethod
        def save(cls) -> None:
            cls.saved.append(True)

    class Playbar:
        @staticmethod
        def frameRange() -> tuple[float, float]:
            return (1.0, 240.0)

    return fake_host_module(
        "hou",
        hipFile=HipFile,
        playbar=Playbar,
        node=lambda p: nets.get(p),
        RopNode=FakeRop,
    )


def test_scene_context(fake_host_module: Any) -> None:
    _install_fake_hou(fake_host_module)
    from sqi_submitter.hosts.houdini.adapter import HoudiniAdapter

    ctx = HoudiniAdapter().scene_context()
    assert ctx.scene_path == "/shows/b/shot.hip"
    assert ctx.frame_range == "1-240"


def test_targets_enumerate_out_and_stage_rops(fake_host_module: Any) -> None:
    _install_fake_hou(fake_host_module)
    from sqi_submitter.hosts.houdini.adapter import HoudiniAdapter

    targets = HoudiniAdapter().render_targets()
    assert [t.extra["RopPath"] for t in targets] == ["/out/mantra1", "/stage/usdrender1"]
    assert targets[0].frame_range == "10-20"
    assert targets[0].output_path == "/r/m.$F4.exr"
    assert targets[1].frame_range is None


def test_untitled_hip_cannot_save(fake_host_module: Any) -> None:
    hou = _install_fake_hou(fake_host_module)
    hou.hipFile.basename = staticmethod(lambda: "untitled.hip")
    from sqi_submitter.hosts.houdini.adapter import HoudiniAdapter

    assert HoudiniAdapter().save_scene() is False
