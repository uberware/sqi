# SPDX-License-Identifier: AGPL-3.0-or-later
"""sqi Submitter for KeyShot: a script that hands the open scene's studios to the sqi dialog.

Install it with ``sqi-submit install-host keyshot``, which copies this file into KeyShot's user
``Scripts`` folder. Then in KeyShot open Window > Scripting Console and run
``sqi_keyshot_submit``. KeyShot runs a script as ``__main__``, so the guard at the bottom calls
``main()``; importing the file does nothing. This file uses only KeyShot's own Python and the
standard library and never imports ``sqi_submitter``: it writes a small context file describing
the scene and its studios and runs ``sqi-submit --context FILE``, whose Qt dialog does the rest.

``lux`` is imported inside function bodies only, so the module imports (and its scene reading can
be tested) without KeyShot. The block between the ``sqi handoff`` markers is a verbatim copy of
``sqi_submitter/hosts/_handoff_block.py``; edit it there and re-copy it.
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
_SAVE_FAILED_MESSAGE = "KeyShot could not save the scene, so nothing was submitted."
_READ_FAILED = "sqi could not read the scene: "


def _frame_range(info: Any) -> str | None:
    """The animation's frames as ``"1-N"`` (``"1"`` for one frame), or ``None`` for none.

    ``lux.getAnimationInfo()`` returns a dict whose ``frames`` entry is the frame count. Frames
    are numbered from 1, as in KeyShot's quick start; the scripting reference does not state the
    base.
    """
    count = info.get("frames") if isinstance(info, dict) else None
    if not isinstance(count, int) or isinstance(count, bool) or count < 1:
        return None
    return "1" if count == 1 else f"1-{count}"


def _keyshot_version(lux: Any) -> str | None:
    """``"<major>.<minor>"`` from ``lux.getKeyShotDisplayVersion()``, or ``None``."""
    try:
        major, minor = lux.getKeyShotDisplayVersion()
        return f"{major}.{minor}"
    except (AttributeError, TypeError, ValueError):
        return None


def _scene_path(lux: Any) -> str | None:
    """The open scene's file, or ``None`` for a scene that was never saved."""
    info = lux.getSceneInfo()
    path = info.get("file") if isinstance(info, dict) else None
    return path if isinstance(path, str) and path else None


def read_scene() -> dict[str, object]:
    """Describe the scene as a version-1 handoff context document.

    Every studio is a target. KeyShot has one animation timeline, so each carries the scene's
    frame range.
    """
    import lux

    frames = _frame_range(lux.getAnimationInfo())
    targets: list[dict[str, object]] = []
    for studio in lux.getStudios():  # the default form yields names, possibly lazily
        name = str(studio)
        targets.append(
            {
                "name": name,
                "kind": "studio",
                "frame_range": frames,
                "output_path": None,
                "extra": {"Studio": name},
                "locked": [],
            }
        )
    return {
        "version": 1,
        "host": {"name": "keyshot", "display_name": "KeyShot", "version": _keyshot_version(lux)},
        "scene": {
            "path": _scene_path(lux),
            "frame_range": frames,
            # The presets' OutputFile needs a ``#`` run for the frame number, which the scene
            # does not hold, so the artist types it in the dialog.
            "output_path": None,
            "renderer": None,
        },
        "targets": targets,
        "server_url": None,
    }


def _warn(text: str) -> None:
    """Show ``text`` in KeyShot's message box, or print it when there is none (headless)."""
    try:
        import lux

        lux.getMessageBox(msg=text, title="sqi", type=lux.MESSAGE_BOX_WARNING)
    except Exception:
        # ASCII only: a narrow-encoding console raises UnicodeEncodeError on the em dash.
        print("sqi: " + text.encode("ascii", "replace").decode("ascii"))


def submit() -> bool:
    """Save if needed, write the context file and launch ``sqi-submit``.

    Anything the artist must act on is shown in a message box rather than raised into the
    console. Returns whether the dialog was launched.
    """
    import lux

    try:
        scene_path = _scene_path(lux)
    except Exception as exc:
        # The scene-info call has not been run on a real install: whatever it raises is shown.
        _warn(_READ_FAILED + str(exc))
        return False
    if scene_path is None:
        _warn(_UNSAVED_MESSAGE)
        return False
    try:
        if lux.isSceneChanged():
            lux.saveFile()
            # A failed save may return instead of raising; the scene is then still changed.
            saved = not lux.isSceneChanged()
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
        # read_scene calls KeyShot APIs that have not been run on a real install: whatever they
        # raise is shown to the artist, not left to reach KeyShot's console.
        _warn(_READ_FAILED + str(exc))
        return False
    try:
        command = resolve_submit_command("")
        launch(command, write_context(data))
    except (RuntimeError, OSError) as exc:
        _warn(str(exc))
        return False
    return True


def main() -> None:
    submit()


if __name__ == "__main__":
    main()
