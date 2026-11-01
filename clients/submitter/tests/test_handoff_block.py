# SPDX-License-Identifier: AGPL-3.0-or-later
"""The shared handoff launch block copied into every Python handoff host."""

from __future__ import annotations

import ast
import ctypes
import json
import os
import shutil
import stat
import subprocess
import sys
from pathlib import Path
from types import SimpleNamespace

import pytest

from sqi_submitter import install
from sqi_submitter.hosts import _handoff_block as block
from tests.conftest import HANDOFF_BEGIN as BEGIN
from tests.conftest import HOSTS_DIR, handoff_host_files

HANDOFF_HOST_FILES = handoff_host_files()

# Hosts whose Python file runs sqi_submitter inside the host, so it carries no launch block.
IN_PROCESS_HOSTS = {"vred"}

# Handoff hosts whose files need not parse on Python 3.7 (Katana 4-5 and Silhouette 2022 ship
# it), with the reason.
NOT_PY37_HOSTS = {"blender": "the add-on evaluates list[...] annotations, so needs 3.9+"}

END = "# --- sqi handoff end ---"


def _python_host_files() -> list[tuple[str, Path]]:
    """Each Python file ``install-host`` ships for a handoff host, with its host."""
    return [
        (host, HOSTS_DIR.parent / rel)
        for host, files in install.HOST_FILES.items()
        if host not in IN_PROCESS_HOSTS
        for rel in files
        if rel.endswith((".py", ".pyp"))
    ]


NOT_FOUND = (
    "sqi-submit was not found. Install it on this machine with: "
    'pip install "sqi-submitter[qt]" \u2014 then set its full path in the sqi '
    "add-on/plugin settings, or as sqi_submit_command in ~/.sqi/submitter.json."
)


def _extract(path: Path) -> str:
    text = path.read_text(encoding="utf-8")
    assert text.count(BEGIN) == 1, f"{path} needs exactly one begin marker"
    assert text.count(END) == 1, f"{path} needs exactly one end marker"
    return text.split(BEGIN, 1)[1].split(END, 1)[0]


