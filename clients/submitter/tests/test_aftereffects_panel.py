# SPDX-License-Identifier: AGPL-3.0-or-later
"""The After Effects ScriptUI panel (``Submit to sqi.jsx``, ExtendScript).

ExtendScript cannot run in CI, so the only automated coverage is: a source scan that the
panel stays ES3, the scene-reading arithmetic and launch strings run under Node (skipped
without ``node``) with ``json2.js`` and stub globals, and the packaged-resource checks. The
panel's UI, pre-check dialogs, preference reads, file writes and the actual launch need a
real After Effects: the AE manual checklist covers them.
"""

from __future__ import annotations

import io
import json
import os
import re
import shutil
import subprocess
from importlib import resources
from pathlib import Path
from typing import Any

import pytest

from sqi_submitter import install
from sqi_submitter.core.handoff import load_context

HOST_DIR = Path(__file__).resolve().parents[1] / "src" / "sqi_submitter" / "hosts" / "aftereffects"
JSX = HOST_DIR / "Submit to sqi.jsx"
JSON2 = HOST_DIR / "json2.js"

_FORBIDDEN = [
    (r"\blet\b", "let"),
    (r"\bconst\b", "const"),
    (r"=>", "arrow function"),
    (r"\.forEach\(", "Array.prototype.forEach"),
    (r"`", "template literal"),
    (r"\.trim\(", "String.prototype.trim"),
    (r"\bObject\.keys\b", "Object.keys"),
]


def _code_only(text: str) -> str:
    """The source without ``/* */`` and ``//`` comments (the panel has no ``//`` in strings)."""
    text = re.sub(r"/\*.*?\*/", "", text, flags=re.DOTALL)
    return re.sub(r"//[^\n]*", "", text)


def _es3_violations(text: str) -> list[str]:
    code = _code_only(text)
    return [label for pattern, label in _FORBIDDEN if re.search(pattern, code)]


def test_es3_scanner_catches_modern_syntax() -> None:
    """The scan below must not be vacuous."""
    assert _es3_violations("let a = 1;") == ["let"]
    assert _es3_violations("var f = (x) => x;") == ["arrow function"]
    assert _es3_violations("xs.forEach(f); var s = `t`;") == [
        "Array.prototype.forEach",
        "template literal",
    ]
    assert _es3_violations("// let const => `x` .forEach(\nvar ok = 1; /* const */") == []


def test_jsx_is_es3() -> None:
    text = JSX.read_text(encoding="utf-8")
    assert _es3_violations(text) == []
    # ExtendScript reads a BOM-less file as the platform encoding: keep the source ASCII.
    assert text.isascii()
    assert text.startswith("// SPDX-License-Identifier: AGPL-3.0-or-later")
    assert '#include "json2.js"' in text


