# SPDX-License-Identifier: AGPL-3.0-or-later
"""sqi Submitter for Katana: one sqi > Submit to sqi... menu entry that hands the project's
Render nodes to the sqi dialog.

Install it with ``sqi-submit install-host katana``, which copies this file and its startup hook
``init.py`` into ``~/.katana/Startup`` (Katana runs ``init.py`` from there at startup), then
restart Katana. This file uses only Katana's own Python and the standard library and never
imports ``sqi_submitter``: it writes a small context file describing the project's Render nodes
and runs ``sqi-submit --context FILE``, whose Qt dialog does the rest. Katana 4 and 5 ship
Python 3.7, so this file parses as 3.7.

``Katana``, ``KatanaInfo``, ``UI4`` and the Qt binding are imported inside function bodies only,
so the module imports (and its project reading can be tested) without Katana. The block between
the ``sqi handoff`` markers is a verbatim copy of ``sqi_submitter/hosts/_handoff_block.py``; edit
it there and re-copy it.
"""

from __future__ import annotations

from typing import Any

# isort: split

# --- sqi handoff begin ---
import contextlib
import json
import os
import shutil
import stat
import subprocess
import sys
import tempfile

NOT_FOUND_MESSAGE = (
    "sqi-submit was not found. Install it on this machine with: "
    'pip install "sqi-submitter[qt]" — then set its full path in the sqi '
    "add-on/plugin settings, or as sqi_submit_command in ~/.sqi/submitter.json."
)


def _settings_command() -> str:
    # Match sqi-submit's Path.home(): on Windows that is USERPROFILE. Python 3.7's expanduser
    # prefers HOME, which is often a network share there.
    home = (os.environ.get("USERPROFILE") if sys.platform == "win32" else None) or (
        os.path.expanduser("~")
    )
    path = os.environ.get("SQI_SUBMITTER_SETTINGS") or os.path.join(home, ".sqi", "submitter.json")
    try:
        # utf-8-sig: PowerShell 5 writes UTF-8 with a BOM, which plain utf-8 rejects.
        with open(path, encoding="utf-8-sig") as handle:
            value = json.load(handle).get("sqi_submit_command")
    except (OSError, ValueError, AttributeError):
        return ""
    return value if isinstance(value, str) else ""


def resolve_submit_command(configured: str) -> str:
    """Find ``sqi-submit``: the host's own setting, the settings file, then PATH."""
    if configured and os.path.exists(configured):
        return configured
    from_file = _settings_command()
    if from_file and os.path.exists(from_file):
        return from_file
    found = shutil.which("sqi-submit")
    if found:
        return found
    raise RuntimeError(NOT_FOUND_MESSAGE)


def _handoff_folder() -> str:
    """``<tmp>/sqi-handoff`` if it is a directory only this user can enter, else a new one.

    On Linux the temp folder is the shared ``/tmp``: another local user could create
    ``sqi-handoff`` first, open to all, and swap the context file (and with it the
    server the API key is sent to) before ``sqi-submit`` reads it.
    """
    base = tempfile.gettempdir()
    folder = os.path.join(base, "sqi-handoff")
    with contextlib.suppress(FileExistsError):  # not a directory: lstat below decides
        os.makedirs(folder, mode=0o700, exist_ok=True)
    try:
        info = os.lstat(folder)
    except OSError:
        return tempfile.mkdtemp(prefix="sqi-handoff-", dir=base)
    getuid = getattr(os, "getuid", None)  # POSIX only
    if not stat.S_ISDIR(info.st_mode) or (getuid is not None and info.st_uid != getuid()):
        # A symlink, a file, or someone else's folder: use a fresh private one.
        return tempfile.mkdtemp(prefix="sqi-handoff-", dir=base)
    if getuid is not None and info.st_mode & 0o077:
        os.chmod(folder, 0o700)
    return folder


