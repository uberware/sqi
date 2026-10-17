# SPDX-License-Identifier: AGPL-3.0-or-later
"""The handoff context file: load, validate, round trip, schema."""

from __future__ import annotations

import json
import shutil
from importlib import resources
from pathlib import Path
from typing import Any

import pytest

from sqi_submitter.core import HostAdapter, RenderTarget, SceneContext
from sqi_submitter.core.handoff import (
    CONTEXT_VERSION,
    ContextAdapter,
    ContextError,
    dump_context,
    load_context,
)

FIXTURES = Path(__file__).parent / "fixtures" / "context"


class TwoTargetAdapter(HostAdapter):
    host_name = "mini"
    display_name = "MiniTool"

    def scene_context(self) -> SceneContext:
        return SceneContext(
            scene_path="/x/s.mini", frame_range="1-4", output_path="/out/", renderer="r"
        )

    def render_targets(self) -> list[RenderTarget]:
        return [
            RenderTarget(
                name="beauty",
                kind="pass",
                frame_range="1-4",
                output_path="/out/beauty.####.exr",
                extra={"Pass": "beauty", "FramesPerTask": "2"},
                locked=frozenset({"FramesPerTask"}),
            ),
            RenderTarget(name="alpha", kind="pass", extra={"Pass": "alpha"}),
        ]


def _write(tmp_path: Path, text: str) -> Path:
    path = tmp_path / "ctx.json"
    path.write_text(text, encoding="utf-8")
    return path


def _from_fixture(tmp_path: Path, name: str) -> Path:
    path = tmp_path / name
    shutil.copy(FIXTURES / name, path)
    return path


def test_context_version_is_one() -> None:
    assert CONTEXT_VERSION == 1


def test_context_round_trip(tmp_path: Path) -> None:
    source = TwoTargetAdapter()
    path = tmp_path / "ctx.json"
    dump_context(source, str(path))
    assert path.exists()

    loaded = load_context(path)

    assert isinstance(loaded, ContextAdapter)
    assert loaded.host_name == "mini"
    assert loaded.display_name == "MiniTool"
    assert loaded.server_url is None
    assert loaded.scene_context() == source.scene_context()
    assert loaded.render_targets() == source.render_targets()
    assert loaded.render_targets()[0].locked == frozenset({"FramesPerTask"})
    assert loaded.render_targets()[1].locked == frozenset()
    assert loaded.is_scene_modified() is False
    assert loaded.save_scene() is True
    assert not path.exists()


def test_context_adapter_round_trips_server_url(tmp_path: Path) -> None:
    path = _from_fixture(tmp_path, "multi-target-locked.json")
    first = load_context(path)
    assert first.server_url == "http://farm.example:8080"
    again = tmp_path / "again.json"
    dump_context(first, str(again))
    assert load_context(again).server_url == "http://farm.example:8080"


def test_context_adapter_reads_documented_example(tmp_path: Path) -> None:
    adapter = load_context(_from_fixture(tmp_path, "aftereffects-minimal.json"))
    assert adapter.host_name == "aftereffects"
    assert adapter.display_name == "After Effects"
    assert adapter.scene_context() == SceneContext(
        scene_path="C:/projects/spot.aep",
        frame_range="1-240",
        output_path="D:/renders/spot/",
        renderer=None,
    )
    (target,) = adapter.render_targets()
    assert target.extra == {"RenderQueueItem": "1"}
    assert target.kind == "render_queue_item"


def test_context_ignores_unknown_keys(tmp_path: Path) -> None:
    adapter = load_context(_from_fixture(tmp_path, "multi-target-locked.json"))
    assert [t.name for t in adapter.render_targets()] == ["Main Comp", "Alt Comp"]
    assert adapter.render_targets()[0].locked == frozenset({"FramesPerTask"})


def test_context_targets_default_to_empty(tmp_path: Path) -> None:
    adapter = load_context(_from_fixture(tmp_path, "scene-only.json"))
    assert adapter.render_targets() == []


def test_context_accepts_utf8_bom(tmp_path: Path) -> None:
    path = tmp_path / "bom.json"
    path.write_bytes(
        b"\xef\xbb\xbf" + b'{"version": 1, "host": {"name": "x"}, "scene": {"path": "/a"}}'
    )
    assert load_context(path).scene_context().scene_path == "/a"


@pytest.mark.parametrize(
    ("fixture", "fragment"),
    [
        ("invalid-version.json", "version"),
        ("invalid-no-scene-path.json", "scene.path"),
        ("invalid-extra-value.json", "extra"),
        ("invalid-locked-key.json", "locked"),
    ],
)
def test_context_adapter_rejects_invalid_fixture(
    tmp_path: Path, fixture: str, fragment: str
) -> None:
    path = _from_fixture(tmp_path, fixture)
    with pytest.raises(ContextError) as info:
        load_context(path)
    assert fixture in info.value.user_message
    assert fragment in info.value.user_message
    assert path.exists()


def test_context_adapter_rejects_unknown_version(tmp_path: Path) -> None:
    path = _write(tmp_path, '{"version": 7, "scene": {"path": "/a"}}')
    with pytest.raises(ContextError) as info:
        load_context(path)
    assert path.name in info.value.user_message
    assert "version" in info.value.user_message
    assert path.exists()