_HARNESS = r"""
const fs = require("fs");
const vm = require("vm");
const json2 = fs.readFileSync(process.env.SQI_JSON2, "utf8");
const jsx = fs
  .readFileSync(process.env.SQI_JSX, "utf8")
  .split(/\r?\n/)
  .filter((line) => !/^\s*#include\b/.test(line))
  .join("\n");
const sandbox = {
  app: {}, system: {}, Folder: {}, File: {}, $: { os: "test" },
  out: null,
};
vm.createContext(sandbox);
const driver = `
function reader(values) {
  return function (section) {
    if (!(section in values)) { throw new Error("no such section"); }
    return values[section];
  };
}
var movie = sqiTarget(2, "Main Comp", "1-240", "D:\\\\out\\\\spot.mov", false);
var seq = sqiTarget(1, "Main Comp", "1001-1250", "D:\\\\out\\\\spot_[#####].png", true);
var ctx = sqiBuildContext("25.2x12", "C:\\\\p\\\\spot.aep", [seq, movie]);
out = {
  r0: sqiFrameRange(0, 0, 10, 0.04),
  r1001: sqiFrameRange(1001, 0, 10, 0.04),
  mid: sqiFrameRange(0, 2, 1, 0.04),
  ntsc: sqiFrameRange(1, 0, 1.001, 1001 / 30000),
  seqTrue: sqiIsSequence("spot_[#####].png"),
  seqFalse: sqiIsSequence("spot.mov"),
  seqNull: sqiIsSequence(null),
  count: sqiFrameCount("1001-1250"),
  movie: movie,
  seq: seq,
  union: sqiUnionRange(["10-20", "1-5", "30-40"]),
  winCmd: sqiLaunchCommand(true, "C:\\\\Program Files\\\\sqi\\\\sqi-submit.exe",
                           "C:\\\\Temp\\\\sqi-handoff\\\\1.json"),
  macCmd: sqiLaunchCommand(false, "/Users/a/bin/sqi-submit", "/tmp/sqi-handoff/1.json"),
  winQuote: sqiLaunchCommand(true, 'C:\\\\a"b.exe', "C:\\\\Temp\\\\1.json"),
  macQuote: sqiLaunchCommand(false, "/a/sqi-submit", '/tmp/a"b.json'),
  macDollar: sqiLaunchCommand(false, "/a/$HOME/sqi-submit", "/tmp/1.json"),
  winPercent: sqiLaunchCommand(true, "C:\\\\a\\\\sqi-submit.exe", "C:\\\\%TEMP%\\\\1.json"),
  accV2: sqiFileAccessAllowed(reader({ "Main Pref Section v2": 1 })),
  accOneOn: sqiFileAccessAllowed(reader({ "Main Pref Section v2": 0, "Main Pref Section": 1 })),
  accV2Off: sqiFileAccessAllowed(reader({ "Main Pref Section v2": 0 })),
  accBothOff: sqiFileAccessAllowed(reader({ "Main Pref Section v2": 0, "Main Pref Section": 0 })),
  accV1: sqiFileAccessAllowed(reader({ "Main Pref Section": 1 })),
  accV1Off: sqiFileAccessAllowed(reader({ "Main Pref Section": 0 })),
  accNone: sqiFileAccessAllowed(reader({})),
  ctx: ctx,
  ctxText: JSON.stringify(ctx),
};
`;
vm.runInContext(json2 + "\n" + jsx + "\n" + driver, sandbox);
process.stdout.write(JSON.stringify(sandbox.out));
"""


def _run_harness() -> dict[str, Any]:
    node = shutil.which("node")
    if node is None:
        pytest.skip("node is not on PATH")
    done = subprocess.run(
        [node, "-e", _HARNESS],
        capture_output=True,
        text=True,
        timeout=60,
        env={**os.environ, "SQI_JSX": str(JSX), "SQI_JSON2": str(JSON2)},
    )
    assert done.returncode == 0, done.stderr
    result: dict[str, Any] = json.loads(done.stdout)
    return result


