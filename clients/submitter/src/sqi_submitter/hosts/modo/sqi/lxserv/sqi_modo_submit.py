# SPDX-License-Identifier: AGPL-3.0-or-later
"""sqi Submitter for Modo: a command that hands the scene's render pass groups to the dialog.

Install it with ``sqi-submit install-host modo``, which copies the ``sqi`` kit folder
(``index.cfg`` and this file under ``lxserv/``) into the user ``Scripts`` folder. Modo imports
every module in a kit's ``lxserv`` folder at startup, and this one blesses the command
``sqi.submit``. This file uses only Modo's own Python and the standard library and never
imports ``sqi_submitter``: it writes a small context file describing the scene and its pass
groups and runs ``sqi-submit --context FILE``, whose Qt dialog does the rest.

``lx``, ``lxu`` and ``modo`` are imported inside function bodies only, so the module imports
(and its scene reading can be tested) without Modo. The block between the ``sqi handoff``
markers is a verbatim copy of ``sqi_submitter/hosts/_handoff_block.py``; edit it there and
re-copy it.
"""

from __future__ import annotations

import importlib.util
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
_SAVE_FAILED_MESSAGE = "Modo could not save the scene, so nothing was submitted."
_READ_FAILED = "sqi could not read the scene: "

# The item type of the scene's render item, which owns the ``first``/``last`` frame channels.
_RENDER_ITEM_TYPE = "polyRender"


def _scene_path() -> str | None:
    """The current scene's file, or ``None`` for a scene never saved.

    ``Scene.Filename`` reports an unnamed scene as a failure, which Python sees as an exception.
    Only that call is read that way: failing to reach the scene or the method (a wrong API name)
    raises to the caller, which shows it as a read failure, not as an unsaved scene.
    """
    import lxu.select

    filename = lxu.select.SceneSelection().current().Filename
    try:
        path = filename()
    except Exception:
        return None
    return path if isinstance(path, str) and path else None


def _scene_changed() -> bool:
    import lxu.select

    return bool(lxu.select.SceneSelection().current().HasChanged())


def _frame_range() -> str | None:
    """The render item's ``first`` and ``last`` frames as ``"S-E"``, lower end first (``"S"``
    for one frame).

    Finds the render item the way Foundry's ChangeRenderFrameRange sample does (walk the scene's
    items for type ``polyRender``), selects it, and reads its channels. ``None`` when the scene
    has no render item. Side effect: the render item becomes the artist's selected item. The
    sample's ``item.id ?`` query takes no index (it follows the ``item.type ? n`` cursor), so
    that form is kept.
    """
    import lx

    for index in range(int(lx.eval("query sceneservice item.N ?"))):
        if lx.eval(f"query sceneservice item.type ? {index}") != _RENDER_ITEM_TYPE:
            continue
        lx.eval(f"select.item {lx.eval('query sceneservice item.id ?')}")
        first = int(lx.eval("item.channel polyRender$first ?"))
        last = int(lx.eval("item.channel polyRender$last ?"))
        lo, hi = sorted((first, last))
        return str(lo) if lo == hi else f"{lo}-{hi}"
    return None


def read_scene() -> dict[str, object]:
    """Describe the scene as a version-1 handoff context document.

    Every render pass group is a target, each carrying the render item's frame range.
    """
    import modo

    frames = _frame_range()
    targets: list[dict[str, object]] = []
    for group in modo.Scene().renderPassGroups:
        targets.append(
            {
                "name": group.name,
                "kind": "pass_group",
                "frame_range": frames,
                "output_path": None,
                "extra": {"PassGroup": group.name},
                "locked": [],
            }
        )
    return {
        "version": 1,
        "host": {"name": "modo", "display_name": "Modo", "version": None},
        "scene": {
            "path": _scene_path(),
            "frame_range": frames,
            # The output files are named by the scene's render outputs.
            "output_path": None,
            "renderer": None,
        },
        "targets": targets,
        "server_url": None,
    }


def _alert(text: str) -> None:
    """Show ``text`` in a Modo info dialog, or print it if the dialog commands fail.

    Uses the documented ``dialog.*`` commands. Modo quotes a command argument in braces, so
    braces in ``text`` are swapped for parentheses.
    """
    try:
        import lx

        lx.eval("dialog.setup info")
        lx.eval("dialog.title {sqi}")
        lx.eval("dialog.msg {" + text.replace("{", "(").replace("}", ")") + "}")
        lx.eval("dialog.open")
    except Exception:
        # ASCII only: a narrow-encoding console raises UnicodeEncodeError on the em dash.
        print("sqi: " + text.encode("ascii", "replace").decode("ascii"))


def submit() -> bool:
    """Save if needed, write the context file and launch ``sqi-submit``.

    Anything the artist must act on is shown in a dialog rather than raised into the console.
    Returns whether the dialog was launched.
    """
    import lx

    try:
        scene_path = _scene_path()
    except Exception as exc:
        # The scene calls have not been run on a real install: whatever they raise is shown.
        _alert(_READ_FAILED + str(exc))
        return False
    if scene_path is None:
        _alert(_UNSAVED_MESSAGE)
        return False
    try:
        if _scene_changed():
            lx.eval("scene.save")
            # A failed save may return instead of raising; the scene is then still changed.
            saved = not _scene_changed()
        else:
            saved = True
    except Exception:
        saved = False
    if not saved:
        _alert(_SAVE_FAILED_MESSAGE)
        return False
    try:
        data = read_scene()
    except Exception as exc:
        # read_scene calls Modo APIs that have not been run on a real install: whatever they
        # raise is shown to the artist, not left to reach Modo's console.
        _alert(_READ_FAILED + str(exc))
        return False
    try:
        command = resolve_submit_command("")
        launch(command, write_context(data))
    except (RuntimeError, OSError) as exc:
        _alert(str(exc))
        return False
    return True


def register() -> None:
    """Bless the ``sqi.submit`` command."""
    import lx
    import lxu.command

    class SubmitCommand(lxu.command.BasicCommand):  # type: ignore[misc]
        def basic_Execute(self, msg: Any, flags: Any) -> None:
            submit()

    lx.bless(SubmitCommand, "sqi.submit")


# Modo imports every lxserv module at startup, so this is what makes the command exist; tests
# load the file without lx and must register nothing.
if importlib.util.find_spec("lx") is not None:
    register()