def write_context(data: dict[str, object]) -> str:
    """Write the handoff context to a new private file in the temp folder; return its path."""
    # mkstemp creates the file exclusively and readable by this user only (0600).
    fd, path = tempfile.mkstemp(prefix="context-", suffix=".json", dir=_handoff_folder())
    with os.fdopen(fd, "w", encoding="utf-8") as handle:
        json.dump(data, handle)
    return path


def windowed_command(command: str) -> str:
    """The GUI-subsystem ``sqi-submitw.exe`` beside a Windows ``sqi-submit.exe``, if any.

    ``sqi-submit.exe`` is a console launcher: even started detached, the ``python.exe`` it
    runs gets a console window of its own, open for as long as the dialog is.
    ``sqi-submitw.exe`` runs ``pythonw.exe``, which has none. Anything else (a ``.cmd``
    wrapper, an install from before ``sqi-submitw`` existed) is used as it is.
    """
    folder, name = os.path.split(command)
    if name.lower() != "sqi-submit.exe":
        return command
    windowed = os.path.join(folder, "sqi-submitw.exe")
    return windowed if os.path.isfile(windowed) else command


def launch(command: str, context_path: str) -> None:
    """Start ``sqi-submit --context`` detached, and let its window take focus."""
    if sys.platform == "win32":
        command = windowed_command(command)
    argv = [command, "--context", context_path]
    if sys.platform == "win32":
        flags = getattr(subprocess, "DETACHED_PROCESS", 0x00000008)
        flags |= getattr(subprocess, "CREATE_NEW_PROCESS_GROUP", 0x00000200)
        subprocess.Popen(argv, creationflags=flags, close_fds=True)
        try:
            import ctypes

            # ASFW_ANY (-1): a pip/pipx/uv sqi-submit.exe runs python.exe as a child of its own.
            ctypes.windll.user32.AllowSetForegroundWindow(-1)
        except Exception:
            pass
    else:
        subprocess.Popen(argv, start_new_session=True, close_fds=True)


# --- sqi handoff end ---

_UNSAVED_MESSAGE = "Save your scene before submitting — the farm renders the file on disk."
_SAVE_FAILED_MESSAGE = "Katana could not save the project, so nothing was submitted."
_READ_FAILED = "sqi could not read the scene: "
# The Render node's farm group is ``farmGlobalSettings`` in the node reference (4.0 to 9.0);
# ``farmSettings`` is the name the RenderScript node uses for the same three parameters.
_FARM_GROUPS = ("farmGlobalSettings", "farmSettings")


def _frames(start: float, end: float) -> str:
    """``"S"`` when the ends are equal, else ``"S-E"``, ends ordered low to high."""
    low, high = sorted((int(start), int(end)))
    return str(low) if low == high else f"{low}-{high}"


def _param_value(node: Any, path: str) -> Any:
    param = node.getParameter(path)
    return None if param is None else param.getValue(0)


def _is_on(value: Any) -> bool:
    if isinstance(value, str):
        return value.strip().lower() in ("yes", "true", "1")
    return bool(value)


def node_frame_range(node: Any, in_time: int, out_time: int) -> str:
    """The frames a Render node renders: its active farm range if enabled, else the project's."""
    for group in _FARM_GROUPS:
        flag = _param_value(node, group + ".setActiveFrameRange")
        if flag is None:
            continue
        if _is_on(flag):
            start = _param_value(node, group + ".activeFrameRange.start")
            end = _param_value(node, group + ".activeFrameRange.end")
            if start is not None and end is not None:
                return _frames(start, end)
        break
    return _frames(in_time, out_time)


def _katana_version() -> str | None:
    release = os.environ.get("KATANA_RELEASE")
    if release:
        return release
    try:
        import KatanaInfo

        return str(KatanaInfo.version)
    except (ImportError, AttributeError):
        return None