def test_jsx_pure_functions_under_node() -> None:
    """Scene arithmetic, target contract and launch strings, run outside After Effects."""
    r = _run_harness()

    assert r["r0"] == "0-249"  # 25 fps, 10 s, display start 0
    assert r["r1001"] == "1001-1250"  # display start 1001
    assert r["mid"] == "50-74"  # a span that does not start at the comp start
    assert r["ntsc"] == "1-30"  # 29.97 fps rounds, never truncates

    assert r["seqTrue"] is True
    assert r["seqFalse"] is False
    assert r["seqNull"] is False
    assert r["count"] == 250

    # A movie is one task: it carries the whole frame count, and the artist cannot change it.
    assert r["movie"]["extra"] == {"RenderQueueItem": "2", "FramesPerTask": "240"}
    assert r["movie"]["locked"] == ["FramesPerTask"]
    assert r["movie"]["kind"] == "render_queue_item"
    assert r["movie"]["name"] == "Main Comp"
    assert r["movie"]["frame_range"] == "1-240"
    assert r["movie"]["output_path"] == "D:\\out\\spot.mov"
    # An image sequence splits freely: no FramesPerTask, nothing locked.
    assert r["seq"]["extra"] == {"RenderQueueItem": "1"}
    assert r["seq"]["locked"] == []

    assert r["union"] == "1-40"

    assert r["winCmd"] == (
        'cmd /c start "" "C:\\Program Files\\sqi\\sqi-submit.exe" '
        '--context "C:\\Temp\\sqi-handoff\\1.json"'
    )
    assert r["macCmd"] == (
        '"/Users/a/bin/sqi-submit" --context "/tmp/sqi-handoff/1.json" >/dev/null 2>&1 &'
    )
    # A path that cannot be quoted safely is refused (the panel alerts), never escaped.
    assert r["winQuote"] is None
    assert r["macQuote"] is None
    assert r["macDollar"] is None
    assert r["winPercent"] is None

    # "Allow Scripts to Write Files and Access Network": v2 section first, then the old one.
    assert r["accV2"] is True
    assert r["accOneOn"] is True  # either section on is enough: AE writes only one of them
    assert r["accV2Off"] is False
    assert r["accBothOff"] is False
    assert r["accV1"] is True
    assert r["accV1Off"] is False
    assert r["accNone"] is None  # cannot tell: the panel proceeds and the write reports it

    context = json.loads(r["ctxText"])
    assert context["version"] == 1
    assert context["host"] == {
        "name": "aftereffects",
        "display_name": "After Effects",
        "version": "25.2x12",
    }
    assert context["scene"]["path"] == "C:\\p\\spot.aep"
    assert context["scene"]["frame_range"] == "1-1250"
    assert [t["extra"]["RenderQueueItem"] for t in context["targets"]] == ["1", "2"]


def test_jsx_context_is_accepted_by_load_context(tmp_path: Path) -> None:
    """What the panel writes is what ``sqi-submit --context`` reads (the real loader)."""
    r = _run_harness()
    file = tmp_path / "ae.json"
    file.write_text(r["ctxText"], encoding="utf-8")

    adapter = load_context(file)

    assert adapter.host_name == "aftereffects"
    assert adapter.display_name == "After Effects"
    assert adapter.scene_context().scene_path == "C:\\p\\spot.aep"
    seq, movie = adapter.render_targets()
    assert seq.name == "Main Comp"
    assert seq.frame_range == "1001-1250"
    assert dict(seq.extra) == {"RenderQueueItem": "1"}
    assert not seq.locked
    assert dict(movie.extra) == {"RenderQueueItem": "2", "FramesPerTask": "240"}
    assert set(movie.locked) == {"FramesPerTask"}


def test_jsx_and_json2_are_package_resources() -> None:
    root = resources.files("sqi_submitter").joinpath("hosts", "aftereffects")
    assert root.joinpath("Submit to sqi.jsx").is_file()
    assert root.joinpath("json2.js").is_file()
    assert root.joinpath("__init__.py").is_file()
    assert install.HOST_FILES["aftereffects"] == [
        "hosts/aftereffects/Submit to sqi.jsx",
        "hosts/aftereffects/json2.js",
    ]


def test_json2_is_vendored_with_provenance() -> None:
    text = JSON2.read_text(encoding="utf-8")
    assert "douglascrockford/JSON-js" in text.splitlines()[1]
    assert "7e83f38a2312429fd4933169c1f6a27fd65e889c" in text.splitlines()[2]
    assert "Public Domain." in text


def test_install_host_copies_both_files(tmp_path: Path) -> None:
    out = io.StringIO()
    assert install.install_host("aftereffects", tmp_path / "ScriptUI Panels", out) == 0
    for name in ("Submit to sqi.jsx", "json2.js"):
        assert (tmp_path / "ScriptUI Panels" / name).read_bytes() == (HOST_DIR / name).read_bytes()
