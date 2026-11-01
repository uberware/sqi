# SPDX-License-Identifier: AGPL-3.0-or-later
"""The Fusion handoff script (``Submit to sqi.lua``, Lua 5.1).

Fusion bundles Lua but not Python, so the submitter is one self-contained Lua file. Its pure
functions (JSON writing, frame ranges, settings parsing, launch strings) and its entry point
run here under a stock Lua interpreter (``lua5.1``, ``luajit`` or ``lua`` on ``PATH``) with a
fake ``comp`` and ``os.execute``. The Lua-run tests skip without an interpreter;
``SQI_REQUIRE_LUA=1``, which CI sets, turns that skip into a failure. Fusion's own attribute
values, the AskUser dialog and the real launch are on the Fusion checklist in
``docs/dcc-submitters.md``.
"""

from __future__ import annotations

import io
import json
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path

import pytest

from sqi_submitter import install
from sqi_submitter.core.handoff import load_context

HOST_DIR = Path(__file__).resolve().parents[1] / "src" / "sqi_submitter" / "hosts" / "fusion"
SCRIPT = HOST_DIR / "Submit to sqi.lua"

UNSAVED = "Save your scene before submitting \u2014 the farm renders the file on disk."
SAVE_FAILED = "Fusion could not save the comp, so nothing was submitted."


def _find_lua() -> str | None:
    for name in ("lua5.1", "luajit", "lua"):
        found = shutil.which(name)
        if found:
            return found
    return None


@pytest.fixture
def lua() -> str:
    found = _find_lua()
    if found is None:
        if os.environ.get("SQI_REQUIRE_LUA") == "1":
            pytest.fail("SQI_REQUIRE_LUA=1 but none of lua5.1, luajit or lua is on PATH")
        pytest.skip("no Lua interpreter (lua5.1, luajit or lua) on PATH")
    return found


def lit(text: str) -> str:
    """A Lua string literal for ``text``, every UTF-8 byte as a decimal escape.

    The harness goes on the command line, where a non-ASCII argument is mangled by the Windows
    code page; escapes keep it ASCII and give Lua the exact bytes.
    """
    return '"' + "".join(f"\\{b:03d}" for b in text.encode("utf-8")) + '"'


_PRELUDE = r"""
function out(k, v) io.write(k, "\t", tostring(v), "\n") end
dofile(os.getenv("SQI_LUA_SCRIPT"))
"""


def run_lua(lua_path: str, body: str, extra_env: dict[str, str] | None = None) -> dict[str, str]:
    """Run ``body`` after loading the script; return the ``key<TAB>value`` lines it printed."""
    env = dict(os.environ, SQI_LUA_SCRIPT=str(SCRIPT), **(extra_env or {}))
    done = subprocess.run(
        [lua_path, "-e", _PRELUDE + body],
        env=env,
        capture_output=True,
        timeout=60,
        check=False,
    )
    stderr = done.stderr.decode("utf-8", "replace")
    assert done.returncode == 0, stderr
    result: dict[str, str] = {}
    for line in done.stdout.decode("utf-8").splitlines():
        key, _, value = line.partition("\t")
        result[key] = value
    return result


# -- source scans (need no Lua) ------------------------------------------------------------


def test_fusion_script_has_spdx_and_is_ascii() -> None:
    text = SCRIPT.read_text(encoding="utf-8")
    assert text.startswith("-- SPDX-License-Identifier: AGPL-3.0-or-later\n")
    # Fusion reads the file as bytes: keep it ASCII so no editor or code page can change it.
    assert text.isascii()


def test_fusion_script_loads_under_lua51(lua: str) -> None:
    """Fusion runs Lua 5.1 (or LuaJIT, which reports 5.1), so the script must load under it.

    A newer Lua also accepts ``goto``, ``//`` and the ``utf8`` library, so loading the script
    there proves nothing about Fusion. Under another version this skips, or fails with
    ``SQI_REQUIRE_LUA=1``.
    """
    version = run_lua(lua, 'out("version", _VERSION)')["version"]
    if version != "Lua 5.1":
        if os.environ.get("SQI_REQUIRE_LUA") == "1":
            pytest.fail(f"SQI_REQUIRE_LUA=1 needs Lua 5.1 or LuaJIT, but {lua} is {version}")
        pytest.skip(f"{lua} is {version}, not Lua 5.1 or LuaJIT")


