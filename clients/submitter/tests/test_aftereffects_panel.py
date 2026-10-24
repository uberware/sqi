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

from sqi_client.models import ProductParameter
from sqi_submitter import install
from sqi_submitter.core.handoff import load_context
from sqi_submitter.core.mapping import prefill

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
function yes() { return true; }
function no() { return false; }
var probeCalls = [];
function fakeRun(output) {
  return function (command) { probeCalls.push(command); return output; };
}
function probeStep(isWindows, exe, run) {
  var before = probeCalls.length;
  var result = sqiProbeVersion(isWindows, exe, run);
  return { result: result, calls: probeCalls.slice(before) };
}
var probes = [
  probeStep(false, "/old/sqi-submit", fakeRun("")),
  probeStep(false, "/old/sqi-submit", fakeRun("sqi-submit 0.3.0\\n")),
  probeStep(false, "/old/sqi-submit", fakeRun("")),
  probeStep(false, "/other/sqi-submit", fakeRun("sqi-submit 0.3.0\\n")),
  probeStep(false, "/boom/sqi-submit", function (c) { probeCalls.push(c); throw new Error("x"); }),
  probeStep(false, "/null/sqi-submit", fakeRun(null)),
  probeStep(true, 'C:\\\\a"b.exe', fakeRun("sqi-submit 0.3.0\\r\\n")),
  probeStep(true, "C:\\\\Program Files (x86)\\\\sqi-submit.exe", fakeRun("sqi-submit 0.3.0\\r\\n"))
];
var dupTargets = sqiUniqueTargetNames([
  sqiTarget(2, "Main Comp", "1-240", "D:\\\\out\\\\spot.mov", false),
  sqiTarget(5, "Main Comp", "1-240", "D:\\\\out\\\\spot_h264.mp4", false),
  sqiTarget(7, "Other", "1-240", "D:\\\\out\\\\other.mov", false)
]);
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
  wExe: sqiWindowedExe(true, "C:\\\\Program Files\\\\sqi\\\\sqi-submit.exe", yes),
  wExeUpper: sqiWindowedExe(true, "C:\\\\sqi\\\\SQI-SUBMIT.EXE", yes),
  wExeSlash: sqiWindowedExe(true, "C:/sqi/sqi-submit.exe", yes),
  wExeBare: sqiWindowedExe(true, "sqi-submit.exe", yes),
  wExeAsked: (function () {
    var asked = [];
    sqiWindowedExe(true, "C:\\\\sqi\\\\sqi-submit.exe", function (p) {
      asked.push(p);
      return true;
    });
    return asked;
  })(),
  wExeMissing: sqiWindowedExe(true, "C:\\\\sqi\\\\sqi-submit.exe", no),
  wExeCmd: sqiWindowedExe(true, "C:\\\\sqi\\\\sqi-submit.cmd", yes),
  wExeOther: sqiWindowedExe(true, "C:\\\\sqi\\\\my-sqi-submit.exe", yes),
  wExeMac: sqiWindowedExe(false, "/Users/a/bin/sqi-submit", yes),
  accV2: sqiFileAccessAllowed(reader({ "Main Pref Section v2": 1 })),
  accOneOn: sqiFileAccessAllowed(reader({ "Main Pref Section v2": 0, "Main Pref Section": 1 })),
  accV2Off: sqiFileAccessAllowed(reader({ "Main Pref Section v2": 0 })),
  accBothOff: sqiFileAccessAllowed(reader({ "Main Pref Section v2": 0, "Main Pref Section": 0 })),
  accV1: sqiFileAccessAllowed(reader({ "Main Pref Section": 1 })),
  accV1Off: sqiFileAccessAllowed(reader({ "Main Pref Section": 0 })),
  accNone: sqiFileAccessAllowed(reader({})),
  ctx: ctx,
  ctxText: JSON.stringify(ctx),
  noCommand: SQI_MSG.noCommand,
  uniqAlready: sqiUniqueNames(["Main", "Alt", "End"]),
  uniqOne: sqiUniqueNames(["Main", "Main"]),
  uniqThree: sqiUniqueNames(["Main", "Alt", "Main", "Main", "Main"]),
  uniqCollide: sqiUniqueNames(["A", "B", "A", "A (#3)"]),
  uniqIndexes: sqiUniqueNames(["Main", "Main"], [4, 9]),
  uniqEmpty: sqiUniqueNames([]),
  uniqProto: sqiUniqueNames(["__proto__", "__proto__", "hasOwnProperty"]),
  dupNames: [dupTargets[0].name, dupTargets[1].name, dupTargets[2].name],
  dupExtra: dupTargets[1].extra,
  verWin: sqiVersionCommand(true, "C:\\\\Program Files\\\\sqi\\\\sqi-submit.exe"),
  verMac: sqiVersionCommand(false, "/Users/a/bin/sqi-submit"),
  verWinQuote: sqiVersionCommand(true, 'C:\\\\a"b.exe'),
  verWinPercent: sqiVersionCommand(true, "C:\\\\%APPDATA%\\\\sqi-submit.exe"),
  verMacDollar: sqiVersionCommand(false, "/a/$HOME/sqi-submit"),
  verMacBacktick: sqiVersionCommand(false, "/a/\\x60x\\x60/sqi-submit"),
  verMacBackslash: sqiVersionCommand(false, "/a/b\\\\c/sqi-submit"),
  okCases: [
    "sqi-submit 0.3.0\\n",
    "sqi-submit 0.3.0\\r\\n",
    "  \\r\\n sqi-submit 1.0\\n",
    "",
    "usage: sqi-submit [-h] [--server SERVER] [--context FILE]\\n"
      + "sqi-submit: error: unrecognized arguments: --detached\\n",
    "sqi-submit",
    "sqi-submit\\n",
    "sqi-submit v0.3.0",
    "Python 3.13.1",
    "xsqi-submit 0.3.0",
  ].map(sqiVersionOk),
  okNull: sqiVersionOk(null),
  okUndefined: sqiVersionOk(undefined),
  probes: probes,
  tooOld: SQI_MSG.tooOld,
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
        encoding="utf-8",  # node writes UTF-8 to a pipe; the Windows locale default is not
        timeout=60,
        env={**os.environ, "SQI_JSX": str(JSX), "SQI_JSON2": str(JSON2)},
    )
    assert done.returncode == 0, done.stderr
    result: dict[str, Any] = json.loads(done.stdout)
    return result