def test_context_adapter_rejects_malformed_json(tmp_path: Path) -> None:
    path = _write(tmp_path, "{not json")
    with pytest.raises(ContextError) as info:
        load_context(path)
    assert path.name in info.value.user_message
    assert "JSON" in info.value.user_message
    assert path.exists()


@pytest.mark.parametrize(
    "text",
    [
        "[" * 100000,  # RecursionError from the decoder, not a JSONDecodeError
        '{"version": ' + "1" * 5000 + "}",  # 3.11+: ValueError, over the int digit limit
    ],
    ids=["deeply-nested", "over-long-integer"],
)
def test_context_adapter_rejects_hostile_json(tmp_path: Path, text: str) -> None:
    path = _write(tmp_path, text)
    with pytest.raises(ContextError) as info:
        load_context(path)
    assert path.name in info.value.user_message
    assert path.exists()


def test_context_adapter_rejects_missing_scene_path(tmp_path: Path) -> None:
    path = _write(tmp_path, '{"version": 1, "scene": {"path": ""}}')
    with pytest.raises(ContextError) as info:
        load_context(path)
    assert path.name in info.value.user_message
    assert "scene.path" in info.value.user_message
    assert path.exists()


def test_context_adapter_rejects_locked_key_not_in_extra(tmp_path: Path) -> None:
    path = _from_fixture(tmp_path, "invalid-locked-key.json")
    with pytest.raises(ContextError) as info:
        load_context(path)
    assert path.name in info.value.user_message
    assert "'B'" in info.value.user_message
    assert path.exists()


def test_context_adapter_rejects_unreadable_file(tmp_path: Path) -> None:
    missing = tmp_path / "gone.json"
    with pytest.raises(ContextError) as info:
        load_context(missing)
    assert "gone.json" in info.value.user_message


@pytest.mark.parametrize("text", ["[]", "3", '"x"', "null"])
def test_context_adapter_rejects_non_object_document(tmp_path: Path, text: str) -> None:
    path = _write(tmp_path, text)
    with pytest.raises(ContextError):
        load_context(path)
    assert path.exists()


# -- schema -------------------------------------------------------------------


def _check(schema: dict[str, Any], value: object, where: str) -> list[str]:
    """A tiny validator for the JSON Schema subset ``context.schema.json`` uses."""
    problems: list[str] = []
    types: dict[str, type] = {
        "object": dict,
        "array": list,
        "string": str,
        "integer": int,
        "null": type(None),
    }
    declared = schema.get("type")
    if declared is not None:
        names = declared if isinstance(declared, list) else [declared]
        ok = any(
            isinstance(value, types[n]) and not (n == "integer" and isinstance(value, bool))
            for n in names
        )
        if not ok:
            return [f"{where}: expected {declared}, got {type(value).__name__}"]
    if "const" in schema and value != schema["const"]:
        problems.append(f"{where}: expected {schema['const']!r}")
    if isinstance(value, str) and len(value) < schema.get("minLength", 0):
        problems.append(f"{where}: too short")
    if isinstance(value, dict):
        for key in schema.get("required", []):
            if key not in value:
                problems.append(f"{where}: missing {key}")
        props = schema.get("properties", {})
        extra_schema = schema.get("additionalProperties", True)
        for key, item in value.items():
            if key in props:
                problems += _check(props[key], item, f"{where}.{key}")
            elif isinstance(extra_schema, dict):
                problems += _check(extra_schema, item, f"{where}.{key}")
    if isinstance(value, list) and isinstance(schema.get("items"), dict):
        for i, item in enumerate(value):
            problems += _check(schema["items"], item, f"{where}[{i}]")
    return problems


def _load_schema() -> dict[str, Any]:
    text = resources.files("sqi_submitter.core").joinpath("context.schema.json").read_text("utf-8")
    loaded = json.loads(text)
    assert isinstance(loaded, dict)
    return loaded


def test_schema_declares_draft_and_version() -> None:
    schema = _load_schema()
    assert schema["$schema"] == "https://json-schema.org/draft/2020-12/schema"
    assert schema["properties"]["version"]["const"] == 1


def test_fixtures_match_schema() -> None:
    schema = _load_schema()
    valid = sorted(p for p in FIXTURES.glob("*.json") if not p.name.startswith("invalid-"))
    assert len(valid) >= 3
    for path in valid:
        document = json.loads(path.read_text(encoding="utf-8"))
        assert _check(schema, document, path.name) == []


def test_invalid_fixtures_that_break_the_schema_are_caught() -> None:
    """The mini-checker must actually reject, or the test above proves nothing."""
    schema = _load_schema()
    for name in ("invalid-version.json", "invalid-no-scene-path.json", "invalid-extra-value.json"):
        document = json.loads((FIXTURES / name).read_text(encoding="utf-8"))
        assert _check(schema, document, name), name


def test_dumped_context_matches_schema(tmp_path: Path) -> None:
    path = tmp_path / "ctx.json"
    dump_context(TwoTargetAdapter(), str(path))
    document = json.loads(path.read_text(encoding="utf-8"))
    assert _check(_load_schema(), document, "dump") == []