def test_fusion_host_package_docstring_names_fusion_and_lua() -> None:
    init = (HOST_DIR / "__init__.py").read_text(encoding="utf-8")
    assert init.startswith("# SPDX-License-Identifier: AGPL-3.0-or-later")
    assert "Fusion" in init
    assert "Lua" in init


# -- pure functions, run under Lua -----------------------------------------------------------


def test_fusion_json_string_escapes(lua: str) -> None:
    cases = {
        "windows": 'C:\\shots\\a "b".comp',
        "tab": "a\tb",
        "ctl": "a\x01b",
        "ctl_high": "\x1f",
        "newline": "a\nb\rc",
        "utf8": "caf\u00e9 \u65e5\u672c",
        "del": "a\x7fb",
        "empty": "",
    }
    body = "".join(f"out({k!r}, sqi_json_string({lit(v)}))\n" for k, v in cases.items())
    got = run_lua(lua, body.replace("'", '"'))
    assert got["windows"] == '"C:\\\\shots\\\\a \\"b\\".comp"'
    assert got["tab"] == '"a\\tb"'
    assert got["ctl"] == '"a\\u0001b"'
    assert got["ctl_high"] == '"\\u001f"'
    assert got["newline"] == '"a\\nb\\rc"'
    assert got["utf8"] == '"caf\u00e9 \u65e5\u672c"'  # bytes >= 0x80 pass through unchanged
    assert got["del"] == '"a\x7fb"'
    assert got["empty"] == '""'
    # Each is JSON that reads back to the input.
    for key, value in cases.items():
        assert json.loads(got[key]) == value, key


def test_fusion_context_json_round_trips(lua: str, tmp_path: Path) -> None:
    scene = 'C:\\shows\\\u00e9 "x"\\shot.comp'
    target = tmp_path / "context.json"
    body = f"""
local f = assert(io.open(os.getenv("SQI_OUT"), "wb"))
f:write(sqi_context_json({lit(scene)}, "1-100", "19.1"))
f:close()
"""
    run_lua(lua, body, {"SQI_OUT": str(target)})
    document = json.loads(target.read_text(encoding="utf-8"))
    assert document == {
        "version": 1,
        "host": {"name": "fusion", "display_name": "Fusion", "version": "19.1"},
        "scene": {
            "path": scene,
            "frame_range": "1-100",
            "output_path": None,
            "renderer": None,
        },
        "targets": [],
        "server_url": None,
    }
    adapter = load_context(target)
    assert adapter.scene_context().scene_path == scene
    assert adapter.host_name == "fusion"
    assert adapter.render_targets() == []
    assert not target.exists()  # a successful read deletes the file


def test_fusion_context_json_allows_unknown_range_and_version(lua: str, tmp_path: Path) -> None:
    target = tmp_path / "context.json"
    body = f"""
local f = assert(io.open(os.getenv("SQI_OUT"), "wb"))
f:write(sqi_context_json({lit("/p/shot.comp")}, nil, nil))
f:close()
"""
    run_lua(lua, body, {"SQI_OUT": str(target)})
    document = json.loads(target.read_text(encoding="utf-8"))
    assert document["scene"]["frame_range"] is None
    assert document["host"]["version"] is None
    assert load_context(target).scene_context().frame_range is None


@pytest.mark.parametrize(
    ("start", "end", "expected"),
    [
        ("1", "1", "1"),
        ("1001", "1100", "1001-1100"),
        ("10.0", "5.0", "5-10"),
        ("1.7", "3.2", "1-3"),
        ("-5", "5", "-5-5"),
        ("2.9", "2.1", "2"),
    ],
)
def test_fusion_frame_range(lua: str, start: str, end: str, expected: str) -> None:
    got = run_lua(lua, f"out('r', sqi_frame_range({start}, {end}))".replace("'", '"'))
    assert got["r"] == expected