@pytest.fixture(scope="module")
def harness() -> dict[str, Any]:
    """The harness output, computed once: it is deterministic and spawning node is slow."""
    return _run_harness()


def test_jsx_pure_functions_under_node(harness: dict[str, Any]) -> None:
    """Scene arithmetic, target contract and launch strings, run outside After Effects."""
    r = harness

    assert r["r0"] == "0-249"  # 25 fps, 10 s, display start 0
    assert r["r1001"] == "1001-1250"  # display start 1001
    assert r["mid"] == "50-74"  # a span that does not start at the comp start
    assert r["ntsc"] == "1-30"  # 29.97 fps rounds, never truncates

    assert r["seqTrue"] is True
    assert r["seqFalse"] is False
    assert r["seqNull"] is False
    assert r["count"] == 250

    # A movie is one task: it carries its whole frame count and its own range, and locks
    # both, so no edit (a wider range, a step, a gap) can split it.
    assert r["movie"]["extra"] == {
        "RenderQueueItem": "2",
        "FramesPerTask": "240",
        "Frames": "1-240",
    }
    assert r["movie"]["extra"]["Frames"] == r["movie"]["frame_range"]
    assert r["movie"]["locked"] == ["FramesPerTask", "Frames"]
    assert r["movie"]["kind"] == "render_queue_item"
    assert r["movie"]["name"] == "Main Comp"
    assert r["movie"]["frame_range"] == "1-240"
    assert r["movie"]["output_path"] == "D:\\out\\spot.mov"
    # An image sequence splits freely: no FramesPerTask, no Frames, nothing locked.
    assert r["seq"]["extra"] == {"RenderQueueItem": "1"}
    assert "Frames" not in r["seq"]["extra"]
    assert r["seq"]["locked"] == []

    assert r["union"] == "1-40"

    assert r["winCmd"] == (
        'cmd /c start "" "C:\\Program Files\\sqi\\sqi-submit.exe" '
        '--context "C:\\Temp\\sqi-handoff\\1.json" --detached'
    )
    # --detached: start gives sqi-submit a console that closes with it, so a failure must
    # also be shown in a message box, whatever stderr is.
    assert r["macCmd"] == (
        '"/Users/a/bin/sqi-submit" --context "/tmp/sqi-handoff/1.json" --detached >/dev/null 2>&1 &'
    )
    # A path that cannot be quoted safely is refused (the panel alerts), never escaped.
    assert r["winQuote"] is None
    assert r["macQuote"] is None
    assert r["macDollar"] is None
    assert r["winPercent"] is None

    # On Windows the windowed sqi-submitw.exe beside sqi-submit.exe is launched: started
    # with `start`, the console sqi-submit.exe would keep a console window open.
    assert r["wExe"] == "C:\\Program Files\\sqi\\sqi-submitw.exe"
    assert r["wExeUpper"] == "C:\\sqi\\sqi-submitw.exe"
    assert r["wExeSlash"] == "C:/sqi/sqi-submitw.exe"
    assert r["wExeBare"] == "sqi-submitw.exe"
    assert r["wExeAsked"] == ["C:\\sqi\\sqi-submitw.exe"]
    assert r["wExeMissing"] == "C:\\sqi\\sqi-submit.exe"  # an install from before it existed
    assert r["wExeCmd"] == "C:\\sqi\\sqi-submit.cmd"  # anything else is launched as it is
    assert r["wExeOther"] == "C:\\sqi\\my-sqi-submit.exe"
    assert r["wExeMac"] == "/Users/a/bin/sqi-submit"

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