def read_scene() -> dict[str, object]:
    """Describe the project as a version-1 handoff context document.

    Every Render node that is not bypassed is a target, with its own frame range.
    """
    from Katana import FarmAPI, NodegraphAPI

    in_time, out_time = NodegraphAPI.GetInTime(), NodegraphAPI.GetOutTime()
    targets: list[dict[str, object]] = []
    for node in NodegraphAPI.GetAllNodesByType("Render"):
        if node.isBypassed():
            continue
        name = node.getName()
        targets.append(
            {
                "name": name,
                "kind": "render_node",
                "frame_range": node_frame_range(node, in_time, out_time),
                "output_path": None,
                "extra": {"RenderNode": name},
                "locked": [],
            }
        )
    return {
        "version": 1,
        "host": {"name": "katana", "display_name": "Katana", "version": _katana_version()},
        "scene": {
            "path": FarmAPI.GetKatanaFileName() or None,
            "frame_range": _frames(in_time, out_time),
            "output_path": None,
            "renderer": None,
        },
        "targets": targets,
        "server_url": None,
    }


def _qt_widgets() -> Any:
    """Katana's own ``QtWidgets``: PySide6 from Katana 8, PyQt5 before.

    The binding is chosen from the version, as Katana's developer guide does: a try/except
    could import a stray installed binding into the wrong Katana.
    """
    import KatanaInfo

    if int(KatanaInfo.version.split(".")[0]) >= 8:
        from PySide6 import QtWidgets

        return QtWidgets
    from PyQt5 import QtWidgets as PyQtWidgets

    return PyQtWidgets


def _warn(text: str) -> None:
    """Show ``text`` in a Qt warning box, or print it when Katana's Qt cannot be loaded."""
    try:
        _qt_widgets().QMessageBox.warning(None, "sqi", text)
    except Exception:
        # ASCII only: a narrow-encoding console raises UnicodeEncodeError on the em dash.
        print("sqi: " + text.encode("ascii", "replace").decode("ascii"))


def submit() -> bool:
    """Save if needed, write the context file and launch ``sqi-submit``.

    Anything the artist must act on is shown in a dialog rather than raised into Katana's
    console. Returns whether the dialog was launched.
    """
    from Katana import FarmAPI, KatanaFile

    try:
        path = FarmAPI.GetKatanaFileName()
    except Exception as exc:
        # The file-name call has not been run on a real install: whatever it raises is shown.
        _warn(_READ_FAILED + str(exc))
        return False
    if not path:
        _warn(_UNSAVED_MESSAGE)
        return False
    try:
        if KatanaFile.IsFileDirty():
            KatanaFile.Save(path)
            # A failed save may return instead of raising; the project is then still dirty.
            saved = not KatanaFile.IsFileDirty()
        else:
            saved = True
    except Exception:
        saved = False
    if not saved:
        _warn(_SAVE_FAILED_MESSAGE)
        return False
    try:
        data = read_scene()
    except Exception as exc:
        # read_scene calls Katana APIs that have not been run on a real install: whatever they
        # raise is shown to the artist, not left to reach Katana's console.
        _warn(_READ_FAILED + str(exc))
        return False
    try:
        command = resolve_submit_command("")
        launch(command, write_context(data))
    except (RuntimeError, OSError) as exc:
        _warn(str(exc))
        return False
    return True


def install_menu(**_kwargs: Any) -> None:
    """Add sqi > Submit to sqi... to Katana's main menu bar.

    Registered as an ``onStartupComplete`` callback, which Katana calls with keyword arguments.
    A session with no UI (``katana --batch`` on a farm machine with this user's Startup folder)
    has no menu to add to, so it returns quietly rather than print into every render log.
    """
    try:
        from UI4.App import MainMenu, MainWindow
    except ImportError:
        return
    window = MainWindow.CurrentMainWindow()
    if window is None:
        return
    menu_bar = window.findChild(MainMenu.MainMenu)
    if menu_bar is None:
        print("sqi: Katana's menu bar was not found, so Submit to sqi... was not added.")
        return
    action = menu_bar.addMenu("sqi").addAction("Submit to sqi…")
    action.triggered.connect(lambda *_checked: submit())