def test_fusion_parse_settings(lua: str) -> None:
    windows = json.dumps({"sqi_submit_command": "C:\\Program Files\\sqi\\sqi-submit.exe"})
    escaped = '{"sqi_submit_command": "C:\\/tools\\/say \\"hi\\"\\\\sqi-submit"}'
    cases = {
        "windows": windows,
        "escapes": escaped,
        "absent": json.dumps({"other": "x"}),
        "number": json.dumps({"sqi_submit_command": 5}),
        "null": json.dumps({"sqi_submit_command": None}),
        "empty": "",
        "bom": "\ufeff" + json.dumps({"sqi_submit_command": "/usr/bin/sqi-submit"}),
        "not_a_key": json.dumps({"note": "sqi_submit_command", "x": "/no"}),
        "nested": json.dumps({"deep": {"sqi_submit_command": "/nested"}}),
        "unicode": json.dumps({"sqi_submit_command": "/h/\u00e9/sqi-submit"}),
        "u_escape": '{"sqi_submit_command": "/h/\\u00e9/\\ud83d\\ude00/sqi-submit"}',
        "after_other": json.dumps({"a": "b\\c", "sqi_submit_command": "/ok"}),
    }
    body = "".join(
        f"out({k!r}, tostring(sqi_parse_settings({lit(v)})))\n" for k, v in cases.items()
    )
    got = run_lua(lua, body.replace("'", '"'))
    assert got["windows"] == "C:\\Program Files\\sqi\\sqi-submit.exe"
    assert got["escapes"] == 'C:/tools/say "hi"\\sqi-submit'
    assert got["absent"] == "nil"
    assert got["number"] == "nil"
    assert got["null"] == "nil"
    assert got["empty"] == "nil"
    assert got["bom"] == "/usr/bin/sqi-submit"
    assert got["not_a_key"] == "nil"
    assert got["nested"] == "nil"  # Python's .get() reads the top level only
    assert got["unicode"] == "/h/\u00e9/sqi-submit"
    assert got["u_escape"] == "/h/\u00e9/\U0001f600/sqi-submit"
    assert got["after_other"] == "/ok"


def test_fusion_windowed_command(lua: str) -> None:
    body = r"""
local function exists_yes() return true end
local function exists_no() return false end
local asked = {}
local function recording(p) asked[#asked + 1] = p; return true end
out("swap", sqi_windowed_command("C:\\Py\\Scripts\\sqi-submit.exe", exists_yes))
out("upper", sqi_windowed_command("C:\\Py\\Scripts\\SQI-Submit.EXE", exists_yes))
out("slash", sqi_windowed_command("C:/Py/Scripts/sqi-submit.exe", exists_yes))
out("bare", sqi_windowed_command("sqi-submit.exe", exists_yes))
out("missing", sqi_windowed_command("C:\\Py\\Scripts\\sqi-submit.exe", exists_no))
out("cmd", sqi_windowed_command("C:\\Py\\Scripts\\sqi-submit.cmd", exists_yes))
out("other", sqi_windowed_command("C:\\Py\\Scripts\\my-sqi-submit.exe", exists_yes))
out("unix", sqi_windowed_command("/usr/bin/sqi-submit", exists_yes))
sqi_windowed_command("C:\\Py\\Scripts\\sqi-submit.exe", recording)
out("asked", asked[1])
"""
    got = run_lua(lua, body)
    assert got["swap"] == "C:\\Py\\Scripts\\sqi-submitw.exe"
    assert got["upper"] == "C:\\Py\\Scripts\\sqi-submitw.exe"
    assert got["slash"] == "C:/Py/Scripts/sqi-submitw.exe"
    assert got["bare"] == "sqi-submitw.exe"
    assert got["missing"] == "C:\\Py\\Scripts\\sqi-submit.exe"
    assert got["cmd"] == "C:\\Py\\Scripts\\sqi-submit.cmd"
    assert got["other"] == "C:\\Py\\Scripts\\my-sqi-submit.exe"
    assert got["unix"] == "/usr/bin/sqi-submit"
    assert got["asked"] == "C:\\Py\\Scripts\\sqi-submitw.exe"