def test_jsx_unique_names_under_node(harness: dict[str, Any]) -> None:
    """Two render-queue items for one comp get distinct target (and so job) names."""
    r = harness

    assert r["uniqAlready"] == ["Main", "Alt", "End"]  # unique names are left alone
    assert r["uniqEmpty"] == []
    # The first occurrence keeps its name; a later one gets its 1-based position.
    assert r["uniqOne"] == ["Main", "Main (#2)"]
    assert r["uniqThree"] == ["Main", "Alt", "Main (#3)", "Main (#4)", "Main (#5)"]
    # "A (#3)" is taken by an existing name, so the duplicate is disambiguated again.
    assert r["uniqCollide"] == ["A", "B", "A (#3) (#3)", "A (#3)"]
    assert r["uniqIndexes"] == ["Main", "Main (#9)"]  # explicit indexes win over positions
    # Names are data, never object keys that collide with Object.prototype.
    assert r["uniqProto"] == ["__proto__", "__proto__ (#2)", "hasOwnProperty"]

    # The panel suffixes with the render-queue index, which is what the artist sees in AE.
    assert r["dupNames"] == ["Main Comp", "Main Comp (#5)", "Other"]
    assert r["dupExtra"] == {"RenderQueueItem": "5", "FramesPerTask": "240", "Frames": "1-240"}


