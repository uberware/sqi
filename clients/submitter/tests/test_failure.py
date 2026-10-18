# SPDX-License-Identifier: AGPL-3.0-or-later
"""A failed ``sqi-submit`` launch is reported where a detached artist can see it."""

from __future__ import annotations

import ctypes
import io
import json
import re
import shutil
import subprocess
import sys
import types
from pathlib import Path
from typing import Any

import pytest

import sqi_submitter
from sqi_submitter import _failure, cli
from sqi_submitter.core.session import Settings
from tests.conftest import no_qt

# Taken before the autouse ``native_messages`` fixture replaces it.
REAL_SHOW_MESSAGE = _failure.show_message

SECRET_URL = "http://farm.secret.example:8080"
SECRET_KEY = "sqi_key_do_not_log"


class _Tty(io.StringIO):
    def isatty(self) -> bool:
        return True


class _Broken(io.StringIO):
    def write(self, s: str) -> int:
        raise OSError("stderr is gone")

    def isatty(self) -> bool:
        raise ValueError("closed")


@pytest.fixture
def sqi_dir(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> Path:
    """A private ~/.sqi holding settings that carry a server URL and an API key."""
    directory = tmp_path / ".sqi"
    directory.mkdir()
    settings = directory / "submitter.json"
    settings.write_text(
        json.dumps({"server_url": SECRET_URL, "api_key": SECRET_KEY}), encoding="utf-8"
    )
    monkeypatch.setenv("SQI_SUBMITTER_SETTINGS", str(settings))
    monkeypatch.setenv("SQI_SERVER_URL", SECRET_URL)
    monkeypatch.setenv("SQI_API_KEY", SECRET_KEY)
    return directory


def _log(directory: Path) -> str:
    return (directory / _failure.LOG_NAME).read_text(encoding="utf-8")


def test_log_path_follows_the_settings_override(sqi_dir: Path) -> None:
    assert _failure.log_path() == sqi_dir / "submit-error.log"
    assert _failure.log_path().parent == Settings().path.parent


def test_log_path_defaults_to_home_dot_sqi(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    monkeypatch.delenv("SQI_SUBMITTER_SETTINGS", raising=False)
    monkeypatch.setenv("HOME", str(tmp_path))
    monkeypatch.setenv("USERPROFILE", str(tmp_path))
    assert _failure.log_path() == tmp_path / ".sqi" / "submit-error.log"


def test_report_creates_the_directory(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    monkeypatch.setenv("SQI_SUBMITTER_SETTINGS", str(tmp_path / "new" / ".sqi" / "s.json"))
    _failure.report_failure("No Qt binding found.")
    assert "No Qt binding found." in _log(tmp_path / "new" / ".sqi")


def test_report_logs_a_timestamped_message_and_nothing_else(sqi_dir: Path) -> None:
    _failure.report_failure("No Qt binding found.")
    _failure.report_failure("Second failure.")

    text = _log(sqi_dir)
    assert "No Qt binding found." in text
    assert text.index("No Qt binding found.") < text.index("Second failure.")
    assert len(re.findall(r"^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d", text, flags=re.MULTILINE)) == 2
    assert SECRET_URL not in text
    assert SECRET_KEY not in text
    assert "farm.secret" not in text


def test_report_logs_the_detail(sqi_dir: Path) -> None:
    _failure.report_failure("It broke.", "Traceback (most recent call last):\n  boom\n")
    text = _log(sqi_dir)
    assert "It broke." in text
    assert "Traceback (most recent call last):" in text


def test_log_is_trimmed_before_it_grows_without_bound(sqi_dir: Path) -> None:
    log = sqi_dir / _failure.LOG_NAME
    line = "old entry " + "x" * 90 + "\n"
    log.write_text(line * 700, encoding="utf-8")  # ~70 KiB
    assert log.stat().st_size > _failure.MAX_LOG_BYTES

    _failure.report_failure("The newest failure.")

    data = log.read_bytes()
    assert len(data) <= _failure.KEEP_LOG_BYTES + 1024
    assert data.decode("utf-8").rstrip().endswith("The newest failure.")
    assert data.startswith(b"old entry ")  # cut at a line boundary, never mid-line


def test_small_log_is_kept(sqi_dir: Path) -> None:
    log = sqi_dir / _failure.LOG_NAME
    log.write_text("first entry\n", encoding="utf-8")
    _failure.report_failure("Second.")
    assert _log(sqi_dir).startswith("first entry")


def test_message_goes_to_stderr(sqi_dir: Path, capsys: pytest.CaptureFixture[str]) -> None:
    _failure.report_failure("No Qt binding found.")
    err = capsys.readouterr().err
    assert "No Qt binding found." in err
    assert str(sqi_dir / _failure.LOG_NAME) in err


def test_stderr_none_does_not_crash(
    sqi_dir: Path, monkeypatch: pytest.MonkeyPatch, native_messages: list[str]
) -> None:
    monkeypatch.setattr(sys, "stderr", None)
    _failure.report_failure("No Qt binding found.")
    assert "No Qt binding found." in _log(sqi_dir)
    # Detached (pythonw): nothing reads stderr, so the message box is the report.
    (shown,) = native_messages
    assert shown.startswith("No Qt binding found.")
    assert shown.endswith(str(sqi_dir / _failure.LOG_NAME))


def test_no_message_box_on_a_terminal(
    sqi_dir: Path, monkeypatch: pytest.MonkeyPatch, native_messages: list[str]
) -> None:
    monkeypatch.setattr(sys, "stderr", _Tty())
    _failure.report_failure("No Qt binding found.")
    assert native_messages == []
    assert "No Qt binding found." in _log(sqi_dir)


def test_message_box_when_stderr_is_not_a_terminal(
    sqi_dir: Path, monkeypatch: pytest.MonkeyPatch, native_messages: list[str]
) -> None:
    monkeypatch.setattr(sys, "stderr", io.StringIO())  # redirected: not a tty, even under -s
    _failure.report_failure("No Qt binding found.")
    assert len(native_messages) == 1


def test_detached_shows_the_message_box_even_on_a_terminal(
    sqi_dir: Path, monkeypatch: pytest.MonkeyPatch, native_messages: list[str]
) -> None:
    # After Effects' `cmd /c start` gives sqi-submit a console of its own: stderr is a tty
    # nobody will read once the process exits.
    stderr = _Tty()
    monkeypatch.setattr(sys, "stderr", stderr)
    _failure.report_failure("No Qt binding found.", detached=True)
    (shown,) = native_messages
    assert shown.startswith("No Qt binding found.")
    assert "No Qt binding found." in stderr.getvalue()
    assert "No Qt binding found." in _log(sqi_dir)


def test_cli_detached_flag_shows_the_box_on_a_terminal(
    sqi_dir: Path, monkeypatch: pytest.MonkeyPatch, native_messages: list[str]
) -> None:
    no_qt(monkeypatch)
    monkeypatch.setattr(sys, "stderr", _Tty())
    assert cli.main(["--context", str(sqi_dir / "ctx.json"), "--detached"]) == 1
    (shown,) = native_messages
    assert shown.startswith("No Qt binding found")


def test_cli_without_detached_a_terminal_suppresses_the_box(
    sqi_dir: Path, monkeypatch: pytest.MonkeyPatch, native_messages: list[str]
) -> None:
    no_qt(monkeypatch)
    monkeypatch.setattr(sys, "stderr", _Tty())
    assert cli.main([]) == 1
    assert native_messages == []
    assert "No Qt binding found" in _log(sqi_dir)


def test_cli_detached_also_covers_an_unexpected_exception(
    sqi_dir: Path, monkeypatch: pytest.MonkeyPatch, native_messages: list[str]
) -> None:
    def explode(server: str | None, context: str | None) -> int:
        raise RuntimeError("boom")

    monkeypatch.setattr(cli, "_run_dialog", explode)
    monkeypatch.setattr(sys, "stderr", _Tty())
    assert cli.main(["--detached"]) == 1
    (shown,) = native_messages
    assert "boom" in shown


def test_cli_detached_flag_is_hidden(capsys: pytest.CaptureFixture[str]) -> None:
    with pytest.raises(SystemExit):
        cli.main(["--help"])
    assert "--detached" not in capsys.readouterr().out


def test_cli_version_prints_to_stdout_without_qt(
    sqi_dir: Path,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
    native_messages: list[str],
) -> None:
    # The After Effects panel probes --version before each launch: it must answer on
    # stdout, exit 0 and need no Qt binding.
    no_qt(monkeypatch)
    with pytest.raises(SystemExit) as exited:
        cli.main(["--version"])
    assert exited.value.code == 0
    out, err = capsys.readouterr()
    assert out == f"sqi-submit {sqi_submitter.__version__}\n"
    assert err == ""
    assert native_messages == []
    assert not (sqi_dir / _failure.LOG_NAME).exists()


def test_cli_version_is_listed_in_help(capsys: pytest.CaptureFixture[str]) -> None:
    with pytest.raises(SystemExit):
        cli.main(["--help"])
    assert "--version" in capsys.readouterr().out


def test_windows_shows_a_message_box(
    sqi_dir: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    calls: list[tuple[Any, ...]] = []

    def message_box(*args: Any) -> int:
        calls.append(args)
        return 1

    run = _Forbidden()
    user32 = types.SimpleNamespace(MessageBoxW=message_box)
    monkeypatch.setattr(_failure, "show_message", REAL_SHOW_MESSAGE)
    monkeypatch.setattr(ctypes, "windll", types.SimpleNamespace(user32=user32), raising=False)
    monkeypatch.setattr(sys, "platform", "win32")
    monkeypatch.setattr(subprocess, "run", run)

    _failure.report_failure("No Qt binding found.")

    assert run.used == []
    ((owner, text, title, flags),) = calls
    assert owner is None
    assert title == "sqi"
    assert flags == 0x10
    assert text.startswith("No Qt binding found.")
    assert text.endswith(str(sqi_dir / _failure.LOG_NAME))


def test_macos_shows_a_dialog_with_the_text_escaped(
    sqi_dir: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    calls: list[tuple[Any, dict[str, Any]]] = []

    def run(*args: Any, **kwargs: Any) -> subprocess.CompletedProcess[bytes]:
        calls.append((args, kwargs))
        return subprocess.CompletedProcess(args[0], 0)

    windll = _Forbidden()
    monkeypatch.setattr(_failure, "show_message", REAL_SHOW_MESSAGE)
    monkeypatch.setattr(ctypes, "windll", windll, raising=False)
    monkeypatch.setattr(sys, "platform", "darwin")
    monkeypatch.setattr(subprocess, "run", run)

    _failure.report_failure('Bad "path" C:\\x')

    assert windll.used == []
    ((args, kwargs),) = calls
    argv = args[0]
    assert argv[:2] == ["osascript", "-e"]
    assert len(argv) == 3
    script = argv[2]
    assert script.startswith('display dialog "Bad \\"path\\" C:\\\\x')
    assert 'with icon stop buttons {"OK"}' in script
    assert kwargs.get("shell") in (None, False)
    assert script.rstrip().endswith('default button "OK"')
    assert str(sqi_dir / _failure.LOG_NAME).replace("\\", "\\\\") in script


@pytest.mark.parametrize("platform", ["freebsd14", "openbsd7"])
def test_other_platforms_only_log(
    sqi_dir: Path,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
    platform: str,
) -> None:
    windll, run, popen = _Forbidden(), _Forbidden(), _Forbidden()
    monkeypatch.setattr(_failure, "show_message", REAL_SHOW_MESSAGE)
    monkeypatch.setattr(ctypes, "windll", windll, raising=False)
    monkeypatch.setattr(sys, "platform", platform)
    monkeypatch.setattr(subprocess, "run", run)
    monkeypatch.setattr(subprocess, "Popen", popen)

    _failure.report_failure("No Qt binding found.")

    assert "No Qt binding found." in _log(sqi_dir)
    assert windll.used == run.used == popen.used == []


LINUX_TOOLS = ["zenity", "kdialog", "xmessage", "notify-send"]


class _LinuxDesktop:
    """A Linux session whose ``shutil.which`` finds exactly ``installed``."""

    def __init__(self, monkeypatch: pytest.MonkeyPatch) -> None:
        self.installed: list[str] = list(LINUX_TOOLS)
        self.looked_up: list[str] = []
        self.runs: list[tuple[list[str], dict[str, Any]]] = []
        self.windll = _Forbidden()
        monkeypatch.setattr(_failure, "show_message", REAL_SHOW_MESSAGE)
        monkeypatch.setattr(ctypes, "windll", self.windll, raising=False)
        monkeypatch.setattr(sys, "platform", "linux")
        monkeypatch.setattr(sys, "stderr", io.StringIO())  # not a terminal
        monkeypatch.setenv("DISPLAY", ":0")
        monkeypatch.delenv("WAYLAND_DISPLAY", raising=False)
        monkeypatch.setattr(shutil, "which", self.which)
        monkeypatch.setattr(subprocess, "run", self.run)

    def which(self, name: str, *args: Any, **kwargs: Any) -> str | None:
        self.looked_up.append(name)
        return f"/usr/bin/{name}" if name in self.installed else None

    def run(self, *args: Any, **kwargs: Any) -> subprocess.CompletedProcess[bytes]:
        self.runs.append((list(args[0]), kwargs))
        return subprocess.CompletedProcess(args[0], 0)


@pytest.fixture
def linux_desktop(sqi_dir: Path, monkeypatch: pytest.MonkeyPatch) -> _LinuxDesktop:
    return _LinuxDesktop(monkeypatch)


def _expected_text(sqi_dir: Path, message: str) -> str:
    return f"{message}\n\nDetails are in:\n{sqi_dir / _failure.LOG_NAME}"


@pytest.mark.parametrize(
    ("installed", "argv_tail"),
    [
        (LINUX_TOOLS, ["--error", "--no-markup", "--title", "sqi", "--text", "TEXT"]),
        (LINUX_TOOLS[1:], ["--error", "TEXT", "--title", "sqi"]),
        (LINUX_TOOLS[2:], ["-center", "TEXT"]),
        (LINUX_TOOLS[3:], ["-u", "critical", "sqi", "TEXT"]),
    ],
)
def test_linux_uses_the_first_installed_tool(
    sqi_dir: Path, linux_desktop: _LinuxDesktop, installed: list[str], argv_tail: list[str]
) -> None:
    linux_desktop.installed = installed
    message = 'No Qt binding found. <b>$(rm -rf ~)</b> "quoted" `x`'

    _failure.report_failure(message)

    text = _expected_text(sqi_dir, message)
    ((argv, kwargs),) = linux_desktop.runs
    tool = installed[0]
    assert argv == [f"/usr/bin/{tool}", *(text if a == "TEXT" else a for a in argv_tail)]
    assert linux_desktop.looked_up == LINUX_TOOLS[: LINUX_TOOLS.index(tool) + 1]
    assert kwargs.get("shell") in (None, False)
    assert kwargs.get("stdin") == subprocess.DEVNULL
    assert linux_desktop.windll.used == []


def test_linux_wayland_alone_is_a_display(
    sqi_dir: Path, linux_desktop: _LinuxDesktop, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.delenv("DISPLAY", raising=False)
    monkeypatch.setenv("WAYLAND_DISPLAY", "wayland-0")
    _failure.report_failure("No Qt binding found.")
    ((argv, _),) = linux_desktop.runs
    assert argv[0] == "/usr/bin/zenity"


@pytest.mark.parametrize("empty", [None, ""])
def test_linux_without_a_display_only_logs(
    sqi_dir: Path,
    linux_desktop: _LinuxDesktop,
    monkeypatch: pytest.MonkeyPatch,
    empty: str | None,
) -> None:
    for name in ("DISPLAY", "WAYLAND_DISPLAY"):
        if empty is None:
            monkeypatch.delenv(name, raising=False)
        else:
            monkeypatch.setenv(name, empty)
    _failure.report_failure("No Qt binding found.")
    assert linux_desktop.runs == []
    assert linux_desktop.looked_up == []
    assert "No Qt binding found." in _log(sqi_dir)


def test_linux_without_a_tool_only_logs(sqi_dir: Path, linux_desktop: _LinuxDesktop) -> None:
    linux_desktop.installed = []
    _failure.report_failure("No Qt binding found.")  # must not raise
    assert linux_desktop.runs == []
    assert linux_desktop.looked_up == LINUX_TOOLS
    assert "No Qt binding found." in _log(sqi_dir)


@pytest.mark.parametrize("error", [FileNotFoundError("gone"), OSError("exec failed")])
def test_linux_a_failing_tool_is_swallowed(
    sqi_dir: Path,
    linux_desktop: _LinuxDesktop,
    monkeypatch: pytest.MonkeyPatch,
    error: Exception,
) -> None:
    def broken(*args: Any, **kwargs: Any) -> subprocess.CompletedProcess[bytes]:
        raise error

    monkeypatch.setattr(subprocess, "run", broken)
    _failure.report_failure("No Qt binding found.")  # must not raise
    assert "No Qt binding found." in _log(sqi_dir)


def test_linux_on_a_terminal_runs_nothing(
    sqi_dir: Path, linux_desktop: _LinuxDesktop, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(sys, "stderr", _Tty())
    _failure.report_failure("No Qt binding found.")
    assert linux_desktop.runs == []


def test_linux_detached_runs_the_tool_on_a_terminal(
    sqi_dir: Path, linux_desktop: _LinuxDesktop, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(sys, "stderr", _Tty())
    _failure.report_failure("No Qt binding found.", detached=True)
    ((argv, _),) = linux_desktop.runs
    assert argv[0] == "/usr/bin/zenity"
    assert argv[-1] == _expected_text(sqi_dir, "No Qt binding found.")


def test_linux_cli_detached_flag_runs_the_tool(
    sqi_dir: Path, linux_desktop: _LinuxDesktop, monkeypatch: pytest.MonkeyPatch
) -> None:
    no_qt(monkeypatch)
    monkeypatch.setattr(sys, "stderr", _Tty())
    assert cli.main(["--context", str(sqi_dir / "ctx.json"), "--detached"]) == 1
    ((argv, _),) = linux_desktop.runs
    assert argv[-1].startswith("No Qt binding found")


def test_exceptions_inside_the_reporter_are_swallowed(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    def broken_path() -> Path:
        raise OSError("no home")

    def broken_show(text: str) -> None:
        raise RuntimeError("no display")

    monkeypatch.setattr(_failure, "log_path", broken_path)
    monkeypatch.setattr(_failure, "show_message", broken_show)
    monkeypatch.setattr(sys, "stderr", _Broken())

    _failure.report_failure("No Qt binding found.", "detail")  # must not raise


def test_real_show_message_swallows_a_failing_backend(monkeypatch: pytest.MonkeyPatch) -> None:
    windll, run = _Forbidden(), _Forbidden()
    monkeypatch.setattr(sys, "platform", "win32")
    monkeypatch.setattr(ctypes, "windll", windll, raising=False)
    REAL_SHOW_MESSAGE("text")  # must not raise
    monkeypatch.setattr(sys, "platform", "darwin")
    monkeypatch.setattr(subprocess, "run", run)
    REAL_SHOW_MESSAGE("text")
    monkeypatch.setattr(sys, "platform", "linux")
    monkeypatch.setenv("DISPLAY", ":0")
    monkeypatch.setattr(shutil, "which", lambda name, *a, **k: f"/usr/bin/{name}")
    REAL_SHOW_MESSAGE("text")
    assert windll.used == ["user32"]
    assert run.used == ["call", "call"]


def test_unwritable_log_still_reports(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
    capsys: pytest.CaptureFixture[str],
    native_messages: list[str],
) -> None:
    blocker = tmp_path / "file"
    blocker.write_text("", encoding="utf-8")
    monkeypatch.setenv("SQI_SUBMITTER_SETTINGS", str(blocker / "s.json"))  # parent is a file
    _failure.report_failure("No Qt binding found.")
    assert "No Qt binding found." in capsys.readouterr().err
    assert native_messages == ["No Qt binding found."]


def test_cli_unexpected_exception_is_logged_with_traceback(
    sqi_dir: Path,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
    native_messages: list[str],
) -> None:
    def explode(server: str | None, context: str | None) -> int:
        raise RuntimeError("boom")

    monkeypatch.setattr(cli, "_run_dialog", explode)

    assert cli.main([]) == 1

    text = _log(sqi_dir)
    assert "Traceback (most recent call last):" in text
    assert "RuntimeError: boom" in text
    assert SECRET_URL not in text
    assert "RuntimeError: boom" in capsys.readouterr().err
    (shown,) = native_messages
    assert "boom" in shown


def test_cli_install_host_does_not_report(
    sqi_dir: Path, tmp_path: Path, native_messages: list[str]
) -> None:
    blocker = tmp_path / "blocker"
    blocker.write_text("", encoding="utf-8")
    assert cli.main(["install-host", "blender", "--dest", str(blocker)]) == 1
    assert native_messages == []
    assert not (sqi_dir / _failure.LOG_NAME).exists()


class _Forbidden:
    """A native backend that must stay untouched: records any use (the reporter swallows
    exceptions, so raising alone would let a wrong branch pass unnoticed)."""

    def __init__(self) -> None:
        self.used: list[str] = []

    def __getattr__(self, name: str) -> Any:
        self.used.append(name)
        raise AssertionError(f"{name} must not be used here")

    def __call__(self, *args: Any, **kwargs: Any) -> Any:
        self.used.append("call")
        raise AssertionError("must not be called")