def test_fusion_find_on_path(lua: str) -> None:
    body = r"""
local asked = {}
local function only(want)
  return function(p) asked[#asked + 1] = p; return p == want end
end
out("posix", sqi_find_on_path(false, "/a::/b/", nil, only("/b/sqi-submit")))
out("posix_asked", table.concat(asked, "|"))
asked = {}
out("win", sqi_find_on_path(true, 'C:\\a;;"C:\\Program Files\\b"', ".COM;.EXE",
  only("C:\\Program Files\\b\\sqi-submit.EXE")))
out("win_asked", table.concat(asked, "|"))
out("win_default", sqi_find_on_path(true, "C:\\a", nil, only("C:\\a\\sqi-submit.EXE")))
out("win_slash", sqi_find_on_path(true, "C:/a", "", only("C:/a/sqi-submit.EXE")))
out("none", tostring(sqi_find_on_path(false, "/a:/b", nil, only("/c/sqi-submit"))))
out("no_path", tostring(sqi_find_on_path(false, nil, nil, only("/a/sqi-submit"))))
"""
    got = run_lua(lua, body)
    assert got["posix"] == "/b/sqi-submit"
    # An empty entry would be the current folder: it is skipped, never searched.
    assert got["posix_asked"] == "/a/sqi-submit|/b/sqi-submit"
    assert got["win"] == "C:\\Program Files\\b\\sqi-submit.EXE"
    assert got["win_asked"] == "|".join(
        [
            "C:\\a\\sqi-submit.COM",
            "C:\\a\\sqi-submit.EXE",
            "C:\\Program Files\\b\\sqi-submit.COM",
            "C:\\Program Files\\b\\sqi-submit.EXE",
        ]
    )
    assert got["win_default"] == "C:\\a\\sqi-submit.EXE"  # PATHEXT unset: the Windows default
    assert got["win_slash"] == "C:/a/sqi-submit.EXE"
    assert got["none"] == "nil"
    assert got["no_path"] == "nil"


def test_fusion_launch_command(lua: str) -> None:
    win_exe = lit("C:\\Program Files\\sqi\\sqi-submitw.exe")
    win_ctx = lit("C:\\Users\\a\\AppData\\Local\\Temp\\sqi-handoff-1-2.json")
    body = f"""
out("win", sqi_launch_command(true, {win_exe}, {win_ctx}))
out("posix", sqi_launch_command(false, "/usr/bin/sqi-submit", "/tmp/lua_AbC123"))
out("quote_posix", sqi_launch_command(false, "/h/o'brien/sqi-submit", "/tmp/it's.json"))
local c1, e1 = sqi_launch_command(true, 'C:\\\\a"b\\\\sqi-submit.exe', "C:\\\\t.json")
out("dq_exe_win", tostring(c1)); out("dq_exe_win_err", tostring(e1))
local c2, e2 = sqi_launch_command(false, "/usr/bin/sqi-submit", '/tmp/a"b.json')
out("dq_ctx_posix", tostring(c2)); out("dq_ctx_posix_err", tostring(e2))
local c3, e3 = sqi_launch_command(true, "C:\\\\a\\\\sqi-submit.exe", "C:\\\\%TEMP%\\\\x.json")
out("pct_win", tostring(c3)); out("pct_win_err", tostring(e3))
out("pct_posix", sqi_launch_command(false, "/a/100%/sqi-submit", "/tmp/x"))
"""
    got = run_lua(lua, body)
    assert got["win"] == (
        'start "" "C:\\Program Files\\sqi\\sqi-submitw.exe" --context '
        '"C:\\Users\\a\\AppData\\Local\\Temp\\sqi-handoff-1-2.json"'
    )
    assert got["posix"] == "'/usr/bin/sqi-submit' --context '/tmp/lua_AbC123' >/dev/null 2>&1 &"
    assert got["quote_posix"] == (
        "'/h/o'\\''brien/sqi-submit' --context '/tmp/it'\\''s.json' >/dev/null 2>&1 &"
    )
    for key in ("dq_exe_win", "dq_ctx_posix"):
        assert got[key] == "nil"
        assert '"' in got[key + "_err"]
        assert "quote" in got[key + "_err"]
    # cmd.exe expands %NAME% even inside quotes, so a Windows path holding % is refused too.
    assert got["pct_win"] == "nil"
    assert "%" in got["pct_win_err"]
    assert got["pct_posix"] == "'/a/100%/sqi-submit' --context '/tmp/x' >/dev/null 2>&1 &"