def test_jsx_version_probe_under_node(harness: dict[str, Any]) -> None:
    """The --version probe that keeps the panel from launching an sqi-submit too old for it."""
    r = harness

    # Windows quotes the whole command once more: cmd /c strips that outer pair, so the
    # path stays quoted even when it holds "(" (Program Files (x86)) or "&".
    assert r["verWin"] == 'cmd /c ""C:\\Program Files\\sqi\\sqi-submit.exe" --version"'
    assert r["verMac"] == '"/Users/a/bin/sqi-submit" --version'
    # The launch's refusal rules: a path that cannot be quoted is refused, never escaped.
    assert r["verWinQuote"] is None
    assert r["verWinPercent"] is None
    assert r["verMacDollar"] is None
    assert r["verMacBacktick"] is None
    assert r["verMacBackslash"] is None

    assert r["okCases"] == [
        True,  # sqi-submit 0.3.0\n
        True,  # CRLF from cmd.exe
        True,  # leading whitespace is trimmed
        False,  # nothing on stdout: an older sqi-submit wrote its usage error to stderr
        False,  # that usage error, had it reached stdout
        False,  # no version
        False,
        False,  # not "<name> <digit>"
        False,  # something else answered
        False,  # the name must start the output
    ]
    assert r["okNull"] is False
    assert r["okUndefined"] is False

    steps = [(p["result"], p["calls"]) for p in r["probes"]]
    mac = '"/old/sqi-submit" --version'
    assert steps[0] == (False, [mac])  # too old: refused ...
    assert steps[1] == (True, [mac])  # ... and not remembered, so a fix is picked up at once
    assert steps[2] == (True, [])  # a success is remembered: no second call for the path
    assert steps[3] == (True, ['"/other/sqi-submit" --version'])  # remembered per path
    assert steps[4] == (False, ['"/boom/sqi-submit" --version'])  # a throwing run is a failure
    assert steps[5] == (False, ['"/null/sqi-submit" --version'])
    assert steps[6] == (None, [])  # an unquotable path is never run
    assert steps[7] == (True, ['cmd /c ""C:\\Program Files (x86)\\sqi-submit.exe" --version"'])


def test_jsx_too_old_message_says_how_to_fix_it(harness: dict[str, Any]) -> None:
    message = harness["tooOld"]
    assert "too old for this panel" in message
    assert 'pip install -U "sqi-submitter[qt]"' in message
    assert "sqi-submit install-host aftereffects" in message
    assert "Locate sqi-submit…" in message
    assert "'" not in message  # cmd.exe passes single quotes through literally


def test_jsx_probes_the_version_before_writing_the_context() -> None:
    """The Submit flow checks --version before it writes the context file or launches."""
    code = _code_only(JSX.read_text(encoding="utf-8"))
    body = code[code.index("function sqiSubmit()") : code.index("function sqiLocate()")]
    probe = body.index("sqiProbeVersion(")
    assert probe < body.index("sqiWriteContext(")
    assert probe < body.index("sqiLaunchCommand(")
    # The probe reads --version output, which only the console sqi-submit has: the
    # windowed one is chosen for the launch alone.
    assert probe < body.index("sqiWindowedExe(")
    assert "sqiLaunchCommand(sqiIsWindows(), launchExe," in body
    assert body.index("SQI_MSG.tooOld") < body.index("sqiWriteContext(")
    assert "system.callSystem(command)" in body[probe:]


def test_jsx_not_found_message_names_the_qt_extra(harness: dict[str, Any]) -> None:
    """sqi-submit needs the [qt] extra; double quotes so the command also works in cmd.exe."""
    message = harness["noCommand"]
    assert 'pip install "sqi-submitter[qt]"' in message
    assert 'pipx install "sqi-submitter[qt]"' in message
    assert "'" not in message
    assert "Locate sqi-submit…" in message


def test_jsx_context_is_accepted_by_load_context(tmp_path: Path, harness: dict[str, Any]) -> None:
    """What the panel writes is what ``sqi-submit --context`` reads (the real loader)."""
    r = harness
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
    assert movie.frame_range == "1-240"
    assert dict(movie.extra) == {"RenderQueueItem": "2", "FramesPerTask": "240", "Frames": "1-240"}
    assert movie.locked == frozenset({"FramesPerTask", "Frames"})

    # The AE presets' parameter names: a movie's locked Frames pre-fills exactly its own
    # range, the same value the frame_range convention gives a sequence for its own.
    params = [
        ProductParameter(name=n, type=t)
        for n, t in (("Frames", "STRING"), ("RenderQueueItem", "INT"), ("FramesPerTask", "INT"))
    ]
    context = adapter.scene_context()
    assert prefill(params, context, movie) == {
        "Frames": "1-240",
        "RenderQueueItem": "2",
        "FramesPerTask": "240",
    }
    assert prefill(params, context, seq) == {"Frames": "1001-1250", "RenderQueueItem": "1"}


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
