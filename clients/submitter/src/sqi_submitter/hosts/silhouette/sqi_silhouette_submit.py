# SPDX-License-Identifier: AGPL-3.0-or-later
"""sqi Submitter for Silhouette: an Action that hands the project's output nodes to the dialog.

Install it with ``sqi-submit install-host silhouette``, which copies this file and a small
``sfxuser.py`` hook into the user ``scripts`` folder (``$(SFX_USER_PATH)/scripts``). Silhouette's
startup script imports a module named ``sfxuser`` from its script search path, and the hook
registers **Submit to sqi...** in the Actions menu. This file uses only Silhouette's own Python
and the standard library and never imports ``sqi_submitter``: it writes a small context file
describing the project and its output nodes and runs ``sqi-submit --context FILE``, whose Qt
dialog does the rest.

``fx`` is imported inside function bodies only, so the module imports (and its project reading
can be tested) without Silhouette. The block between the ``sqi handoff`` markers is a verbatim
copy of ``sqi_submitter/hosts/_handoff_block.py``; edit it there and re-copy it.
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
    # The home sqi-submit reads (Path.home()): on Windows that is USERPROFILE, which Python 3.7's
    # expanduser ranks below HOME, often set to a network share there.
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
_SAVE_FAILED_MESSAGE = "Silhouette could not save the project, so nothing was submitted."
_READ_FAILED = "sqi could not read the scene: "

# The ``type`` of a node that writes the render (the one ``sfxcmd -node`` names by label).
_OUTPUT_NODE_TYPE = "OutputNode"


def bundle_path(path: str) -> str:
    """The ``.sfx`` bundle directory for ``path``, never a file inside it.

    A Silhouette project is a folder (``shot.sfx``) holding ``project.sfx``, and ``fx`` reports
    the file inside. ``sfxcmd`` must be given the folder. The split is done on the string so a
    Windows path (backslashes) and a POSIX one are handled the same on either OS.
    """
    trimmed = path.rstrip("/\\")
    cut = max(trimmed.rfind("/"), trimmed.rfind("\\"))
    name = trimmed[cut + 1 :]
    parent = trimmed[:cut] if cut > 0 else ""
    parent_name = parent[max(parent.rfind("/"), parent.rfind("\\")) + 1 :]
    if name.lower() == "project.sfx" or (
        parent_name.lower().endswith(".sfx") and not name.lower().endswith(".sfx")
    ):
        return parent
    return trimmed


def session_frame_range(session: Any) -> str:
    """The session's frames, ``startFrame`` to ``startFrame + duration - 1``, as ``"S-E"``.

    These are session numbering (a shot at 1001 is 1001...), which is what ``sfxcmd -range``
    takes. One frame is spelled ``"S"``.
    """
    start = int(session.startFrame)
    end = start + max(int(session.duration), 1) - 1
    return str(start) if end == start else f"{start}-{end}"


def _project_path(fx: Any) -> str | None:
    """The open project's bundle directory, or ``None`` for a project never saved."""
    project = fx.activeProject()
    path = getattr(project, "path", None) if project is not None else None
    return bundle_path(path) if isinstance(path, str) and path else None


def read_scene() -> dict[str, object]:
    """Describe the project as a version-1 handoff context document.

    Every (session, output node) pair across every session is a target, carrying its own
    session's frame range.
    """
    import fx

    project = fx.activeProject()
    targets: list[dict[str, object]] = []
    for session in project.sessions if project is not None else []:
        frames = session_frame_range(session)
        for node in session.nodes:
            if node.type != _OUTPUT_NODE_TYPE:
                continue
            targets.append(
                {
                    "name": f"{session.label} / {node.label}",
                    "kind": "output_node",
                    "frame_range": frames,
                    "output_path": None,
                    "extra": {"Session": session.label, "OutputNode": node.label},
                    "locked": [],
                }
            )
    active = fx.activeSession()
    return {
        "version": 1,
        "host": {"name": "silhouette", "display_name": "Silhouette", "version": None},
        "scene": {
            "path": _project_path(fx),
            "frame_range": session_frame_range(active) if active is not None else None,
            # The output files are named by each Output node inside the project.
            "output_path": None,
            "renderer": None,
        },
        "targets": targets,
        "server_url": None,
    }


def _qt_widgets() -> Any:
    """Silhouette's own ``QtWidgets``: PySide6 from 2026, PySide2 before (one is bundled)."""
    try:
        from PySide6 import QtWidgets

        return QtWidgets
    except ImportError:
        from PySide2 import QtWidgets as PySide2Widgets

        return PySide2Widgets


def _warn(text: str) -> None:
    """Show ``text`` in a Qt warning box, or print it when there is no running Qt application.

    ``fx`` has no documented message call, so this uses the Qt Silhouette bundles. Without a
    ``QApplication`` (a headless ``sfxcmd -script``) a message box would crash the process.
    """
    try:
        widgets = _qt_widgets()
        if widgets.QApplication.instance() is None:
            raise RuntimeError("no QApplication")
        widgets.QMessageBox.warning(None, "sqi", text)
    except Exception:
        # ASCII only: a narrow-encoding console raises UnicodeEncodeError on the em dash.
        print("sqi: " + text.encode("ascii", "replace").decode("ascii"))


def submit() -> bool:
    """Save if needed, write the context file and launch ``sqi-submit``.

    Anything the artist must act on is shown in a dialog rather than raised into the console.
    Returns whether the dialog was launched.
    """
    import fx

    try:
        project_path = _project_path(fx)
    except Exception as exc:
        # The project call has not been run on a real install: whatever it raises is shown.
        _warn(_READ_FAILED + str(exc))
        return False
    if project_path is None:
        _warn(_UNSAVED_MESSAGE)
        return False
    try:
        project = fx.activeProject()
        if project.is_modified:
            project.save()
            # A failed save may return instead of raising; the project is then still modified.
            saved = not project.is_modified
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
        # read_scene calls Silhouette APIs that have not been run on a real install: whatever
        # they raise is shown to the artist, not left to reach Silhouette's console.
        _warn(_READ_FAILED + str(exc))
        return False
    try:
        command = resolve_submit_command("")
        launch(command, write_context(data))
    except (RuntimeError, OSError) as exc:
        _warn(str(exc))
        return False
    return True


def register() -> None:
    """Add **Submit to sqi...** to Silhouette's Actions menu."""
    import fx

    class SubmitAction(fx.Action):  # type: ignore[misc]
        def __init__(self) -> None:
            fx.Action.__init__(self, "Submit to sqi…")

        def available(self) -> bool:
            return True

        def execute(self) -> None:
            submit()

    fx.addAction(SubmitAction())