# -- the entry point, against a fake comp ---------------------------------------------------


def _entry(
    tmp_path: Path,
    *,
    filename: str | None = None,
    modified: str = "true",
    start: str = "1001",
    end: str = "1100",
    save_result: str = "true",
    windows: bool = False,
    path: str = "",
    ask_user: bool = True,
    execute_result: str = "0",
    version: str = "19.1",
) -> str:
    """Lua that runs ``sqi_submit()`` with a fake ``comp`` and records what it did."""
    scene = filename if filename is not None else str(tmp_path / "shot.comp")
    attrs = f"COMPS_FileName = {lit(scene)}, COMPB_Modified = {modified}"
    if start != "nil":
        attrs += f", COMPN_RenderStart = {start}"
    if end != "nil":
        attrs += f", COMPN_RenderEnd = {end}"
    return f"""
local events = {{}}
local function ev(s) events[#events + 1] = s end
local real_getenv = os.getenv
local env = {{
  HOME = {lit(str(tmp_path))}, USERPROFILE = {lit(str(tmp_path))}, TEMP = {lit(str(tmp_path))},
  SQI_SUBMITTER_SETTINGS = os.getenv("SQI_SETTINGS"), PATH = {lit(path)},
}}
os.getenv = function(k) return env[k] end
local tmpcount = 0
os.tmpname = function()
  tmpcount = tmpcount + 1
  local p = {lit(str(tmp_path / "lua_tmp"))} .. tmpcount
  io.open(p, "wb"):close()
  return p
end
io.popen = function(cmd) ev("popen:" .. cmd) end
local real_execute = os.execute
os.execute = function(cmd)
  -- The PATH search's executable probe runs for real, against the files the test made.
  if cmd:sub(1, 5) == "test " then return real_execute(cmd) end
  ev("exec:" .. cmd); return {execute_result}
end
local printed = {{}}
print = function(...) printed[#printed + 1] = table.concat({{...}}, " ") end
fusion = {{ Version = {version} }}
comp = {{
  GetAttrs = function(self) return {{ {attrs} }} end,
  Save = function(self, p) ev("save:" .. tostring(p)); return {save_result} end,
}}
{
        '''comp.AskUser = function(self, title, controls)
  ev("ask:" .. title .. "|" .. controls[1].Default)
end'''
        if ask_user
        else ""
    }
sqi_is_windows = function() return {"true" if windows else "false"} end
local ok = sqi_submit()
out("ok", tostring(ok))
out("events", table.concat(events, "\\n"):gsub("\\n", "<>"))
out("printed", table.concat(printed, "<>"))
"""


def _events(got: dict[str, str]) -> list[str]:
    return got["events"].split("<>") if got["events"] else []


def _on_path(
    tmp_path: Path, name: str = "sqi-submit", folder: str = "bin", mode: int = 0o755
) -> Path:
    """Create an empty ``folder/name`` under ``tmp_path`` (executable by default), return it."""
    exe = tmp_path / folder / name
    exe.parent.mkdir()
    exe.write_bytes(b"")
    exe.chmod(mode)
    return exe


# A POSIX PATH is split on ':', which also splits a Windows drive-letter folder.
_POSIX_PATH = pytest.mark.skipif(sys.platform == "win32", reason="POSIX PATH on a Windows host")


def test_fusion_unsaved_comp_is_refused(lua: str, tmp_path: Path) -> None:
    got = run_lua(lua, _entry(tmp_path, filename=""), {"SQI_SETTINGS": ""})
    assert got["ok"] == "false"
    assert _events(got) == [f"ask:Submit to sqi|{UNSAVED}"]
    # The Console always gets the message: AskUser may accept the control table and show nothing.
    assert got["printed"] == f"Submit to sqi: {UNSAVED}"


