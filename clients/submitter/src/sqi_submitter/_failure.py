# SPDX-License-Identifier: AGPL-3.0-or-later
"""Report a failed ``sqi-submit`` launch where the artist can see it.

A handoff host (Blender, Cinema 4D, After Effects) starts ``sqi-submit`` detached, with
no console, so a message printed to stderr alone reaches nobody. A failure is therefore
also appended to ``submit-error.log`` next to the submitter settings and, when stderr is
not a terminal (or the launch passed ``--detached``), shown in a native message box
(Windows), dialog (macOS) or, on a Linux desktop, the first of ``zenity``, ``kdialog``,
``xmessage`` and ``notify-send`` that is installed.

Everything here is best effort and stdlib only: reporting must never raise, so it cannot
mask the original failure or change the exit code. Only the message (and, for an
unexpected exception, its traceback) is written: the settings file and the context file
are never read into the log, but an unexpected exception's own message and traceback are
logged as they are.
"""

from __future__ import annotations

import contextlib
import os
import stat
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
    """Show *text* natively: a message box on Windows, a dialog on macOS, a desktop
    dialog or notification on Linux (see ``_linux_command``), else nothing."""
    platform = sys.platform
    with contextlib.suppress(Exception):
        if platform == "win32":
            import ctypes

            windll = getattr(ctypes, "windll")  # noqa: B009 - absent off Windows
            # A detached process may not take the foreground (Windows refuses unless the
            # host allowed it, which After Effects does not), so the box also stays on top:
            # with no owner window it would otherwise open behind the host.
            flags = 0x10 | 0x10000 | 0x40000  # MB_ICONERROR | MB_SETFOREGROUND | MB_TOPMOST
            windll.user32.MessageBoxW(None, text, "sqi", flags)
        elif platform == "darwin":
            quoted = text.replace("\\", "\\\\").replace('"', '\\"')
            script = (
                f'display dialog "{quoted}" with title "sqi" '
                'with icon stop buttons {"OK"} default button "OK"'
            )
            _run(["osascript", "-e", script])
        elif platform.startswith("linux"):
            argv = _linux_command(text)
            if argv is not None:
                _run(argv)


def _linux_command(text: str) -> list[str] | None:
    """The command showing *text* on a Linux desktop, or ``None`` for log only.

    Only with a display (``DISPLAY`` or ``WAYLAND_DISPLAY``), and only with one of the
    tools installed, tried in order. *text* is always a single argument, never parsed by
    a shell; ``--no-markup`` stops zenity reading it as Pango markup.
    """
    if not (os.environ.get("DISPLAY") or os.environ.get("WAYLAND_DISPLAY")):
        return None
    import shutil

    candidates: list[tuple[str, list[str]]] = [
        ("zenity", ["--error", "--no-markup", "--title", "sqi", "--text", text]),
        ("kdialog", ["--error", text, "--title", "sqi"]),
        ("xmessage", ["-center", text]),
        ("notify-send", ["-u", "critical", "sqi", text]),
    ]
    for tool, args in candidates:
        found = shutil.which(tool)
        if found:
            return [found, *args]
    return None


def _run(argv: list[str]) -> None:
    """Run *argv* (no shell) and wait for it: a dialog returns once it is dismissed."""
    import subprocess

    subprocess.run(
        argv,
        check=False,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )


def _append_log(path: Path, message: str, detail: str | None) -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    stamp = datetime.now().astimezone().isoformat(timespec="seconds")
    entry = f"{stamp} sqi-submit failed: {message}\n"
    if detail:
        entry += detail if detail.endswith("\n") else detail + "\n"
    # One handle for the trim and the append, so nothing can be swapped in between.
    fd = _open_log(path)
    try:
        _trim(fd)
        _write_all(fd, (entry + "\n").encode("utf-8"))
    finally:
        os.close(fd)
    return path


def _open_log(path: Path) -> int:
    """Open (or create) the log for appending, refusing any file it could clobber.

    The settings directory can be moved anywhere with ``SQI_SUBMITTER_SETTINGS``. In a
    folder other users can write to, a planted ``submit-error.log`` would otherwise have
    this process append to, and trim, a file of the artist's. So the log is refused when
    it is a symbolic link, anything but a regular file, a hard link to another name, or
    (POSIX) owned by someone else. POSIX refuses a symlink in the open itself
    (``O_NOFOLLOW``); Windows has no such flag, so a symlink is refused by a check first
    (creating one there needs a privilege by default). ``O_APPEND`` keeps concurrent
    appends from two failures whole. Raises ``OSError``.
    """
    nofollow = getattr(os, "O_NOFOLLOW", 0)
    if not nofollow and path.is_symlink():
        raise OSError(f"{path} is a symbolic link")
    flags = os.O_RDWR | os.O_APPEND | os.O_CREAT | nofollow | getattr(os, "O_BINARY", 0)
    fd = os.open(path, flags, 0o600)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode):
            raise OSError(f"{path} is not a regular file")
        if info.st_nlink != 1:
            raise OSError(f"{path} has other hard links")
        getuid = getattr(os, "getuid", None)  # POSIX only, as is fchmod
        if getuid is not None:
            if info.st_uid != getuid():
                raise OSError(f"{path} is owned by another user")
            if info.st_mode & 0o077:
                # A log written before it was created private keeps its old mode.
                getattr(os, "fchmod")(fd, 0o600)  # noqa: B009 - absent on Windows
    except BaseException:
        os.close(fd)
        raise
    return fd


def _trim(fd: int) -> None:
    """Keep the log bounded: past ``MAX_LOG_BYTES``, keep only its last whole lines.

    The descriptor is ``O_APPEND``, so every write lands at the end: after truncating to
    nothing, writing the kept tail puts it at the start.
    """
    if os.fstat(fd).st_size <= MAX_LOG_BYTES:
        return
    os.lseek(fd, -KEEP_LOG_BYTES, os.SEEK_END)
    tail = _read_all(fd)
    newline = tail.find(b"\n")
    os.ftruncate(fd, 0)
    _write_all(fd, tail[newline + 1 :] if newline != -1 else b"")


def _read_all(fd: int) -> bytes:
    chunks = []
    while chunk := os.read(fd, 64 * 1024):
        chunks.append(chunk)
    return b"".join(chunks)


def _write_all(fd: int, data: bytes) -> None:
    view = memoryview(data)
    while view:
        view = view[os.write(fd, view) :]


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