@pytest.fixture
def isolated(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    """No settings file and no sqi-submit on PATH, unless a test adds them."""
    settings = tmp_path / "no-such-settings.json"
    monkeypatch.setenv("SQI_SUBMITTER_SETTINGS", str(settings))
    monkeypatch.setattr(shutil, "which", lambda _name: None)
    return settings


def _fake_exe(tmp_path: Path, name: str) -> str:
    exe = tmp_path / name
    exe.write_text("", encoding="utf-8")
    return str(exe)


def test_resolve_submit_command_prefers_configured(tmp_path: Path, isolated: Path) -> None:
    configured = _fake_exe(tmp_path, "configured-submit")
    isolated.write_text(
        json.dumps({"sqi_submit_command": _fake_exe(tmp_path, "from-file")}), encoding="utf-8"
    )
    assert block.resolve_submit_command(configured) == configured


def test_resolve_submit_command_reads_settings_file(tmp_path: Path, isolated: Path) -> None:
    from_file = _fake_exe(tmp_path, "from-file")
    isolated.write_text(json.dumps({"sqi_submit_command": from_file}), encoding="utf-8")
    assert block.resolve_submit_command("") == from_file
    # A configured path that does not exist falls through to the file.
    assert block.resolve_submit_command(str(tmp_path / "gone")) == from_file


def test_resolve_submit_command_falls_back_to_path(
    tmp_path: Path, isolated: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    on_path = _fake_exe(tmp_path, "sqi-submit")
    asked: list[str] = []

    def which(name: str) -> str:
        asked.append(name)
        return on_path

    monkeypatch.setattr(shutil, "which", which)
    assert block.resolve_submit_command("") == on_path
    assert asked == ["sqi-submit"]
    # A settings file that is unreadable JSON is ignored, not fatal.
    isolated.write_text("{not json", encoding="utf-8")
    assert block.resolve_submit_command("") == on_path


def test_resolve_submit_command_reads_settings_file_with_bom(
    tmp_path: Path, isolated: Path
) -> None:
    """PowerShell 5's ``Set-Content -Encoding utf8`` writes a BOM; it must not hide the path."""
    from_file = _fake_exe(tmp_path, "from-file")
    isolated.write_bytes(
        b"\xef\xbb\xbf" + json.dumps({"sqi_submit_command": from_file}).encode("utf-8")
    )
    assert block.resolve_submit_command("") == from_file


def test_settings_file_is_found_under_userprofile_on_windows(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """sqi-submit reads USERPROFILE on Windows; a HOME set there (Python 3.7 ranks it first in
    ``expanduser``) must not send the host to a different settings file."""
    from_profile = _fake_exe(tmp_path, "from-profile")
    settings = tmp_path / "profile" / ".sqi" / "submitter.json"
    settings.parent.mkdir(parents=True)
    settings.write_text(json.dumps({"sqi_submit_command": from_profile}), encoding="utf-8")
    monkeypatch.delenv("SQI_SUBMITTER_SETTINGS", raising=False)
    monkeypatch.setenv("USERPROFILE", str(tmp_path / "profile"))
    monkeypatch.setenv("HOME", str(tmp_path / "network-home"))
    monkeypatch.setattr(sys, "platform", "win32")
    monkeypatch.setattr(shutil, "which", lambda _name: None)
    assert block.resolve_submit_command("") == from_profile


def test_resolve_submit_command_not_found_message(isolated: Path) -> None:
    with pytest.raises(RuntimeError) as excinfo:
        block.resolve_submit_command("")
    assert str(excinfo.value) == NOT_FOUND


def test_write_context_is_valid_json(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("tempfile.gettempdir", lambda: str(tmp_path))
    data: dict[str, object] = {"version": 1, "scene": "a/b.ma", "targets": [{"name": "x"}]}
    first = Path(block.write_context(data))
    second = Path(block.write_context(data))
    assert first != second
    assert first.parent == tmp_path / "sqi-handoff"
    assert first.name.startswith("context-") and first.suffix == ".json"
    assert json.loads(first.read_text(encoding="utf-8")) == data


def test_write_context_avoids_a_non_directory(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Something else squatting on ``<tmp>/sqi-handoff`` gets a private folder instead."""
    monkeypatch.setattr("tempfile.gettempdir", lambda: str(tmp_path))
    (tmp_path / "sqi-handoff").write_text("not a folder", encoding="utf-8")
    path = Path(block.write_context({"version": 1}))
    assert path.parent.parent == tmp_path
    assert path.parent.name.startswith("sqi-handoff-")
    assert json.loads(path.read_text(encoding="utf-8")) == {"version": 1}


_POSIX_ONLY = pytest.mark.skipif(sys.platform == "win32", reason="POSIX permissions and owners")


def _mode(path: Path) -> int:
    return stat.S_IMODE(os.lstat(path).st_mode)


@_POSIX_ONLY
def test_write_context_tightens_an_open_folder(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr("tempfile.gettempdir", lambda: str(tmp_path))
    folder = tmp_path / "sqi-handoff"
    folder.mkdir()
    folder.chmod(0o777)
    path = Path(block.write_context({"version": 1}))
    assert path.parent == folder
    assert _mode(folder) == 0o700
    assert _mode(path) == 0o600


@_POSIX_ONLY
def test_write_context_new_folder_and_file_are_private(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr("tempfile.gettempdir", lambda: str(tmp_path))
    previous = os.umask(0)  # the modes must not depend on the caller's umask
    try:
        path = Path(block.write_context({"version": 1}))
    finally:
        os.umask(previous)
    assert _mode(path.parent) == 0o700
    assert _mode(path) == 0o600


@_POSIX_ONLY
def test_write_context_refuses_a_folder_another_user_owns(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr("tempfile.gettempdir", lambda: str(tmp_path))
    folder = tmp_path / "sqi-handoff"
    folder.mkdir()
    folder.chmod(0o777)
    # The folder is really ours; pretend we are someone else. (st_uid, not os.getuid,
    # so the test still type-checks on a Windows host, where os has no getuid.)
    someone_else = folder.stat().st_uid + 1
    monkeypatch.setattr("os.getuid", lambda: someone_else)
    path = Path(block.write_context({"version": 1}))
    assert path.parent != folder
    assert path.parent.parent == tmp_path
    assert path.parent.name.startswith("sqi-handoff-")
    assert list(folder.iterdir()) == []  # nothing was written into the foreign folder
    assert _mode(folder) == 0o777  # and it was not touched
    assert _mode(path.parent) == 0o700
    assert _mode(path) == 0o600


@_POSIX_ONLY
def test_write_context_refuses_a_symlinked_folder(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr("tempfile.gettempdir", lambda: str(tmp_path))
    target = tmp_path / "attacker"
    target.mkdir()
    (tmp_path / "sqi-handoff").symlink_to(target, target_is_directory=True)
    path = Path(block.write_context({"version": 1}))
    assert path.parent.parent == tmp_path
    assert path.parent.name.startswith("sqi-handoff-")
    assert list(target.iterdir()) == []


class _Recorder:
    def __init__(self) -> None:
        self.calls: list[tuple[list[str], dict[str, object]]] = []

    def popen(self, argv: list[str], **kwargs: object) -> SimpleNamespace:
        self.calls.append((argv, kwargs))
        return SimpleNamespace(pid=4242)


def test_launch_detaches_posix(monkeypatch: pytest.MonkeyPatch) -> None:
    rec = _Recorder()
    monkeypatch.setattr(subprocess, "Popen", rec.popen)
    monkeypatch.setattr(sys, "platform", "linux")
    block.launch("/opt/sqi-submit", "/tmp/ctx.json")
    [(argv, kwargs)] = rec.calls
    assert argv == ["/opt/sqi-submit", "--context", "/tmp/ctx.json"]
    assert kwargs["start_new_session"] is True
    assert "creationflags" not in kwargs


def test_launch_detaches_windows(monkeypatch: pytest.MonkeyPatch) -> None:
    rec = _Recorder()
    allowed: list[int] = []
    user32 = SimpleNamespace(AllowSetForegroundWindow=allowed.append)
    monkeypatch.setattr(subprocess, "Popen", rec.popen)
    monkeypatch.setattr(sys, "platform", "win32")
    monkeypatch.setattr(ctypes, "windll", SimpleNamespace(user32=user32), raising=False)
    block.launch("C:/sqi/sqi-submit.exe", "C:/tmp/ctx.json")
    [(argv, kwargs)] = rec.calls
    assert argv == ["C:/sqi/sqi-submit.exe", "--context", "C:/tmp/ctx.json"]
    assert kwargs["creationflags"] == 0x00000008 | 0x00000200  # DETACHED | NEW_PROCESS_GROUP
    assert "start_new_session" not in kwargs
    # ASFW_ANY, not the child's pid: a pip/pipx/uv sqi-submit.exe launcher runs the
    # real python.exe as its own child, and the window lives in that grandchild.
    assert allowed == [-1]


@pytest.mark.parametrize("name", ["sqi-submit.exe", "SQI-SUBMIT.EXE", "sqi-submit.EXE"])
def test_windowed_command_prefers_sqi_submitw(tmp_path: Path, name: str) -> None:
    # shutil.which spells the extension the way PATHEXT does (".EXE").
    windowed = _fake_exe(tmp_path, "sqi-submitw.exe")
    assert block.windowed_command(str(tmp_path / name)) == windowed


def test_windowed_command_keeps_a_console_exe_with_no_sibling(tmp_path: Path) -> None:
    exe = _fake_exe(tmp_path, "sqi-submit.exe")
    assert block.windowed_command(exe) == exe


@pytest.mark.parametrize("name", ["sqi-submit.cmd", "sqi-submit", "my-sqi-submit.exe"])
def test_windowed_command_leaves_other_programs_alone(tmp_path: Path, name: str) -> None:
    _fake_exe(tmp_path, "sqi-submitw.exe")
    exe = str(tmp_path / name)
    assert block.windowed_command(exe) == exe


def test_launch_windows_starts_sqi_submitw(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    rec = _Recorder()
    user32 = SimpleNamespace(AllowSetForegroundWindow=lambda _pid: None)
    monkeypatch.setattr(subprocess, "Popen", rec.popen)
    monkeypatch.setattr(sys, "platform", "win32")
    monkeypatch.setattr(ctypes, "windll", SimpleNamespace(user32=user32), raising=False)
    windowed = _fake_exe(tmp_path, "sqi-submitw.exe")
    block.launch(_fake_exe(tmp_path, "sqi-submit.exe"), "C:/tmp/ctx.json")
    [(argv, _kwargs)] = rec.calls
    assert argv == [windowed, "--context", "C:/tmp/ctx.json"]


def test_launch_posix_never_swaps_the_command(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    rec = _Recorder()
    monkeypatch.setattr(subprocess, "Popen", rec.popen)
    monkeypatch.setattr(sys, "platform", "linux")
    _fake_exe(tmp_path, "sqi-submitw.exe")
    exe = _fake_exe(tmp_path, "sqi-submit.exe")
    block.launch(exe, "/tmp/ctx.json")
    [(argv, _kwargs)] = rec.calls
    assert argv[0] == exe


def test_launch_windows_ignores_foreground_errors(monkeypatch: pytest.MonkeyPatch) -> None:
    rec = _Recorder()

    def boom(_pid: int) -> None:
        raise OSError("no desktop")

    monkeypatch.setattr(subprocess, "Popen", rec.popen)
    monkeypatch.setattr(sys, "platform", "win32")
    monkeypatch.setattr(
        ctypes,
        "windll",
        SimpleNamespace(user32=SimpleNamespace(AllowSetForegroundWindow=boom)),
        raising=False,
    )
    block.launch("sqi-submit", "ctx.json")
    assert len(rec.calls) == 1


def test_block_is_self_contained() -> None:
    text = _extract(HOSTS_DIR / "_handoff_block.py")
    assert "sqi_submitter" not in text
    for needed in ("contextlib", "json", "os", "shutil", "stat", "subprocess", "sys", "tempfile"):
        assert f"import {needed}\n" in text


def test_block_identical_in_every_host() -> None:
    canonical = _extract(HOSTS_DIR / "_handoff_block.py")
    for host_file in HANDOFF_HOST_FILES:
        assert _extract(host_file) == canonical, f"{host_file} drifted from _handoff_block.py"


def test_every_handoff_python_file_carries_the_block_or_is_a_hook() -> None:
    """Every Python file a handoff host ships carries the launch block or is a hook."""
    found = set(HANDOFF_HOST_FILES)
    for host, path in _python_host_files():
        hook = install.HOOK_MARKER in path.read_text(encoding="utf-8").splitlines()
        assert path in found or hook, f"{host}: {path.name} has no launch block and is no hook"
    assert len(found) >= 7  # blender, cinema4d, unreal, katana, keyshot, silhouette, modo


def test_handoff_hosts_parse_as_python37() -> None:
    for host, path in _python_host_files():
        if host in NOT_PY37_HOSTS:
            continue
        text = path.read_text(encoding="utf-8")
        ast.parse(text, filename=str(path), feature_version=(3, 7))
        assert "from __future__ import annotations" in text, f"{path} needs the __future__ import"