def test_fusion_messages_reach_the_console_without_askuser(lua: str, tmp_path: Path) -> None:
    got = run_lua(lua, _entry(tmp_path, filename="", ask_user=False), {"SQI_SETTINGS": ""})
    assert got["ok"] == "false"
    assert _events(got) == []
    assert got["printed"].endswith(UNSAVED)


def test_fusion_modified_comp_is_saved_then_launched(lua: str, tmp_path: Path) -> None:
    exe = tmp_path / "sqi-submit"
    exe.write_bytes(b"")
    settings = tmp_path / "settings.json"
    settings.write_text(json.dumps({"sqi_submit_command": str(exe)}), encoding="utf-8")
    scene = str(tmp_path / "shot.comp")
    # The settings file wins over an sqi-submit on PATH.
    on_path = _on_path(tmp_path)
    got = run_lua(lua, _entry(tmp_path, path=str(on_path.parent)), {"SQI_SETTINGS": str(settings)})
    assert got["ok"] == "true"
    events = _events(got)
    assert events[0] == f"save:{scene}"
    assert len(events) == 2
    assert events[1].startswith("exec:")
    context = tmp_path / "lua_tmp1"
    assert f"--context '{context}'" in events[1]
    assert events[1].startswith(f"exec:'{exe}' --context ")
    adapter = load_context(context)
    assert adapter.scene_context().scene_path == scene
    assert adapter.scene_context().frame_range == "1001-1100"
    assert adapter.host_version == "19.1"


def test_fusion_unmodified_comp_is_not_saved(lua: str, tmp_path: Path) -> None:
    settings = tmp_path / "settings.json"
    exe = tmp_path / "sqi-submit"
    exe.write_bytes(b"")
    settings.write_text(json.dumps({"sqi_submit_command": str(exe)}), encoding="utf-8")
    got = run_lua(lua, _entry(tmp_path, modified="false"), {"SQI_SETTINGS": str(settings)})
    assert got["ok"] == "true"
    assert [e for e in _events(got) if e.startswith("save:")] == []


def test_fusion_unknown_modified_state_saves_to_be_safe(lua: str, tmp_path: Path) -> None:
    got = run_lua(lua, _entry(tmp_path, modified="nil"), {"SQI_SETTINGS": ""})
    assert _events(got)[0].startswith("save:")


@pytest.mark.parametrize("result", ["false", "error('disk full')"])
def test_fusion_failed_save_submits_nothing(lua: str, tmp_path: Path, result: str) -> None:
    got = run_lua(lua, _entry(tmp_path, save_result=result), {"SQI_SETTINGS": ""})
    assert got["ok"] == "false"
    events = _events(got)
    assert events[-1] == f"ask:Submit to sqi|{SAVE_FAILED}"
    assert [e for e in events if e.startswith(("exec:", "popen:"))] == []


def test_fusion_frames_come_from_the_render_range(lua: str, tmp_path: Path) -> None:
    exe = tmp_path / "sqi-submit"
    exe.write_bytes(b"")
    settings = tmp_path / "settings.json"
    settings.write_text(json.dumps({"sqi_submit_command": str(exe)}), encoding="utf-8")
    run_lua(lua, _entry(tmp_path, start="5.0", end="2.0"), {"SQI_SETTINGS": str(settings)})
    assert load_context(tmp_path / "lua_tmp1").scene_context().frame_range == "2-5"


def test_fusion_missing_render_range_gives_no_range(lua: str, tmp_path: Path) -> None:
    exe = tmp_path / "sqi-submit"
    exe.write_bytes(b"")
    settings = tmp_path / "settings.json"
    settings.write_text(json.dumps({"sqi_submit_command": str(exe)}), encoding="utf-8")
    got = run_lua(lua, _entry(tmp_path, start="nil"), {"SQI_SETTINGS": str(settings)})
    assert got["ok"] == "true"
    assert load_context(tmp_path / "lua_tmp1").scene_context().frame_range is None


