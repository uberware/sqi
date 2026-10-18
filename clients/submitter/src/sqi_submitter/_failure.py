# SPDX-License-Identifier: AGPL-3.0-or-later
"""Report a failed ``sqi-submit`` launch where the artist can see it.

A handoff host (Blender, Cinema 4D, After Effects) starts ``sqi-submit`` detached, with
no console, so a message printed to stderr alone reaches nobody. A failure is therefore
also appended to ``submit-error.log`` next to the submitter settings and, when stderr is
not a terminal (or the launch passed ``--detached``), shown in a native message box
(Windows) or dialog (macOS).

Everything here is best effort and stdlib only: reporting must never raise, so it cannot
mask the original failure or change the exit code. Only the message (and, for an
unexpected exception, its traceback) is written: the settings file and the context file
are never read into the log, but an unexpected exception's own message and traceback are
logged as they are.
"""

from __future__ import annotations

import contextlib
import os
import sys
from datetime import datetime
from pathlib import Path

__all__ = ["KEEP_LOG_BYTES", "LOG_NAME", "MAX_LOG_BYTES", "log_path", "report_failure"]

LOG_NAME = "submit-error.log"
MAX_LOG_BYTES = 64 * 1024
"""A log larger than this is cut down to its last ``KEEP_LOG_BYTES`` before appending."""
KEEP_LOG_BYTES = 16 * 1024


def log_path() -> Path:
    """``submit-error.log`` in the settings directory (``~/.sqi`` unless overridden)."""
    from sqi_submitter.core.session import Settings

    return Settings().path.parent / LOG_NAME


def report_failure(message: str, detail: str | None = None, *, detached: bool = False) -> None:
    """Print, log and (when detached) show *message*; *detail* goes to stderr and the log.

    *detached* (``sqi-submit --detached``, passed by a host launch) shows the message even
    when stderr is a terminal: After Effects' ``cmd /c start`` gives ``sqi-submit`` a
    console of its own, which closes with the process.
    """
    path = None
    with contextlib.suppress(Exception):
        path = _append_log(log_path(), message, detail)
    _print_stderr(message, detail, path)
    if detached or not _on_terminal():
        text = message if path is None else f"{message}\n\nDetails are in:\n{path}"
        with contextlib.suppress(Exception):
            show_message(text)


def show_message(text: str) -> None:
    """Show *text* natively: a message box on Windows, a dialog on macOS, else nothing."""
    platform = sys.platform
    with contextlib.suppress(Exception):
        if platform == "win32":
            import ctypes

            windll = getattr(ctypes, "windll")  # noqa: B009 - absent off Windows
            windll.user32.MessageBoxW(None, text, "sqi", 0x10)  # MB_ICONERROR
        elif platform == "darwin":
            import subprocess

            quoted = text.replace("\\", "\\\\").replace('"', '\\"')
            script = (
                f'display dialog "{quoted}" with title "sqi" '
                'with icon stop buttons {"OK"} default button "OK"'
            )
            subprocess.run(
                ["osascript", "-e", script],
                check=False,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )


def _append_log(path: Path, message: str, detail: str | None) -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    _trim(path)
    stamp = datetime.now().astimezone().isoformat(timespec="seconds")
    entry = f"{stamp} sqi-submit failed: {message}\n"
    if detail:
        entry += detail if detail.endswith("\n") else detail + "\n"
    with path.open("a", encoding="utf-8") as log:
        log.write(entry + "\n")
    return path


def _trim(path: Path) -> None:
    """Keep the log bounded: past ``MAX_LOG_BYTES``, keep only its last whole lines."""
    try:
        size = path.stat().st_size
    except OSError:
        return
    if size <= MAX_LOG_BYTES:
        return
    with path.open("rb") as log:
        log.seek(-KEEP_LOG_BYTES, os.SEEK_END)
        tail = log.read()
    newline = tail.find(b"\n")
    path.write_bytes(tail[newline + 1 :] if newline != -1 else b"")


def _print_stderr(message: str, detail: str | None, path: Path | None) -> None:
    stream = sys.stderr
    if stream is None:  # pythonw, or a detached process
        return
    with contextlib.suppress(Exception):
        if detail:
            stream.write(detail if detail.endswith("\n") else detail + "\n")
        stream.write(message + "\n")
        if path is not None:
            stream.write(f"(logged to {path})\n")
        stream.flush()


def _on_terminal() -> bool:
    stream = sys.stderr
    if stream is None:
        return False
    try:
        return bool(stream.isatty())
    except Exception:  # a closed or odd stream is not a terminal
        return False