@pytest.mark.parametrize("windows", [pytest.param(False, marks=_POSIX_PATH), True])
def test_fusion_finds_sqi_submit_on_path(lua: str, tmp_path: Path, windows: bool) -> None:
    (tmp_path / "empty").mkdir()
    exe = _on_path(tmp_path, "sqi-submit.EXE" if windows else "sqi-submit")
    path = (";" if windows else ":").join([str(tmp_path / "empty"), str(exe.parent)])
    got = run_lua(lua, _entry(tmp_path, windows=windows, path=path), {"SQI_SETTINGS": ""})
    assert got["ok"] == "true"
    events = _events(got)
    assert not [e for e in events if e.startswith("popen:")]  # no `where` or `command -v`
    if windows:
        assert events[-1].startswith(f'exec:start "" "{exe}" --context "')
    else:
        assert events[-1].startswith(f"exec:'{exe}' --context '")


@_POSIX_PATH
@pytest.mark.parametrize("blocker", ["file", "folder"])
def test_fusion_path_search_skips_what_cannot_run(lua: str, tmp_path: Path, blocker: str) -> None:
    """A non-executable file or a folder named sqi-submit earlier on PATH does not win."""
    if blocker == "file":
        first = _on_path(tmp_path, folder="first", mode=0o644)
    else:
        first = tmp_path / "first" / "sqi-submit"
        first.mkdir(parents=True)
    exe = _on_path(tmp_path)
    path = f"{first.parent}:{exe.parent}"
    got = run_lua(lua, _entry(tmp_path, path=path), {"SQI_SETTINGS": ""})
    assert got["ok"] == "true"
    assert _events(got)[-1].startswith(f"exec:'{exe}' --context '")


def test_fusion_settings_path(lua: str) -> None:
    """The settings file sits under the home sqi-submit reads: USERPROFILE on Windows, even with
    HOME set."""
    body = r"""
local env = { HOME = "/net/home/a", USERPROFILE = "C:\\Users\\a" }
os.getenv = function(k) return env[k] end
out("win", sqi_settings_path(true))
out("posix", sqi_settings_path(false))
env.SQI_SUBMITTER_SETTINGS = "/etc/sqi.json"
out("override", sqi_settings_path(true))
env = {}
out("none", tostring(sqi_settings_path(false)))
"""
    got = run_lua(lua, body)
    assert got["win"] == "C:\\Users\\a\\.sqi\\submitter.json"
    assert got["posix"] == "/net/home/a/.sqi/submitter.json"
    assert got["override"] == "/etc/sqi.json"
    assert got["none"] == "nil"


def test_fusion_windows_launch_prefers_the_windowed_exe(lua: str, tmp_path: Path) -> None:
    (tmp_path / "sqi-submit.exe").write_bytes(b"")
    (tmp_path / "sqi-submitw.exe").write_bytes(b"")
    settings = tmp_path / "settings.json"
    settings.write_text(
        json.dumps({"sqi_submit_command": str(tmp_path / "sqi-submit.exe")}), encoding="utf-8"
    )
    got = run_lua(lua, _entry(tmp_path, windows=True), {"SQI_SETTINGS": str(settings)})
    assert got["ok"] == "true"
    assert _events(got)[-1].startswith(
        f'exec:start "" "{tmp_path / "sqi-submitw.exe"}" --context "'
    )


def test_fusion_windows_context_file_is_named_under_temp(lua: str, tmp_path: Path) -> None:
    (tmp_path / "sqi-submit.exe").write_bytes(b"")
    settings = tmp_path / "settings.json"
    settings.write_text(
        json.dumps({"sqi_submit_command": str(tmp_path / "sqi-submit.exe")}), encoding="utf-8"
    )
    got = run_lua(lua, _entry(tmp_path, windows=True), {"SQI_SETTINGS": str(settings)})
    assert got["ok"] == "true"
    written = [p for p in tmp_path.glob("sqi-handoff-*.json")]
    assert len(written) == 1
    assert re.fullmatch(r"sqi-handoff-\d+-\d+\.json", written[0].name)
    assert load_context(written[0]).scene_context().frame_range == "1001-1100"


def test_fusion_not_found_names_the_fix(lua: str, tmp_path: Path) -> None:
    got = run_lua(lua, _entry(tmp_path), {"SQI_SETTINGS": ""})
    assert got["ok"] == "false"
    events = _events(got)
    assert [e for e in events if e.startswith("exec:")] == []
    assert events[-1].startswith("ask:Submit to sqi|sqi-submit was not found")
    assert "sqi_submit_command" in events[-1]


@pytest.mark.skipif(sys.platform == "win32", reason="Windows forbids a quote in a folder name")
def test_fusion_path_with_a_quote_is_refused(lua: str, tmp_path: Path) -> None:
    exe = _on_path(tmp_path, folder='say "hi"')
    got = run_lua(lua, _entry(tmp_path, path=str(exe.parent)), {"SQI_SETTINGS": ""})
    assert got["ok"] == "false"
    events = _events(got)
    assert [e for e in events if e.startswith("exec:")] == []
    assert "quote" in events[-1]


@_POSIX_PATH
def test_fusion_failed_launch_is_reported(lua: str, tmp_path: Path) -> None:
    exe = _on_path(tmp_path)
    got = run_lua(
        lua, _entry(tmp_path, path=str(exe.parent), execute_result="1"), {"SQI_SETTINGS": ""}
    )
    assert got["ok"] == "false"
    assert "could not be started" in _events(got)[-1]


def test_fusion_script_does_nothing_without_a_comp(lua: str) -> None:
    """Loading the file outside Fusion (no ``comp``) only defines functions."""
    got = run_lua(lua, 'out("type", type(sqi_submit))\nout("comp", tostring(comp))')
    assert got == {"type": "function", "comp": "nil"}


# -- install ---------------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("platform", "env", "expected"),
    [
        (
            "win32",
            {"APPDATA": "C:/Users/a/AppData/Roaming"},
            Path("C:/Users/a/AppData/Roaming")
            / "Blackmagic Design"
            / "Fusion"
            / "Scripts"
            / "Comp",
        ),
        (
            "darwin",
            {"HOME": "/Users/a"},
            Path("/Users/a")
            / "Library"
            / "Application Support"
            / "Blackmagic Design"
            / "Fusion"
            / "Scripts"
            / "Comp",
        ),
        (
            "linux",
            {"HOME": "/home/a"},
            Path("/home/a") / ".fusion" / "BlackmagicDesign" / "Fusion" / "Scripts" / "Comp",
        ),
        ("win32", {}, None),
        ("win32", {"HOME": "/home/a"}, None),
        ("darwin", {}, None),
        ("darwin", {"HOME": ""}, None),
        ("linux", {}, None),
    ],
)
def test_default_destination_fusion(
    platform: str, env: dict[str, str], expected: Path | None
) -> None:
    assert install.default_destination("fusion", platform, env) == expected


def test_fusion_files_and_hook_rules() -> None:
    assert install.HOST_FILES["fusion"] == ["hosts/fusion/Submit to sqi.lua"]
    assert "fusion" in install.KNOWN_HOSTS
    assert install.HOOK_MARKER not in SCRIPT.read_text(encoding="utf-8")  # a script, no hook


def test_fusion_next_step_and_no_destination_hint(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(install, "default_destination", lambda host, platform, env: None)
    out = io.StringIO()
    assert install.install_host("fusion", None, out) == 0
    text = out.getvalue()
    assert "Submit to sqi.lua" in text
    assert "Scripts" in text
    assert "In Fusion choose Script > Submit to sqi." in text


def test_install_host_copies_the_fusion_script_with_its_spaces(tmp_path: Path) -> None:
    dest = tmp_path / "Comp"
    out = io.StringIO()
    assert install.install_host("fusion", dest, out) == 0
    assert [p.name for p in dest.iterdir()] == ["Submit to sqi.lua"]
    assert (dest / "Submit to sqi.lua").read_bytes() == SCRIPT.read_bytes()
    assert out.getvalue().rstrip().endswith("In Fusion choose Script > Submit to sqi.")
